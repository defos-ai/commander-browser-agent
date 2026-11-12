package browser

import (
	"context"
	"errors"

	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander-browser-agent/internal/textrender"
)

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
func RenderText(ctx context.Context, text string, width, height int) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, errors.Join(ctx.Err(), context.Cause(ctx))
	}
	if err := textrender.Validate(text, width, height); err != nil {
		return nil, err
	}
	lease, _ := ctx.Value(rendererRuntimeKey{}).(*rendererLease)
	if lease == nil || lease.runtime == nil || lease.lifetime == nil {
		return nil, errors.New("text rendering requires a retained managed browser session")
	}
	runtime := lease.runtime
	if runtime.page == nil || runtime.endpoint == "" || runtime.renderer == nil ||
		chromedp.FromContext(ctx) != chromedp.FromContext(runtime.page) {
		return nil, errors.New("text rendering requires a retained managed browser session")
	}
	if lease.lifetime.Err() != nil {
		return nil, errors.Join(lease.lifetime.Err(), context.Cause(lease.lifetime))
	}
	text = sensitive.Redact(ctx, text)
	if len(text) > textrender.TextLimit {
		return nil, errors.New("protected render text exceeds its supported limit")
	}
	return runtime.renderer.Render(ctx, lease.lifetime, runtime.endpoint, text, width, height)
}
