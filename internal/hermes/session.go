package hermes

import (
	"context"
	"errors"
	"sync"
)

type workerContextKey struct{}

type workerGrant struct {
	mu     sync.Mutex
	active bool
	worker *Worker
	ctx    context.Context
	uses   sync.WaitGroup
}

// WithWorker lends one manager-owned worker to a synchronous session callback.
// A retained context never becomes valid again for a later callback.
func WithWorker(ctx context.Context, worker *Worker, work func(context.Context) error) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if worker == nil || work == nil {
		return ErrUnavailable
	}
	select {
	case <-worker.Done():
		return ErrClosed
	default:
	}
	lifetime, stop := context.WithCancelCause(ctx)
	ownerWatch := context.AfterFunc(worker.lifetime, func() { stop(context.Cause(worker.lifetime)) })
	defer ownerWatch()
	grant := &workerGrant{active: true, worker: worker, ctx: lifetime}
	defer func() {
		grant.mu.Lock()
		grant.active = false
		grant.mu.Unlock()
		stop(ErrClosed)
		grant.uses.Wait()
	}()
	return work(context.WithValue(lifetime, workerContextKey{}, grant))
}

// Run uses the private grant supplied by the manager's current callback. It
// exposes no worker handle, and rejects detached or expired session contexts.
func Run(ctx context.Context, request Request, handler Handler) (Result, error) {
	var result Result
	err := withGrantedWorker(ctx, func(active context.Context, worker *Worker) error {
		var err error
		result, err = worker.Run(active, request, handler)
		return err
	})
	return result, err
}

func withGrantedWorker(ctx context.Context, work func(context.Context, *Worker) error) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	grant, ok := ctx.Value(workerContextKey{}).(*workerGrant)
	if !ok || grant == nil || ctx.Done() == nil {
		return ErrClosed
	}
	grant.mu.Lock()
	if !grant.active || grant.ctx.Err() != nil {
		grant.mu.Unlock()
		return errors.Join(ErrClosed, contextError(grant.ctx))
	}
	grant.uses.Add(1)
	worker := grant.worker
	grant.mu.Unlock()
	defer grant.uses.Done()
	active, stop := context.WithCancelCause(ctx)
	defer stop(context.Canceled)
	watch := context.AfterFunc(grant.ctx, func() { stop(context.Cause(grant.ctx)) })
	defer watch()
	return work(active, worker)
}
