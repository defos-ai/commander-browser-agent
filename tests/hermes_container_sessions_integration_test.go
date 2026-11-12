//go:build hermesintegration && hermescontainerintegration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

func TestHermesContainerSessionsDispatchReuseIsolationAndClose(t *testing.T) {
	options := browserruntime.HermesOptions{Container: browserruntime.HermesContainerOptions{
		Socket: os.Getenv("HERMES_CONTAINER_TEST_SOCKET"), Image: os.Getenv("HERMES_CONTAINER_TEST_IMAGE"),
	}}
	if options.Container.Socket == "" || options.Container.Image == "" {
		t.Fatal("isolated session probes require an explicit daemon socket and pinned image ID")
	}
	api := newHermesDockerProbe(options.Container.Socket)
	t.Cleanup(api.client.CloseIdleConnections)
	var h *hermesSessionHost
	t.Cleanup(func() {
		if h == nil {
			return
		}
		for _, claim := range h.claims {
			cleanupHermesContainerSession(t, api, hermes.ContainerConfig{Socket: options.Container.Socket, Image: options.Container.Image, Claim: claim})
		}
	})
	var firstCode, reuseCode, freshCode atomic.Value
	firstModel, first := hermesContainerSessionScript(t, &firstCode, "Isolated relay session established")
	reuseModel, reuse := hermesContainerSessionScript(t, &reuseCode, "Retained relay cookie confirmed")
	freshModel, fresh := hermesContainerSessionScript(t, &freshCode, "Relay cookie missing")
	blockedKey := "fixture-container-session-wait-" + id.New()
	blocked := fakes.NewHermesModel(t, blockedKey, func(fakes.HermesRequest, int) fakes.HermesResponse {
		return fakes.HermesResponse{Block: true}
	})
	wait := hermesRequest(blocked, []byte(blockedKey))
	wait.Provider = "other"
	cancel, cancelled := make(chan struct{}), make(chan error, 1)
	scope := make(chan context.Context, 1)
	h = newHermesSessionHostWithOptions(t, map[string]hermesSessionScenario{
		"prepare": {prepareOnly: true}, "first": {request: first, scope: scope},
		"reuse": {request: reuse}, "fresh": {request: fresh}, "wait": {request: wait, cancel: cancel, result: cancelled},
	}, options)
	ctx, stop := context.WithTimeout(t.Context(), 75*time.Second)
	defer stop()
	root := h.task(nil, sessions.Open, "prepare")
	prepared := h.call(root)
	binding := h.binding(root)
	if prepared.Session.State != "starting" || firstModel.Calls() != 0 {
		t.Fatal("an isolated worker was advertised as connected before opening its browser")
	}
	unready := h.task(&root, sessions.Use, "first")
	unreadyBinding := h.binding(unready)
	beforeEntered := h.entered.Load()
	if _, ok := h.finish(h.dispatch(unready, &unreadyBinding)); ok || h.entered.Load() != beforeEntered || firstModel.Calls() != 0 {
		t.Fatal("a use action reached an isolated worker before its browser connected")
	}
	config := hermes.ContainerConfig{Socket: options.Container.Socket, Image: options.Container.Image, Claim: binding.Claim()}
	container := hermesContainerModelReference(t, ctx, api, config)
	site := api.exec(t, ctx, container, hermesContainerModelSite)["url"]
	address, err := url.Parse(site)
	if err != nil || address.Hostname() != "127.0.0.1" {
		t.Fatal("the isolated session fixture did not use its private loopback site")
	}
	port, err := strconv.Atoi(address.Port())
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("the isolated session fixture did not expose a valid private port")
	}
	firstCode.Store(fmt.Sprintf("new_tab(%q)\nprint(js('document.body.textContent'))", site+"/set"))
	reuseCode.Store(fmt.Sprintf("goto_url(%q)\nprint(js('document.body.textContent'))", site+"/read"))
	freshCode.Store(fmt.Sprintf("new_tab(%q)\nprint(js('document.body.textContent'))", site+"/read"))
	firstTask := h.task(&root, sessions.Open, "first")
	opened := h.call(firstTask)
	if opened.Claim != prepared.Claim || opened.Session.Reference != binding.Reference || opened.Session.State != "connected" || firstModel.Calls() != 2 {
		t.Fatal("a direct-parent open changed its isolated worker or original creation claim")
	}
	if _, err := hermes.Run(<-scope, first, nil); err == nil || firstModel.Calls() != 2 {
		t.Fatal("an isolated worker accepted work through an expired callback grant")
	}
	independent := h.task(nil, sessions.Open, "prepare")
	freshPrepared := h.call(independent)
	freshBinding := h.binding(independent)
	otherConfig := hermes.ContainerConfig{Socket: options.Container.Socket, Image: options.Container.Image, Claim: freshBinding.Claim()}
	otherContainer := hermesContainerModelReference(t, ctx, api, otherConfig)
	// The same canonical origin runs independently inside each namespace. A
	// fresh root must not receive the first worker's HttpOnly cookie.
	otherSite := api.exec(t, ctx, otherContainer, strings.Replace(hermesContainerModelSite,
		"reservation.bind(('127.0.0.1', 0))", fmt.Sprintf("reservation.bind(('127.0.0.1', %d))", port), 1))["url"]
	if container == otherContainer || site != otherSite || freshPrepared.Claim == prepared.Claim {
		t.Fatal("independent Commander roots did not retain separate isolated resources")
	}
	freshResult := h.call(h.task(&independent, sessions.Open, "fresh"))
	if freshResult.Claim != freshPrepared.Claim || freshModel.Calls() != 2 {
		t.Fatal("the independent isolated browser did not preserve its own creation identity")
	}
	child := h.task(&firstTask, sessions.Use, "reuse")
	if continued := h.call(child); continued.Claim != opened.Claim || reuseModel.Calls() != 2 {
		t.Fatal("a use continuation lost its authenticated isolated browser")
	}
	before := h.snapshot(binding.Reference)
	if !h.control(binding, "inspect") || !h.control(binding, "verify") {
		t.Fatal("Commander could not inspect or verify its isolated browser")
	}
	after := h.snapshot(binding.Reference)
	if after.State != "connected" || after.Reference != before.Reference || after.Revision < before.Revision {
		t.Fatal("isolated health controls changed ownership or lost inventory revisions")
	}
	metadata, _ := json.Marshal(h.host.Describe().Sessions)
	for _, private := range []string{site, string(first.APIKey), blockedKey, options.Container.Socket, options.Container.Image, container, "ws://", "/home/commander"} {
		if bytes.Contains(metadata, []byte(private)) {
			t.Error("private isolated runtime data escaped through Commander inventory")
		}
	}
	for _, changed := range []browserruntime.HermesOptions{
		{Container: browserruntime.HermesContainerOptions{Socket: options.Container.Socket, Image: "sha256:" + strings.Repeat("f", 64)}},
		{Container: browserruntime.HermesContainerOptions{Socket: options.Container.Socket + ".different", Image: options.Container.Image}},
		hermesScreenshotOptions(),
	} {
		called := false
		if _, err := h.manager.RunHermes(action.WithSession(ctx, &binding), changed, func(context.Context) error { called = true; return nil }); err == nil || called {
			t.Fatal("an isolated creation claim switched its pinned image or execution mode")
		}
	}
	foreign := binding
	foreign.CommandID = id.New()
	called := false
	if _, err := h.manager.RunHermes(action.WithSession(ctx, &foreign), options, func(context.Context) error { called = true; return nil }); !errors.Is(err, sessions.ErrConflict) || called {
		t.Fatal("an isolated worker accepted a changed full creation claim")
	}
	waiting := h.task(&child, sessions.Open, "wait")
	waitingBinding := h.binding(waiting)
	command := h.dispatch(waiting, &waitingBinding)
	select {
	case <-blocked.Blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("the isolated session's cancellable provider request did not start")
	}
	close(cancel)
	if _, ok := h.finish(command); ok {
		t.Fatal("cancelled isolated session work reported success")
	}
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("isolated session cancellation lost its caller cause")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("isolated session cancellation did not join its action")
	}
	select {
	case <-blocked.Disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("the provider request survived the isolated session cancellation receipt")
	}
	if resumed := h.call(h.task(&waiting, sessions.Use, "reuse")); resumed.Claim != opened.Claim || reuseModel.Calls() != 4 || blocked.Calls() != 1 {
		t.Fatal("clean cancellation replaced the retained isolated browser")
	}
	for _, retained := range []sessions.Binding{binding, freshBinding} {
		if !h.control(retained, "close") || h.snapshot(retained.Reference).State != "closed" {
			t.Fatal("confirmed isolated close did not retain its Commander tombstone")
		}
		if len(hermesContainerModelReferences(t, ctx, api, hermes.ContainerConfig{Socket: options.Container.Socket, Image: options.Container.Image, Claim: retained.Claim()})) != 0 {
			t.Fatal("confirmed Commander close retained its isolated container")
		}
	}
	closedTask := h.task(&root, sessions.Open, "reuse")
	closedBinding := h.binding(closedTask)
	if _, ok := h.finish(h.dispatch(closedTask, &closedBinding)); ok || reuseModel.Calls() != 4 {
		t.Fatal("a closed isolated claim reopened or reached the model")
	}
}

