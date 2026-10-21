package browser

import (
	"github.com/defos-ai/commander-browser-agent/actions"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

// Responsibilities returns the default capabilities without starting an agent.
func Responsibilities() []string {
	return []string{"Check rendered accessibility and capture desktop/mobile screenshots"}
}

// ActionDescriptions returns release metadata without schemas or discovery state.
func ActionDescriptions() []actionmodel.Action {
	return []actionmodel.Action{
		{Name: actions.CheckAccessibilityName, Description: actions.CheckAccessibilityDescription},
		{Name: actions.CapturePageName, Description: actions.CapturePageDescription},
	}
}
