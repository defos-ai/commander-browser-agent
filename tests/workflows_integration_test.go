//go:build browserintegration

package browser_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

func TestBrowserAgentWorkflowAndAccountIsolation(t *testing.T) {
	h, site := newRuntime(t), fixtures.NewBrowser(t)
	keys := []string{"fixture-alpha-model-key-b8a23e", "fixture-beta-model-key-d861c4"}
	protected := append([]string{}, keys...)
	for _, account := range site.Accounts {
		protected = append(protected, account.Username, account.Password)
	}
	bindings := []sessions.Binding{h.Binding(), h.Binding()}
	for i := range bindings {
		bindings[i] = loginAccount(t, h, bindings[i], site, i)
	}
	const questionText, answerText = "Which study detail should I collect?", "operator-study-choice-3a72"
	model := fakes.NewModel(t, keys[0], func(turn intelligence.Turn) intelligence.Command {
		if !strings.Contains(turn.Observation.Text, site.Accounts[0].Label) || strings.Contains(turn.Observation.Text, site.Accounts[1].Label) {
			t.Error("account A did not retain its own authenticated state")
		}
		if len(turn.Clarifications) == 0 {
			return intelligence.Command{Action: "ask", Question: questionText, Options: []string{"Sample count", "Study name"}}
		}
		if turn.Clarifications[0].Answer != answerText {
			t.Error("saved clarification answer did not reach the browsing model")
		}
		if !strings.Contains(turn.Observation.Text, "42 samples") {
			typed := false
			for _, step := range turn.RecentSteps {
				typed = typed || step.Action == "type_text"
			}
			for _, target := range turn.Observation.Targets {
				if !typed && target.Label == "Search details" {
					return intelligence.Command{Action: "type_text", Target: target.ID, Text: "study"}
				}
				if typed && target.Label == "Load details" {
					return intelligence.Command{Action: "click", Target: target.ID}
				}
			}
			t.Error("model could not find the observed interaction target")
		}
		return groundedFinish(turn)
	}, append(protected, "hidden-form-value", "hidden-page-value")...)
	command := h.Call("browse", map[string]any{"objective": "Find the account study's sample count.", "url": site.URL + "/account"}, bindings[0])
	h.Setup(command, model, keys[0])
	request := h.Wait(func(m messages.Message) bool { return m.Type == controllerevents.QuestionRequested })
	question := h.Answer(request, "") // Saved pending questions keep the browser/model waiting.
	retry := h.Wait(func(m messages.Message) bool { return m.Type == controllerevents.QuestionRequested })
	if !reflect.DeepEqual(request, retry) || model.Calls() != 1 || question.Answer != "" {
		t.Fatal("pending clarification replayed model work or changed request identity")
	}
	h.Answer(retry, answerText)
	h.Answer(request, answerText)
	output := h.Result(command)
	var findings actions.BrowseOutput
	runtimeCheck(t, json.Unmarshal(output.Output, &findings))
	if findings.Status != "completed" || model.Calls() != 5 || len(findings.Evidence) != 1 || len(findings.Findings) != 1 || output.Session == nil || output.Session.ID != bindings[0].ID || !strings.Contains(findings.Evidence[0].Text, "42 samples") {
		t.Fatal("agent did not clarify, type, click and return grounded findings in its authenticated session")
	}
	for _, name := range []string{"findings.json", "evidence.json"} {
		data := h.Download(output, name)
		if !json.Valid(data) {
			t.Fatal("agent did not upload a valid findings artifact")
		}
		assertNoPrivate(t, data, append(protected, questionText, answerText)...)
	}
	facts := h.Facts(command)
	if len(facts) != 7 {
		t.Fatal("five-command browsing did not retain start, command and final recovery facts")
	}
	var finished struct {
		Session  sessions.CreationClaim `json:"session"`
		Phase    string                 `json:"phase"`
		Delivery string                 `json:"result_delivery"`
		Progress intelligence.Progress  `json:"progress"`
	}
	runtimeCheck(t, json.Unmarshal(facts["browse_finished"], &finished))
	digest := sha256.Sum256(runtimeJSON(t, findings.Evidence[0]))
	if finished.Session != bindings[0].Claim() || finished.Phase != "work_prepared" || finished.Delivery != "unconfirmed" || finished.Progress.Clarifications != 1 || len(finished.Progress.Evidence) != 1 || finished.Progress.Evidence[0].SHA256 != fmt.Sprintf("%x", digest) {
		t.Fatal("checkpoint lost its original ownership, evidence reference or delivery limits")
	}
	for _, fact := range facts {
		assertNoPrivate(t, fact, append(protected, questionText, answerText, site.URL, "42 samples")...)
	}
	checkProtectedScreenshots(t, h, bindings[0], []string{site.Accounts[0].Username, site.Accounts[0].Password, keys[0]})
	other := fakes.NewModel(t, keys[1], func(turn intelligence.Turn) intelligence.Command {
		if !strings.Contains(turn.Observation.Text, site.Accounts[1].Label) || strings.Contains(turn.Observation.Text, site.Accounts[0].Label) {
			t.Error("account B inherited account A's browser state")
		}
		return groundedFinish(turn)
	}, protected...)
	for execution := 0; execution < 2; execution++ {
		if execution == 1 {
			closed := h.Control(bindings[0], "close")
			if len(closed.Sessions) != 2 {
				t.Fatal("closing A discarded B's inventory")
			}
			for _, item := range closed.Sessions {
				if item.ID == bindings[0].ID && item.State != "closed" || item.ID == bindings[1].ID && item.State != "connected" {
					t.Fatal("closing A changed the wrong session")
				}
			}
		}
		command := h.Call("browse", map[string]any{"objective": "Verify the account identity.", "url": site.URL + "/account"}, bindings[1])
		if execution == 0 {
			h.Setup(command, other, keys[1])
		}
		result := h.Result(command)
		if result.Session == nil || result.Session.ID != bindings[1].ID || site.Posts.Load() != 2 || other.Calls() != 2*(execution+1) {
			t.Fatal("independent account reuse repeated login, setup or browser work")
		}
	}
	logged := false
	for _, event := range h.history {
		assertNoPrivate(t, event.Payload, protected...)
		logged = logged || event.Type == controllerevents.ExecutionLogged
	}
	if !logged {
		t.Fatal("agent emitted no execution progress")
	}
}

