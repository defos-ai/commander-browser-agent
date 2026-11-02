package browser_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/lightpanda"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
)

func TestLightpandaDiscoveryRespectsScopeAndRediscovery(t *testing.T) {
	env, executable := newLightpandaEnvironment(t)
	env.values["LIGHTPANDA_PATH"] = executable
	found, err := lightpanda.Discover(t.Context(), env, nil)
	if err != nil || found.Executable() != executable {
		t.Fatalf("configured runtime: executable=%q error=%v", found.Executable(), err)
	}
	if !slices.Equal(env.argv, []string{executable, "version"}) || env.versionBudget <= 0 || env.versionBudget > 5*time.Second {
		t.Fatalf("version check was not a bounded direct command: argv=%v budget=%v", env.argv, env.versionBudget)
	}
	_, otherExecutable := newLightpandaEnvironment(t)
	found, err = lightpanda.Discover(t.Context(), env, &environment.DiscoveryScope{Paths: []string{filepath.Dir(otherExecutable)}})
	if err != nil || found.Executable() != otherExecutable {
		t.Fatalf("explicit scope did not win: executable=%q error=%v", found.Executable(), err)
	}
	found, err = lightpanda.Discover(t.Context(), env, &environment.DiscoveryScope{Paths: []string{}})
	var unavailable *lightpanda.UnavailableError
	if found.Executable() != "" || !errors.As(err, &unavailable) || unavailable.Requirement != (actionmodel.Requirement{Kind: actionmodel.ExecutableRequirement, Name: "lightpanda"}) {
		t.Fatalf("restricted rediscovery retained readiness: runtime=%q error=%v", found.Executable(), err)
	}
	found, err = lightpanda.Discover(t.Context(), env, nil)
	if err != nil || found.Executable() != executable {
		t.Fatalf("runtime did not recover: executable=%q error=%v", found.Executable(), err)
	}
	delete(env.values, "LIGHTPANDA_PATH")
	found, err = lightpanda.Discover(t.Context(), env, nil)
	if err != nil || found.Executable() != executable {
		t.Fatalf("normal executable search: executable=%q error=%v", found.Executable(), err)
	}
}

func TestLightpandaDiscoveryResolvesConfiguredSymlinkWithinScope(t *testing.T) {
	env, executable := newLightpandaEnvironment(t)
	linkDirectory := t.TempDir()
	link := filepath.Join(linkDirectory, filepath.Base(executable))
	if err := os.Symlink(executable, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	env.values["LIGHTPANDA_PATH"] = link
	found, err := lightpanda.Discover(t.Context(), env, nil)
	if err != nil || found.Executable() != executable {
		t.Fatalf("configured symlink discovery: executable=%q error=%v", found.Executable(), err)
	}
	found, err = lightpanda.Discover(t.Context(), env, &environment.DiscoveryScope{Paths: []string{linkDirectory}})
	var unavailable *lightpanda.UnavailableError
	if found.Executable() != "" || !errors.As(err, &unavailable) {
		t.Fatalf("configured symlink escaped explicit scope: executable=%q error=%v", found.Executable(), err)
	}
}

func TestLightpandaDiscoveryReportsRequirementsWithoutProcessOutput(t *testing.T) {
	for _, test := range []struct {
		name        string
		configure   func(*lightpandaEnvironment)
		requirement actionmodel.Requirement
	}{
		{name: "relative path", configure: func(env *lightpandaEnvironment) { env.values["LIGHTPANDA_PATH"] = "relative/lightpanda" }, requirement: actionmodel.Requirement{Kind: actionmodel.ConfigurationRequirement, Name: "LIGHTPANDA_PATH"}},
		{name: "missing", configure: func(env *lightpandaEnvironment) { env.Local.SearchPaths = []string{} }},
		{name: "wrong version", configure: func(env *lightpandaEnvironment) { env.result.Stdout = "1.0.3 private process output" }},
		{name: "empty output", configure: func(env *lightpandaEnvironment) { env.result.Stdout = "" }},
		{name: "nonzero exit", configure: func(env *lightpandaEnvironment) {
			env.result.ReturnCode = 7
			env.result.Stderr = "private process output"
		}},
		{name: "execution failure", configure: func(env *lightpandaEnvironment) { env.runErr = errors.New("private process output") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			env, _ := newLightpandaEnvironment(t)
			test.configure(env)
			found, err := lightpanda.Discover(t.Context(), env, nil)
			var unavailable *lightpanda.UnavailableError
			if found.Executable() != "" || !errors.As(err, &unavailable) || unavailable.Reason == "" || strings.Contains(err.Error(), "private process output") {
				t.Fatalf("unusable runtime did not report a safe readiness gap: executable=%q error=%v", found.Executable(), err)
			}
			want := test.requirement
			if want.Name == "" {
				want = actionmodel.Requirement{Kind: actionmodel.ExecutableRequirement, Name: "lightpanda"}
			}
			if unavailable.Requirement != want {
				t.Fatalf("requirement=%+v, want %+v", unavailable.Requirement, want)
			}
		})
	}
}

func TestLightpandaDiscoveryPropagatesCancellationAndDiscoveryErrors(t *testing.T) {
	env, _ := newLightpandaEnvironment(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if found, err := lightpanda.Discover(ctx, env, nil); !errors.Is(err, context.Canceled) || found.Executable() != "" || len(env.argv) != 0 {
		t.Fatalf("canceled discovery: executable=%q error=%v command=%v", found.Executable(), err, env.argv)
	}
	want := errors.New("environment discovery failed")
	env.findErr = want
	if found, err := lightpanda.Discover(t.Context(), env, nil); !errors.Is(err, want) || found.Executable() != "" {
		t.Fatalf("discovery error: executable=%q error=%v", found.Executable(), err)
	}
	env.findErr = nil
	ctx, cancel = context.WithCancel(t.Context())
	env.onRun = func() { cancel() }
	if found, err := lightpanda.Discover(ctx, env, nil); !errors.Is(err, context.Canceled) || found.Executable() != "" {
		t.Fatalf("version cancellation: executable=%q error=%v", found.Executable(), err)
	}
}

type lightpandaEnvironment struct {
	*environment.Local
	values        map[string]string
	result        environment.Result
	findErr       error
	runErr        error
	onRun         func()
	argv          []string
	versionBudget time.Duration
}

func newLightpandaEnvironment(t *testing.T) (*lightpandaEnvironment, string) {
	t.Helper()
	directory := t.TempDir()
	name := "lightpanda"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(directory, name)
	if err := os.WriteFile(executable, []byte("fixture executable, never launched"), 0700); err != nil {
		t.Fatal(err)
	}
	return &lightpandaEnvironment{Local: &environment.Local{SearchPaths: []string{directory}}, values: map[string]string{}, result: environment.Result{Stdout: lightpanda.Version}}, executable
}

func (env *lightpandaEnvironment) LookupEnv(name string) (string, bool) {
	value, ok := env.values[name]
	return value, ok
}

func (env *lightpandaEnvironment) FindExecutable(ctx context.Context, name string, scope *environment.DiscoveryScope) (string, error) {
	if env.findErr != nil {
		return "", env.findErr
	}
	return env.Local.FindExecutable(ctx, name, scope)
}

func (env *lightpandaEnvironment) Run(ctx context.Context, argv []string) (environment.Result, error) {
	env.argv = slices.Clone(argv)
	if deadline, ok := ctx.Deadline(); ok {
		env.versionBudget = time.Until(deadline)
	}
	if env.onRun != nil {
		env.onRun()
	}
	return env.result, env.runErr
}
