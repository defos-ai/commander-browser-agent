//go:build hermesintegration

package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander/framework/agent/action"
	agentevents "github.com/defos-ai/commander/framework/comms/clients/agent/events"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

// This is a test-owned composition of the staged helpers, not another public
// action backend. It mirrors load/setup/start/run/finalize through real contexts.
func (h *hermesSessionHost) runCheckpointed(ctx context.Context, binding sessions.Binding, scenario hermesSessionScenario) (output hermesSessionOutput, runErr error) {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-scenario.cancel:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-joined
		if scenario.result != nil {
			scenario.result <- runErr
		}
	}()
	actionName := "browse"
	if scenario.login != nil {
		actionName = "login"
	}
	request := scenario.request
	var checkpoints *browsing.HermesCheckpoints
	runErr = h.manager.WithSession(ctx, func(setupCtx context.Context) error {
		var err error
		checkpoints, err = browsing.NewHermesCheckpoints(setupCtx, actionName)
		if err != nil {
			return err
		}
		if scenario.managed {
			request, err = h.setup.PrepareHermes(setupCtx)
			if err != nil {
				return err
			}
			request.Objective, request.MaxIterations, request.MaxTokens, request.TimeoutSeconds = "Complete the checkpoint fixture objective.", 4, 512, 25
		}
		limits := browsing.HermesCheckpointLimits{MaxIterations: request.MaxIterations, MaxTokensPerResponse: request.MaxTokens, TimeoutSeconds: request.TimeoutSeconds}
		if actionName == "login" {
			limits = browsing.HermesCheckpointLimits{TimeoutSeconds: 25}
		}
		return checkpoints.Start(setupCtx, limits)
	})
	if scenario.managed {
		defer clear(request.APIKey)
	}
	if runErr != nil {
		return output, runErr
	}
	output.Claim = binding.Claim()
	output.Session, runErr = h.manager.RunHermes(ctx, hermesScreenshotOptions(), func(workCtx context.Context) error {
		if scenario.login != nil {
			result, err := runHermesLoginFixture(workCtx, *scenario.login)
			output.Login = &result
			return err
		}
		handler := checkpoints.Handler(func(ctx context.Context, event hermes.Event) (hermes.Reply, error) {
			hermesEventPrivate(h.t, event, string(request.APIKey))
			switch event.Kind {
			case "admit":
				return hermes.Reply{Approved: true}, nil
			case "question":
				question, err := action.Question(ctx, "native_fixture_question", event.Text, event.Options...)
				return hermes.Reply{Answer: question.Answer}, err
			}
			return hermes.Reply{}, nil
		})
		var err error
		output.Result, err = hermes.Run(workCtx, request, handler)
		return err
	})
	status, submitted := output.Result.Status, false
	if output.Login != nil {
		status, submitted = output.Login.Status, output.Login.Submitted
	}
	if runErr != nil || status == "" {
		status = "failed"
	}
	// Validate the prepared typed result before publishing its final fact. The
	// framework repeats validation when this function returns to its Executor.
	encoded, encodeErr := json.Marshal(output)
	var value any
	schema, schemaErr := action.SchemaFor[hermesSessionOutput]()
	resolved, resolveErr := action.ResolveSchema(schema)
	if encodeErr != nil || json.Unmarshal(encoded, &value) != nil || schemaErr != nil || resolveErr != nil || resolved.Validate(value) != nil {
		runErr = errors.Join(runErr, errors.New("native fixture output validation failed"))
		status = "failed"
	}
	finishErr := h.manager.WithSession(ctx, func(finishCtx context.Context) error {
		return checkpoints.Finish(finishCtx, status, output.Result.ModelRequests, submitted)
	})
	if finishErr != nil {
		return hermesSessionOutput{}, errors.Join(runErr, finishErr)
	}
	return output, runErr
}

func (h *hermesSessionHost) checkpoint(operation, name string) messages.Message {
	h.t.Helper()
	return h.wait(func(message messages.Message) bool {
		if message.Type != controllerevents.CheckpointRequested {
			return false
		}
		request, err := controllerevents.DecodeCheckpointRequest(message)
		hermesSessionCheck(h.t, err)
		return request.Operation == operation && request.Name == name
	})
}

