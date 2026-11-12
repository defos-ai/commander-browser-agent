package browser

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

// HermesOptions are trusted discovered runtime paths. They are not action input
// or browser session selectors. Lightpanda is the only supported engine.
type HermesOptions struct {
	Python     string
	Source     string
	Executable string
}

func (o HermesOptions) validate() error {
	if !filepath.IsAbs(o.Python) || !filepath.IsAbs(o.Source) || !filepath.IsAbs(o.Executable) {
		return errors.New("Hermes requires absolute discovered runtime paths")
	}
	return nil
}

// RunHermes retains one native worker under the exact Commander creation claim.
// The callback receives a scoped private bridge, accessible to the agent's
// internal Hermes adapter. Returning from it revokes that bridge scope.
func (m *Manager) RunHermes(ctx context.Context, options HermesOptions, work func(context.Context) error) (view sessions.Session, runErr error) {
	if err := ctx.Err(); err != nil {
		return view, errors.Join(err, context.Cause(ctx))
	}
	if err := options.validate(); err != nil {
		return view, err
	}
	if work == nil {
		return view, errors.New("browser session work is required")
	}
	binding, ok := action.SessionFromContext(ctx)
	if !ok {
		return view, errors.New("browser work requires a framework session binding")
	}
	if err := binding.Validate(); err != nil {
		return view, err
	}
	m.mu.Lock()
	item := m.items[binding.ID]
	if item == nil {
		m.mu.Unlock()
		return view, sessions.ErrUnknownSession
	}
	if item.claim != binding.Claim() {
		m.mu.Unlock()
		return view, sessions.ErrConflict
	}
	m.mu.Unlock()
	select {
	case item.gate <- struct{}{}:
		defer func() { <-item.gate }()
	case <-ctx.Done():
		return view, errors.Join(ctx.Err(), context.Cause(ctx))
	case <-item.stopping:
		m.mu.Lock()
		defer m.mu.Unlock()
		return item.view.Clone(), m.availableLocked(item)
	}
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if item.view.Busy {
			item.view.Busy = false
			m.touchLocked(item)
		}
		view = item.view.Clone()
	}()
	worker, err := m.hermesWorker(ctx, item, binding.Mode, options)
	if err != nil {
		return view, err
	}
	workCtx, cancel := context.WithCancelCause(ctx)
	workCtx = sensitive.WithValues(workCtx, item.values)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-worker.Done():
			cancel(hermes.ErrLost)
		case <-workCtx.Done():
		}
	}()
	stopLifetime := context.AfterFunc(item.lifetime, func() { cancel(sessions.ErrSessionClosing) })
	defer func() { stopLifetime(); cancel(nil); <-watchDone }()
	runErr = hermes.WithWorker(workCtx, worker, work)
	if workCtx.Err() != nil {
		runErr = errors.Join(runErr, workCtx.Err(), context.Cause(workCtx))
	}
	// Verification is an independent bounded control probe. It must not turn
	// cancellation of one action into an automatic browser replacement.
	if workCtx.Err() == nil {
		if err := m.refreshHermes(workCtx, item, worker); err != nil && !errors.Is(err, hermes.ErrNotConnected) {
			runErr = errors.Join(runErr, err)
		}
	}
	return view, runErr
}

