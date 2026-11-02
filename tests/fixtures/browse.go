//go:build browserintegration

package fixtures

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

	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
)

// NewBrowseSite contains JS-driven content, non-secret interaction and canary
// form values that observations must omit. Credentials are synthetic fixtures.
func NewBrowseSite(t *testing.T, modelKey string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := strings.ReplaceAll(browsePage, "ENCODED_MODEL_KEY_CANARY", fmt.Sprintf("%%%02x%s", modelKey[0], modelKey[1:]))
		body = strings.ReplaceAll(body, "MODEL_KEY_CANARY", modelKey)
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("q") == "solar lamp" {
			_, _ = io.WriteString(w, `{"result":"The solar lamp costs 24 credits."}`)
		} else {
			_, _ = io.WriteString(w, `{"result":"No matching product."}`)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// NewBrowseScopeSite redirects a document to a distinct hostname on loopback.
// The foreign counter proves interception stopped the request before delivery.
func NewBrowseScopeSite(t *testing.T) (*httptest.Server, string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	redirects := &atomic.Int32{}
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "foreign-document-canary")
	}))
	t.Cleanup(foreign.Close)
	foreignURL := strings.Replace(foreign.URL, "127.0.0.1", "localhost", 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Scope fixture</title></head><body><a href="/leave">Follow redirect</a></body></html>`)
	})
	mux.HandleFunc("GET /leave", func(w http.ResponseWriter, r *http.Request) {
		redirects.Add(1)
		http.Redirect(w, r, foreignURL+"/private", http.StatusFound)
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)
	return origin, foreignURL, hits, redirects
}

const browsePage = `<!doctype html><html><head><title>Fixture catalog</title></head><body>
<h1>Catalog</h1><p id="ready"></p>
<label>Search fixtures <input id="query" type="search" aria-label="Search fixtures"></label>
<button id="search">Search catalog</button><p id="result">No search yet.</p>
<input type="password" value="password-value-canary">
<input name="api_token" value="token-value-canary">
<textarea hidden>hidden-text-canary</textarea>
<p hidden>hidden-paragraph-canary</p>
<p>Known-secret redaction fixture: MODEL_KEY_CANARY</p>
<a href="/ENCODED_MODEL_KEY_CANARY">Protected URL fixture</a>
<script>
document.querySelector('#ready').textContent = 'JavaScript catalog ready';
document.querySelector('#search').addEventListener('click', async () => {
  const response = await fetch('/search?q=' + encodeURIComponent(document.querySelector('#query').value));
  document.querySelector('#result').textContent = (await response.json()).result;
});
</script></body></html>`

type BrowseModel struct {
	URL   string
	mu    sync.Mutex
	calls int
}

func (model *BrowseModel) Calls() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.calls
}

// NewBrowseModel implements the actual HTTP model protocol; it is never injected
// into production actions. The key is accepted in one authentication header only.
func NewBrowseModel(t *testing.T, key string, decide func(intelligence.Turn) intelligence.Command) *BrowseModel {
	t.Helper()
	model := &BrowseModel{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil || len(body) > 65536 {
			http.Error(w, "Invalid fixture request", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			t.Error("model authentication did not use the managed fixture key")
		}
		for _, canary := range []string{key, fmt.Sprintf("%%%02x%s", key[0], key[1:]), "password-value-canary", "token-value-canary", "hidden-text-canary", "hidden-paragraph-canary"} {
			if bytes.Contains(body, []byte(canary)) {
				t.Error("a protected fixture value entered model context")
			}
		}
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Limit    int  `json:"max_completion_tokens"`
			Parallel bool `json:"parallel_tool_calls"`
			Store    bool `json:"store"`
		}
		if json.Unmarshal(body, &request) != nil || request.Limit < 1 || request.Limit > 1024 || request.Parallel || request.Store {
			t.Error("model request did not preserve execution bounds")
		}
		var turn intelligence.Turn
		found := false
		for _, message := range request.Messages {
			if message.Role == "user" {
				found = json.Unmarshal([]byte(message.Content), &turn) == nil
			}
		}
		if !found {
			t.Error("model request did not contain a structured browser turn")
			http.Error(w, "Invalid fixture turn", http.StatusBadRequest)
			return
		}
		if len(turn.AllowedHosts) == 0 {
			t.Error("model turn did not receive the effective navigation scope")
		}
		model.mu.Lock()
		model.calls++
		call := model.calls
		model.mu.Unlock()
		arguments, err := json.Marshal(decide(turn))
		if err != nil {
			http.Error(w, "Invalid fixture response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": fmt.Sprintf("call_%d", call), "type": "function", "function": map[string]any{"name": "browser_step", "arguments": string(arguments)},
			}}}, "finish_reason": "tool_calls",
		}}})
	}))
	model.URL = server.URL + "/v1/chat/completions"
	t.Cleanup(server.Close)
	return model
}