func (h *hermesSessionHost) persistCheckpoint(message messages.Message) agentevents.CheckpointResponse {
	h.t.Helper()
	request, err := controllerevents.DecodeCheckpointRequest(message)
	hermesSessionCheck(h.t, err)
	record, exists := h.checkpoints[request.CommandID]
	if !exists || record.Command.AgentID != message.AgentID || record.Command.OperationID != message.OperationID || record.Command.TaskID != message.TaskID {
		h.t.Fatal("native checkpoint lost its exact current execution envelope")
	}
	response := agentevents.CheckpointResponse{CommandID: request.CommandID, Name: request.Name}
	if request.Operation == controllerevents.CheckpointSave {
		if _, err := record.RecordCheckpoint(request.Name, request.Data); err != nil {
			response.Error = agentevents.CheckpointConflict
			return response
		}
	}
	h.checkpoints[request.CommandID] = record
	response.Data = bytes.Clone(record.Checkpoints[request.Name])
	if len(response.Data) == 0 {
		response.Error = agentevents.CheckpointNotFound
	}
	return response
}

func (h *hermesSessionHost) replyCheckpoint(request messages.Message, response agentevents.CheckpointResponse) {
	h.t.Helper()
	message, err := agentevents.NewCheckpointResponse(request, response)
	hermesSessionCheck(h.t, err)
	if response.Error != "" {
		// Preserve a refusal for already queued retries; a later automatic reply
		// must not accidentally turn that controller decision into approval.
		h.accepted[request.ID] = message
	}
	hermesSessionCheck(h.t, (agentevents.Client{Bus: h.bus}).Publish(h.t.Context(), message))
}

func (h *hermesSessionHost) refuseCheckpoint(message messages.Message) {
	h.t.Helper()
	request, err := controllerevents.DecodeCheckpointRequest(message)
	hermesSessionCheck(h.t, err)
	h.replyCheckpoint(message, agentevents.CheckpointResponse{CommandID: request.CommandID, Name: request.Name, Error: agentevents.CheckpointConflict})
}

