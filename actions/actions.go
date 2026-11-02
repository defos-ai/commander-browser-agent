package actions

import "github.com/defos-ai/commander/framework/agent/action"

// NewActions keeps the catalog explicitly empty until browser actions are added.
func NewActions() []action.Executor {
	return []action.Executor{}
}
