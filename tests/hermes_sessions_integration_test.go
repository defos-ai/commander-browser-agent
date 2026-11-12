//go:build hermesintegration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	browserlogin "github.com/defos-ai/commander-browser-agent/browser/login"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/comms"
	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/executions"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/secrets"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
	runtimeSessions "github.com/defos-ai/commander/framework/sessions"
	fsartifacts "github.com/defos-ai/commander/providers/fs/artifacts"
	fscomms "github.com/defos-ai/commander/providers/fs/comms"
)

type hermesSessionScenario struct {
	request      hermes.Request
	login        *hermesLoginFixture
	managed      bool
	prepareOnly  bool
	checkpointed bool
	cancel       <-chan struct{}
	scope        chan context.Context
	result       chan error
}

type hermesSessionOutput struct {
	Result  hermes.Result          `json:"result"`
	Login   *browserlogin.Result   `json:"login,omitempty"`
	Claim   sessions.CreationClaim `json:"claim"`
	Session sessions.Session       `json:"session"`
}

// This host registers only test-owned open/use actions. Production action
// schemas and model setup are deliberately outside this session-binding test.
type hermesSessionHost struct {
	t                      *testing.T
	host                   *agent.Agent
	manager                *browserruntime.Manager
	options                browserruntime.HermesOptions
	files                  *fsartifacts.ArtifactFileFsStore
	bus                    comms.Bus
	observer               comms.Subscription
	definitions            []actionmodel.Definition
	claims                 map[string]sessions.CreationClaim
	pending                []messages.Message
	history                []messages.Message
	accepted               map[string]messages.Message
	entered                atomic.Int32
	expectUncertainCleanup bool
	setup                  browsing.Setup
	modelKeyMetadata       map[string]secrets.Metadata
	checkpoints            map[string]executions.Execution
}

func newHermesSessionHost(t *testing.T, cases map[string]hermesSessionScenario, extra ...func(*hermesSessionHost) action.Executor) *hermesSessionHost {
	t.Helper()
	return newHermesSessionHostWithOptions(t, cases, hermesScreenshotOptions(), extra...)
}

