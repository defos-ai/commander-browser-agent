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
	"strings"
	"sync/atomic"
	"testing"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/internal/commander"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander-browser-agent/tests/fakes"
	controllerevents "github.com/defos-ai/commander/framework/comms/clients/controller/events"
	"github.com/defos-ai/commander/framework/comms/messages"
)

const hermesProviderFailureCanary = "fixture-private-provider-error-detail"

func TestHermesModelFailuresStayClassifiedOutsideNativeHistory(t *testing.T) {
	worker := newHermesWorker(t)
	for _, test := range []struct {
		name      string
		status    int
		malformed bool
		failure   error
	}{
		{name: "credentials", status: http.StatusUnauthorized, failure: contracts.ErrModelCredentials},
		{name: "rejected_request", status: http.StatusBadRequest, failure: contracts.ErrModelRequestRejected},
		{name: "rate_limited", status: http.StatusTooManyRequests, failure: contracts.ErrModelRateLimited},
		{name: "malformed_response", status: http.StatusOK, malformed: true, failure: contracts.ErrModelResponse},
		{name: "error_envelope", status: http.StatusOK, failure: contracts.ErrModelResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := "fixture-model-failure-key-" + test.name
			provider, calls, _ := hermesFailingProvider(t, key, test.status, test.malformed)
			request := hermes.Request{Objective: "Complete the local model failure fixture.", Provider: "other", Model: "gpt-4.1-mini",
				Endpoint: provider.URL + "/chat/completions", APIKey: []byte(key), MaxIterations: 1, MaxTokens: 128, TimeoutSeconds: 25}
			defer clear(request.APIKey)
			var events []hermes.Event
			result, err := worker.Run(t.Context(), request, func(_ context.Context, event hermes.Event) (hermes.Reply, error) {
				events = append(events, event)
				if event.Kind != "progress" {
					t.Error("a failed model request reached browser admission or clarification")
				}
				return hermes.Reply{}, nil
			})
			if !errors.Is(err, test.failure) || result.Status != "failed" || result.Text != "" || calls.Load() != 1 {
				t.Fatal("native failure lost its safe classification, exposed final text or retried the provider")
			}
			encoded, encodeErr := json.Marshal(events)
			if encodeErr != nil {
				t.Fatal("cannot inspect bounded native failure events")
			}
			for _, private := range []string{key, hermesProviderFailureCanary, provider.URL, "Model request unavailable"} {
				if strings.Contains(err.Error(), private) || strings.Contains(result.Text, private) || bytes.Contains(encoded, []byte(private)) {
					t.Fatal("provider details or the native failure transcript crossed the result boundary")
				}
			}
		})
	}
}

func TestHermesBrowseModelFailureWaitsForFinalCheckpoint(t *testing.T) {
	for _, refuseFinal := range []bool{false, true} {
		t.Run(fmt.Sprintf("refuse_final=%t", refuseFinal), func(t *testing.T) {
			const key = "fixture-managed-model-failure-key"
			provider, calls, validations := hermesFailingProvider(t, key, http.StatusUnauthorized, false)
			endpoint := provider.URL + "/chat/completions"
			h := newDefaultHermesSessionHost(t)
			task := h.nativeBrowseTask(nil, commander.HermesBrowseInput{Objective: "Read the local failure fixture.", URL: "https://example.test/",
				MaxIterations: 1, MaxTokensPerResponse: 128, TimeoutSeconds: 30})
			binding := h.binding(task)
			command := h.dispatch(task, &binding)
			h.answerModelConfig(command, "other", "gpt-4.1-mini", endpoint)
			h.deliverModelSecret(command, modelSecretName(endpoint), key, 1)
			h.rememberModelConfig(command, "other", "gpt-4.1-mini", endpoint)
			final := h.checkpoint(controllerevents.CheckpointSave, "browse_finished")
			request, err := controllerevents.DecodeCheckpointRequest(final)
			hermesSessionCheck(t, err)
			var fact struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(request.Data, &fact) != nil || fact.Status != "failed" || calls.Load() != 1 || validations.Load() != 1 {
				t.Fatal("model failure finalized before its quiescent failed checkpoint or retried the provider")
			}
			for _, message := range h.history {
				if message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed) {
					t.Fatal("model failure was published before the final checkpoint receipt")
				}
			}
			want := contracts.ErrModelCredentials.Error() + "; inspect the session before an explicit continuation"
			if refuseFinal {
				h.refuseCheckpoint(final)
				want = "native browser work is unconfirmed; inspect the session before an explicit continuation"
			} else {
				h.replyCheckpoint(final, h.persistCheckpoint(final))
			}
			message := h.wait(func(message messages.Message) bool {
				return message.CorrelationID == command.ID && (message.Type == controllerevents.ActionSucceeded || message.Type == controllerevents.CommandFailed)
			})
			var failure controllerevents.CommandError
			if message.Type != controllerevents.CommandFailed || json.Unmarshal(message.Payload, &failure) != nil || failure.Message != want {
				t.Fatal("browse lost its fixed model error or hid a failed final checkpoint")
			}
			for _, private := range []string{key, hermesProviderFailureCanary, "Model request unavailable"} {
				if bytes.Contains(message.Payload, []byte(private)) {
					t.Fatal("the public browse failure exposed model credentials or provider diagnostics")
				}
			}
			h.pending = append([]messages.Message{message}, h.pending...)
			if _, ok := h.screenshotResult(command); ok {
				t.Fatal("failed model work published successful output or artifacts")
			}
			if !h.control(binding, "close") || h.snapshot(binding.Reference).State != "closed" {
				t.Fatal("model failure prevented confirmed session cleanup")
			}
		})
	}
}

func hermesFailingProvider(t *testing.T, key string, status int, malformed bool) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	calls, validations := new(atomic.Int32), new(atomic.Int32)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
		defer clear(body)
		if err != nil || len(body) > 2<<20 || r.Method != http.MethodPost || r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("model failure fixture received an unexpected provider request")
		}
		w.Header().Set("Content-Type", "application/json")
		var request fakes.HermesRequest
		if json.Unmarshal(body, &request) == nil && request.IsModelValidation() {
			validations.Add(1)
			_, _ = io.WriteString(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			return
		}
		calls.Add(1)
		w.Header().Set("X-Provider-Private", hermesProviderFailureCanary)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(status)
		if malformed {
			_, _ = io.WriteString(w, `{"private":"`+hermesProviderFailureCanary)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": hermesProviderFailureCanary + " " + key}})
	}))
	t.Cleanup(provider.Close)
	return provider, calls, validations
}
