package browsing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

const hermesFenceLimit = 30

var errHermesCheckpoint = errors.New("cannot confirm native browser progress; inspect the session before an explicit continuation")

// HermesCheckpointLimits describe native iteration and response allowances, not
// a cost limit or a count of operations inside one Python invocation.
type HermesCheckpointLimits struct {
	MaxIterations        int `json:"max_iterations"`
	MaxTokensPerResponse int `json:"max_tokens_per_response"`
	TimeoutSeconds       int `json:"timeout_seconds"`
}

type hermesCheckpointLimits struct {
	HermesCheckpointLimits
	SharedFenceLimit int `json:"shared_fence_limit"`
}

type hermesCheckpointCounters struct {
	Fences               int `json:"fences"`
	NativeBatchesFenced  int `json:"native_batches_fenced"`
	NativeBatchesStarted int `json:"native_batches_started"`
	BatchesReturned      int `json:"batches_returned"`
	QuestionFences       int `json:"question_fences"`
	AnswersPrepared      int `json:"answers_prepared"`
	// Only Finish has the runtime's reported count; earlier facts are unknown.
	ModelRequests *int `json:"model_requests,omitempty"`
}

type hermesCheckpointFact struct {
	Version        int                      `json:"version"`
	Action         string                   `json:"action"`
	Runtime        string                   `json:"runtime"`
	Engine         string                   `json:"engine"`
	Session        sessions.CreationClaim   `json:"session"`
	Phase          string                   `json:"phase"`
	Limits         hermesCheckpointLimits   `json:"limits"`
	Counters       hermesCheckpointCounters `json:"counters"`
	PendingKind    string                   `json:"pending_kind,omitempty"`
	PendingOrdinal int                      `json:"pending_ordinal,omitempty"`
	BatchSequence  int                      `json:"batch_sequence,omitempty"`
	BatchSemantics string                   `json:"batch_semantics"`
	Status         string                   `json:"status,omitempty"`
	Submitted      bool                     `json:"submitted,omitempty"`
	Recovery       string                   `json:"recovery"`
	ResultDelivery string                   `json:"result_delivery"`
}

// HermesCheckpoints fence a whole native Python invocation or a non-secret
// question. They contain no browser snapshots, content, arguments or transcripts.
// Thirty shared fences plus start and finish fit Commander's immutable limit.
type HermesCheckpoints struct {
	action        string
	claim         sessions.CreationClaim
	execution     action.Execution
	hasExecution  bool
	enabled       bool
	gate          chan struct{}
	failureMu     sync.Mutex
	failure       error
	started       bool
	startAttempt  bool
	finishAttempt bool
	callback      bool
	limits        hermesCheckpointLimits
	counters      hermesCheckpointCounters
	pendingKind   string
	pending       int
	batchSequence int
	batchStage    string
}

// NewHermesCheckpoints must run before setup, secret requests or browser startup.
// Any existing start fence refuses replay, regardless of its schema or runtime.
func NewHermesCheckpoints(ctx context.Context, actionName string) (*HermesCheckpoints, error) {
	if actionName != "browse" && actionName != "login" {
		return nil, errHermesCheckpoint
	}
	if ctx.Err() != nil {
		return nil, hermesCheckpointError(ctx, ctx.Err())
	}
	binding, ok := action.SessionFromContext(ctx)
	if !ok || binding.Validate() != nil {
		return nil, errors.New("native browser checkpoints require the framework session binding")
	}
	c := &HermesCheckpoints{action: actionName, claim: binding.Claim(), gate: make(chan struct{}, 1)}
	c.execution, c.hasExecution = action.ExecutionFromContext(ctx)
	waiting, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	_, found, err := action.LoadCheckpoint(waiting, actionName+"_started")
	if errors.Is(err, action.ErrCheckpointsUnavailable) && waiting.Err() == nil {
		return c, nil
	}
	if err != nil {
		return nil, hermesCheckpointError(waiting, err)
	}
	if found {
		return nil, errors.New("browser work is already recorded for this execution; inspect its session and checkpoints before an explicit continuation")
	}
	c.enabled = true
	return c, nil
}

func (c *HermesCheckpoints) fail(err error) error {
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	if c.failure == nil {
		c.failure = err
	}
	return c.failure
}

func (c *HermesCheckpoints) failed() error {
	c.failureMu.Lock()
	defer c.failureMu.Unlock()
	return c.failure
}

func (c *HermesCheckpoints) enter(ctx context.Context) error {
	if ctx.Err() != nil {
		return c.fail(hermesCheckpointError(ctx, ctx.Err()))
	}
	select {
	case c.gate <- struct{}{}:
		if ctx.Err() != nil {
			c.leave()
			return c.fail(hermesCheckpointError(ctx, ctx.Err()))
		}
		if err := c.failed(); err != nil {
			c.leave()
			return err
		}
		return nil
	case <-ctx.Done():
		return c.fail(hermesCheckpointError(ctx, ctx.Err()))
	}
}

