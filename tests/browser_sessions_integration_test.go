//go:build browserintegration

package browser_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	browser "github.com/defos-ai/commander-browser-agent"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/comms"
	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
	fsartifacts "github.com/defos-ai/commander/providers/fs/artifacts"
	fscomms "github.com/defos-ai/commander/providers/fs/comms"
)

func TestBrowserSessionsRetainIdentityWhenStartupCleanupFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("startup cleanup fixture requires Unix shell and directory permission enforcement")
	}
	if os.Geteuid() == 0 {
		t.Skip("startup cleanup fixture requires a non-root user so removing a profile from a nonwritable parent fails")
	}
	privateParent := t.TempDir()
	executable := fixtures.NewBrowserCleanupFailureExecutable(t)
	t.Setenv("TMPDIR", privateParent)
	defer func() {
		if err := os.Chmod(privateParent, 0700); err != nil {
			t.Errorf("restore private fixture directory permissions: %v", err)
		}
	}()
	owner, err := id.AgentID(string(browser.AgentType))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := browserruntime.New(owner)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture deliberately leaves cleanup uncertain. Restore directory
	// permissions above before testing.T removes the private leftover profile.
	defer func() { _ = manager.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	binding := newBrowserSessionBinding(owner)
	if err := manager.Prepare(ctx, binding.Claim()); err != nil {
		t.Fatal(err)
	}
	_, err = manager.Run(action.WithSession(ctx, &binding), browserruntime.Options{Engine: browserruntime.Lightpanda, Executable: executable}, func(context.Context) error {
		t.Error("work ran despite failed browser startup")
		return nil
	})
	if err == nil {
		t.Fatal("failed browser startup was accepted")
	}
	info, err := os.Stat(privateParent)
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("startup fixture did not make the profile parent nonwritable: info=%v err=%v", info, err)
	}
	closed, err := manager.Control(ctx, browserSessionControl(binding, "close"))
	if err == nil {
		t.Fatal("close falsely confirmed cleanup of a retained private profile")
	}
	assertBrowserSession(t, closed, binding.Reference, "lost")
	binding.Mode = sessions.Reuse
	if _, err := manager.Run(action.WithSession(ctx, &binding), browserruntime.Options{Engine: browserruntime.Lightpanda, Executable: executable}, func(context.Context) error {
		t.Error("work ran in a session whose cleanup is uncertain")
		return nil
	}); err == nil {
		t.Fatal("uncertain session identity was silently restored")
	}
	if err := os.Chmod(privateParent, 0700); err != nil {
		t.Fatal(err)
	}
	closed, err = manager.Control(ctx, browserSessionControl(binding, "close"))
	if err != nil {
		t.Fatalf("retry close after restoring profile cleanup: %v", err)
	}
	assertBrowserSession(t, closed, binding.Reference, "closed")
	if remaining, err := os.ReadDir(privateParent); err != nil || len(remaining) != 0 {
		t.Fatalf("confirmed close retained a private profile: entries=%v error=%v", remaining, err)
	}
}

