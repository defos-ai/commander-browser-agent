package browsing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	browserruntime "github.com/defos-ai/commander-browser-agent/browser"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/artifacts"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

// HermesBrowseRunner is the existing host owner, not another browser manager.
// Preparation and finalization preserve the session lifetime without starting it.
type HermesBrowseRunner interface {
	WithSession(context.Context, func(context.Context) error) error
	RunHermes(context.Context, browserruntime.HermesOptions, func(context.Context) error) (sessions.Session, error)
}

// RunHermesBrowse stages the native browse contract without changing the default
// action catalog. Runtime paths are trusted discovery output, never action input.
// The Setup receiver retains only non-secret choices for the exact creation claim.
func (s *Setup) RunHermesBrowse(ctx context.Context, runner HermesBrowseRunner, options browserruntime.HermesOptions, raw json.RawMessage) (result action.Result, runErr error) {
	if ctx.Err() != nil {
		return result, errors.Join(ctx.Err(), context.Cause(ctx))
	}
	input, err := DecodeHermesBrowseInput(raw)
	if err != nil {
		return result, err
	}
	if runner == nil {
		return result, errors.New("native browse requires the host browser session runner")
	}
	if _, err := action.OpenSession(ctx); err != nil {
		return result, err
	}
	var request hermes.Request
	var checkpoints *HermesCheckpoints
	setupCtx, stopSetup := context.WithTimeout(ctx, 10*time.Minute)
	err = runner.WithSession(setupCtx, func(ctx context.Context) error {
		var err error
		checkpoints, err = NewHermesCheckpoints(ctx, "browse")
		if err != nil {
			return err
		}
		request, err = s.PrepareHermes(ctx)
		return err
	})
	if setupCtx.Err() != nil {
		err = errors.Join(err, setupCtx.Err(), context.Cause(setupCtx))
	}
	stopSetup()
	// Worker.Run joins the model transport before returning; owned key bytes
	// cannot be cleared while its HTTP work is still using them.
	defer clear(request.APIKey)
	if err != nil {
		return result, err
	}
	if len(request.APIKey) == 0 || len(request.APIKey) > sensitive.MaxValueBytes {
		return result, errors.New("native browser model credentials are invalid")
	}
	for _, value := range request.APIKey {
		if value < 0x21 || value > 0x7e {
			return result, errors.New("native browser model credentials are invalid")
		}
	}
	request.MaxIterations, request.MaxTokens, request.TimeoutSeconds = input.MaxIterations, input.MaxTokensPerResponse, input.TimeoutSeconds
	// One wall clock covers admission, startup, questions, native work and final
	// persistence. It cannot extend the execution's existing outer deadline.
	workCtx, stopWork := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
	defer stopWork()
	err = runner.WithSession(workCtx, func(ctx context.Context) error {
		if err := sensitive.Remember(ctx, request.APIKey); err != nil {
			return errors.New("cannot protect native model credentials; use a new session")
		}
		if err := CheckURLCredential(input.URL, string(request.APIKey)); err != nil {
			return err
		}
		if sensitive.Redact(ctx, input.URL) != input.URL {
			return errors.New("starting URL contains protected session values")
		}
		request.Objective = sensitive.Redact(ctx, input.ObjectiveForHermes())
		if len(request.Objective) > 16384 {
			return errors.New("protected native objective exceeds its size limit")
		}
		return checkpoints.Start(ctx, HermesCheckpointLimits{MaxIterations: input.MaxIterations,
			MaxTokensPerResponse: input.MaxTokensPerResponse, TimeoutSeconds: input.TimeoutSeconds})
	})
	if err != nil {
		return result, hermesBrowseError(workCtx, err)
	}

	var native hermes.Result
	questions := 0
	snapshot, runErr := runner.RunHermes(workCtx, options, func(ctx context.Context) error {
		handler := checkpoints.Handler(func(ctx context.Context, event hermes.Event) (hermes.Reply, error) {
			switch event.Kind {
			case "admit":
				return hermes.Reply{Approved: true}, nil
			case "question":
				questions++
				if questions > 4 {
					return hermes.Reply{}, errors.New("native browser clarification limit reached")
				}
				waiting, stop := context.WithTimeout(ctx, 3*time.Minute)
				defer stop()
				const notice = "Browser clarification (non-secret). Do not enter passwords, credentials, authentication codes or API keys.\n\n"
				options := make([]string, len(event.Options))
				for i, option := range event.Options {
					options[i] = sensitive.Redact(ctx, option)
				}
				question, err := action.Question(waiting, fmt.Sprintf("browser_clarification_%d", questions), notice+sensitive.Redact(ctx, event.Text), options...)
				return hermes.Reply{Answer: sensitive.Redact(ctx, question.Answer)}, err
			case "progress":
				message := "Native browser reasoning " + event.Text
				if event.Tool != "" {
					message = "Native browser batch " + event.Text
				}
				err := action.Log(ctx, message)
				if errors.Is(err, action.ErrLogsUnavailable) {
					err = nil // Direct calls can omit execution logs.
				}
				return hermes.Reply{}, err
			}
			return hermes.Reply{}, errors.New("unexpected native browser event")
		})
		var err error
		native, err = hermes.Run(ctx, request, handler)
		return err
	})
	result.Session = &snapshot
	// Validate prepared output only after native quiescence and manager health
	// checks. The final checkpoint receipt precedes artifact/result publication.
	finishErr := runner.WithSession(workCtx, func(ctx context.Context) error {
		status := native.Status
		if runErr != nil || status == "" {
			status = "failed"
		}
		var encoded []byte
		var outputErr error
		if runErr == nil {
			output := HermesBrowseOutput{Runtime: "hermes", Engine: "lightpanda", Status: status,
				Text: sensitive.Redact(ctx, native.Text), ModelRequests: native.ModelRequests}
			outputErr = output.Validate()
			if outputErr == nil {
				encoded, outputErr = json.Marshal(output)
			}
			if outputErr != nil {
				status = "failed"
			}
		}
		if err := checkpoints.Finish(ctx, status, native.ModelRequests, false); err != nil {
			return err
		}
		if outputErr != nil {
			return outputErr
		}
		if runErr == nil {
			result.Output = encoded
			result.Artifacts = []action.Artifact{{Name: "Native browser result", Files: []artifacts.FileSource{
				{Name: "browse.json", MediaType: "application/json", Data: encoded},
			}}}
		}
		return nil
	})
	runErr = errors.Join(runErr, finishErr)
	if workCtx.Err() != nil {
		runErr = errors.Join(runErr, workCtx.Err(), context.Cause(workCtx))
	}
	if runErr != nil {
		return action.Result{Session: result.Session}, hermesBrowseError(workCtx, runErr)
	}
	return result, nil
}

func hermesBrowseError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), context.Cause(ctx))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("native browser work is unconfirmed; inspect the session before an explicit continuation")
}
