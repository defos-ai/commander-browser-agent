//go:build hermesintegration

package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/actions"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	browserscreenshot "github.com/defos-ai/commander-browser-agent/browser/screenshot"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/artifacts"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/sessions"
	runtimeSessions "github.com/defos-ai/commander/framework/sessions"
)

// The migration uses a test-owned action until the production catalog cuts over.
// Returning bytes through Result exercises Commander's actual artifact upload.
type hermesScreenshot struct {
	*action.Base
	manager *browserruntime.Manager
	options browserruntime.HermesOptions
}

func newHermesScreenshot(manager *browserruntime.Manager) *hermesScreenshot {
	return &hermesScreenshot{Base: action.NewBase(actionmodel.Action{
		Name: "native-capture", Description: "Capture the retained native fixture.", SessionMode: "use",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"viewport":{"type":"string","enum":["desktop","mobile"]}}}`),
	}), manager: manager, options: hermesScreenshotOptions()}
}

func (capture *hermesScreenshot) Run(ctx context.Context, raw json.RawMessage) (action.Result, error) {
	if _, err := action.UseSession(ctx); err != nil {
		return action.Result{}, err
	}
	input := struct {
		Viewport string `json:"viewport"`
	}{Viewport: "desktop"}
	if err := action.DecodeInput(raw, &input); err != nil {
		return action.Result{}, err
	}
	width, height := 1280, 720
	if input.Viewport == "mobile" {
		width, height = 390, 844
	} else if input.Viewport != "desktop" {
		return action.Result{}, errors.New("unsupported native fixture viewport")
	}
	var image browserscreenshot.Image
	view, err := capture.manager.RunHermes(ctx, capture.options, func(ctx context.Context) error {
		var err error
		image, err = browserscreenshot.CaptureHermes(ctx, width, height)
		return err
	})
	if err != nil {
		return action.Result{}, err
	}
	metadata, err := json.Marshal(actions.ScreenshotOutput{Engine: browserruntime.Lightpanda, Renderer: browserscreenshot.Renderer,
		Viewport: input.Viewport, Width: width, Height: height, MediaType: "image/png", Truncated: image.Truncated})
	if err != nil {
		return action.Result{}, err
	}
	return action.Result{Session: &view, Output: metadata, Artifacts: []action.Artifact{{Name: "Native protected screenshot", Files: []artifacts.FileSource{
		{Name: "screenshot.png", MediaType: "image/png", Data: image.PNG},
		{Name: "screenshot.json", MediaType: "application/json", Data: metadata},
	}}}}, nil
}

func hermesScreenshotOptions() browserruntime.HermesOptions {
	config := hermesConfig()
	return browserruntime.HermesOptions{Python: config.Python, Source: config.Source, Executable: config.Lightpanda}
}

func (h *hermesSessionHost) screenshotTask(parent *operations.Task, viewport string) operations.Task {
	h.t.Helper()
	input, _ := json.Marshal(map[string]string{"viewport": viewport})
	next, err := runtimeSessions.Normalize(parent, []actionmodel.NextAction{{ActionName: "native-capture", Input: input}}, nil, h.definitions...)
	hermesSessionCheck(h.t, err)
	if parent == nil {
		h.t.Fatal("a native fixture screenshot needs a session-aware parent")
	}
	return operations.NewTask(parent.OperationID, parent.ID, next[0])
}

func (h *hermesSessionHost) screenshotResult(command messages.Message) (agent.ActionOutput, bool) {
	h.t.Helper()
	message := h.wait(func(m messages.Message) bool {
		return m.CorrelationID == command.ID && (m.Type == controllerevents.ActionSucceeded || m.Type == controllerevents.CommandFailed)
	})
	receipt, err := agentevents.NewResultAccepted(message)
	hermesSessionCheck(h.t, err)
	h.accepted[message.ID] = receipt
	hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), receipt))
	var output agent.ActionOutput
	if message.Type == controllerevents.ActionSucceeded {
		hermesSessionCheck(h.t, json.Unmarshal(message.Payload, &output))
	}
	return output, message.Type == controllerevents.ActionSucceeded
}

func (h *hermesSessionHost) screenshotDownload(output agent.ActionOutput, name string) []byte {
	h.t.Helper()
	for _, artifact := range output.Artifacts {
		for _, file := range artifact.Files {
			if file.Name == name {
				data, err := h.files.Get(h.t.Context(), artifact.ID, file)
				hermesSessionCheck(h.t, err)
				return data
			}
		}
	}
	h.t.Fatal("the native screenshot artifact file was not uploaded")
	return nil
}

func captureHermesScreenshot(t *testing.T, h *hermesSessionHost, parent operations.Task, viewport string) ([sha256.Size]byte, bool) {
	t.Helper()
	task := h.screenshotTask(&parent, viewport)
	binding := h.binding(task)
	before := hermesScreenshotState(t, h, binding)
	output, ok := h.screenshotResult(h.dispatch(task, &binding))
	if !ok || output.Session == nil || output.Session.Reference != binding.Reference || len(output.Artifacts) != 1 || len(output.Artifacts[0].Files) != 2 ||
		!bytes.Equal(output.Output, h.screenshotDownload(output, "screenshot.json")) || before != hermesScreenshotState(t, h, binding) {
		t.Fatal("native screenshot changed its retained source or lost artifact metadata")
	}
	var metadata actions.ScreenshotOutput
	if json.Unmarshal(output.Output, &metadata) != nil || metadata.Engine != browserruntime.Lightpanda || metadata.Renderer != browserscreenshot.Renderer || metadata.MediaType != "image/png" || metadata.Viewport != viewport {
		t.Fatal("native screenshot metadata did not identify its text renderer")
	}
	width, height := 1280, 720
	if viewport == "mobile" {
		width, height = 390, 844
	}
	decoded, err := png.Decode(bytes.NewReader(h.screenshotDownload(output, "screenshot.png")))
	if err != nil || decoded.Bounds().Dx() != width || decoded.Bounds().Dy() != height || metadata.Width != width || metadata.Height != height {
		t.Fatal("native screenshot artifact was not a PNG of the declared dimensions")
	}
	canonical := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canonical, canonical.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return sha256.Sum256(canonical.Pix), metadata.Truncated
}

// The caller has delivered real managed username/password/OTP values and used an
// authenticated fake provider. No new worker or secret delivery is needed here.
func checkHermesProtectedScreenshots(t *testing.T, h *hermesSessionHost, parent operations.Task, site *fixtures.BrowserSite, key string, modelCalls func() int) {
	t.Helper()
	binding := h.binding(parent)
	binding.Mode = sessions.Use
	protected := []string{site.Accounts[0].Username, site.Accounts[0].Password, site.Code, key}
	title := "Protected account " + strings.Join(protected, " ")
	body := `<h1>Account summary</h1><p id="public">The study measured 42 samples.</p>`
	for _, value := range protected {
		body += `<p class="secret">Credential echo ` + html.EscapeString(value) + `</p>`
	}
	body += `<input value="excluded-form-a"><textarea>excluded-textarea-a</textarea><p id="hidden" hidden>excluded-hidden-a</p>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/overlap" {
			_, _ = io.WriteString(w, `<title>Unicode overlap</title><p>`+strings.Repeat("漢", 12000)+`</p>`)
			return
		}
		if r.URL.Path == "/boundary" || r.URL.Path == "/boundary-redacted" {
			value := key
			if r.URL.Path == "/boundary-redacted" {
				value = "[REDACTED]"
			}
			_, _ = io.WriteString(w, "<title>"+strings.Repeat("x", 250)+html.EscapeString(value)+"</title><p>Title boundary fixture</p>")
			return
		}
		_, _ = io.WriteString(w, "<title>"+html.EscapeString(title)+"</title>"+body)
	}))
	t.Cleanup(server.Close)
	load := func(path string) {
		// Only a fixture URL enters native code. Secret echoes arrive as ordinary
		// page content; the provider key is never an RPC parameter or Python input.
		expression := fmt.Sprintf(`(async()=>{const response=await fetch(%q);const source=new DOMParser().parseFromString(await response.text(),'text/html');document.title=source.title;document.body.innerHTML=source.body.innerHTML;return true})()`, server.URL+path)
		hermesScreenshotEvaluate(t, h, binding, expression)
	}
	load("/")
	checkHermesScreenshotScopes(t, h, binding)
	calls, events := modelCalls(), len(h.history)
	first, truncated := captureHermesScreenshot(t, h, parent, "desktop")
	if truncated {
		t.Fatal("a short native screenshot fixture was unexpectedly truncated")
	}
	// Replacement markers have exactly the renderer's protected visible text;
	// form and hidden text changes must not affect its pixels.
	replacementTitle := "Protected account " + strings.TrimSpace(strings.Repeat("[REDACTED] ", len(protected)))
	expression := fmt.Sprintf(`document.title=%q;for(const element of document.querySelectorAll('.secret'))element.textContent='Credential echo [REDACTED]';document.querySelector('input').value='excluded-form-b';document.querySelector('textarea').textContent='excluded-textarea-b';document.querySelector('#hidden').textContent='excluded-hidden-b';true`, replacementTitle)
	hermesScreenshotEvaluate(t, h, binding, expression)
	if second, _ := captureHermesScreenshot(t, h, parent, "desktop"); first != second {
		t.Fatal("native screenshot pixels disclosed a credential, OTP, provider key, hidden text or form value")
	}
	hermesScreenshotEvaluate(t, h, binding, `document.querySelector('#public').textContent='The study measured 73 samples.';true`)
	if changed, _ := captureHermesScreenshot(t, h, parent, "desktop"); first == changed {
		t.Fatal("changed public text did not change native screenshot pixels")
	}
	captureHermesScreenshot(t, h, parent, "mobile")
	load("/overlap")
	if _, truncated := captureHermesScreenshot(t, h, parent, "desktop"); !truncated {
		t.Fatal("native screenshot lost its Unicode overlap truncation marker")
	}
	load("/boundary")
	boundary, truncated := captureHermesScreenshot(t, h, parent, "desktop")
	if !truncated {
		t.Fatal("native screenshot lost its title boundary truncation marker")
	}
	load("/boundary-redacted")
	if redacted, _ := captureHermesScreenshot(t, h, parent, "desktop"); boundary != redacted {
		t.Fatal("native screenshot truncated a secret before protecting its title")
	}
	if modelCalls() != calls {
		t.Fatal("native screenshot capture invoked a model")
	}
	for _, message := range h.history[events:] {
		if message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested {
			// A retried delivery request with an already saved response is an
			// existing bus exchange, rather than capture requesting another value.
			if _, answered := h.accepted[message.ID]; !answered {
				t.Fatal("native screenshot capture asked a question or requested credentials")
			}
		}
	}
	assertNativeLoginPrivate(t, h, protected...)
}

