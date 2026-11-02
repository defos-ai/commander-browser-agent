// Package browser owns browser state for one agent process.
package browser

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

// Manager implements Commander session inventory and controls. Browser processes
// are created only by Run, after the framework has prepared their identities.
type Manager struct {
	owner     string
	mu        sync.Mutex
	items     map[string]*entry
	changes   chan struct{}
	stopped   bool
	closeOnce sync.Once
	closeErr  error
}

type entry struct {
	claim       sessions.CreationClaim
	view        sessions.Session
	engine      Engine
	gate        chan struct{}
	stopping    chan struct{}
	lifetime    context.Context
	cancel      context.CancelFunc
	startDone   chan struct{}
	browser     *browserRuntime
	closing     bool
	explicit    bool
	cleanupDone chan struct{}
	cleaned     bool
	cleanupErr  error
}

var _ sessions.Provider = (*Manager)(nil)

func New(ownerAgentID string) (*Manager, error) {
	if !id.ValidAgent(ownerAgentID) {
		return nil, errors.New("browser sessions require an exact owning agent ID")
	}
	return &Manager{owner: ownerAgentID, items: map[string]*entry{}, changes: make(chan struct{}, 1)}, nil
}

// Prepare retains an unstarted framework-issued claim. Commander owns scope
// admission; distinct identities remain independent even with the same ScopeID.
// Closed identities remain tombstoned for this manager's lifetime.
func (m *Manager) Prepare(ctx context.Context, claim sessions.CreationClaim) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := claim.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.prepareLocked(claim)
	return err
}

func (m *Manager) prepareLocked(claim sessions.CreationClaim) (*entry, error) {
	if claim.OwnerAgentID != m.owner {
		return nil, sessions.ErrConflict
	}
	if old := m.items[claim.ID]; old != nil {
		if old.claim != claim {
			return nil, sessions.ErrConflict
		}
		return old, m.availableLocked(old)
	}
	if m.stopped {
		return nil, errors.New("browser session manager is stopped")
	}
	now := time.Now().UTC()
	item := &entry{claim: claim, gate: make(chan struct{}, 1), stopping: make(chan struct{}),
		view: sessions.Session{Reference: claim.Reference, Kind: "browser", Label: "Browser session", State: "starting", Revision: 1, CreatedAt: now, UpdatedAt: now}}
	m.items[claim.ID] = item
	m.notifyLocked()
	return item, nil
}

// Snapshot returns public metadata only, without probing or exposing browser
// endpoints, profile paths, page contents, cookies or credentials.
func (m *Manager) Snapshot() []sessions.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := make([]sessions.Session, 0, len(m.items))
	for _, item := range m.items {
		items = append(items, item.view.Clone())
	}
	return items
}

func (m *Manager) Changes() <-chan struct{} { return m.changes }

// WithSession binds preparation work to an already prepared identity without
// starting a browser. Closing the session also cancels questions or secret waits.
func (m *Manager) WithSession(ctx context.Context, work func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if work == nil {
		return errors.New("browser session work is required")
	}
	binding, ok := action.SessionFromContext(ctx)
	if !ok {
		return errors.New("browser work requires a framework session binding")
	}
	if err := binding.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	item := m.items[binding.ID]
	if item == nil {
		m.mu.Unlock()
		return sessions.ErrUnknownSession
	}
	if item.claim != binding.Claim() {
		m.mu.Unlock()
		return sessions.ErrConflict
	}
	if err := m.availableLocked(item); err != nil {
		m.mu.Unlock()
		return err
	}
	stopping := item.stopping
	m.mu.Unlock()

	workCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-stopping:
			cancel(sessions.ErrSessionClosing)
		case <-workCtx.Done():
		}
	}()
	defer func() { cancel(nil); <-done }()
	select {
	case <-stopping:
		cancel(sessions.ErrSessionClosing)
	default:
	}
	if err := workCtx.Err(); err != nil {
		return err
	}
	err := work(workCtx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		err = workCtx.Err()
	}
	return err
}

