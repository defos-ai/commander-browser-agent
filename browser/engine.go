// Package browser owns process-local browsers bound to Commander sessions.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	lpruntime "github.com/defos-ai/commander-browser-agent/browser/lightpanda"
)

type Engine string

const Lightpanda Engine = "lightpanda"

// Options select an engine explicitly. Executable comes from action discovery.
type Options struct {
	Engine     Engine
	Executable string
}

func (o Options) validate() error {
	if o.Engine != Lightpanda {
		return errors.New("select lightpanda explicitly; other browser engines are not supported")
	}
	if !filepath.IsAbs(o.Executable) {
		return errors.New("browser executable must be an absolute discovered path")
	}
	return nil
}

func engineLabel(engine Engine) string {
	if engine == Lightpanda {
		return "Lightpanda browser session"
	}
	return "Browser session"
}

const startupTimeout = 15 * time.Second
const cleanupTimeout = 4 * time.Second

var errPortInUse = errors.New("browser startup port occupied")

type browserRuntime struct {
	ctx             context.Context
	cancel          context.CancelFunc
	page            context.Context
	cancelPage      context.CancelFunc
	cancelAllocator context.CancelFunc
	command         *exec.Cmd
	waited          chan struct{}
	initialized     chan struct{}
	profile         string
	closeOnce       sync.Once
	closed          chan struct{}
	profileMu       sync.Mutex
	removal         *profileRemoval
}

type profileRemoval struct {
	done chan struct{}
	err  error
}

func launchBrowser(parent context.Context, options Options) (*browserRuntime, error) {
	// Port selection cannot be atomic with Lightpanda's bind. Retry only an
	// occupied startup port, before any browser work or authentication occurs.
	for attempt := 0; attempt < 3; attempt++ {
		runtime, err := launchAttempt(parent, options)
		if runtime != nil || !errors.Is(err, errPortInUse) {
			return runtime, err
		}
	}
	return nil, errors.New("cannot reserve a local Lightpanda endpoint")
}

