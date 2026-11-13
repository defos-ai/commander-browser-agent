package actions

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	browserscreenshot "github.com/defos-ai/commander-browser-agent/browser/screenshot"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/artifacts"
)

const ScreenshotName = "screenshot"
const ScreenshotDescription = "Capture a protected text-only PNG snapshot of the existing Hermes/Lightpanda session bound by Commander. Continue a session-aware task or supply a trusted Commander binding; no session IDs belong in action input."

type ScreenshotInput struct {
	Viewport       string `json:"viewport,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// ScreenshotOutput also accompanies the PNG as its artifact metadata file.
type ScreenshotOutput struct {
	Engine    browserruntime.Engine `json:"engine"`
	Renderer  string                `json:"renderer"`
	Viewport  string                `json:"viewport"`
	Width     int                   `json:"width"`
	Height    int                   `json:"height"`
	MediaType string                `json:"media_type"`
	Truncated bool                  `json:"truncated"`
}

type Screenshot struct {
	*action.Base
	runner         BrowserRunner
	configuration  atomic.Pointer[browserruntime.HermesOptions]
	validateInput  func(any) error
	validateOutput func(any) error
}

func NewScreenshot(runner BrowserRunner) *Screenshot {
	input, err := action.ResolveSchema(json.RawMessage(screenshotInputSchema))
	if err != nil {
		panic("invalid built-in screenshot input schema: " + err.Error())
	}
	output, err := action.ResolveSchema(json.RawMessage(screenshotOutputSchema))
	if err != nil {
		panic("invalid built-in screenshot output schema: " + err.Error())
	}
	return &Screenshot{
		Base: action.NewBase(actionmodel.Action{Name: ScreenshotName, Description: ScreenshotDescription, SessionMode: "use",
			InputSchema: json.RawMessage(screenshotInputSchema), OutputSchema: json.RawMessage(screenshotOutputSchema)}),
		runner: runner, validateInput: input.Validate, validateOutput: output.Validate,
	}
}

func (a *Screenshot) Discover(ctx context.Context, env environment.Environment, scope *environment.DiscoveryScope) (status action.Status, discoveryErr error) {
	a.configuration.Store(nil)
	a.SetStatus(action.Unknown)
	defer func() {
		if err := ctx.Err(); err != nil {
			a.configuration.Store(nil)
			a.SetStatus(action.Unknown)
			status, discoveryErr = action.Unknown, errors.Join(err, context.Cause(ctx))
		}
	}()
	configuration, status, err := discoverRuntime(ctx, a.Base, a.runner, env, scope)
	if err == nil && status == action.Ready {
		a.configuration.Store(configuration)
	}
	return status, err
}

func (a *Screenshot) Run(ctx context.Context, raw json.RawMessage) (action.Result, error) {
	if err := ctx.Err(); err != nil {
		return action.Result{}, errors.Join(err, context.Cause(ctx))
	}
	if err := a.Check(); err != nil {
		return action.Result{}, err
	}
	configuration := a.configuration.Load()
	if configuration == nil || a.runner == nil {
		return action.Result{}, errors.New("screenshot is unavailable; run discovery first")
	}
	if len(raw) > 8192 || validateBrowseJSON(a.validateInput, raw) != nil {
		return action.Result{}, errors.New("screenshot input does not match its schema")
	}
	input := ScreenshotInput{Viewport: "desktop", TimeoutSeconds: 30}
	if action.DecodeInput(raw, &input) != nil || input.TimeoutSeconds < 5 || input.TimeoutSeconds > 60 {
		return action.Result{}, errors.New("screenshot requires a bounded timeout")
	}
	width, height := 1280, 720
	switch input.Viewport {
	case "desktop":
	case "mobile":
		width, height = 390, 844
	default:
		return action.Result{}, errors.New("screenshot viewport must be desktop or mobile")
	}
	if _, err := action.UseSession(ctx); err != nil {
		return action.Result{}, err
	}
	budget, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := action.Log(budget, "Capturing a protected text snapshot from the retained browser page"); err != nil {
		return action.Result{}, err
	}
	var capture browserscreenshot.Image
	snapshot, runErr := a.runner.RunHermes(budget, *configuration, func(ctx context.Context) error {
		var err error
		capture, err = browserscreenshot.CaptureHermes(ctx, width, height)
		return err
	})
	result := action.Result{Session: &snapshot}
	if budget.Err() != nil {
		clear(capture.PNG)
		return result, errors.Join(budget.Err(), context.Cause(budget))
	}
	if runErr != nil {
		clear(capture.PNG)
		if errors.Is(runErr, context.Canceled) {
			return result, context.Canceled
		}
		if errors.Is(runErr, context.DeadlineExceeded) {
			return result, context.DeadlineExceeded
		}
		return result, errors.New("cannot capture the protected screenshot; inspect the session before retrying")
	}
	output := ScreenshotOutput{Engine: browserruntime.Lightpanda, Renderer: browserscreenshot.Renderer,
		Viewport: input.Viewport, Width: width, Height: height, MediaType: "image/png", Truncated: capture.Truncated}
	encoded, err := json.Marshal(output)
	if err != nil || validateBrowseJSON(a.validateOutput, encoded) != nil {
		clear(capture.PNG)
		return result, errors.New("screenshot metadata does not match its schema")
	}
	if err := action.Log(budget, "Protected Lightpanda PNG and renderer metadata are ready"); err != nil {
		clear(capture.PNG)
		return result, err
	}
	if budget.Err() != nil {
		clear(capture.PNG)
		return result, errors.Join(budget.Err(), context.Cause(budget))
	}
	result.Output = encoded
	result.Artifacts = []action.Artifact{{Name: "Lightpanda protected text screenshot", Files: []artifacts.FileSource{
		{Name: "screenshot.png", MediaType: "image/png", Data: capture.PNG},
		{Name: "screenshot.json", MediaType: "application/json", Data: encoded},
	}}}
	return result, nil
}

const screenshotInputSchema = `{
  "type":"object","additionalProperties":false,"properties":{
    "viewport":{"type":"string","enum":["desktop","mobile"],"default":"desktop","description":"Size of the protected text rendering: desktop 1280x720 or mobile 390x844. These are viewport sizes, not device emulation."},
    "timeout_seconds":{"type":"integer","minimum":5,"maximum":60,"default":30}
  }
}`

const screenshotOutputSchema = `{
  "type":"object","additionalProperties":false,"properties":{
    "engine":{"type":"string","enum":["lightpanda"]},
    "renderer":{"type":"string","enum":["lightpanda-text-snapshot"]},
    "viewport":{"type":"string","enum":["desktop","mobile"]},
    "width":{"type":"integer","enum":[390,1280]},"height":{"type":"integer","enum":[720,844]},
    "media_type":{"type":"string","enum":["image/png"]},"truncated":{"type":"boolean"}
  },"required":["engine","renderer","viewport","width","height","media_type","truncated"]
}`