func hermesScreenshotEvaluate(t *testing.T, h *hermesSessionHost, binding sessions.Binding, expression string) {
	t.Helper()
	_, err := h.manager.RunHermes(action.WithSession(t.Context(), &binding), hermesScreenshotOptions(), func(ctx context.Context) error {
		return hermes.WithExistingBrowser(ctx, func(source *hermes.Browser) error {
			var response struct {
				Exception json.RawMessage `json:"exceptionDetails"`
			}
			if err := source.Execute(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}, &response); err != nil {
				return err
			}
			if len(response.Exception) != 0 {
				return errors.New("native screenshot fixture evaluation failed")
			}
			return nil
		})
	})
	hermesSessionCheck(t, err)
}

func hermesScreenshotState(t *testing.T, h *hermesSessionHost, binding sessions.Binding) [sha256.Size]byte {
	t.Helper()
	var state struct {
		Frame json.RawMessage
		Page  json.RawMessage
	}
	_, err := h.manager.RunHermes(action.WithSession(t.Context(), &binding), hermesScreenshotOptions(), func(ctx context.Context) error {
		return hermes.WithExistingBrowser(ctx, func(source *hermes.Browser) error {
			if err := source.Execute(ctx, "Page.getFrameTree", map[string]any{}, &state.Frame); err != nil {
				return err
			}
			// The observer distinguishes read-only capture from mutate-and-restore.
			expression := `(()=>{if(!window.__commanderScreenshotObserver){window.__commanderScreenshotMutations=0;window.__commanderScreenshotObserver=new MutationObserver(()=>window.__commanderScreenshotMutations++);window.__commanderScreenshotObserver.observe(document,{subtree:true,childList:true,attributes:true,characterData:true})}return JSON.stringify({url:location.href,title:document.title,body:document.body.innerHTML,controls:Array.from(document.querySelectorAll('input,textarea,select')).map(element=>({tag:element.tagName,type:element.type,value:element.value,checked:element.checked,selected:element.selectedIndex,start:element.selectionStart,end:element.selectionEnd})),focus:document.activeElement&&{tag:document.activeElement.tagName,id:document.activeElement.id,name:document.activeElement.name},width:innerWidth,height:innerHeight,ratio:devicePixelRatio,mobile:matchMedia('(max-width:600px)').matches,mutations:window.__commanderScreenshotMutations})})()`
			return source.Execute(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true}, &state.Page)
		})
	})
	hermesSessionCheck(t, err)
	encoded, err := json.Marshal(state)
	hermesSessionCheck(t, err)
	return sha256.Sum256(encoded)
}