func assertHermesCheckpointFacts(t *testing.T, h *hermesSessionHost, command messages.Message, binding sessions.Binding, forbidden ...string) {
	t.Helper()
	for _, data := range h.checkpoints[command.ID].Checkpoints {
		var fact struct {
			Version        int                    `json:"version"`
			Action         string                 `json:"action"`
			Runtime        string                 `json:"runtime"`
			Engine         string                 `json:"engine"`
			Session        sessions.CreationClaim `json:"session"`
			Phase          string                 `json:"phase"`
			Limits         map[string]int         `json:"limits"`
			Counters       map[string]int         `json:"counters"`
			PendingKind    string                 `json:"pending_kind"`
			PendingOrdinal int                    `json:"pending_ordinal"`
			BatchSequence  int                    `json:"batch_sequence"`
			BatchSemantics string                 `json:"batch_semantics"`
			Status         string                 `json:"status"`
			Submitted      bool                   `json:"submitted"`
			Recovery       string                 `json:"recovery"`
			ResultDelivery string                 `json:"result_delivery"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&fact) != nil || fact.Version != 3 || fact.Runtime != "hermes" || fact.Engine != "lightpanda" || fact.Session != binding.Claim() ||
			fact.Action != "browse" && fact.Action != "login" || fact.BatchSemantics != "whole_python_invocation" || fact.Recovery != "inspect_session_before_explicit_continuation" || fact.ResultDelivery != "unconfirmed" || fact.Limits["shared_fence_limit"] != 30 {
			t.Fatal("native checkpoint exceeded its metadata contract or changed its original claim")
		}
		for name := range fact.Counters {
			switch name {
			case "fences", "native_batches_fenced", "native_batches_started", "batches_returned", "question_fences", "answers_prepared", "model_requests":
			default:
				t.Fatal("native checkpoint introduced a non-counter observation field")
			}
		}
		for _, value := range forbidden {
			if value != "" && bytes.Contains(data, []byte(value)) {
				t.Fatal("native checkpoint persisted private model, browser or question content")
			}
		}
	}
}

func assertHermesCheckpointFailure(t *testing.T, h *hermesSessionHost, command messages.Message) {
	t.Helper()
	message := h.wait(func(message messages.Message) bool {
		return message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested ||
			message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed)
	})
	if message.Type != controllerevents.CommandFailed {
		t.Fatal("a refused native checkpoint published questions, secrets or successful output")
	}
	var returned struct {
		Output    json.RawMessage `json:"output"`
		Artifacts json.RawMessage `json:"artifacts"`
	}
	if json.Unmarshal(message.Payload, &returned) != nil || len(returned.Output) != 0 || len(returned.Artifacts) != 0 {
		t.Fatal("a refused native checkpoint exposed prepared output or artifacts")
	}
	h.pending = append([]messages.Message{message}, h.pending...)
	if _, ok := h.finish(command); ok {
		t.Fatal("a refused native checkpoint was accepted as successful work")
	}
}

func TestHermesCheckpointRecoveryGatePrecedesManagedSetup(t *testing.T) {
	for _, saved := range []bool{true, false} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			h := newHermesSessionHost(t, map[string]hermesSessionScenario{"guarded": {managed: true, checkpointed: true}})
			profiles := hermesSessionProfiles(t)
			root := h.task(nil, sessions.Open, "guarded")
			binding := h.binding(root)
			command := h.dispatch(root, &binding)
			load := h.checkpoint(controllerevents.CheckpointLoad, "browse_started")
			response := agentevents.CheckpointResponse{CommandID: command.ID, Name: "browse_started", Error: agentevents.CheckpointUnknownExecution}
			if saved {
				record := h.checkpoints[command.ID]
				_, err := record.RecordCheckpoint("browse_started", json.RawMessage(`{"version":2,"phase":"browser_work_pending"}`))
				hermesSessionCheck(t, err)
				h.checkpoints[command.ID] = record
				response = h.persistCheckpoint(load)
			}
			h.replyCheckpoint(load, response)
			assertHermesCheckpointFailure(t, h, command)
			if h.snapshot(binding.Reference).State != "starting" || !reflect.DeepEqual(profiles, hermesSessionProfiles(t)) {
				t.Fatal("recovery refusal started or replaced a native runtime")
			}
			for _, message := range h.history {
				if message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested {
					t.Fatal("recovery refusal ran managed setup before its load gate")
				}
			}
		})
	}
}

func TestHermesCheckpointReceiptsFenceQuestionsBatchesAndFinalization(t *testing.T) {
	var effects atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		effects.Add(1)
		_, _ = io.WriteString(w, "Checkpoint browser observation")
	}))
	t.Cleanup(site.Close)
	key, question, answer := "fixture-native-checkpoint-"+id.New(), "Which local detail should be read?", "Read the fixture"
	code := fmt.Sprintf("# Read one local fixture\nnew_tab(%q)\nprint(js('document.body.textContent'))", site.URL)
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		switch call {
		case 1:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("checkpoint-question", "clarify", map[string]any{"questions": []any{map[string]any{"question": question, "choices": []string{answer, "Stop the fixture"}}}})}}
		case 2:
			if !request.HasToolText(answer) {
				t.Error("the checkpointed question answer did not reach native reasoning")
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("checkpoint-batch", "browser_exec", map[string]any{"code": code})}}
		default:
			if call != 3 || !request.HasToolText("Checkpoint browser observation") {
				t.Error("checkpoint receipt replay repeated model or browser work")
			}
			return fakes.HermesResponse{Text: "Checkpoint fixture completed."}
		}
	})
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{"guarded": {managed: true, checkpointed: true}})
	root := h.task(nil, sessions.Open, "guarded")
	binding := h.binding(root)
	command := h.dispatch(root, &binding)
	h.answerSetup(command, "browser_model_provider", "Other (OpenAI-compatible)")
	h.answerSetup(command, "browser_model_name", "gpt-4.1-mini")
	endpoint := model.URL + "/chat/completions"
	h.answerSetup(command, "browser_model_url", endpoint)
	h.deliverModelKey(command, modelSecretName(endpoint), key, 1)
	start := h.checkpoint(controllerevents.CheckpointSave, "browse_started")
	startReceipt := h.persistCheckpoint(start)
	startRetry := h.checkpoint(controllerevents.CheckpointSave, "browse_started")
	if !reflect.DeepEqual(start, startRetry) || model.Calls() != 0 || effects.Load() != 0 || h.snapshot(binding.Reference).State != "starting" {
		t.Fatal("unconfirmed start receipt allowed runtime or model work")
	}
	h.replyCheckpoint(startRetry, startReceipt)
	clarification := h.checkpoint(controllerevents.CheckpointSave, "browse_step_01")
	clarificationReceipt := h.persistCheckpoint(clarification)
	clarificationRetry := h.checkpoint(controllerevents.CheckpointSave, "browse_step_01")
	if !reflect.DeepEqual(clarification, clarificationRetry) || model.Calls() != 1 || effects.Load() != 0 {
		t.Fatal("unconfirmed clarification receipt repeated native reasoning")
	}
	for _, message := range h.history {
		if message.Type == controllerevents.QuestionRequested {
			request, err := controllerevents.DecodeQuestionRequest(message)
			hermesSessionCheck(t, err)
			if request.Key == "native_fixture_question" {
				t.Fatal("clarification was published before its checkpoint receipt")
			}
		}
	}
	h.replyCheckpoint(clarificationRetry, clarificationReceipt)
	h.answerSetup(command, "native_fixture_question", answer)
	batch := h.checkpoint(controllerevents.CheckpointSave, "browse_step_02")
	batchReceipt := h.persistCheckpoint(batch)
	// A validly shaped receipt with conflicting data does not grant dispatch.
	wrong := batchReceipt
	wrong.Data = json.RawMessage(`{"receipt":"conflicting"}`)
	h.replyCheckpoint(batch, wrong)
	batchRetry := h.checkpoint(controllerevents.CheckpointSave, "browse_step_02")
	if !reflect.DeepEqual(batch, batchRetry) || model.Calls() != 2 || effects.Load() != 0 {
		t.Fatal("missing or conflicting batch receipt allowed a native browser effect")
	}
	h.replyCheckpoint(batchRetry, batchReceipt)
	final := h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	finalReceipt := h.persistCheckpoint(final)
	var fact struct {
		Counters map[string]int `json:"counters"`
	}
	if json.Unmarshal(finalReceipt.Data, &fact) != nil || fact.Counters["fences"] != 2 || fact.Counters["native_batches_fenced"] != 1 ||
		fact.Counters["native_batches_started"] != 1 || fact.Counters["batches_returned"] != 1 || fact.Counters["question_fences"] != 1 || fact.Counters["answers_prepared"] != 1 || fact.Counters["model_requests"] != 3 {
		t.Fatal("native checkpoints lost their shared question/batch ordinals or honest return counters")
	}
	finalRetry := h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	if !reflect.DeepEqual(final, finalRetry) || model.Calls() != 3 || effects.Load() != 1 {
		t.Fatal("final checkpoint retry repeated finished native work")
	}
	for _, message := range h.history {
		if message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed) {
			t.Fatal("native output was published before the final checkpoint receipt")
		}
	}
	h.replyCheckpoint(finalRetry, finalReceipt)
	output, ok := h.finish(command)
	if !ok || output.Result.Status != "completed" || output.Claim != binding.Claim() || len(h.checkpoints[command.ID].Checkpoints) != 4 {
		t.Fatal("checkpointed native flow lost its result, combined fence count or original claim")
	}
	assertHermesCheckpointFacts(t, h, command, binding, key, site.URL, code, question, answer, "Checkpoint browser observation", "Complete the checkpoint fixture objective.")
}

func modelSecretName(endpoint string) string {
	return fmt.Sprintf("browser-model-api-key-%x", sha256.Sum256([]byte(endpoint)))
}

func TestHermesCheckpointFailuresRefuseOutputAndNativeReplay(t *testing.T) {
	for _, phase := range []string{"question", "batch", "final"} {
		t.Run(phase, func(t *testing.T) {
			var effects atomic.Int32
			site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				effects.Add(1)
				_, _ = io.WriteString(w, "Receipt failure fixture")
			}))
			t.Cleanup(site.Close)
			key := "fixture-checkpoint-refusal-" + id.New()
			code := fmt.Sprintf("# Attempt one local batch\nnew_tab(%q)\nprint(js('document.body.textContent'))", site.URL)
			model := fakes.NewHermesModel(t, key, func(_ fakes.HermesRequest, call int) fakes.HermesResponse {
				if call == 1 {
					if phase == "question" {
						return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("refused-question", "clarify", map[string]any{"questions": []any{map[string]any{"question": "Read the local fixture?"}}})}}
					}
					tools := []fakes.HermesToolCall{fakes.HermesCall("first-native-effect", "browser_exec", map[string]any{"code": code})}
					if phase == "batch" {
						tools = append(tools, fakes.HermesCall("different-native-effect", "browser_exec", map[string]any{"code": code}))
					}
					return fakes.HermesResponse{Tools: tools}
				}
				return fakes.HermesResponse{Text: "Receipt fixture completed."}
			})
			h := newHermesSessionHost(t, map[string]hermesSessionScenario{"guarded": {request: hermesRequest(model, []byte(key)), checkpointed: true}})
			root := h.task(nil, sessions.Open, "guarded")
			binding := h.binding(root)
			command := h.dispatch(root, &binding)
			name := "browse_step_01"
			if phase == "final" {
				name = "browse_finished"
			}
			pending := h.checkpoint(controllerevents.CheckpointSave, name)
			h.refuseCheckpoint(pending)
			assertHermesCheckpointFailure(t, h, command)
			expectedEffects := int32(0)
			if phase == "final" {
				expectedEffects = 1
			}
			if effects.Load() != expectedEffects || model.Calls() > 5 || h.snapshot(binding.Reference).Reference != binding.Reference {
				t.Fatal("checkpoint refusal replayed a native effect or discarded its retained reference")
			}
			for _, message := range h.history {
				if message.Type == controllerevents.QuestionRequested || message.Type == controllerevents.SecretSyncRequested {
					t.Fatal("a rejected checkpoint published a question or requested credentials")
				}
				if message.Type == controllerevents.CheckpointRequested {
					request, err := controllerevents.DecodeCheckpointRequest(message)
					hermesSessionCheck(t, err)
					if request.Name == "browse_step_02" {
						t.Fatal("a rejected native batch admitted a different-ID retry")
					}
				}
			}
			assertHermesCheckpointFacts(t, h, command, binding, key, site.URL, code, "Receipt failure fixture")
		})
	}
}

func TestHermesCheckpointCancellationRejectsLateReceiptAndAllowsExplicitContinuation(t *testing.T) {
	var effects atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		effects.Add(1)
		_, _ = io.WriteString(w, "Checkpoint cancellation account")
	}))
	t.Cleanup(site.Close)
	firstModel, first := hermesSessionScript(t, fmt.Sprintf("# Open the retained fixture\nnew_tab(%q)\nprint(js('document.body.textContent'))", site.URL), "Checkpoint cancellation account")
	model, request := hermesSessionScript(t, fmt.Sprintf("# Continue the retained fixture\ngoto_url(%q)\nprint(js('document.body.textContent'))", site.URL), "Checkpoint cancellation account")
	continueModel, continued := hermesSessionScript(t, fmt.Sprintf("# Explicitly continue the retained fixture\ngoto_url(%q)\nprint(js('document.body.textContent'))", site.URL), "Checkpoint cancellation account")
	cancel, cancelled := make(chan struct{}), make(chan error, 1)
	h := newHermesSessionHost(t, map[string]hermesSessionScenario{
		"first": {request: first}, "guarded": {request: request, checkpointed: true, cancel: cancel, result: cancelled}, "continue": {request: continued},
	})
	root := h.task(nil, sessions.Open, "first")
	binding := h.binding(root)
	h.call(root)
	child := h.task(&root, sessions.Open, "guarded")
	childBinding := h.binding(child)
	command := h.dispatch(child, &childBinding)
	pending := h.checkpoint(controllerevents.CheckpointSave, "browse_step_01")
	receipt := h.persistCheckpoint(pending)
	if effects.Load() != 1 || firstModel.Calls() != 2 || model.Calls() != 1 || childBinding.Claim() != binding.Claim() {
		t.Fatal("native cancellation fixture crossed its unconfirmed batch fence")
	}
	close(cancel)
	assertHermesCheckpointFailure(t, h, command)
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("native checkpoint cancellation lost its caller cause")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("native checkpoint cancellation did not join its pending callback")
	}
	h.replyCheckpoint(pending, receipt)
	if h.snapshot(binding.Reference).State != "connected" || effects.Load() != 1 || model.Calls() != 1 {
		t.Fatal("a late checkpoint receipt resumed work or lost a quiescent browser")
	}
	if output := h.call(h.task(&child, sessions.Open, "continue")); output.Claim != binding.Claim() || output.Result.Status != "completed" || effects.Load() != 2 || model.Calls() != 1 || continueModel.Calls() != 2 {
		t.Fatal("explicit continuation did not preserve the original live session")
	}
	assertHermesCheckpointFacts(t, h, command, binding, site.URL, string(request.APIKey), "Checkpoint cancellation account")
}
