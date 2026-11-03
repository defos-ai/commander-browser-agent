package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/actions"
	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

func TestCheckpointSavedStartRefusesReplayBeforeSetup(t *testing.T) {
	for _, name := range []string{actions.BrowseName, actions.LoginName} {
		t.Run(name, func(t *testing.T) {
			const private = "fixture-private-checkpoint-value"
			probe := &checkpointActionProbe{found: true, data: json.RawMessage(`{"private":"` + private + `"}`)}
			runner := &checkpointActionRunner{}
			executor := newCheckpointAction(t, name, runner)
			binding := newModelSetupBinding()
			questions, secrets := openAISetupQuestions(), &browseSecretProbe{}
			ctx := action.WithCheckpoints(checkpointActionContext(t.Context(), binding, questions, secrets), probe)
			result, err := executor.Run(ctx, checkpointActionInput(t, name, binding))
			if err == nil || !strings.Contains(err.Error(), "already recorded") || strings.Contains(err.Error(), private) ||
				runner.calls != 0 || len(questions.calls) != 0 || secrets.requests != 0 || probe.saves != 0 ||
				!slices.Equal(probe.loads, []string{name + "_started"}) || len(result.Output) != 0 || len(result.Artifacts) != 0 {
				t.Fatal("saved work was replayed, reached setup, or exposed checkpoint content")
			}
		})
	}
}

func TestCheckpointFailuresStopBrowserWorkWithoutExposingProviderErrors(t *testing.T) {
	for _, name := range []string{actions.BrowseName, actions.LoginName} {
		for _, stage := range []string{"load", "save"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				const private = "fixture-checkpoint-provider-private-error"
				probe := &checkpointActionProbe{}
				if stage == "load" {
					probe.loadErr = errors.New(private)
				} else {
					probe.saveErr = errors.New(private)
				}
				runner := &checkpointActionRunner{}
				executor := newCheckpointAction(t, name, runner)
				binding := newModelSetupBinding()
				questions := openAISetupQuestions()
				questions.answers["browser_login_profile"] = "work"
				secrets := &browseSecretProbe{value: []byte("fixture-managed-checkpoint-key")}
				ctx := action.WithCheckpoints(checkpointActionContext(t.Context(), binding, questions, secrets), probe)
				result, err := executor.Run(ctx, checkpointActionInput(t, name, binding))
				if err == nil || strings.Contains(err.Error(), private) || len(result.Output) != 0 || len(result.Artifacts) != 0 {
					t.Fatal("unconfirmed checkpoint exposed provider details or produced artifacts")
				}
				if stage == "load" {
					if probe.saves != 0 || runner.calls != 0 || len(questions.calls) != 0 || secrets.requests != 0 {
						t.Fatal("checkpoint read failure crossed the setup/browser boundary")
					}
				} else if probe.saves != 1 || runner.calls != 1 {
					t.Fatal("checkpoint save failure did not stop before browser operations")
				}
			})
		}
	}
}

func TestCheckpointLoadCancellationPreservesClassificationAndHidesCause(t *testing.T) {
	for _, name := range []string{actions.BrowseName, actions.LoginName} {
		t.Run(name, func(t *testing.T) {
			const private = "fixture-private-checkpoint-cancellation-cause"
			probe := &checkpointActionProbe{waiting: make(chan struct{})}
			runner := &checkpointActionRunner{}
			executor := newCheckpointAction(t, name, runner)
			binding := newModelSetupBinding()
			questions, secrets := openAISetupQuestions(), &browseSecretProbe{}
			parent, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			ctx := action.WithCheckpoints(checkpointActionContext(parent, binding, questions, secrets), probe)
			done := make(chan error, 1)
			go func() {
				_, err := executor.Run(ctx, checkpointActionInput(t, name, binding))
				done <- err
			}()
			select {
			case <-probe.waiting:
			case <-time.After(3 * time.Second):
				t.Fatal("action did not wait for its checkpoint")
			}
			cancel(errors.New(private))
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), private) || runner.calls != 0 || len(questions.calls) != 0 || secrets.requests != 0 {
					t.Fatal("checkpoint cancellation lost its classification, exposed its cause, or continued work")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancelled checkpoint wait did not stop")
			}
		})
	}
}

