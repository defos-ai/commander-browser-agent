package browser

import (
	"context"
	"errors"

	"github.com/defos-ai/commander-browser-agent/internal/hermes"
)

// Runtime owns a retained process and lends its private execution context only
// for the current callback. Health and close remain independent of that work.
// It exposes neither browser endpoints nor a native worker handle.
type Runtime interface {
	Done() <-chan struct{}
	Verify(context.Context) error
	Close() error
	WithContext(context.Context, func(context.Context) error) error
}

var _ Runtime = (*browserRuntime)(nil)

// Runtime health classifications let session owners retain their own inventory
// policy without depending on the private native bridge.
var (
	ErrRuntimeLost         = hermes.ErrLost
	ErrRuntimeClosed       = hermes.ErrClosed
	ErrRuntimeUncertain    = hermes.ErrUncertain
	ErrRuntimeNotConnected = hermes.ErrNotConnected
)

// Start launches the explicitly selected Go browser runtime. Failed startup can
// return an owned runtime alongside its error when cleanup remains uncertain.
func Start(ctx context.Context, options Options) (Runtime, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	runtime, err := launchBrowser(ctx, options)
	if runtime == nil {
		return nil, err
	}
	return runtime, err
}

func (*browserRuntime) String() string   { return "private browser runtime" }
func (*browserRuntime) GoString() string { return "private browser runtime" }

// WithContext installs the retained Go transport and renderer lease while
// preserving the caller's execution values and cancellation lifetime.
func (r *browserRuntime) WithContext(ctx context.Context, work func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.page == nil {
		return ErrRuntimeClosed
	}
	if r.page.Err() != nil {
		return ErrRuntimeLost
	}
	if work == nil {
		return errors.New("browser session work is required")
	}
	workCtx, cancel := context.WithCancel(ctx)
	workCtx = withRendererRuntime(workCtx, r)
	stopBrowser := context.AfterFunc(r.page, cancel)
	defer func() { stopBrowser(); cancel() }()
	merged := browserContext{Context: workCtx, browser: context.WithoutCancel(r.page)}
	err := work(merged)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		err = workCtx.Err()
	}
	return err
}

// The browser context supplies chromedp's private target key; execution values
// remain available from the action context. Cancellation belongs to this call.
type browserContext struct {
	context.Context
	browser context.Context
}

func (ctx browserContext) Value(key any) any {
	if value := ctx.browser.Value(key); value != nil {
		return value
	}
	return ctx.Context.Value(key)
}