func checkHermesScreenshotScopes(t *testing.T, h *hermesSessionHost, binding sessions.Binding) {
	t.Helper()
	logs := &hermesRenderingWait{entered: make(chan struct{})}
	var expired context.Context
	_, err := h.manager.RunHermes(action.WithLogs(action.WithSession(t.Context(), &binding), logs), hermesScreenshotOptions(), func(ctx context.Context) error {
		expired = ctx
		image, err := browserscreenshot.CaptureHermes(context.WithoutCancel(ctx), 1280, 720)
		if err == nil || len(image.PNG) != 0 {
			return errors.New("detached native screenshot retained its private lease")
		}
		open := binding
		open.Mode = sessions.Open
		image, err = browserscreenshot.CaptureHermes(action.WithSession(ctx, &open), 1280, 720)
		if err == nil || len(image.PNG) != 0 {
			return errors.New("native screenshot accepted an open binding")
		}
		return nil
	})
	hermesSessionCheck(t, err)
	image, err := browserscreenshot.CaptureHermes(expired, 1280, 720)
	if err == nil || len(image.PNG) != 0 {
		t.Fatal("completed native execution retained screenshot authority")
	}
	select {
	case <-logs.entered:
		t.Fatal("invalid native screenshot authority allocated a renderer")
	default:
	}
}

