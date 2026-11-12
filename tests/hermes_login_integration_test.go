//go:build hermesintegration

package browser_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	browserlogin "github.com/defos-ai/commander-browser-agent/browser/login"
	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent/action"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/secrets"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

type hermesLoginFixture struct {
	request browserlogin.Request
	profile string
	outcome chan browserlogin.Result
}

func nativeLoginFixture(site *fixtures.BrowserSite, account int, path, verification string) *hermesLoginFixture {
	return &hermesLoginFixture{profile: site.Accounts[account].Profile, request: browserlogin.Request{
		URL: site.URL + path, VerificationURL: site.URL + verification, ExpectedText: site.Accounts[account].Label,
	}}
}

func runHermesLoginFixture(ctx context.Context, fixture hermesLoginFixture) (result browserlogin.Result, err error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if fixture.outcome != nil {
		defer func() { fixture.outcome <- result }()
	}
	username, password, err := browsing.LoginSecretNames(fixture.request.URL, fixture.profile)
	if err != nil {
		return result, err
	}
	codeName, err := browsing.LoginCodeSecretName(fixture.request.URL, fixture.profile)
	if err != nil {
		return result, err
	}
	request := fixture.request
	request.OneTimeCode = func(ctx context.Context) ([]byte, error) {
		value, err := action.RequestTransientSecret(ctx, codeName)
		if err != nil && ctx.Err() == nil {
			clear(value)
			return nil, action.ErrSecretUnavailable
		}
		return value, err
	}
	return browserlogin.RunHermes(ctx, request, func(ctx context.Context) (browserlogin.Credentials, error) {
		user, err := action.RequestReusableSecret(ctx, username)
		if err != nil {
			return browserlogin.Credentials{}, err
		}
		pass, err := action.RequestReusableSecret(ctx, password)
		if err != nil {
			clear(user)
			return browserlogin.Credentials{}, err
		}
		return browserlogin.Credentials{Username: user, Password: pass}, nil
	})
}

func (h *hermesSessionHost) loginCredentials(command messages.Message, site *fixtures.BrowserSite, account int) {
	h.t.Helper()
	user, pass, err := browsing.LoginSecretNames(site.URL, site.Accounts[account].Profile)
	hermesSessionCheck(h.t, err)
	h.deliverModelKey(command, user, site.Accounts[account].Username, 1)
	h.deliverModelKey(command, pass, site.Accounts[account].Password, 1)
}

func (h *hermesSessionHost) loginCodeRequest(command messages.Message, site *fixtures.BrowserSite) messages.Message {
	h.t.Helper()
	message := h.setupMessage(command)
	if message.Type != controllerevents.SecretSyncRequested {
		h.t.Fatal("deterministic login requested a model or ended before its code challenge")
	}
	request, err := controllerevents.DecodeSecretSyncRequested(message)
	hermesSessionCheck(h.t, err)
	defer clear(request.Default)
	name, err := browsing.LoginCodeSecretName(site.URL, site.Accounts[0].Profile)
	hermesSessionCheck(h.t, err)
	if request.Request.Key.Name != name || request.Request.EffectiveType() != secrets.SecretTransient || request.Default != nil {
		h.t.Fatal("login code request lost its origin/profile purpose or transient type")
	}
	if request.Request.AgentID != h.host.ID() || request.Request.CommandID != command.ID || request.Request.OperationID != command.OperationID || request.Request.TaskID != command.TaskID {
		h.t.Fatal("login code request was not bound to its current leaf execution")
	}
	if request.Request.ExpiresAt.Sub(request.Request.RequestedAt) > 3*time.Minute {
		h.t.Fatal("login code request exceeded its bounded lifetime")
	}
	return message
}

func (h *hermesSessionHost) loginCode(message messages.Message, value, failure string) messages.Message {
	h.t.Helper()
	request, err := controllerevents.DecodeSecretSyncRequested(message)
	hermesSessionCheck(h.t, err)
	var owned []byte
	if failure == "" {
		owned = []byte(value)
		defer clear(owned)
	}
	delivery, err := agentevents.NewTransientSecretResponse(request.Request, owned, failure)
	hermesSessionCheck(h.t, err)
	h.accepted[message.ID] = delivery
	hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), delivery))
	return delivery
}

