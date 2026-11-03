package browsing

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Budget counts active work while keeping clarification waits subject to
// their own timeout and the original task deadline. Its context keeps the
// parent's fixed Deadline; the active timer cancels it with DeadlineExceeded.
type Budget struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	mu        sync.Mutex
	remaining time.Duration
	started   time.Time
	timer     *time.Timer
	epoch     uint64
}

func NewBudget(parent context.Context, allowance time.Duration) *Budget {
	ctx, cancel := context.WithCancelCause(parent)
	b := &Budget{ctx: ctx, cancel: cancel, remaining: allowance}
	b.Resume()
	return b
}

func (b *Budget) Context() context.Context { return b.ctx }

func (b *Budget) Pause() {
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

func (b *Budget) Resume() {
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

func (b *Budget) Close() {
	b.Pause()
	b.cancel(context.Canceled)
}

func (b *Budget) Err() error {
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
