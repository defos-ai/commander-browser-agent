package browser

import (
	"sort"

	agentactions "github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander/framework/agent/action"
)

// NewActions creates the default catalog, currently empty while the new actions
// are being implemented. Future instances must keep discovery state per host.
func NewActions() []action.Executor {
	return agentactions.NewActions()
}

func ActionNames() []string {
	actions := ActionDescriptions()
	names := make([]string, 0, len(actions))
	for _, instance := range actions {
		names = append(names, instance.Name)
	}
	sort.Strings(names)
	return names
}