func requireNativeLogin(t *testing.T, output hermesSessionOutput, status string, submitted bool) {
	t.Helper()
	if output.Login == nil {
		t.Fatal("deterministic native login omitted its result")
	}
	actualStatus, reason := "unknown", "other"
	switch output.Login.Status {
	case "verified", "unverified", "requires_challenge", "unsupported_form", "submission_uncertain":
		actualStatus = output.Login.Status
	}
	switch output.Login.Reason {
	case "not_attempted", "existing_session", "verification_matched", "verification_not_matched", "verification_unavailable", "verification_unconfirmed",
		"submission_unconfirmed", "code_submission_unconfirmed", "code_not_verified", "code_unavailable", "invalid_code", "one_time_code_form",
		"unsafe_form_destination", "challenge_form_changed", "challenge_form_changed_after_fill", "unsupported_challenge_form", "login_navigation_unavailable":
		reason = output.Login.Reason
	}
	if output.Login.Status != status {
		t.Fatalf("deterministic native login status=%s reason=%s submitted=%t; expected status=%s", actualStatus, reason, output.Login.Submitted, status)
	}
	if output.Login.Submitted != submitted {
		t.Fatalf("deterministic native login submitted=%t reason=%s; expected submitted=%t", output.Login.Submitted, reason, submitted)
	}
	if output.Result.ModelRequests != 0 {
		t.Fatal("deterministic native login performed model requests")
	}
}

func assertNativeLoginPrivate(t *testing.T, h *hermesSessionHost, protected ...string) {
	t.Helper()
	for _, message := range h.history {
		for _, value := range protected {
			if bytes.Contains(message.Payload, []byte(value)) {
				t.Error("a login value entered public Commander events")
			}
		}
	}
}

func TestHermesManagedLoginOTPThenNativeBrowse(t *testing.T) {
	site := fixtures.NewBrowser(t)
	key := "fixture-native-otp-provider-" + id.New()
	protected := []string{site.Accounts[0].Username, site.Accounts[0].Password, site.Code}
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		if call == 1 {
			code := fmt.Sprintf("# Read the retained signed-in account\ngoto_url(%q)\nprint(js('document.body.textContent'))", site.URL+"/otp-account")
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("signed-in-account", "browser_exec", map[string]any{"code": code})}}
		}
		if call != 2 || !request.HasToolText(site.Accounts[0].Label) {
			t.Error("native browsing lost the deterministic login's authenticated cookies")
		}
		return fakes.HermesResponse{Text: "Signed-in fixture confirmed."}
	}, protected...)
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{
		"login":  {login: nativeLoginFixture(site, 0, "/otp-login", "/otp-account")},
		"browse": {request: hermesRequest(model, []byte(key))},
	})
	root := h.task(nil, sessions.Open, "login")
	binding := h.binding(root)
	command := h.dispatch(root, &binding)
	h.loginCredentials(command, site, 0)
	request := h.loginCodeRequest(command, site)
	if site.Posts.Load() != 1 || site.CodePosts.Load() != 0 || model.Calls() != 0 {
		t.Fatal("login reached a model or submitted a code before managed delivery")
	}
	delivery := h.loginCode(request, site.Code, "")
	hermesSessionCheck(t, (agentevents.Client{Bus: h.bus}).Publish(t.Context(), delivery))
	receipt := h.wait(func(message messages.Message) bool {
		return message.Type == controllerevents.SecretTransientReceived && message.CorrelationID == delivery.ID
	})
	hermesSessionCheck(t, controllerevents.ValidateTransientSecretReceivedDelivery(receipt, delivery))
	output, ok := h.finish(command)
	if !ok {
		t.Fatal("native OTP login did not finish")
	}
	requireNativeLogin(t, output, "verified", true)
	if output.Claim != binding.Claim() || output.Session.State != "connected" || site.CodePosts.Load() != 1 {
		t.Fatal("OTP login lost its original session or repeated submission")
	}
	reused := h.call(h.task(&root, sessions.Open, "login"))
	requireNativeLogin(t, reused, "verified", false)
	if site.Posts.Load() != 1 || site.CodePosts.Load() != 1 || model.Calls() != 0 {
		t.Fatal("verified-session reuse repeated login or performed model setup")
	}
	for _, message := range h.history {
		if message.Type == controllerevents.QuestionRequested {
			t.Error("deterministic login asked for model setup")
		}
	}
	browsed := h.call(h.task(&root, sessions.Open, "browse"))
	if browsed.Result.Status != "completed" || model.Calls() != 2 || browsed.Claim != binding.Claim() {
		t.Fatal("native browsing did not reuse the same authenticated session")
	}
	assertNativeLoginPrivate(t, h, append(protected, key)...)
}

