package browser_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	browser "github.com/defos-ai/commander-browser-agent"
	"github.com/defos-ai/commander-browser-agent/actions"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
	fsartifacts "github.com/defos-ai/commander/providers/fs/artifacts"
)

func TestBrowseCatalogKeepsDiscoveryPerHostAndPreservesEmptySelection(t *testing.T) {
	runner := &browseUnusedRunner{}
	first, second := browser.NewActions(runner), browser.NewActions(runner)
	env := browseDiscoveryEnvironment(t)
	if _, err := first[0].Discover(t.Context(), env, nil); err != nil {
		t.Fatal(err)
	}
	if first[0].Action().Status != action.Ready || second[0].Action().Status != action.Unknown {
		t.Fatal("catalog instances share discovery readiness")
	}
	store, err := fsartifacts.NewArtifactFileFsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, err := browser.NewAgent(agent.AgentConfig{Actions: []action.Executor{}, ArtifactFileStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if len(host.Describe().Actions) != 0 {
		t.Fatal("an explicitly empty action selection gained default actions")
	}
	if len(first[0].Action().RequiredSecrets) != 0 {
		t.Fatal("on-demand model credentials block startup readiness")
	}
}

func TestBrowseDefaultCatalogRequiresItsHostBrowserRunner(t *testing.T) {
	store, err := fsartifacts.NewArtifactFileFsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := browseInventoryOnlyProvider{}
	if _, err := browser.NewAgent(agent.AgentConfig{Sessions: provider, ArtifactFileStore: store}); err == nil || !strings.Contains(err.Error(), "browser session runner") {
		t.Fatalf("default catalog accepted an inventory-only session provider: %v", err)
	}
	if _, err := browser.NewAgent(agent.AgentConfig{Sessions: provider, Actions: []action.Executor{}, ArtifactFileStore: store}); err != nil {
		t.Fatalf("explicitly empty catalog unnecessarily requires a browser runner: %v", err)
	}
}

func TestBrowseRejectsInvalidInputBeforeRequestingCredentials(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "implicit engine", change: func(input map[string]any) { delete(input, "engine") }},
		{name: "unsupported engine", change: func(input map[string]any) { input["engine"] = "chromium" }},
		{name: "invalid session scope", change: func(input map[string]any) { input["session_scope"] = "not-an-id" }},
		{name: "invalid session identity", change: func(input map[string]any) { input["session_id"] = "not-an-id" }},
		{name: "unknown field", change: func(input map[string]any) { input["api_key"] = "private-input-sentinel" }},
		{name: "blank objective", change: func(input map[string]any) { input["objective"] = "  " }},
		{name: "long objective", change: func(input map[string]any) { input["objective"] = strings.Repeat("x", 4001) }},
		{name: "URL credentials", change: func(input map[string]any) { input["url"] = "https://user:private-input-sentinel@example.test/" }},
		{name: "invalid query escaping", change: func(input map[string]any) { input["url"] = "https://example.test/?query=%zz" }},
		{name: "scope does not include URL", change: func(input map[string]any) { input["allowed_hosts"] = []string{"other.example.test"} }},
		{name: "host includes a port", change: func(input map[string]any) { input["allowed_hosts"] = []string{"example.test:443"} }},
		{name: "step minimum", change: func(input map[string]any) { input["max_steps"] = 0 }},
		{name: "step maximum", change: func(input map[string]any) { input["max_steps"] = 31 }},
		{name: "timeout minimum", change: func(input map[string]any) { input["timeout_seconds"] = 4 }},
		{name: "timeout maximum", change: func(input map[string]any) { input["timeout_seconds"] = 181 }},
		{name: "token minimum", change: func(input map[string]any) { input["max_completion_tokens"] = 1023 }},
		{name: "token maximum", change: func(input map[string]any) { input["max_completion_tokens"] = 32769 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &browseUnusedRunner{}
			executor := actions.NewBrowse(runner)
			discoverBrowse(t, executor, browseDiscoveryEnvironment(t))
			secrets := &browseSecretProbe{}
			input := validBrowseInput()
			test.change(input)
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			_, err = executor.Run(action.WithSecrets(t.Context(), secrets), raw)
			if err == nil || secrets.requests != 0 || runner.calls != 0 {
				t.Fatalf("invalid input crossed the credential/browser boundary: error=%v secret_requests=%d browser_calls=%d", err, secrets.requests, runner.calls)
			}
			if strings.Contains(err.Error(), "private-input-sentinel") {
				t.Fatal("validation error exposed private input")
			}
		})
	}
	t.Run("valid defaults reach managed credential request", func(t *testing.T) {
		runner := &browseUnusedRunner{}
		executor := actions.NewBrowse(runner)
		discoverBrowse(t, executor, browseDiscoveryEnvironment(t))
		secrets := &browseSecretProbe{}
		raw, err := json.Marshal(validBrowseInput())
		if err != nil {
			t.Fatal(err)
		}
		_, err = executor.Run(action.WithSecrets(t.Context(), secrets), raw)
		if !errors.Is(err, action.ErrSecretUnavailable) || secrets.requests != 1 || secrets.name != actions.BrowserModelSecret || runner.calls != 0 {
			t.Fatalf("valid work did not request managed model credentials before opening a browser: error=%v requests=%d name=%q browser_calls=%d", err, secrets.requests, secrets.name, runner.calls)
		}
	})
}

