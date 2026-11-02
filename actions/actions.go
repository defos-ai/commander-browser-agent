package actions

import "github.com/defos-ai/commander/framework/agent/action"

// NewActions gives each host fresh discovery state and its shared session runner.
func NewActions(runner BrowserRunner) []action.Executor {
	return []action.Executor{NewBrowse(runner)}
}
