//go:build browserintegration

package browser_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

func TestBrowseCheckpointFactsDescribeReferencesWithoutPrivateContent(t *testing.T) {
	_ = browserSessionOptions(t)
	site, destination, hits, redirects := fixtures.NewBrowseHostsSite(t)
	const key = "fixture-checkpoint-model-key-c903bd71"
	const questionText = "private-checkpoint-question-canary"
	const optionText = "private-checkpoint-option-canary"
	const answerText = "private-checkpoint-answer-canary"
	model := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		if len(turn.Clarifications) == 0 {
			return intelligence.Command{Action: "ask", Question: questionText, Options: []string{optionText}}
		}
		return finishOrExtractStudy(turn)
	})
	framework, binding, command := startCheckpointBrowse(t, model, key, site.URL+"/redirect", 8, 8192)
	peer := framework.peer
	started := waitBrowseCheckpoint(t, peer, "browse_started")
	if model.Calls() != 0 || hits.Load() != 0 || redirects.Load() != 0 {
		t.Fatal("browse crossed its start fence before checkpoint persistence")
	}
	peer.replyCheckpoint(started, peer.checkpointResponse(started))
	request, question := nextBrowseQuestion(t, peer, command, "browser_clarification_1", key)
	now := question.AskedAt
	question.Status, question.Answer, question.AnsweredBy, question.AnsweredAt = "answered", answerText, "operator", &now
	deliverBrowseQuestion(t, peer, request, question)
	output := receiveBrowse(t, peer, command, key, questionText, optionText, answerText)
	var findings actions.BrowseOutput
	if json.Unmarshal(output.Output, &findings) != nil || findings.Status != "completed" || len(findings.Evidence) != 1 || model.Calls() != 3 {
		t.Fatal("checkpointed browsing did not return grounded findings")
	}
	facts := peer.checkpointFacts(command)
	if len(facts) != 5 {
		t.Fatalf("three-command browsing persisted %d facts, want five", len(facts))
	}
	for name, data := range facts {
		fact := decodeBrowseFact(t, data)
		if fact.Version != 1 || fact.Action != "browse" || fact.Engine != "lightpanda" || fact.Session != binding.Claim() || fact.Recovery != "inspect_session_before_explicit_continuation" || fact.ResultDelivery != "unconfirmed" {
			t.Fatalf("checkpoint %q lost its exact session claim or recovery limits", name)
		}
		for _, private := range []string{key, key[:16], questionText, optionText, answerText, site.URL, destination, "42 samples", "Extract the private checkpoint fixture."} {
			if bytes.Contains(data, []byte(private)) {
				t.Fatal("checkpoint persisted private content instead of bounded references")
			}
		}
		assertCheckpointContentKeysAbsent(t, data)
	}
	for step, actionName := range []string{"ask", "extract", "finish"} {
		fact := decodeBrowseFact(t, facts[fmt.Sprintf("browse_step_%02d", step+1)])
		if fact.Phase != "command_pending" || fact.PendingCommand != actionName || fact.Progress == nil || fact.Progress.Command != actionName || fact.Progress.Usage.Steps != step+1 || len(fact.Progress.Steps) != step {
			t.Fatal("step checkpoint confused a pending command with completed browser work")
		}
	}
	encodedEvidence, err := json.Marshal(findings.Evidence[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encodedEvidence)
	finish := decodeBrowseFact(t, facts["browse_finished"])
	pendingFinish := decodeBrowseFact(t, facts["browse_step_03"])
	for _, progress := range []*intelligence.Progress{pendingFinish.Progress, finish.Progress} {
		if progress == nil || len(progress.Evidence) != 1 || progress.Evidence[0].ID != findings.Evidence[0].ID || progress.Evidence[0].SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatal("checkpoint evidence reference does not match the redacted output evidence")
		}
	}
	if finish.Phase != "work_prepared" || finish.Status != "completed" || !reflect.DeepEqual(finish.Progress.Steps, findings.Steps) || finish.Progress.Usage != findings.Usage || finish.Progress.Clarifications != 1 {
		t.Fatal("final checkpoint did not describe prepared work before result delivery")
	}
	checkBrowseArtifacts(t, framework.files, output, key, questionText, optionText, answerText)
}