func (c *HermesCheckpoints) leave() { <-c.gate }

func (c *HermesCheckpoints) identity(ctx context.Context) bool {
	binding, ok := action.SessionFromContext(ctx)
	if !ok || binding.Validate() != nil || binding.Claim() != c.claim {
		return false
	}
	execution, present := action.ExecutionFromContext(ctx)
	return present == c.hasExecution && (!present || execution == c.execution)
}

// Start records the native-work fence exactly once, including without durable
// storage. Login uses the same fence but never starts model reasoning.
func (c *HermesCheckpoints) Start(ctx context.Context, limits HermesCheckpointLimits) error {
	if err := c.enter(ctx); err != nil {
		return err
	}
	defer c.leave()
	if c.startAttempt || c.callback || !c.identity(ctx) {
		return c.fail(errHermesCheckpoint)
	}
	c.startAttempt = true
	if limits.TimeoutSeconds < 1 || limits.TimeoutSeconds > 600 ||
		c.action == "browse" && (limits.MaxIterations < 1 || limits.MaxIterations > 30 || limits.MaxTokensPerResponse < 1 || limits.MaxTokensPerResponse > 16384) ||
		c.action == "login" && (limits.MaxIterations != 0 || limits.MaxTokensPerResponse != 0) {
		return c.fail(errHermesCheckpoint)
	}
	c.limits = hermesCheckpointLimits{HermesCheckpointLimits: limits, SharedFenceLimit: hermesFenceLimit}
	if err := c.save(ctx, c.action+"_started", c.fact("browser_work_pending")); err != nil {
		return err
	}
	if err := c.failed(); err != nil {
		return err
	}
	c.started = true
	return nil
}

// Handler persists admission/question fences before invoking the inner handler.
// Answers are counted only as prepared, never as delivered to the model.
func (c *HermesCheckpoints) Handler(inner hermes.Handler) hermes.Handler {
	return func(ctx context.Context, event hermes.Event) (hermes.Reply, error) {
		if err := c.enter(ctx); err != nil {
			return hermes.Reply{}, err
		}
		if !c.started || c.finishAttempt || c.callback || c.action != "browse" || !c.identity(ctx) || !validHermesCheckpointEvent(event) {
			c.leave()
			return hermes.Reply{}, c.fail(errHermesCheckpoint)
		}
		if err := c.event(ctx, event); err != nil {
			c.leave()
			return hermes.Reply{}, err
		}
		c.callback = true
		c.leave()

		// No lock or execution gate is held across user callbacks. A reentrant
		// call is refused by callback state rather than deadlocking its caller.
		var reply hermes.Reply
		var err error
		if inner != nil {
			reply, err = inner(ctx, event)
		} else if event.Kind != "progress" {
			err = errHermesCheckpoint
		}
		if err != nil {
			return hermes.Reply{}, c.fail(hermesCheckpointError(ctx, err))
		}
		if err := c.enter(ctx); err != nil {
			return hermes.Reply{}, err
		}
		defer c.leave()
		c.callback = false
		if !c.identity(ctx) || ctx.Err() != nil ||
			event.Kind == "admit" && (!reply.Approved || reply.Answer != "") ||
			event.Kind == "question" && !hermesCheckpointText(reply.Answer, 2000) {
			return hermes.Reply{}, c.fail(hermesCheckpointError(ctx, errHermesCheckpoint))
		}
		if event.Kind == "question" {
			c.counters.AnswersPrepared++
			c.pendingKind, c.pending = "", 0
		}
		return reply, nil
	}
}

// event runs under the execution gate and retains only fixed metadata.
func (c *HermesCheckpoints) event(ctx context.Context, event hermes.Event) error {
	if event.Kind == "progress" {
		if event.Tool == "" {
			return nil
		}
		if event.Sequence != c.batchSequence ||
			event.Text == "started" && c.batchStage != "fenced" ||
			event.Text == "finished" && c.batchStage != "started" {
			return c.fail(errHermesCheckpoint)
		}
		if event.Text == "started" {
			c.batchStage = "started"
			c.counters.NativeBatchesStarted++
		} else {
			c.batchStage = "returned"
			c.counters.BatchesReturned++
			c.pendingKind, c.pending = "", 0
		}
		return nil
	}
	if c.counters.Fences >= hermesFenceLimit || c.batchStage == "fenced" || c.batchStage == "started" {
		return c.fail(errHermesCheckpoint)
	}
	if event.Kind == "admit" {
		if event.Sequence != c.batchSequence+1 || event.Sequence > c.limits.MaxIterations {
			return c.fail(errHermesCheckpoint)
		}
		c.batchSequence = event.Sequence
		c.batchStage = "fenced"
		c.counters.NativeBatchesFenced++
	} else {
		if c.counters.QuestionFences >= 4 {
			return c.fail(errHermesCheckpoint)
		}
		c.counters.QuestionFences++
	}
	c.counters.Fences++
	c.pendingKind, c.pending = event.Kind, c.counters.Fences
	return c.save(ctx, fmt.Sprintf("%s_step_%02d", c.action, c.pending), c.fact("native_work_pending"))
}

