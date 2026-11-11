//go:build hermesintegration

package fakes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

type HermesToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type HermesRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func (r HermesRequest) HasTool(name string) bool {
	for _, tool := range r.Tools {
		if tool.Function.Name == name {
			return true
		}
	}
	return false
}

func (r HermesRequest) HasToolText(text string) bool {
	for _, message := range r.Messages {
		if message.Role == "tool" && bytes.Contains(message.Content, []byte(text)) {
			return true
		}
	}
	return false
}

type HermesResponse struct {
	Text  string
	Tools []HermesToolCall
	Block bool
}

func HermesCall(id, name string, arguments any) HermesToolCall {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		panic("invalid synthetic Hermes tool arguments")
	}
	call := HermesToolCall{ID: id, Type: "function"}
	call.Function.Name, call.Function.Arguments = name, string(encoded)
	return call
}

// HermesModel serves the real SDK HTTP boundary. Bodies remain in memory;
// failures report contract names, never request bodies or authentication values.
type HermesModel struct {
	URL          string
	Blocked      chan struct{}
	Disconnected chan struct{}
	calls        atomic.Int32
	blockedOnce  sync.Once
	closedOnce   sync.Once
}

func (m *HermesModel) Calls() int { return int(m.calls.Load()) }

func NewHermesModel(t *testing.T, key string, decide func(HermesRequest, int) HermesResponse) *HermesModel {
	t.Helper()
	model := &HermesModel{Blocked: make(chan struct{}), Disconnected: make(chan struct{})}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("Hermes model authentication was absent or outside its selected key")
			http.Error(w, "Fixture authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			// The actual SDK probes custom endpoints before its first turn.
			switch r.URL.Path {
			case "/v1/models":
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4.1-mini","object":"model"}]}`)
			case "/api/v1/models", "/api/tags", "/v1/props", "/props", "/version":
				http.NotFound(w, r)
			default:
				t.Error("Hermes made an unexpected metadata request")
				http.NotFound(w, r)
			}
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
		if err != nil || len(body) > 2<<20 || r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Error("Hermes escaped the fixture HTTP contract")
			http.Error(w, "Invalid fixture request", http.StatusBadRequest)
			return
		}
		if bytes.Contains(body, []byte(key)) {
			t.Error("the provider key entered a model request body")
		}
		var request HermesRequest
		if json.Unmarshal(body, &request) != nil || request.Model != "gpt-4.1-mini" {
			t.Error("Hermes sent an invalid model request")
			http.Error(w, "Invalid fixture model", http.StatusBadRequest)
			return
		}
		number := int(model.calls.Add(1))
		response := decide(request, number)
		if response.Block {
			model.blockedOnce.Do(func() { close(model.Blocked) })
			select {
			case <-r.Context().Done():
				model.closedOnce.Do(func() { close(model.Disconnected) })
			case <-release:
			}
			return
		}
		message := map[string]any{"role": "assistant", "content": response.Text}
		finish := "stop"
		if len(response.Tools) > 0 {
			message["content"], message["tool_calls"] = nil, response.Tools
			finish = "tool_calls"
		}
		common := map[string]any{"id": fmt.Sprintf("fixture-%d", number), "created": 1, "model": request.Model}
		usage := map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		if !request.Stream {
			common["object"], common["usage"] = "chat.completion", usage
			common["choices"] = []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(common)
			return
		}
		if len(response.Tools) > 0 {
			calls := make([]any, 0, len(response.Tools))
			for index, call := range response.Tools {
				calls = append(calls, map[string]any{"index": index, "id": call.ID, "type": call.Type, "function": call.Function})
			}
			message["tool_calls"] = calls
		}
		w.Header().Set("Content-Type", "text/event-stream")
		common["object"] = "chat.completion.chunk"
		for _, choice := range []any{
			map[string]any{"index": 0, "delta": message, "finish_reason": nil},
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish},
		} {
			common["choices"], common["usage"] = []any{choice}, usage
			chunk, _ := json.Marshal(common)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	model.URL = server.URL + "/v1"
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return model
}