func TestHermesManagedLoginIsolatesAccountsOnOneOrigin(t *testing.T) {
	site := fixtures.NewBrowser(t)
	key := "fixture-native-accounts-provider-" + id.New()
	protected := []string{site.Accounts[0].Username, site.Accounts[0].Password, site.Accounts[1].Username, site.Accounts[1].Password}
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		if call%2 == 1 {
			code := fmt.Sprintf("# Read the retained account\ngoto_url(%q)\nprint(js('document.body.textContent'))", site.URL+"/account")
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("isolated-account", "browser_exec", map[string]any{"code": code})}}
		}
		account := call/2 - 1
		if account > 1 || !request.HasToolText(site.Accounts[account].Label) || request.HasToolText(site.Accounts[1-account].Label) {
			t.Error("independent native logins shared an authenticated cookie jar")
		}
		return fakes.HermesResponse{Text: "Isolated account confirmed."}
	}, protected...)
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{
		"alpha":  {login: nativeLoginFixture(site, 0, "/login", "/account")},
		"beta":   {login: nativeLoginFixture(site, 1, "/login", "/account")},
		"browse": {request: hermesRequest(model, []byte(key))},
	})
	var roots [2]operationsTaskFixture
	for account, scenario := range []string{"alpha", "beta"} {
		root := h.task(nil, sessions.Open, scenario)
		binding := h.binding(root)
		command := h.dispatch(root, &binding)
		h.loginCredentials(command, site, account)
		output, ok := h.finish(command)
		if !ok {
			t.Fatal("isolated password login failed")
		}
		requireNativeLogin(t, output, "verified", true)
		roots[account] = operationsTaskFixture{task: root, binding: binding}
	}
	if model.Calls() != 0 || site.Posts.Load() != 2 || roots[0].binding.Reference == roots[1].binding.Reference {
		t.Fatal("independent logins performed model work or shared session identity")
	}
	for _, root := range roots {
		if output := h.call(h.task(&root.task, sessions.Open, "browse")); output.Result.Status != "completed" || output.Claim != root.binding.Claim() {
			t.Fatal("native account browsing changed its original session")
		}
	}
	if model.Calls() != 4 {
		t.Fatal("native account inspection exceeded its scripted model work")
	}
	assertNativeLoginPrivate(t, h, append(protected, key)...)
}

type operationsTaskFixture struct {
	task    operations.Task
	binding sessions.Binding
}

