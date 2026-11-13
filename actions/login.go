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
	browserlogin "github.com/defos-ai/commander-browser-agent/browser/login"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

const LoginName = "login"
const LoginDescription = "Use managed credentials and one-time codes to sign in on a Hermes/Lightpanda session, check a configured success marker on the same origin, and retain the session for Commander continuations. Login does not invoke a model. No session IDs belong in action input."

type LoginInput struct {
	URL               string `json:"url"`
	VerificationURL   string `json:"verification_url"`
	ExpectedText      string `json:"expected_text"`
	CredentialProfile string `json:"credential_profile,omitempty"`
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty"`
}

type LoginOutput struct {
	Engine browserruntime.Engine `json:"engine"`
	browserlogin.Result
}

type Login struct {
	*action.Base
	runner         BrowserRunner
	configuration  atomic.Pointer[browserruntime.HermesOptions]
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
		Base: action.NewBase(actionmodel.Action{Name: LoginName, Description: LoginDescription, SessionMode: "open",
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
			status, discoveryErr = action.Unknown, errors.Join(err, context.Cause(ctx))
		}
	}()
	configuration, status, err := discoverRuntime(ctx, a.Base, a.runner, env, scope)
	if err == nil && status == action.Ready {
		a.configuration.Store(configuration)
	}
	return status, err
}

func (a *Login) Run(ctx context.Context, raw json.RawMessage) (result action.Result, runErr error) {
	if err := ctx.Err(); err != nil {
		return action.Result{}, errors.Join(err, context.Cause(ctx))
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
	if _, err := action.OpenSession(ctx); err != nil {
		return action.Result{}, err
	}
	setup, stopSetup := context.WithTimeout(ctx, 10*time.Minute)
	var checkpoints *browsing.HermesCheckpoints
	err := a.runner.WithSession(setup, func(ctx context.Context) error {
		var err error
		checkpoints, err = browsing.NewHermesCheckpoints(ctx, LoginName)
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
	})
	if setup.Err() != nil {
		err = errors.Join(err, setup.Err(), context.Cause(setup))
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
	if err := action.Log(budget.Context(), "Starting managed login"); err != nil {
		return action.Result{}, err
	}
	if err := a.runner.WithSession(budget.Context(), func(ctx context.Context) error {
		return checkpoints.Start(ctx, browsing.HermesCheckpointLimits{TimeoutSeconds: input.TimeoutSeconds})
	}); err != nil {
		return action.Result{}, loginActionError(budget.Context(), err)
	}
	var outcome browserlogin.Result
	var snapshot sessions.Session
	snapshot, runErr = a.runner.RunHermes(budget.Context(), *configuration, func(ctx context.Context) error {
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
		outcome, err = browserlogin.RunHermes(ctx, browserlogin.Request{
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
	var encoded []byte
	finishErr := a.runner.WithSession(budget.Context(), func(ctx context.Context) error {
		status := "failed"
		var outputErr error
		if runErr == nil {
			output := LoginOutput{Engine: browserruntime.Lightpanda, Result: outcome}
			output.Status = sensitive.Redact(ctx, output.Status)
			output.Reason = sensitive.Redact(ctx, output.Reason)
			encoded, outputErr = json.Marshal(output)
			if outputErr == nil {
				outputErr = validateBrowseJSON(a.validateOutput, encoded)
			}
			if outputErr == nil {
				outputErr = action.Log(ctx, "Managed login finished; inspect its verification status")
			}
			if outputErr == nil {
				status = output.Status
			}
		}
		if err := checkpoints.Finish(ctx, status, 0, outcome.Submitted); err != nil {
			return err
		}
		return outputErr
	})
	runErr = errors.Join(runErr, finishErr)
	if budget.Context().Err() != nil {
		runErr = errors.Join(runErr, budget.Err(), context.Cause(budget.Context()))
	}
	if runErr != nil {
		clear(encoded)
		return result, loginActionError(budget.Context(), runErr)
	}
	result.Output = encoded
	return result, nil
}

func loginActionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), context.Cause(ctx))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("managed login could not be completed; inspect the session before an explicit continuation")
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
    "timeout_seconds":{"type":"integer","minimum":5,"maximum":180,"default":120}
  },"required":["url","verification_url","expected_text"]
}`

const loginOutputSchema = `{
  "type":"object","additionalProperties":false,"properties":{
    "engine":{"type":"string","enum":["lightpanda"]},
    "status":{"type":"string","enum":["verified","unverified","requires_challenge","unsupported_form","submission_uncertain"]},
    "submitted":{"type":"boolean"},"reason":{"type":"string"}
  },"required":["engine","status","submitted","reason"]
}`
