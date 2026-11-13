package actions

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
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

type BrowseInput = browsing.HermesBrowseInput
type BrowseOutput = browsing.HermesBrowseOutput

type Browse struct {
	*action.Base
	runner        BrowserRunner
	configuration atomic.Pointer[browserruntime.HermesOptions]
	setup         browsing.Setup
}

// NewBrowse creates fresh discovery/setup state without opening a runtime.
func NewBrowse(runner BrowserRunner) *Browse {
	return &Browse{
		Base: action.NewBase(actionmodel.Action{
			Name: BrowseName, Description: BrowseDescription, SessionMode: "open",
			InputSchema: json.RawMessage(browsing.HermesBrowseInputSchema), OutputSchema: json.RawMessage(browsing.HermesBrowseOutputSchema),
		}),
		runner: runner,
	}
}

func (a *Browse) Discover(ctx context.Context, env environment.Environment, scope *environment.DiscoveryScope) (status action.Status, discoveryErr error) {
	a.configuration.Store(nil)
	defer func() {
		if ctx.Err() != nil {
			a.configuration.Store(nil)
			a.SetStatus(action.Unknown)
			status, discoveryErr = action.Unknown, errors.Join(ctx.Err(), context.Cause(ctx))
		}
	}()
	configuration, status, err := discoverRuntime(ctx, a.Base, a.runner, env, scope)
	if err == nil && status == action.Ready {
		a.configuration.Store(configuration)
	}
	return status, err
}

func (a *Browse) Run(ctx context.Context, raw json.RawMessage) (action.Result, error) {
	if ctx.Err() != nil {
		return action.Result{}, errors.Join(ctx.Err(), context.Cause(ctx))
	}
	if err := a.Check(); err != nil {
		return action.Result{}, err
	}
	configuration := a.configuration.Load()
	if configuration == nil || a.runner == nil {
		return action.Result{}, errors.New("browse is unavailable; run discovery first")
	}
	return a.setup.RunHermesBrowse(ctx, a.runner, *configuration, raw)
}

func validateBrowseJSON(validate func(any) error, raw json.RawMessage) error {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return validate(value)
}