// Finish runs after native cleanup and prepared-output validation. Artifact
// upload and controller acceptance occur later and remain explicitly unconfirmed.
func (c *HermesCheckpoints) Finish(ctx context.Context, status string, modelRequests int, submitted bool) error {
	if err := c.enter(ctx); err != nil {
		return err
	}
	defer c.leave()
	if !c.started || c.finishAttempt || c.callback || !c.identity(ctx) || modelRequests < 0 || modelRequests > 1_000_000 ||
		c.action == "browse" && (submitted || !hermesBrowseStatus(status)) ||
		c.action == "login" && (modelRequests != 0 || !hermesLoginStatus(status)) {
		return c.fail(errHermesCheckpoint)
	}
	c.finishAttempt = true
	c.counters.ModelRequests = &modelRequests
	fact := c.fact("work_prepared")
	fact.Status, fact.Submitted = status, submitted
	pending := c.counters.NativeBatchesFenced != c.counters.BatchesReturned || c.counters.QuestionFences != c.counters.AnswersPrepared
	if status == "failed" || status == "uncertain" || status == "interrupted" || status == "submission_uncertain" || pending {
		fact.Phase = "work_unconfirmed"
	}
	if status == "completed" && pending {
		fact.Phase, fact.Status = "work_unconfirmed", "uncertain"
	}
	if err := c.save(ctx, c.action+"_finished", fact); err != nil {
		return err
	}
	if status == "completed" && pending {
		return c.fail(errHermesCheckpoint)
	}
	return c.failed()
}

func (c *HermesCheckpoints) fact(phase string) hermesCheckpointFact {
	return hermesCheckpointFact{Version: 3, Action: c.action, Runtime: "hermes", Engine: "lightpanda", Session: c.claim,
		Phase: phase, Limits: c.limits, Counters: c.counters, PendingKind: c.pendingKind, PendingOrdinal: c.pending, BatchSequence: c.batchSequence,
		BatchSemantics: "whole_python_invocation", Recovery: "inspect_session_before_explicit_continuation", ResultDelivery: "unconfirmed"}
}

func (c *HermesCheckpoints) save(ctx context.Context, name string, fact hermesCheckpointFact) error {
	if !c.enabled {
		if ctx.Err() != nil {
			return c.fail(hermesCheckpointError(ctx, ctx.Err()))
		}
		return nil
	}
	data, err := json.Marshal(fact)
	if err != nil {
		return c.fail(errHermesCheckpoint)
	}
	waiting, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := action.SaveCheckpoint(waiting, name, data); err != nil {
		return c.fail(hermesCheckpointError(waiting, err))
	}
	return c.failed()
}

func validHermesCheckpointEvent(event hermes.Event) bool {
	switch event.Kind {
	case "admit":
		return event.Sequence > 0 && event.Tool == "browser_exec" && event.Text == "" && len(event.Options) == 0
	case "question":
		if event.Sequence != 0 || event.Tool != "" || !hermesCheckpointText(event.Text, 2000) || len(event.Options) > 4 {
			return false
		}
		for _, option := range event.Options {
			if !hermesCheckpointText(option, 500) {
				return false
			}
		}
		return true
	case "progress":
		return len(event.Options) == 0 && (event.Text == "started" || event.Text == "finished") &&
			(event.Tool == "" && event.Sequence == 0 || event.Tool == "browser_exec" && event.Sequence > 0)
	}
	return false
}

func hermesCheckpointText(text string, limit int) bool {
	return len(text) <= limit && strings.TrimSpace(text) != "" && utf8.ValidString(text) && !strings.ContainsRune(text, 0)
}

func hermesBrowseStatus(status string) bool {
	switch status {
	case "completed", "incomplete", "interrupted", "failed", "uncertain":
		return true
	}
	return false
}

func hermesLoginStatus(status string) bool {
	switch status {
	case "verified", "unverified", "unsupported_form", "requires_challenge", "submission_uncertain", "failed":
		return true
	}
	return false
}

func hermesCheckpointError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), context.Cause(ctx))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errHermesCheckpoint
}
