//go:build hermescontainerintegration

package browser_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/hermes/containers"
	"github.com/defos-ai/commander/framework/id"
)

// This probe deliberately uses a Docker-capable test parent. Worker containers
// receive no daemon mount, host paths or ambient environment. The parent and two
// children each have their own 1 CPU / 1 GB limit, supplied by setup and Launch.
func TestHermesContainersIsolateWorkersAndOwnNamespaceLifetime(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container isolation probes run in the Linux test host")
	}
	socket, image := os.Getenv("HERMES_CONTAINER_TEST_SOCKET"), os.Getenv("HERMES_CONTAINER_TEST_IMAGE")
	if socket == "" || image == "" {
		t.Fatal("container probes require an explicit test daemon socket and pinned image ID")
	}
	ctx, stop := context.WithTimeout(t.Context(), 2*time.Minute)
	defer stop()
	api := newHermesDockerProbe(socket)
	t.Cleanup(api.client.CloseIdleConnections)
	canary := "fixture-parent-private-" + id.New()
	parentFile := t.TempDir() + "/private-marker"
	if os.WriteFile(parentFile, []byte(canary), 0600) != nil {
		t.Fatal("cannot prepare the host-only file marker")
	}
	// Initial argv/environment bytes are observable through /proc. Only their
	// hash crosses into a child probe, avoiding a self-matching assertion.
	parent := exec.CommandContext(ctx, "/opt/hermes/.venv/bin/python", "-I", "-S", "-c", "import time; time.sleep(90)", canary)
	parent.Env = []string{"HERMES_CONTAINER_PARENT_MARKER=" + canary}
	parent.Stdout, parent.Stderr = io.Discard, io.Discard
	if parent.Start() != nil {
		t.Fatal("cannot prepare the host-only process marker")
	}
	t.Cleanup(func() { _ = parent.Process.Kill(); _ = parent.Wait() })
	t.Setenv("HERMES_CONTAINER_PARENT_MARKER", canary)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot prepare the host-only loopback listener")
	}
	t.Cleanup(func() { _ = listener.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	parentPID, _ := os.Readlink("/proc/self/ns/pid")
	parentNet, _ := os.Readlink("/proc/self/ns/net")
	if parentPID == "" || parentNet == "" {
		t.Fatal("cannot identify the test parent's namespaces")
	}

	workers := make([]*hermesContainerProbe, 2)
	namespaces := make([]map[string]string, 2)
	volumes := make([]string, 2)
	for index := range workers {
		instance, err := containers.Launch(ctx, containers.Config{Socket: socket, Image: image, Session: id.New()})
		var worker *hermesContainerProbe
		if instance != nil {
			worker = &hermesContainerProbe{instance: instance, reader: bufio.NewReaderSize(instance.Output(), 4096)}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_ = instance.CloseIO()
				if !worker.removed && (instance.Kill(cleanup) != nil || instance.Remove(cleanup) != nil) {
					t.Error("isolated Hermes cleanup was not confirmed")
				}
			})
		}
		if err != nil || worker == nil {
			t.Fatal("the pinned isolated Hermes container did not launch")
		}
		workers[index] = worker
		worker.expect(t, ctx, "ready", "", map[string]any{"hermes": "v0.23.1"})
		var inspection struct {
			Config struct {
				User                 string
				Env, Cmd, Entrypoint []string
			}
			Mounts []struct {
				Type, Name, Destination string
				RW                      bool
			}
			HostConfig struct {
				NetworkMode, PidMode, IpcMode               string
				ReadonlyRootfs, Privileged, PublishAllPorts bool
				NanoCpus, Memory, MemorySwap, PidsLimit     int64
				CapDrop, SecurityOpt, Binds                 []string
				PortBindings                                map[string]json.RawMessage
			}
		}
		api.json(t, ctx, "GET", "/containers/"+instance.ID()+"/json", nil, &inspection)
		policy := inspection.HostConfig
		if inspection.Config.User != "1000:1000" || !policy.ReadonlyRootfs || policy.Privileged || policy.PublishAllPorts ||
			policy.NetworkMode != "bridge" || policy.PidMode != "" || policy.IpcMode != "private" ||
			policy.NanoCpus != 1_000_000_000 || policy.Memory != 1_000_000_000 || policy.MemorySwap != policy.Memory || policy.PidsLimit != 128 ||
			!hermesProbeContains(policy.CapDrop, "ALL") || !hermesProbeContains(policy.SecurityOpt, "no-new-privileges") ||
			len(policy.Binds) != 0 || len(inspection.Mounts) > 2 || len(policy.PortBindings) != 0 {
			t.Fatal("Hermes worker did not retain its fixed container isolation policy")
		}
		for _, mount := range inspection.Mounts {
			if mount.Type == "tmpfs" && mount.Destination == "/tmp" && mount.RW {
				continue
			}
			if mount.Type != "volume" || mount.Destination != "/home/commander" || !mount.RW || len(mount.Name) != 64 || volumes[index] != "" {
				t.Fatal("the worker home is not one owned private Docker volume")
			}
			volumes[index] = mount.Name
		}
		if volumes[index] == "" {
			t.Fatal("the worker has no retained private home volume")
		}
		configuration, _ := json.Marshal(inspection.Config)
		if bytes.Contains(configuration, []byte(canary)) {
			t.Fatal("the worker inherited private host argv or environment")
		}
		marker := fmt.Sprintf("worker-%d", index)
		namespaces[index] = api.exec(t, ctx, instance.ID(), hermesContainerIsolationScript(parentFile, socket, marker, fmt.Sprintf("%x", sha256.Sum256([]byte(canary))), port))
		if namespaces[index]["marker"] != marker || namespaces[index]["pid"] == parentPID || namespaces[index]["net"] == parentNet {
			t.Fatal("the worker reached host state or reused the parent's namespaces")
		}
		verify := id.New()
		worker.send(t, "verify", verify)
		worker.expect(t, ctx, "health", verify, map[string]any{"state": "starting"})
	}
	if namespaces[0]["pid"] == namespaces[1]["pid"] || namespaces[0]["net"] == namespaces[1]["net"] || volumes[0] == volumes[1] {
		t.Fatal("independent workers share process or network namespaces")
	}
	retained := api.exec(t, ctx, workers[0].instance.ID(), "from pathlib import Path\nimport json\nprint(json.dumps({'marker': Path('/home/commander/commander-isolation-marker').read_text()}))")
	if retained["marker"] != "worker-0" {
		t.Fatal("one worker overwrote another worker's private files")
	}
	parentBytes, err := os.ReadFile(parentFile)
	if err != nil || string(parentBytes) != canary || workers[1].instance.Remove(ctx) == nil {
		t.Fatal("workers changed host state or allowed removal before confirmed stop")
	}
	closed := id.New()
	workers[0].send(t, "close", closed)
	workers[0].expect(t, ctx, "closed", closed, map[string]any{"clean": true})
	if code, err := workers[0].instance.Wait(ctx); err != nil || code != 0 || workers[0].instance.Remove(ctx) != nil {
		t.Fatal("clean worker close did not confirm exit and removal")
	}
	workers[0].removed = true
	_ = api.request(t, ctx, "GET", "/volumes/"+volumes[0], nil, http.StatusNotFound).Body.Close()
	// Each probe left a detached loopback server. Killing the other container's
	// init owns that process too, regardless of its session/process group.
	if workers[1].instance.Kill(ctx) != nil {
		t.Fatal("forced namespace teardown was not confirmed")
	}
	if code, err := workers[1].instance.Wait(ctx); err != nil || code != 137 {
		t.Fatal("forced teardown did not join detached work")
	}
	var stopped struct{ State struct{ Running bool } }
	api.json(t, ctx, "GET", "/containers/"+workers[1].instance.ID()+"/json", nil, &stopped)
	var volume struct{ Name string }
	api.json(t, ctx, "GET", "/volumes/"+volumes[1], nil, &volume)
	if stopped.State.Running || volume.Name != volumes[1] || workers[1].instance.Remove(ctx) != nil {
		t.Fatal("forced stop failed to retain private state until explicit removal")
	}
	workers[1].removed = true
	_ = api.request(t, ctx, "GET", "/volumes/"+volumes[1], nil, http.StatusNotFound).Body.Close()
}

func hermesProbeContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value || candidate == value+":true" {
			return true
		}
	}
	return false
}

type hermesContainerProbe struct {
	instance *containers.Instance
	reader   *bufio.Reader
	removed  bool
}

func (probe *hermesContainerProbe) send(t *testing.T, kind, identifier string) {
	t.Helper()
	if json.NewEncoder(probe.instance.Input()).Encode(map[string]any{"version": 1, "kind": kind, "id": identifier, "payload": map[string]any{}}) != nil {
		t.Fatal("cannot send the bounded worker control frame")
	}
}

func (probe *hermesContainerProbe) expect(t *testing.T, ctx context.Context, kind, identifier string, payload map[string]any) {
	t.Helper()
	type outcome struct {
		data []byte
		err  error
	}
	read := make(chan outcome, 1)
	go func() { data, err := probe.reader.ReadSlice('\n'); read <- outcome{data, err} }()
	select {
	case result := <-read:
		var frame struct {
			Version  int
			Kind, ID string
			Payload  map[string]any
		}
		if result.err != nil || len(result.data) > 4096 || json.Unmarshal(result.data, &frame) != nil || frame.Version != 1 || frame.Kind != kind || frame.ID != identifier {
			t.Fatal("isolated worker control protocol lost its bounded correlation")
		}
		actual, _ := json.Marshal(frame.Payload)
		expected, _ := json.Marshal(payload)
		if !bytes.Equal(actual, expected) {
			t.Fatal("isolated worker returned unexpected readiness or lifecycle metadata")
		}
	case <-ctx.Done():
		_ = probe.instance.CloseIO()
		t.Fatal("isolated worker control did not finish within the test bound")
	}
}

