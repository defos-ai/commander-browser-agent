//go:build hermesintegration && hermescontainerintegration

package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander/framework/id"
)

func TestHermesContainerModelRelayProtectsNativeReasoningAndReuse(t *testing.T) {
	worker, config, api := newHermesContainerModelWorker(t, false)
	ctx, stop := context.WithTimeout(t.Context(), 75*time.Second)
	defer stop()
	container := hermesContainerModelReference(t, ctx, api, config)
	site := api.exec(t, ctx, container, hermesContainerModelSite)["url"]
	if !strings.HasPrefix(site, "http://127.0.0.1:") {
		t.Fatal("the isolated native fixture did not use a private loopback site")
	}
	firstKey, secondKey := "fixture-container-model-first-"+id.New(), "fixture-container-model-second-"+id.New()
	var firstCode, secondCode atomic.Value
	model := fakes.NewHermesModel(t, firstKey, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		switch call {
		case 1, 4:
			code := firstCode.Load().(string)
			if call == 4 {
				code = secondCode.Load().(string)
			}
			tool := fakes.HermesCall(fmt.Sprintf("relay-native-%d-", call)+firstKey, "browser_exec", map[string]any{"code": code})
			for _, key := range []string{firstKey, secondKey} {
				tool.Function.Arguments = strings.ReplaceAll(tool.Function.Arguments, key, fakes.HermesUnicode(key))
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{tool}}
		case 2:
			if !request.HasToolText("Isolated relay session established") || !request.HasToolText("PRIVATE_SETUP_CLEAR") {
				t.Error("native automation lost its observation or private setup audit")
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("relay-clarify", "clarify", map[string]any{
				"questions": []any{map[string]any{"question": "Which fixture detail should be confirmed?", "choices": []string{"Cookie continuity", "Page title"}}},
			})}}
		case 3:
			if !request.HasToolText("Cookie continuity") {
				t.Error("the private relay lost its bounded clarification answer")
			}
			return fakes.HermesResponse{Text: "Isolated relay completed. Provider echo " + firstKey, Escape: firstKey}
		case 5:
			if !request.HasToolText("Retained relay cookie confirmed") || !request.HasToolText("PRIVATE_SETUP_CLEAR") {
				t.Error("a rotated provider client lost the retained isolated browser")
			}
			return fakes.HermesResponse{Text: "Retained relay completed. Provider echoes " + firstKey + " " + secondKey, Escape: firstKey}
		default:
			t.Error("the isolated reasoning fixture exceeded its scripted requests")
			return fakes.HermesResponse{Text: "Unexpected fixture continuation."}
		}
	})
	endpoint := model.URL + "/chat/completions"
	audit := hermesContainerModelAudit(firstKey, secondKey, endpoint)
	firstCode.Store("# Provider echo " + firstKey + "\n" + audit + fmt.Sprintf("\nnew_tab(%q)\nprint(js('document.body.textContent'))", site+"/set"))
	secondCode.Store("# Provider echoes " + firstKey + " " + secondKey + "\n" + audit + fmt.Sprintf("\ngoto_url(%q)\nprint(js('document.body.textContent'))", site+"/read"))
	var admissions, questions atomic.Int32
	handler := func(_ context.Context, event hermes.Event) (hermes.Reply, error) {
		for _, key := range []string{firstKey, secondKey} {
			hermesEventPrivate(t, event, key)
		}
		switch event.Kind {
		case "admit":
			admissions.Add(1)
			return hermes.Reply{Approved: true}, nil
		case "question":
			questions.Add(1)
			return hermes.Reply{Answer: "Cookie continuity"}, nil
		}
		return hermes.Reply{}, nil
	}
	request := hermesRequest(model, []byte(firstKey))
	request.Provider = "other"
	defer clear(request.APIKey)
	first, err := worker.Run(ctx, request, handler)
	if err != nil || first.Status != "completed" || first.ModelRequests != 3 || model.Calls() != 3 || admissions.Load() != 1 || questions.Load() != 1 ||
		!strings.Contains(first.Text, "Isolated relay completed") || !strings.Contains(first.Text, "[REDACTED]") {
		t.Fatal("isolated Hermes did not complete its protected native reasoning workflow")
	}
	hermesResultPrivate(t, first, firstKey, endpoint)
	model.RotateKey(secondKey)
	rotated := hermesRequest(model, []byte(secondKey))
	rotated.Provider = "other"
	defer clear(rotated.APIKey)
	second, err := worker.Run(ctx, rotated, handler)
	if err != nil || second.Status != "completed" || second.ModelRequests != 2 || model.Calls() != 5 || admissions.Load() != 2 || questions.Load() != 1 ||
		!strings.Contains(second.Text, "Retained relay completed") {
		t.Fatal("an isolated worker did not retain its browser across provider-key rotation")
	}
	for _, key := range []string{firstKey, secondKey} {
		hermesResultPrivate(t, second, key, endpoint)
	}
	var inspection struct {
		Config struct{ Env, Cmd, Entrypoint []string }
	}
	api.json(t, ctx, http.MethodGet, "/containers/"+container+"/json", nil, &inspection)
	encoded, _ := json.Marshal(inspection)
	for _, private := range []string{firstKey, secondKey, endpoint} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("model configuration or a real provider key entered the worker's creation settings")
		}
	}
	// The source page is live, but its loopback CDP endpoint belongs to another
	// network namespace. Rendering must not try the old host-loopback fallback.
	err = hermes.WithWorker(ctx, worker, func(ctx context.Context) error {
		return hermes.WithExistingBrowser(ctx, func(browser *hermes.Browser) error {
			if _, err := browser.RenderText(ctx, "Protected fixture", 1280, 720); !errors.Is(err, hermes.ErrUnavailable) {
				t.Error("an isolated browser attempted rendering before its private renderer relay")
			}
			return nil
		})
	})
	if err != nil || worker.Verify(ctx) != nil || model.Calls() != 5 {
		t.Fatal("the unsupported renderer path changed the retained isolated session")
	}
}

