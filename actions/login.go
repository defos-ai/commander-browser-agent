package actions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/browser/lightpanda"
	browserlogin "github.com/defos-ai/commander-browser-agent/browser/login"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

const LoginName = "login"
const LoginDescription = "Sign in through supported username/password and one-time-code forms using managed secrets, verify a same-site success condition and retain the Lightpanda session for reuse"

type LoginInput struct {
	URL               string `json:"url"`
	VerificationURL   string `json:"verification_url"`
	ExpectedText      string `json:"expected_text"`
	CredentialProfile string `json:"credential_profile,omitempty"`
	SessionScope      string `json:"session_scope"`
	SessionID         string `json:"session_id,omitempty"`
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty"`
}

type LoginOutput struct {
	Engine browserruntime.Engine `json:"engine"`
	browserlogin.Result
}

type Login struct {
	*action.Base
	runner         BrowserRunner
	configuration  atomic.Pointer[lightpanda.Runtime]
	validateInput  func(any) error
	validateOutput func(any) error
}

func NewLogin(runner BrowserRunner) *Login {
	input, err := action.ResolveSchema(json.RawMessage(loginInputSchema))
	if err != nil {
		panic("invalid built-in login input schema: " + err.Error())
	}
	output, err := action.ResolveSchema(json.RawMessage(loginOutputSchema))
	if err != nil {
		panic("invalid built-in login output schema: " + err.Error())
	}
	return &Login{
		Base: action.NewBase(actionmodel.Action{Name: LoginName, Description: LoginDescription, SessionMode: "create",
			InputSchema: json.RawMessage(loginInputSchema), OutputSchema: json.RawMessage(loginOutputSchema)}),
		runner: runner, validateInput: input.Validate, validateOutput: output.Validate,
	}
}