type hermesDockerProbe struct{ client *http.Client }

func newHermesDockerProbe(socket string) hermesDockerProbe {
	return hermesDockerProbe{client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

func (api hermesDockerProbe) request(t *testing.T, ctx context.Context, method, path string, input any, expected ...int) *http.Response {
	t.Helper()
	body, _ := json.Marshal(input)
	request, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.41"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal("cannot prepare the test Docker API request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := api.client.Do(request)
	if err != nil {
		t.Fatal("test Docker API transport failed")
	}
	if len(expected) == 0 && (response.StatusCode < 200 || response.StatusCode >= 300) || len(expected) == 1 && response.StatusCode != expected[0] {
		_ = response.Body.Close()
		t.Fatal("test Docker API request was refused")
	}
	return response
}

func (api hermesDockerProbe) json(t *testing.T, ctx context.Context, method, path string, input, output any) {
	t.Helper()
	response := api.request(t, ctx, method, path, input)
	defer response.Body.Close()
	if json.NewDecoder(io.LimitReader(response.Body, 256<<10)).Decode(output) != nil {
		t.Fatal("test Docker API metadata was invalid")
	}
}

func (api hermesDockerProbe) exec(t *testing.T, ctx context.Context, container, script string) map[string]string {
	t.Helper()
	var created struct{ ID string }
	api.json(t, ctx, "POST", "/containers/"+container+"/exec", map[string]any{
		"AttachStdout": true, "AttachStderr": true, "User": "1000:1000", "Tty": false,
		"Cmd": []string{"/opt/hermes/.venv/bin/python", "-I", "-c", script},
	}, &created)
	if len(created.ID) != 64 {
		t.Fatal("test Docker exec returned invalid identity")
	}
	response := api.request(t, ctx, "POST", "/exec/"+created.ID+"/start", map[string]any{"Detach": false, "Tty": false})
	defer response.Body.Close()
	var stdout bytes.Buffer
	for stdout.Len() <= 65536 {
		var header [8]byte
		_, err := io.ReadFull(response.Body, header[:])
		if errors.Is(err, io.EOF) {
			break
		}
		length := binary.BigEndian.Uint32(header[4:])
		if err != nil || header[0] != 1 || length > 65536 || stdout.Len()+int(length) > 65536 {
			t.Fatal("test Docker exec produced unbounded or noisy output")
		}
		if _, err := io.CopyN(&stdout, response.Body, int64(length)); err != nil {
			t.Fatal("test Docker exec output ended unexpectedly")
		}
	}
	var status struct {
		Running  bool
		ExitCode int
	}
	api.json(t, ctx, "GET", "/exec/"+created.ID+"/json", nil, &status)
	var output map[string]string
	if status.Running || status.ExitCode != 0 || json.Unmarshal(stdout.Bytes(), &output) != nil {
		t.Fatal("the isolated worker state probe failed")
	}
	return output
}

func hermesContainerIsolationScript(parentFile, socket, marker, digest string, port int) string {
	server := fmt.Sprintf("import socket\ns=socket.socket()\ns.bind(('127.0.0.1', %d))\ns.listen()\nwhile True:\n c,_=s.accept(); c.sendall(%q.encode()); c.close()\n", port, marker)
	return fmt.Sprintf(`import hashlib, json, os, socket, subprocess, sys, time
from pathlib import Path
assert not Path(%q).exists()
assert not Path(%q).exists() and not Path('/var/run/docker.sock').exists()
assert 'HERMES_CONTAINER_PARENT_MARKER' not in os.environ
for process in Path('/proc').glob('[0-9]*'):
    for field in ('cmdline', 'environ'):
        try:
            values = (process / field).read_bytes().split(b'\0')
        except (OSError, PermissionError):
            continue
        for value in values:
            candidates = (value, value.split(b'=', 1)[-1])
            assert all(hashlib.sha256(item).hexdigest() != %q for item in candidates)
path = Path('/home/commander/commander-isolation-marker')
assert not path.exists()
path.write_text(%q)
child = subprocess.Popen([sys.executable, '-I', '-c', %q], start_new_session=True,
                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
assert os.getsid(child.pid) == child.pid
for attempt in range(100):
    try:
        with socket.create_connection(('127.0.0.1', %d), timeout=.1) as connection:
            assert connection.recv(64).decode() == %q
        break
    except OSError:
        time.sleep(.02)
else:
    raise RuntimeError('private_listener_unavailable')
print(json.dumps({'pid': os.readlink('/proc/self/ns/pid'), 'net': os.readlink('/proc/self/ns/net'), 'marker': path.read_text()}))
`, parentFile, socket, digest, marker, server, port, marker)
}
