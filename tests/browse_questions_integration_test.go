//go:build browserintegration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/actions"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

const clarificationNotice = "Browser clarification (non-secret). Do not enter passwords, credentials, authentication codes or API keys.\n\n"
const clarificationQuestion = "Which study detail should I extract?"

func TestBrowseClarificationUsesSavedAnswersWithoutReplayingWork(t *testing.T) {
	_ = browserSessionOptions(t)
	for _, origin := range []operations.AnswerOrigin{operations.AnswerOperator, operations.AnswerModel} {
		t.Run(string(origin), func(t *testing.T) {
			site := fixtures.NewBrowseNavigationSite(t)
			const key = "fixture-clarification-managed-key-84c6ec01"
			const privateAnswer = "private-clarification-answer-canary"
			model := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
				if len(turn.Clarifications) == 0 {
					return intelligence.Command{Action: "ask", Question: clarificationQuestion, Options: []string{"Sample count", "Study title"}}
				}
				if len(turn.Clarifications) != 1 || turn.Clarifications[0].Question != clarificationQuestion || !slices.Equal(turn.Clarifications[0].Options, []string{"Sample count", "Study title"}) || turn.Clarifications[0].Answer != privateAnswer+" [REDACTED]" {
					t.Error("resolved clarification was not retained safely for later model decisions")
				}
				return finishOrExtractStudy(turn)
			})
			framework, binding, command := startClarifyingBrowse(t, model, key, site.URL+"/details", 5)
			peer := framework.peer
			request, question := nextBrowseQuestion(t, peer, command, "browser_clarification_1", key)
			if question.Text != clarificationNotice+clarificationQuestion || !slices.Equal(question.Options, []string{"Sample count", "Study title"}) || model.Calls() != 1 {
				t.Fatal("browse clarification lost its non-secret notice, options or execution ordering")
			}
			if origin == operations.AnswerModel {
				question.AskedAt, question.FallbackAt = time.Now().Add(-3*time.Minute), time.Now().Add(-time.Minute)
			}
			deliverBrowseQuestion(t, peer, request, question)
			retried := peer.wait(func(message messages.Message) bool { return message.Type == controllerevents.QuestionRequested })
			if !reflect.DeepEqual(retried, request) || model.Calls() != 1 {
				t.Fatal("pending clarification replayed model work or changed its request identity")
			}
			inspected := peer.control(binding, "inspect")
			if len(inspected.Sessions) != 1 || !inspected.Sessions[0].Busy {
				t.Fatal("waiting clarification did not retain independently inspectable browser work")
			}
			assertBrowserSession(t, inspected.Sessions[0], binding.Reference, "connected")
			if origin == operations.AnswerOperator {
				// Waiting must outlast the entire active browsing allowance without
				// consuming it. The model itself is never used to answer this wait.
				timer := time.NewTimer(5500 * time.Millisecond)
				select {
				case <-timer.C:
				case <-t.Context().Done():
					timer.Stop()
					t.Fatal(t.Context().Err())
				}
			}
			now := time.Now().UTC()
			question.Status, question.Answer, question.AnsweredBy, question.AnsweredAt = operations.QuestionAnswered, privateAnswer+" "+key, origin, &now
			deliverBrowseQuestion(t, peer, request, question)
			deliverBrowseQuestion(t, peer, request, question)
			output := receiveBrowse(t, peer, command, key, privateAnswer)
			var result actions.BrowseOutput
			if err := json.Unmarshal(output.Output, &result); err != nil {
				t.Fatal(err)
			}
			if result.Status != "completed" || len(result.Evidence) != 1 || len(result.Findings) != 1 || len(result.Steps) != 3 || result.Steps[0].Action != "ask" || result.Steps[0].Outcome != "completed" || model.Calls() != 3 {
				t.Fatalf("clarification did not resume grounded browsing exactly once: %+v calls=%d", result, model.Calls())
			}
			if bytes.Contains(output.Output, []byte("clarifications")) || bytes.Contains(output.Output, []byte(clarificationQuestion)) {
				t.Fatal("execution-only clarification history entered the result")
			}
			checkBrowseArtifacts(t, framework.files, output, key, privateAnswer, clarificationQuestion)
			for _, message := range peer.pending {
				if message.Type == controllerevents.ExecutionLogged && (bytes.Contains(message.Payload, []byte(key)) || bytes.Contains(message.Payload, []byte(privateAnswer)) || bytes.Contains(message.Payload, []byte(clarificationQuestion))) {
					t.Fatal("clarification content entered public progress logs")
				}
			}
		})
	}
}

