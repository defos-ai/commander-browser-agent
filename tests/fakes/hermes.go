//go:build hermesintegration

package fakes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	Text     string
	Tools    []HermesToolCall
	Block    bool
	Escape   string
	Redirect string
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
	mu           sync.RWMutex
	key          string
	protected    []string
}

func (m *HermesModel) Calls() int { return int(m.calls.Load()) }

func (m *HermesModel) RotateKey(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.key = key
	m.protected = append(m.protected, key)
}

func NewHermesModel(t *testing.T, key string, decide func(HermesRequest, int) HermesResponse, forbidden ...string) *HermesModel {
	t.Helper()
	model := &HermesModel{Blocked: make(chan struct{}), Disconnected: make(chan struct{}), key: key, protected: append([]string{key}, forbidden...)}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model.mu.RLock()
		key, protected := model.key, append([]string(nil), model.protected...)
		model.mu.RUnlock()
		for _, value := range protected {
			if value == "" {
				continue
			}
			if strings.Contains(r.URL.String(), value) {
				t.Error("a protected value entered the provider URL")
			}
			for name, values := range r.Header {
				if strings.Contains(name, value) {
					t.Error("a protected value entered a provider header name")
				}
				for _, header := range values {
					if strings.Contains(header, value) && !(value == key && name == "Authorization" && header == "Bearer "+key) {
						t.Error("a protected value entered a non-authentication provider header")
					}
				}
			}
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("Hermes model authentication was absent or outside its selected key")
			http.Error(w, "Fixture authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			// SDK discovery belongs to the private proxy, never this provider.
			t.Error("SDK metadata probes escaped the private model proxy")
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
		if err != nil || len(body) > 2<<20 || r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Error("Hermes escaped the fixture HTTP contract")
			http.Error(w, "Invalid fixture request", http.StatusBadRequest)
			return
		}
		for _, value := range protected {
			if hermesJSONContains(body, value, 0) {
				t.Error("a protected value entered a model request body or replay")
			}
		}
		var request HermesRequest
		if json.Unmarshal(body, &request) != nil || request.Model != "gpt-4.1-mini" {
			t.Error("Hermes sent an invalid model request")
			http.Error(w, "Invalid fixture model", http.StatusBadRequest)
			return
		}
		number := int(model.calls.Add(1))
		response := decide(request, number)
		if response.Redirect != "" {
			http.Redirect(w, r, response.Redirect, http.StatusTemporaryRedirect)
			return
		}
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
			encoded, _ := json.Marshal(common)
			if response.Escape != "" {
				encoded = bytes.ReplaceAll(encoded, []byte(response.Escape), []byte(HermesUnicode(response.Escape)))
			}
			_, _ = w.Write(encoded)
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

// HermesUnicode creates JSON escapes for the fixture's ASCII synthetic keys.
func HermesUnicode(value string) string {
	var encoded strings.Builder
	for _, character := range value {
		_, _ = fmt.Fprintf(&encoded, `\u%04x`, character)
	}
	return encoded.String()
}

func hermesJSONContains(data []byte, value string, depth int) bool {
	if value == "" || depth > 16 {
		return false
	}
	if bytes.Contains(data, []byte(value)) {
		return true
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return false
	}
	var contains func(any) bool
	contains = func(item any) bool {
		switch item := item.(type) {
		case string:
			return strings.Contains(item, value) || hermesJSONContains([]byte(item), value, depth+1)
		case []any:
			for _, child := range item {
				if contains(child) {
					return true
				}
			}
		case map[string]any:
			for name, child := range item {
				if strings.Contains(name, value) || contains(child) {
					return true
				}
			}
		}
		return false
	}
	return contains(decoded)
}
