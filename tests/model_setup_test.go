package browser_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/actions"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

func TestModelSetupDiscoveryNeverReadsModelEnvironmentOrStartsSetup(t *testing.T) {
	env := newModelSetupEnvironment(t)
	questions, secrets := &modelSetupQuestions{}, &browseSecretProbe{}
	executor := actions.NewBrowse(&browseUnusedRunner{})
	ctx := action.WithQuestions(action.WithSecrets(t.Context(), secrets), questions)
	status, err := executor.Discover(ctx, env, nil)
	if err != nil || status != action.Ready || len(executor.Action().RequiredSecrets) != 0 || secrets.requests != 0 || len(questions.calls) != 0 {
		t.Fatalf("discovery opened setup or required credentials: status=%s error=%v", status, err)
	}
	assertModelEnvironmentIgnored(t, env)
}

func TestModelSetupAsksProviderThenExactModelThenManagedKey(t *testing.T) {
	for _, test := range []struct{ name, provider, more, keyName string }{
		{name: "OpenAI", provider: "OpenAI", keyName: "OPENAI_API_KEY"},
		{name: "Anthropic", provider: "Anthropic", keyName: "ANTHROPIC_API_KEY"},
		{name: "Gemini", provider: "Google Gemini", keyName: "GEMINI_API_KEY"},
		{name: "xAI", provider: "More providers", more: "xAI", keyName: "XAI_API_KEY"},
		{name: "DeepSeek", provider: "More providers", more: "DeepSeek", keyName: "DEEPSEEK_API_KEY"},
		{name: "freeform provider", provider: "openai", keyName: "OPENAI_API_KEY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newModelSetupEnvironment(t)
			const key = "fixture-managed-selection-key-92ac421f"
			runner := &browseUnusedRunner{err: errors.New("fixture runner failure: " + key)}
			executor := actions.NewBrowse(runner)
			discoverBrowse(t, executor, env)
			secrets := &browseSecretProbe{values: map[string][]byte{test.keyName: []byte(key)}}
			questions := &modelSetupQuestions{answers: map[string]string{"browser_model_provider": test.provider, "browser_model_provider_more": test.more, "browser_model_name": "fixture-model-v1"}}
			questions.onQuestion = func() {
				if secrets.requests != 0 {
					t.Error("credential requested before selection completed")
				}
			}
			_, err := executeModelSetup(t, executor, t.Context(), newModelSetupBinding(), questions, secrets, 0)
			if err == nil || runner.calls != 1 || !slices.Equal(secrets.names, []string{test.keyName}) || strings.Contains(err.Error(), key) {
				t.Fatalf("provider did not request its managed key safely: error=%v browser_calls=%d requests=%v", err, runner.calls, secrets.names)
			}
			want := []string{"browser_model_provider", "browser_model_name"}
			if test.more != "" {
				want = []string{"browser_model_provider", "browser_model_provider_more", "browser_model_name"}
			}
			if !slices.Equal(questions.keys(), want) || !slices.Equal(questions.calls[0].options, []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}) {
				t.Fatalf("provider/model question sequence changed: %v", questions.keys())
			}
			if test.more != "" && !slices.Equal(questions.calls[1].options, []string{"xAI", "DeepSeek", "Other (OpenAI-compatible)"}) {
				t.Fatal("additional provider menu changed")
			}
			if len(questions.calls[len(questions.calls)-1].options) != 0 {
				t.Fatal("exact model ID question offered a stale model list")
			}
			for _, returned := range secrets.returned {
				for _, value := range returned {
					if value != 0 {
						t.Fatal("framework-owned returned secret buffer was retained")
					}
				}
			}
			assertModelEnvironmentIgnored(t, env)
		})
	}
}

func TestModelSetupCustomEndpointAlwaysAsksAndBindsItsManagedCredential(t *testing.T) {
	const endpoint = "https://custom-model.example.test/v1/chat/completions"
	if actions.ModelSecretName(endpoint) != modelSetupSecretName(endpoint) || actions.ModelSecretName(endpoint) == actions.ModelSecretName(endpoint+"/other") {
		t.Fatal("custom credential name is not bound to exact endpoint")
	}
	env := newModelSetupEnvironment(t)
	const key = "fixture-custom-managed-key-fbbd1610"
	runner := &browseUnusedRunner{err: errors.New("fixture browser: " + key)}
	executor := actions.NewBrowse(runner)
	discoverBrowse(t, executor, env)
	questions := &modelSetupQuestions{answers: map[string]string{"browser_model_provider": "More providers", "browser_model_provider_more": "Other (OpenAI-compatible)", "browser_model_name": "fixture-model-v1", "browser_model_url": endpoint}}
	secrets := &browseSecretProbe{values: map[string][]byte{modelSetupSecretName(endpoint): []byte(key)}}
	_, err := executeModelSetup(t, executor, t.Context(), newModelSetupBinding(), questions, secrets, 0)
	if err == nil || runner.calls != 1 || !slices.Equal(secrets.names, []string{modelSetupSecretName(endpoint)}) || strings.Contains(err.Error(), key) {
		t.Fatal("custom endpoint did not use its managed credential safely")
	}
	if !slices.Equal(questions.keys(), []string{"browser_model_provider", "browser_model_provider_more", "browser_model_name", "browser_model_url"}) {
		t.Fatalf("custom setup order changed: %v", questions.keys())
	}
	assertModelEnvironmentIgnored(t, env)
}

