//go:build hermesintegration

package browser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/internal/browsing"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	"github.com/defos-ai/commander/framework/agent/action"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	actionmodel "github.com/defos-ai/commander/framework/domain/actions"
	"github.com/defos-ai/commander/framework/domain/operations"
	"github.com/defos-ai/commander/framework/id"
	runtimeSessions "github.com/defos-ai/commander/framework/sessions"
)

// This test-owned adapter exercises the shared native browse composition
// directly. The managed workflow separately uses the default action catalog.
type hermesBrowseFixture struct {
	*action.Base
	host     *hermesSessionHost
	returned chan action.Result
}

func newHermesBrowseFixture(h *hermesSessionHost) action.Executor {
	return &hermesBrowseFixture{Base: action.NewBase(actionmodel.Action{
		Name: "native-browse", Description: "Exercise the native browse contract.", SessionMode: "open",
		InputSchema: json.RawMessage(browsing.HermesBrowseInputSchema), OutputSchema: json.RawMessage(browsing.HermesBrowseOutputSchema),
	}), host: h}
}

func (fixture *hermesBrowseFixture) Run(ctx context.Context, raw json.RawMessage) (action.Result, error) {
	result, err := fixture.host.setup.RunHermesBrowse(ctx, fixture.host.manager, fixture.host.options, raw)
	if fixture.returned != nil {
		fixture.returned <- result
	}
	return result, err
}

func (h *hermesSessionHost) nativeBrowseTask(parent *operations.Task, input browsing.HermesBrowseInput) operations.Task {
	h.t.Helper()
	raw, err := json.Marshal(input)
	hermesSessionCheck(h.t, err)
	name := "native-browse"
	if h.production {
		name = "browse"
	}
	next, err := runtimeSessions.Normalize(parent, []actionmodel.NextAction{{ActionName: name, Input: raw}}, nil, h.definitions...)
	hermesSessionCheck(h.t, err)
	operation, parentID := id.OperationID(), ""
	if parent != nil {
		operation, parentID = parent.OperationID, parent.ID
	}
	return operations.NewTask(operation, parentID, next[0])
}

