//go:build hermesintegration

package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

func hermesConfig() hermes.Config {
	session := id.New()
	return hermes.Config{
		Python: "/opt/hermes/.venv/bin/python", Source: "/opt/hermes", Lightpanda: "/usr/local/bin/lightpanda",
		Claim: sessions.CreationClaim{
			Reference: sessions.Reference{ID: session, ScopeID: session, OwnerAgentID: "browser-agent-0123abcd"},
			CommandID: id.New(), OperationID: id.OperationID(), TaskID: id.TaskID(),
		},
	}
}

func newHermesWorker(t *testing.T) *hermes.Worker {
	t.Helper()
	worker, err := hermes.Start(t.Context(), hermesConfig())
	if err != nil {
		t.Fatal("the packaged Hermes worker did not become ready")
	}
	t.Cleanup(func() { closeHermesWorker(t, worker) })
	return worker
}

func closeHermesWorker(t *testing.T, worker *hermes.Worker) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- worker.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Error("Hermes worker cleanup was not confirmed")
		}
	case <-time.After(12 * time.Second):
		t.Error("Hermes worker cleanup did not join within its bound")
	}
}

func hermesRequest(model *fakes.HermesModel, key []byte) hermes.Request {
	return hermes.Request{
		Objective: "Complete the local fixture objective.", Provider: "openai", Model: "gpt-4.1-mini",
		BaseURL: model.URL, APIKey: key, MaxIterations: 4, MaxTokens: 512, TimeoutSeconds: 25,
	}
}

func hermesEventPrivate(t *testing.T, event hermes.Event, key string) {
	t.Helper()
	encoded, _ := json.Marshal(event)
	if bytes.Contains(encoded, []byte(key)) || bytes.Contains(encoded, []byte("new_tab(")) || bytes.Contains(encoded, []byte("goto_url(")) {
		t.Error("private model credentials or native tool code escaped through a callback")
	}
	if event.Kind != "question" && len(event.Options) != 0 || event.Kind == "admit" && event.Text != "" ||
		event.Kind == "progress" && event.Text != "started" && event.Text != "finished" {
		t.Error("a metadata callback contained free-form tool or runtime text")
	}
	switch event.Kind {
	case "admit", "progress", "question":
	default:
		t.Error("the worker published an event outside its private protocol")
	}
}

