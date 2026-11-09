package browser_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	browser "github.com/defos-ai/commander-browser-agent"
	"github.com/defos-ai/commander-browser-agent/actions"
	runtime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
	runtimeSessions "github.com/defos-ai/commander/framework/sessions"
	fsartifacts "github.com/defos-ai/commander/providers/fs/artifacts"
)

func TestAgentCatalogMatchesMetadataAndPreservesEmptySelection(t *testing.T) {
	files, err := fsartifacts.NewArtifactFileFsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, err := browser.NewAgent(agent.AgentConfig{ArtifactFileStore: files})
	if err != nil {
		t.Fatal(err)
	}
	metadata, snapshot := browser.ActionDescriptions(), host.Describe()
	if !slices.Equal(browser.ActionNames(), []string{"browse", "login", "screenshot"}) || len(metadata) != len(snapshot.Actions) || !reflect.DeepEqual(browser.Responsibilities(), snapshot.Responsibilities) {
		t.Fatal("registered capabilities differ from release metadata")
	}
	first, second := browser.NewActions(nil), browser.NewActions(nil)
	for i, registered := range snapshot.Actions {
		if registered.Name != metadata[i].Name || registered.Description != metadata[i].Description || registered.SessionMode != metadata[i].SessionMode || len(registered.RequiredSecrets) != 0 || first[i] == second[i] {
			t.Fatal("catalog shares readiness, requires early secrets, or differs from metadata")
		}
	}
	metadata[0].Description = "caller mutation"
	if browser.ActionDescriptions()[0].Description == metadata[0].Description {
		t.Fatal("metadata shares mutable state")
	}
	empty, err := browser.NewAgent(agent.AgentConfig{ArtifactFileStore: files, Actions: []action.Executor{}})
	if err != nil || len(empty.Describe().Actions) != 0 {
		t.Fatal("explicitly empty catalog gained default actions")
	}
}

func TestSessionClaimsAndDelayedCloseKeepIdentitiesIsolated(t *testing.T) {
	binding := unitBinding()
	manager, err := runtime.New(binding.OwnerAgentID)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil || manager.Snapshot()[0].State != "starting" {
		t.Fatal("preparation did not retain an unstarted session")
	}
	conflict := binding
	conflict.OperationID = id.New()
	if err := manager.Prepare(t.Context(), conflict.Claim()); !errors.Is(err, sessions.ErrConflict) {
		t.Fatal("retained identity accepted a conflicting creation")
	}
	foreign := unitBinding()
	foreign.OwnerAgentID = "browser-agent-89abcdef"
	if err := manager.Prepare(t.Context(), foreign.Claim()); !errors.Is(err, sessions.ErrConflict) {
		t.Fatal("session accepted a different owning agent")
	}
	replacement := unitBinding()
	replacement.ScopeID = binding.ScopeID
	if err := manager.Prepare(t.Context(), replacement.Claim()); err != nil {
		t.Fatal("controller-admitted replacement was rejected")
	}
	closed, err := manager.Control(t.Context(), sessions.Control{Reference: binding.Reference, Method: "close", RequestID: id.New(), CommandID: binding.CommandID, OperationID: binding.OperationID, TaskID: binding.TaskID})
	if err != nil || closed.State != "closed" || len(manager.Snapshot()) != 2 {
		t.Fatal("close discarded identity evidence")
	}
	if err := manager.Prepare(t.Context(), binding.Claim()); !errors.Is(err, sessions.ErrSessionClosed) {
		t.Fatal("delayed preparation restored a closed identity")
	}
	called := false
	err = manager.WithSession(action.WithSession(t.Context(), &replacement), func(context.Context) error { called = true; return nil })
	if err != nil || !called {
		t.Fatal("closing an old identity affected its independent replacement")
	}
}

