package browser

import (
	"github.com/defos-ai/commander-browser-agent/actions"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

// Responsibilities returns the default capabilities without starting an agent.
func Responsibilities() []string {
	return []string{"Browse websites with the agent's own model and report structured findings and evidence", "Maintain isolated Lightpanda browser sessions for Commander"}
}

// ActionDescriptions returns release metadata without schemas or discovery state.
func ActionDescriptions() []actionmodel.Action {
	return []actionmodel.Action{{Name: actions.BrowseName, Description: actions.BrowseDescription, SessionMode: "create"}}
}