func TestHermesOTPContinuationRejectsStaleAndExpiredDeliveries(t *testing.T) {
	site := fixtures.NewBrowser(t)
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{"code": {login: nativeLoginFixture(site, 0, "/otp-code", "/otp-account")}})
	root := h.task(nil, sessions.Open, "code")
	binding := h.binding(root)
	command := h.dispatch(root, &binding)
	oldRequest := h.loginCodeRequest(command, site)
	oldDelivery := h.loginCode(oldRequest, "incorrect-fixture-code", "")
	output, ok := h.finish(command)
	if !ok {
		t.Fatal("rejected native code did not return its challenge state")
	}
	requireNativeLogin(t, output, "requires_challenge", true)
	child := h.task(&root, sessions.Open, "code")
	childBinding := h.binding(child)
	command = h.dispatch(child, &childBinding)
	current := h.loginCodeRequest(command, site)
	if current.ID == oldRequest.ID {
		t.Fatal("a later login execution reused its old transient request")
	}
	hermesSessionCheck(t, (agentevents.Client{Bus: h.bus}).Publish(t.Context(), oldDelivery))
	retry := h.loginCodeRequest(command, site)
	if retry.ID != current.ID || site.CodePosts.Load() != 1 {
		t.Fatal("an old code delivery satisfied another execution")
	}
	h.loginCode(current, "", agentevents.TransientSecretExpired)
	output, ok = h.finish(command)
	if !ok {
		t.Fatal("request-specific code expiry cancelled its live browser session")
	}
	requireNativeLogin(t, output, "requires_challenge", false)
	last := h.task(&child, sessions.Open, "code")
	lastBinding := h.binding(last)
	command = h.dispatch(last, &lastBinding)
	h.loginCode(h.loginCodeRequest(command, site), site.Code, "")
	output, ok = h.finish(command)
	if !ok {
		t.Fatal("fresh code delivery did not finish the retained challenge")
	}
	requireNativeLogin(t, output, "verified", true)
	if site.Posts.Load() != 0 || site.CodePosts.Load() != 2 || output.Claim != binding.Claim() {
		t.Fatal("challenge continuation restarted password login or retried a settled code")
	}
	assertNativeLoginPrivate(t, h, site.Code, "incorrect-fixture-code")
}

func TestHermesOTPStopsAtMutationPendingCloseAndUncertainPost(t *testing.T) {
	for _, scenario := range []string{"changed_form", "close_pending", "uncertain_post"} {
		t.Run(scenario, func(t *testing.T) {
			site := fixtures.NewBrowser(t)
			path := "/otp-code"
			if scenario == "changed_form" {
				path = "/otp-mutable-code"
			}
			if scenario == "uncertain_post" {
				path = "/otp-drop-code"
			}
			fixture := nativeLoginFixture(site, 0, path, "/otp-account")
			fixture.outcome = make(chan browserlogin.Result, 1)
			h := newHermesSessionHost(t, map[string]hermesSessionScenario{"code": {login: fixture}})
			root := h.task(nil, sessions.Open, "code")
			binding := h.binding(root)
			command := h.dispatch(root, &binding)
			request := h.loginCodeRequest(command, site)
			if site.CodePosts.Load() != 0 {
				t.Fatal("the browser submitted before a transient delivery")
			}
			if scenario == "close_pending" {
				if !h.control(binding, "close") {
					t.Fatal("session close did not join the pending code waiter")
				}
				if _, ok := h.finish(command); ok || h.snapshot(binding.Reference).State != "closed" {
					t.Fatal("closed code work resumed or reported success")
				}
			} else {
				if scenario == "changed_form" {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					err := site.ChangeCodeDestination(ctx)
					cancel()
					hermesSessionCheck(t, err)
				}
				h.loginCode(request, site.Code, "")
				output, ok := h.finish(command)
				if !ok && scenario == "uncertain_post" && h.snapshot(binding.Reference).State == "lost" {
					h.expectUncertainCleanup = true
					select {
					case outcome := <-fixture.outcome:
						output.Login = &outcome
					case <-time.After(2 * time.Second):
						t.Fatal("uncertain native login did not return its deterministic result")
					}
				} else if !ok {
					t.Fatal("deterministic code work did not return its bounded outcome")
				}
				if scenario == "changed_form" {
					requireNativeLogin(t, output, "requires_challenge", false)
					if site.CodeInputs.Load() != 0 || output.Login.Reason == "code_unavailable" {
						t.Fatal("a mutated form received code input or merely expired its fixture delivery")
					}
				} else {
					requireNativeLogin(t, output, "submission_uncertain", true)
					if site.CodePosts.Load() < 1 {
						t.Fatal("the uncertainty fixture did not reach its website POST")
					}
				}
			}
			if site.Foreign.Load() != 0 || site.Posts.Load() != 0 || scenario != "uncertain_post" && site.CodePosts.Load() != 0 {
				t.Fatal("stopped native login forwarded a secret or repeated submission")
			}
			requests := make(map[string]bool)
			for _, message := range h.history {
				if message.Type != controllerevents.SecretSyncRequested {
					continue
				}
				decoded, err := controllerevents.DecodeSecretSyncRequested(message)
				hermesSessionCheck(t, err)
				if decoded.Request.EffectiveType() == secrets.SecretTransient {
					requests[decoded.Request.ID] = true
				}
				clear(decoded.Default)
			}
			if len(requests) != 1 {
				t.Fatal("stopped native code work automatically requested another code")
			}
			assertNativeLoginPrivate(t, h, site.Code)
		})
	}
}