func TestStandaloneSessionClaimsRetainExactFlatCreation(t *testing.T) {
	for _, withoutOperation := range []bool{false, true} {
		binding := unitBinding()
		binding.CommandID = id.New() // A standalone command need not equal its task ID.
		if withoutOperation {
			binding.OperationID, binding.TaskID = "", ""
		}
		manager, err := runtime.New(binding.OwnerAgentID)
		if err != nil {
			t.Fatal(err)
		}
		closed, err := manager.Control(t.Context(), sessions.Control{Reference: binding.Reference, Method: "close", RequestID: id.New(), CommandID: binding.CommandID, OperationID: binding.OperationID, TaskID: binding.TaskID})
		if err != nil || closed.State != "closed" || !errors.Is(manager.Prepare(t.Context(), binding.Claim()), sessions.ErrSessionClosed) {
			t.Fatal("delayed standalone creation escaped its exact close tombstone")
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionContractsAllocateRootIdentityAndKeepContinuations(t *testing.T) {
	raw := json.RawMessage(`{"url":"https://example.test/","objective":"Read the page"}`)
	for _, opener := range []action.Executor{actions.NewBrowse(nil), actions.NewLogin(nil)} {
		definition := opener.Action()
		if definition.Name == "login" {
			raw = json.RawMessage(`{"url":"https://example.test/","verification_url":"https://example.test/account","expected_text":"Signed in"}`)
		}
		next, err := runtimeSessions.Normalize(nil, []action.NextAction{{ActionName: definition.Name, Input: raw}}, nil, definition.Definition())
		if err != nil || len(next) != 1 || next[0].SessionIntent == nil || next[0].SessionIntent.Error != "" || definition.SessionMode != string(sessions.Open) {
			t.Fatal("root browser work did not receive an open-session intent")
		}
		root := operations.NewTask(id.OperationID(), "", next[0])
		if root.SessionIntent.ID != root.ID || root.SessionIntent.ScopeID != root.ID || root.SessionIntent.Validate() != nil || string(root.Input) != string(raw) {
			t.Fatal("root identity was not controller-allocated independently of business input")
		}
		for _, executor := range []action.Executor{actions.NewBrowse(nil), actions.NewLogin(nil), actions.NewScreenshot(nil)} {
			contract := executor.Action()
			children, err := runtimeSessions.Normalize(&root, []action.NextAction{{ActionName: contract.Name, Input: json.RawMessage(`{}`)}}, nil, contract.Definition())
			if err != nil || len(children) != 1 || children[0].SessionIntent == nil {
				t.Fatal("browser continuation did not resolve its session contract")
			}
			child := operations.NewTask(root.OperationID, root.ID, children[0])
			if child.ID == root.ID || child.SessionIntent.ID != root.SessionIntent.ID || child.SessionIntent.ScopeID != root.SessionIntent.ScopeID || string(child.SessionIntent.Mode) != contract.SessionMode || child.SessionIntent.Validate() != nil {
				t.Fatal("open/use continuation replaced its inherited browser identity")
			}
		}
	}
	screenshot := actions.NewScreenshot(nil).Action()
	next, err := runtimeSessions.Normalize(nil, []action.NextAction{{ActionName: screenshot.Name}}, nil, screenshot.Definition())
	if err != nil || next[0].SessionIntent == nil || next[0].SessionIntent.Error == "" {
		t.Fatal("use-only screenshot invented a root browser session")
	}
}

func TestActionsRejectSessionSelectorsBeforeSideEffects(t *testing.T) {
	for _, name := range []string{"browse", "login", "screenshot"} {
		for _, selector := range []string{"session_scope", "session_id"} {
			t.Run(name+"/"+selector, func(t *testing.T) {
				probe := &unitSetup{t: t}
				var executor action.Executor = actions.NewBrowse(probe)
				input := map[string]any{"url": "https://example.test/", "objective": "Read the page"}
				binding := unitBinding()
				if name == "login" {
					executor, input = actions.NewLogin(probe), map[string]any{"url": "https://example.test/", "verification_url": "https://example.test/account", "expected_text": "Signed in", "credential_profile": "work"}
				} else if name == "screenshot" {
					executor, input, binding.Mode = actions.NewScreenshot(probe), map[string]any{}, sessions.Use
				}
				if status, err := executor.Discover(t.Context(), unitEnvironment{probe}, nil); err != nil || status != action.Ready {
					t.Fatal("selector regression could not discover its action")
				}
				input[selector] = id.New()
				raw, _ := json.Marshal(input)
				ctx := action.WithQuestions(action.WithSecrets(action.WithSession(t.Context(), &binding), probe), probe)
				result, err := executor.Run(ctx, raw)
				if err == nil || len(probe.events) != 0 || probe.runs != 0 || len(result.Output) != 0 || len(result.Artifacts) != 0 {
					t.Fatal("business input session selector reached setup or browser work")
				}
			})
		}
	}
}

func TestBrowseSetupUsesManagedKeysAndRetainsOnlySessionSelection(t *testing.T) {
	const endpoint = "https://custom.example.test/v1/chat/completions"
	for _, provider := range []string{"OpenAI", "Other"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("BROWSER_MODEL_PROVIDER", "invalid-legacy-provider")
			t.Setenv("BROWSER_MODEL_API_KEY", "ignored-environment-key")
			t.Setenv("OPENAI_API_KEY", "ignored-environment-key")
			probe := &unitSetup{t: t, key: "fixture-first-private-model-key", answers: map[string]string{"browser_model_provider": provider, "browser_model_name": "exact-model-v1", "browser_model_url": endpoint}}
			executor := actions.NewBrowse(probe)
			if status, err := executor.Discover(t.Context(), unitEnvironment{probe}, nil); err != nil || status != action.Ready || len(probe.events) != 0 {
				t.Fatal("discovery started setup or required credentials")
			}
			binding := unitBinding()
			run := func(binding sessions.Binding) error {
				input := map[string]any{"objective": "Find a price", "url": "https://example.test/"}
				raw, _ := json.Marshal(input)
				ctx := action.WithQuestions(action.WithSecrets(action.WithSession(t.Context(), &binding), probe), probe)
				result, err := executor.Run(ctx, raw)
				if err != nil && (strings.Contains(err.Error(), probe.key) || len(result.Output) != 0 || len(result.Artifacts) != 0) {
					t.Fatal("failed browser work exposed credentials or artifacts")
				}
				return err
			}
			name := "OPENAI_API_KEY"
			want := []string{"browser_model_provider", "browser_model_name"}
			if provider == "Other" {
				name = fmt.Sprintf("browser-model-api-key-%x", sha256.Sum256([]byte(endpoint)))
				want = append(want, "browser_model_url")
			}
			want = append(want, name)
			if run(binding) == nil || !slices.Equal(probe.events, want) || probe.runs != 1 {
				t.Fatal("setup did not ask provider/model before its canonical managed key")
			}
			binding.Mode, probe.key, probe.events = sessions.Open, "fixture-rotated-private-model-key", nil
			if run(binding) == nil || !slices.Equal(probe.events, []string{name}) || probe.runs != 2 {
				t.Fatal("reuse repeated questions or cached a plaintext key")
			}
			binding.OperationID = id.New()
			if err := run(binding); !errors.Is(err, sessions.ErrConflict) || len(probe.events) != 1 || probe.runs != 2 {
				t.Fatal("conflicting creation reused model setup")
			}
			for _, value := range probe.returned {
				if bytesRemain := strings.Trim(string(value), "\x00"); bytesRemain != "" {
					t.Fatal("managed key buffer was retained")
				}
			}
		})
	}
}

func TestCheckpointRecoveryGateStopsBeforeModelSetup(t *testing.T) {
	for _, failure := range []error{nil, errors.New("private-checkpoint-provider-error")} {
		probe := &unitSetup{t: t}
		executor := actions.NewBrowse(probe)
		if status, err := executor.Discover(t.Context(), unitEnvironment{probe}, nil); err != nil || status != action.Ready {
			t.Fatal("checkpoint fixture discovery failed")
		}
		binding := unitBinding()
		raw, _ := json.Marshal(map[string]string{"objective": "Find a price", "url": "https://example.test/"})
		ctx := action.WithCheckpoints(action.WithSession(t.Context(), &binding), unitCheckpoint{failure})
		ctx = action.WithQuestions(action.WithSecrets(ctx, probe), probe)
		result, err := executor.Run(ctx, raw)
		if err == nil || strings.Contains(err.Error(), "private-checkpoint-provider-error") || len(probe.events) != 0 || probe.runs != 0 || len(result.Output) != 0 || len(result.Artifacts) != 0 {
			t.Fatal("saved or unconfirmed progress reached setup/browser work or exposed provider details")
		}
	}
}

type unitCheckpoint struct{ err error }

func (p unitCheckpoint) LoadCheckpoint(context.Context, string) (json.RawMessage, bool, error) {
	return json.RawMessage(`{}`), true, p.err
}
func (unitCheckpoint) SaveCheckpoint(context.Context, string, json.RawMessage) error {
	return errors.New("unexpected checkpoint write")
}

type unitSetup struct {
	t          *testing.T
	key        string
	executable string
	answers    map[string]string
	events     []string
	returned   [][]byte
	runs       int
}

func (p *unitSetup) LookupEnv(name string) (string, bool) {
	if name != "LIGHTPANDA_PATH" {
		p.t.Error("agent read model configuration from the environment")
	}
	if p.executable == "" {
		p.executable = filepath.Join(p.t.TempDir(), "lightpanda")
	}
	return p.executable, true
}
func (p *unitSetup) FindExecutable(context.Context, string, *environment.DiscoveryScope) (string, error) {
	return p.executable, nil
}

type unitEnvironment struct{ *unitSetup }

func (unitEnvironment) Run(context.Context, []string) (environment.Result, error) {
	return environment.Result{Stdout: "1.0.2"}, nil
}
func (p *unitSetup) Question(_ context.Context, key, text string, options []string) (operations.Question, error) {
	p.events = append(p.events, key)
	return operations.Question{Status: operations.QuestionAnswered, Key: key, Text: text, Options: options, Answer: p.answers[key], AnsweredBy: operations.AnswerOperator}, nil
}
func (*unitSetup) Secret(string) ([]byte, error) { return nil, action.ErrSecretUnavailable }
func (p *unitSetup) RequestSecret(_ context.Context, name string) ([]byte, error) {
	p.events = append(p.events, name)
	value := []byte(p.key)
	p.returned = append(p.returned, value)
	return value, nil
}
func (p *unitSetup) Run(context.Context, runtime.Options, func(context.Context) error) (sessions.Session, error) {
	p.runs++
	return sessions.Session{}, errors.New("fixture browser failure: " + p.key)
}

func unitBinding() sessions.Binding {
	task := id.New()
	return sessions.Binding{Reference: sessions.Reference{ID: task, ScopeID: task, OwnerAgentID: "browser-agent-0123abcd"}, Mode: sessions.Open, OperationID: id.New(), TaskID: task, CommandID: task}
}
