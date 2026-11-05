//go:build browserintegration

package browser_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/agent/action"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

func TestLoginTransientCodeWorkflowAndProtection(t *testing.T) {
	h, site := newRuntime(t), fixtures.NewBrowser(t)
	binding := h.Binding()
	command := h.Call("login", otpInput(site), binding)
	username, password := loginSecrets(site.URL, site.Accounts[0].Profile)
	h.Secret(username, site.Accounts[0].Username)
	h.Secret(password, site.Accounts[0].Password)
	request := waitCode(t, h, command, username)
	if site.Posts.Load() != 1 || site.CodePosts.Load() != 0 {
		t.Fatal("password login did not stop before the code submission")
	}
	delivery := h.Transient(request, site.Code, "")
	runtimeCheck(t, (agentevents.Client{Bus: h.bus}).Publish(t.Context(), delivery))
	receipt := h.Wait(func(m messages.Message) bool {
		return m.Type == controllerevents.SecretTransientReceived && m.CorrelationID == delivery.ID
	})
	runtimeCheck(t, controllerevents.ValidateTransientSecretReceivedDelivery(receipt, delivery))
	assertOTPResult(t, h.Result(command), binding, "verified", true)
	if site.CodePosts.Load() != 1 {
		t.Fatal("duplicate code delivery repeated the website submission")
	}
	binding.Mode = sessions.Reuse
	assertOTPResult(t, h.Result(h.Call("login", otpInput(site), binding)), binding, "verified", false)
	const key = "fixture-otp-model-key-9a12bc"
	protected := []string{site.Accounts[0].Username, site.Accounts[0].Password, site.Code, key}
	model := fakes.NewModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		if !strings.Contains(turn.Observation.Text, site.Accounts[0].Label) {
			t.Error("code login did not retain its authenticated account")
		}
		return groundedFinish(turn)
	}, protected...)
	browse := h.Call("browse", map[string]any{"objective": "Verify the signed-in account.", "url": site.URL + "/otp-account"}, binding)
	h.Setup(browse, model, key)
	output := h.Result(browse)
	for _, file := range []string{"findings.json", "evidence.json"} {
		assertNoPrivate(t, h.Download(output, file), protected...)
	}
	checkProtectedScreenshots(t, h, binding, protected)
	for _, event := range h.history {
		assertNoPrivate(t, event.Payload, protected...)
	}
	if site.Posts.Load() != 1 || site.CodePosts.Load() != 1 || model.Calls() != 2 {
		t.Fatal("session reuse replayed login or model work")
	}
}

func TestLoginTransientCodeContinuationNeedsFreshDelivery(t *testing.T) {
	h, site := newRuntime(t), fixtures.NewBrowser(t)
	binding, _ := seedCodePage(t, h, site, "/otp-code", nil)
	username, _ := loginSecrets(site.URL, site.Accounts[0].Profile)
	first := h.Call("login", otpInput(site), binding)
	request := waitCode(t, h, first, username)
	oldDelivery := h.Transient(request, "incorrect-code", "")
	assertOTPResult(t, h.Result(first), binding, "requires_challenge", true)
	if site.Posts.Load() != 0 || site.CodePosts.Load() != 1 {
		t.Fatal("retained code challenge restarted password login or retried an invalid code")
	}
	second := h.Call("login", otpInput(site), binding)
	current := waitCode(t, h, second, username)
	if current.ID == request.ID {
		t.Fatal("a later execution reused a settled code request")
	}
	runtimeCheck(t, (agentevents.Client{Bus: h.bus}).Publish(t.Context(), oldDelivery))
	retry := waitCode(t, h, second, username)
	if retry.ID != current.ID || site.CodePosts.Load() != 1 {
		t.Fatal("an old response satisfied a different request with the same secret name")
	}
	h.Transient(current, "", agentevents.TransientSecretExpired)
	assertOTPResult(t, h.Result(second), binding, "requires_challenge", false)
	third := h.Call("login", otpInput(site), binding)
	h.Transient(waitCode(t, h, third, username), site.Code, "")
	assertOTPResult(t, h.Result(third), binding, "verified", true)
	if site.Posts.Load() != 0 || site.CodePosts.Load() != 2 {
		t.Fatal("expired code delivery submitted or restarted an authentication step")
	}
}