// This suite owns real browser resources through the production provider. It
// needs installed Lightpanda, but no model, controller application or demo.
func TestBrowserSessionsRetainIsolateAndCloseLightpanda(t *testing.T) {
	options := browserSessionOptions(t)
	t.Run(string(options.Engine), func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		site := fixtures.NewBrowserSessionSite(t)
		manager := newBrowserSessionManager(t)
		binding := newBrowserSessionBinding(manager.owner)
		if err := manager.Prepare(ctx, binding.Claim()); err != nil {
			t.Fatal(err)
		}
		var connectionClosed <-chan struct{}
		created, err := manager.Run(action.WithSession(ctx, &binding), options, func(ctx context.Context) error {
			connectionClosed = chromedp.FromContext(ctx).Browser.LostConnection
			return browserFixtureLogin(ctx, site.URL)
		})
		if err != nil {
			t.Fatalf("create authenticated session: %v", err)
		}
		assertBrowserSession(t, created, binding.Reference, "connected")
		if created.Kind != string(options.Engine) || created.Busy {
			t.Fatalf("session does not describe its idle engine: %+v", created)
		}
		binding.Mode = sessions.Reuse
		assertBrowserAccount(t, manager.Manager, ctx, binding, options, site.URL, "signed in")
		verified, err := manager.Control(ctx, browserSessionControl(binding, "verify"))
		if err != nil {
			t.Fatalf("verify live browser session: %v", err)
		}
		assertBrowserSession(t, verified, binding.Reference, "connected")

		wrongEngine := options
		wrongEngine.Engine = browserruntime.Engine("chromium")
		called := false
		_, err = manager.Run(action.WithSession(ctx, &binding), wrongEngine, func(context.Context) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatal("engine mismatch ran work or accepted authenticated state")
		}
		assertBrowserAccount(t, manager.Manager, ctx, binding, options, site.URL, "signed in")

		independent := newBrowserSessionBinding(manager.owner)
		if err := manager.Prepare(ctx, independent.Claim()); err != nil {
			t.Fatal(err)
		}
		assertBrowserAccount(t, manager.Manager, ctx, independent, options, site.URL, "signed out")
		independent.Mode = sessions.Reuse

		work, cancelWork := context.WithCancelCause(action.WithSession(ctx, &binding))
		defer cancelWork(nil)
		cancellationCause := errors.New("fixture action cancellation")
		started, observedCause, done := runWaitingBrowserSession(manager.Manager, work, options)
		awaitBrowserSignal(t, started)
		cancelWork(cancellationCause)
		if err := awaitBrowserRun(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled execution returned %v", err)
		}
		if cause := awaitBrowserRun(t, observedCause); cause != cancellationCause {
			t.Fatalf("browser callback lost the action cancellation cause: %v", cause)
		}
		assertBrowserAccount(t, manager.Manager, ctx, binding, options, site.URL, "signed in")

		started, _, done = runWaitingBrowserSession(manager.Manager, action.WithSession(ctx, &binding), options)
		awaitBrowserSignal(t, started)
		closed, err := manager.Control(ctx, browserSessionControl(binding, "close"))
		if err != nil {
			t.Fatalf("close active session: %v", err)
		}
		if err := awaitBrowserRun(t, done); err == nil {
			t.Fatal("close did not interrupt active browser work")
		}
		assertBrowserSession(t, closed, binding.Reference, "closed")
		awaitBrowserSignal(t, connectionClosed)
		assertBrowserAccount(t, manager.Manager, ctx, independent, options, site.URL, "signed out")
		if _, err := manager.Run(action.WithSession(ctx, &binding), options, func(context.Context) error { return nil }); err == nil {
			t.Fatal("closed session was silently recreated")
		}
		lost := newBrowserSessionBinding(manager.owner)
		if err := manager.Prepare(ctx, lost.Claim()); err != nil {
			t.Fatal(err)
		}
		_, err = manager.Run(action.WithSession(ctx, &lost), options, func(ctx context.Context) error {
			if err := browserFixtureLogin(ctx, site.URL); err != nil {
				return err
			}
			browser := chromedp.FromContext(ctx)
			return target.CloseTarget(browser.Target.TargetID).Do(cdp.WithExecutor(ctx, browser.Browser))
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("close browser target to simulate session loss: %v", err)
		}
		awaitBrowserSessionState(t, manager.Manager, lost.Reference, "lost")
		// Scope admission belongs to the controller. A newly prepared claim
		// represents its admitted replacement after abandoning the old ID.
		replacement := newBrowserSessionBinding(manager.owner)
		replacement.ScopeID = lost.ScopeID
		if err := manager.Prepare(ctx, replacement.Claim()); err != nil {
			t.Fatalf("prepare controller-admitted replacement: %v", err)
		}
		assertBrowserAccount(t, manager.Manager, ctx, replacement, options, site.URL, "signed out")
		replacement.Mode = sessions.Reuse
		retainedLost, err := manager.Control(ctx, browserSessionControl(lost, "inspect"))
		if err != nil {
			t.Fatal(err)
		}
		assertBrowserSession(t, retainedLost, lost.Reference, "lost")
		lost.Mode = sessions.Reuse
		if _, err := manager.Run(action.WithSession(ctx, &lost), options, func(context.Context) error { return nil }); err == nil {
			t.Fatal("lost session was silently restored")
		}
		closedLost, err := manager.Control(ctx, browserSessionControl(lost, "close"))
		if err != nil {
			t.Fatalf("explicitly close lost session: %v", err)
		}
		assertBrowserSession(t, closedLost, lost.Reference, "closed")
		assertBrowserAccount(t, manager.Manager, ctx, replacement, options, site.URL, "signed out")
		if err := manager.Close(); err != nil {
			t.Fatalf("shut down browser owner: %v", err)
		}
		states := map[string]string{}
		for _, item := range manager.Snapshot() {
			states[item.ID] = item.State
			if item.Busy {
				t.Fatal("shutdown retained a busy session")
			}
		}
		if len(states) != 4 || states[binding.ID] != "closed" || states[lost.ID] != "closed" || states[independent.ID] != "lost" || states[replacement.ID] != "lost" {
			t.Fatalf("shutdown discarded retained lifecycle evidence: %v", states)
		}
	})
}

