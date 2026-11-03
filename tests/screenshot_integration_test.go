//go:build browserintegration

package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/actions"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

func TestScreenshotThroughFrameworkBetweenLoginAndBrowse(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	const key = "fixture-screenshot-model-key-930d37d1"
	model := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		if !strings.Contains(turn.Observation.Text, "Account verified") || !strings.Contains(turn.Observation.Text, "42 samples") {
			t.Error("screenshot changed the authenticated retained page or cookie state")
		}
		return finishOrExtractStudy(turn)
	})
	framework := startBrowseFramework(t, key)
	peer := framework.peer
	binding := newBrowserSessionBinding(framework.host.ID())
	login := publishLogin(t, peer, binding, loginInput(binding, site.URL, "/login", "work"))
	username, password := loginSecretNames(site.URL, "work")
	deliverBrowseModelSecret(t, peer, model, username, site.Username)
	deliverBrowseModelSecret(t, peer, model, password, site.Password)
	loggedIn := receiveBrowse(t, peer, login, site.Username, site.Password, key)
	var loginOutput actions.LoginOutput
	if json.Unmarshal(loggedIn.Output, &loginOutput) != nil || loginOutput.Status != "verified" || loggedIn.Session == nil {
		t.Fatal("screenshot workflow did not start with a verified session")
	}
	binding.Mode = sessions.Use
	for _, viewport := range []string{"", "mobile"} {
		command := publishScreenshot(t, peer, binding, viewport)
		output := receiveScreenshot(t, peer, command)
		if output.Session == nil || model.Calls() != 0 || site.Submissions.Load() != 1 {
			t.Fatal("screenshot lost the session, used a model or repeated login")
		}
		assertBrowserSession(t, *output.Session, binding.Reference, "connected")
		if len(output.Artifacts) != 1 || output.Artifacts[0].Name != "Lightpanda protected text screenshot" || len(output.Artifacts[0].Files) != 2 {
			t.Fatal("screenshot did not upload its PNG and renderer metadata together")
		}
		var data []byte
		for _, file := range output.Artifacts[0].Files {
			contents, err := framework.files.Get(t.Context(), output.Artifacts[0].ID, file)
			if err != nil {
				t.Fatal(err)
			}
			switch file.Name {
			case "screenshot.png":
				if file.MediaType != "image/png" {
					t.Fatal("PNG artifact has the wrong media type")
				}
				data = contents
			case "screenshot.json":
				if file.MediaType != "application/json" || !bytes.Equal(contents, output.Output) {
					t.Fatal("renderer metadata sidecar differs from the action output")
				}
			default:
				t.Fatal("screenshot produced an unexpected artifact file")
			}
		}
		decodeScreenshot(t, output.Output, data, viewport)
	}
	binding.Mode = sessions.Reuse
	browse := publishBrowse(t, peer.bus, binding, site.URL+"/account", 4)
	answerBrowseQuestion(t, peer, browse, model, "browser_model_provider", "other", key, []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}, false)
	answerBrowseQuestion(t, peer, browse, model, "browser_model_name", "fixture-model", key, nil, false)
	answerBrowseQuestion(t, peer, browse, model, "browser_model_url", model.URL, key, nil, false)
	deliverBrowseModelSecret(t, peer, model, modelSetupSecretName(model.URL), key)
	browsed := receiveBrowse(t, peer, browse, site.Username, site.Password, key)
	var findings actions.BrowseOutput
	if json.Unmarshal(browsed.Output, &findings) != nil || findings.Status != "completed" || browsed.Session == nil || browsed.Session.ID != binding.ID || site.Submissions.Load() != 1 {
		t.Fatal("login, screenshot and browse did not reuse one authenticated session")
	}
	closed := peer.control(binding, "close")
	if len(closed.Sessions) != 1 {
		t.Fatal("close discarded the captured session")
	}
	assertBrowserSession(t, closed.Sessions[0], binding.Reference, "closed")
}