func TestCheckpointLoadStopsWhenPreparedSessionCloses(t *testing.T) {
	for _, name := range []string{actions.BrowseName, actions.LoginName} {
		t.Run(name, func(t *testing.T) {
			binding := newModelSetupBinding()
			manager, err := browserruntime.New(binding.OwnerAgentID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := manager.Close(); err != nil {
					t.Error("cannot close checkpoint session fixture")
				}
			})
			if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
				t.Fatal(err)
			}
			executor := newCheckpointAction(t, name, manager)
			probe := &checkpointActionProbe{waiting: make(chan struct{})}
			questions, secrets := openAISetupQuestions(), &browseSecretProbe{}
			ctx := action.WithCheckpoints(checkpointActionContext(t.Context(), binding, questions, secrets), probe)
			done := make(chan error, 1)
			go func() {
				_, err := executor.Run(ctx, checkpointActionInput(t, name, binding))
				done <- err
			}()
			select {
			case <-probe.waiting:
			case <-time.After(3 * time.Second):
				t.Fatal("action did not wait for its checkpoint")
			}
			closed, err := manager.Control(t.Context(), sessions.Control{Reference: binding.Reference, Method: "close", RequestID: id.New(), Creation: &binding.Creation})
			if err != nil || closed.State != "closed" || closed.Busy {
				t.Fatal("close did not confirm cleanup of the unstarted checkpoint session")
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || len(questions.calls) != 0 || secrets.requests != 0 || probe.saves != 0 {
					t.Fatal("session close did not cancel the checkpoint wait before setup")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("closed session left its checkpoint wait running")
			}
		})
	}
}

type checkpointActionProbe struct {
	loads            []string
	saves            int
	found            bool
	data             json.RawMessage
	loadErr, saveErr error
	waiting          chan struct{}
}

func (probe *checkpointActionProbe) LoadCheckpoint(ctx context.Context, name string) (json.RawMessage, bool, error) {
	probe.loads = append(probe.loads, name)
	if probe.waiting != nil {
		close(probe.waiting)
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	return bytes.Clone(probe.data), probe.found, probe.loadErr
}

func (probe *checkpointActionProbe) SaveCheckpoint(context.Context, string, json.RawMessage) error {
	probe.saves++
	return probe.saveErr
}

type checkpointActionRunner struct{ calls int }

func (runner *checkpointActionRunner) Run(ctx context.Context, _ browserruntime.Options, work func(context.Context) error) (sessions.Session, error) {
	runner.calls++
	return sessions.Session{}, work(ctx)
}

func newCheckpointAction(t *testing.T, name string, runner actions.BrowserRunner) action.Executor {
	t.Helper()
	var executor action.Executor
	if name == actions.BrowseName {
		executor = actions.NewBrowse(runner)
	} else {
		executor = actions.NewLogin(runner)
	}
	if status, err := executor.Discover(t.Context(), browseDiscoveryEnvironment(t), nil); err != nil || status != action.Ready {
		t.Fatal("checkpoint fixture action was not ready")
	}
	return executor
}

func checkpointActionContext(ctx context.Context, binding sessions.Binding, questions *modelSetupQuestions, secrets *browseSecretProbe) context.Context {
	ctx = action.WithSession(ctx, &binding)
	ctx = action.WithQuestions(ctx, questions)
	ctx = action.WithSecrets(ctx, secrets)
	return action.WithLogs(ctx, &loginLogProbe{})
}

func checkpointActionInput(t *testing.T, name string, binding sessions.Binding) json.RawMessage {
	t.Helper()
	input := modelSetupInput(binding)
	if name == actions.LoginName {
		input = loginInput(binding, "https://account.example.test", "/login", "work")
		delete(input, "credential_profile")
	}
	return marshalLoginInput(t, input)
}