func (m *Manager) hermesWorker(ctx context.Context, item *entry, mode sessions.Mode, options HermesOptions) (*hermes.Worker, error) {
	m.mu.Lock()
	if err := m.availableLocked(item); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if item.backend != "" && (item.backend != "hermes" || item.hermesPaths != options) {
		m.mu.Unlock()
		return nil, errors.New("browser session belongs to a different runtime")
	}
	if worker, ok := item.resource.(*hermes.Worker); ok {
		m.mu.Unlock()
		err := m.refreshHermes(ctx, item, worker)
		if err != nil && !(mode == sessions.Open && errors.Is(err, hermes.ErrNotConnected)) {
			return nil, err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := m.availableLocked(item); err != nil {
			return nil, err
		}
		item.view.Busy = true
		m.touchLocked(item)
		return worker, nil
	}
	if mode != sessions.Open || item.backend != "" {
		m.mu.Unlock()
		return nil, errors.New("browser session has not been started")
	}
	item.backend, item.hermesPaths, item.engine = "hermes", options, Lightpanda
	item.view.Kind, item.view.Label, item.view.Busy = string(Lightpanda), "Lightpanda browser session (Hermes)", true
	item.lifetime, item.cancel = context.WithCancel(context.Background())
	item.startDone = make(chan struct{})
	m.touchLocked(item)
	m.mu.Unlock()
	startupCancelled := make(chan struct{})
	stopStartup := context.AfterFunc(ctx, func() {
		item.cancel()
		close(startupCancelled)
	})
	worker, err := hermes.Start(item.lifetime, hermes.Config{
		Python: options.Python, Source: options.Source, Lightpanda: options.Executable, Claim: item.claim,
	})
	if !stopStartup() {
		<-startupCancelled
	}
	m.mu.Lock()
	if worker != nil {
		item.resource = worker
	}
	close(item.startDone)
	if err == nil && worker == nil {
		err = errors.New("Hermes startup returned no runtime")
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), context.Cause(ctx), err)
	} else if err == nil && (item.closing || m.stopped || item.lifetime.Err() != nil) {
		err = sessions.ErrSessionClosing
	}
	if err != nil {
		if !item.closing && !m.stopped {
			item.view.State, item.view.Reason = "failed", "Hermes worker startup failed"
			m.touchLocked(item)
		}
		m.cleanupLocked(item, false)
		m.mu.Unlock()
		return nil, err
	}
	item.view.State, item.view.Reason = "starting", "Browser has not been opened"
	m.touchLocked(item)
	m.mu.Unlock()
	go m.monitor(item, worker)
	go m.monitorHermes(item, worker)
	return worker, nil
}

func (m *Manager) refreshHermes(ctx context.Context, item *entry, worker *hermes.Worker) error {
	m.mu.Lock()
	if err := m.availableLocked(item); err != nil {
		m.mu.Unlock()
		return err
	}
	revision := item.view.Revision
	m.mu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := worker.Verify(bounded)
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), context.Cause(ctx))
	}
	if unavailable := m.availableLocked(item); unavailable != nil {
		return unavailable
	}
	m.applyHermesHealthLocked(item, err, revision)
	return err
}

// Loss is sticky and authoritative. Other receipts describe their probe's
// starting revision, and must not overwrite a newer inventory observation.
func (m *Manager) applyHermesHealthLocked(item *entry, err error, revision uint64) {
	lost := errors.Is(err, hermes.ErrLost) || errors.Is(err, hermes.ErrClosed) || errors.Is(err, hermes.ErrUncertain)
	if !lost && item.view.Revision != revision {
		return
	}
	switch {
	case err == nil:
		now := time.Now().UTC()
		item.view.State, item.view.Reason, item.view.VerifiedAt = "connected", "", &now
		if item.view.ConnectedAt == nil {
			item.view.ConnectedAt = &now
		}
	case lost:
		item.view.State, item.view.Reason, item.view.VerifiedAt = "lost", "Browser connection was lost", nil
		m.cleanupLocked(item, false)
	case errors.Is(err, hermes.ErrNotConnected) && item.view.ConnectedAt == nil:
		item.view.State, item.view.Reason, item.view.VerifiedAt = "starting", "Browser has not been opened", nil
	default:
		item.view.State, item.view.Reason, item.view.VerifiedAt = "unverified", "Browser connection could not be verified", nil
	}
	m.touchLocked(item)
}

func (m *Manager) monitorHermes(item *entry, worker *hermes.Worker) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-item.stopping:
			return
		case <-worker.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			idle := !item.closing && !item.view.Busy && !m.stopped
			m.mu.Unlock()
			if idle {
				_ = m.refreshHermes(item.lifetime, item, worker)
			}
		}
	}
}