// Run uses the exact framework binding and an explicitly selected engine.
// Action cancellation interrupts this work without closing a connected session.
func (m *Manager) Run(ctx context.Context, options Options, work func(context.Context) error) (view sessions.Session, runErr error) {
	if err := ctx.Err(); err != nil {
		return view, err
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
		return view, ctx.Err()
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
	browser, err := m.browser(ctx, item, binding.Mode, options)
	if err != nil {
		return view, err
	}
	workCtx, cancel := context.WithCancel(ctx)
	stopBrowser := context.AfterFunc(browser.Context(), cancel)
	stopLifetime := context.AfterFunc(item.lifetime, cancel)
	defer func() { stopBrowser(); stopLifetime(); cancel() }()
	merged := browserContext{Context: workCtx, browser: context.WithoutCancel(browser.Context())}
	runErr = work(merged)
	if ctx.Err() != nil {
		runErr = ctx.Err()
	} else if runErr == nil {
		runErr = workCtx.Err()
	}
	return view, runErr
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

func (m *Manager) browser(ctx context.Context, item *entry, mode sessions.Mode, options Options) (*browserRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if err := m.availableLocked(item); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if item.engine != "" && item.engine != options.Engine {
		m.mu.Unlock()
		return nil, errors.New("browser engine differs from the session's engine")
	}
	if item.browser != nil {
		if item.view.State != "connected" || item.browser.Context().Err() != nil {
			m.mu.Unlock()
			return nil, errors.New("browser session is not connected")
		}
		item.view.Busy = true
		m.touchLocked(item)
		browser := item.browser
		m.mu.Unlock()
		return browser, nil
	}
	if mode != sessions.Create || item.engine != "" {
		m.mu.Unlock()
		return nil, errors.New("browser session has not been started")
	}
	item.engine = options.Engine
	item.view.Kind, item.view.Label, item.view.Busy = string(options.Engine), engineLabel(options.Engine), true
	item.lifetime, item.cancel = context.WithCancel(context.Background())
	item.startDone = make(chan struct{})
	m.touchLocked(item)
	m.mu.Unlock()

	stopStartup := context.AfterFunc(ctx, item.cancel)
	browser, err := launchBrowser(item.lifetime, options)
	stopStartup()
	m.mu.Lock()
	item.browser = browser
	close(item.startDone)
	if err == nil && browser == nil {
		err = errors.New("browser startup returned no runtime")
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if err == nil && (item.closing || m.stopped || item.lifetime.Err() != nil) {
		err = sessions.ErrSessionClosing
	}
	if err != nil {
		if !item.closing && !m.stopped {
			item.view.State, item.view.Reason = "failed", "Browser startup failed"
			m.touchLocked(item)
		}
		m.cleanupLocked(item, false)
		m.mu.Unlock()
		return nil, err
	}
	now := time.Now().UTC()
	item.view.State, item.view.Reason = "connected", ""
	item.view.ConnectedAt, item.view.VerifiedAt = &now, &now
	m.touchLocked(item)
	m.mu.Unlock()
	go m.monitor(item, browser)
	return browser, nil
}

func (m *Manager) monitor(item *entry, browser *browserRuntime) {
	<-browser.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !item.closing && !m.stopped {
		item.view.State, item.view.Reason, item.view.VerifiedAt = "lost", "Browser connection was lost", nil
		m.touchLocked(item)
		m.cleanupLocked(item, false)
	}
}

func (m *Manager) Control(ctx context.Context, request sessions.Control) (sessions.Session, error) {
	if err := ctx.Err(); err != nil {
		return sessions.Session{}, err
	}
	if err := request.Validate(); err != nil {
		return sessions.Session{}, err
	}
	if request.OwnerAgentID != m.owner {
		return sessions.Session{}, sessions.ErrConflict
	}
	if request.Method == "abandon" {
		return sessions.Session{}, errors.New("abandonment is controller-owned")
	}
	if request.Creation != nil {
		if err := (sessions.CreationClaim{Reference: request.Reference, Creation: *request.Creation}).Validate(); err != nil {
			return sessions.Session{}, err
		}
	}
	if request.Method == "close" && (request.Creation == nil || request.RequestID == "") {
		return sessions.Session{}, errors.New("close requires its creation claim and request ID")
	}
	m.mu.Lock()
	item := m.items[request.ID]
	if item == nil && request.Method == "close" {
		if m.stopped {
			m.mu.Unlock()
			return sessions.Session{}, errors.New("browser session manager is stopped")
		}
		// A delayed close can precede its creation delivery even after another
		// identity uses this scope. Retain its tombstone independently.
		now := time.Now().UTC()
		item = &entry{claim: sessions.CreationClaim{Reference: request.Reference, Creation: *request.Creation},
			gate: make(chan struct{}, 1), stopping: make(chan struct{}), cleanupDone: make(chan struct{}),
			closing: true, explicit: true, cleaned: true,
			view: sessions.Session{Reference: request.Reference, Kind: "browser", Label: "Browser session", State: "closed", Revision: 1, CreatedAt: now, UpdatedAt: now}}
		close(item.stopping)
		close(item.cleanupDone)
		m.items[request.ID] = item
		m.notifyLocked()
	}
	if item == nil {
		m.mu.Unlock()
		return sessions.Session{}, sessions.ErrUnknownSession
	}
	if item.claim.Reference != request.Reference || request.Creation != nil && item.claim.Creation != *request.Creation {
		m.mu.Unlock()
		return sessions.Session{}, sessions.ErrConflict
	}
	if request.Method == "inspect" {
		view := item.view.Clone()
		m.mu.Unlock()
		return view, nil
	}
	if m.stopped {
		m.mu.Unlock()
		return sessions.Session{}, errors.New("browser session manager is stopped")
	}
	if request.Method == "close" {
		done := m.cleanupLocked(item, true)
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return sessions.Session{}, ctx.Err()
		case <-done:
			m.mu.Lock()
			defer m.mu.Unlock()
			return item.view.Clone(), item.cleanupErr
		}
	}
	if err := m.availableLocked(item); err != nil {
		m.mu.Unlock()
		return sessions.Session{}, err
	}
	if item.browser == nil {
		m.mu.Unlock()
		return sessions.Session{}, errors.New("browser session has not been started")
	}
	browser := item.browser
	m.mu.Unlock()
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := browser.Verify(bounded)
	cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return item.view.Clone(), ctx.Err()
	}
	if unavailable := m.availableLocked(item); unavailable != nil {
		return item.view.Clone(), unavailable
	}
	if err != nil {
		item.view.State, item.view.Reason, item.view.VerifiedAt = "unverified", "Browser connection could not be verified", nil
	} else {
		now := time.Now().UTC()
		item.view.State, item.view.Reason, item.view.VerifiedAt = "connected", "", &now
	}
	m.touchLocked(item)
	return item.view.Clone(), err
}

// cleanupLocked revokes access immediately. Resource cleanup continues if a
// control's waiting context ends; only confirmed explicit closure reports closed.
func (m *Manager) cleanupLocked(item *entry, explicit bool) <-chan struct{} {
	if explicit {
		item.explicit = true
	}
	if !item.closing {
		item.closing = true
		close(item.stopping)
	}
	if item.cancel != nil {
		item.cancel()
	}
	if item.cleaned && item.cleanupErr == nil {
		if explicit && item.view.State != "closed" {
			m.cleanedLocked(item, nil)
		}
		return item.cleanupDone
	}
	if item.cleanupDone != nil && !item.cleaned {
		return item.cleanupDone
	}
	item.cleanupDone, item.cleaned, item.cleanupErr = make(chan struct{}), false, nil
	if explicit {
		item.view.State, item.view.Reason = "unverified", "Browser session is closing"
		m.touchLocked(item)
	}
	done, started := item.cleanupDone, item.startDone
	go func() {
		if started != nil {
			<-started
		}
		m.mu.Lock()
		browser := item.browser
		m.mu.Unlock()
		var err error
		if browser != nil {
			err = browser.Close()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.cleanedLocked(item, err)
		close(done)
	}()
	return done
}

func (m *Manager) cleanedLocked(item *entry, err error) {
	item.cleaned, item.cleanupErr, item.view.Busy, item.view.VerifiedAt = true, err, false, nil
	if err != nil {
		item.view.State, item.view.Reason = "lost", "Browser cleanup could not be confirmed"
	} else if item.explicit {
		item.view.State, item.view.Reason = "closed", ""
	} else if item.view.State != "failed" {
		item.view.State = "lost"
		if item.view.Reason == "" {
			item.view.Reason = "Browser connection was lost"
		}
	}
	m.touchLocked(item)
}

// Close stops owned browsers without publishing explicit-close evidence.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.stopped = true
		var pending []struct {
			item *entry
			done <-chan struct{}
		}
		for _, item := range m.items {
			if item.view.State == "closed" {
				continue
			}
			if item.view.State != "failed" {
				item.view.State, item.view.Reason, item.view.VerifiedAt = "lost", "Browser session owner stopped", nil
				m.touchLocked(item)
			}
			pending = append(pending, struct {
				item *entry
				done <-chan struct{}
			}{item, m.cleanupLocked(item, false)})
		}
		m.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, wait := range pending {
			select {
			case <-wait.done:
				m.mu.Lock()
				m.closeErr = errors.Join(m.closeErr, wait.item.cleanupErr)
				m.mu.Unlock()
			case <-ctx.Done():
				m.closeErr = errors.Join(m.closeErr, ctx.Err())
				return
			}
		}
	})
	return m.closeErr
}

func (m *Manager) availableLocked(item *entry) error {
	if item.view.State == "closed" {
		return sessions.ErrSessionClosed
	}
	if m.stopped {
		return errors.New("browser session manager is stopped")
	}
	if item.closing {
		return sessions.ErrSessionClosing
	}
	if item.view.State == "lost" || item.view.State == "failed" {
		return fmt.Errorf("%w: browser session is %s", sessions.ErrConflict, item.view.State)
	}
	return nil
}

func (m *Manager) touchLocked(item *entry) {
	item.view.Revision++
	item.view.UpdatedAt = time.Now().UTC()
	m.notifyLocked()
}

func (m *Manager) notifyLocked() {
	select {
	case m.changes <- struct{}{}:
	default:
	}
}
