package login

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
)

// RunHermes executes the same deterministic login flow on the native worker's
// retained connection. Credentials and codes never enter an agent invocation.
func RunHermes(ctx context.Context, request Request, credentials func(context.Context) (Credentials, error)) (result Result, runErr error) {
	result = Result{Status: "unverified", Reason: "not_attempted"}
	if _, err := validateRequest(ctx, request, credentials); err != nil {
		return result, err
	}
	runErr = hermes.WithBrowser(ctx, func(browser *hermes.Browser) error {
		var err error
		result, err = run(ctx, request, credentials, func(ctx context.Context, origin string) (*driver, error) {
			return newDriverWithConnection(ctx, origin, &hermesConnection{browser: browser})
		})
		return err
	})
	return result, runErr
}

type hermesConnection struct {
	browser *hermes.Browser
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

func (c *hermesConnection) Execute(ctx context.Context, method string, params, result any) error {
	if c.browser.Execute(ctx, method, params, result) != nil {
		return errBrowser
	}
	return nil
}

func (c *hermesConnection) Listen(ctx context.Context, event func(any), failed func()) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errBrowser
	}
	watching, cancel := context.WithCancel(ctx)
	c.cancel, c.done = cancel, make(chan struct{})
	bounded, stop := context.WithTimeout(watching, 8*time.Second)
	defer stop()
	executor := cdp.WithExecutor(bounded, c)
	if page.Enable().Do(executor) != nil || cdpruntime.Enable().Do(executor) != nil || network.Enable().Do(executor) != nil {
		cancel()
		close(c.done)
		return errBrowser
	}
	go c.listen(watching, event, failed)
	return nil
}

func (c *hermesConnection) listen(ctx context.Context, event func(any), failed func()) {
	defer close(c.done)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		bounded, stop := context.WithTimeout(ctx, time.Second)
		events, err := c.browser.Drain(bounded)
		stop()
		if err != nil || len(events) > 128 {
			if ctx.Err() == nil {
				failed()
			}
			return
		}
		for _, item := range events {
			decoded, err := loginEvent(item)
			clear(item.Params)
			if err != nil {
				if ctx.Err() == nil {
					failed()
				}
				return
			}
			event(decoded)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func loginEvent(event hermes.BrowserEvent) (any, error) {
	switch event.Method {
	case "Fetch.requestPaused":
		var value fetch.EventRequestPaused
		if json.Unmarshal(event.Params, &value) != nil || value.RequestID == "" || value.Request == nil || value.Request.URL == "" {
			return nil, errBrowser
		}
		return &value, nil
	case "Network.requestWillBeSent":
		var value network.EventRequestWillBeSent
		if json.Unmarshal(event.Params, &value) != nil || value.RequestID == "" || value.Request == nil || value.Request.Method == "" {
			return nil, errBrowser
		}
		return &value, nil
	case "Network.loadingFinished":
		var value network.EventLoadingFinished
		if json.Unmarshal(event.Params, &value) != nil || value.RequestID == "" {
			return nil, errBrowser
		}
		return &value, nil
	case "Network.responseReceived":
		var value network.EventResponseReceived
		if json.Unmarshal(event.Params, &value) != nil || value.RequestID == "" || value.Response == nil || value.Response.Status < 100 || value.Response.Status > 599 {
			return nil, errBrowser
		}
		return &value, nil
	case "Network.loadingFailed":
		var value network.EventLoadingFailed
		if json.Unmarshal(event.Params, &value) != nil || value.RequestID == "" {
			return nil, errBrowser
		}
		return &value, nil
	}
	return nil, errBrowser
}

func (c *hermesConnection) Navigate(ctx context.Context, address string) error {
	executor := cdp.WithExecutor(ctx, c)
	frame, loader, message, download, err := page.Navigate(address).Do(executor)
	if err != nil || message != "" || download || frame == "" || loader == "" {
		return errBrowser
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		tree, err := page.GetFrameTree().Do(executor)
		if err != nil || tree == nil || tree.Frame == nil {
			return errBrowser
		}
		if tree.Frame.ID == frame && tree.Frame.LoaderID == loader {
			value, exception, err := cdpruntime.Evaluate("document.readyState").WithSilent(true).WithReturnByValue(true).Do(executor)
			if err != nil || exception != nil || value == nil {
				return errBrowser
			}
			var state string
			if json.Unmarshal(value.Value, &state) != nil || state != "loading" && state != "interactive" && state != "complete" {
				return errBrowser
			}
			if state == "interactive" || state == "complete" {
				confirmed, err := page.GetFrameTree().Do(executor)
				if err != nil || confirmed == nil || confirmed.Frame == nil {
					return errBrowser
				}
				if confirmed.Frame.ID == frame && confirmed.Frame.LoaderID == loader {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errBrowser
		case <-ticker.C:
		}
	}
}

func (c *hermesConnection) CloseTarget(ctx context.Context) error {
	if c.browser.CloseTarget(ctx) != nil {
		return errBrowser
	}
	return nil
}

func (c *hermesConnection) Close(ctx context.Context) error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errBrowser
	}
}