func TestLoginTransientCodeStopsAtChangedFormUncertainPostOrClose(t *testing.T) {
	for _, scenario := range []string{"changed_form", "uncertain_post", "close_pending"} {
		t.Run(scenario, func(t *testing.T) {
			h, site := newRuntime(t), fixtures.NewBrowser(t)
			path := "/otp-code"
			if scenario == "changed_form" {
				path = "/otp-mutable-code"
			} else if scenario == "uncertain_post" {
				path = "/otp-drop-code"
			}
			var fetchPosts, networkPosts atomic.Int32
			binding, stop := seedCodePage(t, h, site, path, func(event any) {
				switch event := event.(type) {
				case *fetch.EventRequestPaused:
					if event.Request.Method == "POST" {
						fetchPosts.Add(1)
					}
				case *network.EventRequestWillBeSent:
					if event.Request.Method == "POST" {
						networkPosts.Add(1)
					}
				}
			})
			defer stop()
			command := h.Call("login", otpInput(site), binding)
			username, _ := loginSecrets(site.URL, site.Accounts[0].Profile)
			request := waitCode(t, h, command, username)
			if site.CodePosts.Load() != 0 {
				t.Fatal("code was submitted before its managed delivery")
			}
			if scenario == "close_pending" {
				closed := h.Control(binding, "close")
				if len(closed.Sessions) != 1 || closed.Sessions[0].State != "closed" {
					t.Fatal("session close did not join the pending code request")
				}
				failed := h.Wait(func(m messages.Message) bool {
					return m.CorrelationID == command.ID && (m.Type == controllerevents.CommandFailed || m.Type == controllerevents.ActionSucceeded)
				})
				if failed.Type != controllerevents.CommandFailed {
					t.Fatal("closed code login resumed or claimed success")
				}
				h.Accept(failed)
			} else {
				if scenario == "changed_form" {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					err := site.ChangeCodeDestination(ctx)
					cancel()
					runtimeCheck(t, err)
				}
				h.Transient(request, site.Code, "")
				output := h.Result(command)
				var result actions.LoginOutput
				runtimeCheck(t, json.Unmarshal(output.Output, &result))
				if scenario == "uncertain_post" {
					// Lightpanda may repeat a lost-response POST below CDP. Assert
					// one owned request and honest uncertainty, not wire exactly-once.
					if result.Status != "submission_uncertain" || !result.Submitted || fetchPosts.Load() != 1 || networkPosts.Load() != 1 || site.CodePosts.Load() < 1 {
						t.Fatalf("uncertain code result: status=%s submitted=%t fetch=%d network=%d posts=%d", result.Status, result.Submitted, fetchPosts.Load(), networkPosts.Load(), site.CodePosts.Load())
					}
				} else if result.Status == "verified" || result.Submitted || site.CodePosts.Load() != 0 {
					t.Fatal("changed code form received credentials or a success claim")
				}
				if scenario == "changed_form" {
					var body string
					_, err := h.manager.Run(action.WithSession(t.Context(), &binding), h.options, func(ctx context.Context) error {
						return chromedp.Run(ctx, chromedp.Evaluate(`document.body.innerHTML`, &body))
					})
					runtimeCheck(t, err)
					assertNoPrivate(t, []byte(body), site.Code)
				}
				for _, event := range h.history {
					assertNoPrivate(t, event.Payload, site.Code)
				}
			}
			if site.Foreign.Load() != 0 || site.Posts.Load() != 0 || scenario != "uncertain_post" && (fetchPosts.Load() != 0 || networkPosts.Load() != 0) {
				t.Fatal("stopped code login leaked to a foreign endpoint or submitted again")
			}
		})
	}
}

func otpInput(site *fixtures.BrowserSite) map[string]any {
	input := loginInputFor(site, 0)
	input["url"], input["verification_url"] = site.URL+"/otp-login", site.URL+"/otp-account"
	return input
}

func waitCode(t *testing.T, h *runtimeHarness, command messages.Message, usernameName string) messages.Message {
	t.Helper()
	message := h.Wait(func(m messages.Message) bool {
		return m.Type == controllerevents.SecretSyncRequested || m.Type == controllerevents.QuestionRequested || m.CorrelationID == command.ID && (m.Type == controllerevents.CommandFailed || m.Type == controllerevents.ActionSucceeded)
	})
	if message.Type != controllerevents.SecretSyncRequested {
		t.Fatal("supported code form did not request a managed transient value")
	}
	request, err := controllerevents.DecodeSecretSyncRequested(message)
	runtimeCheck(t, err)
	defer clear(request.Default)
	if request.Request.CommandID != command.ID || request.Request.Key.Name != strings.TrimSuffix(usernameName, "-username")+"-otp" {
		t.Fatal("code request escaped its current execution or origin/profile purpose")
	}
	return message
}

func assertOTPResult(t *testing.T, output agent.ActionOutput, binding sessions.Binding, status string, submitted bool) {
	t.Helper()
	var result actions.LoginOutput
	runtimeCheck(t, json.Unmarshal(output.Output, &result))
	if result.Status != status || result.Submitted != submitted || output.Session == nil || output.Session.Reference != binding.Reference {
		t.Fatalf("code login result: status=%s submitted=%t session_retained=%t", result.Status, result.Submitted, output.Session != nil && output.Session.Reference == binding.Reference)
	}
}

func seedCodePage(t *testing.T, h *runtimeHarness, site *fixtures.BrowserSite, path string, listen func(any)) (sessions.Binding, context.CancelFunc) {
	t.Helper()
	binding := h.Binding()
	runtimeCheck(t, h.manager.Prepare(t.Context(), binding.Claim()))
	stop := func() {}
	_, err := h.manager.Run(action.WithSession(t.Context(), &binding), h.options, func(ctx context.Context) error {
		if listen != nil {
			var listening context.Context
			listening, stop = context.WithCancel(context.WithoutCancel(ctx))
			chromedp.ListenTarget(listening, listen)
		}
		return chromedp.Run(ctx, chromedp.Navigate(site.URL+path))
	})
	runtimeCheck(t, err)
	t.Cleanup(stop)
	binding.Mode = sessions.Reuse
	return binding, stop
}
