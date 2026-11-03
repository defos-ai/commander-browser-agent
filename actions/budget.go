package actions

import (
	"context"
	"errors"
	"sync"
	"time"
)

// browseBudget counts active work while keeping clarification waits subject to
// their own timeout and the original task deadline. Its context keeps the
// parent's fixed Deadline; the active timer cancels it with DeadlineExceeded.
type browseBudget struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	mu        sync.Mutex
	remaining time.Duration
	started   time.Time
	timer     *time.Timer
	epoch     uint64
}

func newBrowseBudget(parent context.Context, allowance time.Duration) *browseBudget {
	ctx, cancel := context.WithCancelCause(parent)
	b := &browseBudget{ctx: ctx, cancel: cancel, remaining: allowance}
	b.resume()
	return b
}

func (b *browseBudget) pause() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer == nil {
		return
	}
	b.timer.Stop()
	b.timer = nil
	b.epoch++
	b.remaining -= time.Since(b.started)
	if b.remaining <= 0 {
		b.cancel(context.DeadlineExceeded)
	}
}

func (b *browseBudget) resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil || b.ctx.Err() != nil {
		return
	}
	b.epoch++
	epoch := b.epoch
	b.started = time.Now()
	b.timer = time.AfterFunc(b.remaining, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.epoch == epoch {
			b.cancel(context.DeadlineExceeded)
		}
	})
}

func (b *browseBudget) close() {
	b.pause()
	b.cancel(context.Canceled)
}

func (b *browseBudget) err() error {
	if b.ctx.Err() == nil {
		return nil
	}
	if errors.Is(context.Cause(b.ctx), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	// A caller's cancellation cause can contain private values. Keep the standard
	// classification at the result boundary without exposing that cause's text.
	return context.Canceled
}
