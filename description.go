package browser

import (
	"github.com/defos-ai/commander-browser-agent/actions"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

// Responsibilities returns the default capabilities without starting an agent.
func Responsibilities() []string {
	return []string{"Browse websites with Hermes native tools and return protected final text and runtime status", "Sign in through supported password and one-time-code forms with managed secrets and retain browser state", "Capture protected text-only PNG snapshots of reusable browser sessions", "Maintain independent Lightpanda browser sessions for Commander"}
}

// ActionDescriptions returns release metadata without schemas or discovery state.
func ActionDescriptions() []actionmodel.Action {
	return []actionmodel.Action{
		{Name: actions.BrowseName, Description: actions.BrowseDescription, SessionMode: "open"},
		{Name: actions.LoginName, Description: actions.LoginDescription, SessionMode: "open"},
		{Name: actions.ScreenshotName, Description: actions.ScreenshotDescription, SessionMode: "use"},
	}
}