func TestScreenshotRendersProtectedVisibleTextWithoutChangingSource(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewLoginSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	username, password := loginSecretNames(site.URL, "work")
	secrets := &browseSecretProbe{values: map[string][]byte{username: []byte(site.Username), password: []byte(site.Password)}}
	ctx := action.WithLogs(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), &loginLogProbe{})
	loginResult, err := realLogin(t, manager).Run(ctx, marshalLoginInput(t, loginInput(binding, site.URL, "/login", "work")))
	var loggedIn actions.LoginOutput
	if err != nil || json.Unmarshal(loginResult.Output, &loggedIn) != nil || loggedIn.Status != "verified" {
		t.Fatal("pixel privacy fixture was not authenticated")
	}
	// Enroll the managed model credential through an actual browse execution,
	// including its normal model setup and HTTP protocol, before capturing it.
	const modelKey = "[Rprivate-screenshot-model-key-4d1876b9"
	modelSite := fixtures.NewBrowseSite(t, modelKey)
	model := fixtures.NewBrowseModel(t, modelKey, func(turn intelligence.Turn) intelligence.Command {
		if len(turn.Evidence) == 0 {
			return intelligence.Command{Action: "extract"}
		}
		return intelligence.Command{Action: "finish", Summary: "The fixture catalog is ready.", Findings: []intelligence.Finding{{Text: "JavaScript catalog ready.", EvidenceIDs: []string{turn.Evidence[0].ID}}}}
	})
	binding.Mode = sessions.Reuse
	browse := actions.NewBrowse(manager.Manager)
	status, err := browse.Discover(t.Context(), &environment.Local{}, nil)
	if err != nil || status != action.Ready {
		t.Fatal("model credential privacy browse discovery failed")
	}
	questions := &modelSetupQuestions{answers: map[string]string{"browser_model_provider": "other", "browser_model_name": "fixture-model", "browser_model_url": model.URL}}
	modelSecrets := &browseSecretProbe{value: []byte(modelKey)}
	browseInput := modelSetupInput(binding)
	browseInput["url"] = modelSite.URL
	ctx = action.WithLogs(action.WithQuestions(action.WithSecrets(action.WithSession(t.Context(), &binding), modelSecrets), questions), &loginLogProbe{})
	browsed, err := browse.Run(ctx, marshalLoginInput(t, browseInput))
	var browseOutput actions.BrowseOutput
	if err != nil || json.Unmarshal(browsed.Output, &browseOutput) != nil || browseOutput.Status != "completed" || model.Calls() != 2 || modelSecrets.requests != 1 || browsed.Session == nil || browsed.Session.ID != binding.ID {
		t.Fatal("model credential privacy fixture did not finish a real managed browse")
	}
	binding.Mode = sessions.Use
	body := fmt.Sprintf(`<h1>Account verified</h1><p id="safe">The private study measured 42 samples.</p><p id="echo">Username %s Password %s</p><p>Model key %s</p><label>Form field<input id="field" value="unrendered-form-value-a"></label><input type="password" value="unrendered-password-value-a"><p id="hidden" hidden>unrendered-hidden-value-a</p>`, site.Username, site.Password, modelKey)
	setScreenshotSource(t, manager, binding, options, "Protected account "+site.Username, body)
	executor := actions.NewScreenshot(manager.Manager)
	status, err = executor.Discover(t.Context(), &environment.Local{}, nil)
	if err != nil || status != action.Ready {
		t.Fatal("real screenshot discovery failed")
	}
	first := captureUnchangedScreenshot(t, executor, manager, binding, options)
	// Mask only registered credential echoes, then change values that must never
	// render. Identical pixels prove their omission without OCR or glyph guesses.
	masked := strings.NewReplacer(site.Username, "[REDACTED]", site.Password, "[REDACTED]", modelKey, "[REDACTED]").Replace(body)
	masked = strings.ReplaceAll(masked, "-value-a", "-value-b")
	setScreenshotSource(t, manager, binding, options, "Protected account [REDACTED]", masked)
	second := captureUnchangedScreenshot(t, executor, manager, binding, options)
	if first != second {
		t.Fatal("known login/model credentials, form values or hidden text changed screenshot pixels")
	}
	// A positive control prevents empty images or suppressed visible text from
	// satisfying the privacy comparison above.
	setScreenshotSource(t, manager, binding, options, "Protected account [REDACTED]", strings.ReplaceAll(masked, "42 samples", "73 samples"))
	third := captureUnchangedScreenshot(t, executor, manager, binding, options)
	if first == third {
		t.Fatal("screenshot did not render changed safe visible text")
	}
	// A credential prefix can also be a prefix of the public replacement
	// marker. A title cut after "[R" must drop the whole interrupted marker.
	padding := strings.Repeat("B", 254)
	publicBody := "<h1>Public excerpt boundary fixture</h1>"
	setScreenshotSource(t, manager, binding, options, padding+modelKey, publicBody)
	boundary := captureUnchangedScreenshot(t, executor, manager, binding, options)
	setScreenshotSource(t, manager, binding, options, padding, publicBody)
	reference := captureUnchangedScreenshot(t, executor, manager, binding, options)
	if boundary != reference {
		t.Fatal("a truncated replacement marker exposed a managed credential prefix")
	}
	if model.Calls() != 2 || modelSecrets.requests != 1 || len(questions.calls) != 3 {
		t.Fatal("screenshot requested additional model work, setup or managed credentials")
	}
	_, err = manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
		var authenticated bool
		if err := chromedp.Run(ctx, chromedp.Navigate(site.URL+"/account"), chromedp.Evaluate(`document.body.textContent.includes("Account verified")`, &authenticated)); err != nil {
			return err
		}
		if !authenticated {
			return fmt.Errorf("screenshot lost the authenticated cookie")
		}
		return nil
	})
	if err != nil || site.Submissions.Load() != 1 {
		t.Fatal("screenshot changed the retained authentication state")
	}
}

