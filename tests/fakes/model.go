//go:build browserintegration

package fakes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
)

type Model struct {
	URL   string
	calls atomic.Int32
}

func (model *Model) Calls() int { return int(model.calls.Load()) }

// NewModel implements the actual bounded Chat Completions tool protocol. Keys
// belong in its authentication header; protected values must never enter turns.
func NewModel(t *testing.T, key string, decide func(intelligence.Turn) intelligence.Command, forbidden ...string) *Model {
	t.Helper()
	model := &Model{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil || len(body) > 65536 || r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "Invalid fixture request", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("model request did not use the managed authentication header")
		}
		for _, value := range append([]string{key}, forbidden...) {
			if bytes.Contains(body, []byte(value)) {
				t.Error("a protected value entered the model request body")
			}
		}
		var request struct {
			Model    string `json:"model"`
			Limit    int    `json:"max_completion_tokens"`
			Parallel bool   `json:"parallel_tool_calls"`
			Store    bool   `json:"store"`
			Messages []struct{ Role, Content string }
		}
		if json.Unmarshal(body, &request) != nil || request.Model != "fixture-model" || request.Limit < 1 || request.Limit > 1024 || request.Parallel || request.Store {
			t.Error("model request escaped its configured protocol or execution budget")
		}
		var turn intelligence.Turn
		found := false
		for _, message := range request.Messages {
			if message.Role == "user" {
				found = json.Unmarshal([]byte(message.Content), &turn) == nil
			}
		}
		if !found || len(turn.AllowedHosts) == 0 {
			http.Error(w, "Invalid structured browser turn", http.StatusBadRequest)
			return
		}
		call := model.calls.Add(1)
		arguments, _ := json.Marshal(decide(turn))
		quoted, _ := json.Marshal(string(arguments))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"browser_step","arguments":%s}}]},"finish_reason":"tool_calls"}]}`, call, quoted)
	}))
	t.Cleanup(server.Close)
	model.URL = server.URL + "/v1/chat/completions"
	return model
}