func TestBrowseSessionCloseInterruptsInBrowserClarification(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	const key = "fixture-closed-clarification-key-c347a9"
	model := fixtures.NewBrowseModel(t, key, func(intelligence.Turn) intelligence.Command {
		return intelligence.Command{Action: "ask", Question: clarificationQuestion}
	})
	framework, binding, command := startClarifyingBrowse(t, model, key, site.URL+"/details", 30)
	peer := framework.peer
	request, question := nextBrowseQuestion(t, peer, command, "browser_clarification_1", key)
	deliverBrowseQuestion(t, peer, request, question)
	started := time.Now()
	closed := peer.control(binding, "close")
	if time.Since(started) > 3*time.Second || len(closed.Sessions) != 1 {
		t.Fatal("closing a pending browser clarification did not finish promptly")
	}
	assertBrowserSession(t, closed.Sessions[0], binding.Reference, "closed")
	outcome := peer.wait(func(message messages.Message) bool {
		return message.CorrelationID == command.ID && (message.Type == controllerevents.CommandFailed || message.Type == controllerevents.ActionSucceeded)
	})
	if outcome.Type != controllerevents.CommandFailed || model.Calls() != 1 {
		t.Fatal("closing a clarification resumed model or browser work")
	}
	peer.accept(outcome)
	now := time.Now().UTC()
	question.Status, question.Answer, question.AnsweredBy, question.AnsweredAt = operations.QuestionAnswered, "Sample count", operations.AnswerOperator, &now
	deliverBrowseQuestion(t, peer, request, question)
	inspected := peer.control(binding, "inspect")
	if len(inspected.Sessions) != 1 || model.Calls() != 1 {
		t.Fatal("late clarification answer revived closed browser work")
	}
	assertBrowserSession(t, inspected.Sessions[0], binding.Reference, "closed")
}

func TestBrowseCancelledClarificationRetainsReusableSession(t *testing.T) {
	_ = browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	const key = "fixture-cancelled-clarification-key-66f195"
	var resumed atomic.Bool
	model := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		if len(turn.Clarifications) != 0 {
			t.Error("a new execution inherited cancelled clarification history")
		}
		if !resumed.Load() {
			return intelligence.Command{Action: "ask", Question: clarificationQuestion}
		}
		return finishOrExtractStudy(turn)
	})
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	executor := actions.NewBrowse(manager.Manager)
	discoverBrowse(t, executor, &environment.Local{})
	questions := &modelSetupQuestions{answers: map[string]string{"browser_model_provider": "other", "browser_model_name": "fixture-model", "browser_model_url": model.URL}, block: "browser_clarification_1", entered: make(chan string, 8)}
	secrets := &browseSecretProbe{value: []byte(key)}
	logs := &clarificationLogProbe{}
	input := modelSetupInput(binding)
	input["url"] = site.URL + "/details"
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	work, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := action.WithLogs(action.WithQuestions(action.WithSecrets(action.WithSession(work, &binding), secrets), questions), logs)
	done := make(chan error, 1)
	go func() { _, err := executor.Run(ctx, raw); done <- err }()
	waiting := time.NewTimer(10 * time.Second)
	defer waiting.Stop()
	for {
		select {
		case name := <-questions.entered:
			if name != "browser_clarification_1" {
				continue
			}
		case <-waiting.C:
			t.Fatal("browse did not reach its in-browser clarification")
		}
		break
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || model.Calls() != 1 {
			t.Fatalf("clarification cancellation resumed model work: error=%v calls=%d", err, model.Calls())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled clarification did not return promptly")
	}
	inspected, err := manager.Control(t.Context(), browserSessionControl(binding, "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	assertBrowserSession(t, inspected, binding.Reference, "connected")
	resumed.Store(true)
	binding.Mode = sessions.Reuse
	input["session_id"] = binding.ID
	raw, err = json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ctx = action.WithLogs(action.WithSecrets(action.WithSession(t.Context(), &binding), secrets), logs)
	result, err := executor.Run(ctx, raw)
	if err != nil || result.Session == nil || model.Calls() != 3 || secrets.requests != 2 {
		t.Fatalf("cancelled clarification prevented reuse without new questions: error=%v calls=%d requests=%d", err, model.Calls(), secrets.requests)
	}
	assertBrowserSession(t, *result.Session, binding.Reference, "connected")
}

func TestBrowseClarificationRejectsInvalidCommandsBeforeQuestion(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		command intelligence.Command
	}{
		{name: "question too long", command: intelligence.Command{Action: "ask", Question: strings.Repeat("x", 1001)}},
		{name: "invalid question UTF8", command: intelligence.Command{Action: "ask", Question: string([]byte{0xff})}},
		{name: "too many options", command: intelligence.Command{Action: "ask", Question: clarificationQuestion, Options: []string{"a", "b", "c", "d", "e"}}},
		{name: "option too long", command: intelligence.Command{Action: "ask", Question: clarificationQuestion, Options: []string{strings.Repeat("x", 161)}}},
		{name: "duplicate options", command: intelligence.Command{Action: "ask", Question: clarificationQuestion, Options: []string{"Sample count", "sample count"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, questions := 0, 0
			model := clarificationModelFunc(func(intelligence.Turn) intelligence.Command {
				calls++
				return test.command
			})
			_, err := manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
				_, err := intelligence.Run(ctx, intelligence.Request{Objective: "Inspect the study.", URL: site.URL, MaxSteps: 8, MaxCompletionTokens: 8192}, model, intelligence.Hooks{Question: func(context.Context, string, string, []string) (string, error) {
					questions++
					return "Sample count", nil
				}})
				return err
			})
			if err == nil || calls != 1 || questions != 0 {
				t.Fatalf("malformed ask reached the question bridge or was replayed: error=%v model_calls=%d questions=%d", err, calls, questions)
			}
		})
		binding.Mode = sessions.Reuse
	}
}