// The harness acts only as a protocol peer. The actual browser agent/framework
// resolves the execution binding, publishes inventory and serves close controls.
func TestBrowserSessionsThroughFrameworkDispatchAndControls(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewBrowserSessionSite(t)
	manager := newBrowserSessionManager(t)
	root := t.TempDir()
	bus, err := fscomms.NewFsBus(filepath.Join(root, "bus"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	observer, err := bus.Subscribe(t.Context(), comms.Inbox{Topic: messages.ControllerEvents(messages.DefaultControllerType), Name: "browser-session-test", Mode: comms.TransientExclusive})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	files, err := fsartifacts.NewArtifactFileFsStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	executor := &browserSessionTestAction{
		Base: action.NewBase(actionmodel.Action{Name: "test-browser-session", Description: "Exercise browser sessions on local test pages", SessionMode: "create",
			InputSchema:  json.RawMessage(`{"type":"object","properties":{"session_scope":{"type":"string"},"session_id":{"type":"string"},"authenticate":{"type":"boolean"},"wait_for_close":{"type":"boolean"}},"required":["session_scope"],"additionalProperties":false}`),
			OutputSchema: json.RawMessage(`{"type":"object","properties":{"account":{"type":"string"}},"required":["account"],"additionalProperties":false}`)}),
		manager: manager.Manager, options: options, siteURL: site.URL, started: make(chan struct{}),
	}
	zero := time.Duration(0)
	host, err := browser.NewAgent(agent.AgentConfig{ID: manager.owner, Sessions: manager.Manager, Actions: []action.Executor{executor}, ArtifactFileStore: files, HeartbeatInterval: &zero})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- host.Serve(serveCtx, bus, nil) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancelServe()
			select {
			case err := <-served:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("stop framework host: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("framework host did not stop")
			}
		})
	}
	t.Cleanup(stop)
	peer := &browserSessionPeer{t: t, bus: bus, observer: observer}
	peer.wait(func(message messages.Message) bool { return message.Type == controllerevents.AgentStarted })
	binding := newBrowserSessionBinding(manager.owner)
	first := peer.dispatch(binding, true, false)
	if first.Session == nil {
		t.Fatal("framework result omitted the created session")
	}
	assertBrowserSession(t, *first.Session, binding.Reference, "connected")
	binding.Mode = sessions.Reuse
	if reused := peer.dispatch(binding, false, false); string(reused.Output) != `{"account":"signed in"}` {
		t.Fatalf("framework session reuse lost authentication: %s", reused.Output)
	}
	inspected := peer.control(binding, "inspect")
	if len(inspected.Sessions) != 1 {
		t.Fatalf("framework inventory contains %d sessions, want one", len(inspected.Sessions))
	}
	assertBrowserSession(t, inspected.Sessions[0], binding.Reference, "connected")
	activeCommand := peer.publishAction(binding, false, true)
	awaitBrowserSignal(t, executor.started)
	closed := peer.control(binding, "close")
	if len(closed.Sessions) != 1 {
		t.Fatalf("close inventory contains %d sessions, want one", len(closed.Sessions))
	}
	assertBrowserSession(t, closed.Sessions[0], binding.Reference, "closed")
	interrupted := peer.wait(func(message messages.Message) bool {
		return message.CorrelationID == activeCommand.ID && message.Type == controllerevents.CommandFailed
	})
	peer.accept(interrupted)

	// Framework shutdown invokes the provider before publishing final inventory.
	remaining := newBrowserSessionBinding(manager.owner)
	peer.dispatch(remaining, false, false)
	stop()
	stopped := peer.wait(func(message messages.Message) bool { return message.Type == controllerevents.AgentStopped })
	var final agent.AgentSnapshot
	if err := json.Unmarshal(stopped.Payload, &final); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, item := range final.Sessions {
		states[item.ID] = item.State
	}
	if len(states) != 2 || states[binding.ID] != "closed" || states[remaining.ID] != "lost" {
		t.Fatalf("stopped framework inventory lost lifecycle evidence: %v", states)
	}
}

type ownedBrowserSessionManager struct {
	*browserruntime.Manager
	owner string
}

func newBrowserSessionManager(t *testing.T) ownedBrowserSessionManager {
	t.Helper()
	owner, err := id.AgentID(string(browser.AgentType))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := browserruntime.New(owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Errorf("release browser sessions: %v", err)
		}
	})
	return ownedBrowserSessionManager{Manager: manager, owner: owner}
}