func launchAttempt(parent context.Context, options Options) (retained *browserRuntime, result error) {
	ctx, cancel := context.WithCancel(parent)
	r := &browserRuntime{ctx: ctx, cancel: cancel, closed: make(chan struct{})}
	startupTimer := time.AfterFunc(startupTimeout, cancel)
	defer startupTimer.Stop()
	defer func() {
		if result != nil {
			if cleanupErr := r.Close(); cleanupErr != nil {
				// The provider must retain uncertain resources even when startup
				// failed, so a later close cannot falsely report the identity closed.
				retained = r
				result = errors.Join(result, cleanupErr)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	profile, err := os.MkdirTemp("", "commander-browser-session-")
	if err != nil {
		return nil, errors.New("cannot create a private browser profile")
	}
	r.profile = profile
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("cannot reserve a local Lightpanda endpoint")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return nil, errors.New("cannot release the browser startup port")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	arguments := []string{"serve", "--host", "127.0.0.1", "--port", strconv.Itoa(port)}
	r.command = exec.CommandContext(ctx, options.Executable, arguments...)
	r.command.Dir = profile
	r.command.Env = browserEnvironment()
	r.command.Stdout, r.command.Stderr = io.Discard, io.Discard
	r.command.WaitDelay = time.Second
	if err := configureCommand(r.command); err != nil {
		return nil, err
	}
	if err := r.command.Start(); err != nil {
		return nil, errors.New("cannot start the selected browser executable")
	}
	r.waited = make(chan struct{})
	go func() {
		_ = r.command.Wait()
		close(r.waited)
		cancel()
	}()
	endpoint, err := awaitEndpoint(ctx, address)
	if err != nil {
		select {
		case <-r.waited:
			if probe, bindErr := net.Listen("tcp4", address); bindErr != nil {
				return nil, errPortInUse
			} else {
				_ = probe.Close()
			}
		default:
		}
		return nil, errors.New("browser startup failed; check the executable and platform requirements")
	}
	allocator, cancelAllocator := chromedp.NewRemoteAllocator(ctx, endpoint, chromedp.NoModifyURL)
	r.cancelAllocator = cancelAllocator
	r.page, r.cancelPage = chromedp.NewContext(allocator,
		chromedp.WithLogf(func(string, ...any) {}),
		chromedp.WithErrorf(func(string, ...any) {}),
		chromedp.WithBrowserOption(chromedp.WithDialTimeout(time.Second)),
	)
	// The first Run must own the long-lived session context. Allocating on an
	// action's timeout context would destroy authenticated state when it ends.
	r.initialized = make(chan struct{})
	var initializeErr error
	go func() {
		initializeErr = chromedp.Run(r.page)
		close(r.initialized)
	}()
	select {
	case <-r.initialized:
		if initializeErr != nil {
			return nil, errors.New("cannot initialize the browser CDP connection")
		}
	case <-ctx.Done():
		return nil, errors.New("browser startup was interrupted or timed out")
	}
	if err := r.Verify(ctx); err != nil {
		return nil, err
	}
	if !startupTimer.Stop() || ctx.Err() != nil {
		return nil, errors.New("browser startup was interrupted or timed out")
	}
	page := chromedp.FromContext(r.page).Target
	chromedp.ListenBrowser(r.page, func(event any) {
		switch event := event.(type) {
		case *target.EventTargetDestroyed:
			if event.TargetID == page.TargetID {
				cancel()
			}
		case *target.EventDetachedFromTarget:
			if event.SessionID == page.SessionID {
				cancel()
			}
		}
	})
	go func() {
		select {
		case <-chromedp.FromContext(r.page).Browser.LostConnection:
			cancel()
		case <-ctx.Done():
		}
	}()
	return r, nil
}

func awaitEndpoint(ctx context.Context, address string) (string, error) {
	client := &http.Client{
		Timeout:       250 * time.Millisecond,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/json/version", nil)
		if err != nil {
			return "", err
		}
		response, err := client.Do(request)
		if err == nil {
			var version struct{ Browser string }
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&version)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK && decodeErr == nil && strings.HasPrefix(version.Browser, "Lightpanda/") {
				return "ws://" + address, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *browserRuntime) Context() context.Context { return r.page }
func (r *browserRuntime) Done() <-chan struct{}    { return r.ctx.Done() }

func (r *browserRuntime) Verify(ctx context.Context) error {
	if r.ctx.Err() != nil {
		return errors.New("browser connection is lost")
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	connection := chromedp.FromContext(r.page)
	var version struct {
		Version string `json:"version"`
	}
	if err := connection.Browser.Execute(probe, "LP.version", nil, &version); err != nil || version.Version != lpruntime.Version {
		return errors.New("cannot verify the supported Lightpanda runtime")
	}
	value, exception, err := cdpruntime.Evaluate("true").WithReturnByValue(true).Do(cdp.WithExecutor(probe, connection.Target))
	if err != nil || exception != nil || value == nil || string(value.Value) != "true" {
		return errors.New("cannot verify the retained browser page")
	}
	return nil
}

func (r *browserRuntime) Close() error {
	r.cancel()
	r.closeOnce.Do(func() {
		go func() {
			if r.waited != nil {
				<-r.waited
			}
			if r.initialized != nil {
				<-r.initialized
			}
			if r.cancelPage != nil {
				r.cancelPage()
			}
			if r.cancelAllocator != nil {
				r.cancelAllocator()
			}
			// Continue cleanup even if every bounded Close caller timed out.
			r.removeProfile()
			close(r.closed)
		}()
	})
	deadline := time.NewTimer(cleanupTimeout)
	defer deadline.Stop()
	select {
	case <-r.closed:
	case <-deadline.C:
		return errors.New("browser cleanup could not be confirmed within its time limit")
	}
	attempt := r.removeProfile()
	select {
	case <-attempt.done:
		return attempt.err
	case <-deadline.C:
		return errors.New("browser cleanup could not be confirmed within its time limit")
	}
}

// Failed profile removal can be retried after transport teardown. Concurrent
// callers share one attempt, whose result stays immutable after completion.
func (r *browserRuntime) removeProfile() *profileRemoval {
	r.profileMu.Lock()
	defer r.profileMu.Unlock()
	if previous := r.removal; previous != nil {
		select {
		case <-previous.done:
			if previous.err == nil {
				return previous
			}
		default:
			return previous
		}
	}
	attempt := &profileRemoval{done: make(chan struct{})}
	r.removal = attempt
	go func() {
		if r.profile != "" {
			if err := os.RemoveAll(r.profile); err != nil {
				attempt.err = errors.New("cannot remove the private browser profile")
			}
		}
		close(attempt.done)
	}()
	return attempt
}

// Pass OS runtime, proxy and certificate settings only. In particular, no
// provider API keys, cookie-jar options or browser-state configuration is inherited.
func browserEnvironment() []string {
	var result []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
			"SYSTEMROOT", "WINDIR", "USERPROFILE", "LOCALAPPDATA", "APPDATA",
			"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE":
			result = append(result, entry)
		}
	}
	return append(result, "LIGHTPANDA_DISABLE_TELEMETRY=true", "LIGHTPANDA_DISABLE_CORE_DUMP=true")
}
