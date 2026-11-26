//go:build hermesintegration

package browser_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/commander"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander-browser-agent/tests/fixtures"
	"github.com/defos-ai/commander/framework/domain/sessions"
	"github.com/defos-ai/commander/framework/id"
)

// An independent verification may wait for a synchronous page request, but the
// worker must still receive close and join that verification before confirming it.
func TestHermesSessionCloseInterruptsSynchronousConnectionVerification(t *testing.T) {
	site := fixtures.NewSynchronous(t, 0)
	key := "fixture-synchronous-control-key-" + id.New()
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		switch call {
		case 1:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("ready-synchronous-page", "browser_exec", map[string]any{
				"code": fmt.Sprintf("cdp('Page.navigate', url=%q)\nprint(js('document.body.textContent'))", site.URL+"/page"),
			})}}
		case 2:
			if !request.HasToolText("37 samples") {
				t.Error("the connection-control fixture did not observe its retained page")
			}
			return fakes.HermesResponse{Text: "The retained page reports 37 samples."}
		default:
			t.Error("connection verification or close restarted native reasoning")
			return fakes.HermesResponse{Text: "Unexpected connection-control continuation."}
		}
	})
	endpoint := model.URL + "/chat/completions"
	h := newDefaultHermesSessionHost(t)
	defer site.Release()
	input := commander.HermesBrowseInput{Objective: "Read the fixture and retain its page.", URL: site.URL + "/page",
		MaxIterations: 2, MaxTokensPerResponse: 512, TimeoutSeconds: 35}
	task := h.nativeBrowseTask(nil, input)
	binding := h.binding(task)
	command := h.dispatch(task, &binding)
	h.answerModelConfig(command, "other", "gpt-4.1-mini", endpoint)
	h.deliverModelSecret(command, modelSecretName(endpoint), key, 1)
	h.rememberModelConfig(command, "other", "gpt-4.1-mini", endpoint)
	output, ok := h.screenshotResult(command)
	var result commander.HermesBrowseOutput
	if !ok || json.Unmarshal(output.Output, &result) != nil || result.Status != "completed" ||
		model.Calls() != 2 || h.snapshot(binding.Reference).State != "connected" {
		t.Fatal("connection-control fixture did not establish its healthy retained browser")
	}

	use := binding
	use.Mode = sessions.Use
	hermesScreenshotEvaluate(t, h, use, "blockSoon(300);true")
	select {
	case <-site.Started:
	case <-time.After(3 * time.Second):
		t.Fatal("the retained page did not begin its held synchronous request")
	}
	type verificationResult struct {
		session sessions.Session
		err     error
	}
	verified := make(chan verificationResult, 1)
	verifyCtx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	go func() {
		view, err := h.manager.Control(verifyCtx, sessions.Control{Reference: binding.Reference, Method: "verify", RequestID: id.New(),
			CommandID: binding.CommandID, OperationID: binding.OperationID, TaskID: binding.TaskID})
		verified <- verificationResult{session: view, err: err}
	}()
	// Keep the bus helper on this goroutine. Direct manager verification avoids
	// racing its pending-message bookkeeping with the close response consumer.
	select {
	case <-verified:
		t.Fatal("the held synchronous page request did not keep verification pending")
	case <-time.After(100 * time.Millisecond):
	}
	started := time.Now()
	if !h.control(binding, "close") || h.snapshot(binding.Reference).State != "closed" {
		t.Fatal("close could not confirm cleanup while connection verification was pending")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("connection verification blocked the worker from promptly handling close")
	}
	select {
	case result := <-verified:
		if result.err == nil || result.session.State == "lost" {
			t.Fatal("pending verification accepted stale health or declared intentional close lost")
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed close did not join its pending connection verification")
	}
	if model.Calls() != 2 || site.Visits.Load() != 1 || site.HeldRequests.Load() != 1 {
		t.Fatal("connection verification or close replayed native reasoning or navigation")
	}
}
