package browser_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
)

func TestProviderProtocolsKeepCommandsTypedAndCredentialsPrivate(t *testing.T) {
	const key = "fixture-private-model-key"
	for _, test := range []struct {
		provider, tokens, choice string
		parallel, store          bool
	}{
		{"openai", "max_completion_tokens", "required", true, true},
		{"anthropic", "max_completion_tokens", "auto", true, false},
		{"gemini", "max_tokens", "auto", false, false},
		{"xai", "max_completion_tokens", "required", true, false},
		{"deepseek", "max_tokens", "auto", false, false},
		{"other", "max_completion_tokens", "required", true, true},
	} {
		t.Run(test.provider, func(t *testing.T) {
			valid := modelReply(`{"action":"ask","question":"Which catalog?","options":["Work","Personal"]}`)
			responses := [][]byte{
				valid,
				bytes.Replace(valid, []byte(`"tool_calls":[`), []byte(`"tool_calls":[{},`), 1),
				modelReply(`{"action":"ask","question":"Which?","options":[null,"Work"]}`),
				modelReply(`{"action":"ask","question":"\u0066ixture-private-model-key"}`),
			}
			var calls, forwarded atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				index := int(calls.Add(1)) - 1
				body, err := io.ReadAll(io.LimitReader(request.Body, (64<<10)+1))
				if err != nil || len(body) > 64<<10 || bytes.Contains(body, []byte(key)) || request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+key {
					t.Error("model credential escaped its header or request bounds")
				}
				var payload map[string]json.RawMessage
				if json.Unmarshal(body, &payload) != nil || string(payload["model"]) != `"exact-model-v1"` || string(payload[test.tokens]) != "321" || string(payload["tool_choice"]) != `"`+test.choice+`"` {
					t.Error("provider did not preserve its exact model and supported wire fields")
				}
				otherTokens := "max_tokens"
				if test.tokens == otherTokens {
					otherTokens = "max_completion_tokens"
				}
				if _, present := payload[otherTokens]; present {
					t.Error("provider sent competing token allowances")
				}
				for name, expected := range map[string]bool{"parallel_tool_calls": test.parallel, "store": test.store} {
					value, present := payload[name]
					if present != expected || present && string(value) != "false" {
						t.Error("provider sent unsupported parallel/storage settings")
					}
				}
				if !bytes.Contains(payload["tools"], []byte(`"browser_step"`)) {
					t.Error("provider omitted its typed browser tool")
				}
				if index < len(responses) {
					_, _ = w.Write(responses[index])
				} else {
					w.Header().Set("Location", destination.URL)
					w.WriteHeader(http.StatusTemporaryRedirect)
					_, _ = w.Write([]byte(key + " private provider error"))
				}
			}))
			defer server.Close()
			model, err := intelligence.NewModel(intelligence.ModelConfig{Provider: test.provider, URL: server.URL, Name: "exact-model-v1"}, []byte(key))
			if err != nil {
				t.Fatal(err)
			}
			defer model.(io.Closer).Close()
			for i := 0; i <= len(responses); i++ {
				command, err := model.Next(t.Context(), intelligence.Turn{Objective: "Find a price"}, 321)
				if i == 0 {
					if err != nil || command.Action != "ask" || command.Question != "Which catalog?" || len(command.Options) != 2 {
						t.Fatal("valid typed clarification was rejected")
					}
				} else if err == nil || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "private provider error") {
					t.Fatal("model accepted an unsafe command or exposed provider data")
				}
			}
			if calls.Load() != int32(len(responses)+1) || forwarded.Load() != 0 {
				t.Fatal("model retried or forwarded an authenticated redirect")
			}
		})
	}
}

func modelReply(arguments string) []byte {
	value := map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{"id": "fixture-call", "type": "function", "function": map[string]any{"name": "browser_step", "arguments": arguments}}},
	}}}}
	encoded, _ := json.Marshal(value)
	return encoded
}