func newHermesSessionHostWithOptions(t *testing.T, cases map[string]hermesSessionScenario, options browserruntime.HermesOptions, extra ...func(*hermesSessionHost) action.Executor) *hermesSessionHost {
	t.Helper()
	profilesBefore := hermesSessionProfiles(t)
	h := &hermesSessionHost{t: t, options: options, claims: map[string]sessions.CreationClaim{}, accepted: map[string]messages.Message{}, checkpoints: map[string]executions.Execution{}}
	owner, err := id.AgentID("browser-agent")
	hermesSessionCheck(t, err)
	h.manager, err = browserruntime.New(owner)
	hermesSessionCheck(t, err)
	var executors []action.Executor
	for _, mode := range []sessions.Mode{sessions.Open, sessions.Use} {
		executor, err := action.NewFunc(action.FuncConfig[struct {
			Scenario string `json:"scenario"`
		}, hermesSessionOutput]{
			Action: actionmodel.Action{Name: "native-" + string(mode), Description: "Exercise the retained native browser fixture.", SessionMode: string(mode)},
			Run: func(ctx context.Context, input struct {
				Scenario string `json:"scenario"`
			}) (hermesSessionOutput, error) {
				var binding sessions.Binding
				var err error
				if mode == sessions.Open {
					binding, err = action.OpenSession(ctx)
				} else {
					binding, err = action.UseSession(ctx)
				}
				if err != nil {
					return hermesSessionOutput{}, err
				}
				h.entered.Add(1)
				scenario, ok := cases[input.Scenario]
				if !ok {
					return hermesSessionOutput{}, errors.New("unknown native fixture scenario")
				}
				if scenario.checkpointed {
					return h.runCheckpointed(ctx, binding, scenario)
				}
				var result hermes.Result
				var loginResult *browserlogin.Result
				view, err := h.manager.RunHermes(ctx, h.options, func(workCtx context.Context) error {
					if scenario.login != nil {
						outcome, err := runHermesLoginFixture(workCtx, *scenario.login)
						loginResult = &outcome
						return err
					}
					request := scenario.request
					if scenario.managed {
						request, err = h.setup.PrepareHermes(workCtx)
						if err != nil {
							return err
						}
						defer clear(request.APIKey)
						request.Objective, request.MaxIterations, request.MaxTokens, request.TimeoutSeconds = "Complete the managed local fixture.", 4, 512, 25
					}
					if scenario.prepareOnly {
						return nil
					}
					handler := func(ctx context.Context, event hermes.Event) (hermes.Reply, error) {
						hermesEventPrivate(t, event, string(request.APIKey))
						if event.Kind == "admit" {
							return hermes.Reply{Approved: true}, nil
						}
						if event.Kind == "question" {
							question, err := action.Question(ctx, "native_fixture_question", event.Text, event.Options...)
							return hermes.Reply{Answer: question.Answer}, err
						}
						return hermes.Reply{}, nil
					}
					if scenario.scope != nil {
						if _, err := hermes.Run(context.WithoutCancel(workCtx), request, handler); err == nil {
							t.Error("a detached context retained native execution authority")
						}
						scenario.scope <- workCtx
					}
					runCtx, cancel := context.WithTimeout(workCtx, 25*time.Second)
					defer cancel()
					joined := make(chan struct{})
					go func() {
						defer close(joined)
						select {
						case <-scenario.cancel:
							cancel()
						case <-runCtx.Done():
						}
					}()
					defer func() { cancel(); <-joined }()
					result, err = hermes.Run(runCtx, request, handler)
					return err
				})
				if scenario.result != nil {
					scenario.result <- err
				}
				return hermesSessionOutput{Result: result, Login: loginResult, Claim: binding.Claim(), Session: view}, err
			},
		})
		hermesSessionCheck(t, err)
		executors = append(executors, executor)
		h.definitions = append(h.definitions, executor.Action().Definition())
	}
	capture := newHermesScreenshot(h.manager)
	capture.options = options
	executors = append(executors, capture)
	h.definitions = append(h.definitions, capture.Action().Definition())
	for _, makeExecutor := range extra {
		executor := makeExecutor(h)
		executors = append(executors, executor)
		h.definitions = append(h.definitions, executor.Action().Definition())
	}
	root := t.TempDir()
	h.bus, err = fscomms.NewFsBus(filepath.Join(root, "bus"))
	hermesSessionCheck(t, err)
	h.observer, err = h.bus.Subscribe(t.Context(), comms.Inbox{Topic: messages.ControllerEvents(messages.DefaultControllerType), Name: "native-session-tests", Mode: comms.TransientExclusive})
	hermesSessionCheck(t, err)
	files, err := fsartifacts.NewArtifactFileFsStore(filepath.Join(root, "storage"))
	hermesSessionCheck(t, err)
	h.files = files
	zero := time.Duration(0)
	h.host, err = agent.NewAgent(agent.AgentConfig{ID: owner, Type: "browser-agent", Actions: executors, Sessions: h.manager, ArtifactFileStore: files,
		Secrets: &agent.SecretConfig{RequestTTL: 2 * time.Second, RefreshInterval: 500 * time.Millisecond, RetryInterval: 100 * time.Millisecond}, HeartbeatInterval: &zero})
	hermesSessionCheck(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- h.host.Serve(ctx, h.bus, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("native fixture host shutdown failed")
			}
		case <-time.After(12 * time.Second):
			t.Error("native fixture host did not join shutdown")
		}
		cleanupErr := h.manager.Close()
		addedProfiles := 0
		for path := range hermesSessionProfiles(t) {
			if !profilesBefore[path] {
				addedProfiles++
			}
		}
		if h.expectUncertainCleanup {
			if !errors.Is(cleanupErr, hermes.ErrUncertain) {
				t.Error("intentional native loss did not retain uncertain cleanup ownership")
			}
			lost := false
			for _, session := range h.manager.Snapshot() {
				lost = lost || session.State == "lost"
			}
			if !lost || addedProfiles == 0 {
				t.Error("uncertain native cleanup discarded its lost reference or private state")
			}
		} else if cleanupErr != nil {
			t.Error("native session cleanup was not confirmed")
		} else if addedProfiles != 0 {
			t.Error("confirmed native session cleanup left a private profile")
		}
		_ = h.observer.Close()
		_ = h.bus.Close()
	})
	h.wait(func(m messages.Message) bool { return m.Type == controllerevents.AgentStarted })
	if len(h.host.Describe().Sessions) != 0 {
		t.Fatal("session discovery opened a native runtime")
	}
	return h
}

