package actions

import (
	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander/framework/agent/action"
)

// Dependencies are scoped capability factories supplied by the composition root.
// Construction runs only inside an existing manager callback, never discovery.
type Dependencies struct {
	BrowserAgent contracts.BrowserAgentFactory
	Browser      contracts.BrowserFactory
}

// NewActions gives each host fresh discovery state and its shared session runner.
func NewActions(runner BrowserRunner, dependencies Dependencies) []action.Executor {
	return []action.Executor{
		NewBrowse(runner, dependencies.BrowserAgent),
		NewLogin(runner, dependencies.Browser),
		NewScreenshot(runner, dependencies.Browser),
	}
}
