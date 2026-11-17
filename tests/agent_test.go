package browser_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	browser "github.com/defos-ai/commander-browser-agent"
	"github.com/defos-ai/commander-browser-agent/actions"
	runtime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	hermesagent "github.com/defos-ai/commander-browser-agent/browser/hermes"
	"github.com/defos-ai/commander-browser-agent/browser/lightpanda"
	"github.com/defos-ai/commander-browser-agent/internal/commander"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	agentmodel "github.com/defos-ai/commander/framework/domain/agents"
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
	host, err := browser.NewAgent(agent.AgentConfig{ArtifactFileStore: files}, realBrowserDependencies())
	if err != nil {
		t.Fatal(err)
	}
	metadata, snapshot := browser.ActionDescriptions(), host.Describe()
	if !slices.Equal(browser.ActionNames(), []string{"browse", "login", "screenshot"}) || len(metadata) != len(snapshot.Actions) || !reflect.DeepEqual(browser.Responsibilities(), snapshot.Responsibilities) {
		t.Fatal("registered capabilities differ from release metadata")
	}
	dependencies := browser.Dependencies()
	versions := map[string]string{"lightpanda": "1.0.2", "hermes-runtime": "v0.23.1"}
	if !reflect.DeepEqual(dependencies, snapshot.Dependencies) || len(dependencies) != len(versions) {
		t.Fatal("registered runtime dependencies differ from release metadata")
	}
	for _, dependency := range dependencies {
		version, exists := versions[dependency.Name]
		if !exists || dependency.Version != version || strings.TrimSpace(dependency.Description) == "" {
			t.Fatal("runtime dependency metadata lost its executable requirement, version or description")
		}
		delete(versions, dependency.Name)
	}
	first, second := browser.NewActions(nil, realBrowserDependencies()), browser.NewActions(nil, realBrowserDependencies())
	for i, registered := range snapshot.Actions {
		if registered.Name != metadata[i].Name || registered.Description != metadata[i].Description || registered.SessionMode != metadata[i].SessionMode || len(registered.RequiredSecrets) != 0 || first[i] == second[i] {
			t.Fatal("catalog shares readiness, requires early secrets, or differs from metadata")
		}
	}
	metadata[0].Description = "caller mutation"
	if browser.ActionDescriptions()[0].Description == metadata[0].Description {
		t.Fatal("metadata shares mutable state")
	}
	dependencies[0].Description = "caller mutation"
	if !reflect.DeepEqual(browser.Dependencies(), snapshot.Dependencies) {
		t.Fatal("dependency metadata shares mutable state")
	}
	empty, err := browser.NewAgent(agent.AgentConfig{ArtifactFileStore: files, Actions: []action.Executor{}, Dependencies: []agentmodel.Dependency{}}, realBrowserDependencies())
	if err != nil || len(empty.Describe().Actions) != 0 || len(empty.Describe().Dependencies) != 0 {
		t.Fatal("explicitly empty catalog gained default actions or dependencies")
	}
	custom := []agentmodel.Dependency{{Name: "custom-runtime", Description: "Run the caller's browser integration.", Version: "2.0"}}
	configured, err := browser.NewAgent(agent.AgentConfig{ArtifactFileStore: files, Dependencies: custom}, realBrowserDependencies())
	if err != nil || !reflect.DeepEqual(configured.Describe().Dependencies, custom) {
		t.Fatal("caller dependency declarations were replaced or merged with defaults")
	}
}

