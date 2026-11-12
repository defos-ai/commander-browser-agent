//go:build hermesintegration && hermescontainerintegration

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
	"strconv"
	"strings"
	"testing"
	"time"

	browserscreenshot "github.com/defos-ai/commander-browser-agent/browser/screenshot"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

// This helper continues the real native reasoning/key-rotation scenario. Capture
// must use its retained browser and fingerprints without another model request.
func checkHermesContainerScreenshots(t *testing.T, worker *hermes.Worker, config hermes.ContainerConfig, calls func() int, keys ...string) {
	t.Helper()
	ctx, stop := context.WithTimeout(t.Context(), 70*time.Second)
	defer stop()
	ctx = action.WithLogs(ctx, &hermesRenderingWait{entered: make(chan struct{})})
	values := sensitive.New()
	defer values.Clear()
	ctx = sensitive.WithValues(ctx, values)
	credentials := []string{"fixture-render-username", "fixture-render-password", "fixture-render-otp"}
	for _, credential := range credentials {
		if sensitive.Remember(ctx, []byte(credential)) != nil {
			t.Fatal("cannot register the isolated screenshot's synthetic credentials")
		}
	}
	protected := append(append([]string{}, keys...), credentials...)
	title := "Protected isolated study " + strings.Join(protected, " ")
	body := `<h1>Account summary</h1><p id="public">The study measured 42 samples.</p>`
	for _, value := range protected {
		body += `<p class="secret">Credential echo ` + html.EscapeString(value) + `</p>`
	}
	body += `<input value="excluded-form-a"><textarea>excluded-textarea-a</textarea><p id="hidden" hidden>excluded-hidden-a</p>`
	// Trusted fixture setup mimics a website echoing known values. It does not
	// send these values to a model or ask the native agent to enter credentials.
	hermesContainerScreenshotEvaluate(t, ctx, worker, fmt.Sprintf(`document.title=%q;document.body.innerHTML=%q;true`, title, body), nil)
	beforeCalls := calls()
	first, _ := captureHermesContainerScreenshot(t, ctx, worker, config, 1280, 720)
	replacementTitle := "Protected isolated study " + strings.TrimSpace(strings.Repeat("[REDACTED] ", len(protected)))
	hermesContainerScreenshotEvaluate(t, ctx, worker, fmt.Sprintf(`document.title=%q;for(const element of document.querySelectorAll('.secret'))element.textContent='Credential echo [REDACTED]';document.querySelector('input').value='excluded-form-b';document.querySelector('textarea').textContent='excluded-textarea-b';document.querySelector('#hidden').textContent='excluded-hidden-b';true`, replacementTitle), nil)
	if second, _ := captureHermesContainerScreenshot(t, ctx, worker, config, 1280, 720); first != second {
		t.Fatal("isolated screenshot pixels disclosed a provider key, credential, code, hidden text or form value")
	}
	hermesContainerScreenshotEvaluate(t, ctx, worker, `document.querySelector('#public').textContent='The study measured 73 samples.';true`, nil)
	if changed, _ := captureHermesContainerScreenshot(t, ctx, worker, config, 1280, 720); first == changed {
		t.Fatal("isolated screenshot did not render changed public text")
	}
	captureHermesContainerScreenshot(t, ctx, worker, config, 390, 844)
	var cookie struct{ Value bool }
	hermesContainerScreenshotEvaluate(t, ctx, worker, `(async()=>{const response=await fetch('/read');return (await response.text()).includes('Retained relay cookie confirmed')})()`, &cookie)
	if !cookie.Value || calls() != beforeCalls || worker.Verify(ctx) != nil {
		t.Fatal("isolated screenshot lost authentication, invoked a model or changed its retained source")
	}
}

func hermesContainerScreenshotContext(ctx context.Context, config hermes.ContainerConfig) context.Context {
	claim := config.Claim
	binding := sessions.Binding{Reference: claim.Reference, Mode: sessions.Use, CommandID: claim.CommandID, OperationID: claim.OperationID, TaskID: claim.TaskID}
	return action.WithSession(ctx, &binding)
}

