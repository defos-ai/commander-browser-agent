//go:build browserintegration

package browser_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent/action"
)

func TestBrowseBlocksPopupDocumentsBeforeAndAfterGuardCleanup(t *testing.T) {
	for _, path := range []string{"/window", "/blank"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			options := browserSessionOptions(t)
			site, foreignHits, separateTargetHits := fixtures.NewBrowsePopupSite(t)
			const key = "fixture-popup-model-key-61d603"
			var selected atomic.Bool
			modelServer := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
				if strings.Contains(turn.Observation.Text, "foreign-popup-document-canary") {
					t.Error("foreign popup entered model context")
				}
				for _, item := range turn.Observation.Targets {
					if item.Label == "Open popup" {
						selected.Store(true)
						return intelligence.Command{Action: "click", Target: item.ID}
					}
				}
				t.Error("fixture popup target was not observed")
				return intelligence.Command{Action: "finish", Summary: "Target unavailable."}
			})
			model, err := intelligence.NewModel(intelligence.ModelConfig{URL: modelServer.URL, Name: "fixture-model"}, []byte(key))
			if err != nil {
				t.Fatal(err)
			}
			if closer, ok := model.(interface{ Close() error }); ok {
				defer closer.Close()
			}
			manager := newBrowserSessionManager(t)
			binding := newBrowserSessionBinding(manager.owner)
			if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
				t.Fatal(err)
			}
			var result intelligence.Result
			retained, err := manager.Run(action.WithSession(t.Context(), &binding), options, func(ctx context.Context) error {
				var runErr error
				result, runErr = intelligence.Run(ctx, intelligence.Request{Objective: "Open the fixture popup within website scope.", URL: site.URL + path, MaxSteps: 1, MaxCompletionTokens: 1024}, model, intelligence.Hooks{})
				return runErr
			})
			if err != nil {
				t.Fatal(err)
			}
			if !selected.Load() || modelServer.Calls() != 1 || result.Status != "incomplete" || len(result.Steps) != 1 || result.Steps[0].Outcome != "unconfirmed" {
				t.Fatalf("popup command did not stop safely: %+v", result)
			}
			assertBrowserSession(t, retained, binding.Reference, "connected")
			// Fetch.disable resumes pending requests. Traffic must remain blocked
			// after Run has stopped loading and removed its action-scoped guard.
			select {
			case <-time.After(200 * time.Millisecond):
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			if foreignHits.Load() != 0 || separateTargetHits.Load() != 0 {
				t.Fatal("popup request escaped its navigation guard or known target rejection")
			}
		})
	}
}