func TestBrowseDiscoveryRequiresSafeExplicitModelConfiguration(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*lightpandaEnvironment)
		missing   string
	}{
		{name: "missing model endpoint", configure: func(env *lightpandaEnvironment) { delete(env.values, "BROWSER_MODEL_URL") }, missing: "BROWSER_MODEL_URL"},
		{name: "missing model name", configure: func(env *lightpandaEnvironment) { delete(env.values, "BROWSER_MODEL_NAME") }, missing: "BROWSER_MODEL_NAME"},
		{name: "endpoint credentials", configure: func(env *lightpandaEnvironment) {
			env.values["BROWSER_MODEL_URL"] = "https://user:private-config-sentinel@model.example.test/v1/chat/completions"
		}, missing: "BROWSER_MODEL_URL"},
		{name: "nonlocal insecure model", configure: func(env *lightpandaEnvironment) {
			env.values["BROWSER_MODEL_URL"] = "http://model.example.test/v1/chat/completions"
		}, missing: "BROWSER_MODEL_URL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &browseUnusedRunner{}
			executor := actions.NewBrowse(runner)
			env := browseDiscoveryEnvironment(t)
			test.configure(env)
			secrets := &browseSecretProbe{}
			status, err := executor.Discover(action.WithSecrets(t.Context(), secrets), env, nil)
			snapshot := executor.Action()
			want := actionmodel.Requirement{Kind: actionmodel.ConfigurationRequirement, Name: test.missing}
			if err != nil || status != action.Unavailable || snapshot.Status != action.Unavailable || snapshot.StatusReason == "" || !slices.Contains(snapshot.MissingRequirements, want) {
				t.Fatalf("invalid model configuration did not report its readiness requirement: action=%+v err=%v", snapshot, err)
			}
			if strings.Contains(snapshot.StatusReason, "private-config-sentinel") || secrets.requests != 0 || runner.calls != 0 {
				t.Fatal("discovery exposed configuration or opened execution dependencies")
			}
		})
	}
}

func TestBrowseRejectsModelCredentialInStartingURL(t *testing.T) {
	const credential = "fixture-managed-secret-sentinel"
	for _, test := range []struct{ name, url string }{
		{name: "literal", url: "https://example.test/" + credential},
		{name: "encoded path", url: "https://example.test/%66ixture-managed-secret-sentinel"},
		{name: "encoded query", url: "https://example.test/?query=%66ixture-managed-secret-sentinel"},
		{name: "encoded fragment", url: "https://example.test/#%66ixture-managed-secret-sentinel"},
		{name: "nested encoding with literal percent", url: "https://example.test/%2566ixture-managed-secret-sentinel%25"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &browseUnusedRunner{}
			executor := actions.NewBrowse(runner)
			discoverBrowse(t, executor, browseDiscoveryEnvironment(t))
			secrets := &browseSecretProbe{value: []byte(credential)}
			input := validBrowseInput()
			input["url"] = test.url
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Run(action.WithSecrets(t.Context(), secrets), raw)
			if err == nil || secrets.requests != 1 || runner.calls != 0 {
				t.Fatalf("protected model credentials reached browser work: error=%v requests=%d browser_calls=%d", err, secrets.requests, runner.calls)
			}
			if strings.Contains(err.Error(), credential) || len(result.Artifacts) != 0 || len(result.Output) != 0 {
				t.Fatal("rejected starting URL exposed model credentials")
			}
		})
	}
}

