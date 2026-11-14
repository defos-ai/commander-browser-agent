// Package hermes adapts the pinned native browser agent to scoped capabilities.
package hermes

import (
	"context"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	native "github.com/defos-ai/commander-browser-agent/internal/hermes"
)

// ModelConfig retains the Go-only factory configuration type for callers using
// the concrete Hermes adapter. Model credentials remain outside browse tasks.
type ModelConfig = contracts.ModelConfig

// Agent lends Hermes reasoning only for its original manager callback.
// It does not own another worker, browser process or automation connection.
type Agent struct {
	scope native.Scope
	model ModelConfig
}

var _ contracts.BrowserAgent = (*Agent)(nil)

// NewAgent binds the current private execution scope without starting work.
// Retaining the adapter cannot authorize a later callback, even on the same worker.
func NewAgent(ctx context.Context, model ModelConfig) (*Agent, error) {
	scope, err := native.NewScope(ctx)
	if err != nil {
		return nil, err
	}
	return &Agent{scope: scope, model: model}, nil
}

func (Agent) String() string   { return "private Hermes browser agent" }
func (Agent) GoString() string { return "private Hermes browser agent" }

func (a *Agent) Browse(ctx context.Context, request contracts.BrowseRequest, hooks contracts.Hooks) (contracts.BrowseResult, error) {
	if a == nil {
		return contracts.BrowseResult{}, native.ErrClosed
	}
	if err := a.scope.Check(ctx); err != nil {
		return contracts.BrowseResult{}, err
	}
	return native.Run(ctx, native.Request{
		Objective: request.Objective, Provider: a.model.Provider, Model: a.model.Model,
		Endpoint: a.model.Endpoint, APIKey: a.model.APIKey,
		MaxIterations: request.MaxIterations, MaxTokens: request.MaxTokensPerResponse,
		TimeoutSeconds: request.TimeoutSeconds,
	}, hooks)
}