func TestBrowseClarificationFailureStopsWithoutReplayingWork(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	const key = "fixture-failed-clarification-key-65f1f4"
	for _, test := range []struct {
		name, answer string
		err, want    error
	}{
		{name: "blank answer", answer: " \n\t"},
		{name: "oversize answer", answer: strings.Repeat("x", 2001)},
		{name: "invalid answer UTF8", answer: string([]byte{0xff})},
		{name: "question deadline", err: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "question cancellation", err: context.Canceled, want: context.Canceled},
		{name: "private hook error", err: errors.New("private hook failure: " + key)},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, questions := 0, 0
			model := clarificationModelFunc(func(intelligence.Turn) intelligence.Command {
				calls++
				return intelligence.Command{Action: "ask", Question: clarificationQuestion}
			})
			logs := &clarificationLogProbe{}
			var result intelligence.Result
			snapshot, err := manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
				var err error
				result, err = intelligence.Run(ctx, intelligence.Request{Objective: "Inspect the study.", URL: site.URL, MaxSteps: 8, MaxCompletionTokens: 8192}, model, intelligence.Hooks{
					Log:    logs.Log,
					Redact: func(text string) string { return strings.ReplaceAll(text, key, "[REDACTED]") },
					Question: func(context.Context, string, string, []string) (string, error) {
						questions++
						return test.answer, test.err
					},
				})
				return err
			})
			if err == nil || test.want != nil && !errors.Is(err, test.want) || calls != 1 || questions != 1 || t.Context().Err() != nil {
				t.Fatalf("failed clarification was replayed or lost its error identity: error=%v model_calls=%d questions=%d", err, calls, questions)
			}
			assertBrowserSession(t, snapshot, binding.Reference, "connected")
			encoded, encodeErr := json.Marshal(result)
			if encodeErr != nil || bytes.Contains(encoded, []byte(key)) || strings.Contains(err.Error(), key) || strings.Contains(strings.Join(logs.messages, "\n"), key) {
				t.Fatal("private hook error entered the error, result or progress logs")
			}
		})
		binding.Mode = sessions.Reuse
	}
}

