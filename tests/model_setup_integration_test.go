//go:build browserintegration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	browser "github.com/defos-ai/commander-browser-agent"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/comms"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/secrets"
	"github.com/defos-ai/commander/framework/id"
	fsartifacts "github.com/defos-ai/commander/providers/fs/artifacts"
	fscomms "github.com/defos-ai/commander/providers/fs/comms"
)

type browseFramework struct {
	peer  *browserSessionPeer
	host  *agent.Agent
	files *fsartifacts.ArtifactFileFsStore
	stop  func()
}

func startBrowseFramework(t *testing.T, key string) *browseFramework {
	t.Helper()
	root := t.TempDir()
	bus, err := fscomms.NewFsBus(filepath.Join(root, "bus"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	observer, err := bus.Subscribe(t.Context(), comms.Inbox{Topic: messages.ControllerEvents(messages.DefaultControllerType), Name: "browse-test", Mode: comms.TransientExclusive})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close() })
	files, err := fsartifacts.NewArtifactFileFsStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := id.AgentID(string(browser.AgentType))
	if err != nil {
		t.Fatal(err)
	}
	zero := time.Duration(0)
	host, err := browser.NewAgent(agent.AgentConfig{ID: owner, ArtifactFileStore: files, HeartbeatInterval: &zero})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, cancelServe := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- host.Serve(serveCtx, bus, nil) }()
	var stopped sync.Once
	stop := func() {
		stopped.Do(func() {
			cancelServe()
			select {
			case err := <-served:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("stop browse host: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("browse host did not stop")
			}
		})
	}
	t.Cleanup(stop)
	peer := &browserSessionPeer{t: t, bus: bus, observer: observer}
	started := peer.wait(func(message messages.Message) bool { return message.Type == controllerevents.AgentStarted })
	if bytes.Contains(started.Payload, []byte(key)) {
		t.Fatal("model credential entered the public agent snapshot")
	}
	var snapshot agent.AgentSnapshot
	if err := json.Unmarshal(started.Payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Actions) != 1 || snapshot.Actions[0].Name != "browse" || snapshot.Actions[0].Status != "ready" {
		t.Fatal("browse was not ready before model setup")
	}
	for _, message := range peer.pending {
		if message.Type == controllerevents.SecretSyncRequested || message.Type == controllerevents.QuestionRequested {
			t.Fatal("discovery requested model configuration or credentials")
		}
	}
	return &browseFramework{peer: peer, host: host, files: files, stop: stop}
}

// Clear legacy settings only to isolate the absent/ignored-environment scenarios.
// Production model setup must never consume these variables.
func clearLegacyModelEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"BROWSER_MODEL_PROVIDER", "BROWSER_MODEL_NAME", "BROWSER_MODEL_URL", "BROWSER_MODEL_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "XAI_API_KEY", "DEEPSEEK_API_KEY"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func nextBrowseQuestion(t *testing.T, peer *browserSessionPeer, command messages.Message, key, credential string) (messages.Message, operations.Question) {
	t.Helper()
	message := peer.wait(func(message messages.Message) bool {
		return message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested || message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed
	})
	if message.Type != controllerevents.QuestionRequested || bytes.Contains(message.Payload, []byte(credential)) {
		t.Fatal("model setup did not ask a non-secret question before credential or browser access")
	}
	request, err := controllerevents.DecodeQuestionRequest(message)
	if err != nil {
		t.Fatal(err)
	}
	if request.Key != key || request.CommandID != command.ID || message.Work != command.Work {
		t.Fatal("model setup question did not retain its execution identity or expected order")
	}
	now := time.Now().UTC()
	question := operations.Question{ID: id.New(), Status: operations.QuestionPending, TaskID: message.Work.TaskID,
		CommandID: command.ID, Key: request.Key, Text: request.Text, Options: request.Options, AskedAt: now, FallbackAt: now.Add(time.Minute)}
	return message, question
}

func deliverBrowseQuestion(t *testing.T, peer *browserSessionPeer, request messages.Message, question operations.Question) {
	t.Helper()
	response, err := agentevents.NewQuestionResponse(request, agentevents.QuestionResponse{CommandID: question.CommandID, Key: question.Key, Question: &question})
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentevents.Client{Bus: peer.bus}).Publish(t.Context(), response); err != nil {
		t.Fatal(err)
	}
}

func answerBrowseQuestion(t *testing.T, peer *browserSessionPeer, command messages.Message, model *fixtures.BrowseModel, key, answer, credential string, options []string, provePending bool) {
	t.Helper()
	request, question := nextBrowseQuestion(t, peer, command, key, credential)
	if !reflect.DeepEqual(question.Options, options) || model.Calls() != 0 {
		t.Fatal("model setup changed its choices or accessed the model before setup completed")
	}
	if provePending {
		deliverBrowseQuestion(t, peer, request, question)
		retried := peer.wait(func(message messages.Message) bool { return message.Type == controllerevents.QuestionRequested })
		if !reflect.DeepEqual(retried, request) || model.Calls() != 0 {
			t.Fatal("pending saved question did not retain its retry identity and block model access")
		}
	}
	now := time.Now().UTC()
	question.Status, question.Answer, question.AnsweredBy, question.AnsweredAt = operations.QuestionAnswered, answer, operations.AnswerOperator, &now
	deliverBrowseQuestion(t, peer, request, question)
	// Replaying the same saved answer must not repeat setup or browser effects.
	deliverBrowseQuestion(t, peer, request, question)
}