func TestHermesNativeEventOverflowLosesSessionBeforeFiltering(t *testing.T) {
	h := newHermesSessionHost(t, nil)
	root := h.task(nil, sessions.Open, "unused")
	binding := h.binding(root)
	hermesSessionCheck(t, h.manager.Prepare(t.Context(), binding.Claim()))
	config := hermesConfig()
	options := browserruntime.HermesOptions{Python: config.Python, Source: config.Source, Executable: config.Lightpanda}
	ctx := action.WithSession(t.Context(), &binding)
	var leaseErr, emitErr, drainErr error
	flooded := false
	_, runErr := h.manager.RunHermes(ctx, options, func(workCtx context.Context) error {
		var retained *hermes.Browser
		leaseErr = hermes.WithBrowser(workCtx, func(browser *hermes.Browser) error { retained = browser; return nil })
		if leaseErr != nil {
			return leaseErr
		}
		if retained == nil || !errors.Is(retained.Execute(workCtx, "Runtime.enable", map[string]any{}, nil), hermes.ErrClosed) {
			leaseErr = errors.New("retained native browser authority was not revoked")
			return leaseErr
		}
		detachedCalled := false
		if err := hermes.WithBrowser(context.WithoutCancel(workCtx), func(*hermes.Browser) error { detachedCalled = true; return nil }); !errors.Is(err, hermes.ErrClosed) || detachedCalled {
			leaseErr = errors.New("detached native browser authority was accepted")
			return leaseErr
		}
		return hermes.WithBrowser(workCtx, func(browser *hermes.Browser) error {
			emitErr = browser.Execute(workCtx, "Runtime.enable", map[string]any{}, nil)
			if emitErr != nil {
				return emitErr
			}
			// Console events are intentionally excluded from login's output
			// filter. Overflow must be detected on the complete daemon ring,
			// whose 500-entry eviction limit exceeds our 128-event boundary.
			emitErr = browser.Execute(workCtx, "Runtime.evaluate", map[string]any{
				"expression":    "for (let index=0; index<600; index++) console.log('native-event-fixture'); true",
				"returnByValue": true,
			}, nil)
			if emitErr != nil {
				return emitErr
			}
			flooded = true
			settled := time.NewTimer(100 * time.Millisecond)
			defer settled.Stop()
			select {
			case <-settled.C:
			case <-workCtx.Done():
				return workCtx.Err()
			}
			_, drainErr = browser.Drain(workCtx)
			return drainErr
		})
	})
	if flooded && h.snapshot(binding.Reference).State == "lost" {
		h.expectUncertainCleanup = true
	}
	if leaseErr != nil {
		t.Fatal("native browser authority outlived its callback or accepted a detached context")
	}
	if emitErr != nil {
		t.Fatal("the pinned runtime did not support the console event overflow fixture")
	}
	if drainErr == nil || runErr == nil {
		t.Fatal("discarded console events concealed an overflowing native event ring")
	}
	h.awaitState(binding.Reference, "lost")
	h.expectUncertainCleanup = true
	called := false
	_, err := h.manager.RunHermes(ctx, options, func(context.Context) error { called = true; return nil })
	if err == nil || called || h.snapshot(binding.Reference).Reference != binding.Reference {
		t.Fatal("event overflow discarded its lost reference or restored its native resource")
	}
}