func TestBrowseClarificationLimitAndKnownKeyRedaction(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	const key = "fixture-redacted-clarification-key-64deaa"
	questions, calls := 0, 0
	model := clarificationModelFunc(func(turn intelligence.Turn) intelligence.Command {
		calls++
		for _, clarification := range turn.Clarifications {
			if clarification.Question != "Choose [REDACTED]" || !slices.Equal(clarification.Options, []string{"[REDACTED]", "Other"}) || clarification.Answer != "Use [REDACTED]" {
				t.Error("known key was retained in clarification history")
			}
		}
		return intelligence.Command{Action: "ask", Question: "Choose " + key, Options: []string{key, "Other"}}
	})
	logs := &clarificationLogProbe{}
	var result intelligence.Result
	_, err := manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
		var err error
		result, err = intelligence.Run(ctx, intelligence.Request{Objective: "Inspect the study.", URL: site.URL, MaxSteps: 8, MaxCompletionTokens: 8192}, model, intelligence.Hooks{
			Redact: func(text string) string { return strings.ReplaceAll(text, key, "[REDACTED]") },
			Log:    logs.Log,
			Question: func(_ context.Context, name, text string, offered []string) (string, error) {
				questions++
				if name != "browser_clarification_"+string(rune('0'+questions)) || text != "Choose [REDACTED]" || !slices.Equal(offered, []string{"[REDACTED]", "Other"}) {
					t.Error("question bridge received a known key or unstable clarification key")
				}
				return "Use " + key, nil
			},
		})
		return err
	})
	if err != nil || result.Status != "budget_exhausted" || questions != 4 || calls != 5 || len(result.Steps) != 4 {
		t.Fatalf("clarification limit did not stop before a fifth question: result=%+v error=%v questions=%d calls=%d", result, err, questions, calls)
	}
	encoded, err := json.Marshal(result)
	if err != nil || bytes.Contains(encoded, []byte(key)) || strings.Contains(strings.Join(logs.messages, "\n"), key) {
		t.Fatal("known key entered clarification output or progress logs")
	}
}

func startClarifyingBrowse(t *testing.T, model *fixtures.BrowseModel, key, address string, seconds int) (*browseFramework, sessions.Binding, messages.Message) {
	t.Helper()
	framework := startBrowseFramework(t, key)
	binding := newBrowserSessionBinding(framework.host.ID())
	input, err := json.Marshal(map[string]any{"objective": "Extract the study detail selected by the operator.", "url": address, "session_scope": binding.ScopeID, "max_steps": 8, "timeout_seconds": seconds})
	if err != nil {
		t.Fatal(err)
	}
	work := messages.WorkContext{OperationID: binding.Creation.OperationID, TaskID: binding.Creation.TaskID}
	command, err := agentcommands.NewRunAction(binding.OwnerAgentID, agentcommands.ActionCall{ActionName: "browse", Input: input, Work: work, Session: &binding})
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentcommands.Client{Bus: framework.peer.bus}).Publish(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	for _, setup := range []struct {
		key, answer string
		options     []string
	}{
		{key: "browser_model_provider", answer: "More providers", options: []string{"OpenAI", "Anthropic", "Google Gemini", "More providers"}},
		{key: "browser_model_provider_more", answer: "Other (OpenAI-compatible)", options: []string{"xAI", "DeepSeek", "Other (OpenAI-compatible)"}},
		{key: "browser_model_name", answer: "fixture-model"},
		{key: "browser_model_url", answer: model.URL},
	} {
		answerBrowseQuestion(t, framework.peer, command, model, setup.key, setup.answer, key, setup.options, false)
	}
	deliverBrowseModelSecret(t, framework.peer, model, actions.ModelSecretName(model.URL), key)
	return framework, binding, command
}

func finishOrExtractStudy(turn intelligence.Turn) intelligence.Command {
	if len(turn.Evidence) == 0 {
		return intelligence.Command{Action: "extract"}
	}
	return intelligence.Command{Action: "finish", Summary: "Found the study sample count.", Findings: []intelligence.Finding{{Text: "The study measured 42 samples.", EvidenceIDs: []string{turn.Evidence[0].ID}}}}
}

type clarificationLogProbe struct{ messages []string }

func (probe *clarificationLogProbe) Log(_ context.Context, message string) error {
	probe.messages = append(probe.messages, message)
	return nil
}

type clarificationModelFunc func(intelligence.Turn) intelligence.Command

func (model clarificationModelFunc) Next(_ context.Context, turn intelligence.Turn, _ int) (intelligence.Command, error) {
	return model(turn), nil
}