func TestScreenshotRejectsRenderingContextRetainedAfterManagerRun(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	var escaped context.Context
	_, err := manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
		if err := chromedp.Run(ctx, chromedp.Navigate(site.URL)); err != nil {
			return err
		}
		escaped = context.WithoutCancel(ctx)
		return nil
	})
	if err != nil || escaped == nil {
		t.Fatal("failed to capture a completed managed execution context")
	}
	binding.Mode = sessions.Use
	before := screenshotSourceState(t, manager, binding, options)
	attempt, cancel := context.WithTimeout(action.WithLogs(escaped, &loginLogProbe{}), 5*time.Second)
	defer cancel()
	data, err := browserruntime.RenderText(attempt, "A completed execution must not allocate a renderer.", 1280, 720)
	if err == nil || len(data) != 0 || !reflect.DeepEqual(before, screenshotSourceState(t, manager, binding, options)) {
		t.Fatal("an escaped completed execution rendered or changed the retained source")
	}
	inspected, err := manager.Control(t.Context(), browserSessionControl(binding, "inspect"))
	if err != nil {
		t.Fatal("expired renderer lease made its source session unavailable")
	}
	assertBrowserSession(t, inspected, binding.Reference, "connected")
}

func TestScreenshotRendererCancellationAndCloseJoinResources(t *testing.T) {
	for _, closeSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel_action", true: "close_session"}[closeSession], func(t *testing.T) {
			options := browserSessionOptions(t)
			site := fixtures.NewLoginSite(t)
			manager := newBrowserSessionManager(t)
			binding := newBrowserSessionBinding(manager.owner)
			if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
				t.Fatal(err)
			}
			username, password := loginSecretNames(site.URL, "work")
			secrets := &browseSecretProbe{values: map[string][]byte{username: []byte(site.Username), password: []byte(site.Password)}}
			ctx := action.WithLogs(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), &loginLogProbe{})
			login, err := realLogin(t, manager).Run(ctx, marshalLoginInput(t, loginInput(binding, site.URL, "/login", "work")))
			var loggedIn actions.LoginOutput
			if err != nil || json.Unmarshal(login.Output, &loggedIn) != nil || loggedIn.Status != "verified" {
				t.Fatal("renderer lifecycle fixture was not authenticated")
			}
			binding.Mode = sessions.Use
			setScreenshotSource(t, manager, binding, options, "Retained account", "<h1>Account verified</h1><p>Retained study page</p>")
			before := screenshotSourceState(t, manager, binding, options)
			executor := actions.NewScreenshot(manager.Manager)
			status, err := executor.Discover(t.Context(), &environment.Local{}, nil)
			if err != nil || status != action.Ready {
				t.Fatal("renderer lifecycle screenshot discovery failed")
			}
			logs := &screenshotRenderingWait{entered: make(chan struct{})}
			work, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = action.WithLogs(action.WithSession(work, &binding), logs)
			type outcome struct {
				result action.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := executor.Run(ctx, marshalLoginInput(t, screenshotInput(binding)))
				done <- outcome{result: result, err: err}
			}()
			select {
			case <-logs.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("screenshot did not allocate its renderer before cancellation")
			}
			controlCtx, stopControl := context.WithTimeout(t.Context(), 10*time.Second)
			defer stopControl()
			inspected, err := manager.Control(controlCtx, browserSessionControl(binding, "inspect"))
			if err != nil || !inspected.Busy {
				t.Fatal("allocated screenshot renderer prevented session inspection")
			}
			assertBrowserSession(t, inspected, binding.Reference, "connected")
			if closeSession {
				closed, err := manager.Control(controlCtx, browserSessionControl(binding, "close"))
				if err != nil {
					t.Fatal("session close failed to join its allocated renderer")
				}
				assertBrowserSession(t, closed, binding.Reference, "closed")
			} else {
				cancel()
			}
			select {
			case stopped := <-done:
				if !errors.Is(stopped.err, context.Canceled) || len(stopped.result.Artifacts) != 0 || len(stopped.result.Output) != 0 {
					t.Fatal("interrupted allocated renderer returned an artifact or lost cancellation")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("interrupted renderer did not release its action promptly")
			}
			if closeSession {
				return
			}
			if !reflect.DeepEqual(before, screenshotSourceState(t, manager, binding, options)) {
				t.Fatal("renderer cancellation changed or lost the connected source page")
			}
			captureUnchangedScreenshot(t, executor, manager, binding, options)
			var authenticated bool
			_, err = manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
				return chromedp.Run(ctx, chromedp.Navigate(site.URL+"/account"), chromedp.Evaluate(`document.body.textContent.includes("Account verified")`, &authenticated))
			})
			if err != nil || !authenticated || site.Submissions.Load() != 1 {
				t.Fatal("cancelled renderer prevented authenticated session reuse")
			}
		})
	}
}