func deliverBrowseModelSecret(t *testing.T, peer *browserSessionPeer, model *fixtures.BrowseModel, name, credential string) {
	t.Helper()
	message := peer.wait(func(message messages.Message) bool {
		return message.Type == controllerevents.SecretSyncRequested || message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed
	})
	if message.Type != controllerevents.SecretSyncRequested || bytes.Contains(message.Payload, []byte(credential)) || model.Calls() != 0 {
		t.Fatal("browse did not wait for its protected provider credential")
	}
	request, err := controllerevents.DecodeSecretSyncRequested(message)
	if err != nil {
		t.Fatal(err)
	}
	if request.Request.Key.Name != name {
		t.Fatal("browse requested a credential not bound to the selected provider endpoint")
	}
	now := time.Now().Add(-time.Second).UTC()
	delivery, err := agentevents.NewSecretDelivery(request.Request, secrets.Metadata{ID: id.New(), Key: request.Request.Key, Version: 1, CreatedAt: now, UpdatedAt: now}, []byte(credential))
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentevents.Client{Bus: peer.bus}).Publish(t.Context(), delivery); err != nil {
		t.Fatal(err)
	}
}

func TestBrowseModelSetupQuestionCancellationStopsBeforeBrowserAccess(t *testing.T) {
	_ = browserSessionOptions(t)
	clearLegacyModelEnvironment(t)
	const credential = "fixture-cancelled-provider-key-083e6"
	site := fixtures.NewBrowseSite(t, credential)
	model := fixtures.NewBrowseModel(t, credential, func(intelligence.Turn) intelligence.Command {
		t.Error("cancelled model setup reached the model")
		return intelligence.Command{Action: "finish", Summary: "Unexpected model call."}
	})
	framework := startBrowseFramework(t, credential)
	binding := newBrowserSessionBinding(framework.host.ID())
	command := publishBrowse(t, framework.peer.bus, binding, site.URL, 12)
	request, question := nextBrowseQuestion(t, framework.peer, command, "browser_model_provider", credential)
	deliverBrowseQuestion(t, framework.peer, request, question)
	snapshot := framework.host.Describe()
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].Kind != "browser" || snapshot.Sessions[0].Busy || model.Calls() != 0 {
		t.Fatal("question wait started browser work before provider selection")
	}
	framework.stop()
	if model.Calls() != 0 {
		t.Fatal("cancelled question wait started model work during shutdown")
	}
}

func TestBrowseSessionCloseCancelsPendingModelSetup(t *testing.T) {
	_ = browserSessionOptions(t)
	clearLegacyModelEnvironment(t)
	const credential = "fixture-closed-setup-key-571900"
	site := fixtures.NewBrowseSite(t, credential)
	model := fixtures.NewBrowseModel(t, credential, func(intelligence.Turn) intelligence.Command {
		t.Error("closed session setup reached the model")
		return intelligence.Command{Action: "finish", Summary: "Unexpected model call."}
	})
	framework := startBrowseFramework(t, credential)
	binding := newBrowserSessionBinding(framework.host.ID())
	command := publishBrowse(t, framework.peer.bus, binding, site.URL, 12)
	request, question := nextBrowseQuestion(t, framework.peer, command, "browser_model_provider", credential)
	deliverBrowseQuestion(t, framework.peer, request, question)
	before := framework.host.Describe()
	if len(before.Sessions) != 1 || before.Sessions[0].Kind != "browser" || before.Sessions[0].Busy {
		t.Fatal("model setup started the browser while waiting for provider selection")
	}
	closed := framework.peer.control(binding, "close")
	if len(closed.Sessions) != 1 {
		t.Fatal("closing a waiting session discarded its identity")
	}
	assertBrowserSession(t, closed.Sessions[0], binding.Reference, "closed")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// The serving agent remains alive. Its action must leave the question wait
	// because the individual session closed, not because host shutdown cancels it.
	var outcome messages.Message
	for outcome.ID == "" {
		for i, message := range framework.peer.pending {
			if message.CorrelationID == command.ID && (message.Type == controllerevents.CommandFailed || message.Type == controllerevents.ActionSucceeded) {
				outcome = message
				framework.peer.pending = append(framework.peer.pending[:i], framework.peer.pending[i+1:]...)
				break
			}
		}
		if outcome.ID != "" {
			break
		}
		delivery, err := framework.peer.observer.Next(ctx)
		if err != nil {
			t.Fatal("closed session did not cancel its pending model-setup question")
		}
		message := delivery.Message()
		if err := delivery.Ack(ctx); err != nil {
			t.Fatal(err)
		}
		framework.peer.pending = append(framework.peer.pending, message)
	}
	if outcome.Type != controllerevents.CommandFailed || bytes.Contains(outcome.Payload, []byte(credential)) || model.Calls() != 0 {
		t.Fatal("closed setup did not finish safely before model access")
	}
	framework.peer.accept(outcome)
}