func captureHermesContainerScreenshot(t *testing.T, ctx context.Context, worker *hermes.Worker, config hermes.ContainerConfig, width, height int) ([sha256.Size]byte, bool) {
	t.Helper()
	before := hermesContainerScreenshotState(t, ctx, worker)
	var captured browserscreenshot.Image
	err := hermes.WithWorker(hermesContainerScreenshotContext(ctx, config), worker, func(active context.Context) error {
		var err error
		captured, err = browserscreenshot.CaptureHermes(active, width, height)
		return err
	})
	if err != nil {
		t.Fatalf("isolated screenshot failed (%T): %v", err, err)
	}
	if before != hermesContainerScreenshotState(t, ctx, worker) {
		t.Fatal("isolated screenshot mutated the retained source")
	}
	decoded, err := png.Decode(bytes.NewReader(captured.PNG))
	if err != nil || decoded.Bounds().Dx() != width || decoded.Bounds().Dy() != height {
		t.Fatal("isolated screenshot did not return a PNG of its requested dimensions")
	}
	canonical := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canonical, canonical.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return sha256.Sum256(canonical.Pix), captured.Truncated
}

func hermesContainerScreenshotEvaluate(t *testing.T, ctx context.Context, worker *hermes.Worker, expression string, result any) {
	t.Helper()
	err := hermes.WithWorker(ctx, worker, func(active context.Context) error {
		return hermes.WithExistingBrowser(active, func(source *hermes.Browser) error {
			var reply struct {
				Result           json.RawMessage `json:"result"`
				ExceptionDetails json.RawMessage `json:"exceptionDetails"`
			}
			if err := source.Execute(active, "Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true}, &reply); err != nil {
				return err
			}
			if len(reply.ExceptionDetails) != 0 {
				return errors.New("isolated screenshot fixture evaluation failed")
			}
			if result != nil {
				return json.Unmarshal(reply.Result, result)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal("isolated screenshot fixture could not inspect its retained source")
	}
}

func hermesContainerScreenshotState(t *testing.T, ctx context.Context, worker *hermes.Worker) [sha256.Size]byte {
	t.Helper()
	var state json.RawMessage
	expression := `(()=>{if(!window.__containerScreenshotObserver){window.__containerScreenshotMutations=0;window.__containerScreenshotObserver=new MutationObserver(()=>window.__containerScreenshotMutations++);window.__containerScreenshotObserver.observe(document,{subtree:true,childList:true,attributes:true,characterData:true})}return JSON.stringify({url:location.href,title:document.title,body:document.body.innerHTML,controls:Array.from(document.querySelectorAll('input,textarea,select')).map(element=>({tag:element.tagName,type:element.type,value:element.value,checked:element.checked,selected:element.selectedIndex,start:element.selectionStart,end:element.selectionEnd})),focus:document.activeElement&&{tag:document.activeElement.tagName,id:document.activeElement.id,name:document.activeElement.name},width:innerWidth,height:innerHeight,ratio:devicePixelRatio,mobile:matchMedia('(max-width:600px)').matches,mutations:window.__containerScreenshotMutations})})()`
	hermesContainerScreenshotEvaluate(t, ctx, worker, expression, &state)
	return sha256.Sum256(state)
}

func TestHermesContainerScreenshotCancellationAndCloseJoinRenderer(t *testing.T) {
	worker, config, api := newHermesContainerModelWorker(t, false)
	ctx, stop := context.WithTimeout(t.Context(), 65*time.Second)
	defer stop()
	ctx = action.WithLogs(ctx, &hermesRenderingWait{entered: make(chan struct{})})
	container := hermesContainerModelReference(t, ctx, api, config)
	site := api.exec(t, ctx, container, hermesContainerModelSite)["url"]
	err := hermes.WithWorker(ctx, worker, func(active context.Context) error {
		return hermes.WithBrowser(active, func(source *hermes.Browser) error {
			return source.Execute(active, "Page.navigate", map[string]any{"url": site + "/set"}, nil)
		})
	})
	if err != nil || worker.Verify(ctx) != nil {
		t.Fatal("the isolated renderer interruption fixture could not retain its browser")
	}
	before := hermesContainerScreenshotState(t, ctx, worker)
	for _, closing := range []bool{false, true} {
		marker := "/home/commander/renderer-stop-" + id.New()
		api.exec(t, ctx, container, hermesContainerRendererWatcher(marker))
		work, cancel := context.WithCancelCause(ctx)
		cause := errors.New("fixture isolated renderer cancellation")
		type outcome struct {
			png []byte
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			var png []byte
			err := hermes.WithWorker(work, worker, func(active context.Context) error {
				return hermes.WithExistingBrowser(active, func(source *hermes.Browser) error {
					var err error
					png, err = source.RenderText(active, strings.Repeat("Protected interruption fixture.\n", 1000), 1280, 720)
					return err
				})
			})
			done <- outcome{png: png, err: err}
		}()
		// A test-only watcher pauses this exact helper while its exec is live.
		// Cancellation must join it before allowing source reuse or close.
		stopped := api.exec(t, ctx, container, fmt.Sprintf(`import json, time
from pathlib import Path
path = Path(%q)
for _ in range(400):
    if path.exists():
        print(path.read_text())
        break
    time.sleep(.01)
else:
    print(json.dumps({'pid': ''}))`, marker))["pid"]
		pid, parseErr := strconv.Atoi(stopped)
		if parseErr != nil || pid < 2 {
			cancel(cause)
			select {
			case result := <-done:
				t.Fatalf("isolated renderer returned before its interruption phase (%T): %v", result.err, result.err)
			default:
			}
			t.Fatal("the isolated renderer did not reach its interruption phase")
		}
		closed := make(chan error, 1)
		if closing {
			go func() { closed <- worker.Close() }()
			select {
			case <-worker.Done():
			case <-time.After(time.Second):
				t.Fatal("isolated worker close did not stop renderer admission")
			}
		} else {
			cancel(cause)
		}
		api.exec(t, ctx, container, fmt.Sprintf(`import json, os, signal
from pathlib import Path
process = Path('/proc/%d')
assert process.joinpath('cmdline').read_bytes().split(b'\0')[0] == b'/usr/local/lib/commander-browser/render-text'
os.kill(%d, signal.SIGCONT)
Path(%q).unlink()
print(json.dumps({'resumed': 'true'}))`, pid, pid, marker))
		select {
		case stopped := <-done:
			if stopped.err == nil || len(stopped.png) != 0 || !closing && !errors.Is(stopped.err, cause) {
				t.Fatal("an interrupted isolated renderer returned image bytes or lost its cancellation cause")
			}
		case <-time.After(7 * time.Second):
			t.Fatal("isolated renderer interruption did not join its helper")
		}
		cancel(nil)
		if closing {
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal("isolated worker close did not confirm renderer and private-state cleanup")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("isolated worker close did not join its active renderer")
			}
			if len(hermesContainerModelReferences(t, ctx, api, config)) != 0 {
				t.Fatal("confirmed isolated close retained its renderer container")
			}
		} else if worker.Verify(ctx) != nil || before != hermesContainerScreenshotState(t, ctx, worker) {
			t.Fatal("isolated renderer cancellation lost or changed its reusable source")
		}
	}
}

func hermesContainerRendererWatcher(marker string) string {
	watcher := fmt.Sprintf(`import json, os, signal, time
from pathlib import Path
for _ in range(8000):
    for process in Path('/proc').glob('[0-9]*'):
        try:
            if process.joinpath('cmdline').read_bytes().split(b'\0')[0] != b'/usr/local/lib/commander-browser/render-text':
                continue
            os.kill(int(process.name), signal.SIGSTOP)
            Path(%q).write_text(json.dumps({'pid': process.name}))
            raise SystemExit(0)
        except (OSError, PermissionError):
            pass
    time.sleep(.001)
Path(%q).write_text(json.dumps({'pid': ''}))`, marker, marker)
	return fmt.Sprintf(`import json, subprocess, sys
subprocess.Popen([sys.executable, '-I', '-S', '-c', %q], start_new_session=True,
                 stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
print(json.dumps({'watching': 'true'}))`, watcher)
}
