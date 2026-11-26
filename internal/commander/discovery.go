package commander

import (
	"context"
	"errors"
	"sync/atomic"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	hermesruntime "github.com/defos-ai/commander-browser-agent/internal/commander/discovery/hermes"
	"github.com/defos-ai/commander-browser-agent/internal/commander/discovery/lightpanda"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

// RuntimeDiscovery keeps the verified runtime options for one action.
type RuntimeDiscovery struct {
	options atomic.Pointer[browserruntime.HermesOptions]
}

func (d *RuntimeDiscovery) Options() *browserruntime.HermesOptions { return d.options.Load() }

// MissingCapabilities identifies required host capabilities absent from this action.
type MissingCapabilities struct {
	BrowserAgentFactory bool
	ModelValidator      bool
	BrowserFactory      bool
}

func (d *RuntimeDiscovery) Discover(ctx context.Context, env environment.Environment, scope *environment.DiscoveryScope, base *action.Base, runner HermesBrowseRunner, missing MissingCapabilities) (status action.Status, discoveryErr error) {
	d.options.Store(nil)
	base.SetStatus(action.Unknown)
	defer func() {
		if err := ctx.Err(); err != nil {
			d.options.Store(nil)
			base.SetStatus(action.Unknown)
			status, discoveryErr = action.Unknown, errors.Join(err, context.Cause(ctx))
		}
	}()
	if err := ctx.Err(); err != nil {
		return action.Unknown, err
	}

	var name, reason string
	switch {
	case missing.BrowserAgentFactory:
		name, reason = "browser-agent-factory", "The host must provide its browser agent factory"
	case missing.ModelValidator:
		name, reason = "browser-model-validator", "The host must provide its browser model validator"
	case missing.BrowserFactory:
		name, reason = "browser-factory", "The host must provide its browser factory"
	case runner == nil:
		name, reason = "browser-session-provider", "The host must provide its browser session runner"
	}
	if name != "" {
		base.SetUnavailable(reason, actionmodel.Requirement{Kind: actionmodel.ConfigurationRequirement, Name: name})
		return action.Unavailable, nil
	}
	if env == nil {
		env = &environment.Local{}
	}
	hermes, err := hermesruntime.Discover(ctx, env, scope)
	var browser lightpanda.Runtime
	if err == nil {
		browser, err = lightpanda.Discover(ctx, env, scope)
	}
	if err != nil {
		var hermesUnavailable *hermesruntime.UnavailableError
		var browserUnavailable *lightpanda.UnavailableError
		switch {
		case errors.As(err, &hermesUnavailable):
			base.SetUnavailable(hermesUnavailable.Reason, hermesUnavailable.Requirement)
			return action.Unavailable, nil
		case errors.As(err, &browserUnavailable):
			base.SetUnavailable(browserUnavailable.Reason, browserUnavailable.Requirement)
			return action.Unavailable, nil
		}
		return action.Unknown, err
	}
	d.options.Store(&browserruntime.HermesOptions{Python: hermes.Python(), Source: hermes.Source(), Executable: browser.Executable()})
	base.SetStatus(action.Ready)
	return action.Ready, nil
}
