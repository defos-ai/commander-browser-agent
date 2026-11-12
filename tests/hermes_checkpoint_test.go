package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/comms/messages"
	"github.com/defos-ai/commander/framework/domain/executions"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

var nativeCheckpointLimits = browsing.HermesCheckpointLimits{
	MaxIterations: 30, MaxTokensPerResponse: 256, TimeoutSeconds: 60,
}

const checkpointPrivateToken = "fixture-key-never-in-checkpoints"
const checkpointPrivateQuestion = "question-content-never-in-checkpoints"
const checkpointPrivateOption = "question-option-never-in-checkpoints"
const checkpointPrivateAnswer = "answer-content-never-in-checkpoints"

func TestHermesCheckpointQuestionsAndNativeCallsShareImmutableBudget(t *testing.T) {
	ctx, store, binding := nativeCheckpointContext(t)
	checkpoints, err := browsing.NewHermesCheckpoints(ctx, "browse")
	if err != nil || checkpoints.Start(ctx, nativeCheckpointLimits) != nil {
		t.Fatal("native checkpoint start failed")
	}
	innerCalls, fences := 0, 0
	handler := checkpoints.Handler(func(_ context.Context, event hermes.Event) (hermes.Reply, error) {
		innerCalls++
		if event.Kind != "progress" {
			fences++
		}
		facts, writes := store.snapshot()
		if len(facts) != 1+fences || writes != 1+fences {
			t.Error("native work or question publication preceded its persistence receipt")
		}
		if event.Kind == "question" {
			return hermes.Reply{Answer: checkpointPrivateAnswer}, nil
		}
		return hermes.Reply{Approved: event.Kind == "admit"}, nil
	})
	nativeCheckpointEvents(t, ctx, handler)
	if err := checkpoints.Finish(ctx, "completed", 27, false); err != nil {
		t.Fatal("thirty shared fences left no room for the final checkpoint")
	}
	facts, writes := store.snapshot()
	if len(facts) != executions.MaxCheckpoints || writes != executions.MaxCheckpoints || innerCalls != 82 {
		t.Fatal("questions, progress or native calls used separate checkpoint allowances")
	}
	for _, name := range []string{"browse_started", "browse_finished"} {
		if _, exists := facts[name]; !exists {
			t.Fatal("native checkpoint lifecycle fact is missing")
		}
	}
	for step := 1; step <= 30; step++ {
		if _, exists := facts[fmt.Sprintf("browse_step_%02d", step)]; !exists {
			t.Fatal("native checkpoint fence names did not share one ordered sequence")
		}
	}
	for name, raw := range facts {
		for _, private := range []string{checkpointPrivateToken, checkpointPrivateQuestion, checkpointPrivateOption, checkpointPrivateAnswer} {
			if bytes.Contains(raw, []byte(private)) {
				t.Fatal("native checkpoint retained private question, answer or token content")
			}
		}
		var fact struct {
			Action         string                 `json:"action"`
			Runtime        string                 `json:"runtime"`
			Engine         string                 `json:"engine"`
			Session        sessions.CreationClaim `json:"session"`
			ResultDelivery string                 `json:"result_delivery"`
			Counters       struct {
				ModelRequests *int `json:"model_requests"`
			} `json:"counters"`
		}
		if json.Unmarshal(raw, &fact) != nil || fact.Action != "browse" || fact.Runtime != "hermes" ||
			fact.Engine != "lightpanda" || fact.Session != binding.Claim() || fact.ResultDelivery != "unconfirmed" {
			t.Fatal("checkpoint metadata lost its exact ownership or claimed accepted result delivery")
		}
		if name != "browse_finished" && fact.Counters.ModelRequests != nil {
			t.Fatal("an intermediate checkpoint claimed unknown model usage")
		}
	}
	var finished struct {
		Counters map[string]int `json:"counters"`
		Limits   struct {
			SharedFenceLimit int `json:"shared_fence_limit"`
		} `json:"limits"`
	}
	if json.Unmarshal(facts["browse_finished"], &finished) != nil || finished.Limits.SharedFenceLimit != 30 ||
		finished.Counters["fences"] != 30 || finished.Counters["question_fences"] != 4 ||
		finished.Counters["native_batches_fenced"] != 26 || finished.Counters["batches_returned"] != 26 ||
		finished.Counters["model_requests"] != 27 {
		t.Fatal("final metadata did not distinguish shared fences, returned native batches and model requests")
	}
	if err := checkpoints.Finish(ctx, "failed", 28, false); err == nil {
		t.Fatal("the prepared final fact could be rewritten")
	}
	if _, err := browsing.NewHermesCheckpoints(ctx, "browse"); err == nil {
		t.Fatal("an execution with a start fence could be replayed")
	}
	if _, after := store.snapshot(); after != writes {
		t.Fatal("finalization or replay attempted another immutable write")
	}
}

