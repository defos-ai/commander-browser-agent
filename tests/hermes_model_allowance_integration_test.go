//go:build hermesintegration

package browser_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/defos-ai/commander-browser-agent/internal/hermes"
)

// A truncated native tool call makes pinned Hermes increase the next response
// allowance. The host must cap that recovery request before provider contact.
func TestHermesModelAllowanceBoundsNativeRecovery(t *testing.T) {
	worker := newHermesWorker(t)
	for _, model := range []string{"gpt-4.1-mini", "gpt-5-mini"} {
		t.Run(model, func(t *testing.T) {
			const key = "fixture-native-allowance-key"
			const allowance = 37
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
				if err != nil || len(body) > 2<<20 || r.Method != http.MethodPost || r.URL.Path != "/chat/completions" ||
					r.Header.Get("Authorization") != "Bearer "+key {
					t.Error("native allowance request escaped its provider contract")
					http.Error(w, "Invalid fixture request", http.StatusBadRequest)
					return
				}
				var request map[string]json.RawMessage
				if json.Unmarshal(body, &request) != nil {
					t.Error("native allowance provider received malformed JSON")
					return
				}
				legacy, hasLegacy := request["max_tokens"]
				completion, hasCompletion := request["max_completion_tokens"]
				selected := legacy
				if hasCompletion {
					selected = completion
				}
				var tokens int
				if hasLegacy == hasCompletion || json.Unmarshal(selected, &tokens) != nil || tokens != allowance {
					t.Error("native recovery exceeded or omitted its host response allowance")
				}
				number := calls.Add(1)
				message := map[string]any{"role": "assistant", "content": "Allowance fixture completed."}
				finish := "stop"
				if number == 1 {
					message["content"] = nil
					message["tool_calls"] = []any{map[string]any{"id": "truncated-fixture", "type": "function",
						"function": map[string]any{"name": "browser_exec", "arguments": `{"code":`}}}
					finish = "length"
				} else if number > 2 {
					t.Error("native allowance recovery exceeded its scripted request count")
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id": "allowance-fixture", "object": "chat.completion", "created": 1, "model": model,
					"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
					"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
				})
			}))
			t.Cleanup(provider.Close)
			request := hermes.Request{Objective: "Complete the allowance fixture.", Provider: "other", Model: model,
				Endpoint: provider.URL + "/chat/completions", APIKey: []byte(key), MaxIterations: 4, MaxTokens: allowance, TimeoutSeconds: 25}
			defer clear(request.APIKey)
			for _, invalid := range []int{-1, 0, 16385} {
				candidate := request
				candidate.MaxTokens = invalid
				if _, err := worker.Run(t.Context(), candidate, nil); err == nil || calls.Load() != 0 {
					t.Fatal("invalid native response allowance reached the provider")
				}
			}
			result, err := worker.Run(t.Context(), request, func(context.Context, hermes.Event) (hermes.Reply, error) {
				t.Error("a truncated tool call reached native execution")
				return hermes.Reply{}, nil
			})
			// Hermes can retry inside one reported iteration; its api_calls
			// counter is independent of these two forwarded HTTP receipts.
			if err != nil || result.Status != "completed" || calls.Load() != 2 {
				t.Fatal("native truncation recovery did not complete within the host response allowance")
			}
		})
	}
}