func hermesSessionProfiles(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "commander-hermes-*"))
	hermesSessionCheck(t, err)
	profiles := make(map[string]bool, len(paths))
	for _, path := range paths {
		profiles[path] = true
	}
	return profiles
}

func (h *hermesSessionHost) task(parent *operations.Task, mode sessions.Mode, scenario string) operations.Task {
	h.t.Helper()
	input, _ := json.Marshal(map[string]string{"scenario": scenario})
	next, err := runtimeSessions.Normalize(parent, []actionmodel.NextAction{{ActionName: "native-" + string(mode), Input: input}}, nil, h.definitions...)
	hermesSessionCheck(h.t, err)
	operation, parentID := id.OperationID(), ""
	if parent != nil {
		operation, parentID = parent.OperationID, parent.ID
	}
	task := operations.NewTask(operation, parentID, next[0])
	if task.SessionIntent == nil || task.SessionIntent.Validate() != nil {
		h.t.Fatal("framework did not allocate the native session intent")
	}
	return task
}

func (h *hermesSessionHost) binding(task operations.Task) sessions.Binding {
	claim, exists := h.claims[task.SessionIntent.ID]
	if !exists {
		claim = sessions.CreationClaim{Reference: sessions.Reference{ID: task.SessionIntent.ID, ScopeID: task.SessionIntent.ScopeID, OwnerAgentID: h.host.ID()}, CommandID: task.CommandID, OperationID: task.OperationID, TaskID: task.ID}
		h.claims[claim.ID] = claim
	}
	return sessions.Binding{Reference: claim.Reference, Mode: task.SessionIntent.Mode, CommandID: claim.CommandID, OperationID: claim.OperationID, TaskID: claim.TaskID}
}

func (h *hermesSessionHost) dispatch(task operations.Task, binding *sessions.Binding) messages.Message {
	h.t.Helper()
	command, err := agentcommands.NewRunAction(h.host.ID(), agentcommands.ActionCall{ActionName: task.ActionName, Input: task.Input, OperationID: task.OperationID, TaskID: task.ID, Session: binding})
	hermesSessionCheck(h.t, err)
	h.checkpoints[command.ID] = executions.Execution{Command: command, StartedAt: time.Now().UTC()}
	hermesSessionCheck(h.t, (agentcommands.Client{Bus: h.bus}).Publish(h.t.Context(), command))
	return command
}

func (h *hermesSessionHost) wait(match func(messages.Message) bool) messages.Message {
	h.t.Helper()
	for index := 0; index < len(h.pending); {
		message := h.pending[index]
		if match(message) {
			h.pending = append(h.pending[:index], h.pending[index+1:]...)
			return message
		}
		if receipt, known := h.accepted[message.ID]; known {
			h.pending = append(h.pending[:index], h.pending[index+1:]...)
			hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), receipt))
			continue
		}
		if message.Type == controllerevents.CheckpointRequested {
			h.pending = append(h.pending[:index], h.pending[index+1:]...)
			h.replyCheckpoint(message, h.persistCheckpoint(message))
			continue
		}
		index++
	}
	ctx, cancel := context.WithTimeout(h.t.Context(), 35*time.Second)
	defer cancel()
	for {
		delivery, err := h.observer.Next(ctx)
		hermesSessionCheck(h.t, err)
		message := delivery.Message()
		hermesSessionCheck(h.t, delivery.Ack(ctx))
		message.Payload = bytes.Clone(message.Payload)
		h.history = append(h.history, message)
		if receipt, ok := h.accepted[message.ID]; ok {
			hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(ctx, receipt))
			continue
		}
		if match(message) {
			return message
		}
		if message.Type == controllerevents.CheckpointRequested {
			h.replyCheckpoint(message, h.persistCheckpoint(message))
			continue
		}
		h.pending = append(h.pending, message)
	}
}