func browserSessionOptions(t *testing.T) browserruntime.Options {
	t.Helper()
	options := browserruntime.Options{Engine: browserruntime.Lightpanda, Executable: os.Getenv("LIGHTPANDA_PATH")}
	if !filepath.IsAbs(options.Executable) {
		t.Fatal("LIGHTPANDA_PATH must name an absolute installed executable for -tags=browserintegration")
	}
	return options
}

func newBrowserSessionBinding(owner string) sessions.Binding {
	task := id.TaskID()
	return sessions.Binding{Reference: sessions.Reference{ID: id.New(), OwnerAgentID: owner, ScopeID: id.New()}, Mode: sessions.Create,
		Creation: sessions.ExecutionRef{OperationID: id.OperationID(), TaskID: task, CommandID: task}}
}

func browserSessionControl(binding sessions.Binding, method string) sessions.Control {
	creation := binding.Creation
	return sessions.Control{Reference: binding.Reference, Method: method, RequestID: id.New(), Creation: &creation}
}

func browserFixtureLogin(ctx context.Context, siteURL string) error {
	// These are fixed dummy fixture values, never managed credentials.
	return chromedp.Run(ctx,
		chromedp.Navigate(siteURL+"/login"),
		chromedp.Evaluate(`(() => {
          document.querySelector("#username").value = "fixture-user";
          document.querySelector("#password").value = "fixture-password";
          document.querySelector("button").click();
        })()`, nil),
		chromedp.Poll(`document.querySelector("#status").dataset.authenticated === "true"`, nil,
			chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(5*time.Second)))
}

func assertBrowserAccount(t *testing.T, manager *browserruntime.Manager, ctx context.Context, binding sessions.Binding, options browserruntime.Options, siteURL, want string) {
	t.Helper()
	var account string
	state, err := manager.Run(action.WithSession(ctx, &binding), options, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Navigate(siteURL+"/account"), chromedp.Evaluate(`document.querySelector("#account").textContent`, &account))
	})
	if err != nil {
		t.Fatalf("navigate in retained session: %v", err)
	}
	assertBrowserSession(t, state, binding.Reference, "connected")
	if account != want {
		t.Fatalf("account status = %q, want %q", account, want)
	}
}

func assertBrowserSession(t *testing.T, snapshot sessions.Session, reference sessions.Reference, state string) {
	t.Helper()
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("invalid session snapshot: %v", err)
	}
	if snapshot.Reference != reference || snapshot.State != state {
		t.Fatalf("session snapshot = %+v, want reference %+v in state %s", snapshot, reference, state)
	}
}

func runWaitingBrowserSession(manager *browserruntime.Manager, ctx context.Context, options browserruntime.Options) (<-chan struct{}, <-chan error, <-chan error) {
	started := make(chan struct{})
	cause := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		_, err := manager.Run(ctx, options, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			cause <- context.Cause(ctx)
			return ctx.Err()
		})
		done <- err
	}()
	return started, cause, done
}

func awaitBrowserSessionState(t *testing.T, manager *browserruntime.Manager, reference sessions.Reference, state string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		for _, item := range manager.Snapshot() {
			if item.Reference == reference && item.State == state {
				return
			}
		}
		select {
		case <-manager.Changes():
		case <-ctx.Done():
			t.Fatalf("session did not enter %s state", state)
		}
	}
}

func awaitBrowserSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for browser lifecycle signal")
	}
}

func awaitBrowserRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("browser execution did not stop")
		return nil
	}
}

type browserSessionTestAction struct {
	*action.Base
	manager *browserruntime.Manager
	options browserruntime.Options
	siteURL string
	started chan struct{}
}

func (a *browserSessionTestAction) Run(ctx context.Context, raw json.RawMessage) (action.Result, error) {
	var input struct {
		SessionScope string `json:"session_scope"`
		SessionID    string `json:"session_id,omitempty"`
		Authenticate bool   `json:"authenticate,omitempty"`
		WaitForClose bool   `json:"wait_for_close,omitempty"`
	}
	if err := action.DecodeInput(raw, &input); err != nil {
		return action.Result{}, err
	}
	binding, bound := action.SessionFromContext(ctx)
	execution, executing := action.ExecutionFromContext(ctx)
	if !bound || !executing {
		return action.Result{}, errors.New("framework execution context is incomplete")
	}
	var account string
	snapshot, err := a.manager.Run(ctx, a.options, func(ctx context.Context) error {
		if actual, ok := action.SessionFromContext(ctx); !ok || actual != binding {
			return errors.New("browser work lost the framework session binding")
		}
		if actual, ok := action.ExecutionFromContext(ctx); !ok || actual != execution {
			return errors.New("browser work lost the framework execution identity")
		}
		if input.WaitForClose {
			close(a.started)
			<-ctx.Done()
			return ctx.Err()
		}
		if input.Authenticate {
			if err := browserFixtureLogin(ctx, a.siteURL); err != nil {
				return err
			}
		}
		return chromedp.Run(ctx, chromedp.Navigate(a.siteURL+"/account"), chromedp.Evaluate(`document.querySelector("#account").textContent`, &account))
	})
	if err != nil {
		return action.Result{}, err
	}
	result, err := action.JSONResult(map[string]string{"account": account})
	result.Session = &snapshot
	return result, err
}