func TestHermesCheckpointOverflowStopsBeforePublicationAndCannotFinish(t *testing.T) {
	for _, durable := range []bool{true, false} {
		t.Run(fmt.Sprintf("durable_%t", durable), func(t *testing.T) {
			ctx, store, _ := nativeCheckpointContext(t)
			if !durable {
				ctx = action.WithCheckpoints(ctx, nil)
			}
			checkpoints, err := browsing.NewHermesCheckpoints(ctx, "browse")
			if err != nil || checkpoints.Start(ctx, nativeCheckpointLimits) != nil {
				t.Fatal("native checkpoint budget could not start")
			}
			innerCalls := 0
			handler := checkpoints.Handler(func(_ context.Context, event hermes.Event) (hermes.Reply, error) {
				innerCalls++
				if event.Kind == "question" {
					return hermes.Reply{Answer: checkpointPrivateAnswer}, nil
				}
				return hermes.Reply{Approved: event.Kind == "admit"}, nil
			})
			nativeCheckpointEvents(t, ctx, handler)
			beforeFacts, beforeWrites := store.snapshot()
			for _, sequence := range []int{27, 28} {
				if _, err := handler(ctx, hermes.Event{Kind: "admit", Tool: "browser_exec", Sequence: sequence}); err == nil {
					t.Fatal("an exhausted fence budget admitted another native call")
				}
			}
			if err := checkpoints.Finish(ctx, "completed", 27, false); err == nil || innerCalls != 82 {
				t.Fatal("an overflow reached publication or was hidden by finalization")
			}
			afterFacts, afterWrites := store.snapshot()
			if len(afterFacts) != len(beforeFacts) || afterWrites != beforeWrites ||
				durable && len(afterFacts) != 31 || !durable && len(afterFacts) != 0 {
				t.Fatal("overflow or a renamed retry consumed another checkpoint fact")
			}
		})
	}
}

func TestHermesCheckpointRejectsForeignClaimAndDifferentLeafExecution(t *testing.T) {
	for _, mismatch := range []string{"creation_claim", "current_leaf_execution"} {
		t.Run(mismatch, func(t *testing.T) {
			ctx, store, binding := nativeCheckpointContext(t)
			foreign := ctx
			if mismatch == "creation_claim" {
				binding.OperationID = id.New()
				foreign = action.WithSession(ctx, &binding)
			} else {
				execution, _ := action.ExecutionFromContext(ctx)
				execution.CommandID, execution.TaskID = id.New(), id.New()
				foreign = action.WithExecution(ctx, execution) // Original creation claim is unchanged.
			}
			checkpoints, err := browsing.NewHermesCheckpoints(ctx, "browse")
			if err != nil || checkpoints.Start(foreign, nativeCheckpointLimits) == nil {
				t.Fatal("a different claim or leaf execution started another owner's checkpoints")
			}
			if facts, writes := store.snapshot(); len(facts) != 0 || writes != 0 {
				t.Fatal("identity mismatch reached checkpoint persistence")
			}
			checkpoints, err = browsing.NewHermesCheckpoints(ctx, "browse")
			if err != nil || checkpoints.Start(ctx, nativeCheckpointLimits) != nil {
				t.Fatal("fresh checkpoint fixture could not start")
			}
			innerCalls := 0
			handler := checkpoints.Handler(func(context.Context, hermes.Event) (hermes.Reply, error) {
				innerCalls++
				return hermes.Reply{Approved: true}, nil
			})
			if _, err := handler(foreign, hermes.Event{Kind: "admit", Tool: "browser_exec", Sequence: 1}); err == nil || innerCalls != 0 {
				t.Fatal("a foreign context reached the native admission handler")
			}
			if err := checkpoints.Finish(ctx, "completed", 1, false); err == nil {
				t.Fatal("an identity failure could be hidden by later finalization")
			}
			if facts, writes := store.snapshot(); len(facts) != 1 || writes != 1 {
				t.Fatal("foreign admission or subsequent finalization persisted another fact")
			}
		})
	}
}

