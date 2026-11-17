package actions

import (
	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander/framework/agent/action"
)

// Dependencies are capabilities supplied by the composition root. Factories and
// model validation run inside a manager callback, never during discovery.
type Dependencies struct {
	BrowserAgent  contracts.BrowserAgentFactory
	Browser       contracts.BrowserFactory
	ValidateModel contracts.ModelValidator
}

// NewActions gives each host fresh discovery state and its shared session runner.
func NewActions(runner BrowserRunner, dependencies Dependencies) []action.Executor {
	return []action.Executor{
		NewBrowse(runner, dependencies.BrowserAgent, dependencies.ValidateModel),
		NewLogin(runner, dependencies.Browser),
		NewScreenshot(runner, dependencies.Browser),
	}
}
