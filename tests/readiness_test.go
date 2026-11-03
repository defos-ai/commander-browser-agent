package browser_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
)

func TestAgentRediscoveryClearsAnUnavailableBrowser(t *testing.T) {
	probe := &unitSetup{t: t}
	executor := actions.NewBrowse(probe)
	ready := unitEnvironment{probe}
	if status, err := executor.Discover(t.Context(), ready, nil); err != nil || status != action.Ready {
		t.Fatal("installed browser was not ready")
	}
	if status, err := executor.Discover(t.Context(), wrongBrowserVersion{ready}, nil); err != nil || status != action.Unavailable {
		t.Fatal("rediscovery retained an unsupported browser version")
	}
	description, _ := json.Marshal(executor.Action())
	_, err := executor.Run(t.Context(), json.RawMessage(`{}`))
	if err == nil || len(probe.events) != 0 || probe.runs != 0 || strings.Contains(string(description), "private-process-output") {
		t.Fatal("unavailable browser reached execution or exposed subprocess output")
	}
	if status, err := executor.Discover(t.Context(), ready, nil); err != nil || status != action.Ready {
		t.Fatal("browser readiness did not recover after installation was corrected")
	}
}

type wrongBrowserVersion struct{ unitEnvironment }

func (wrongBrowserVersion) Run(context.Context, []string) (environment.Result, error) {
	return environment.Result{Stdout: "1.0.3 private-process-output"}, nil
}
