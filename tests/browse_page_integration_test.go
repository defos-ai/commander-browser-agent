//go:build browserintegration

package browser_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

func TestBrowseFollowsSameHostLinkAndExtractsAfterNavigation(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	const key = "fixture-navigation-model-key-3b3dcb2e"
	server := fixtures.NewBrowseModel(t, key, func(turn intelligence.Turn) intelligence.Command {
		if len(turn.Evidence) > 0 {
			return intelligence.Command{Action: "finish", Summary: "Found the study sample count.", Findings: []intelligence.Finding{{Text: "The study measured 42 samples.", EvidenceIDs: []string{turn.Evidence[0].ID}}}}
		}
		if turn.Observation.URL == site.URL+"/details" && strings.Contains(turn.Observation.Text, "42 samples") {
			return intelligence.Command{Action: "extract"}
		}
		for _, target := range turn.Observation.Targets {
			if target.Label == "Read study details" {
				return intelligence.Command{Action: "click", Target: target.ID}
			}
		}
		t.Error("fixture model did not observe the expected navigation state")
		return intelligence.Command{Action: "finish", Summary: "The expected study page was unavailable."}
	})
	model := pageIntegrationModel(t, server, key)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := manager.Prepare(ctx, binding.Claim()); err != nil {
		t.Fatal(err)
	}
	var result intelligence.Result
	snapshot, err := manager.Run(action.WithSession(ctx, &binding), options, func(ctx context.Context) error {
		var err error
		result, err = intelligence.Run(ctx, intelligence.Request{Objective: "Follow the study link and extract its sample count.", URL: site.URL, MaxSteps: 5, MaxCompletionTokens: 5120}, model, intelligence.Hooks{})
		return err
	})
	if err != nil {
		t.Fatalf("browse through a same-host document navigation: %v", err)
	}
	assertBrowserSession(t, snapshot, binding.Reference, "connected")
	if result.Status != "completed" || result.FinalURL != site.URL+"/details" || len(result.Findings) != 1 || len(result.Evidence) != 1 || !strings.Contains(result.Evidence[0].Text, "42 samples") {
		t.Fatalf("navigation did not yield grounded findings on the destination page: %+v", result)
	}
	if server.Calls() != 3 || result.Usage.ModelCalls != 3 || len(result.Steps) != 3 || result.Steps[0].Action != "click" || result.Steps[1].Action != "extract" || result.Steps[2].Action != "finish" {
		t.Fatalf("unexpected navigation workflow: calls=%d steps=%+v", server.Calls(), result.Steps)
	}
}

func TestBrowseCancellationDuringModelRequestReleasesGuardAndSession(t *testing.T) {
	options := browserSessionOptions(t)
	site, foreignURL, foreignHits, _ := fixtures.NewBrowseScopeSite(t)
	const key = "fixture-cancellation-model-key-4e532fd1"
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var once sync.Once
	server := fixtures.NewBrowseModel(t, key, func(intelligence.Turn) intelligence.Command {
		once.Do(func() { close(entered) })
		<-release
		return intelligence.Command{Action: "wait"}
	})
	model := pageIntegrationModel(t, server, key)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	if err := manager.Prepare(t.Context(), binding.Claim()); err != nil {
		t.Fatal(err)
	}
	work, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		snapshot sessions.Session
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		snapshot, err := manager.Run(action.WithSession(work, &binding), options, func(ctx context.Context) error {
			_, err := intelligence.Run(ctx, intelligence.Request{Objective: "Inspect the current page.", URL: site.URL, MaxSteps: 5, MaxCompletionTokens: 5120}, model, intelligence.Hooks{})
			return err
		})
		done <- outcome{snapshot: snapshot, err: err}
	}()
	awaitBrowserSignal(t, entered)
	cancel()
	select {
	case stopped := <-done:
		if !errors.Is(stopped.err, context.Canceled) {
			t.Fatalf("cancelled model request returned %v", stopped.err)
		}
		assertBrowserSession(t, stopped.snapshot, binding.Reference, "connected")
	case <-time.After(5 * time.Second):
		t.Fatal("browser work did not stop promptly while its model response was pending")
	}
	if server.Calls() != 1 {
		t.Fatalf("cancellation unexpectedly replayed model work: %d calls", server.Calls())
	}
	// The retained browser must permit later work beyond the finished action's
	// host scope. Otherwise the cancelled action left its Fetch guard installed.
	binding.Mode = sessions.Reuse
	authorized, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	var text string
	snapshot, err := manager.Run(action.WithSession(authorized, &binding), options, func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Navigate(foreignURL+"/authorized-after-cancellation"), chromedp.Evaluate(`document.body.textContent`, &text))
	})
	if err != nil {
		t.Fatalf("cancelled browse left its session or navigation guard unusable: %v", err)
	}
	assertBrowserSession(t, snapshot, binding.Reference, "connected")
	if foreignHits.Load() == 0 || !strings.Contains(text, "foreign-document-canary") {
		t.Fatal("later authorized navigation did not reach its destination")
	}
}