func (a *Login) Discover(ctx context.Context, env environment.Environment, scope *environment.DiscoveryScope) (status action.Status, discoveryErr error) {
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

func (a *Login) Run(ctx context.Context, raw json.RawMessage) (result action.Result, runErr error) {
	if err := ctx.Err(); err != nil {
		return action.Result{}, err
	}
	if err := a.Check(); err != nil {
		return action.Result{}, err
	}
	configuration := a.configuration.Load()
	if configuration == nil || a.runner == nil {
		return action.Result{}, errors.New("login is unavailable; run discovery first")
	}
	if len(raw) > 32*1024 || validateBrowseJSON(a.validateInput, raw) != nil {
		return action.Result{}, errors.New("login input does not match its schema")
	}
	input := LoginInput{TimeoutSeconds: 120}
	if action.DecodeInput(raw, &input) != nil {
		return action.Result{}, errors.New("invalid login input")
	}
	if err := input.validate(); err != nil {
		return action.Result{}, err
	}
	binding, ok := action.SessionFromContext(ctx)
	if !ok || binding.Validate() != nil || (binding.Mode != sessions.Create && binding.Mode != sessions.Reuse) || binding.ScopeID != input.SessionScope ||
		binding.Mode == sessions.Create && input.SessionID != "" || binding.Mode == sessions.Reuse && input.SessionID != binding.ID {
		return action.Result{}, errors.New("login requires the matching framework browser session binding")
	}
	setup, stopSetup := context.WithTimeout(ctx, 10*time.Minute)
	var checkpoints *browsing.Checkpoints
	var err error
	prepare := func(ctx context.Context) error {
		checkpoints, err = browsing.NewCheckpoints(ctx, LoginName)
		if err != nil {
			return err
		}
		if input.CredentialProfile == "" {
			question, err := action.Question(ctx, "browser_login_profile", "Which non-secret credential profile should this login use? Enter a lowercase alias such as work or personal, not a username, password or verification code.")
			if err != nil {
				return err
			}
			input.CredentialProfile = strings.TrimSpace(question.Answer)
		}
		if !browsing.ValidLoginProfile(input.CredentialProfile) {
			return errors.New("login credential_profile must be a non-secret lowercase alias of at most 32 characters")
		}
		return nil
	}
	if runner, ok := a.runner.(browserSetupRunner); ok {
		err = runner.WithSession(setup, prepare)
	} else {
		err = prepare(setup)
	}
	if err == nil {
		err = setup.Err()
	}
	stopSetup()
	if err != nil {
		return action.Result{}, err
	}
	usernameName, passwordName, err := browsing.LoginSecretNames(input.URL, input.CredentialProfile)
	if err != nil {
		return action.Result{}, err
	}
	codeName, err := browsing.LoginCodeSecretName(input.URL, input.CredentialProfile)
	if err != nil {
		return action.Result{}, err
	}
	budget := browsing.NewBudget(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer budget.Close()
	var outcome browserlogin.Result
	defer func() {
		status := "failed"
		if runErr == nil && len(result.Output) != 0 {
			status = outcome.Status
		}
		finish := func(ctx context.Context) error {
			return checkpoints.Finish(ctx, status, nil, outcome.Submitted)
		}
		var finishErr error
		if runner, ok := a.runner.(browserSetupRunner); ok {
			finishErr = runner.WithSession(budget.Context(), finish)
		} else {
			finishErr = finish(budget.Context())
		}
		if err := budget.Err(); err != nil {
			finishErr = err
		}
		if finishErr != nil {
			result = action.Result{Session: result.Session}
			if !errors.Is(finishErr, context.Canceled) && !errors.Is(finishErr, context.DeadlineExceeded) {
				finishErr = errors.New("cannot finalize browser progress; inspect the session before retrying")
			}
			runErr = errors.Join(runErr, finishErr)
		}
	}()
	if err := action.Log(budget.Context(), "Starting managed login"); err != nil {
		return action.Result{}, err
	}
	var snapshot sessions.Session
	snapshot, runErr = a.runner.Run(budget.Context(), browserruntime.Options{Engine: browserruntime.Lightpanda, Executable: configuration.Executable()}, func(ctx context.Context) error {
		if err := checkpoints.Start(ctx, browsing.CheckpointLimits{TimeoutSeconds: input.TimeoutSeconds}, "login"); err != nil {
			return err
		}
		request := func(ctx context.Context, name string, secretType action.SecretType) ([]byte, error) {
			budget.Pause()
			defer budget.Resume()
			waiting, stop := context.WithTimeout(ctx, 3*time.Minute)
			defer stop()
			if secretType == action.SecretTransient {
				if err := action.Log(waiting, "Waiting for a one-time login code"); err != nil {
					return nil, err
				}
				value, err := action.RequestTransientSecret(waiting, name)
				// Request TTL/withdrawal can end before the action's wait. Keep a
				// live session available for explicit continuation in that case.
				if err != nil && waiting.Err() == nil {
					clear(value)
					return nil, action.ErrSecretUnavailable
				}
				return value, err
			}
			return action.RequestReusableSecret(waiting, name)
		}
		var err error
		outcome, err = browserlogin.Run(ctx, browserlogin.Request{
			URL: input.URL, VerificationURL: input.VerificationURL, ExpectedText: input.ExpectedText,
			OneTimeCode: func(ctx context.Context) ([]byte, error) {
				return request(ctx, codeName, action.SecretTransient)
			},
		}, func(ctx context.Context) (browserlogin.Credentials, error) {
			username, err := request(ctx, usernameName, action.SecretReusable)
			if err != nil {
				return browserlogin.Credentials{}, err
			}
			password, err := request(ctx, passwordName, action.SecretReusable)
			if err != nil {
				clear(username)
				return browserlogin.Credentials{}, err
			}
			return browserlogin.Credentials{Username: username, Password: password}, nil
		})
		return err
	})
	result = action.Result{Session: &snapshot}
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
		return result, errors.New("managed login could not be completed; inspect the session before retrying")
	}
	encoded, err := json.Marshal(LoginOutput{Engine: browserruntime.Lightpanda, Result: outcome})
	if err != nil || validateBrowseJSON(a.validateOutput, encoded) != nil {
		return result, errors.New("login result does not match its schema")
	}
	if err := action.Log(budget.Context(), "Managed login finished; inspect its verification status"); err != nil {
		return result, err
	}
	if err := budget.Err(); err != nil {
		return result, err
	}
	result.Output = encoded
	return result, nil
}

func (input LoginInput) validate() error {
	origin, err := browsing.LoginOrigin(input.URL)
	if err != nil {
		return err
	}
	verificationOrigin, err := browsing.LoginOrigin(input.VerificationURL)
	if err != nil || origin != verificationOrigin {
		return errors.New("verification_url must use the same exact origin as the login URL")
	}
	if !utf8.ValidString(input.ExpectedText) || strings.TrimSpace(input.ExpectedText) == "" || len(input.ExpectedText) > 256 || strings.ContainsRune(input.ExpectedText, '\x00') {
		return errors.New("expected_text must be a non-secret verification marker of at most 256 UTF-8 bytes")
	}
	if input.CredentialProfile != "" && !browsing.ValidLoginProfile(input.CredentialProfile) {
		return errors.New("credential_profile must be a lowercase non-secret alias of at most 32 characters")
	}
	if !id.Valid(input.SessionScope) || input.SessionID != "" && !id.Valid(input.SessionID) {
		return errors.New("login requires canonical session_scope and session_id values")
	}
	if input.TimeoutSeconds < 5 || input.TimeoutSeconds > 180 {
		return errors.New("login timeout_seconds must be between 5 and 180")
	}
	return nil
}

const loginInputSchema = `{
  "type":"object","additionalProperties":false,"properties":{
    "url":{"type":"string","minLength":1,"maxLength":4096},
    "verification_url":{"type":"string","minLength":1,"maxLength":4096},
    "expected_text":{"type":"string","minLength":1,"maxLength":256,"description":"Non-secret visible marker on the verification page used to check sign-in."},
    "credential_profile":{"type":"string","pattern":"^[a-z][a-z0-9_-]{0,31}$"},
    "session_scope":{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},
    "session_id":{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},
    "timeout_seconds":{"type":"integer","minimum":5,"maximum":180,"default":120}
  },"required":["url","verification_url","expected_text","session_scope"]
}`

const loginOutputSchema = `{
  "type":"object","additionalProperties":false,"properties":{
    "engine":{"type":"string","enum":["lightpanda"]},
    "status":{"type":"string","enum":["verified","unverified","requires_challenge","unsupported_form","submission_uncertain"]},
    "submitted":{"type":"boolean"},"reason":{"type":"string"}
  },"required":["engine","status","submitted","reason"]
}`