func hermesContainerSessionScript(t *testing.T, code *atomic.Value, observation string) (*fakes.HermesModel, hermes.Request) {
	t.Helper()
	key := "fixture-container-session-" + id.New()
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		if call%2 == 1 {
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("isolated-session-step", "browser_exec", map[string]any{"code": code.Load().(string)})}}
		}
		if !request.HasToolText(observation) {
			t.Error("isolated session did not preserve its cookie observation")
		}
		return fakes.HermesResponse{Text: "Isolated session fixture completed."}
	})
	request := hermesRequest(model, []byte(key))
	request.Provider = "other"
	return model, request
}

// Cleanup runs after the owning manager joins. Only freshly allocated fixture
// claims are eligible for forced teardown if an assertion left uncertain state.
func cleanupHermesContainerSession(t *testing.T, api hermesDockerProbe, config hermes.ContainerConfig) {
	t.Helper()
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	for _, container := range hermesContainerModelReferences(t, ctx, api, config) {
		var state struct {
			State  struct{ Running bool }
			Mounts []struct{ Type, Name string }
		}
		api.json(t, ctx, http.MethodGet, "/containers/"+container+"/json", nil, &state)
		if state.State.Running {
			_ = api.request(t, ctx, http.MethodPost, "/containers/"+container+"/kill?signal=KILL", nil).Body.Close()
		}
		var receipt struct{ StatusCode int }
		api.json(t, ctx, http.MethodPost, "/containers/"+container+"/wait?condition=not-running", nil, &receipt)
		_ = api.request(t, ctx, http.MethodDelete, "/containers/"+container+"?force=false&v=true", nil).Body.Close()
		for _, mount := range state.Mounts {
			if mount.Type == "volume" {
				_ = api.request(t, ctx, http.MethodGet, "/volumes/"+mount.Name, nil, http.StatusNotFound).Body.Close()
			}
		}
	}
}
