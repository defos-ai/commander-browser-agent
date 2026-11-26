package browser

import (
	"errors"

	agentactions "github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/internal/commander"
	agents "github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/id"
)

const AgentType agents.AgentType = "browser-agent"

var _ agentactions.BrowserRunner = (*commander.SessionManager)(nil)

// NewAgent supplies a process-local session provider and uses the injected
// capabilities for an omitted action catalog. An explicitly empty catalog stays empty.
func NewAgent(config agents.AgentConfig, dependencies agentactions.Dependencies) (*agents.Agent, error) {
	config.Type = AgentType
	if config.Sessions == nil {
		if config.ID == "" {
			var err error
			config.ID, err = id.AgentID(string(AgentType))
			if err != nil {
				return nil, err
			}
		}
		provider, err := commander.NewSessionManager(config.ID)
		if err != nil {
			return nil, err
		}
		config.Sessions = provider
	}
	if config.Responsibilities == nil {
		config.Responsibilities = Responsibilities()
	}
	if config.Dependencies == nil {
		config.Dependencies = Dependencies()
	}
	if config.Actions == nil {
		runner, ok := config.Sessions.(agentactions.BrowserRunner)
		if !ok {
			return nil, errors.New("default browser actions require a browser session runner")
		}
		config.Actions = NewActions(runner, dependencies)
	}
	if config.Secrets == nil {
		config.Secrets = &agents.SecretConfig{}
	}
	return agents.NewAgent(config)
}