func TestHermesBrowseNativeContractDispatchReuseAndWallTime(t *testing.T) {
	key := "fixture-native-browse-key-" + id.New()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<title>Study</title><p>The study measured 42 samples.</p><p>Provider echo "+key+"</p>")
	}))
	t.Cleanup(site.Close)
	model := fakes.NewHermesModel(t, key, func(request fakes.HermesRequest, call int) fakes.HermesResponse {
		switch call {
		case 1:
			startingURL := false
			for _, message := range request.Messages {
				startingURL = startingURL || message.Role == "user" && bytes.Contains(message.Content, []byte(site.URL))
			}
			if !startingURL {
				t.Error("the native objective lost its validated starting URL")
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("browse-open", "browser_exec", map[string]any{
				"code": fmt.Sprintf("# Read the local study\nnew_tab(%q)\nprint(js('document.body.textContent'))", site.URL),
			})}}
		case 2:
			if !request.HasToolText("42 samples") || !request.HasToolText("[REDACTED]") {
				t.Error("native browsing lost the protected page observation")
			}
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("browse-clarify", "clarify", map[string]any{
				"questions": []any{map[string]any{"question": "Which figure should be reported?", "choices": []string{"42 samples", "Page title"}}},
			})}}
		case 3:
			if !request.HasToolText("42 samples") {
				t.Error("the Commander clarification answer did not reach Hermes")
			}
			return fakes.HermesResponse{Text: "The study measured 42 samples. Provider echo " + key}
		case 4:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("browse-reuse", "browser_exec", map[string]any{
				"code": "# Inspect the retained study\nprint(js('document.body.textContent'))",
			})}}
		case 5:
			if !request.HasToolText("42 samples") {
				t.Error("a native continuation lost its original browser page")
			}
			return fakes.HermesResponse{Text: "The retained study still reports 42 samples."}
		case 6:
			return fakes.HermesResponse{Tools: []fakes.HermesToolCall{fakes.HermesCall("browse-wait", "clarify", map[string]any{
				"questions": []any{map[string]any{"question": "Which study should be read next?", "choices": []string{"Current study", "Another study"}}},
			})}}
		default:
			t.Error("native browsing continued after an unanswered clarification expired")
			return fakes.HermesResponse{Text: "Unexpected fixture continuation."}
		}
	})
	endpoint := model.URL + "/chat/completions"
	secretName := fmt.Sprintf("browser-model-api-key-%x", sha256.Sum256([]byte(endpoint)))
	returned := make(chan action.Result, 3)
	h := newHermesSessionHost(t, nil, func(h *hermesSessionHost) action.Executor {
		fixture := newHermesBrowseFixture(h).(*hermesBrowseFixture)
		fixture.returned = returned
		return fixture
	})
	input := browsing.HermesBrowseInput{Objective: "Read the study and report its sample count.", URL: site.URL, MaxIterations: 4, MaxTokensPerResponse: 512, TimeoutSeconds: 30}
	root := h.nativeBrowseTask(nil, input)
	binding := h.binding(root)
	command := h.dispatch(root, &binding)
	h.answerSetup(command, "browser_model_provider", "Other (OpenAI-compatible)")
	h.answerSetup(command, "browser_model_name", "gpt-4.1-mini")
	h.answerSetup(command, "browser_model_url", endpoint)
	if model.Calls() != 0 {
		t.Fatal("native browsing contacted a model before managed key delivery")
	}
	lease := h.deliverModelKey(command, secretName, key, 1)
	h.answerSetup(command, "browser_clarification_1", "42 samples")
	final := h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	if model.Calls() != 3 {
		t.Fatal("native browse finalized before the scripted reasoning returned")
	}
	h.replyCheckpoint(final, h.persistCheckpoint(final))
	first, ok := h.screenshotResult(command)
	<-returned
	var output browsing.HermesBrowseOutput
	if !ok || json.Unmarshal(first.Output, &output) != nil || output.Validate() != nil || output.Status != "completed" || output.ModelRequests != 3 ||
		!strings.Contains(output.Text, "42 samples") || !strings.Contains(output.Text, "[REDACTED]") || bytes.Contains(first.Output, []byte(key)) ||
		first.Session == nil || first.Session.Reference != binding.Reference || len(first.Artifacts) != 1 || len(first.Artifacts[0].Files) != 1 ||
		!bytes.Equal(first.Output, h.screenshotDownload(first, "browse.json")) {
		t.Fatal("native browse lost its protected final-text contract, session or uploaded artifact")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(first.Output, &fields) != nil || len(fields) != 5 || fields["text"] == nil || fields["model_requests"] == nil {
		t.Fatal("native browse invented typed findings/evidence or omitted its reported counts")
	}
	assertHermesCheckpointFacts(t, h, command, binding, key, site.URL, input.Objective, "Which figure should be reported?", "42 samples")
	if h.snapshot(binding.Reference).State != "connected" {
		t.Fatal("native browsing did not retain its verified browser session")
	}
	// Reuse still resolves a current managed delivery. The shared non-secret
	// setup survives, and the native worker retains its original page.
	waitHermesBrowseLease(t, lease)
	child := h.nativeBrowseTask(&root, input)
	childBinding := h.binding(child)
	command = h.dispatch(child, &childBinding)
	lease = h.deliverModelKey(command, secretName, key, 2)
	final = h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
	h.refuseCheckpoint(final)
	if _, ok := h.screenshotResult(command); ok || model.Calls() != 5 || childBinding.Claim() != binding.Claim() {
		t.Fatal("native continuation repeated setup or accepted an unconfirmed final receipt")
	}
	suppressed := <-returned
	if len(suppressed.Output) != 0 || len(suppressed.Artifacts) != 0 || suppressed.Session == nil || suppressed.Session.Reference != binding.Reference ||
		h.snapshot(binding.Reference).State != "connected" {
		t.Fatal("failed native final persistence leaked prepared output or lost its exact live session")
	}
	// Warm reuse removes interpreter/browser startup variability. Leaving the
	// next clarification unanswered must consume the requested wall allowance.
	waitHermesBrowseLease(t, lease)
	input.TimeoutSeconds = 5
	timed := h.nativeBrowseTask(&child, input)
	timedBinding := h.binding(timed)
	command = h.dispatch(timed, &timedBinding)
	started := time.Now()
	h.deliverModelKey(command, secretName, key, 3)
	question := h.setupMessage(command)
	request, err := controllerevents.DecodeQuestionRequest(question)
	if err != nil || request.Key != "browser_clarification_1" || request.CommandID != command.ID || model.Calls() != 6 {
		t.Fatal("the warm native browse did not reach its bounded unanswered clarification")
	}
	if _, ok := h.screenshotResult(command); ok || time.Since(started) > 13*time.Second || model.Calls() != 6 {
		t.Fatal("native clarification paused its wall allowance or returned successful prepared output")
	}
	if _, exists := h.checkpoints[command.ID].Checkpoints["browse_finished"]; exists {
		t.Fatal("expired native work published a final fact outside its live execution")
	}
	assertHermesCheckpointFacts(t, h, command, timedBinding, key, site.URL, input.Objective, request.Text)
	if !h.control(binding, "close") || h.snapshot(binding.Reference).State != "closed" {
		t.Fatal("the retained native browser could not be closed after clarification timeout")
	}
}

func waitHermesBrowseLease(t *testing.T, lease time.Time) {
	t.Helper()
	wait := time.NewTimer(max(time.Until(lease), 0) + 100*time.Millisecond)
	defer wait.Stop()
	select {
	case <-wait.C:
	case <-t.Context().Done():
		t.Fatal("native browse managed lease wait was cancelled")
	}
}