func TestBrowserAgentCheckpointReceiptsAndNavigationScope(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(fmt.Sprintf("restricted=%t", restricted), func(t *testing.T) {
			h, site := newRuntime(t), fixtures.NewBrowser(t)
			const key = "fixture-checkpoint-model-key-597b"
			model := fakes.NewModel(t, key, func(turn intelligence.Turn) intelligence.Command {
				if !strings.Contains(turn.Observation.Text, "Foreign document") {
					return intelligence.Command{Action: "navigate", URL: site.URL + "/redirect"}
				}
				return groundedFinish(turn)
			})
			input := map[string]any{"objective": "Read the linked document.", "url": site.URL + "/details"}
			if restricted {
				address, err := url.Parse(site.URL)
				runtimeCheck(t, err)
				input["allowed_hosts"] = []string{address.Hostname()}
			}
			binding := h.Binding()
			command := h.Call("browse", input, binding)
			h.Setup(command, model, key)
			pending := waitCheckpoint(t, h, "browse_step_01")
			persisted := h.Persist(pending) // Deliberately lose the persistence receipt.
			retry := waitCheckpoint(t, h, "browse_step_01")
			if !reflect.DeepEqual(pending, retry) || model.Calls() != 1 || site.Redirects.Load() != 0 {
				t.Fatal("pending checkpoint retry repeated a model call or allowed navigation")
			}
			h.Reply(retry, persisted)
			if !restricted {
				final := waitCheckpoint(t, h, "browse_finished")
				persisted := h.Persist(final)
				retry := waitCheckpoint(t, h, "browse_finished")
				if !reflect.DeepEqual(final, retry) || model.Calls() != 3 || site.Foreign.Load() != 1 {
					t.Fatal("final checkpoint retry repeated finished browser work")
				}
				h.Reply(retry, persisted)
			}
			output := h.Result(command)
			var result actions.BrowseOutput
			runtimeCheck(t, json.Unmarshal(output.Output, &result))
			if output.Session == nil || output.Session.ID != binding.ID || site.Redirects.Load() != 1 {
				t.Fatal("navigation lost its session or retried a website request")
			}
			if restricted && (result.Status != "incomplete" || result.Steps[0].Outcome != "unconfirmed" || site.Foreign.Load() != 0 || !bytes.Contains(h.Facts(command)["browse_finished"], []byte(`"phase":"work_unconfirmed"`))) {
				t.Fatal("restricted navigation reached a foreign host or concealed its uncertain effect")
			}
			if !restricted && result.Status != "completed" {
				t.Fatal("default wildcard scope did not permit the cross-host document")
			}
		})
	}
}

func TestBrowserAgentStopsAtChallengeOrClosedCredentialWait(t *testing.T) {
	for _, challenge := range []bool{true, false} {
		t.Run(fmt.Sprintf("challenge=%t", challenge), func(t *testing.T) {
			h, site := newRuntime(t), fixtures.NewBrowser(t)
			binding := h.Binding()
			input := loginInputFor(site, 0)
			if challenge {
				input["url"], input["verification_url"] = site.URL+"/challenge", site.URL+"/challenge"
			}
			command := h.Call("login", input, binding)
			if challenge {
				var result actions.LoginOutput
				runtimeCheck(t, json.Unmarshal(h.Result(command).Output, &result))
				if result.Status != "requires_challenge" || result.Submitted {
					t.Fatal("visible code challenge was treated as verified or submitted")
				}
				for _, event := range h.history {
					if event.Type == controllerevents.SecretSyncRequested {
						t.Fatal("challenge requested reusable credentials")
					}
				}
			} else {
				h.Wait(func(m messages.Message) bool { return m.Type == controllerevents.SecretSyncRequested })
				closed := h.Control(binding, "close")
				if len(closed.Sessions) != 1 || closed.Sessions[0].State != "closed" {
					t.Fatal("session close did not join the pending credential operation")
				}
				failed := h.Wait(func(m messages.Message) bool {
					return m.CorrelationID == command.ID && (m.Type == controllerevents.CommandFailed || m.Type == controllerevents.ActionSucceeded)
				})
				if failed.Type != controllerevents.CommandFailed {
					t.Fatal("closed login resumed or reported success")
				}
				h.Accept(failed)
			}
			if site.Posts.Load() != 0 {
				t.Fatal("blocked login submitted credentials")
			}
		})
	}
}

