package actions

import (
	"context"
	"encoding/json"
	"errors"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/internal/commander"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

const BrowseName = "browse"
const BrowseDescription = "Use Hermes native browser tools in a Commander-bound Lightpanda session to navigate and interact with websites. Return protected final text, runtime status and reported model-request counts. Commander carries the session through task continuations; no session IDs belong in action input."

// BrowserRunner is the same host owner used for inventory and session controls.
// Actions cannot create another manager or transfer state between runtimes.
type BrowserRunner interface {
	WithSession(context.Context, func(context.Context) error) error
	RunHermes(context.Context, browserruntime.HermesOptions, func(context.Context) error) (sessions.Session, error)
}

type BrowseInput = commander.HermesBrowseInput
type BrowseOutput = commander.HermesBrowseOutput

type Browse struct {
	*action.Base
	runner       BrowserRunner
	agentFactory contracts.BrowserAgentFactory
	discovery    commander.RuntimeDiscovery
	setup        commander.Setup
}

// NewBrowse creates fresh discovery/setup state without opening a runtime.
func NewBrowse(runner BrowserRunner, factory contracts.BrowserAgentFactory, validator contracts.ModelValidator) *Browse {
	return &Browse{
		Base: action.NewBase(actionmodel.Action{
			Name: BrowseName, Description: BrowseDescription, SessionMode: "open",
			InputSchema: json.RawMessage(commander.HermesBrowseInputSchema), OutputSchema: json.RawMessage(commander.HermesBrowseOutputSchema),
		}),
		runner: runner, agentFactory: factory, setup: commander.Setup{ValidateModel: validator},
	}
}

func (a *Browse) Discover(ctx context.Context, env environment.Environment, scope *environment.DiscoveryScope) (action.Status, error) {
	return a.discovery.Discover(ctx, env, scope, a.Base, a.runner, commander.MissingCapabilities{
		BrowserAgentFactory: a.agentFactory == nil,
		ModelValidator:      a.setup.ValidateModel == nil,
	})
}

func (a *Browse) Run(ctx context.Context, raw json.RawMessage) (action.Result, error) {
	if ctx.Err() != nil {
		return action.Result{}, errors.Join(ctx.Err(), context.Cause(ctx))
	}
	if err := a.Check(); err != nil {
		return action.Result{}, err
	}
	configuration := a.discovery.Options()
	if configuration == nil || a.runner == nil || a.agentFactory == nil {
		return action.Result{}, errors.New("browse is unavailable; run discovery first")
	}
	return a.setup.RunHermesBrowse(ctx, a.runner, *configuration, a.agentFactory, raw)
}

func validateBrowseJSON(validate func(any) error, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return validate(value)
}
