package browser_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/internal/commander"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

func TestBrowseFailureDistinguishesChildCancellationFromItsCaller(t *testing.T) {
	const unconfirmed = "native browser work is unconfirmed; inspect the session before an explicit continuation"
	for _, test := range []struct {
		name    string
		failure error
		want    string
	}{
		{name: "lost_runtime_cancel", failure: errors.Join(browserruntime.ErrRuntimeLost, browserruntime.ErrRuntimeUncertain, context.Canceled), want: "browser session was lost; native work is unconfirmed; inspect the session before an explicit continuation"},
		{name: "child_cancel", failure: context.Canceled, want: unconfirmed},
		{name: "child_deadline", failure: context.DeadlineExceeded, want: unconfirmed},
		{name: "model_failure", failure: contracts.ErrModelCredentials, want: contracts.ErrModelCredentials.Error() + "; inspect the session before an explicit continuation"},
		{name: "model_with_uncertain_cancel", failure: errors.Join(contracts.ErrModelCredentials, browserruntime.ErrRuntimeUncertain, context.Canceled), want: unconfirmed},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, checkpoints, binding := nativeCheckpointContext(t)
			result, err := runBrowseFailureFixture(t, ctx, binding, func(context.Context) error { return test.failure })
			if ctx.Err() != nil || err == nil || err.Error() != test.want || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("a nested failure impersonated caller cancellation or lost its safe diagnosis")
			}
			if len(result.Output) != 0 || len(result.Artifacts) != 0 || result.Session == nil || result.Session.Reference != binding.Reference {
				t.Fatal("failed browser work published output or lost its recovery session")
			}
			facts, _ := checkpoints.snapshot()
			var final struct {
				Phase  string `json:"phase"`
				Status string `json:"status"`
			}
			if json.Unmarshal(facts["browse_finished"], &final) != nil || final.Phase != "work_unconfirmed" || final.Status != "failed" {
				t.Fatal("a child failure skipped its unconfirmed final checkpoint")
			}
		})
	}
}

func TestBrowseFailurePreservesActualCallerCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			parent, _, binding := nativeCheckpointContext(t)
			var ctx context.Context
			var cancel context.CancelFunc
			var cause error
			want := context.Canceled
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(parent, time.Second)
				want = context.DeadlineExceeded
			} else {
				cause = errors.New("fixture caller cancellation cause")
				var stop context.CancelCauseFunc
				ctx, stop = context.WithCancelCause(parent)
				cancel = func() { stop(cause) }
			}
			defer cancel()
			result, err := runBrowseFailureFixture(t, ctx, binding, func(active context.Context) error {
				if mode == "cancel" {
					cancel()
				}
				<-active.Done()
				return errors.Join(browserruntime.ErrRuntimeUncertain, active.Err())
			})
			if !errors.Is(err, want) || cause != nil && !errors.Is(err, cause) || ctx.Err() != want || len(result.Output) != 0 || len(result.Artifacts) != 0 {
				t.Fatal("actual caller cancellation lost its context cause or published artifacts")
			}
		})
	}
}

func runBrowseFailureFixture(t *testing.T, ctx context.Context, binding sessions.Binding, failure func(context.Context) error) (action.Result, error) {
	t.Helper()
	t.Setenv("BROWSER_MODEL_PROVIDER", "openai")
	t.Setenv("BROWSER_MODEL_NAME", "chat-latest")
	probe := &unitSetup{t: t, key: "fixture-private-browse-failure-key"}
	ctx = action.WithSecrets(ctx, probe)
	setup := commander.Setup{ValidateModel: probe.validateModel}
	runner := &browseFailureRunner{unitSetup: probe, view: sessions.Session{Reference: binding.Reference, State: "lost"}}
	result, err := setup.RunHermesBrowse(ctx, runner, browserruntime.HermesOptions{},
		func(context.Context, contracts.ModelConfig) (contracts.BrowserAgent, error) {
			return browseFailureAgent(failure), nil
		},
		json.RawMessage(`{"objective":"Read the local fixture","url":"https://example.test/","timeout_seconds":5}`))
	if probe.runs != 1 || probe.validations != 1 {
		t.Fatal("failure fixture did not reach its configured browser capability")
	}
	assertUnitSetupCleared(t, probe)
	return result, err
}

type browseFailureRunner struct {
	*unitSetup
	view sessions.Session
}

func (r *browseFailureRunner) RunHermes(ctx context.Context, _ browserruntime.HermesOptions, work func(context.Context) error) (sessions.Session, error) {
	r.runs++
	return r.view, r.WithSession(ctx, work)
}

type browseFailureAgent func(context.Context) error

func (fail browseFailureAgent) Browse(ctx context.Context, _ contracts.BrowseRequest, _ contracts.Hooks) (contracts.BrowseResult, error) {
	return contracts.BrowseResult{Status: "failed", Text: "private native failure transcript"}, fail(ctx)
}