type browserSessionPeer struct {
	t        *testing.T
	bus      comms.Bus
	observer comms.Subscription
	pending  []messages.Message
}

func (p *browserSessionPeer) publishAction(binding sessions.Binding, authenticate, wait bool) messages.Message {
	p.t.Helper()
	input := map[string]any{"session_scope": binding.ScopeID, "authenticate": authenticate, "wait_for_close": wait}
	if binding.Mode != sessions.Create {
		input["session_id"] = binding.ID
	}
	raw, err := json.Marshal(input)
	if err != nil {
		p.t.Fatal(err)
	}
	work := messages.WorkContext{OperationID: binding.Creation.OperationID, TaskID: id.TaskID()}
	if binding.Mode == sessions.Create {
		work.TaskID = binding.Creation.TaskID
	}
	command, err := agentcommands.NewRunAction(binding.OwnerAgentID, agentcommands.ActionCall{ActionName: "test-browser-session", Input: raw, Work: work, Session: &binding})
	if err != nil {
		p.t.Fatal(err)
	}
	if err := (agentcommands.Client{Bus: p.bus}).Publish(p.t.Context(), command); err != nil {
		p.t.Fatal(err)
	}
	return command
}

func (p *browserSessionPeer) dispatch(binding sessions.Binding, authenticate, wait bool) agent.ActionOutput {
	p.t.Helper()
	command := p.publishAction(binding, authenticate, wait)
	message := p.wait(func(message messages.Message) bool {
		return message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed)
	})
	if message.Type != controllerevents.ActionSucceeded {
		p.t.Fatalf("framework browser action failed: %s", message.Payload)
	}
	var output agent.ActionOutput
	if err := json.Unmarshal(message.Payload, &output); err != nil {
		p.t.Fatal(err)
	}
	if err := output.Validate(); err != nil {
		p.t.Fatal(err)
	}
	p.accept(message)
	return output
}

func (p *browserSessionPeer) control(binding sessions.Binding, method string) agent.AgentSnapshot {
	p.t.Helper()
	command, err := messages.NewMessage(agentcommands.ControlSession, binding.OwnerAgentID, browserSessionControl(binding, method))
	if err != nil {
		p.t.Fatal(err)
	}
	if err := (agentcommands.Client{Bus: p.bus}).Publish(p.t.Context(), command); err != nil {
		p.t.Fatal(err)
	}
	message := p.wait(func(message messages.Message) bool { return message.CorrelationID == command.ID })
	if message.Type != controllerevents.AgentUpdated {
		p.t.Fatalf("framework session control failed: %s", message.Payload)
	}
	var snapshot agent.AgentSnapshot
	if err := json.Unmarshal(message.Payload, &snapshot); err != nil {
		p.t.Fatal(err)
	}
	return snapshot
}

func (p *browserSessionPeer) accept(result messages.Message) {
	p.t.Helper()
	// The harness has validated the outcome above. Send explicit acceptance;
	// acknowledging transport delivery alone does not settle framework results.
	receipt, err := agentevents.NewResultAccepted(result)
	if err != nil {
		p.t.Fatal(err)
	}
	if err := (agentevents.Client{Bus: p.bus}).Publish(p.t.Context(), receipt); err != nil {
		p.t.Fatal(err)
	}
}

func (p *browserSessionPeer) wait(match func(messages.Message) bool) messages.Message {
	p.t.Helper()
	for i, message := range p.pending {
		if match(message) {
			p.pending = append(p.pending[:i], p.pending[i+1:]...)
			return message
		}
	}
	ctx, cancel := context.WithTimeout(p.t.Context(), 30*time.Second)
	defer cancel()
	for {
		delivery, err := p.observer.Next(ctx)
		if err != nil {
			p.t.Fatalf("receive browser framework event: %v", err)
		}
		message := delivery.Message()
		if err := delivery.Ack(ctx); err != nil {
			p.t.Fatal(err)
		}
		if match(message) {
			return message
		}
		p.pending = append(p.pending, message)
	}
}
