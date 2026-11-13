package actions

import (
	"context"
	"errors"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	hermesruntime "github.com/defos-ai/commander-browser-agent/browser/hermes"
	"github.com/defos-ai/commander-browser-agent/browser/lightpanda"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

func discoverRuntime(ctx context.Context, base *action.Base, runner BrowserRunner, env environment.Environment, scope *environment.DiscoveryScope) (*browserruntime.HermesOptions, action.Status, error) {
	base.SetStatus(action.Unknown)
	if ctx.Err() != nil {
		return nil, action.Unknown, errors.Join(ctx.Err(), context.Cause(ctx))
	}
	if runner == nil {
		base.SetUnavailable("The host must provide its browser session runner", actionmodel.Requirement{Kind: actionmodel.ConfigurationRequirement, Name: "browser-session-provider"})
		return nil, action.Unavailable, nil
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
			return nil, action.Unavailable, nil
		case errors.As(err, &browserUnavailable):
			base.SetUnavailable(browserUnavailable.Reason, browserUnavailable.Requirement)
			return nil, action.Unavailable, nil
		}
		return nil, action.Unknown, err
	}
	if ctx.Err() != nil {
		return nil, action.Unknown, errors.Join(ctx.Err(), context.Cause(ctx))
	}
	base.SetStatus(action.Ready)
	return &browserruntime.HermesOptions{Python: hermes.Python(), Source: hermes.Source(), Executable: browser.Executable()}, action.Ready, nil
}