func TestBrowseCompletionBudgetStopsAfterOneModelCallAndReportsFinalURL(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	const key = "fixture-budget-model-key-84b88ac2"
	server := fixtures.NewBrowseModel(t, key, func(intelligence.Turn) intelligence.Command {
		return intelligence.Command{Action: "navigate", URL: site.URL + "/details"}
	})
	model := pageIntegrationModel(t, server, key)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := manager.Prepare(ctx, binding.Claim()); err != nil {
		t.Fatal(err)
	}
	var result intelligence.Result
	_, err := manager.Run(action.WithSession(ctx, &binding), options, func(ctx context.Context) error {
		var err error
		result, err = intelligence.Run(ctx, intelligence.Request{Objective: "Open the study page within a strict completion allowance.", URL: site.URL, MaxSteps: 30, MaxCompletionTokens: 1024}, model, intelligence.Hooks{})
		return err
	})
	if err != nil {
		t.Fatalf("bounded model-directed navigation: %v", err)
	}
	if result.Status != "budget_exhausted" || result.Usage.ModelCalls != 1 || server.Calls() != 1 || result.Usage.CompletionTokensReserved != 1024 || result.Usage.Steps != 1 {
		t.Fatalf("completion budget did not stop after one reserved model call: result=%+v calls=%d", result, server.Calls())
	}
	if result.FinalURL != site.URL+"/details" {
		t.Fatalf("budget exhaustion reported the previous page URL: %q", result.FinalURL)
	}
}

func TestBrowseRejectsCompletionWithoutExtractedEvidence(t *testing.T) {
	options := browserSessionOptions(t)
	site := fixtures.NewBrowseNavigationSite(t)
	const key = "fixture-unsupported-finish-key-1e4bb229"
	server := fixtures.NewBrowseModel(t, key, func(intelligence.Turn) intelligence.Command {
		return intelligence.Command{Action: "finish", Summary: "The requested research is complete."}
	})
	model := pageIntegrationModel(t, server, key)
	manager := newBrowserSessionManager(t)
	binding := newBrowserSessionBinding(manager.owner)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := manager.Prepare(ctx, binding.Claim()); err != nil {
		t.Fatal(err)
	}
	var result intelligence.Result
	_, err := manager.Run(action.WithSession(ctx, &binding), options, func(ctx context.Context) error {
		var err error
		result, err = intelligence.Run(ctx, intelligence.Request{Objective: "Research the study sample count.", URL: site.URL, MaxSteps: 5, MaxCompletionTokens: 5120}, model, intelligence.Hooks{})
		return err
	})
	if result.Status != "incomplete" || len(result.Evidence) != 0 {
		t.Fatalf("model declared unsupported completion without evidence: status=%s evidence=%d error=%v", result.Status, len(result.Evidence), err)
	}
	if server.Calls() != 1 {
		t.Fatalf("invalid completion was unexpectedly retried: %d model calls", server.Calls())
	}
}

func pageIntegrationModel(t *testing.T, server *fixtures.BrowseModel, key string) intelligence.Model {
	t.Helper()
	credential := []byte(key)
	model, err := intelligence.NewModel(intelligence.ModelConfig{URL: server.URL, Name: "fixture-model"}, credential)
	if err != nil {
		clear(credential)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closer, ok := model.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				t.Errorf("close fixture model client: %v", err)
			}
		}
		clear(credential)
	})
	return model
}