func (h *hermesSessionHost) finish(command messages.Message) (hermesSessionOutput, bool) {
	h.t.Helper()
	message := h.wait(func(m messages.Message) bool {
		return m.CorrelationID == command.ID && (m.Type == controllerevents.ActionSucceeded || m.Type == controllerevents.CommandFailed)
	})
	// The fixture retains the accepted message before acknowledging its outcome.
	receipt, err := agentevents.NewResultAccepted(message)
	hermesSessionCheck(h.t, err)
	h.accepted[message.ID] = receipt
	hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), receipt))
	var result hermesSessionOutput
	if message.Type == controllerevents.ActionSucceeded {
		var output agent.ActionOutput
		hermesSessionCheck(h.t, json.Unmarshal(message.Payload, &output))
		hermesSessionCheck(h.t, json.Unmarshal(output.Output, &result))
	}
	return result, message.Type == controllerevents.ActionSucceeded
}

func (h *hermesSessionHost) call(task operations.Task) hermesSessionOutput {
	h.t.Helper()
	binding := h.binding(task)
	result, ok := h.finish(h.dispatch(task, &binding))
	if !ok || result.Claim != binding.Claim() {
		h.t.Fatal("native session action failed or changed its original creation claim")
	}
	return result
}

func (h *hermesSessionHost) control(binding sessions.Binding, method string) bool {
	h.t.Helper()
	command, err := messages.NewMessage(agentcommands.ControlSession, h.host.ID(), sessions.Control{Reference: binding.Reference, Method: method, RequestID: id.New(), CommandID: binding.CommandID, OperationID: binding.OperationID, TaskID: binding.TaskID})
	hermesSessionCheck(h.t, err)
	hermesSessionCheck(h.t, (agentcommands.Client{Bus: h.bus}).Publish(h.t.Context(), command))
	response := h.wait(func(m messages.Message) bool {
		return m.CorrelationID == command.ID && (m.Type == controllerevents.AgentUpdated || m.Type == controllerevents.CommandFailed)
	})
	return response.Type == controllerevents.AgentUpdated
}

func (h *hermesSessionHost) snapshot(reference sessions.Reference) sessions.Session {
	h.t.Helper()
	for _, session := range h.host.Describe().Sessions {
		if session.Reference == reference {
			return session
		}
	}
	h.t.Fatal("the native runtime disappeared from Commander inventory")
	return sessions.Session{}
}

func (h *hermesSessionHost) awaitState(reference sessions.Reference, state string) sessions.Session {
	h.t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		view := h.snapshot(reference)
		if view.State == state {
			return view
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			h.t.Fatal("the native session did not settle in its expected inventory state")
			return sessions.Session{}
		}
	}
}

func hermesSessionCheck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal("native session fixture operation failed")
	}
}

func hermesSessionScript(t *testing.T, code, observation string) (*fakes.HermesModel, hermes.Request) {
	t.Helper()
	key := "fixture-native-session-" + id.New()
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		if call%2 == 1 {
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("native-session-step", "browser_exec", map[string]any{"code": code})}}
		}
		if observation != "" && !request.HasToolText(observation) {
			t.Error("native session did not preserve its browser observation")
		}
		return fakes.HermesResponse{Text: "Native fixture completed."}
	})
	return model, hermesRequest(model, []byte(key))
}

