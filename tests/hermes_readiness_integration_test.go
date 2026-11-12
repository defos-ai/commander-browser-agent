//go:build hermesintegration

package browser_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	hermesruntime "github.com/defos-ai/commander-browser-agent/browser/hermes"
	"github.com/defos-ai/commander/framework/agent/environment"
)

type hermesReadinessLocal struct {
	environment.Local
	lookups []string
	finds   int
	calls   [][]string
	result  environment.Result
}

func (probe *hermesReadinessLocal) LookupEnv(name string) (string, bool) {
	probe.lookups = append(probe.lookups, name)
	return probe.Local.LookupEnv(name)
}

func (probe *hermesReadinessLocal) FindExecutable(context.Context, string, *environment.DiscoveryScope) (string, error) {
	probe.finds++
	return "", nil
}

func (probe *hermesReadinessLocal) Run(ctx context.Context, argv []string) (environment.Result, error) {
	probe.calls = append(probe.calls, append([]string(nil), argv...))
	result, err := probe.Local.Run(ctx, argv)
	probe.result = result
	return result, err
}

func TestHermesReadinessUsesPackagedVirtualEnvironmentWithoutStartingWork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("packaged Hermes integration requires the Linux runtime image")
	}
	const canary = "fixture-readiness-model-key-never-output"
	var modelRequests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelRequests.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(model.Close)
	home := t.TempDir()
	if os.WriteFile(filepath.Join(home, "sentinel"), []byte("untouched"), 0600) != nil {
		t.Fatal("cannot prepare the readiness home marker")
	}
	t.Setenv("HERMES_INSTALL_ROOT", "/opt/hermes")
	t.Setenv("HOME", home)
	t.Setenv("HERMES_HOME", home)
	t.Setenv("OPENAI_API_KEY", canary)
	t.Setenv("OPENAI_BASE_URL", model.URL+"/v1")
	t.Setenv("BROWSER_MODEL_API_KEY", canary)
	t.Setenv("BROWSER_MODEL_URL", model.URL+"/v1/chat/completions")
	t.Setenv("BROWSER_MODEL_NAME", "fixture-readiness-model")
	profiles, processes := hermesSessionProfiles(t), hermesReadinessProcesses(t)
	legacy, err := filepath.Glob(filepath.Join(os.TempDir(), "commander-browser-session-*"))
	hermesSessionCheck(t, err)
	probe := &hermesReadinessLocal{}
	found, err := hermesruntime.Discover(t.Context(), probe, &environment.DiscoveryScope{Paths: []string{"/opt/hermes/.venv/bin"}})
	if err != nil || found.Source() != "/opt/hermes" || found.Python() != "/opt/hermes/.venv/bin/python" || probe.finds != 0 || len(probe.calls) != 1 ||
		!reflect.DeepEqual(probe.lookups, []string{"HERMES_INSTALL_ROOT"}) || probe.calls[0][0] != "/opt/hermes/.venv/bin/python" {
		t.Fatal("real pinned Hermes discovery lost its packaged virtual environment")
	}
	var actual, expected map[string]string
	if json.Unmarshal([]byte(probe.result.Stdout), &actual) != nil || json.Unmarshal([]byte(hermesReadyReceipt), &expected) != nil || !reflect.DeepEqual(actual, expected) ||
		probe.result.Stderr != "" || strings.Contains(probe.result.Stdout, canary) || modelRequests.Load() != 0 {
		t.Fatal("readiness was noisy, exposed model configuration or made a model request")
	}
	entries, err := os.ReadDir(home)
	hermesSessionCheck(t, err)
	marker, err := os.ReadFile(filepath.Join(home, "sentinel"))
	hermesSessionCheck(t, err)
	if len(entries) != 1 || string(marker) != "untouched" || !reflect.DeepEqual(profiles, hermesSessionProfiles(t)) {
		t.Fatal("offline discovery created a session or changed the caller's runtime home")
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "commander-browser-session-*"))
	hermesSessionCheck(t, err)
	if !reflect.DeepEqual(legacy, after) {
		t.Fatal("offline Hermes discovery created a browser profile")
	}
	for pid, name := range hermesReadinessProcesses(t) {
		if processes[pid] != name {
			t.Fatal("offline Hermes discovery left a Python or browser descendant")
		}
	}
}