func TestBrowseCheckpointReceiptRetriesDoNotReplayClickOrFinishedWork(t *testing.T) {
	_ = browserSessionOptions(t)
	site, destination, hits, _ := fixtures.NewBrowseHostsSite(t)
	const key = "fixture-checkpoint-click-key-48a9b290"
	model := checkpointClickModel(t, key, destination)
	framework, _, command := startCheckpointBrowse(t, model, key, site.URL, 8, 8192)
	peer := framework.peer
	first := waitBrowseCheckpoint(t, peer, "browse_step_01")
	persisted := peer.checkpointResponse(first) // Persistence succeeds; its reply is lost.
	retried := waitBrowseCheckpoint(t, peer, "browse_step_01")
	if !reflect.DeepEqual(first, retried) || model.Calls() != 1 || hits.Load() != 0 {
		t.Fatal("lost pending-command receipt replayed model work or allowed its click")
	}
	peer.replyCheckpoint(retried, persisted)
	peer.replyCheckpoint(first, persisted)
	finished := waitBrowseCheckpoint(t, peer, "browse_finished")
	prepared := peer.checkpointResponse(finished)
	retried = waitBrowseCheckpoint(t, peer, "browse_finished")
	if !reflect.DeepEqual(finished, retried) || model.Calls() != 3 || hits.Load() != 1 {
		t.Fatal("lost final receipt replayed completed model or browser work")
	}
	for _, message := range peer.pending {
		if message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed) {
			t.Fatal("checkpointed work returned before final persistence was acknowledged")
		}
	}
	peer.replyCheckpoint(retried, prepared)
	output := receiveBrowse(t, peer, command, key)
	if model.Calls() != 3 || hits.Load() != 1 || len(peer.checkpointFacts(command)) != 5 || len(output.Artifacts) != 1 {
		t.Fatal("checkpoint acknowledgement changed the completed click workflow")
	}
}

func TestBrowseCheckpointUnconfirmedCommandKeepsRecoveryReference(t *testing.T) {
	_ = browserSessionOptions(t)
	site, _, foreignHits, redirects := fixtures.NewBrowseScopeSite(t)
	startingURL, err := url.Parse(site.URL)
	if err != nil {
		t.Fatal(err)
	}
	const key = "fixture-checkpoint-unconfirmed-key-25f9d8"
	model := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		for _, target := range turn.Observation.Targets {
			if target.Label == "Follow redirect" {
				return intelligence.Command{Action: "click", Target: target.ID}
			}
		}
		t.Error("unconfirmed-command fixture did not expose its redirect target")
		return intelligence.Command{Action: "finish", Summary: "Redirect unavailable."}
	})
	framework, binding, command := startCheckpointBrowse(t, model, key, site.URL, 8, 8192, []string{startingURL.Hostname()})
	output := receiveBrowse(t, framework.peer, command, key)
	var findings actions.BrowseOutput
	if json.Unmarshal(output.Output, &findings) != nil || findings.Status != "incomplete" || len(findings.Steps) != 1 || findings.Steps[0].Action != "click" || findings.Steps[0].Outcome != "unconfirmed" || output.Session == nil || model.Calls() != 1 || redirects.Load() != 1 || foreignHits.Load() != 0 {
		t.Fatal("uncertain redirect did not stop once with its retained recovery reference")
	}
	assertBrowserSession(t, *output.Session, binding.Reference, "connected")
	facts := framework.peer.checkpointFacts(command)
	if len(facts) != 3 {
		t.Fatal("unconfirmed command did not preserve its start, pending command and final facts")
	}
	pending := decodeBrowseFact(t, facts["browse_step_01"])
	finished := decodeBrowseFact(t, facts["browse_finished"])
	if pending.Phase != "command_pending" || pending.PendingCommand != "click" || pending.Session != binding.Claim() || finished.Phase != "work_unconfirmed" || finished.Status != "incomplete" || finished.Session != binding.Claim() || finished.ResultDelivery != "unconfirmed" || finished.Progress == nil || !reflect.DeepEqual(finished.Progress.Steps, findings.Steps) {
		t.Fatal("checkpoint recovery metadata treated an uncertain click as confirmed work")
	}
}

