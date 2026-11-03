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
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
)

type formState struct {
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Submitted bool   `json:"submitted"`
}

type driver struct {
	ctx        context.Context
	cancel     context.CancelFunc
	origin     string
	queue      chan *fetch.EventRequestPaused
	done       chan struct{}
	closed     atomic.Bool
	failed     atomic.Bool
	blocked    atomic.Bool
	submitting atomic.Bool
	uncertain  atomic.Bool
	destroy    sync.Once
	world      cdpruntime.ExecutionContextID
	mu         sync.Mutex
	pending    map[network.RequestID]bool
	activity   time.Time
}

func newDriver(ctx context.Context, origin string) (*driver, error) {
	connection := chromedp.FromContext(ctx)
	if connection == nil || connection.Target == nil || connection.Browser == nil || ctx.Err() != nil {
		return nil, errBrowser
	}
	guard, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d := &driver{ctx: guard, cancel: cancel, origin: origin, queue: make(chan *fetch.EventRequestPaused, 128), done: make(chan struct{}), pending: make(map[network.RequestID]bool), activity: time.Now()}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(d.requests)
	}
	go func() { workers.Wait(); close(d.done) }()
	chromedp.ListenTarget(guard, func(event any) {
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
			if len(d.pending) >= 256 {
				d.failed.Store(true)
			} else {
				d.pending[event.RequestID] = d.submitting.Load() && event.Request.Method != "GET" && event.Request.Method != "HEAD"
			}
			d.activity = time.Now()
			d.mu.Unlock()
		case *network.EventLoadingFinished:
			d.finished(event.RequestID, false)
		case *network.EventLoadingFailed:
			d.finished(event.RequestID, true)
		}
	})
	bounded, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	if chromedp.Run(bounded, fetch.Enable()) != nil {
		_ = d.close()
		return nil, errBrowser
	}
	return d, nil
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
			executor := cdp.WithExecutor(ctx, chromedp.FromContext(d.ctx).Target)
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
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), 2*time.Second)
			defer cancel()
			connection := chromedp.FromContext(d.ctx)
			_ = target.CloseTarget(connection.Target.TargetID).Do(cdp.WithExecutor(ctx, connection.Browser))
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
	connection := chromedp.FromContext(d.ctx)
	executor := cdp.WithExecutor(ctx, connection.Target)
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
	if err != nil {
		d.destroyTarget()
		return errBrowser
	}
	return nil
}

func (d *driver) clearFields(ctx context.Context) {
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = d.call(bounded, `const f=globalThis.__commanderLogin; if(f) { f.username.value=''; f.password.value=''; } return true;`, nil, nil)
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
	if chromedp.Run(bounded, chromedp.Navigate(address)) != nil || d.blocked.Load() || d.failed.Load() {
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
	var address string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		tree, err := page.GetFrameTree().Do(ctx)
		if err != nil || tree == nil || tree.Frame == nil {
			return errBrowser
		}
		origin, err := browsing.LoginOrigin(tree.Frame.URL)
		if err != nil || origin != d.origin {
			return errBrowser
		}
		address = tree.Frame.URL
		d.world, err = page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("commander-browser-login").Do(ctx)
		return err
	}))
	if err != nil {
		return "", errBrowser
	}
	return address, nil
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
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		value, exception, err := cdpruntime.CallFunctionOn("function(args){" + loginDOM + body + "}").
			WithExecutionContextID(d.world).WithArguments([]*cdpruntime.CallArgument{{Value: encoded}}).
			WithSilent(true).WithReturnByValue(true).Do(ctx)
		if err != nil || exception != nil || value == nil {
			return errBrowser
		}
		if result != nil && json.Unmarshal(value.Value, result) != nil {
			return errBrowser
		}
		return nil
	}))
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
	d.submitting.Store(true)
	if d.call(bounded, submitLoginDOM, arguments, &state) != nil {
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
