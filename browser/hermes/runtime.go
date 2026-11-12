// Package hermes discovers the pinned Python browser-agent runtime without
// starting an agent, model request or browser session.
package hermes

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

const Version = "v0.23.1"

const sourceCommit = "3b421578588243f920886e6fe79a6faa87ea026e"
const pythonVersion = "3.14.10"
const harnessVersion = "0.1.13"
const defaultSource = "/opt/hermes"

//go:embed check.py
var readinessChecker string

// Runtime identifies an offline-checked package and its exact virtual-environment
// invocation path. It owns no worker or browser process.
type Runtime struct {
	python string
	source string
}

func (r Runtime) Python() string { return r.python }
func (r Runtime) Source() string { return r.source }

// UnavailableError describes a prerequisite the operator can repair. Its fields
// never contain subprocess output or configured filesystem paths.
type UnavailableError struct {
	Reason      string
	Requirement actionmodel.Requirement
}

func (e *UnavailableError) Error() string { return e.Reason }

// Discover checks one trusted Hermes package root. An explicit executable scope
// must admit its exact virtual-environment bin directory. The Python invocation
// deliberately retains its symlink: resolving it would discard venv identity.
// Every failure returns a zero Runtime so stale readiness cannot survive.
func Discover(ctx context.Context, env environment.Environment, scope *environment.DiscoveryScope) (Runtime, error) {
	if ctx.Err() != nil {
		return Runtime{}, contextError(ctx)
	}
	if !supportedPlatform() {
		return Runtime{}, unavailable("Hermes browser sessions require Unix process-group support; use Docker or WSL", actionmodel.ConfigurationRequirement, "hermes-runtime")
	}
	if env == nil {
		env = &environment.Local{}
	}
	source, _ := env.LookupEnv("HERMES_INSTALL_ROOT")
	if source == "" {
		source = defaultSource
	}
	if !filepath.IsAbs(source) || len(source) > 4096 || strings.ContainsRune(source, 0) {
		return Runtime{}, unavailable("HERMES_INSTALL_ROOT must be an absolute package directory", actionmodel.ConfigurationRequirement, "HERMES_INSTALL_ROOT")
	}
	source = filepath.Clean(source)
	bin := filepath.Join(source, ".venv", "bin")
	if !scopeAllows(scope, bin) {
		return Runtime{}, unavailable("Hermes runtime is outside the executable discovery scope; include its virtual environment bin directory", actionmodel.ConfigurationRequirement, "search-paths")
	}
	python := filepath.Join(bin, "python")
	if ctx.Err() != nil {
		return Runtime{}, contextError(ctx)
	}
	bounded, stop := context.WithTimeout(ctx, 25*time.Second)
	defer stop()
	result, err := env.Run(bounded, []string{python, "-I", "-S", "-c", readinessChecker, "--source", source})
	if ctx.Err() != nil {
		return Runtime{}, contextError(ctx)
	}
	if bounded.Err() != nil {
		return Runtime{}, unavailable("Hermes runtime readiness check timed out; use the packaged runtime and rediscover", actionmodel.ExecutableRequirement, "hermes-runtime")
	}
	if err != nil || result.ReturnCode != 0 || !ready(result.Stdout) {
		return Runtime{}, unavailable("Install the packaged Hermes "+Version+" runtime or rebuild the browser agent image", actionmodel.ExecutableRequirement, "hermes-runtime")
	}
	return Runtime{python: python, source: source}, nil
}

func scopeAllows(scope *environment.DiscoveryScope, bin string) bool {
	if scope == nil || scope.Paths == nil {
		return true
	}
	for _, directory := range scope.Paths {
		if filepath.IsAbs(directory) && !strings.ContainsRune(directory, 0) && filepath.Clean(directory) == bin {
			return true
		}
	}
	return false
}

func ready(output string) bool {
	if len(output) > 4096 {
		return false
	}
	// Decode the small fixed object ourselves so duplicate keys, nested values,
	// unknown fields and trailing JSON cannot masquerade as a readiness receipt.
	decoder := json.NewDecoder(strings.NewReader(output))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	fields := make(map[string]string, 5)
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return false
		}
		if _, duplicate := fields[name]; duplicate {
			return false
		}
		switch name {
		case "status", "hermes", "commit", "python", "browser_harness":
		default:
			return false
		}
		var value string
		if decoder.Decode(&value) != nil {
			return false
		}
		fields[name] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return false
	}
	return len(fields) == 5 && fields["status"] == "ready" && fields["hermes"] == Version &&
		fields["commit"] == sourceCommit && fields["python"] == pythonVersion && fields["browser_harness"] == harnessVersion
}

func unavailable(reason string, kind actionmodel.RequirementKind, name string) *UnavailableError {
	return &UnavailableError{Reason: reason, Requirement: actionmodel.Requirement{Kind: kind, Name: name}}
}

func contextError(ctx context.Context) error {
	return errors.Join(ctx.Err(), context.Cause(ctx))
}
