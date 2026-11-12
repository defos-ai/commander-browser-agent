package browser_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	hermesruntime "github.com/defos-ai/commander-browser-agent/browser/hermes"
	"github.com/defos-ai/commander/framework/agent/environment"
)

const hermesReadyReceipt = `{"status":"ready","hermes":"v0.23.1","commit":"3b421578588243f920886e6fe79a6faa87ea026e","python":"3.14.10","browser_harness":"0.1.13"}`

type hermesDiscoveryEnvironment struct {
	root      string
	lookups   []string
	finds     int
	calls     [][]string
	remaining time.Duration
	result    environment.Result
	failure   error
	run       func(context.Context) (environment.Result, error)
}

func (probe *hermesDiscoveryEnvironment) LookupEnv(name string) (string, bool) {
	probe.lookups = append(probe.lookups, name)
	if name == "HERMES_INSTALL_ROOT" {
		return probe.root, probe.root != ""
	}
	return "unexpected-model-environment-value", true
}

func (probe *hermesDiscoveryEnvironment) FindExecutable(context.Context, string, *environment.DiscoveryScope) (string, error) {
	probe.finds++
	return "/unexpected/resolved/python", nil
}

func (probe *hermesDiscoveryEnvironment) Run(ctx context.Context, argv []string) (environment.Result, error) {
	probe.calls = append(probe.calls, append([]string(nil), argv...))
	if deadline, bounded := ctx.Deadline(); bounded {
		probe.remaining = time.Until(deadline)
	}
	if probe.run != nil {
		return probe.run(ctx)
	}
	return probe.result, probe.failure
}

func requireHermesDiscoveryPlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("native Hermes discovery requires Unix process ownership")
	}
}

func assertHermesDiscoveryInvocation(t *testing.T, probe *hermesDiscoveryEnvironment, root string, calls int) {
	t.Helper()
	if probe.finds != 0 || !reflect.DeepEqual(probe.lookups, []string{"HERMES_INSTALL_ROOT"}) || len(probe.calls) != calls {
		t.Fatal("Hermes discovery searched another interpreter or read model configuration")
	}
	if calls == 0 {
		return
	}
	argv := probe.calls[0]
	if len(argv) != 7 || argv[0] != filepath.Join(root, ".venv", "bin", "python") || argv[1] != "-I" || argv[2] != "-S" || argv[3] != "-c" ||
		strings.TrimSpace(argv[4]) == "" || argv[5] != "--source" || argv[6] != root || probe.remaining <= 0 || probe.remaining > 25*time.Second {
		t.Fatal("Hermes discovery lost its lexical virtual environment or bounded isolated invocation")
	}
}

func TestHermesDiscoveryKeepsVirtualEnvironmentAndExplicitScope(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	root := filepath.Join(t.TempDir(), "Hermes package")
	bin := filepath.Join(root, ".venv", "bin")
	if os.MkdirAll(bin, 0700) != nil {
		t.Fatal("cannot prepare the synthetic virtual environment")
	}
	sharedPython := filepath.Join(t.TempDir(), "python")
	if os.WriteFile(sharedPython, []byte("synthetic interpreter"), 0700) != nil || os.Symlink(sharedPython, filepath.Join(bin, "python")) != nil {
		t.Fatal("cannot prepare the synthetic interpreter symlink")
	}
	for _, test := range []struct {
		name       string
		configured string
		scope      *environment.DiscoveryScope
		allowed    bool
	}{
		{name: "packaged_default", allowed: true},
		{name: "lexical_symlink", configured: root, scope: &environment.DiscoveryScope{Paths: []string{bin}}, allowed: true},
		{name: "omitted_scope_paths", configured: root, scope: &environment.DiscoveryScope{}, allowed: true},
		{name: "explicit_admission", configured: root, scope: &environment.DiscoveryScope{Paths: []string{"/other", bin}}, allowed: true},
		{name: "empty_scope", configured: root, scope: &environment.DiscoveryScope{Paths: []string{}}},
		{name: "source_is_not_invocation_directory", configured: root, scope: &environment.DiscoveryScope{Paths: []string{root}}},
		{name: "other_invocation_directory", configured: root, scope: &environment.DiscoveryScope{Paths: []string{"/other/.venv/bin"}}},
		{name: "relative_scope", configured: root, scope: &environment.DiscoveryScope{Paths: []string{".venv/bin"}}},
		{name: "relative_package_root", configured: "relative/hermes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := &hermesDiscoveryEnvironment{root: test.configured, result: environment.Result{Stdout: hermesReadyReceipt}}
			found, err := hermesruntime.Discover(t.Context(), probe, test.scope)
			selected := test.configured
			if selected == "" {
				selected = "/opt/hermes"
			}
			calls := 0
			if test.allowed {
				calls = 1
				if err != nil || found.Source() != selected || found.Python() != filepath.Join(selected, ".venv", "bin", "python") {
					t.Fatal("pinned Hermes discovery rejected its authorized virtual environment")
				}
			} else {
				var unavailable *hermesruntime.UnavailableError
				if !errors.As(err, &unavailable) || unavailable.Requirement.Name == "" || found.Source() != "" || found.Python() != "" {
					t.Fatal("Hermes discovery ran outside its explicit executable scope")
				}
			}
			assertHermesDiscoveryInvocation(t, probe, selected, calls)
		})
	}
}

