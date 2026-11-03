//go:build browserintegration

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/actions"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

// The main workflow supplies a real managed login and model-enrolled session.
func checkProtectedScreenshots(t *testing.T, h *runtimeHarness, binding sessions.Binding, protected []string) {
	t.Helper()
	binding.Mode = sessions.Use
	var escaped context.Context
	_, err := h.manager.Run(action.WithSession(t.Context(), &binding), h.options, func(ctx context.Context) error {
		escaped = context.WithoutCancel(ctx)
		return nil
	})
	if err != nil {
		t.Fatal("cannot inspect completed renderer ownership")
	}
	logs := &renderingWait{entered: make(chan struct{})}
	expired, cancel := context.WithTimeout(action.WithLogs(escaped, logs), time.Second)
	defer cancel()
	data, err := browserruntime.RenderText(expired, "Expired execution", 1280, 720)
	if err == nil || len(data) != 0 {
		t.Fatal("completed execution retained rendering access")
	}
	select {
	case <-logs.entered:
		t.Fatal("completed execution allocated a renderer")
	default:
	}
	body := `<h1>Account summary</h1><p>The study measured 42 samples.</p>`
	for _, value := range protected {
		body += "<p>Credential echo " + html.EscapeString(value) + "</p>"
	}
	body += `<input value="excluded-form-a"><textarea>excluded-textarea-a</textarea><p hidden>excluded-hidden-a</p>`
	title := "Protected account " + protected[0]
	setScreenshotDOM(t, h, binding, title, body)
	first := captureScreenshot(t, h, binding, "desktop")
	for _, value := range protected {
		title, body = strings.ReplaceAll(title, value, "[REDACTED]"), strings.ReplaceAll(body, html.EscapeString(value), "[REDACTED]")
	}
	body = strings.ReplaceAll(body, "-a", "-b")
	setScreenshotDOM(t, h, binding, title, body)
	if first != captureScreenshot(t, h, binding, "desktop") {
		t.Fatal("credentials, hidden text or form values changed screenshot pixels")
	}
	setScreenshotDOM(t, h, binding, title, strings.ReplaceAll(body, "42 samples", "73 samples"))
	if first == captureScreenshot(t, h, binding, "desktop") {
		t.Fatal("changed public text did not change screenshot pixels")
	}
	captureScreenshot(t, h, binding, "mobile")
}

func captureScreenshot(t *testing.T, h *runtimeHarness, binding sessions.Binding, viewport string) [sha256.Size]byte {
	t.Helper()
	before := screenshotState(t, h, binding)
	output := h.Result(h.Call("screenshot", map[string]any{"viewport": viewport}, binding))
	if output.Session == nil || output.Session.ID != binding.ID || len(output.Artifacts) != 1 || len(output.Artifacts[0].Files) != 2 ||
		!bytes.Equal(output.Output, h.Download(output, "screenshot.json")) || before != screenshotState(t, h, binding) {
		t.Fatal("screenshot changed its retained source or lost artifact metadata")
	}
	var metadata actions.ScreenshotOutput
	if json.Unmarshal(output.Output, &metadata) != nil || metadata.Engine != "lightpanda" || metadata.Renderer != "lightpanda-text-snapshot" || metadata.MediaType != "image/png" || metadata.Viewport != viewport {
		t.Fatal("screenshot metadata did not identify its text renderer")
	}
	width, height := 1280, 720
	if viewport == "mobile" {
		width, height = 390, 844
	}
	decoded, err := png.Decode(bytes.NewReader(h.Download(output, "screenshot.png")))
	if err != nil || decoded.Bounds().Dx() != width || decoded.Bounds().Dy() != height || metadata.Width != width || metadata.Height != height {
		t.Fatal("uploaded screenshot is not a real PNG with the declared dimensions")
	}
	canonical := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canonical, canonical.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return sha256.Sum256(canonical.Pix)
}

func TestScreenshotRendererCancellationAndClose(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%t", closeSession), func(t *testing.T) {
			h := newRuntime(t)
			site := fixtures.NewBrowser(t)
			binding := loginAccount(t, h, h.Binding(), site, 0)
			binding.Mode = sessions.Use
			before := screenshotState(t, h, binding)
			executor := actions.NewScreenshot(h.manager)
			if status, err := executor.Discover(t.Context(), &environment.Local{}, nil); err != nil || status != action.Ready {
				t.Fatal("screenshot action was not ready")
			}
			logs := &renderingWait{entered: make(chan struct{})}
			work, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := action.WithLogs(action.WithSession(work, &binding), logs)
			type outcome struct {
				result action.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := executor.Run(ctx, runtimeJSON(t, map[string]any{"session_scope": binding.ScopeID, "session_id": binding.ID}))
				done <- outcome{result, err}
			}()
			select {
			case <-logs.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("screenshot did not allocate its renderer before interruption")
			}
			inspected := h.Control(binding, "inspect")
			if len(inspected.Sessions) != 1 || !inspected.Sessions[0].Busy {
				t.Fatal("allocated rendering prevented independent session inspection")
			}
			if closeSession {
				closed := h.Control(binding, "close")
				if len(closed.Sessions) != 1 || closed.Sessions[0].State != "closed" || closed.Sessions[0].Busy {
					t.Fatal("session close did not join its allocated renderer")
				}
			} else {
				cancel()
			}
			select {
			case stopped := <-done:
				if !errors.Is(stopped.err, context.Canceled) || len(stopped.result.Output) != 0 || len(stopped.result.Artifacts) != 0 {
					t.Fatal("interrupted renderer returned an artifact or lost cancellation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("interrupted renderer did not release its action promptly")
			}
			if !closeSession {
				if before != screenshotState(t, h, binding) {
					t.Fatal("renderer cancellation changed authenticated page state")
				}
				captureScreenshot(t, h, binding, "desktop")
			}
			if site.Posts.Load() != 1 {
				t.Fatal("renderer interruption repeated authenticated form submission")
			}
		})
	}
}

type renderingWait struct {
	entered chan struct{}
	once    sync.Once
}

func (logs *renderingWait) Log(ctx context.Context, message string) error {
	if message == "Rendering the protected screenshot" {
		logs.once.Do(func() { close(logs.entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func setScreenshotDOM(t *testing.T, h *runtimeHarness, binding sessions.Binding, title, body string) {
	t.Helper()
	_, err := h.manager.Run(action.WithSession(t.Context(), &binding), h.options, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`document.title=%s;document.body.innerHTML=%s`, runtimeJSON(t, title), runtimeJSON(t, body)), nil))
	})
	if err != nil {
		t.Fatal("cannot prepare screenshot source")
	}
}

func screenshotState(t *testing.T, h *runtimeHarness, binding sessions.Binding) [sha256.Size]byte {
	t.Helper()
	var state struct {
		Document string
		Cookies  string
		Target   string
	}
	_, err := h.manager.Run(action.WithSession(t.Context(), &binding), h.options, func(ctx context.Context) error {
		if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({url:location.href,title:document.title,body:document.body.innerHTML,width:innerWidth,height:innerHeight,ratio:devicePixelRatio,mobile:matchMedia('(max-width:600px)').matches})`, &state.Document)); err != nil {
			return err
		}
		cookies, err := network.GetCookies().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target))
		if err != nil {
			return err
		}
		state.Cookies = string(runtimeJSON(t, cookies))
		targets, err := target.GetTargets().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		if err == nil {
			state.Target = string(runtimeJSON(t, targets))
		}
		return err
	})
	if err != nil {
		t.Fatal("cannot inspect retained screenshot source")
	}
	return sha256.Sum256(runtimeJSON(t, state))
}