func TestModelSetupRetainsOnlySessionSelectionAndRequestsCurrentManagedKey(t *testing.T) {
	env := newModelSetupEnvironment(t)
	const first = "fixture-first-managed-key-6c44f193"
	runner := &browseUnusedRunner{err: errors.New(first)}
	executor := actions.NewBrowse(runner)
	discoverBrowse(t, executor, env)
	binding := newModelSetupBinding()
	questions := openAISetupQuestions()
	secrets := &browseSecretProbe{values: map[string][]byte{"OPENAI_API_KEY": []byte(first)}}
	_, err := executeModelSetup(t, executor, t.Context(), binding, questions, secrets, 0)
	if err == nil || runner.calls != 1 || len(questions.calls) != 2 {
		t.Fatalf("initial selection did not reach browser work: %v", err)
	}
	binding.Mode = sessions.Reuse
	const rotated = "fixture-rotated-managed-key-579d032f"
	secrets.values["OPENAI_API_KEY"] = []byte(rotated)
	runner.err = errors.New(rotated)
	questions = &modelSetupQuestions{}
	_, err = executeModelSetup(t, executor, t.Context(), binding, questions, secrets, 0)
	if err == nil || runner.calls != 2 || len(questions.calls) != 0 || !slices.Equal(secrets.names, []string{"OPENAI_API_KEY", "OPENAI_API_KEY"}) || strings.Contains(err.Error(), rotated) {
		t.Fatal("session selection was re-asked or API key was cached")
	}
	conflicting := binding
	conflicting.Creation.OperationID = id.OperationID()
	_, err = executeModelSetup(t, executor, t.Context(), conflicting, questions, secrets, 0)
	if !errors.Is(err, sessions.ErrConflict) || runner.calls != 2 || secrets.requests != 2 || len(questions.calls) != 0 {
		t.Fatal("changed creation claim reused retained selection")
	}
	replacement := newModelSetupBinding()
	replacement.ScopeID = binding.ScopeID
	questions = openAISetupQuestions()
	_, err = executeModelSetup(t, executor, t.Context(), replacement, questions, secrets, 0)
	if err == nil || runner.calls != 3 || len(questions.calls) != 2 || secrets.requests != 3 {
		t.Fatal("different session identity inherited another selection")
	}
	assertModelEnvironmentIgnored(t, env)
}

func TestModelSetupCancellationAndBudgetsPrecedeBrowserExecution(t *testing.T) {
	for _, stage := range []string{"browser_model_provider", "browser_model_name", "credential"} {
		t.Run(stage, func(t *testing.T) {
			env := newModelSetupEnvironment(t)
			runner := &browseUnusedRunner{}
			executor := actions.NewBrowse(runner)
			discoverBrowse(t, executor, env)
			questions := openAISetupQuestions()
			questions.block, questions.entered = stage, make(chan string, 4)
			secrets := &browseSecretProbe{wait: true, entered: make(chan string, 1)}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := executeModelSetup(t, executor, ctx, newModelSetupBinding(), questions, secrets, 0)
				done <- err
			}()
			entered, expected := questions.entered, stage
			if stage == "credential" {
				entered, expected = secrets.entered, "OPENAI_API_KEY"
			}
			waiting := time.NewTimer(3 * time.Second)
			defer waiting.Stop()
			for {
				select {
				case key := <-entered:
					if key != expected {
						continue
					}
				case <-waiting.C:
					t.Fatal("setup did not enter requested waiting stage")
				}
				break
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || runner.calls != 0 {
					t.Fatalf("setup cancellation did not stop browser work: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancelled setup did not return")
			}
			assertModelEnvironmentIgnored(t, env)
		})
	}
	t.Run("browser budget starts after setup", func(t *testing.T) {
		env := newModelSetupEnvironment(t)
		runner := &modelSetupBudgetRunner{}
		executor := actions.NewBrowse(runner)
		discoverBrowse(t, executor, env)
		questions := openAISetupQuestions()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		_, err := executeModelSetup(t, executor, ctx, newModelSetupBinding(), questions, &browseSecretProbe{value: []byte("fixture-budget-managed-key-9b97771d")}, 5)
		if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || len(questions.calls) != 2 || questions.calls[0].remaining < 20*time.Second || questions.calls[0].remaining > 30*time.Second || runner.elapsed < 4*time.Second || runner.elapsed > 10*time.Second {
			t.Fatal("operator setup consumed browser budget or escaped parent deadline")
		}
	})
	t.Run("parent cancellation cause stays private", func(t *testing.T) {
		const key = "fixture-cancel-cause-key-88dc74b3"
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(context.Canceled)
		runner := &modelSetupBudgetRunner{onRun: func() { cancel(errors.New("private model credential: " + key)) }}
		executor := actions.NewBrowse(runner)
		discoverBrowse(t, executor, newModelSetupEnvironment(t))
		result, err := executeModelSetup(t, executor, ctx, newModelSetupBinding(), openAISetupQuestions(), &browseSecretProbe{value: []byte(key)}, 5)
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), key) || len(result.Output) != 0 || len(result.Artifacts) != 0 {
			t.Fatal("parent cancellation leaked its private cause or lost cancellation classification")
		}
	})
}