func TestHermesScreenshotUseDoesNotStartBrowser(t *testing.T) {
	h := newHermesSessionHost(t, nil)
	root := h.task(nil, sessions.Open, "unstarted")
	binding := h.binding(root)
	hermesSessionCheck(t, h.manager.Prepare(t.Context(), binding.Claim()))
	before := hermesSessionProfiles(t)
	task := h.screenshotTask(&root, "desktop")
	binding = h.binding(task)
	if output, ok := h.screenshotResult(h.dispatch(task, &binding)); ok || len(output.Output) != 0 || len(output.Artifacts) != 0 {
		t.Fatal("native screenshot created or captured an unstarted browser")
	}
	if h.snapshot(binding.Reference).State != "starting" || h.entered.Load() != 0 || len(before) != len(hermesSessionProfiles(t)) {
		t.Fatal("use-only native screenshot changed unstarted runtime ownership")
	}
}

func TestHermesScreenshotRendererCancellationAndClose(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%t", closeSession), func(t *testing.T) {
			site := fixtures.NewBrowser(t)
			h := newHermesSessionHost(t, map[string]hermesSessionScenario{"login": {login: nativeLoginFixture(site, 0, "/login", "/account")}})
			root := h.task(nil, sessions.Open, "login")
			binding := h.binding(root)
			command := h.dispatch(root, &binding)
			h.loginCredentials(command, site, 0)
			output, ok := h.finish(command)
			if !ok {
				t.Fatal("native renderer fixture login failed")
			}
			requireNativeLogin(t, output, "verified", true)
			binding.Mode = sessions.Use
			before := hermesScreenshotState(t, h, binding)
			logs := &hermesRenderingWait{entered: make(chan struct{}), pause: true}
			work, cancel := context.WithCancel(t.Context())
			defer cancel()
			type outcome struct {
				result action.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				ctx := action.WithLogs(action.WithSession(work, &binding), logs)
				result, err := newHermesScreenshot(h.manager).Run(ctx, json.RawMessage(`{}`))
				done <- outcome{result: result, err: err}
			}()
			select {
			case <-logs.entered:
			case <-time.After(12 * time.Second):
				t.Fatal("native screenshot did not reach its allocated rendering phase")
			}
			if !h.control(binding, "inspect") || !h.snapshot(binding.Reference).Busy {
				t.Fatal("native screenshot blocked independent session inspection")
			}
			if closeSession {
				if !h.control(binding, "close") || h.snapshot(binding.Reference).State != "closed" || h.snapshot(binding.Reference).Busy {
					t.Fatal("native session close did not join its allocated renderer")
				}
			} else {
				cancel()
			}
			select {
			case stopped := <-done:
				if !errors.Is(stopped.err, context.Canceled) || len(stopped.result.Output) != 0 || len(stopped.result.Artifacts) != 0 {
					t.Fatal("interrupted native renderer returned artifacts or lost cancellation")
				}
			case <-time.After(6 * time.Second):
				t.Fatal("native renderer did not join after interruption")
			}
			if !closeSession {
				if h.snapshot(binding.Reference).State != "connected" || before != hermesScreenshotState(t, h, binding) {
					t.Fatal("native screenshot cancellation lost or changed its healthy source")
				}
				captureHermesScreenshot(t, h, root, "desktop")
			}
			if site.Posts.Load() != 1 {
				t.Fatal("native screenshot interruption repeated authenticated submission")
			}
		})
	}
}

type hermesRenderingWait struct {
	entered chan struct{}
	once    sync.Once
	pause   bool
}

func (logs *hermesRenderingWait) Log(ctx context.Context, message string) error {
	if message == "Rendering the protected screenshot" {
		logs.once.Do(func() { close(logs.entered) })
		if logs.pause {
			<-ctx.Done()
			return ctx.Err()
		}
	}
	return nil
}
