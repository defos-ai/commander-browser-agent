// Package textrender owns temporary, unauthenticated Lightpanda renderers.
package textrender

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander/framework/agent/action"
)

const stepTimeout = 8 * time.Second
const cleanupTimeout = 3 * time.Second
const TextLimit = 64 << 10
const pngBytes = 32 << 20

var errRenderer = errors.New("Lightpanda text rendering could not be completed")
var errCleanup = errors.New("renderer cleanup could not be confirmed; inspect the browser session")

// Owner retains renderer cleanup after a bounded Render or Wait returns. The
// endpoint passed to Render must identify the owner's already-running process.
type Owner struct {
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex
	stopping      bool
	cleanupFailed bool
	renders       sync.WaitGroup
	waitOnce      sync.Once
	joined        chan struct{}
	uncertainOnce sync.Once
	onUncertain   func()
}

// New constructs no connections or processes.
func New(owner context.Context, onUncertain func()) *Owner {
	ctx, cancel := context.WithCancel(owner)
	return &Owner{ctx: ctx, cancel: cancel, joined: make(chan struct{}), onUncertain: onUncertain}
}

// Validate applies the native renderer's text and image limits before admission.
func Validate(text string, width, height int) error {
	if width < 128 || width > 2048 || height < 128 || height > 2048 || int64(width)*int64(height) > 4_000_000 ||
		len(text) > TextLimit || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		return errors.New("text rendering requires bounded UTF-8 text and supported image dimensions")
	}
	return nil
}

// Stop closes admission and interrupts active rendering. Cleanup stays owned
// until Wait confirms that every target, allocator and socket has stopped.
func (o *Owner) Stop() {
	o.mu.Lock()
	o.stopping = true
	o.mu.Unlock()
	o.cancel()
}

// Wait first stops admission. Its caller may stop waiting without abandoning
// the cleanup workers; a later Wait can confirm their actual completion.
func (o *Owner) Wait(ctx context.Context) error {
	o.Stop()
	o.waitOnce.Do(func() {
		go func() {
			o.renders.Wait()
			close(o.joined)
		}()
	})
	select {
	case <-o.joined:
		o.mu.Lock()
		failed := o.cleanupFailed
		o.mu.Unlock()
		if failed {
			return errCleanup
		}
		return nil
	case <-ctx.Done():
		o.uncertain()
		return errors.Join(errCleanup, contextError(ctx))
	}
}

func (o *Owner) uncertain() {
	o.mu.Lock()
	o.cleanupFailed = true
	o.mu.Unlock()
	o.Stop()
	o.uncertainOnce.Do(func() {
		// A callback may itself wait for cleanup. Never execute it while holding
		// a lock or as part of a renderer worker that Wait needs to join.
		if o.onUncertain != nil {
			go o.onUncertain()
		}
	})
}