func TestHermesSessionsFrameworkDispatchReuseAndIsolation(t *testing.T) {
	var retained, isolated atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "native-account", Value: "alpha", Path: "/", HttpOnly: true})
			_, _ = io.WriteString(w, "<p>Native account established</p>")
			return
		}
		if cookie, err := r.Cookie("native-account"); err == nil && cookie.Value == "alpha" {
			retained.Add(1)
			_, _ = io.WriteString(w, "<p>Native account retained</p>")
			return
		}
		isolated.Add(1)
		_, _ = io.WriteString(w, "<p>Independent native session</p>")
	}))
	t.Cleanup(site.Close)
	firstModel, first := hermesSessionScript(t, fmt.Sprintf("# Establish the fixture account\nnew_tab(%q)\nprint(js('document.body.textContent'))", site.URL+"/set"), "Native account established")
	reuseModel, reuse := hermesSessionScript(t, fmt.Sprintf("# Read the retained account\ngoto_url(%q)\nprint(js('document.body.textContent'))", site.URL+"/read"), "Native account retained")
	freshModel, fresh := hermesSessionScript(t, fmt.Sprintf("# Read an independent fixture session\nnew_tab(%q)\nprint(js('document.body.textContent'))", site.URL+"/read"), "Independent native session")
	scope := make(chan context.Context, 1)
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{"first": {request: first, scope: scope}, "reuse": {request: reuse}, "fresh": {request: fresh}})
	root := h.task(nil, sessions.Open, "first")
	if root.SessionIntent.ID != root.ID || root.SessionIntent.ScopeID != root.ID {
		t.Fatal("root session identity came from business input instead of framework allocation")
	}
	opened := h.call(root)
	binding := h.binding(root)
	if opened.Session.Reference != binding.Reference || opened.Session.State != "connected" || firstModel.Calls() != 2 {
		t.Fatal("root native work did not retain its framework session")
	}
	legacyCalled := false
	_, legacyErr := h.manager.Run(action.WithSession(t.Context(), &binding), browserruntime.Options{
		Engine: browserruntime.Lightpanda, Executable: hermesConfig().Lightpanda,
	}, func(context.Context) error { legacyCalled = true; return nil })
	if legacyErr == nil || legacyCalled || firstModel.Calls() != 2 {
		t.Fatal("the same claim mixed legacy and native browser automation")
	}
	if _, err := hermes.Run(<-scope, first, nil); err == nil || firstModel.Calls() != 2 {
		t.Fatal("a retained context executed outside its owning manager callback")
	}
	child := h.task(&root, sessions.Open, "reuse")
	continued := h.call(child)
	if child.ID == root.ID || continued.Claim != opened.Claim || continued.Session.Reference != opened.Session.Reference {
		t.Fatal("direct-parent open continuation replaced its original session claim")
	}
	used := h.call(h.task(&child, sessions.Use, "reuse"))
	if used.Claim != opened.Claim || retained.Load() != 2 || reuseModel.Calls() != 4 {
		t.Fatal("use continuation lost the same authenticated browser")
	}
	independent := h.task(nil, sessions.Open, "fresh")
	freshResult := h.call(independent)
	if freshResult.Claim == opened.Claim || isolated.Load() != 1 || freshModel.Calls() != 2 {
		t.Fatal("independent roots shared authenticated browser state")
	}
	before := h.snapshot(binding.Reference)
	if !h.control(binding, "inspect") || !h.control(binding, "verify") {
		t.Fatal("native session metadata controls failed")
	}
	after := h.snapshot(binding.Reference)
	metadata, _ := json.Marshal(h.host.Describe().Sessions)
	for _, forbidden := range []string{site.URL, string(first.APIKey), "native-account", "ws://", "/tmp/commander", "document.body"} {
		if bytes.Contains(metadata, []byte(forbidden)) {
			t.Error("private browser state escaped through Commander inventory")
		}
	}
	if after.State != "connected" || after.Revision < before.Revision || after.Reference != before.Reference {
		t.Fatal("verification changed native ownership or lost inventory revisions")
	}
	if !h.control(binding, "close") || h.snapshot(binding.Reference).State != "closed" {
		t.Fatal("confirmed close did not retain a session tombstone")
	}
	beforeCalls := reuseModel.Calls()
	closedChild := h.task(&root, sessions.Open, "reuse")
	closedBinding := h.binding(closedChild)
	if _, ok := h.finish(h.dispatch(closedChild, &closedBinding)); ok || reuseModel.Calls() != beforeCalls {
		t.Fatal("a closed native session reopened or reached the model")
	}
}

func TestHermesSessionsRejectUnboundForeignClosedAndChangedClaims(t *testing.T) {
	model, request := hermesSessionScript(t, "# No native work is permitted\nprint(page_info())", "")
	readyKey := "fixture-worker-ready-" + id.New()
	readyModel := fakes.NewHermesModel(t, readyKey, func(_ fakes.HermesRequest, _ int) fakes.HermesResponse {
		return fakes.HermesResponse{Text: "The native worker is ready without a browser."}
	})
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{"blocked": {request: request}, "ready": {request: hermesRequest(readyModel, []byte(readyKey))}})
	for _, scenario := range []string{"unbound", "foreign", "closed", "changed_claim"} {
		t.Run(scenario, func(t *testing.T) {
			task := h.task(nil, sessions.Open, "blocked")
			binding := h.binding(task)
			assigned := &binding
			before := h.entered.Load()
			switch scenario {
			case "unbound":
				assigned = nil
			case "foreign":
				binding.OwnerAgentID = "browser-agent-89abcdef"
			case "closed":
				hermesSessionCheck(t, h.manager.Prepare(t.Context(), binding.Claim()))
				if !h.control(binding, "close") {
					t.Fatal("native close tombstone could not be prepared")
				}
			case "changed_claim":
				hermesSessionCheck(t, h.manager.Prepare(t.Context(), binding.Claim()))
				binding.CommandID, binding.OperationID, binding.TaskID = id.New(), id.OperationID(), id.TaskID()
			}
			if _, ok := h.finish(h.dispatch(task, assigned)); ok || model.Calls() != 0 {
				t.Fatal("an unauthorized native binding reached the SDK")
			}
			if scenario != "changed_claim" && h.entered.Load() != before {
				t.Fatal("framework session rejection occurred after action work began")
			}
		})
	}
	ready := h.task(nil, sessions.Open, "ready")
	if opened := h.call(ready); opened.Session.State != "starting" || readyModel.Calls() != 1 {
		t.Fatal("worker readiness was incorrectly advertised as a connected browser")
	}
	use := h.task(&ready, sessions.Use, "ready")
	binding := h.binding(use)
	before := h.entered.Load()
	if _, ok := h.finish(h.dispatch(use, &binding)); ok || readyModel.Calls() != 1 || h.entered.Load() != before {
		t.Fatal("use reached native work before a real browser was connected")
	}
}