func TestHermesContainerModelRelayCancellationJoinsBeforeReuse(t *testing.T) {
	worker, _, _ := newHermesContainerModelWorker(t, false)
	key := "fixture-container-model-cancel-" + id.New()
	blocked := fakes.NewHermesModel(t, key, func(fakes.HermesRequest, int) fakes.HermesResponse {
		return fakes.HermesResponse{Block: true}
	})
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause := errors.New("fixture isolated model cancellation")
	request := hermesRequest(blocked, []byte(key))
	request.Provider = "other"
	defer clear(request.APIKey)
	done := make(chan error, 1)
	go func() { _, err := worker.Run(ctx, request, nil); done <- err }()
	select {
	case <-blocked.Blocked:
	case <-time.After(25 * time.Second):
		t.Fatal("the actual isolated model request did not reach the host provider")
	}
	cancel(cause)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
			t.Fatal("isolated model cancellation lost its caller cause")
		}
	case <-time.After(12 * time.Second):
		t.Fatal("isolated model cancellation did not join its active request")
	}
	select {
	case <-blocked.Disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider request survived the isolated cancellation receipt")
	}
	fresh, stop := context.WithTimeout(t.Context(), 25*time.Second)
	defer stop()
	if !errors.Is(worker.Verify(fresh), hermes.ErrNotConnected) {
		t.Fatal("clean model cancellation replaced or lost the unopened browser resource")
	}
	otherKey := "fixture-container-model-after-" + id.New()
	model := fakes.NewHermesModel(t, otherKey, func(fakes.HermesRequest, int) fakes.HermesResponse {
		return fakes.HermesResponse{Text: "Isolated cancellation reuse completed. Old echo " + key, Escape: key}
	}, key)
	next := hermesRequest(model, []byte(otherKey))
	next.Provider = "other"
	defer clear(next.APIKey)
	result, err := worker.Run(fresh, next, nil)
	if err != nil || result.Status != "completed" || model.Calls() != 1 || blocked.Calls() != 1 || !strings.Contains(result.Text, "[REDACTED]") {
		t.Fatal("the isolated worker did not reuse cleanly after joined model cancellation")
	}
	hermesResultPrivate(t, result, key, blocked.URL)
}