func hermesResultPrivate(t *testing.T, result hermes.Result, key, site string) {
	t.Helper()
	encoded, _ := json.Marshal(result)
	for _, forbidden := range []string{key, site, "new_tab(", "goto_url(", "tool_calls", "messages", "last_reasoning"} {
		if forbidden != "" && bytes.Contains(encoded, []byte(forbidden)) {
			t.Error("private runtime data escaped through the final result")
		}
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	for name := range fields {
		if name != "status" && name != "text" && name != "model_requests" {
			t.Error("the result grew beyond its final-text/status/count allowlist")
		}
	}
}

func TestHermesNativeBrowserRetainsSessionBetweenRuns(t *testing.T) {
	var sets, reads, retained atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/set":
			sets.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "fixture-session", Value: "retained", Path: "/", HttpOnly: true})
			_, _ = io.WriteString(w, "<title>Fixture session</title><p>Fixture session established</p>")
		case "/read":
			reads.Add(1)
			cookie, err := r.Cookie("fixture-session")
			if err == nil && cookie.Value == "retained" {
				retained.Add(1)
				_, _ = io.WriteString(w, "<p>Retained cookie confirmed</p>")
				return
			}
			_, _ = io.WriteString(w, "<p>Fixture cookie missing</p>")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(site.Close)
	firstKey := "fixture-hermes-first-" + id.New()
	firstModel := fakes.NewHermesModel(t, firstKey, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		switch call {
		case 1:
			if !request.HasTool("browser_exec") || !request.HasTool("clarify") {
				t.Error("Hermes did not advertise its native browser and clarification tools")
			}
			code := fmt.Sprintf("# Open the fixture page\nnew_tab(%q)\nprint(js(\"document.body.textContent\"))", site.URL+"/set")
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("open-fixture", "browser_exec", map[string]any{"code": code})}}
		case 2:
			if !request.HasToolText("Fixture session established") {
				t.Error("native browser observations did not reach the real Hermes loop")
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("clarify-fixture", "clarify", map[string]any{
				"questions": []any{map[string]any{"question": "Which fixture detail should be confirmed?", "choices": []string{"Cookie continuity", "Page title"}}},
			})}}
		case 3:
			if !request.HasToolText("Cookie continuity") {
				t.Error("the host clarification answer did not reach the next model request")
			}
			return fakes.HermesResponse{Text: "First fixture run completed."}
		default:
			t.Error("Hermes exceeded the scripted successful workflow")
			return fakes.HermesResponse{Text: "Fixture stopped."}
		}
	})
	worker := newHermesWorker(t)
	var admissions, questions, progress atomic.Int32
	handler := func(_ context.Context, event hermes.Event) (hermes.Reply, error) {
		hermesEventPrivate(t, event, firstKey)
		switch event.Kind {
		case "admit":
			if event.Tool != "browser_exec" || event.Sequence < 1 || sets.Load() != 0 {
				t.Error("native navigation occurred before its host admission")
			}
			admissions.Add(1)
			return hermes.Reply{Approved: true}, nil
		case "question":
			questions.Add(1)
			if event.Text != "Which fixture detail should be confirmed?" || len(event.Options) != 2 {
				t.Error("the clarification contract changed")
			}
			return hermes.Reply{Answer: "Cookie continuity"}, nil
		case "progress":
			progress.Add(1)
		}
		return hermes.Reply{}, nil
	}
	keyBytes := []byte(firstKey)
	first, err := worker.Run(t.Context(), hermesRequest(firstModel, keyBytes), handler)
	clear(keyBytes)
	if err != nil || first.Status != "completed" || first.Text != "First fixture run completed." {
		t.Fatal("Hermes did not complete its native browser and clarification workflow")
	}
	hermesResultPrivate(t, first, firstKey, site.URL)
	if first.ModelRequests != 3 || firstModel.Calls() != 3 || admissions.Load() != 1 || questions.Load() != 1 || progress.Load() == 0 || sets.Load() != 1 {
		t.Fatal("the workflow lost its bounded model/tool/callback ordering")
	}

	// A fresh provider client/key is supplied while the worker retains its
	// existing browser. No model transcript is passed into this second Run.
	secondKey := "fixture-hermes-second-" + id.New()
	secondModel := fakes.NewHermesModel(t, secondKey, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		if call == 1 {
			code := fmt.Sprintf("# Read the retained fixture session\ngoto_url(%q)\nprint(js(\"document.body.textContent\"))", site.URL+"/read")
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("read-retained", "browser_exec", map[string]any{"code": code})}}
		}
		if call != 2 || !request.HasToolText("Retained cookie confirmed") {
			t.Error("the second native run did not observe retained browser cookies")
		}
		return fakes.HermesResponse{Text: "Retained cookie confirmed."}
	})
	second, err := worker.Run(t.Context(), hermesRequest(secondModel, []byte(secondKey)), func(_ context.Context, event hermes.Event) (hermes.Reply, error) {
		hermesEventPrivate(t, event, secondKey)
		if event.Kind == "admit" {
			if reads.Load() != 0 {
				t.Error("the retained browser navigated before the second admission")
			}
			admissions.Add(1)
			return hermes.Reply{Approved: true}, nil
		}
		return hermes.Reply{}, nil
	})
	if err != nil || second.Status != "completed" || second.Text != "Retained cookie confirmed." || second.ModelRequests != 2 || secondModel.Calls() != 2 || firstModel.Calls() != 3 {
		t.Fatal("a fresh model client did not complete work on the retained browser")
	}
	hermesResultPrivate(t, second, secondKey, site.URL)
	if retained.Load() != 1 || reads.Load() != 1 || sets.Load() != 1 || admissions.Load() != 2 {
		t.Fatal("browser state or host admission was not retained correctly")
	}
	closeHermesWorker(t, worker)
	if _, err := worker.Run(t.Context(), hermesRequest(secondModel, []byte(secondKey)), handler); !errors.Is(err, hermes.ErrClosed) || secondModel.Calls() != 2 {
		t.Fatal("a closed worker accepted another execution")
	}
}

