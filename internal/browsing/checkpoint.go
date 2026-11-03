package browsing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

// Checkpoints retain execution-local recovery facts, never browser state. The
// started fence, at most thirty command fences, and final fact fit Commander's
// thirty-two immutable checkpoints. A receipt is required before browser effects.
type Checkpoints struct {
	action  string
	claim   sessions.CreationClaim
	limits  CheckpointLimits
	enabled bool
	started bool
	failed  bool
	step    int
}

type CheckpointLimits struct {
	MaxSteps            int `json:"max_steps,omitempty"`
	TimeoutSeconds      int `json:"timeout_seconds"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
}

type checkpointFact struct {
	Version        int                    `json:"version"`
	Action         string                 `json:"action"`
	Engine         string                 `json:"engine"`
	Session        sessions.CreationClaim `json:"session"`
	Phase          string                 `json:"phase"`
	PendingCommand string                 `json:"pending_command,omitempty"`
	Limits         CheckpointLimits       `json:"limits"`
	Progress       *intelligence.Progress `json:"progress,omitempty"`
	Status         string                 `json:"status,omitempty"`
	Submitted      bool                   `json:"submitted,omitempty"`
	Recovery       string                 `json:"recovery"`
	ResultDelivery string                 `json:"result_delivery"`
}

// NewCheckpoints refuses to replay an execution which already crossed its start
// fence. A new execution can continue only through an explicit live session
// binding and fresh observations. Direct calls without a controller remain usable
// but provide no durable checkpoints.
func NewCheckpoints(ctx context.Context, actionName string) (*Checkpoints, error) {
	c := &Checkpoints{action: actionName}
	waiting, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	_, found, err := action.LoadCheckpoint(waiting, actionName+"_started")
	if errors.Is(err, action.ErrCheckpointsUnavailable) {
		return c, nil
	}
	if err != nil {
		return nil, checkpointError(ctx, err)
	}
	if found {
		return nil, errors.New("browser work is already recorded for this execution; inspect its session and checkpoints before an explicit continuation")
	}
	binding, ok := action.SessionFromContext(ctx)
	if !ok || binding.Validate() != nil {
		return nil, errors.New("browser checkpoints require the framework session binding")
	}
	c.claim, c.enabled = binding.Claim(), true
	return c, nil
}

func (c *Checkpoints) Start(ctx context.Context, limits CheckpointLimits, pendingCommand string) error {
	if !c.enabled {
		return nil
	}
	if c.started || c.failed {
		return errors.New("browser checkpoint start cannot be repeated")
	}
	c.limits = limits
	fact := c.fact("browser_work_pending")
	fact.PendingCommand = pendingCommand
	if err := c.save(ctx, c.action+"_started", fact); err != nil {
		return err
	}
	c.started = true
	return nil
}

// Progress includes prior confirmed steps and the next pending command. Its
// persistence does not prove that the pending command ran or had no side effects.
func (c *Checkpoints) Progress(ctx context.Context, progress intelligence.Progress) error {
	if !c.enabled {
		return nil
	}
	if !c.started || c.failed || progress.Usage.Steps != c.step+1 || progress.Usage.Steps > 30 {
		return errors.New("browser checkpoint progression is invalid")
	}
	fact := c.fact("command_pending")
	fact.PendingCommand, fact.Progress = progress.Command, &progress
	if err := c.save(ctx, fmt.Sprintf("%s_step_%02d", c.action, progress.Usage.Steps), fact); err != nil {
		return err
	}
	c.step = progress.Usage.Steps
	return nil
}

// Finish runs after browser work and cleanup, while the execution is still active.
// Artifact upload and controller acceptance occur later and remain unconfirmed.
func (c *Checkpoints) Finish(ctx context.Context, status string, progress *intelligence.Progress, submitted bool) error {
	if !c.enabled || !c.started || c.failed {
		return nil
	}
	fact := c.fact("work_prepared")
	fact.Status, fact.Progress, fact.Submitted = status, progress, submitted
	if status == "failed" || status == "submission_uncertain" {
		fact.Phase = "work_unconfirmed"
	}
	if progress != nil {
		for _, step := range progress.Steps {
			if step.Outcome == "unconfirmed" {
				fact.Phase = "work_unconfirmed"
			}
		}
	}
	return c.save(ctx, c.action+"_finished", fact)
}

func (c *Checkpoints) fact(phase string) checkpointFact {
	return checkpointFact{Version: 1, Action: c.action, Engine: "lightpanda", Session: c.claim,
		Phase: phase, Limits: c.limits, Recovery: "inspect_session_before_explicit_continuation", ResultDelivery: "unconfirmed"}
}

func (c *Checkpoints) save(ctx context.Context, name string, fact checkpointFact) error {
	data, err := json.Marshal(fact)
	if err == nil {
		waiting, stop := context.WithTimeout(ctx, 5*time.Second)
		err = action.SaveCheckpoint(waiting, name, data)
		stop()
	}
	if err != nil {
		c.failed = true
		return checkpointError(ctx, err)
	}
	return nil
}

func checkpointError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("cannot persist browser progress; inspect the session before retrying")
}