func TestHermesContainerModelRelayRejectsOversizedProviderReply(t *testing.T) {
	worker, _, _ := newHermesContainerModelWorker(t, true)
	key := "fixture-container-model-bound-" + id.New()
	marker := "fixture-oversized-reply-" + id.New()
	model := fakes.NewHermesModel(t, key, func(fakes.HermesRequest, int) fakes.HermesResponse {
		// Above the isolated relay's 512 KiB bound, below the local transport's
		// 1 MiB response bound. This response-only marker must never be replayed.
		return fakes.HermesResponse{Text: marker + strings.Repeat("x", 600<<10)}
	}, marker)
	request := hermesRequest(model, []byte(key))
	request.Provider, request.MaxIterations, request.TimeoutSeconds = "other", 1, 10
	defer clear(request.APIKey)
	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	result, err := worker.Run(ctx, request, func(context.Context, hermes.Event) (hermes.Reply, error) {
		t.Error("an oversized model reply reached native admission or a public callback")
		return hermes.Reply{}, nil
	})
	if err == nil && result.Status == "completed" || model.Calls() < 1 || model.Calls() > request.MaxIterations+1 || strings.Contains(result.Text, marker) {
		t.Fatal("an oversized isolated model reply escaped its bounded failure path")
	}
	hermesResultPrivate(t, result, key, model.URL)
}

func newHermesContainerModelWorker(t *testing.T, allowUncertain bool) (*hermes.Worker, hermes.ContainerConfig, hermesDockerProbe) {
	t.Helper()
	config := hermes.ContainerConfig{Socket: os.Getenv("HERMES_CONTAINER_TEST_SOCKET"), Image: os.Getenv("HERMES_CONTAINER_TEST_IMAGE"), Claim: hermesConfig().Claim}
	if config.Socket == "" || config.Image == "" {
		t.Fatal("container model probes require an explicit daemon socket and pinned image ID")
	}
	api := newHermesDockerProbe(config.Socket)
	t.Cleanup(api.client.CloseIdleConnections)
	var worker *hermes.Worker
	t.Cleanup(func() {
		if worker != nil {
			if err := worker.Close(); err != nil && !(allowUncertain && errors.Is(err, hermes.ErrUncertain)) {
				t.Error("isolated model worker close was not confirmed")
			}
		}
		// A failed test may deliberately leave an uncertain runtime. Only this
		// exact newly allocated fixture claim is eligible for forced cleanup.
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for _, container := range hermesContainerModelReferences(t, cleanup, api, config) {
			var state struct {
				State  struct{ Running bool }
				Mounts []struct{ Type, Name string }
			}
			api.json(t, cleanup, http.MethodGet, "/containers/"+container+"/json", nil, &state)
			if state.State.Running {
				_ = api.request(t, cleanup, http.MethodPost, "/containers/"+container+"/kill?signal=KILL", nil).Body.Close()
			}
			var receipt struct{ StatusCode int }
			api.json(t, cleanup, http.MethodPost, "/containers/"+container+"/wait?condition=not-running", nil, &receipt)
			_ = api.request(t, cleanup, http.MethodDelete, "/containers/"+container+"?force=false&v=true", nil).Body.Close()
			for _, mount := range state.Mounts {
				if mount.Type == "volume" {
					_ = api.request(t, cleanup, http.MethodGet, "/volumes/"+mount.Name, nil, http.StatusNotFound).Body.Close()
				}
			}
		}
	})
	ctx, stop := context.WithTimeout(t.Context(), 40*time.Second)
	defer stop()
	var err error
	worker, err = hermes.StartContainer(ctx, config)
	if err != nil || worker == nil {
		t.Fatal("the isolated model worker did not become ready")
	}
	return worker, config, api
}