func TestHermesReadinessRefusesCaughtImportActivity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("packaged Hermes integration requires the Linux runtime image")
	}
	var connections, requests atomic.Int32
	endpoint := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	endpoint.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	endpoint.Start()
	t.Cleanup(endpoint.Close)
	manifest, err := os.ReadFile("/opt/hermes/commander-runtime.json")
	hermesSessionCheck(t, err)
	const private = "fixture-private-readiness-import-output"
	t.Setenv("OPENAI_API_KEY", private)
	t.Setenv("OPENAI_BASE_URL", endpoint.URL+"/v1")
	for _, test := range []struct {
		name, attempt string
	}{
		{name: "network", attempt: fmt.Sprintf("import socket\nwith socket.socket() as connection:\n    try:\n        connection.connect(('127.0.0.1', %d))\n    except Exception:\n        pass\n", endpoint.Listener.Addr().(*net.TCPAddr).Port)},
		{name: "subprocess", attempt: "import subprocess, sys\ntry:\n    subprocess.run([sys.executable, '-I', '-S', '-c', 'pass'], check=True)\nexcept Exception:\n    pass\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// This is an owned synthetic source fixture. The packaged upstream
			// source and virtual environment remain untouched.
			source := t.TempDir()
			if os.Symlink("/opt/hermes/.venv", filepath.Join(source, ".venv")) != nil || os.WriteFile(filepath.Join(source, "commander-runtime.json"), manifest, 0600) != nil {
				t.Fatal("cannot prepare the isolated import fixture")
			}
			module := `import os
from pathlib import Path
home = Path(os.environ['HOME'])
assert Path.cwd() == home and Path(os.environ['HERMES_HOME']) == home
assert home.name.startswith('commander-hermes-readiness-')
assert 'OPENAI_API_KEY' not in os.environ and 'OPENAI_BASE_URL' not in os.environ
print('fixture-private-readiness-import-output')
os.write(2, b'fixture-private-readiness-import-output')
` + test.attempt
			if os.WriteFile(filepath.Join(source, "run_agent.py"), []byte(module), 0600) != nil {
				t.Fatal("cannot prepare the guarded import activity")
			}
			t.Setenv("HERMES_INSTALL_ROOT", source)
			profiles, processes := hermesSessionProfiles(t), hermesReadinessProcesses(t)
			probe := &hermesReadinessLocal{}
			found, err := hermesruntime.Discover(t.Context(), probe, &environment.DiscoveryScope{Paths: []string{filepath.Join(source, ".venv", "bin")}})
			var unavailable *hermesruntime.UnavailableError
			var receipt map[string]string
			if !errors.As(err, &unavailable) || found.Source() != "" || found.Python() != "" || len(probe.calls) != 1 || probe.calls[0][0] != filepath.Join(source, ".venv", "bin", "python") ||
				json.Unmarshal([]byte(probe.result.Stdout), &receipt) != nil || len(receipt) != 3 || receipt["status"] != "unavailable" || receipt["reason"] != "runtime_activity_attempted" || receipt["remedy"] == "" || probe.result.ReturnCode != 1 {
				t.Fatal("a caught import activity attempt bypassed the readiness guard")
			}
			if probe.result.Stderr != "" || strings.Contains(probe.result.Stdout, private) || strings.Contains(err.Error(), private) || strings.Contains(err.Error(), source) || connections.Load() != 0 || requests.Load() != 0 ||
				!reflect.DeepEqual(profiles, hermesSessionProfiles(t)) {
				t.Fatal("guarded readiness leaked import output, made network traffic or retained a session")
			}
			for pid, name := range hermesReadinessProcesses(t) {
				if processes[pid] != name {
					t.Fatal("guarded readiness left an imported Python or browser child")
				}
			}
		})
	}
}

func hermesReadinessProcesses(t *testing.T) map[string]string {
	t.Helper()
	entries, err := filepath.Glob("/proc/[0-9]*/comm")
	hermesSessionCheck(t, err)
	processes := make(map[string]string)
	for _, entry := range entries {
		name, err := os.ReadFile(entry)
		if err != nil {
			continue // A process can exit while /proc is being inspected.
		}
		comm := strings.TrimSpace(string(name))
		if strings.HasPrefix(comm, "python") || strings.HasPrefix(comm, "lightpanda") {
			processes[filepath.Base(filepath.Dir(entry))] = comm
		}
	}
	return processes
}
