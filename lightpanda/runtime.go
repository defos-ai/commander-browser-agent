// Package lightpanda discovers the supported browser automation executable.
package lightpanda

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

// Version is the Lightpanda release supported by this agent.
const Version = "1.0.2"

// Runtime identifies a version-checked executable. It owns no browser process.
type Runtime struct{ executable string }

// Executable returns the discovered absolute path, or empty for a zero Runtime.
func (r Runtime) Executable() string { return r.executable }

// UnavailableError describes a prerequisite that an operator can repair.
type UnavailableError struct {
	Reason      string
	Requirement actionmodel.Requirement
}

func (e *UnavailableError) Error() string { return e.Reason }

// Discover checks LIGHTPANDA_PATH or searches for lightpanda through env. An
// explicit scope wins over the configured path. Every failure returns a zero
// Runtime, so callers can replace their previous discovery result directly.
func Discover(ctx context.Context, env environment.Environment, scope *environment.DiscoveryScope) (Runtime, error) {
	if err := ctx.Err(); err != nil {
		return Runtime{}, err
	}
	if env == nil {
		env = &environment.Local{}
	}
	name := "lightpanda"
	searchScope := scope.Clone()
	configured, _ := env.LookupEnv("LIGHTPANDA_PATH")
	if configured != "" {
		if !filepath.IsAbs(configured) {
			return Runtime{}, &UnavailableError{
				Reason:      "LIGHTPANDA_PATH must be an absolute executable path",
				Requirement: actionmodel.Requirement{Kind: actionmodel.ConfigurationRequirement, Name: "LIGHTPANDA_PATH"},
			}
		}
		if searchScope == nil || searchScope.Paths == nil {
			if resolved, err := filepath.EvalSymlinks(configured); err == nil {
				configured = resolved
			}
			if searchScope == nil {
				searchScope = &environment.DiscoveryScope{}
			}
			searchScope.Paths = []string{filepath.Dir(configured)}
		}
		name = filepath.Base(configured)
	}
	executable, err := env.FindExecutable(ctx, name, searchScope)
	if err := ctx.Err(); err != nil {
		return Runtime{}, err
	}
	if err != nil {
		return Runtime{}, err
	}
	if executable == "" {
		return Runtime{}, unavailable("Install Lightpanda " + Version + " or set LIGHTPANDA_PATH to its executable")
	}
	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, runErr := env.Run(versionCtx, []string{executable, "version"})
	if err := ctx.Err(); err != nil {
		return Runtime{}, err
	}
	if versionCtx.Err() != nil {
		return Runtime{}, unavailable("Lightpanda version check timed out; check the executable and rediscover")
	}
	if runErr != nil || result.ReturnCode != 0 {
		return Runtime{}, unavailable("Cannot execute Lightpanda " + Version + "; install the binary for this platform or correct LIGHTPANDA_PATH")
	}
	if strings.TrimSpace(result.Stdout) != Version {
		return Runtime{}, unavailable("Lightpanda " + Version + " is required; install that version or correct LIGHTPANDA_PATH")
	}
	return Runtime{executable: executable}, nil
}

func unavailable(reason string) *UnavailableError {
	return &UnavailableError{Reason: reason, Requirement: actionmodel.Requirement{Kind: actionmodel.ExecutableRequirement, Name: "lightpanda"}}
}
