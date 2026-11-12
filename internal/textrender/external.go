package textrender

import (
	"context"
	"errors"
)

// RenderUsing owns an external renderer for the same callback lifetime as a
// local renderer. render must join its connection and process before returning;
// a missing cleanup receipt must be reported to the owning session as uncertain.
func (o *Owner) RenderUsing(ctx, lease context.Context, text string, width, height int,
	render func(context.Context) ([]byte, error),
) ([]byte, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := Validate(text, width, height); err != nil {
		return nil, err
	}
	if lease == nil || lease.Done() == nil || render == nil {
		return nil, errRenderer
	}
	o.mu.Lock()
	if o.stopping || o.ctx.Err() != nil || lease.Err() != nil {
		o.mu.Unlock()
		return nil, errors.Join(errRenderer, contextError(lease), contextError(o.ctx))
	}
	o.renders.Add(1)
	o.mu.Unlock()
	defer o.renders.Done()

	lifetime, cancel := context.WithCancelCause(ctx)
	stopLease := context.AfterFunc(lease, func() { cancel(context.Cause(lease)) })
	stopOwner := context.AfterFunc(o.ctx, func() { cancel(context.Cause(o.ctx)) })
	defer stopLease()
	defer stopOwner()
	defer cancel(context.Canceled)
	png, err := render(lifetime)
	if ctx.Err() != nil || lease.Err() != nil || o.ctx.Err() != nil {
		clear(png)
		return nil, errors.Join(err, contextError(ctx), contextError(lease), contextError(o.ctx))
	}
	return png, err
}
