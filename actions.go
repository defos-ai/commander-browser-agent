package browser

import (
	"sort"

	agentactions "github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander/framework/agent/action"
)

// NewActions creates fresh actions using this host's shared browser sessions.
func NewActions(runner agentactions.BrowserRunner) []action.Executor {
	return agentactions.NewActions(runner)
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