func TestHermesAdmissionRefusesEffectsAndTerminal(t *testing.T) {
	config := hermesConfig()
	config.Python = "/missing-hermes-python"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if worker, err := hermes.Start(ctx, config); worker != nil || !errors.Is(err, hermes.ErrUnavailable) {
		t.Fatal("invalid runtime startup was not rejected with a readiness error")
	}
	var effects atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		effects.Add(1)
		_, _ = io.WriteString(w, "Unexpected native effect")
	}))
	t.Cleanup(site.Close)
	for _, name := range []string{"refused", "receipt_error"} {
		t.Run(name, func(t *testing.T) {
			key := "fixture-hermes-refusal-" + id.New()
			model := fakes.NewHermesModel(t, key, func(_ fakes.HermesRequest, call int) fakes.HermesResponse {
				if call == 1 {
					code := fmt.Sprintf("# Attempt the guarded fixture effect\nnew_tab(%q)\nprint(page_info())", site.URL)
					// Different call IDs cannot evade an execution-wide refusal,
					// even when one model response contains another attempt.
					return fakes.HermesResponse{Tools: []fakes.HermesToolCall{
						fakes.HermesCall("first-effect", "browser_exec", map[string]any{"code": code}),
						fakes.HermesCall("retry-effect", "browser_exec", map[string]any{"code": code}),
					}}
				}
				return fakes.HermesResponse{Text: "The fixture effect was refused."}
			})
			worker := newHermesWorker(t)
			var admissions atomic.Int32
			result, err := worker.Run(t.Context(), hermesRequest(model, []byte(key)), func(_ context.Context, event hermes.Event) (hermes.Reply, error) {
				hermesEventPrivate(t, event, key)
				if event.Kind == "admit" {
					admissions.Add(1)
					if name == "receipt_error" {
						return hermes.Reply{}, errors.New("synthetic checkpoint receipt unavailable")
					}
				}
				return hermes.Reply{}, nil
			})
			if err == nil && result.Status != "failed" {
				t.Error("host refusal was reported as completed browser work")
			}
			if admissions.Load() != 1 || effects.Load() != 0 || model.Calls() > 5 {
				t.Fatal("refusal failed to fence subsequent native effects")
			}
			hermesResultPrivate(t, result, key, site.URL)
		})
	}
	t.Run("terminal_is_denied", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "unexpected-terminal-effect")
		key := "fixture-hermes-terminal-" + id.New()
		model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
			if call == 1 {
				if !request.HasTool("terminal") {
					t.Error("the native browser's terminal prerequisite was silently bypassed")
				}
				return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("denied-terminal", "terminal", map[string]any{"command": "touch " + strconv.Quote(marker)})}}
			}
			return fakes.HermesResponse{Text: "Terminal execution is unavailable."}
		})
		worker := newHermesWorker(t)
		var admissions atomic.Int32
		result, _ := worker.Run(t.Context(), hermesRequest(model, []byte(key)), func(_ context.Context, event hermes.Event) (hermes.Reply, error) {
			hermesEventPrivate(t, event, key)
			if event.Kind == "admit" {
				admissions.Add(1)
			}
			return hermes.Reply{Approved: true}, nil
		})
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) || admissions.Load() != 0 || effects.Load() != 0 || model.Calls() > 5 {
			t.Fatal("the terminal helper performed a native host effect")
		}
		hermesResultPrivate(t, result, key, site.URL)
	})
}

func TestHermesCancellationJoinsPrivateWorker(t *testing.T) {
	for _, mode := range []string{"model_http", "question"} {
		t.Run(mode, func(t *testing.T) {
			key := "fixture-hermes-cancel-" + id.New()
			model := fakes.NewHermesModel(t, key, func(_ fakes.HermesRequest, _ int) fakes.HermesResponse {
				if mode == "model_http" {
					return fakes.HermesResponse{Block: true}
				}
				return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("pending-question", "clarify", map[string]any{
					"questions": []any{map[string]any{"question": "Which fixture detail should be used?", "choices": []string{"First", "Second"}}},
				})}}
			})
			worker := newHermesWorker(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			question := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				_, err := worker.Run(ctx, hermesRequest(model, []byte(key)), func(ctx context.Context, event hermes.Event) (hermes.Reply, error) {
					hermesEventPrivate(t, event, key)
					if event.Kind == "question" {
						select {
						case question <- struct{}{}:
						default:
						}
						<-ctx.Done()
						return hermes.Reply{}, ctx.Err()
					}
					return hermes.Reply{}, nil
				})
				done <- err
			}()
			waiting := model.Blocked
			if mode == "question" {
				waiting = question
			}
			select {
			case <-waiting:
			case <-time.After(20 * time.Second):
				t.Fatal("the actual Hermes loop did not enter its cancellable wait")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("worker cancellation lost the caller's context cause")
				}
			case <-time.After(12 * time.Second):
				t.Fatal("cancellation did not join the private runtime worker")
			}
			closeHermesWorker(t, worker)
			if mode == "model_http" {
				select {
				case <-model.Disconnected:
				case <-time.After(2 * time.Second):
					t.Error("the model provider request remained active after confirmed cleanup")
				}
			}
			if model.Calls() != 1 {
				t.Error("cancellation issued another model request")
			}
			if _, err := worker.Run(t.Context(), hermesRequest(model, []byte(key)), nil); !errors.Is(err, hermes.ErrClosed) || model.Calls() != 1 {
				t.Error("a cancelled and closed worker accepted additional work")
			}
		})
	}
}
