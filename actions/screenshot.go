package actions

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/browser/lightpanda"
	browserscreenshot "github.com/defos-ai/commander-browser-agent/browser/screenshot"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/artifacts"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

const ScreenshotName = "screenshot"
const ScreenshotDescription = "Capture a protected text-only PNG snapshot of an existing Lightpanda browser session"

type ScreenshotInput struct {
	SessionScope   string `json:"session_scope"`
	SessionID      string `json:"session_id"`
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
	configuration  atomic.Pointer[lightpanda.Runtime]
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
			status, discoveryErr = action.Unknown, err
		}
	}()
	if a.runner == nil {
		a.SetUnavailable("The host must provide its browser session runner", actionmodel.Requirement{Kind: actionmodel.ConfigurationRequirement, Name: "browser-session-provider"})
		return action.Unavailable, nil
	}
	if env == nil {
		env = &environment.Local{}
	}
	runtime, err := lightpanda.Discover(ctx, env, scope)
	if err != nil {
		var unavailable *lightpanda.UnavailableError
		if errors.As(err, &unavailable) {
			a.SetUnavailable(unavailable.Reason, unavailable.Requirement)
			return action.Unavailable, nil
		}
		return action.Unknown, err
	}
	if err := ctx.Err(); err != nil {
		return action.Unknown, err
	}
	a.configuration.Store(&runtime)
	a.SetStatus(action.Ready)
	return action.Ready, nil
}

func (a *Screenshot) Run(ctx context.Context, raw json.RawMessage) (action.Result, error) {
	if err := ctx.Err(); err != nil {
		return action.Result{}, err
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
	if action.DecodeInput(raw, &input) != nil || !id.Valid(input.SessionScope) || !id.Valid(input.SessionID) ||
		input.TimeoutSeconds < 5 || input.TimeoutSeconds > 60 {
		return action.Result{}, errors.New("screenshot requires a valid existing session and bounded timeout")
	}
	width, height := 1280, 720
	switch input.Viewport {
	case "desktop":
	case "mobile":
		width, height = 390, 844
	default:
		return action.Result{}, errors.New("screenshot viewport must be desktop or mobile")
	}
	binding, ok := action.SessionFromContext(ctx)
	if !ok || binding.Validate() != nil || binding.Mode != sessions.Use || binding.ID != input.SessionID || binding.ScopeID != input.SessionScope {
		return action.Result{}, errors.New("screenshot requires the matching framework-owned existing session binding")
	}
	budget, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := action.Log(budget, "Capturing a protected text snapshot from the retained browser page"); err != nil {
		return action.Result{}, err
	}
	var capture browserscreenshot.Image
	snapshot, runErr := a.runner.Run(budget, browserruntime.Options{Engine: browserruntime.Lightpanda, Executable: configuration.Executable()}, func(ctx context.Context) error {
		var err error
		capture, err = browserscreenshot.Capture(ctx, width, height)
		return err
	})
	result := action.Result{Session: &snapshot}
	if err := budget.Err(); err != nil {
		return result, err
	}
	if runErr != nil {
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
		return result, errors.New("screenshot metadata does not match its schema")
	}
	if err := action.Log(budget, "Protected Lightpanda PNG and renderer metadata are ready"); err != nil {
		return result, err
	}
	if err := budget.Err(); err != nil {
		return result, err
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
    "session_scope":{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},
    "session_id":{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},
    "viewport":{"type":"string","enum":["desktop","mobile"],"default":"desktop","description":"Size of the protected text rendering: desktop 1280x720 or mobile 390x844. These are viewport sizes, not device emulation."},
    "timeout_seconds":{"type":"integer","minimum":5,"maximum":60,"default":30}
  },"required":["session_scope","session_id"]
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