type modelSetupBudgetRunner struct {
	elapsed time.Duration
	onRun   func()
}

func (runner *modelSetupBudgetRunner) Run(ctx context.Context, _ browserruntime.Options, _ func(context.Context) error) (sessions.Session, error) {
	started := time.Now()
	if runner.onRun != nil {
		runner.onRun()
	}
	<-ctx.Done()
	runner.elapsed = time.Since(started)
	return sessions.Session{}, ctx.Err()
}

type modelSetupQuestionCall struct {
	key, text string
	options   []string
	remaining time.Duration
}
type modelSetupQuestions struct {
	answers    map[string]string
	calls      []modelSetupQuestionCall
	block      string
	entered    chan string
	onQuestion func()
}

func (probe *modelSetupQuestions) Question(ctx context.Context, key, text string, options []string) (operations.Question, error) {
	call := modelSetupQuestionCall{key: key, text: text, options: slices.Clone(options)}
	if deadline, ok := ctx.Deadline(); ok {
		call.remaining = time.Until(deadline)
	}
	probe.calls = append(probe.calls, call)
	if probe.onQuestion != nil {
		probe.onQuestion()
	}
	if probe.entered != nil {
		probe.entered <- key
	}
	if key == probe.block {
		<-ctx.Done()
		return operations.Question{}, ctx.Err()
	}
	answer := probe.answers[key]
	if answer == "" {
		return operations.Question{}, errors.New("unexpected model setup question")
	}
	now := time.Now().UTC()
	return operations.Question{ID: id.New(), Status: operations.QuestionAnswered, Key: key, Text: text, Options: slices.Clone(options), Answer: answer, AnsweredBy: operations.AnswerOperator, AskedAt: now.Add(-time.Second), AnsweredAt: &now, FallbackAt: now.Add(time.Minute)}, nil
}

func (probe *modelSetupQuestions) keys() []string {
	var keys []string
	for _, call := range probe.calls {
		keys = append(keys, call.key)
	}
	return keys
}

type modelSetupEnvironment struct {
	*lightpandaEnvironment
	lookups []string
}

func (env *modelSetupEnvironment) LookupEnv(name string) (string, bool) {
	env.lookups = append(env.lookups, name)
	return env.lightpandaEnvironment.LookupEnv(name)
}
func newModelSetupEnvironment(t *testing.T) *modelSetupEnvironment {
	t.Helper()
	env := browseDiscoveryEnvironment(t)
	for _, name := range []string{"BROWSER_MODEL_PROVIDER", "BROWSER_MODEL_NAME", "BROWSER_MODEL_URL", "BROWSER_MODEL_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY", "DEEPSEEK_API_KEY"} {
		env.values[name] = "private-ignored-model-environment-sentinel"
	}
	return &modelSetupEnvironment{lightpandaEnvironment: env}
}
func assertModelEnvironmentIgnored(t *testing.T, env *modelSetupEnvironment) {
	t.Helper()
	for _, lookup := range env.lookups {
		if strings.HasPrefix(lookup, "BROWSER_MODEL_") || strings.HasSuffix(lookup, "API_KEY") {
			t.Fatalf("model setup looked up removed environment setting %q", lookup)
		}
	}
}
func openAISetupQuestions() *modelSetupQuestions {
	return &modelSetupQuestions{answers: map[string]string{"browser_model_provider": "OpenAI", "browser_model_name": "fixture-model-v1"}}
}

func newModelSetupBinding() sessions.Binding {
	task := id.TaskID()
	return sessions.Binding{Reference: sessions.Reference{ID: id.New(), OwnerAgentID: "browser-agent-0123abcd", ScopeID: id.New()}, Mode: sessions.Create, Creation: sessions.ExecutionRef{OperationID: id.OperationID(), TaskID: task, CommandID: task}}
}

func modelSetupInput(binding sessions.Binding) map[string]any {
	input := validBrowseInput()
	input["session_scope"] = binding.ScopeID
	if binding.Mode == sessions.Reuse {
		input["session_id"] = binding.ID
	}
	return input
}

func executeModelSetup(t *testing.T, executor *actions.Browse, ctx context.Context, binding sessions.Binding, questions *modelSetupQuestions, secrets *browseSecretProbe, timeout int) (action.Result, error) {
	t.Helper()
	input := modelSetupInput(binding)
	if timeout != 0 {
		input["timeout_seconds"] = timeout
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ctx = action.WithSession(action.WithSecrets(ctx, secrets), &binding)
	if questions != nil {
		ctx = action.WithQuestions(ctx, questions)
	}
	return executor.Run(ctx, raw)
}

func modelSetupSecretName(address string) string {
	digest := sha256.Sum256([]byte(address))
	return "browser-model-api-key-" + hex.EncodeToString(digest[:])
}