// Render receives already-protected plain text, never source HTML or browser
// state. lease must be the exact cancellable callback lifetime that lent access.
// PNG bytes are returned only after the temporary connection has been cleaned.
func (o *Owner) Render(ctx context.Context, lease context.Context, endpoint, text string, width, height int) (png []byte, renderErr error) {
	if ctx.Err() != nil {
		return nil, contextError(ctx)
	}
	if err := Validate(text, width, height); err != nil {
		return nil, err
	}
	if lease == nil || lease.Done() == nil || endpoint == "" {
		return nil, errors.New("text rendering requires a retained managed browser session")
	}
	if lease.Err() != nil {
		return nil, contextError(lease)
	}
	o.mu.Lock()
	if lease.Err() != nil {
		o.mu.Unlock()
		return nil, contextError(lease)
	}
	if o.stopping || o.ctx.Err() != nil {
		o.mu.Unlock()
		return nil, errors.New("the browser session is no longer available for rendering")
	}
	o.renders.Add(1)
	o.mu.Unlock()

	lifetime, cancelLifetime := context.WithCancelCause(ctx)
	stopLease := context.AfterFunc(lease, func() { cancelLifetime(context.Cause(lease)) })
	stopOwner := context.AfterFunc(o.ctx, func() { cancelLifetime(context.Cause(o.ctx)) })
	allocator, cancelAllocator := chromedp.NewRemoteAllocator(lifetime, endpoint, chromedp.NoModifyURL)
	renderer, _ := chromedp.NewContext(allocator,
		chromedp.WithLogf(func(string, ...any) {}),
		chromedp.WithErrorf(func(string, ...any) {}),
		chromedp.WithBrowserOption(chromedp.WithDialTimeout(time.Second)),
	)
	defer func() {
		stopLease()
		stopOwner()
		cancelLifetime(context.Canceled)
		cleaned := make(chan error, 1)
		go func() {
			err := chromedp.Cancel(renderer)
			cancelAllocator()
			connection := chromedp.FromContext(renderer)
			connection.Allocator.Wait()
			if connection.Browser != nil {
				<-connection.Browser.LostConnection
			}
			if err != nil {
				o.mu.Lock()
				o.cleanupFailed = true
				o.mu.Unlock()
				o.uncertain()
			}
			o.renders.Done()
			cleaned <- err
		}()
		deadline := time.NewTimer(cleanupTimeout)
		defer deadline.Stop()
		select {
		case err := <-cleaned:
			if err != nil {
				png = nil
				renderErr = errors.Join(renderErr, errCleanup)
			}
		case <-deadline.C:
			png = nil
			renderErr = errors.Join(renderErr, errCleanup)
			o.uncertain()
		}
		if ctx.Err() != nil {
			png = nil
			renderErr = errors.Join(renderErr, contextError(ctx))
		} else if lease.Err() != nil {
			png = nil
			renderErr = errors.Join(renderErr, contextError(lease))
		} else if o.ctx.Err() != nil {
			png = nil
			renderErr = errors.Join(renderErr, contextError(o.ctx))
		}
	}()

	// The first Run inherits the full renderer lifetime; ending a temporary
	// startup context would close the new CDP connection immediately.
	startup := time.AfterFunc(stepTimeout, func() { cancelLifetime(context.DeadlineExceeded) })
	err := chromedp.Run(renderer)
	started := startup.Stop()
	if err != nil || !started || lifetime.Err() != nil {
		if lifetime.Err() != nil {
			return nil, contextError(lifetime)
		}
		return nil, errRenderer
	}
	bounded, stop := context.WithTimeout(renderer, stepTimeout)
	err = chromedp.Run(bounded, emulation.SetDeviceMetricsOverride(int64(width), int64(height), 1, false),
		chromedp.ActionFunc(func(ctx context.Context) error {
			tree, err := page.GetFrameTree().Do(ctx)
			if err != nil || tree == nil || tree.Frame == nil || tree.Frame.URL != "about:blank" {
				return errRenderer
			}
			world, err := page.CreateIsolatedWorld(tree.Frame.ID).WithWorldName("commander-text-renderer").Do(ctx)
			if err != nil {
				return errRenderer
			}
			encoded, err := json.Marshal(text)
			if err != nil {
				return errRenderer
			}
			defer clear(encoded)
			value, exception, err := cdpruntime.CallFunctionOn(renderTextDOM).
				WithExecutionContextID(world).WithArguments([]*cdpruntime.CallArgument{{Value: encoded}}).
				WithSilent(true).WithReturnByValue(true).Do(ctx)
			if err != nil || exception != nil || value == nil || string(value.Value) != "true" {
				return errRenderer
			}
			return nil
		}))
	stop()
	if err != nil {
		if lifetime.Err() != nil {
			return nil, contextError(lifetime)
		}
		return nil, errRenderer
	}
	if err := action.Log(lifetime, "Rendering the protected screenshot"); err != nil {
		if lifetime.Err() != nil {
			return nil, contextError(lifetime)
		}
		return nil, errRenderer
	}
	bounded, stop = context.WithTimeout(renderer, stepTimeout)
	err = chromedp.Run(bounded, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		png, err = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(ctx)
		return err
	}))
	stop()
	if err != nil || len(png) == 0 || len(png) > pngBytes {
		if lifetime.Err() != nil {
			return nil, contextError(lifetime)
		}
		return nil, errRenderer
	}
	return png, nil
}

func contextError(ctx context.Context) error {
	return errors.Join(ctx.Err(), context.Cause(ctx))
}

const renderTextDOM = `function(text) {
 const root=document.documentElement||document.appendChild(document.createElement('html'));
 let body=document.body;
 if(!body) { body=document.createElement('body');root.appendChild(body); }
 body.textContent='';
 const pre=document.createElement('pre');pre.textContent=text;body.appendChild(pre);
 return true;
}`
