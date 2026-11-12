package login

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
)

type formState struct {
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Submitted bool   `json:"submitted"`
}

type driver struct {
	connection        connection
	ctx               context.Context
	cancel            context.CancelFunc
	origin            string
	queue             chan *fetch.EventRequestPaused
	done              chan struct{}
	closed            atomic.Bool
	failed            atomic.Bool
	blocked           atomic.Bool
	submitting        atomic.Bool
	uncertain         atomic.Bool
	destroy           sync.Once
	destroying        atomic.Bool
	destroyDone       chan struct{}
	world             cdpruntime.ExecutionContextID
	mu                sync.Mutex
	pending           map[network.RequestID]bool
	activity          time.Time
	frame             cdp.FrameID
	responseConfirmed bool
}

func newDriver(ctx context.Context, origin string) (*driver, error) {
	connection, err := defaultConnection(ctx)
	if err != nil {
		return nil, errBrowser
	}
	return newDriverWithConnection(ctx, origin, connection)
}

func newDriverWithConnection(ctx context.Context, origin string, connection connection) (*driver, error) {
	if ctx.Err() != nil {
		return nil, errBrowser
	}
	guard, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d := &driver{connection: connection, ctx: guard, cancel: cancel, origin: origin, queue: make(chan *fetch.EventRequestPaused, 128), done: make(chan struct{}), destroyDone: make(chan struct{}), pending: make(map[network.RequestID]bool), activity: time.Now()}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(d.requests)
	}
	go func() { workers.Wait(); close(d.done) }()
	err := connection.Listen(guard, func(event any) {
		switch event := event.(type) {
		case *fetch.EventRequestPaused:
			select {
			case d.queue <- event:
			default:
				d.failed.Store(true)
				d.destroyTarget()
			}
		case *network.EventRequestWillBeSent:
			d.mu.Lock()
			// A redirect reuses its request ID for the next hop. Attribute the
			// preceding POST response before replacing that pending record.
			if event.RedirectResponse != nil {
				d.confirmResponseLocked(event.RequestID, event.RedirectResponse.Status)
			}
			if len(d.pending) >= 256 {
				d.failed.Store(true)
			} else {
				d.pending[event.RequestID] = d.submitting.Load() && event.Type == network.ResourceTypeDocument &&
					event.FrameID == d.frame && event.Request.Method == "POST"
			}
			d.activity = time.Now()
			d.mu.Unlock()
		case *network.EventResponseReceived:
			if event.Response != nil {
				d.mu.Lock()
				d.confirmResponseLocked(event.RequestID, event.Response.Status)
				d.mu.Unlock()
			}
		case *network.EventLoadingFinished:
			d.finished(event.RequestID, false)
		case *network.EventLoadingFailed:
			d.finished(event.RequestID, true)
		}
	}, func() {
		if !d.closed.Load() {
			d.failed.Store(true)
			d.destroyTarget()
		}
	})
	bounded, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	if err != nil || fetch.Enable().Do(cdp.WithExecutor(bounded, connection)) != nil {
		_ = d.close()
		return nil, errBrowser
	}
	return d, nil
}

func (d *driver) confirmResponseLocked(id network.RequestID, status int64) {
	if d.pending[id] && status >= 200 && status <= 599 {
		d.responseConfirmed = true
	}
}

func (d *driver) beginSubmission() {
	d.mu.Lock()
	d.responseConfirmed = false
	d.submitting.Store(true)
	d.mu.Unlock()
}

func (d *driver) hasSubmissionResponse() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.responseConfirmed
}

func (d *driver) finished(id network.RequestID, failed bool) {
	d.mu.Lock()
	if failed && d.pending[id] {
		d.uncertain.Store(true)
	}
	delete(d.pending, id)
	d.activity = time.Now()
	d.mu.Unlock()
}

func (d *driver) requests() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case request := <-d.queue:
			ctx, cancel := context.WithTimeout(d.ctx, 2*time.Second)
			executor := cdp.WithExecutor(ctx, d.connection)
			origin, err := browsing.LoginOrigin(request.Request.URL)
			if !d.closed.Load() && err == nil && origin == d.origin {
				err = fetch.ContinueRequest(request.RequestID).Do(executor)
			} else {
				if request.ResourceType == network.ResourceTypeDocument {
					d.blocked.Store(true)
				}
				err = fetch.FailRequest(request.RequestID, network.ErrorReasonBlockedByClient).Do(executor)
			}
			cancel()
			if err != nil && !d.closed.Load() {
				d.failed.Store(true)
				d.destroyTarget()
			}
		}
	}
}

func (d *driver) destroyTarget() {
	d.destroy.Do(func() {
		d.destroying.Store(true)
		go func() {
			defer close(d.destroyDone)
			ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), 2*time.Second)
			defer cancel()
			_ = d.connection.CloseTarget(ctx)
		}()
	})
}

func (d *driver) close() error {
	d.closed.Store(true)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), 3*time.Second)
	defer cancel()
	// Clear only the private form references. A navigation has already destroyed
	// the old execution world when these references no longer exist.
	d.clearFields(ctx)
	executor := cdp.WithExecutor(ctx, d.connection)
	err := page.StopLoading().Do(executor)
	if err == nil {
		err = fetch.Disable().Do(executor)
	}
	d.cancel()
	select {
	case <-d.done:
	case <-ctx.Done():
		err = errBrowser
	}
	if d.connection.Close(ctx) != nil {
		err = errBrowser
	}
	if err != nil {
		d.destroyTarget()
	}
	// A native connection is borrowed only until this login callback returns.
	// Join any earlier destruction request before its private lease is revoked.
	if d.destroying.Load() {
		<-d.destroyDone // CloseTarget has its own two-second deadline.
	}
	if err != nil {
		return errBrowser
	}
	return nil
}

