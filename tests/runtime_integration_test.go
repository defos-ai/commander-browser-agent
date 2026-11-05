//go:build browserintegration

package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	browser "github.com/defos-ai/commander-browser-agent"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander/framework/agent"
	"github.com/defos-ai/commander/framework/comms"
	agentcommands "github.com/defos-ai/commander/framework/comms/clients/agent/commands"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/executions"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/domain/secrets"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
	fsartifacts "github.com/defos-ai/commander/providers/fs/artifacts"
	fscomms "github.com/defos-ai/commander/providers/fs/comms"
)

type runtimeHarness struct {
	t         *testing.T
	host      *agent.Agent
	manager   *browserruntime.Manager
	options   browserruntime.Options
	bus       comms.Bus
	observer  comms.Subscription
	files     *fsartifacts.ArtifactFileFsStore
	pending   []messages.Message
	history   []messages.Message
	records   map[string]executions.Execution
	questions map[string]operations.Question
	replies   map[string]messages.Message
}

func newRuntime(t *testing.T) *runtimeHarness {
	t.Helper()
	h := &runtimeHarness{t: t, records: map[string]executions.Execution{}, questions: map[string]operations.Question{}, replies: map[string]messages.Message{}}
	h.options = browserruntime.Options{Engine: browserruntime.Lightpanda, Executable: os.Getenv("LIGHTPANDA_PATH")}
	if !filepath.IsAbs(h.options.Executable) {
		t.Fatal("browserintegration requires absolute LIGHTPANDA_PATH")
	}
	root := t.TempDir()
	bus, err := fscomms.NewFsBus(filepath.Join(root, "bus"))
	runtimeCheck(t, err)
	h.bus = bus
	h.observer, err = bus.Subscribe(t.Context(), comms.Inbox{Topic: messages.ControllerEvents(messages.DefaultControllerType), Name: "browser-tests", Mode: comms.TransientExclusive})
	runtimeCheck(t, err)
	h.files, err = fsartifacts.NewArtifactFileFsStore(filepath.Join(root, "artifacts"))
	runtimeCheck(t, err)
	owner, err := id.AgentID(string(browser.AgentType))
	runtimeCheck(t, err)
	h.manager, err = browserruntime.New(owner)
	runtimeCheck(t, err)
	zero := time.Duration(0)
	h.host, err = browser.NewAgent(agent.AgentConfig{ID: owner, Sessions: h.manager, ArtifactFileStore: h.files, HeartbeatInterval: &zero})
	runtimeCheck(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.host.Serve(ctx, bus, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("agent shutdown did not join browser resources")
		}
		if err := h.manager.Close(); err != nil {
			t.Errorf("browser resource cleanup failed: %v", err)
		}
		_ = h.observer.Close()
		_ = bus.Close()
	})
	started := h.Wait(func(m messages.Message) bool { return m.Type == controllerevents.AgentStarted })
	var snapshot agent.AgentSnapshot
	runtimeCheck(t, json.Unmarshal(started.Payload, &snapshot))
	names := map[string]bool{}
	for _, action := range snapshot.Actions {
		if action.Status != "ready" || len(action.RequiredSecrets) != 0 {
			t.Fatal("actions must be ready before questions or managed credentials")
		}
		names[action.Name] = true
	}
	if len(names) != 3 || !names["browse"] || !names["login"] || !names["screenshot"] {
		t.Fatal("agent registered an unexpected action catalog")
	}
	return h
}

func (h *runtimeHarness) Binding() sessions.Binding {
	task := id.TaskID()
	return sessions.Binding{Reference: sessions.Reference{ID: id.New(), OwnerAgentID: h.host.ID(), ScopeID: id.New()}, Mode: sessions.Create, Creation: sessions.ExecutionRef{OperationID: id.OperationID(), TaskID: task, CommandID: task}}
}

func (h *runtimeHarness) Call(name string, input map[string]any, binding sessions.Binding) messages.Message {
	h.t.Helper()
	input["session_scope"] = binding.ScopeID
	taskID := id.TaskID()
	if binding.Mode == sessions.Create {
		taskID = binding.Creation.TaskID
	} else {
		input["session_id"] = binding.ID
	}
	command, err := agentcommands.NewRunAction(binding.OwnerAgentID, agentcommands.ActionCall{ActionName: name, Input: runtimeJSON(h.t, input), OperationID: binding.Creation.OperationID, TaskID: taskID, Session: &binding})
	runtimeCheck(h.t, err)
	h.records[command.ID] = executions.Execution{Command: command, StartedAt: time.Now().UTC()}
	runtimeCheck(h.t, (agentcommands.Client{Bus: h.bus}).Publish(h.t.Context(), command))
	return command
}

