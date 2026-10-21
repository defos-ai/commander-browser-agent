package browser

import agents "github.com/defos-ai/commander/framework/agent"

const AgentType agents.AgentType = "browser-agent"

// NewAgent supplies a fresh rendering catalog when config.Actions is nil.
// An explicitly empty catalog stays empty.
func NewAgent(config agents.AgentConfig) (*agents.Agent, error) {
	config.Type = AgentType
	if config.Responsibilities == nil {
		config.Responsibilities = Responsibilities()
	}
	if config.Actions == nil {
		config.Actions = NewActions()
	}
	return agents.NewAgent(config)
}
