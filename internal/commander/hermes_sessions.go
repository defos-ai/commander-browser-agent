package commander

import (
	"context"
	"errors"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

// RunHermes retains one native worker under the exact Commander creation claim.
// The callback receives a scoped private bridge, accessible to the agent's
// internal Hermes adapter. Returning from it revokes that bridge scope.
func (m *SessionManager) RunHermes(ctx context.Context, options browserruntime.HermesOptions, work func(context.Context) error) (view sessions.Session, runErr error) {
	if err := ctx.Err(); err != nil {
		return view, errors.Join(err, context.Cause(ctx))
	}
	if err := options.Validate(); err != nil {
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
	worker, err := m.hermesRuntime(ctx, item, binding.Mode, options)
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
			cancel(browserruntime.ErrRuntimeLost)
		case <-workCtx.Done():
		}
	}()
	stopLifetime := context.AfterFunc(item.lifetime, func() { cancel(sessions.ErrSessionClosing) })
	defer func() { stopLifetime(); cancel(nil); <-watchDone }()
	runErr = worker.WithContext(workCtx, work)
	if workCtx.Err() != nil {
		runErr = errors.Join(runErr, workCtx.Err(), context.Cause(workCtx))
	}
	// Verification is an independent bounded control probe. It must not turn
	// cancellation of one action into an automatic browser replacement.
	if workCtx.Err() == nil {
		if err := m.refreshHermes(workCtx, item, worker); err != nil && !errors.Is(err, browserruntime.ErrRuntimeNotConnected) {
			runErr = errors.Join(runErr, err)
		}
	}
	return view, runErr
}

func (m *SessionManager) hermesRuntime(ctx context.Context, item *entry, mode sessions.Mode, options browserruntime.HermesOptions) (browserruntime.Runtime, error) {
	m.mu.Lock()
	if err := m.availableLocked(item); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if item.backend != "" && (item.backend != "hermes" || item.hermesOptions != options) {
		m.mu.Unlock()
		return nil, errors.New("browser session belongs to a different runtime")
	}
	if worker := item.resource; worker != nil {
		m.mu.Unlock()
		err := m.refreshHermes(ctx, item, worker)
		if err != nil && !(mode == sessions.Open && errors.Is(err, browserruntime.ErrRuntimeNotConnected)) {
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
	item.backend, item.hermesOptions, item.engine = "hermes", options, browserruntime.Lightpanda
	item.view.Kind, item.view.Label, item.view.Busy = string(browserruntime.Lightpanda), "Lightpanda browser session (Hermes)", true
	item.lifetime, item.cancel = context.WithCancel(context.Background())
	item.startDone = make(chan struct{})
	m.touchLocked(item)
	m.mu.Unlock()
	startupCancelled := make(chan struct{})
	stopStartup := context.AfterFunc(ctx, func() {
		item.cancel()
		close(startupCancelled)
	})
	worker, err := browserruntime.StartHermes(item.lifetime, options, item.claim.ID)
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

func (m *SessionManager) refreshHermes(ctx context.Context, item *entry, worker browserruntime.Runtime) error {
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
func (m *SessionManager) applyHermesHealthLocked(item *entry, err error, revision uint64) {
	lost := errors.Is(err, browserruntime.ErrRuntimeLost) || errors.Is(err, browserruntime.ErrRuntimeClosed) || errors.Is(err, browserruntime.ErrRuntimeUncertain)
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
	case errors.Is(err, browserruntime.ErrRuntimeNotConnected) && item.view.ConnectedAt == nil:
		item.view.State, item.view.Reason, item.view.VerifiedAt = "starting", "Browser has not been opened", nil
	default:
		item.view.State, item.view.Reason, item.view.VerifiedAt = "unverified", "Browser connection could not be verified", nil
	}
	m.touchLocked(item)
}

func (m *SessionManager) monitorHermes(item *entry, worker browserruntime.Runtime) {
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
