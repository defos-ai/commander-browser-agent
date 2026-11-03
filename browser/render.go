package browser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
)

const rendererStepTimeout = 8 * time.Second
const rendererCleanupTimeout = 3 * time.Second
const rendererTextLimit = 64 << 10
const rendererPNGBytes = 32 << 20

var errRenderer = errors.New("Lightpanda text rendering could not be completed")
var errRendererCleanup = errors.New("renderer cleanup could not be confirmed; inspect the browser session")

type rendererRuntimeKey struct{}

type rendererLease struct {
	runtime  *browserRuntime
	lifetime context.Context
}

func withRendererRuntime(ctx context.Context, runtime *browserRuntime) context.Context {
	return context.WithValue(ctx, rendererRuntimeKey{}, &rendererLease{runtime: runtime, lifetime: ctx})
}

// RenderText renders protected text with the native Lightpanda PNG renderer.
// It requires Manager.Run's retained-session context. A separate CDP connection
// to that same owned process isolates the renderer's cookies, page and viewport;
// it receives no authenticated browser state and never navigates to a website.
func RenderText(ctx context.Context, text string, width, height int) (png []byte, renderErr error) {
	if err := ctx.Err(); err != nil {
		return nil, rendererContextError(ctx)
	}
	if width < 128 || width > 2048 || height < 128 || height > 2048 || int64(width)*int64(height) > 4_000_000 ||
		len(text) > rendererTextLimit || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		return nil, errors.New("text rendering requires bounded UTF-8 text and supported image dimensions")
	}
	lease, _ := ctx.Value(rendererRuntimeKey{}).(*rendererLease)
	if lease == nil || lease.runtime == nil || lease.lifetime == nil {
		return nil, errors.New("text rendering requires a retained managed browser session")
	}
	runtime := lease.runtime
	if runtime.page == nil || runtime.endpoint == "" ||
		chromedp.FromContext(ctx) != chromedp.FromContext(runtime.page) {
		return nil, errors.New("text rendering requires a retained managed browser session")
	}
	if lease.lifetime.Err() != nil {
		return nil, rendererContextError(lease.lifetime)
	}
	text = sensitive.Redact(ctx, text)
	if len(text) > rendererTextLimit {
		return nil, errors.New("protected render text exceeds its supported limit")
	}
	runtime.renderMu.Lock()
	if lease.lifetime.Err() != nil {
		runtime.renderMu.Unlock()
		return nil, rendererContextError(lease.lifetime)
	}
	if runtime.renderClosing || runtime.ctx.Err() != nil {
		runtime.renderMu.Unlock()
		return nil, errors.New("the browser session is no longer available for rendering")
	}
	runtime.renderers.Add(1)
	runtime.renderMu.Unlock()

	lifetime, cancelLifetime := context.WithCancelCause(ctx)
	stopLease := context.AfterFunc(lease.lifetime, func() { cancelLifetime(context.Cause(lease.lifetime)) })
	stopRuntime := context.AfterFunc(runtime.ctx, func() { cancelLifetime(context.Canceled) })
	allocator, cancelAllocator := chromedp.NewRemoteAllocator(lifetime, runtime.endpoint, chromedp.NoModifyURL)
	renderer, _ := chromedp.NewContext(allocator,
		chromedp.WithLogf(func(string, ...any) {}),
		chromedp.WithErrorf(func(string, ...any) {}),
		chromedp.WithBrowserOption(chromedp.WithDialTimeout(time.Second)),
	)
	defer func() {
		stopLease()
		stopRuntime()
		cancelLifetime(context.Canceled)
		cleaned := make(chan error, 1)
		// Keep ownership after a bounded caller returns. Source shutdown joins
		// this worker before reporting cleanup or removing its private profile.
		go func() {
			defer runtime.renderers.Done()
			err := chromedp.Cancel(renderer)
			cancelAllocator()
			connection := chromedp.FromContext(renderer)
			connection.Allocator.Wait()
			if connection.Browser != nil {
				<-connection.Browser.LostConnection
			}
			cleaned <- err
		}()
		deadline := time.NewTimer(rendererCleanupTimeout)
		defer deadline.Stop()
		select {
		case err := <-cleaned:
			if err != nil {
				png = nil
				renderErr = errors.Join(renderErr, errRendererCleanup)
				runtime.cancel()
			}
		case <-deadline.C:
			png = nil
			renderErr = errors.Join(renderErr, errRendererCleanup)
			runtime.cancel()
		}
		if ctx.Err() != nil {
			png = nil
			renderErr = errors.Join(renderErr, rendererContextError(ctx))
		} else if lease.lifetime.Err() != nil {
			png = nil
			renderErr = errors.Join(renderErr, rendererContextError(lease.lifetime))
		}
	}()

	// Allocation must inherit the full renderer lifetime. Canceling a short
	// first-Run context after startup would close this new CDP connection.
	startup := time.AfterFunc(rendererStepTimeout, func() { cancelLifetime(context.DeadlineExceeded) })
	err := chromedp.Run(renderer)
	started := startup.Stop()
	if err != nil || !started || lifetime.Err() != nil {
		if lifetime.Err() != nil {
			return nil, rendererContextError(lifetime)
		}
		return nil, errRenderer
	}
	bounded, stop := context.WithTimeout(renderer, rendererStepTimeout)
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
			return nil, rendererContextError(lifetime)
		}
		return nil, errRenderer
	}
	if err := action.Log(lifetime, "Rendering the protected screenshot"); err != nil {
		if lifetime.Err() != nil {
			return nil, rendererContextError(lifetime)
		}
		return nil, errRenderer
	}
	bounded, stop = context.WithTimeout(renderer, rendererStepTimeout)
	err = chromedp.Run(bounded, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		png, err = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).Do(ctx)
		return err
	}))
	stop()
	if err != nil || len(png) == 0 || len(png) > rendererPNGBytes {
		if lifetime.Err() != nil {
			return nil, rendererContextError(lifetime)
		}
		return nil, errRenderer
	}
	return png, nil
}

func rendererContextError(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return context.Canceled
}

const renderTextDOM = `function(text) {
 const root=document.documentElement||document.appendChild(document.createElement('html'));
 let body=document.body;
 if(!body) { body=document.createElement('body');root.appendChild(body); }
 body.textContent='';
 const pre=document.createElement('pre');pre.textContent=text;body.appendChild(pre);
 return true;
}`
