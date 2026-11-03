package browser_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

func TestLoginDiscoveryRequiresOnlyLightpandaAndClearsStaleReadiness(t *testing.T) {
	env := newModelSetupEnvironment(t)
	secrets, questions := &browseSecretProbe{}, &modelSetupQuestions{}
	executor := actions.NewLogin(&browseUnusedRunner{})
	ctx := action.WithQuestions(action.WithSecrets(t.Context(), secrets), questions)
	status, err := executor.Discover(ctx, env, nil)
	if err != nil || status != action.Ready || len(executor.Action().RequiredSecrets) != 0 || executor.Action().SessionMode != "create" || secrets.requests != 0 || len(questions.calls) != 0 {
		t.Fatal("login discovery required credentials, model selection or browser work")
	}
	assertModelEnvironmentIgnored(t, env)
	status, err = executor.Discover(ctx, env, &environment.DiscoveryScope{Paths: []string{}})
	if err != nil || status != action.Unavailable || executor.Action().Status != action.Unavailable {
		t.Fatal("restricted rediscovery retained login readiness")
	}
	if _, err := executor.Run(ctx, json.RawMessage(`{}`)); err == nil || secrets.requests != 0 || len(questions.calls) != 0 {
		t.Fatal("unavailable login opened questions or credentials")
	}
}

func TestLoginRejectsInvalidInputBeforeQuestionsCredentialsAndBrowser(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{name: "missing verification URL", change: func(input map[string]any) { delete(input, "verification_url") }},
		{name: "missing expected marker", change: func(input map[string]any) { delete(input, "expected_text") }},
		{name: "blank marker", change: func(input map[string]any) { input["expected_text"] = " \n " }},
		{name: "long marker", change: func(input map[string]any) { input["expected_text"] = strings.Repeat("x", 257) }},
		{name: "insecure remote login", change: func(input map[string]any) {
			input["url"] = "http://account.example.test/login"
			input["verification_url"] = "http://account.example.test/account"
		}},
		{name: "URL credentials", change: func(input map[string]any) {
			input["url"] = "https://private-login-sentinel:password@account.example.test/login"
		}},
		{name: "foreign verification host", change: func(input map[string]any) { input["verification_url"] = "https://other.example.test/account" }},
		{name: "foreign verification port", change: func(input map[string]any) { input["verification_url"] = "https://account.example.test:8443/account" }},
		{name: "invalid profile", change: func(input map[string]any) { input["credential_profile"] = "private-login-sentinel@example.test" }},
		{name: "long profile", change: func(input map[string]any) { input["credential_profile"] = strings.Repeat("a", 33) }},
		{name: "secret name override", change: func(input map[string]any) { input["password_secret"] = "private-login-sentinel" }},
		{name: "engine override", change: func(input map[string]any) { input["engine"] = "lightpanda" }},
		{name: "invalid session scope", change: func(input map[string]any) { input["session_scope"] = "not-a-session-scope" }},
		{name: "unexpected create identity", change: func(input map[string]any) { input["session_id"] = newModelSetupBinding().ID }},
		{name: "timeout minimum", change: func(input map[string]any) { input["timeout_seconds"] = 4 }},
		{name: "timeout maximum", change: func(input map[string]any) { input["timeout_seconds"] = 181 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &browseUnusedRunner{}
			executor := actions.NewLogin(runner)
			discoverLogin(t, executor)
			binding := newModelSetupBinding()
			input := loginInput(binding, "https://account.example.test", "/login", "work")
			test.change(input)
			secrets, questions := &browseSecretProbe{}, &modelSetupQuestions{}
			ctx := action.WithLogs(action.WithQuestions(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), questions), &loginLogProbe{})
			_, err := executor.Run(ctx, marshalLoginInput(t, input))
			if err == nil || runner.calls != 0 || secrets.requests != 0 || len(questions.calls) != 0 || strings.Contains(err.Error(), "private-login-sentinel") {
				t.Fatal("invalid login input reached questions, credentials or browser work, or exposed input")
			}
		})
	}
}

func TestLoginAsksOnlyForMissingNonSecretProfile(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		t.Run(map[bool]string{false: "question", true: "provided"}[supplied], func(t *testing.T) {
			runner := &browseUnusedRunner{}
			executor := actions.NewLogin(runner)
			discoverLogin(t, executor)
			binding := newModelSetupBinding()
			input := loginInput(binding, "https://account.example.test", "/login", "work")
			if !supplied {
				delete(input, "credential_profile")
			}
			secrets := &browseSecretProbe{}
			questions := &modelSetupQuestions{answers: map[string]string{"browser_login_profile": "work"}}
			ctx := action.WithLogs(action.WithQuestions(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), questions), &loginLogProbe{})
			_, err := executor.Run(ctx, marshalLoginInput(t, input))
			wantQuestions := []string(nil)
			if !supplied {
				wantQuestions = []string{"browser_login_profile"}
			}
			if err == nil || runner.calls != 1 || secrets.requests != 0 || !slices.Equal(questions.keys(), wantQuestions) {
				t.Fatal("login used model setup or requested credentials before examining a real form")
			}
		})
	}
}

func discoverLogin(t *testing.T, executor *actions.Login) {
	t.Helper()
	status, err := executor.Discover(t.Context(), browseDiscoveryEnvironment(t), nil)
	if err != nil || status != action.Ready {
		t.Fatalf("discover login: status=%s error=%v", status, err)
	}
}

func loginInput(binding sessions.Binding, origin, path, profile string) map[string]any {
	input := map[string]any{"url": origin + path, "verification_url": origin + "/account", "expected_text": "Account verified", "credential_profile": profile, "session_scope": binding.ScopeID}
	if binding.Mode == sessions.Reuse {
		input["session_id"] = binding.ID
	}
	return input
}

func marshalLoginInput(t *testing.T, input map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func loginSecretNames(origin, profile string) (string, string) {
	digest := sha256.Sum256([]byte(origin + "\x00" + profile))
	base := "browser-login-" + profile + "-" + hex.EncodeToString(digest[:])
	return base + "-username", base + "-password"
}

type loginLogProbe struct{ messages []string }

func (probe *loginLogProbe) Log(_ context.Context, message string) error {
	probe.messages = append(probe.messages, message)
	return nil
}