func TestHermesSessionCloseInterruptsModelAndQuestionWaits(t *testing.T) {
	for _, mode := range []string{"model", "question"} {
		t.Run(mode, func(t *testing.T) {
			key := "fixture-session-close-" + id.New()
			model := fakes.NewHermesModel(t, key, func(_ fakes.HermesRequest, _ int) fakes.HermesResponse {
				if mode == "model" {
					return fakes.HermesResponse{Block: true}
				}
				return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("session-close-question", "clarify", map[string]any{"questions": []any{map[string]any{"question": "Which local fixture should be used?", "choices": []string{"First", "Second"}}}})}}
			})
			h := newHermesSessionHost(t, map[string]hermesSessionScenario{"wait": {request: hermesRequest(model, []byte(key))}})
			task := h.task(nil, sessions.Open, "wait")
			binding := h.binding(task)
			command := h.dispatch(task, &binding)
			if mode == "model" {
				select {
				case <-model.Blocked:
				case <-time.After(20 * time.Second):
					t.Fatal("the model HTTP request did not start")
				}
			} else {
				h.wait(func(m messages.Message) bool {
					return m.Type == controllerevents.QuestionRequested && m.CorrelationID == command.ID
				})
			}
			if !h.control(binding, "inspect") || !h.snapshot(binding.Reference).Busy {
				t.Fatal("inspection blocked behind pending native work")
			}
			if h.snapshot(binding.Reference).State != "starting" {
				t.Fatal("a worker with no browser was advertised as connected")
			}
			if !h.control(binding, "close") {
				t.Fatal("session close did not interrupt and join its pending work")
			}
			if _, ok := h.finish(command); ok || h.snapshot(binding.Reference).State != "closed" || model.Calls() != 1 {
				t.Fatal("pending native work survived confirmed session closure")
			}
			if mode == "model" {
				select {
				case <-model.Disconnected:
				case <-time.After(2 * time.Second):
					t.Error("provider HTTP remained active after session close")
				}
			}
		})
	}
}