func TestBrowseCheckpointWaitTimeoutOrCloseStopsBeforeClick(t *testing.T) {
	_ = browserSessionOptions(t)
	for _, closeSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "persistence_timeout", true: "session_close"}[closeSession], func(t *testing.T) {
			site, destination, hits, _ := fixtures.NewBrowseHostsSite(t)
			const key = "fixture-checkpoint-wait-key-8c94e514"
			model := checkpointClickModel(t, key, destination)
			framework, binding, command := startCheckpointBrowse(t, model, key, site.URL, 8, 8192)
			peer := framework.peer
			first := waitBrowseCheckpoint(t, peer, "browse_step_01")
			_ = peer.checkpointResponse(first)
			peer.checkpointHook = func(message messages.Message, request controllerevents.CheckpointRequest) bool {
				if request.Name != "browse_step_01" || request.CommandID != command.ID {
					return false
				}
				if !reflect.DeepEqual(first, message) {
					t.Error("blocked checkpoint retry changed its immutable request")
				}
				return true
			}
			if closeSession {
				closed := peer.control(binding, "close")
				if len(closed.Sessions) != 1 {
					t.Fatal("closing a checkpoint waiter lost its session reference")
				}
				assertBrowserSession(t, closed.Sessions[0], binding.Reference, "closed")
			}
			outcome := peer.wait(func(message messages.Message) bool {
				return message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed)
			})
			if outcome.Type != controllerevents.CommandFailed || model.Calls() != 1 || hits.Load() != 0 || len(peer.checkpointFacts(command)) != 2 {
				t.Fatal("failed pending-command persistence allowed or replayed its click")
			}
			peer.accept(outcome)
			peer.checkpointHook = nil
			if closeSession {
				return
			}
			inspected := peer.control(binding, "inspect")
			if len(inspected.Sessions) != 1 || inspected.Sessions[0].Busy {
				t.Fatal("checkpoint timeout left the reusable session busy")
			}
			assertBrowserSession(t, inspected.Sessions[0], binding.Reference, "connected")
			binding.Mode = sessions.Reuse
			continuation := publishBrowse(t, peer.bus, binding, site.URL, 8)
			continued := receiveBrowse(t, peer, continuation, key)
			if continued.Session == nil || continued.Session.ID != binding.ID || model.Calls() != 4 || hits.Load() != 1 {
				t.Fatal("explicit new execution did not continue the retained browser safely")
			}
		})
	}
}

func TestBrowseThirtyStepBudgetFitsFrameworkCheckpointLimit(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	const key = "fixture-checkpoint-budget-key-c80251"
	model := fixtures.NewBrowseModel(t, key, func(intelligence.Turn) intelligence.Command { return intelligence.Command{Action: "wait"} })
	framework, _, command := startCheckpointBrowse(t, model, key, site.URL, 30, 32768)
	output := receiveBrowse(t, framework.peer, command, key)
	var findings actions.BrowseOutput
	if json.Unmarshal(output.Output, &findings) != nil || findings.Status != "budget_exhausted" || findings.Usage.Steps != 30 || model.Calls() != 30 {
		t.Fatal("thirty-step browse did not reach its intended execution budget")
	}
	facts := framework.peer.checkpointFacts(command)
	if len(facts) != 32 || len(facts["browse_started"]) == 0 || len(facts["browse_finished"]) == 0 {
		t.Fatal("maximum browse budget did not fit exactly thirty-two immutable facts")
	}
	for step := 1; step <= 30; step++ {
		if len(facts[fmt.Sprintf("browse_step_%02d", step)]) == 0 {
			t.Fatal("maximum browse budget omitted a pending-command checkpoint")
		}
	}
}

