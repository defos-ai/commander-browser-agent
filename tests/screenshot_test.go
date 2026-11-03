package browser_test

import (
	"context"
	"errors"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

func TestScreenshotDiscoveryRequiresOnlyLightpanda(t *testing.T) {
	env := newModelSetupEnvironment(t)
	runner := &browseUnusedRunner{}
	executor := actions.NewScreenshot(runner)
	questions, secrets := &modelSetupQuestions{}, &browseSecretProbe{}
	ctx := action.WithQuestions(action.WithSecrets(t.Context(), secrets), questions)
	status, err := executor.Discover(ctx, env, nil)
	if err != nil || status != action.Ready || executor.Action().SessionMode != "use" || len(executor.Action().RequiredSecrets) != 0 || runner.calls != 0 || secrets.requests != 0 || len(questions.calls) != 0 {
		t.Fatal("screenshot discovery opened a browser, questions, model settings or credentials")
	}
	assertModelEnvironmentIgnored(t, env)
	status, err = executor.Discover(ctx, env, &environment.DiscoveryScope{Paths: []string{}})
	if err != nil || status != action.Unavailable || executor.Action().Status != action.Unavailable {
		t.Fatal("restricted rediscovery retained screenshot readiness")
	}
}

func TestScreenshotRejectsInvalidInputAndBindingsBeforeBrowserAccess(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]any, *sessions.Binding)
	}{
		{name: "missing identity", change: func(input map[string]any, _ *sessions.Binding) { delete(input, "session_id") }},
		{name: "missing scope", change: func(input map[string]any, _ *sessions.Binding) { delete(input, "session_scope") }},
		{name: "wrong scope", change: func(input map[string]any, _ *sessions.Binding) {
			input["session_scope"] = newModelSetupBinding().ScopeID
		}},
		{name: "wrong identity", change: func(input map[string]any, _ *sessions.Binding) { input["session_id"] = newModelSetupBinding().ID }},
		{name: "create binding", change: func(_ map[string]any, binding *sessions.Binding) { binding.Mode = sessions.Create }},
		{name: "reuse binding", change: func(_ map[string]any, binding *sessions.Binding) { binding.Mode = sessions.Reuse }},
		{name: "URL input", change: func(input map[string]any, _ *sessions.Binding) {
			input["url"] = "https://example.test/private-screenshot-sentinel"
		}},
		{name: "engine input", change: func(input map[string]any, _ *sessions.Binding) { input["engine"] = "lightpanda" }},
		{name: "model input", change: func(input map[string]any, _ *sessions.Binding) { input["model"] = "private-screenshot-sentinel" }},
		{name: "secret input", change: func(input map[string]any, _ *sessions.Binding) { input["password"] = "private-screenshot-sentinel" }},
		{name: "unknown viewport", change: func(input map[string]any, _ *sessions.Binding) { input["viewport"] = "tablet" }},
		{name: "timeout minimum", change: func(input map[string]any, _ *sessions.Binding) { input["timeout_seconds"] = 4 }},
		{name: "timeout maximum", change: func(input map[string]any, _ *sessions.Binding) { input["timeout_seconds"] = 61 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &browseUnusedRunner{}
			executor := actions.NewScreenshot(runner)
			discoverScreenshot(t, executor)
			binding := newModelSetupBinding()
			binding.Mode = sessions.Use
			input := screenshotInput(binding)
			test.change(input, &binding)
			questions, secrets := &modelSetupQuestions{}, &browseSecretProbe{}
			ctx := action.WithLogs(action.WithQuestions(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), questions), &loginLogProbe{})
			_, err := executor.Run(ctx, marshalLoginInput(t, input))
			if err == nil || runner.calls != 0 || secrets.requests != 0 || len(questions.calls) != 0 {
				t.Fatal("invalid screenshot input reached browser work, questions or credentials")
			}
		})
	}
}

func TestScreenshotCancellationPreventsBrowserWork(t *testing.T) {
	runner := &browseUnusedRunner{}
	executor := actions.NewScreenshot(runner)
	discoverScreenshot(t, executor)
	binding := newModelSetupBinding()
	binding.Mode = sessions.Use
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := executor.Run(action.WithSession(ctx, &binding), marshalLoginInput(t, screenshotInput(binding)))
	if !errors.Is(err, context.Canceled) || runner.calls != 0 {
		t.Fatal("cancelled screenshot entered the browser")
	}
}

func discoverScreenshot(t *testing.T, executor *actions.Screenshot) {
	t.Helper()
	status, err := executor.Discover(t.Context(), browseDiscoveryEnvironment(t), nil)
	if err != nil || status != action.Ready {
		t.Fatal("screenshot discovery did not become ready")
	}
}

func screenshotInput(binding sessions.Binding) map[string]any {
	return map[string]any{"session_scope": binding.ScopeID, "session_id": binding.ID}
}