// Match before automatic checkpoint replies, allowing a test to hold a fence.
func (h *runtimeHarness) Wait(match func(messages.Message) bool) messages.Message {
	h.t.Helper()
	for i := 0; i < len(h.pending); {
		message := h.pending[i]
		if h.replay(message) {
			h.pending = append(h.pending[:i], h.pending[i+1:]...)
			continue
		}
		matched := match(message)
		if matched || message.Type == controllerevents.CheckpointRequested {
			h.pending = append(h.pending[:i], h.pending[i+1:]...)
			if matched {
				return message
			}
			h.Reply(message, h.Persist(message))
			continue
		}
		i++
	}
	ctx, cancel := context.WithTimeout(h.t.Context(), 30*time.Second)
	defer cancel()
	for {
		delivery, err := h.observer.Next(ctx)
		runtimeCheck(h.t, err)
		message := delivery.Message()
		runtimeCheck(h.t, delivery.Ack(ctx))
		message.Payload = bytes.Clone(message.Payload)
		h.history = append(h.history, message)
		if h.replay(message) {
			continue
		}
		if match(message) {
			return message
		}
		if message.Type == controllerevents.CheckpointRequested {
			h.Reply(message, h.Persist(message))
		} else {
			h.pending = append(h.pending, message)
		}
	}
}

func (h *runtimeHarness) replay(request messages.Message) bool {
	if request.Type != controllerevents.QuestionRequested && request.Type != controllerevents.SecretSyncRequested {
		return false
	}
	response, known := h.replies[request.ID]
	if known {
		runtimeCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), response))
	}
	return known
}

func (h *runtimeHarness) Answer(request messages.Message, answer string, origin ...operations.AnswerOrigin) operations.Question {
	h.t.Helper()
	payload, err := controllerevents.DecodeQuestionRequest(request)
	runtimeCheck(h.t, err)
	question, found := h.questions[request.ID]
	if found && question.Status == operations.QuestionAnswered {
		if answer != "" && answer != question.Answer {
			h.t.Fatal("a saved question cannot change its answered value")
		}
		h.replay(request)
		return question
	}
	now := time.Now().UTC()
	if !found {
		question = operations.Question{ID: id.New(), Status: operations.QuestionPending, TaskID: request.TaskID, CommandID: payload.CommandID, Key: payload.Key, Text: payload.Text, Options: payload.Options, AskedAt: now, FallbackAt: now.Add(time.Minute)}
	}
	if answer != "" {
		by := operations.AnswerOperator
		if len(origin) > 0 {
			by = origin[0]
		}
		if by == operations.AnswerModel {
			question.AskedAt, question.FallbackAt = now.Add(-3*time.Minute), now.Add(-time.Minute)
		}
		question.Status, question.Answer, question.AnsweredBy, question.AnsweredAt = operations.QuestionAnswered, answer, by, &now
	}
	h.questions[request.ID] = question
	response, err := agentevents.NewQuestionResponse(request, agentevents.QuestionResponse{CommandID: payload.CommandID, Key: payload.Key, Question: &question})
	runtimeCheck(h.t, err)
	if question.Status == operations.QuestionAnswered {
		h.replies[request.ID] = response
	}
	runtimeCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), response))
	return question
}

func (h *runtimeHarness) Secret(name, value string) {
	h.t.Helper()
	message := h.Wait(func(m messages.Message) bool {
		return m.Type == controllerevents.SecretSyncRequested || m.Type == controllerevents.QuestionRequested || m.Type == controllerevents.CommandFailed || m.Type == controllerevents.ActionSucceeded
	})
	if message.Type != controllerevents.SecretSyncRequested || bytes.Contains(message.Payload, []byte(value)) {
		h.t.Fatal("managed credential was not requested before delivery")
	}
	request, err := controllerevents.DecodeSecretSyncRequested(message)
	runtimeCheck(h.t, err)
	defer clear(request.Default)
	if request.Request.Key.Name != name {
		h.t.Fatal("action requested a credential outside its selected purpose")
	}
	now := time.Now().UTC()
	delivery, err := agentevents.NewSecretDelivery(request.Request, secrets.Metadata{ID: id.New(), Key: request.Request.Key, Version: 1, CreatedAt: now, UpdatedAt: now}, []byte(value))
	runtimeCheck(h.t, err)
	h.replies[message.ID] = delivery
	runtimeCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), delivery))
}

func (h *runtimeHarness) Transient(message messages.Message, value, code string) messages.Message {
	h.t.Helper()
	request, err := controllerevents.DecodeSecretSyncRequested(message)
	runtimeCheck(h.t, err)
	defer clear(request.Default)
	record, found := h.records[request.Request.CommandID]
	if !found || request.Default != nil || request.Request.Type != secrets.SecretTransient ||
		request.Request.OperationID != record.Command.OperationID || request.Request.TaskID != record.Command.TaskID ||
		request.Request.AgentID != record.Command.AgentID || request.Request.ExpiresAt.Sub(request.Request.RequestedAt) > 3*time.Minute {
		h.t.Fatal("code request lost its execution identity, type or bounded lifetime")
	}
	var bytesValue []byte
	if code == "" {
		bytesValue = []byte(value)
		defer clear(bytesValue)
	}
	delivery, err := agentevents.NewTransientSecretResponse(request.Request, bytesValue, code)
	runtimeCheck(h.t, err)
	h.replies[message.ID] = delivery
	runtimeCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), delivery))
	return delivery
}