func TestHermesDiscoveryRejectsUntrustedReceiptsAndClearsReadiness(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	const private = "fixture-private-readiness-output"
	probe := &hermesDiscoveryEnvironment{root: "/trusted/hermes-fixture", result: environment.Result{Stdout: hermesReadyReceipt}}
	if ready, err := hermesruntime.Discover(t.Context(), probe, nil); err != nil || ready.Python() == "" {
		t.Fatal("readiness fixture did not begin with a valid pinned runtime")
	}
	for _, test := range []struct {
		name    string
		result  environment.Result
		failure error
	}{
		{name: "missing_interpreter", result: environment.Result{Stdout: private, Stderr: private}, failure: errors.New(private)},
		{name: "failed_checker", result: environment.Result{Stdout: hermesReadyReceipt, Stderr: private, ReturnCode: 1}},
		{name: "wrong_identity", result: environment.Result{Stdout: strings.Replace(hermesReadyReceipt, "3.14.10", "3.14.9", 1)}},
		{name: "extra_json", result: environment.Result{Stdout: hermesReadyReceipt + ` {"private":"` + private + `"}`}},
		{name: "unknown_field", result: environment.Result{Stdout: strings.TrimSuffix(hermesReadyReceipt, "}") + `,"extra":"` + private + `"}`}},
		{name: "duplicate_field", result: environment.Result{Stdout: strings.TrimSuffix(hermesReadyReceipt, "}") + `,"status":"ready"}`}},
		{name: "oversized_output", result: environment.Result{Stdout: strings.Repeat(private, 4097/len(private)+1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe.result, probe.failure = test.result, test.failure
			found, err := hermesruntime.Discover(t.Context(), probe, nil)
			var unavailable *hermesruntime.UnavailableError
			if !errors.As(err, &unavailable) || found.Python() != "" || found.Source() != "" || unavailable.Reason == "" || unavailable.Requirement.Name == "" {
				t.Fatal("failed Hermes rediscovery retained a usable runtime")
			}
			metadata, marshalErr := json.Marshal(unavailable)
			if marshalErr != nil || strings.Contains(err.Error(), private) || strings.Contains(string(metadata), private) || strings.Contains(string(metadata), probe.root) {
				t.Fatal("Hermes discovery exposed subprocess output or a configured private path")
			}
		})
	}
	probe.result, probe.failure = environment.Result{Stdout: hermesReadyReceipt}, nil
	if found, err := hermesruntime.Discover(t.Context(), probe, nil); err != nil || found.Python() == "" || found.Source() != probe.root || probe.finds != 0 {
		t.Fatal("Hermes discovery did not recover after a corrected installation")
	}
	for _, name := range probe.lookups {
		if name != "HERMES_INSTALL_ROOT" {
			t.Fatal("Hermes rediscovery read model configuration")
		}
	}
}

func TestHermesDiscoveryCancellationJoinsAndPreservesCause(t *testing.T) {
	requireHermesDiscoveryPlatform(t)
	cause := errors.New("fixture readiness cancellation")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	entered := make(chan struct{})
	probe := &hermesDiscoveryEnvironment{run: func(ctx context.Context) (environment.Result, error) {
		close(entered)
		<-ctx.Done()
		return environment.Result{Stdout: "private-cancelled-process-output"}, ctx.Err()
	}}
	type outcome struct {
		runtime hermesruntime.Runtime
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		found, err := hermesruntime.Discover(ctx, probe, nil)
		done <- outcome{runtime: found, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Hermes discovery did not enter its bounded checker")
	}
	cancel(cause)
	select {
	case stopped := <-done:
		if !errors.Is(stopped.err, context.Canceled) || !errors.Is(stopped.err, cause) || stopped.runtime.Python() != "" || stopped.runtime.Source() != "" || strings.Contains(stopped.err.Error(), "private-cancelled-process-output") {
			t.Fatal("Hermes discovery lost cancellation identity or leaked subprocess output")
		}
	case <-time.After(time.Second):
		t.Fatal("Hermes discovery did not join its cancelled checker")
	}
	before := len(probe.calls)
	if found, err := hermesruntime.Discover(ctx, probe, nil); !errors.Is(err, cause) || found.Python() != "" || len(probe.calls) != before {
		t.Fatal("cancelled discovery started another checker")
	}
	deadline, stop := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer stop()
	probe.run = func(ctx context.Context) (environment.Result, error) {
		<-ctx.Done()
		return environment.Result{}, ctx.Err()
	}
	if found, err := hermesruntime.Discover(deadline, probe, nil); !errors.Is(err, context.DeadlineExceeded) || found.Python() != "" || found.Source() != "" {
		t.Fatal("hung Hermes discovery exceeded or lost its caller deadline")
	}
}