func TestHermesSessionCleanCancellationPreservesBrowserAndPageLossDoesNotReplaceIt(t *testing.T) {
	var retained atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/set" {
			http.SetCookie(w, &http.Cookie{Name: "native-cancel", Value: "retained", Path: "/", HttpOnly: true})
			_, _ = io.WriteString(w, "<p>Cancellation fixture established</p>")
			return
		}
		if cookie, err := r.Cookie("native-cancel"); err == nil && cookie.Value == "retained" {
			retained.Add(1)
			_, _ = io.WriteString(w, "<p>Cancellation cookie retained</p>")
			return
		}
		_, _ = io.WriteString(w, "<p>Cancellation cookie missing</p>")
	}))
	t.Cleanup(site.Close)
	_, first := hermesSessionScript(t, fmt.Sprintf("# Establish cancellation fixture\nnew_tab(%q)\nprint(js('document.body.textContent'))", site.URL+"/set"), "Cancellation fixture established")
	key := "fixture-session-idle-cancel-" + id.New()
	blocked := fakes.NewHermesModel(t, key, func(_ fakes.HermesRequest, _ int) fakes.HermesResponse { return fakes.HermesResponse{Block: true} })
	reuseModel, reuse := hermesSessionScript(t, fmt.Sprintf("# Verify retained cancellation state\ngoto_url(%q)\nprint(js('document.body.textContent'))", site.URL+"/read"), "Cancellation cookie retained")
	lossModel, loss := hermesSessionScript(t, "# Close the current fixture page\npage = next(item for item in cdp('Target.getTargets')['targetInfos'] if item['type'] == 'page')\nprint(cdp('Target.closeTarget', targetId=page['targetId']))", "")
	cancel := make(chan struct{})
	cancelled := make(chan error, 1)
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{"first": {request: first}, "idle": {request: hermesRequest(blocked, []byte(key)), cancel: cancel, result: cancelled}, "reuse": {request: reuse}, "loss": {request: loss}})
	root := h.task(nil, sessions.Open, "first")
	opened := h.call(root)
	binding := h.binding(root)
	idle := h.task(&root, sessions.Open, "idle")
	idleBinding := h.binding(idle)
	command := h.dispatch(idle, &idleBinding)
	select {
	case <-blocked.Blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("the cancellable idle HTTP request did not start")
	}
	close(cancel)
	if _, ok := h.finish(command); ok {
		t.Fatal("cancelled native work reported success")
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("session cancellation lost its caller context cause")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled native action did not return its context cause")
	}
	select {
	case <-blocked.Disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("clean cancellation did not join the provider HTTP request")
	}
	if h.snapshot(binding.Reference).State != "connected" || blocked.Calls() != 1 {
		t.Fatal("clean idle cancellation discarded the retained browser")
	}
	if resumed := h.call(h.task(&idle, sessions.Use, "reuse")); resumed.Claim != opened.Claim || retained.Load() != 1 || reuseModel.Calls() != 2 {
		t.Fatal("clean cancellation replaced the authenticated browser")
	}
	closing := h.task(&root, sessions.Open, "loss")
	closingBinding := h.binding(closing)
	h.expectUncertainCleanup = true
	h.finish(h.dispatch(closing, &closingBinding))
	h.control(binding, "verify") // A closed page is a lost resource, never a restoration trigger.
	h.awaitState(binding.Reference, "lost")
	before := lossModel.Calls() + reuseModel.Calls()
	later := h.task(&root, sessions.Open, "reuse")
	laterBinding := h.binding(later)
	if _, ok := h.finish(h.dispatch(later, &laterBinding)); ok || lossModel.Calls()+reuseModel.Calls() != before || h.snapshot(binding.Reference).Reference != binding.Reference {
		t.Fatal("lost native work silently allocated a replacement runtime")
	}
}

func TestHermesSessionCancellationDuringNativeWorkRetiresClaim(t *testing.T) {
	started := make(chan struct{}, 1)
	var visits atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		visits.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<p>Native cancellation fixture entered</p>")
	}))
	t.Cleanup(site.Close)
	code := fmt.Sprintf("# Begin the native cancellation fixture\nnew_tab(%q)\nimport time\ntime.sleep(20)\nprint('Native fixture finished')", site.URL)
	model, request := hermesSessionScript(t, code, "Native fixture finished")
	cancel := make(chan struct{})
	returned := make(chan error, 1)
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{"native": {request: request, cancel: cancel, result: returned}})
	root := h.task(nil, sessions.Open, "native")
	binding := h.binding(root)
	command := h.dispatch(root, &binding)
	select {
	case <-started:
	case <-time.After(20 * time.Second):
		t.Fatal("the real native browser invocation did not reach its fixture")
	}
	h.expectUncertainCleanup = true
	close(cancel)
	if _, ok := h.finish(command); ok {
		t.Fatal("interrupted native work was reported as completed")
	}
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("native interruption lost its caller context cause")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupted native work did not return its context cause")
	}
	h.awaitState(binding.Reference, "lost")
	before := visits.Load()
	later := h.task(&root, sessions.Open, "native")
	laterBinding := h.binding(later)
	if _, ok := h.finish(h.dispatch(later, &laterBinding)); ok || model.Calls() != 1 || visits.Load() != before {
		t.Fatal("uncertain native work reached the model or silently replaced its browser")
	}
	if h.snapshot(binding.Reference).Reference != binding.Reference {
		t.Fatal("native interruption discarded its original session reference")
	}
}