func (h *runtimeHarness) Setup(command messages.Message, model *fakes.Model, key string) {
	h.t.Helper()
	for _, setup := range []struct{ name, answer string }{{"browser_model_provider", "other"}, {"browser_model_name", "fixture-model"}, {"browser_model_url", model.URL}} {
		request := h.Wait(func(m messages.Message) bool {
			return m.Type == controllerevents.QuestionRequested || m.Type == controllerevents.CommandFailed || m.Type == controllerevents.ActionSucceeded
		})
		payload, err := controllerevents.DecodeQuestionRequest(request)
		runtimeCheck(h.t, err)
		if payload.Key != setup.name || payload.CommandID != command.ID || model.Calls() != 0 {
			h.t.Fatal("model setup lost its order or reached the model before credentials")
		}
		h.Answer(request, setup.answer)
	}
	h.Secret(modelSecret(model.URL), key)
}

func (h *runtimeHarness) Result(command messages.Message) agent.ActionOutput {
	h.t.Helper()
	message := h.Wait(func(m messages.Message) bool {
		return m.CorrelationID == command.ID && (m.Type == controllerevents.ActionSucceeded || m.Type == controllerevents.CommandFailed)
	})
	if message.Type != controllerevents.ActionSucceeded {
		h.t.Fatal("agent action failed")
	}
	var output agent.ActionOutput
	runtimeCheck(h.t, json.Unmarshal(message.Payload, &output))
	runtimeCheck(h.t, output.Validate())
	h.Accept(message)
	return output
}

func (h *runtimeHarness) Accept(result messages.Message) {
	record := h.records[result.CorrelationID]
	now := time.Now().UTC()
	record.Result, record.CompletedAt = &result, &now
	h.records[result.CorrelationID] = record
	receipt, err := agentevents.NewResultAccepted(result)
	runtimeCheck(h.t, err)
	runtimeCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), receipt))
}

func (h *runtimeHarness) Control(binding sessions.Binding, method string) agent.AgentSnapshot {
	creation := binding.Creation
	command, err := messages.NewMessage(agentcommands.ControlSession, binding.OwnerAgentID, sessions.Control{Reference: binding.Reference, Creation: &creation, Method: method, RequestID: id.New()})
	runtimeCheck(h.t, err)
	runtimeCheck(h.t, (agentcommands.Client{Bus: h.bus}).Publish(h.t.Context(), command))
	message := h.Wait(func(m messages.Message) bool { return m.CorrelationID == command.ID })
	if message.Type != controllerevents.AgentUpdated {
		h.t.Fatal("session control failed")
	}
	var snapshot agent.AgentSnapshot
	runtimeCheck(h.t, json.Unmarshal(message.Payload, &snapshot))
	return snapshot
}

func (h *runtimeHarness) Persist(message messages.Message) agentevents.CheckpointResponse {
	request, err := controllerevents.DecodeCheckpointRequest(message)
	runtimeCheck(h.t, err)
	record, found := h.records[request.CommandID]
	if !found || record.Command.AgentID != message.AgentID || record.Command.OperationID != message.OperationID || record.Command.TaskID != message.TaskID {
		h.t.Fatal("checkpoint did not belong to its dispatched execution")
	}
	response := agentevents.CheckpointResponse{CommandID: request.CommandID, Name: request.Name}
	if request.Operation == controllerevents.CheckpointSave {
		if _, err := record.RecordCheckpoint(request.Name, request.Data); err != nil {
			response.Error = agentevents.CheckpointConflict
			return response
		}
	}
	h.records[request.CommandID] = record
	response.Data = bytes.Clone(record.Checkpoints[request.Name])
	if len(response.Data) == 0 {
		response.Error = agentevents.CheckpointNotFound
	}
	return response
}

func (h *runtimeHarness) Reply(request messages.Message, response agentevents.CheckpointResponse) {
	message, err := agentevents.NewCheckpointResponse(request, response)
	runtimeCheck(h.t, err)
	runtimeCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), message))
}

func (h *runtimeHarness) Facts(command messages.Message) map[string]json.RawMessage {
	return h.records[command.ID].Checkpoints
}

func (h *runtimeHarness) Download(output agent.ActionOutput, name string) []byte {
	h.t.Helper()
	for _, artifact := range output.Artifacts {
		for _, file := range artifact.Files {
			if file.Name == name {
				data, err := h.files.Get(h.t.Context(), artifact.ID, file)
				runtimeCheck(h.t, err)
				return data
			}
		}
	}
	h.t.Fatal("expected artifact file was not uploaded")
	return nil
}

func modelSecret(endpoint string) string {
	return fmt.Sprintf("browser-model-api-key-%x", sha256.Sum256([]byte(endpoint)))
}

func loginSecrets(origin, profile string) (string, string) {
	base := fmt.Sprintf("browser-login-%s-%x", profile, sha256.Sum256([]byte(origin+"\x00"+profile)))
	return base + "-username", base + "-password"
}

func runtimeJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	runtimeCheck(t, err)
	return data
}

func runtimeCheck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