func TestBrowseRediscoveryClearsReadinessAndRespectsExecutableScope(t *testing.T) {
	runner := &browseUnusedRunner{}
	executor := actions.NewBrowse(runner)
	env := browseDiscoveryEnvironment(t)
	discoverBrowse(t, executor, env)
	status, err := executor.Discover(t.Context(), env, &environment.DiscoveryScope{Paths: []string{}})
	if err != nil || status != action.Unavailable || executor.Action().Status != action.Unavailable {
		t.Fatalf("restricted executable scope retained readiness: status=%s err=%v", status, err)
	}
	secrets := &browseSecretProbe{}
	raw, err := json.Marshal(validBrowseInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Run(action.WithSecrets(t.Context(), secrets), raw); err == nil || secrets.requests != 0 {
		t.Fatal("restricted rediscovery retained an executable configuration")
	}
	discoverBrowse(t, executor, env)
	delete(env.values, "BROWSER_MODEL_URL")
	status, err = executor.Discover(t.Context(), env, nil)
	if err != nil || status != action.Unavailable {
		t.Fatalf("missing model configuration retained readiness: %s, %v", status, err)
	}
	if _, err := executor.Run(action.WithSecrets(t.Context(), secrets), raw); err == nil || secrets.requests != 0 {
		t.Fatal("failed model rediscovery retained old configuration")
	}
	env.values["BROWSER_MODEL_URL"] = "https://model.example.test/v1/chat/completions"
	discoverBrowse(t, executor, env)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	status, err = executor.Discover(ctx, env, nil)
	if !errors.Is(err, context.Canceled) || status != action.Unknown || executor.Action().Status != action.Unknown {
		t.Fatalf("cancelled rediscovery retained readiness: status=%s err=%v", status, err)
	}
}

func browseDiscoveryEnvironment(t *testing.T) *lightpandaEnvironment {
	t.Helper()
	env, executable := newLightpandaEnvironment(t)
	env.values["LIGHTPANDA_PATH"] = executable
	env.values["BROWSER_MODEL_URL"] = "https://model.example.test/v1/chat/completions"
	env.values["BROWSER_MODEL_NAME"] = "fixture-model"
	return env
}

func discoverBrowse(t *testing.T, executor *actions.Browse, env *lightpandaEnvironment) {
	t.Helper()
	status, err := executor.Discover(t.Context(), env, nil)
	if err != nil || status != action.Ready {
		t.Fatalf("discover configured browse action: %s, %v", status, err)
	}
}

func validBrowseInput() map[string]any {
	return map[string]any{"objective": "Extract the study names", "url": "https://example.test/", "engine": "lightpanda", "session_scope": id.New()}
}

type browseUnusedRunner struct{ calls int }

func (runner *browseUnusedRunner) Run(context.Context, browserruntime.Options, func(context.Context) error) (sessions.Session, error) {
	runner.calls++
	return sessions.Session{}, errors.New("unexpected browser use")
}

type browseSecretProbe struct {
	requests int
	name     string
	value    []byte
}

type browseInventoryOnlyProvider struct{}

func (browseInventoryOnlyProvider) Prepare(context.Context, sessions.CreationClaim) error { return nil }
func (browseInventoryOnlyProvider) Snapshot() []sessions.Session                          { return nil }
func (browseInventoryOnlyProvider) Changes() <-chan struct{}                              { return nil }
func (browseInventoryOnlyProvider) Control(context.Context, sessions.Control) (sessions.Session, error) {
	return sessions.Session{}, errors.New("unused inventory-only control")
}

func (*browseSecretProbe) Secret(string) ([]byte, error) { return nil, action.ErrSecretUnavailable }
func (probe *browseSecretProbe) RequestSecret(_ context.Context, name string) ([]byte, error) {
	probe.requests++
	probe.name = name
	if probe.value != nil {
		return slices.Clone(probe.value), nil
	}
	return nil, action.ErrSecretUnavailable
}