func TestBrowserAgentBoundsWorkAndRejectsUngroundedCompletion(t *testing.T) {
	for _, ungrounded := range []bool{false, true} {
		t.Run(fmt.Sprintf("ungrounded=%t", ungrounded), func(t *testing.T) {
			h, site := newRuntime(t), fixtures.NewBrowser(t)
			const key = "fixture-bounded-agent-key-319c"
			model := fakes.NewModel(t, key, func(intelligence.Turn) intelligence.Command {
				if ungrounded {
					return intelligence.Command{Action: "finish", Summary: "Unsupported claim."}
				}
				return intelligence.Command{Action: "wait"}
			})
			command := h.Call("browse", map[string]any{"objective": "Read the study.", "url": site.URL + "/details", "max_steps": 1}, h.Binding())
			h.Setup(command, model, key)
			if ungrounded {
				failed := h.Wait(func(m messages.Message) bool {
					return m.CorrelationID == command.ID && (m.Type == controllerevents.CommandFailed || m.Type == controllerevents.ActionSucceeded)
				})
				if failed.Type != controllerevents.CommandFailed {
					t.Fatal("model completed without extracted evidence and citations")
				}
				h.Accept(failed)
			} else {
				var result actions.BrowseOutput
				runtimeCheck(t, json.Unmarshal(h.Result(command).Output, &result))
				if result.Status != "budget_exhausted" || result.Usage.Steps != 1 || result.Usage.CompletionTokensReserved != 1024 {
					t.Fatal("agent escaped its step or reserved-completion allowance")
				}
			}
			if model.Calls() != 1 {
				t.Fatal("agent retried rejected or exhausted model work")
			}
		})
	}
}

func loginAccount(t *testing.T, h *runtimeHarness, binding sessions.Binding, site *fixtures.BrowserSite, index int) sessions.Binding {
	t.Helper()
	account := site.Accounts[index]
	command := h.Call("login", loginInputFor(site, index), binding)
	username, password := loginSecrets(site.URL, account.Profile)
	h.Secret(username, account.Username)
	h.Secret(password, account.Password)
	output := h.Result(command)
	var result actions.LoginOutput
	runtimeCheck(t, json.Unmarshal(output.Output, &result))
	if result.Status != "verified" || !result.Submitted || output.Session == nil || output.Session.Reference != binding.Reference || len(output.Artifacts) != 0 || len(h.Facts(command)) != 2 {
		t.Fatal("managed login did not verify and retain its isolated browser")
	}
	for _, fact := range h.Facts(command) {
		assertNoPrivate(t, fact, account.Username, account.Password, site.URL, account.Label)
	}
	binding.Mode = sessions.Reuse
	return binding
}

func loginInputFor(site *fixtures.BrowserSite, index int) map[string]any {
	return map[string]any{"url": site.URL + "/login", "verification_url": site.URL + "/account", "expected_text": site.Accounts[index].Label, "credential_profile": site.Accounts[index].Profile}
}

func groundedFinish(turn intelligence.Turn) intelligence.Command {
	if len(turn.Evidence) == 0 {
		return intelligence.Command{Action: "extract"}
	}
	return intelligence.Command{Action: "finish", Summary: "The page provides verified evidence.", Findings: []intelligence.Finding{{Text: "The requested detail was extracted from the page.", EvidenceIDs: []string{turn.Evidence[0].ID}}}}
}

func waitCheckpoint(t *testing.T, h *runtimeHarness, name string) messages.Message {
	t.Helper()
	return h.Wait(func(m messages.Message) bool {
		if m.Type != controllerevents.CheckpointRequested {
			return false
		}
		request, err := controllerevents.DecodeCheckpointRequest(m)
		runtimeCheck(t, err)
		return request.Operation == controllerevents.CheckpointSave && request.Name == name
	})
}

func assertNoPrivate(t *testing.T, data []byte, values ...string) {
	t.Helper()
	for _, value := range values {
		if bytes.Contains(data, []byte(value)) || len(value) >= 16 && bytes.Contains(data, []byte(value[:16])) {
			t.Fatal("protected content entered public agent output")
		}
	}
}
