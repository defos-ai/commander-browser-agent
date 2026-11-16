package browser

import (
	"github.com/defos-ai/commander-browser-agent/actions"
	hermesruntime "github.com/defos-ai/commander-browser-agent/internal/commander/discovery/hermes"
	lightpandaruntime "github.com/defos-ai/commander-browser-agent/internal/commander/discovery/lightpanda"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	agentmodel "github.com/defos-ai/commander/framework/domain/agents"
)

// Responsibilities returns the default capabilities without starting an agent.
func Responsibilities() []string {
	return []string{"Browse websites with Hermes native tools and return protected final text and runtime status", "Sign in through supported password and one-time-code forms with managed secrets and retain browser state", "Capture protected text-only PNG snapshots of reusable browser sessions", "Maintain independent Lightpanda browser sessions for Commander"}
}

// Dependencies returns external software guidance without checking installation.
// Each call owns its slice; actual readiness belongs to action discovery.
func Dependencies() []agentmodel.Dependency {
	return []agentmodel.Dependency{
		{
			Name:        "lightpanda",
			Description: "Included in the browser agent Docker image. Native runs require the supported Lightpanda binary; Linux ARM64 requires glibc 2.38 or newer.",
			Version:     lightpandaruntime.Version,
		},
		{
			Name:        "hermes-runtime",
			Description: "Included in the browser agent Docker image. Native runs require the pinned Hermes source and frozen CPython 3.14.10 environment with browser-harness 0.1.13 under HERMES_INSTALL_ROOT (default /opt/hermes), using .venv/bin/python.",
			Version:     hermesruntime.Version,
		},
	}
}

// ActionDescriptions returns release metadata without schemas or discovery state.
func ActionDescriptions() []actionmodel.Action {
	return []actionmodel.Action{
		{Name: actions.BrowseName, Description: actions.BrowseDescription, SessionMode: "open"},
		{Name: actions.LoginName, Description: actions.LoginDescription, SessionMode: "open"},
		{Name: actions.ScreenshotName, Description: actions.ScreenshotDescription, SessionMode: "use"},
	}
}