func hermesContainerModelReference(t *testing.T, ctx context.Context, api hermesDockerProbe, config hermes.ContainerConfig) string {
	t.Helper()
	references := hermesContainerModelReferences(t, ctx, api, config)
	if len(references) != 1 {
		t.Fatal("the isolated model fixture lost its exact container identity")
	}
	return references[0]
}

func hermesContainerModelReferences(t *testing.T, ctx context.Context, api hermesDockerProbe, config hermes.ContainerConfig) []string {
	t.Helper()
	filters, _ := json.Marshal(map[string][]string{"label": {"io.defos.commander.browser.worker=hermes", "io.defos.commander.browser.session=" + config.Claim.ID}})
	var found []struct{ ID, ImageID string }
	api.json(t, ctx, http.MethodGet, "/containers/json?all=1&filters="+url.QueryEscape(string(filters)), nil, &found)
	references := make([]string, 0, len(found))
	for _, container := range found {
		if len(container.ID) != 64 || container.ImageID != config.Image {
			t.Fatal("the isolated model fixture encountered a foreign container")
		}
		references = append(references, container.ID)
	}
	return references
}

// Real credentials and endpoints never enter the worker through this audit;
// only hashes of synthetic values and their common encoded forms are sent.
func hermesContainerModelAudit(values ...string) string {
	hashes := make([]string, 0, len(values)*3)
	for _, value := range values {
		for _, candidate := range []string{value, base64.RawStdEncoding.EncodeToString([]byte(value)), fakes.HermesUnicode(value)} {
			hashes = append(hashes, fmt.Sprintf("%x", sha256.Sum256([]byte(candidate))))
		}
	}
	encoded, _ := json.Marshal(hashes)
	return fmt.Sprintf(`import hashlib, os, re
from pathlib import Path
private_hashes = set(%s)
def audit(value):
    for token in re.findall(rb"[A-Za-z0-9_.:/+%%-]{16,}|(?:\\u[0-9a-fA-F]{4})+", value):
        assert hashlib.sha256(token).hexdigest() not in private_hashes
for value in os.environ.values():
    audit(value.encode())
for path in Path(os.environ['HOME']).rglob('*'):
    if not path.is_symlink() and path.is_file() and path.stat().st_size <= 1048576:
        audit(path.read_bytes())
print('PRIVATE_SETUP_CLEAR')`, encoded)
}

const hermesContainerModelSite = `import http.server, json, socket, subprocess, sys, time
with socket.socket() as reservation:
    reservation.bind(('127.0.0.1', 0))
    port = reservation.getsockname()[1]
server = """import http.server, sys
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b'Isolated relay session established'
        self.send_response(200)
        self.send_header('Content-Type', 'text/html; charset=utf-8')
        if self.path == '/set':
            self.send_header('Set-Cookie', 'relay-session=retained; HttpOnly; Path=/')
        elif self.path == '/read':
            body = b'Retained relay cookie confirmed' if 'relay-session=retained' in self.headers.get('Cookie', '') else b'Relay cookie missing'
        self.end_headers()
        self.wfile.write(b'<title>Isolated study</title><p>' + body + b'</p>')
    def log_message(self, *_arguments):
        pass
http.server.HTTPServer(('127.0.0.1', int(sys.argv[1])), Handler).serve_forever()
"""
subprocess.Popen([sys.executable, '-I', '-S', '-c', server, str(port)], start_new_session=True,
                 stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
for attempt in range(100):
    try:
        with socket.create_connection(('127.0.0.1', port), timeout=.1):
            break
    except OSError:
        time.sleep(.02)
else:
    raise RuntimeError('fixture_site_unavailable')
print(json.dumps({'url': 'http://127.0.0.1:' + str(port)}))
`
