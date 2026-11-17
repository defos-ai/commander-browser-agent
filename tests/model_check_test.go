package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	hermesagent "github.com/defos-ai/commander-browser-agent/browser/hermes"
)

func TestModelCheckPreservesOpenAIProfileWithoutRequiringReplyText(t *testing.T) {
	const key = "fixture-model-check-openai-key"
	const model = "chat-latest"
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
		if err != nil || len(body) > 2<<20 || r.Method != http.MethodPost || r.URL.Path != "/chat/completions" ||
			r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer "+key || bytes.Contains(body, []byte(key)) {
			t.Error("model check escaped its selected completion endpoint or credential boundary")
			http.Error(w, "Invalid fixture request", http.StatusBadRequest)
			return
		}
		for name, values := range r.Header {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "openai-") || strings.HasPrefix(lower, "x-stainless-") {
				t.Error("model check forwarded SDK metadata")
			}
			for _, value := range values {
				if name != "Authorization" && strings.Contains(value, key) {
					t.Error("model check placed its credential outside the authentication header")
				}
			}
		}
		var request map[string]json.RawMessage
		if json.Unmarshal(body, &request) != nil {
			t.Error("model check sent malformed JSON")
			return
		}
		var selected string
		var allowance int
		_, legacy := request["max_tokens"]
		if json.Unmarshal(request["model"], &selected) != nil || selected != model || string(request["stream"]) != "false" ||
			legacy || json.Unmarshal(request["max_completion_tokens"], &allowance) != nil || allowance != 128 {
			t.Error("model check changed the selected model, streaming policy or OpenAI response allowance")
			http.Error(w, "Invalid fixture request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null},"finish_reason":"length"}]}`)
	}))
	t.Cleanup(provider.Close)
	configuration := hermesagent.ModelConfig{Provider: "openai", Model: model, Endpoint: provider.URL + "/chat/completions", APIKey: []byte(key)}
	defer clear(configuration.APIKey)
	if err := hermesagent.ValidateModel(t.Context(), configuration); err != nil || calls.Load() != 1 {
		t.Fatal("a valid bounded completion was not accepted as one model connection check")
	}
}

func TestModelCheckClassifiesProviderFailuresWithoutExposingDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		invalid bool
		failure error
	}{
		{name: "rejected", status: http.StatusBadRequest, failure: contracts.ErrModelRequestRejected},
		{name: "credentials", status: http.StatusUnauthorized, failure: contracts.ErrModelCredentials},
		{name: "rate_limit", status: http.StatusTooManyRequests, failure: contracts.ErrModelRateLimited},
		{name: "error_envelope", status: http.StatusOK, failure: contracts.ErrModelResponse},
		{name: "invalid_json", status: http.StatusOK, invalid: true, failure: contracts.ErrModelResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			const canary = "fixture-private-model-check-diagnostic"
			key := "fixture-model-check-key-" + test.name
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("model check failure request escaped its provider contract")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Provider-Diagnostic", canary)
				w.WriteHeader(test.status)
				if test.invalid {
					_, _ = io.WriteString(w, "invalid-json "+canary+" "+key)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": canary + " " + key}})
			}))
			t.Cleanup(provider.Close)
			configuration := hermesagent.ModelConfig{Provider: "openai", Model: "chat-latest", Endpoint: provider.URL + "/chat/completions", APIKey: []byte(key)}
			defer clear(configuration.APIKey)
			err := hermesagent.ValidateModel(t.Context(), configuration)
			if !errors.Is(err, test.failure) || calls.Load() != 1 {
				t.Fatal("model check lost its fixed failure category or retried the provider")
			}
			for _, private := range []string{canary, key, provider.URL, "X-Provider-Diagnostic", "invalid-json", "Model request unavailable"} {
				if strings.Contains(err.Error(), private) {
					t.Fatal("model check exposed provider diagnostics, routing or credentials")
				}
			}
		})
	}
}

func TestModelCheckCancellationStopsInFlightRequest(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Error("canceled model check retried its provider")
			http.Error(w, "Unexpected fixture request", http.StatusBadRequest)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error("cannot read the bounded cancellation fixture request")
		}
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	t.Cleanup(func() {
		provider.CloseClientConnections()
		provider.Close()
	})
	configuration := hermesagent.ModelConfig{Provider: "openai", Model: "chat-latest", Endpoint: provider.URL + "/chat/completions", APIKey: []byte("fixture-model-check-cancel-key")}
	defer clear(configuration.APIKey)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- hermesagent.ValidateModel(ctx, configuration) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("model check did not reach the in-flight fixture")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("model check did not preserve caller cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("model check did not join its canceled HTTP work")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("model check left its provider request active after cancellation")
	}
	if calls.Load() != 1 {
		t.Fatal("model check cancellation changed its single-request allowance")
	}
}
