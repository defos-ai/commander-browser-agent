package browser_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
)

func TestAgentRediscoveryClearsAnUnavailableBrowser(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	probe := &unitSetup{t: t}
	executor := actions.NewBrowse(probe)
	ready := unitEnvironment{probe}
	if status, err := executor.Discover(t.Context(), ready, nil); err != nil || status != action.Ready {
		t.Fatal("installed browser was not ready")
	}
	for _, missing := range []string{"hermes", "lightpanda"} {
		probe.unavailableHermes, probe.unavailableLightpanda = missing == "hermes", missing == "lightpanda"
		if status, err := executor.Discover(t.Context(), ready, nil); err != nil || status != action.Unavailable {
			t.Fatal("rediscovery retained an unavailable runtime")
		}
		description, _ := json.Marshal(executor.Action())
		_, err := executor.Run(t.Context(), json.RawMessage(`{}`))
		if err == nil || len(probe.events) != 0 || probe.runs != 0 || strings.Contains(string(description), "private-runtime-output") {
			t.Fatal("unavailable runtime reached execution or exposed subprocess output")
		}
		probe.unavailableHermes, probe.unavailableLightpanda = false, false
		if status, err := executor.Discover(t.Context(), ready, nil); err != nil || status != action.Ready {
			t.Fatal("browser readiness did not recover after installation was corrected")
		}
	}
}

func TestActionsRespectExecutableDiscoveryScope(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	probe := &unitSetup{t: t}
	for _, executor := range []action.Executor{actions.NewBrowse(probe), actions.NewLogin(probe), actions.NewScreenshot(probe)} {
		ready := unitEnvironment{probe}
		if status, err := executor.Discover(t.Context(), ready, nil); err != nil || status != action.Ready {
			t.Fatal("the packaged runtime was not ready before scope restriction")
		}
		before := probe.discoveryRuns
		if status, err := executor.Discover(t.Context(), ready, &environment.DiscoveryScope{Paths: []string{}}); err != nil || status != action.Unavailable {
			t.Fatal("an action ignored the empty executable discovery scope")
		}
		if _, err := executor.Run(t.Context(), json.RawMessage(`{}`)); err == nil || probe.discoveryRuns != before || probe.runs != 0 || len(probe.events) != 0 {
			t.Fatal("scope refusal retained executable or action authority")
		}
	}
}
