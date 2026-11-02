package browser

import (
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

// Responsibilities returns the default capabilities without starting an agent.
func Responsibilities() []string {
	return []string{"Maintain isolated Lightpanda browser sessions for Commander"}
}

// ActionDescriptions returns release metadata without schemas or discovery state.
func ActionDescriptions() []actionmodel.Action {
	return []actionmodel.Action{}
}