func decodeScreenshot(t *testing.T, metadata, data []byte, viewport string) [sha256.Size]byte {
	t.Helper()
	if viewport == "" {
		viewport = "desktop"
	}
	var output actions.ScreenshotOutput
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&output) != nil || output.Engine != "lightpanda" || output.Renderer != "lightpanda-text-snapshot" || output.Viewport != viewport || output.MediaType != "image/png" {
		t.Fatal("screenshot did not identify its protected text renderer")
	}
	width, height := 1280, 720
	if viewport == "mobile" {
		width, height = 390, 844
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil || decoded.Bounds().Dx() != width || decoded.Bounds().Dy() != height || output.Width != width || output.Height != height {
		t.Fatal("screenshot artifact is not a real PNG with the declared viewport dimensions")
	}
	canonical := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canonical, canonical.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	return sha256.Sum256(canonical.Pix)
}

func captureUnchangedScreenshot(t *testing.T, executor *actions.Screenshot, manager ownedBrowserSessionManager, binding sessions.Binding, options browserruntime.Options) [sha256.Size]byte {
	t.Helper()
	before := screenshotSourceState(t, manager, binding, options)
	ctx := action.WithLogs(action.WithSession(t.Context(), &binding), &loginLogProbe{})
	result, err := executor.Run(ctx, marshalLoginInput(t, screenshotInput(binding)))
	if err != nil || result.Session == nil || len(result.Artifacts) != 1 || len(result.Artifacts[0].Files) != 2 {
		t.Fatal("protected screenshot failed or lost its session/artifact")
	}
	assertBrowserSession(t, *result.Session, binding.Reference, "connected")
	if !reflect.DeepEqual(before, screenshotSourceState(t, manager, binding, options)) {
		t.Fatal("screenshot changed the source page, viewport, media or original targets")
	}
	var data []byte
	for _, file := range result.Artifacts[0].Files {
		if file.Name == "screenshot.png" {
			data = file.Data
		}
		if file.Name == "screenshot.json" && !bytes.Equal(file.Data, result.Output) {
			t.Fatal("screenshot metadata sidecar differs from its result")
		}
	}
	return decodeScreenshot(t, result.Output, data, "desktop")
}

