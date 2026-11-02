package browser

import (
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	agents "github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/id"
)

const AgentType agents.AgentType = "browser-agent"

// NewAgent supplies a process-local session provider and the default action catalog
// when they are omitted. An explicitly empty action catalog stays empty.
func NewAgent(config agents.AgentConfig) (*agents.Agent, error) {
	config.Type = AgentType
	if config.Sessions == nil {
		if config.ID == "" {
			var err error
			config.ID, err = id.AgentID(string(AgentType))
			if err != nil {
				return nil, err
			}
		}
		provider, err := browserruntime.New(config.ID)
		if err != nil {
			return nil, err
		}
		config.Sessions = provider
	}
	if config.Responsibilities == nil {
		config.Responsibilities = Responsibilities()
	}
	if config.Actions == nil {
		config.Actions = NewActions()
	}
	return agents.NewAgent(config)
}