func TestSessionClaimsAndDelayedCloseKeepIdentitiesIsolated(t *testing.T) {
	binding := unitBinding()
	manager, err := commander.NewSessionManager(binding.OwnerAgentID)
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
		manager, err := commander.NewSessionManager(binding.OwnerAgentID)
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
	for _, opener := range []action.Executor{actions.NewBrowse(nil, nil, nil), actions.NewLogin(nil, nil)} {
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
		for _, executor := range []action.Executor{actions.NewBrowse(nil, nil, nil), actions.NewLogin(nil, nil), actions.NewScreenshot(nil, nil)} {
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
	screenshot := actions.NewScreenshot(nil, nil).Action()
	next, err := runtimeSessions.Normalize(nil, []action.NextAction{{ActionName: screenshot.Name}}, nil, screenshot.Definition())
	if err != nil || next[0].SessionIntent == nil || next[0].SessionIntent.Error == "" {
		t.Fatal("use-only screenshot invented a root browser session")
	}
}

func TestActionsRejectSessionSelectorsBeforeSideEffects(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	for _, name := range []string{"browse", "login", "screenshot"} {
		for _, selector := range []string{"session_scope", "session_id"} {
			t.Run(name+"/"+selector, func(t *testing.T) {
				probe := &unitSetup{t: t}
				var executor action.Executor = unitAction(probe, "browse")
				input := map[string]any{"url": "https://example.test/", "objective": "Read the page"}
				binding := unitBinding()
				if name == "login" {
					executor, input = unitAction(probe, "login"), map[string]any{"url": "https://example.test/", "verification_url": "https://example.test/account", "expected_text": "Signed in", "credential_profile": "work"}
				} else if name == "screenshot" {
					executor, input, binding.Mode = unitAction(probe, "screenshot"), map[string]any{}, sessions.Use
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

func TestScreenshotRejectsOpenBindingBeforeSideEffects(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	probe := &unitSetup{t: t}
	executor := unitAction(probe, "screenshot")
	if status, err := executor.Discover(t.Context(), unitEnvironment{probe}, nil); err != nil || status != action.Ready {
		t.Fatal("screenshot binding regression could not discover its action")
	}
	binding := unitBinding()
	ctx := action.WithQuestions(action.WithSecrets(action.WithSession(t.Context(), &binding), probe), probe)
	ctx = action.WithLogs(ctx, probe)
	result, err := executor.Run(ctx, json.RawMessage(`{}`))
	if err == nil || len(probe.events) != 0 || probe.runs != 0 || len(result.Output) != 0 || len(result.Artifacts) != 0 {
		t.Fatal("screenshot accepted an open binding or reached logs, setup or browser work")
	}
}

func TestBrowseSetupValidatesBeforeRememberingAndRetainsOnlySessionSelection(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	const endpoint = "https://custom.example.test/v1/chat/completions"
	for _, test := range []struct {
		name, envProvider, envModel, provider string
		questions                             []string
	}{
		{name: "questions_openai", provider: "openai", questions: []string{"browser_model_provider_1", "browser_model_1"}},
		{name: "environment_openai", envProvider: "openai", envModel: "chat-latest", provider: "openai"},
		{name: "question_provider", envModel: "chat-latest", provider: "openai", questions: []string{"browser_model_provider_1"}},
		{name: "custom_model", envProvider: "other", provider: "other", questions: []string{"browser_model_1", "browser_model_url_1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BROWSER_MODEL_PROVIDER", test.envProvider)
			t.Setenv("BROWSER_MODEL_NAME", test.envModel)
			t.Setenv("BROWSER_MODEL_URL", "https://ignored.example.test/v1/chat/completions")
			t.Setenv("BROWSER_MODEL_API_KEY", "ignored-environment-key")
			t.Setenv("OPENAI_API_KEY", "ignored-environment-key")
			probe := &unitSetup{t: t, key: "fixture-first-private-model-key", provider: test.provider, model: "exact-model-v1", endpoint: endpoint}
			executor := unitAction(probe, "browse")
			if status, err := executor.Discover(t.Context(), unitEnvironment{probe}, nil); err != nil || status != action.Ready || len(probe.events) != 0 {
				t.Fatal("discovery started setup or required credentials")
			}
			binding := unitBinding()
			run := func(binding sessions.Binding) error {
				ctx := unitSetupContext(t, probe, binding)
				result, err := executor.Run(ctx, json.RawMessage(`{"objective":"Find a price","url":"https://example.test/"}`))
				if err != nil && (strings.Contains(err.Error(), probe.key) || len(result.Output) != 0 || len(result.Artifacts) != 0) {
					t.Fatal("failed browser work exposed credentials or artifacts")
				}
				return err
			}
			name := "OPENAI_API_KEY"
			if test.provider == "other" {
				name = fmt.Sprintf("browser-model-api-key-%x", sha256.Sum256([]byte(endpoint)))
			}
			want := append(slices.Clone(test.questions), name, "validate-model", "seed:"+commander.ModelProviderSecretName)
			if test.provider == "other" {
				want = append(want, "seed:"+commander.ModelURLSecretName)
			}
			want = append(want, "seed:"+commander.ModelNameSecretName)
			if run(binding) == nil || !slices.Equal(probe.events, want) || probe.runs != 1 || probe.validations != 1 {
				t.Fatal("setup did not ask, request its canonical key, validate and only then remember settings")
			}
			selectedModel := test.envModel
			if selectedModel == "" {
				selectedModel = probe.model
			}
			if probe.defaults[commander.ModelProviderSecretName] != test.provider || probe.defaults[commander.ModelNameSecretName] != selectedModel || (test.provider == "other" && probe.defaults[commander.ModelURLSecretName] != endpoint) {
				t.Fatal("successful validation did not seed the exact selected settings")
			}
			t.Setenv("BROWSER_MODEL_PROVIDER", "changed-invalid-provider")
			t.Setenv("BROWSER_MODEL_NAME", "changed-model")
			probe.provider, probe.model, probe.endpoint = "changed-invalid-provider", "changed-model", "https://changed.example.test/v1/chat/completions"
			probe.key, probe.events = "fixture-rotated-private-model-key", nil
			if run(binding) == nil || !slices.Equal(probe.events, []string{name}) || probe.runs != 2 || probe.validations != 1 {
				t.Fatal("reuse repeated questions, validation or defaults, or cached a plaintext key")
			}
			binding.OperationID = id.New()
			if err := run(binding); !errors.Is(err, sessions.ErrConflict) || len(probe.events) != 1 || probe.runs != 2 {
				t.Fatal("conflicting creation reused model setup")
			}
			assertUnitSetupCleared(t, probe)
		})
	}
}

func TestModelSetupRetriesSelectionsAtMostThreeTimesWithoutCachingFailure(t *testing.T) {
	for _, test := range []struct {
		name, provider, model, endpoint string
		failure                         error
		questions                       []string
		validations                     int
	}{
		{name: "provider", provider: "fixture-private-invalid-provider", questions: []string{"browser_model_provider_"}},
		{name: "model", provider: "openai", model: "fixture-private-invalid-model\n", questions: []string{"browser_model_provider_", "browser_model_"}},
		{name: "endpoint", provider: "other", model: "chat-latest", endpoint: "http://remote.example.test/v1/chat/completions", questions: []string{"browser_model_provider_", "browser_model_", "browser_model_url_"}},
		{name: "request_rejected", provider: "openai", model: "chat-latest", failure: contracts.ErrModelRequestRejected, questions: []string{"browser_model_provider_", "browser_model_"}, validations: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BROWSER_MODEL_PROVIDER", "")
			t.Setenv("BROWSER_MODEL_NAME", "")
			probe := &unitSetup{t: t, key: "fixture-rejected-private-key", provider: test.provider, model: test.model, endpoint: test.endpoint, validationError: test.failure}
			setup := commander.Setup{ValidateModel: probe.validateModel}
			ctx := unitSetupContext(t, probe, unitBinding())
			var want []string
			for attempt := 1; attempt <= 3; attempt++ {
				for _, prefix := range test.questions {
					want = append(want, fmt.Sprintf("%s%d", prefix, attempt))
				}
				if test.validations != 0 {
					want = append(want, "OPENAI_API_KEY", "validate-model")
				}
			}
			for run := 1; run <= 2; run++ {
				model, err := setup.PrepareModel(ctx)
				if err == nil || len(model.APIKey) != 0 || strings.Contains(err.Error(), "fixture-private") || probe.validations != run*test.validations || !slices.Equal(probe.events, slices.Repeat(want, run)) {
					t.Fatal("invalid selection escaped its three-attempt bound, leaked values or was cached")
				}
			}
			assertUnitSetupCleared(t, probe)
		})
	}
}

func TestModelSetupRetriesRejectedEnvironmentSelectionWithQuestions(t *testing.T) {
	t.Setenv("BROWSER_MODEL_PROVIDER", "openai")
	t.Setenv("BROWSER_MODEL_NAME", "rejected-model")
	probe := &unitSetup{t: t, key: "fixture-retry-private-key", provider: "openai", model: "working-model", validationFailures: []error{contracts.ErrModelRequestRejected, nil}}
	setup := commander.Setup{ValidateModel: probe.validateModel}
	model, err := setup.PrepareModel(unitSetupContext(t, probe, unitBinding()))
	defer clear(model.APIKey)
	want := []string{"OPENAI_API_KEY", "validate-model", "browser_model_provider_2", "browser_model_2", "OPENAI_API_KEY", "validate-model", "seed:" + commander.ModelProviderSecretName, "seed:" + commander.ModelNameSecretName}
	if err != nil || model.Model != "working-model" || probe.validations != 2 || !slices.Equal(probe.events, want) {
		t.Fatal("a rejected environment candidate was retried without operator/model selection or seeded before success")
	}
	clear(model.APIKey)
	assertUnitSetupCleared(t, probe)
}

func TestModelSetupTerminalFailuresDoNotRememberAndClearCredentials(t *testing.T) {
	for _, test := range []struct {
		name     string
		failure  error
		keyError bool
		conflict bool
	}{
		{name: "credentials", failure: contracts.ErrModelCredentials},
		{name: "rate_limit", failure: contracts.ErrModelRateLimited},
		{name: "availability", failure: contracts.ErrModelUnavailable},
		{name: "unsafe_response", failure: contracts.ErrModelResponse},
		{name: "cancelled", failure: context.Canceled},
		{name: "key_delivery", keyError: true},
		{name: "default_conflict", conflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("BROWSER_MODEL_PROVIDER", "openai")
			t.Setenv("BROWSER_MODEL_NAME", "chat-latest")
			probe := &unitSetup{t: t, key: "fixture-terminal-private-key", provider: "openai", model: "chat-latest", validationError: test.failure}
			if test.keyError {
				probe.keyError = errors.New("fixture-private-delivery-error")
			}
			if test.conflict {
				probe.seedConflict = commander.ModelProviderSecretName
			}
			setup := commander.Setup{ValidateModel: probe.validateModel}
			binding := unitBinding()
			model, err := setup.PrepareModel(unitSetupContext(t, probe, binding))
			want := []string{"OPENAI_API_KEY"}
			if !test.keyError {
				want = append(want, "validate-model")
			}
			if test.conflict {
				want = append(want, "seed:"+commander.ModelProviderSecretName)
			}
			if err == nil || len(model.APIKey) != 0 || strings.Contains(err.Error(), probe.key) || !slices.Equal(probe.events, want) {
				t.Fatal("a terminal failure retried, remembered settings or exposed a credential")
			}
			if test.failure != nil && !errors.Is(err, test.failure) {
				t.Fatal("validation failure lost its safe category")
			}
			assertUnitSetupCleared(t, probe)
			if test.conflict {
				probe.events, probe.seedConflict = nil, ""
				model, err = setup.PrepareModel(unitSetupContext(t, probe, binding))
				clear(model.APIKey)
				if err != nil || probe.validations != 2 {
					t.Fatal("a conflicting default was cached before all receipts matched")
				}
			}
		})
	}
}

func unitSetupContext(t *testing.T, probe *unitSetup, binding sessions.Binding) context.Context {
	t.Helper()
	return action.WithQuestions(action.WithSecrets(action.WithSession(t.Context(), &binding), probe), probe)
}

func assertUnitSetupCleared(t *testing.T, probe *unitSetup) {
	t.Helper()
	for _, value := range probe.returned {
		if strings.Trim(string(value), "\x00") != "" {
			t.Fatal("model setup retained an owned credential or default receipt buffer")
		}
	}
}

func TestCheckpointRecoveryGateStopsBeforeModelSetup(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	for _, failure := range []error{nil, errors.New("private-checkpoint-provider-error")} {
		probe := &unitSetup{t: t}
		executor := unitAction(probe, "browse")
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

const unitHermesRoot = "/trusted/hermes-fixture"
const unitLightpandaExecutable = "/trusted/lightpanda"

type unitSetup struct {
	t                     *testing.T
	key                   string
	provider              string
	model                 string
	endpoint              string
	keyError              error
	validationError       error
	validationFailures    []error
	seedConflict          string
	validations           int
	defaults              map[string]string
	unavailableHermes     bool
	unavailableLightpanda bool
	discoveryRuns         int
	values                *sensitive.Values
	events                []string
	returned              [][]byte
	runs                  int
}

func (p *unitSetup) LookupEnv(name string) (string, bool) {
	switch name {
	case "HERMES_INSTALL_ROOT":
		return unitHermesRoot, true
	case "LIGHTPANDA_PATH":
		return unitLightpandaExecutable, true
	default:
		p.t.Error("agent read configuration outside the packaged runtime paths")
		return "", false
	}
}
func (p *unitSetup) FindExecutable(_ context.Context, name string, scope *environment.DiscoveryScope) (string, error) {
	if name != "lightpanda" {
		p.t.Error("runtime discovery searched an unexpected executable")
		return "", errors.New("unexpected executable")
	}
	if scope != nil && scope.Paths != nil && !slices.Contains(scope.Paths, "/trusted") {
		return "", nil
	}
	return unitLightpandaExecutable, nil
}

type unitEnvironment struct{ *unitSetup }

func (p unitEnvironment) Run(_ context.Context, argv []string) (environment.Result, error) {
	p.discoveryRuns++
	if len(argv) == 7 && argv[0] == unitHermesRoot+"/.venv/bin/python" && argv[1] == "-I" && argv[2] == "-S" && argv[3] == "-c" && argv[4] != "" && argv[5] == "--source" && argv[6] == unitHermesRoot {
		if p.unavailableHermes {
			return environment.Result{Stdout: "private-runtime-output", ReturnCode: 1}, nil
		}
		return environment.Result{Stdout: hermesReadyReceipt}, nil
	}
	if slices.Equal(argv, []string{unitLightpandaExecutable, "version"}) {
		if p.unavailableLightpanda {
			return environment.Result{Stdout: "private-runtime-output", ReturnCode: 1}, nil
		}
		return environment.Result{Stdout: "1.0.2"}, nil
	}
	p.t.Error("runtime discovery invoked an unexpected program")
	return environment.Result{}, errors.New("unexpected execution")
}
func (p *unitSetup) Question(_ context.Context, key, text string, options []string) (operations.Question, error) {
	p.events = append(p.events, key)
	if p.key != "" && (strings.Contains(text, p.key) || strings.Contains(strings.Join(options, " "), p.key)) {
		p.t.Error("model setup placed an API key in a question")
	}
	answer := p.model
	switch {
	case strings.HasPrefix(key, "browser_model_provider_"):
		answer = p.provider
		if !slices.Equal(options, []string{"OpenAI", "Other (OpenAI-compatible)"}) {
			p.t.Error("model provider question lost its supported choices")
		}
	case strings.HasPrefix(key, "browser_model_url_"):
		answer = p.endpoint
	}
	return operations.Question{Status: operations.QuestionAnswered, Key: key, Text: text, Options: options, Answer: answer, AnsweredBy: operations.AnswerOperator}, nil
}
func (*unitSetup) Secret(string) ([]byte, error) { return nil, action.ErrSecretUnavailable }
func (p *unitSetup) RequestSecretWithOptions(_ context.Context, name string, options action.SecretRequestOptions) ([]byte, error) {
	if options.Type != action.SecretReusable {
		p.t.Error("model setup changed its reusable delivery type")
	}
	value := []byte(p.key)
	if name == commander.ModelProviderSecretName || name == commander.ModelNameSecretName || name == commander.ModelURLSecretName {
		p.events = append(p.events, "seed:"+name)
		if options.Default == nil || string(options.Default) == p.key {
			p.t.Error("model settings lost their selected default or received an API key")
		}
		if p.defaults == nil {
			p.defaults = make(map[string]string)
		}
		p.defaults[name] = string(options.Default)
		value = slices.Clone(options.Default)
		if p.seedConflict == name {
			value = []byte("different-existing-setting")
		}
	} else {
		p.events = append(p.events, name)
		if options.Default != nil {
			p.t.Error("API key request supplied a default")
		}
	}
	p.returned = append(p.returned, value)
	if options.Default == nil {
		return value, p.keyError
	}
	return value, nil
}
func (p *unitSetup) validateModel(_ context.Context, model contracts.ModelConfig) error {
	p.events = append(p.events, "validate-model")
	p.validations++
	if string(model.APIKey) != p.key || model.Model == "" || model.Endpoint == "" {
		p.t.Error("model validation did not receive the selected managed key and complete tuple")
	}
	if p.validations <= len(p.validationFailures) {
		return p.validationFailures[p.validations-1]
	}
	return p.validationError
}
func (p *unitSetup) Log(ctx context.Context, _ string) error {
	p.events = append(p.events, "execution-log")
	return ctx.Err()
}
func (p *unitSetup) WithSession(ctx context.Context, work func(context.Context) error) error {
	if p.values == nil {
		p.values = sensitive.New()
		p.t.Cleanup(p.values.Clear)
	}
	return work(sensitive.WithValues(ctx, p.values))
}
func (p *unitSetup) RunHermes(_ context.Context, options runtime.HermesOptions, _ func(context.Context) error) (sessions.Session, error) {
	p.runs++
	if options != (runtime.HermesOptions{Python: unitHermesRoot + "/.venv/bin/python", Source: unitHermesRoot, Executable: unitLightpandaExecutable}) {
		p.t.Error("action did not use the discovered local runtime")
	}
	return sessions.Session{}, errors.New("fixture browser failure: " + p.key)
}

func unitBinding() sessions.Binding {
	task := id.New()
	return sessions.Binding{Reference: sessions.Reference{ID: task, ScopeID: task, OwnerAgentID: "browser-agent-0123abcd"}, Mode: sessions.Open, OperationID: id.New(), TaskID: task, CommandID: task}
}

// The test host supplies the real adapters through the same dependency contract
// as CLI composition. These factories remain lazy and create no runtime owner.
func realBrowserDependencies() actions.Dependencies {
	return actions.Dependencies{
		ValidateModel: hermesagent.ValidateModel,
		BrowserAgent: func(ctx context.Context, model contracts.ModelConfig) (contracts.BrowserAgent, error) {
			return hermesagent.NewAgent(ctx, model)
		},
		Browser: func(ctx context.Context) (contracts.Browser, error) {
			return lightpanda.NewBrowser(ctx)
		},
	}
}

// Unit scenarios exercise the host's fresh actions with explicitly supplied adapters.
func unitAction(runner actions.BrowserRunner, name string) action.Executor {
	dependencies := realBrowserDependencies()
	if probe, ok := runner.(*unitSetup); ok {
		dependencies.ValidateModel = probe.validateModel
	}
	for _, executor := range browser.NewActions(runner, dependencies) {
		if executor.Action().Name == name {
			return executor
		}
	}
	panic("unknown browser action fixture")
}