func setScreenshotSource(t *testing.T, manager ownedBrowserSessionManager, binding sessions.Binding, options browserruntime.Options, title, body string) {
	t.Helper()
	titleJSON, _ := json.Marshal(title)
	bodyJSON, _ := json.Marshal(body)
	_, err := manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`document.title=%s;document.body.innerHTML=%s;`, titleJSON, bodyJSON), nil))
	})
	if err != nil {
		t.Fatal("prepare protected screenshot source")
	}
}

func screenshotSourceState(t *testing.T, manager ownedBrowserSessionManager, binding sessions.Binding, options browserruntime.Options) struct {
	Digest  [sha256.Size]byte
	Targets []string
} {
	t.Helper()
	var result struct {
		Digest  [sha256.Size]byte
		Targets []string
	}
	_, err := manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
		var state string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`JSON.stringify({url:location.href,title:document.title,body:document.body.innerHTML,width:innerWidth,height:innerHeight,ratio:devicePixelRatio,mobile:matchMedia('(max-width:600px)').matches,dark:matchMedia('(prefers-color-scheme:dark)').matches})`, &state)); err != nil {
			return err
		}
		result.Digest = sha256.Sum256([]byte(state))
		targets, err := target.GetTargets().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		if err != nil {
			return err
		}
		for _, info := range targets {
			if info.Type == "page" {
				result.Targets = append(result.Targets, string(info.TargetID))
			}
		}
		slices.Sort(result.Targets)
		return nil
	})
	if err != nil {
		t.Fatal("inspect retained screenshot source state")
	}
	return result
}

type screenshotRenderingWait struct {
	entered chan struct{}
	once    sync.Once
}

func (probe *screenshotRenderingWait) Log(ctx context.Context, message string) error {
	if message != "Rendering the protected screenshot" {
		return nil
	}
	probe.once.Do(func() { close(probe.entered) })
	<-ctx.Done()
	return ctx.Err()
}

func publishScreenshot(t *testing.T, peer *browserSessionPeer, binding sessions.Binding, viewport string) messages.Message {
	t.Helper()
	input := screenshotInput(binding)
	if viewport != "" {
		input["viewport"] = viewport
	}
	command, err := agentcommands.NewRunAction(binding.OwnerAgentID, agentcommands.ActionCall{ActionName: "screenshot", Input: marshalLoginInput(t, input), Work: messages.WorkContext{OperationID: binding.Creation.OperationID, TaskID: id.TaskID()}, Session: &binding})
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentcommands.Client{Bus: peer.bus}).Publish(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	return command
}

func receiveScreenshot(t *testing.T, peer *browserSessionPeer, command messages.Message) agent.ActionOutput {
	t.Helper()
	message := peer.wait(func(message messages.Message) bool {
		return message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested || message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed)
	})
	if message.Type != controllerevents.ActionSucceeded {
		t.Fatal("screenshot failed or requested questions/credentials")
	}
	var output agent.ActionOutput
	if json.Unmarshal(message.Payload, &output) != nil || output.Validate() != nil {
		t.Fatal("invalid screenshot action result")
	}
	peer.accept(message)
	return output
}