func nativeCheckpointEvents(t *testing.T, ctx context.Context, handler hermes.Handler) {
	t.Helper()
	for question := 0; question < 4; question++ {
		if _, err := handler(ctx, hermes.Event{Kind: "question", Text: checkpointPrivateQuestion + " " + checkpointPrivateToken,
			Options: []string{checkpointPrivateOption}}); err != nil {
			t.Fatal("a bounded clarification could not reserve its shared fence")
		}
	}
	for sequence := 1; sequence <= 26; sequence++ {
		if _, err := handler(ctx, hermes.Event{Kind: "admit", Tool: "browser_exec", Sequence: sequence}); err != nil {
			t.Fatal("a native call could not reserve its shared fence")
		}
		for _, phase := range []string{"started", "finished"} {
			if _, err := handler(ctx, hermes.Event{Kind: "progress", Tool: "browser_exec", Sequence: sequence, Text: phase}); err != nil {
				t.Fatal("native progress unexpectedly consumed another fence")
			}
		}
	}
}

type memoryHermesCheckpoints struct {
	mu     sync.Mutex
	record executions.Execution
	writes int
}

func (store *memoryHermesCheckpoints) LoadCheckpoint(ctx context.Context, name string) (json.RawMessage, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	raw, found := store.record.Checkpoints[name]
	return bytes.Clone(raw), found, ctx.Err()
}

func (store *memoryHermesCheckpoints) SaveCheckpoint(ctx context.Context, name string, data json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.writes++
	_, err := store.record.RecordCheckpoint(name, data)
	return err
}

func (store *memoryHermesCheckpoints) snapshot() (map[string]json.RawMessage, int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	facts := make(map[string]json.RawMessage, len(store.record.Checkpoints))
	for name, data := range store.record.Checkpoints {
		facts[name] = bytes.Clone(data)
	}
	return facts, store.writes
}

func nativeCheckpointContext(t *testing.T) (context.Context, *memoryHermesCheckpoints, sessions.Binding) {
	t.Helper()
	binding := unitBinding()
	execution := action.Execution{CommandID: id.New(), OperationID: binding.OperationID, TaskID: id.New()}
	command, err := messages.NewMessage(messages.RunAction, binding.OwnerAgentID, map[string]any{})
	if err != nil {
		t.Fatal("checkpoint fixture command is invalid")
	}
	command.ID, command.OperationID, command.TaskID = execution.CommandID, execution.OperationID, execution.TaskID
	store := &memoryHermesCheckpoints{record: executions.Execution{Command: command, StartedAt: time.Now().UTC()}}
	if err := store.record.Validate(); err != nil {
		t.Fatal("checkpoint fixture execution is invalid")
	}
	ctx := action.WithCheckpoints(action.WithExecution(action.WithSession(t.Context(), &binding), execution), store)
	return ctx, store, binding
}

var _ action.CheckpointProvider = (*memoryHermesCheckpoints)(nil)
