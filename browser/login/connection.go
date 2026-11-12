package login

import (
	"context"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// connection only adapts a retained transport. Form policy and network guarding
// belong to driver, independently of the selected host runtime.
type connection interface {
	cdp.Executor
	Listen(context.Context, func(any), func()) error
	Navigate(context.Context, string) error
	CloseTarget(context.Context) error
	Close(context.Context) error
}

type goConnection struct {
	target  *chromedp.Target
	browser *chromedp.Browser
}

func defaultConnection(ctx context.Context) (connection, error) {
	current := chromedp.FromContext(ctx)
	if current == nil || current.Target == nil || current.Browser == nil || ctx.Err() != nil {
		return nil, errBrowser
	}
	return &goConnection{target: current.Target, browser: current.Browser}, nil
}

func (c *goConnection) Execute(ctx context.Context, method string, params, result any) error {
	return c.target.Execute(ctx, method, params, result)
}

func (c *goConnection) Listen(ctx context.Context, event func(any), _ func()) error {
	chromedp.ListenTarget(ctx, event)
	return nil
}

func (*goConnection) Navigate(ctx context.Context, address string) error {
	return chromedp.Run(ctx, chromedp.Navigate(address))
}

func (c *goConnection) CloseTarget(ctx context.Context) error {
	return target.CloseTarget(c.target.TargetID).Do(cdp.WithExecutor(ctx, c.browser))
}

// The manager owns the retained Go transport. Listener cancellation is owned by
// driver's guard context; closing this adapter does not close the browser.
func (*goConnection) Close(context.Context) error { return nil }