func (d *driver) clearFields(ctx context.Context) {
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// Successful submission can replace the isolated execution context. Reopen
	// the current same-origin world before best-effort cleanup; references from
	// an old document have already disappeared with that document.
	if _, err := d.prepareWorld(bounded); err != nil {
		return
	}
	_ = d.call(bounded, `for(const f of [globalThis.__commanderLogin,globalThis.__commanderLoginChallenge]) { if(f) for(const key of ['username','password','code']) {if(f[key]) f[key].value='';} } return true;`, nil, nil)
}

func (d *driver) navigate(ctx context.Context, address string) error {
	origin, err := browsing.LoginOrigin(address)
	if err != nil || origin != d.origin || d.failed.Load() {
		return errBrowser
	}
	d.world = 0
	d.blocked.Store(false)
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if d.connection.Navigate(bounded, address) != nil || d.blocked.Load() || d.failed.Load() {
		return errBrowser
	}
	return d.settle(bounded)
}

func (d *driver) settle(ctx context.Context) error {
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	start := time.Now()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		d.mu.Lock()
		idle := len(d.pending) == 0 && time.Since(d.activity) >= 150*time.Millisecond && time.Since(start) >= 150*time.Millisecond
		d.mu.Unlock()
		if d.failed.Load() || d.blocked.Load() || d.uncertain.Load() {
			return errBrowser
		}
		if idle {
			return nil
		}
		select {
		case <-bounded.Done():
			return errBrowser
		case <-ticker.C:
		}
	}
}

func (d *driver) prepareWorld(ctx context.Context) (string, error) {
	executor := cdp.WithExecutor(ctx, d.connection)
	tree, err := page.GetFrameTree().Do(executor)
	if err != nil || tree == nil || tree.Frame == nil {
		return "", errBrowser
	}
	origin, err := browsing.LoginOrigin(tree.Frame.URL)
	if err != nil || origin != d.origin {
		return "", errBrowser
	}
	d.mu.Lock()
	d.frame = tree.Frame.ID
	d.mu.Unlock()
	d.world, err = page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("commander-browser-login").Do(executor)
	if err != nil {
		return "", errBrowser
	}
	return tree.Frame.URL, nil
}

func (d *driver) call(ctx context.Context, body string, arguments any, result any) error {
	if d.world == 0 || d.failed.Load() && !d.closed.Load() {
		return errBrowser
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return errBrowser
	}
	defer clear(encoded)
	value, exception, err := cdpruntime.CallFunctionOn("function(args){" + loginDOM + body + "}").
		WithExecutionContextID(d.world).WithArguments([]*cdpruntime.CallArgument{{Value: encoded}}).
		WithSilent(true).WithReturnByValue(true).Do(cdp.WithExecutor(ctx, d.connection))
	if err != nil || exception != nil || value == nil {
		return errBrowser
	}
	if result != nil && json.Unmarshal(value.Value, result) != nil {
		return errBrowser
	}
	return nil
}

func (d *driver) inspect(ctx context.Context) (formState, error) {
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := d.prepareWorld(bounded); err != nil {
		return formState{}, errBrowser
	}
	var state formState
	if d.call(bounded, inspectLoginDOM, d.origin, &state) != nil {
		return state, errBrowser
	}
	return state, nil
}

func (d *driver) submit(ctx context.Context, credentials Credentials) (formState, error) {
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := d.prepareWorld(bounded); err != nil {
		return formState{}, errBrowser
	}
	var state formState
	arguments := map[string]string{"origin": d.origin, "username": string(credentials.Username), "password": string(credentials.Password)}
	d.beginSubmission()
	if d.call(bounded, submitLoginDOM, arguments, &state) != nil {
		return state, errBrowser
	}
	return state, nil
}

func (d *driver) submitChallenge(ctx context.Context, code []byte) (formState, error) {
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if _, err := d.prepareWorld(bounded); err != nil {
		return formState{}, errBrowser
	}
	var state formState
	arguments := map[string]string{"origin": d.origin, "code": string(code)}
	d.beginSubmission()
	if d.call(bounded, submitChallengeDOM, arguments, &state) != nil {
		return state, errBrowser
	}
	return state, nil
}

func verificationTarget(address string) string {
	origin, err := browsing.LoginOrigin(address)
	if err != nil {
		return ""
	}
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" || u.ForceQuery {
		path += "?" + u.RawQuery
	}
	return origin + path
}

func (d *driver) verify(ctx context.Context, address, expected string) (bool, error) {
	bounded, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	current, err := d.prepareWorld(bounded)
	if err != nil {
		return false, errBrowser
	}
	if verificationTarget(current) != verificationTarget(address) {
		return false, nil
	}
	var matched bool
	if d.call(bounded, `return visibleText(document.body||document.documentElement).includes(args);`, expected, &matched) != nil {
		return false, errBrowser
	}
	return matched, nil
}