type browseCheckpointFact struct {
	Version        int                    `json:"version"`
	Action         string                 `json:"action"`
	Engine         string                 `json:"engine"`
	Session        sessions.CreationClaim `json:"session"`
	Phase          string                 `json:"phase"`
	PendingCommand string                 `json:"pending_command"`
	Progress       *intelligence.Progress `json:"progress"`
	Status         string                 `json:"status"`
	Submitted      bool                   `json:"submitted"`
	Recovery       string                 `json:"recovery"`
	ResultDelivery string                 `json:"result_delivery"`
}

func decodeBrowseFact(t *testing.T, data json.RawMessage) browseCheckpointFact {
	t.Helper()
	var fact browseCheckpointFact
	if json.Unmarshal(data, &fact) != nil {
		t.Fatal("invalid or missing browser checkpoint fact")
	}
	return fact
}

func waitBrowseCheckpoint(t *testing.T, peer *browserSessionPeer, name string) messages.Message {
	t.Helper()
	return peer.wait(func(message messages.Message) bool {
		if message.Type != controllerevents.CheckpointRequested {
			return false
		}
		request, err := controllerevents.DecodeCheckpointRequest(message)
		return err == nil && request.Operation == controllerevents.CheckpointSave && request.Name == name
	})
}

func checkpointClickModel(t *testing.T, key, destination string) *fixtures.BrowseModel {
	t.Helper()
	return fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		if len(turn.Evidence) > 0 {
			return finishOrExtractStudy(turn)
		}
		if turn.Observation.URL == destination {
			return intelligence.Command{Action: "extract"}
		}
		for _, target := range turn.Observation.Targets {
			if target.Label == "Read external study" {
				return intelligence.Command{Action: "click", Target: target.ID}
			}
		}
		t.Error("checkpoint click fixture target was not observed")
		return intelligence.Command{Action: "finish", Summary: "Target unavailable."}
	})
}

func startCheckpointBrowse(t *testing.T, model *fixtures.BrowseModel, key, address string, steps, tokens int, allowedHosts ...[]string) (*browseFramework, sessions.Binding, messages.Message) {
	t.Helper()
	framework := startBrowseFramework(t, key)
	binding := newBrowserSessionBinding(framework.host.ID())
	input := map[string]any{"objective": "Extract the private checkpoint fixture.", "url": address, "session_scope": binding.ScopeID, "max_steps": steps, "max_completion_tokens": tokens}
	if len(allowedHosts) > 0 {
		input["allowed_hosts"] = allowedHosts[0]
	}
	command, err := agentcommands.NewRunAction(binding.OwnerAgentID, agentcommands.ActionCall{ActionName: "browse", Input: marshalLoginInput(t, input), Work: messages.WorkContext{OperationID: binding.Creation.OperationID, TaskID: binding.Creation.TaskID}, Session: &binding})
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentcommands.Client{Bus: framework.peer.bus}).Publish(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	answerBrowseQuestion(t, framework.peer, command, model, "browser_model_provider", "other", key, []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}, false)
	answerBrowseQuestion(t, framework.peer, command, model, "browser_model_name", "fixture-model", key, nil, false)
	answerBrowseQuestion(t, framework.peer, command, model, "browser_model_url", model.URL, key, nil, false)
	deliverBrowseModelSecret(t, framework.peer, model, modelSetupSecretName(model.URL), key)
	return framework, binding, command
}

func assertCheckpointContentKeysAbsent(t *testing.T, data json.RawMessage) {
	t.Helper()
	var document any
	if json.Unmarshal(data, &document) != nil {
		t.Fatal("invalid checkpoint JSON")
	}
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				switch strings.ToLower(key) {
				case "url", "title", "text", "objective", "question", "answer", "options", "username", "password", "cookies", "transcript", "files":
					t.Fatal("checkpoint schema retained browser/model content instead of references")
				}
				visit(child)
			}
		case []any:
			for _, child := range value {
				visit(child)
			}
		}
	}
	visit(document)
}
