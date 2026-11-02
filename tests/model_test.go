package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
)

const modelFixtureKey = "fixture-api-credential-must-stay-private"

func TestModelConfigurationRequiresExplicitSafeEndpointAndName(t *testing.T) {
	for _, endpoint := range []string{"https://models.example/v1/chat/completions", "http://localhost/custom/chat", "http://127.0.0.1:1234/chat", "http://[::1]:1234/chat", "http://127.2.3.4/chat"} {
		if err := intelligence.ValidateModelConfig(intelligence.ModelConfig{URL: endpoint, Name: "fixture-model"}); err != nil {
			t.Fatalf("rejected allowed endpoint %q: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"", "/v1/chat/completions", "http://models.example/chat", "http://127.0.0.1.example/chat", "http://localhost.example/chat", "http://0.0.0.0/chat", "https://operator:password@models.example/chat", "https://models.example/chat?token=value", "https://models.example/chat?", "https://models.example/chat#", "http://127.0.0.1:99999/chat", "https://models.example:"} {
		if err := intelligence.ValidateModelConfig(intelligence.ModelConfig{URL: endpoint, Name: "fixture-model"}); err == nil {
			t.Fatalf("accepted invalid endpoint %q", endpoint)
		}
	}
	for _, name := range []string{"", " ", " padded", "padded ", "line\nbreak", strings.Repeat("m", 201)} {
		if err := intelligence.ValidateModelConfig(intelligence.ModelConfig{URL: "https://models.example/chat", Name: name}); err == nil {
			t.Fatalf("accepted invalid model name %q", name)
		}
	}
}

func TestModelSendsOneBoundedStructuredCommandRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/exact/chat/completions" || request.Header.Get("Authorization") != "Bearer "+modelFixtureKey {
			t.Error("model request changed its endpoint, method or credential header")
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, (64<<10)+1))
		if err != nil || len(body) > 64<<10 || bytes.Contains(body, []byte(modelFixtureKey)) {
			t.Error("model request exceeded its limit or exposed its credential")
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
		}
		for key, want := range map[string]string{"model": `"fixture-model"`, "tool_choice": `"required"`, "parallel_tool_calls": "false", "store": "false", "max_completion_tokens": "123"} {
			if string(payload[key]) != want {
				t.Errorf("request field %s = %s, want %s", key, payload[key], want)
			}
		}
		if _, present := payload["temperature"]; present {
			t.Error("model request introduced a temperature default")
		}
		var tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(payload["tools"], &tools) != nil || len(tools) != 1 || tools[0].Type != "function" || tools[0].Function.Name != "browser_step" || tools[0].Function.Parameters["additionalProperties"] != false {
			t.Error("request did not offer the single bounded browser tool")
		}
		_, _ = w.Write(modelCompletion(`{"action":"extract"}`))
	}))
	defer server.Close()
	key := []byte(modelFixtureKey)
	model, err := intelligence.NewModel(intelligence.ModelConfig{URL: server.URL + "/exact/chat/completions", Name: "fixture-model"}, key)
	if err != nil {
		t.Fatal(err)
	}
	defer model.(io.Closer).Close()
	command, err := model.Next(t.Context(), intelligence.Turn{Objective: "Collect fixture evidence"}, 123)
	if err != nil || command.Action != "extract" {
		t.Fatalf("optional command fields were rejected: command=%+v error=%v", command, err)
	}
	if _, err := model.Next(t.Context(), intelligence.Turn{}, 123); err != nil {
		t.Fatalf("rejected a tool-call identity reused in an independent request: %v", err)
	}
	clear(key)
	if _, err := model.Next(t.Context(), intelligence.Turn{}, 123); err == nil || calls.Load() != 2 {
		t.Fatal("client copied the caller-owned key or sent a request after its lifetime ended")
	}
}

func TestModelRejectsMalformedOrUnsafeProviderCommands(t *testing.T) {
	valid := modelCompletion(`{"action":"extract"}`)
	for name, body := range map[string][]byte{
		"malformed envelope":   []byte(`{"choices":`),
		"missing choice":       []byte(`{"choices":[]}`),
		"multiple choices":     bytes.Replace(valid, []byte(`{"choices":[`), []byte(`{"choices":[{},`), 1),
		"duplicate envelope":   []byte(`{"choices":[],"choices":[]}`),
		"truncated completion": bytes.Replace(valid, []byte(`"tool_calls","message"`), []byte(`"length","message"`), 1),
		"refusal":              bytes.Replace(valid, []byte(`"role":"assistant"`), []byte(`"refusal":"declined","role":"assistant"`), 1),
		"free text":            bytes.Replace(valid, []byte(`"role":"assistant"`), []byte(`"content":"extra instructions","role":"assistant"`), 1),
		"wrong tool":           bytes.Replace(valid, []byte(`"name":"browser_step"`), []byte(`"name":"execute_script"`), 1),
		"wrong tool type":      bytes.Replace(valid, []byte(`"type":"function"`), []byte(`"type":"other"`), 1),
		"multiple tool calls":  bytes.Replace(valid, []byte(`"tool_calls":[`), []byte(`"tool_calls":[{},`), 1),
		"wrong message role":   bytes.Replace(valid, []byte(`"role":"assistant"`), []byte(`"role":"tool"`), 1),
		"missing call id":      bytes.Replace(valid, []byte(`"id":"call-fixture"`), []byte(`"id":""`), 1),
		"unknown action":       modelCompletion(`{"action":"javascript"}`),
		"unknown argument":     modelCompletion(`{"action":"click","selector":"button"}`),
		"case alias argument":  modelCompletion(`{"action":"extract","Action":"navigate"}`),
		"null argument":        modelCompletion(`{"action":"extract","target":null}`),
		"malformed arguments":  modelCompletion(`{"action":`),
		"multiple values":      modelCompletion(`{"action":"extract"}{"action":"navigate"}`),
		"duplicate arguments":  modelCompletion(`{"action":"extract","action":"navigate"}`),
		"wrong argument type":  modelCompletion(`{"action":"type_text","target":"1"}`),
		"unknown finding":      modelCompletion(`{"action":"finish","findings":[{"text":"claim","evidence_ids":["e1"],"script":"ignored"}]}`),
		"null reference":       modelCompletion(`{"action":"finish","findings":[{"text":"claim","evidence_ids":[null]}]}`),
		"too large":            append(bytes.Repeat([]byte(" "), 64<<10), valid...),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { _, _ = w.Write(body) }))
			defer server.Close()
			model := newModelFixture(t, server.URL)
			if _, err := model.Next(t.Context(), intelligence.Turn{}, 123); err == nil {
				t.Fatal("accepted a malformed or unsafe model command")
			}
		})
	}
}

func TestModelCloseWaitsForRequestsAndReleasesBorrowedCredential(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		_, _ = w.Write(modelCompletion(`{"action":"extract"}`))
	}))
	defer server.Close()
	key := []byte(modelFixtureKey)
	model, err := intelligence.NewModel(intelligence.ModelConfig{URL: server.URL, Name: "fixture-model"}, key)
	if err != nil {
		t.Fatal(err)
	}
	closer, ok := model.(io.Closer)
	if !ok {
		t.Fatal("model does not provide request-lifetime cleanup")
	}
	requestDone, closeDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := model.Next(t.Context(), intelligence.Turn{}, 123); requestDone <- err }()
	<-started
	go func() { closeDone <- closer.Close() }()
	select {
	case <-closeDone:
		close(release)
		t.Fatal("model released its credential reference before the request finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if string(key) != modelFixtureKey {
		t.Fatal("model cleanup modified caller-owned credentials")
	}
	if _, err := model.Next(t.Context(), intelligence.Turn{}, 123); err == nil || calls.Load() != 1 {
		t.Fatal("closed model accepted another request")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestModelRejectsEscapedCredentialEchoes(t *testing.T) {
	arguments := `{"action":"type_text","target":1,"text":"\u0066ixture-api-credential-must-stay-private"}`
	body := modelCompletion(arguments)
	if bytes.Contains(body, []byte(modelFixtureKey)) {
		t.Fatal("fixture did not encode its credential echo")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	if _, err := newModelFixture(t, server.URL).Next(t.Context(), intelligence.Turn{}, 123); err == nil || strings.Contains(err.Error(), modelFixtureKey) || strings.Contains(err.Error(), server.URL) {
		t.Fatal("model credential echo was accepted or exposed in an error")
	}
}

func TestModelNeverRedirectsRetriesOrLeaksProviderErrors(t *testing.T) {
	var forwarded, calls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { forwarded.Add(1) }))
	defer destination.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusUnauthorized, http.StatusTooManyRequests} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			calls.Add(1)
			w.Header().Set("Location", destination.URL)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(modelFixtureKey + " private provider error"))
		}))
		_, err := newModelFixture(t, server.URL).Next(t.Context(), intelligence.Turn{}, 123)
		server.Close()
		if err == nil || strings.Contains(err.Error(), modelFixtureKey) || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "private provider error") {
			t.Fatal("provider failure or redirect exposed private details")
		}
	}
	if forwarded.Load() != 0 || calls.Load() != 3 {
		t.Fatalf("model client forwarded credentials or retried: forwarded=%d calls=%d", forwarded.Load(), calls.Load())
	}
}

func TestModelRejectsOversizedOrCanceledInputBeforeSending(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { calls.Add(1) }))
	defer server.Close()
	model := newModelFixture(t, server.URL)
	turn := intelligence.Turn{Observation: intelligence.Observation{Text: strings.Repeat("x", 64<<10)}}
	if _, err := model.Next(t.Context(), turn, 123); err == nil {
		t.Fatal("sent an oversized model input")
	}
	turn = intelligence.Turn{Objective: modelFixtureKey}
	if _, err := model.Next(t.Context(), turn, 123); err == nil {
		t.Fatal("sent a credential in model context")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := model.Next(ctx, intelligence.Turn{}, 123); err != context.Canceled || calls.Load() != 0 {
		t.Fatalf("canceled or unsafe input reached provider: calls=%d error=%v", calls.Load(), err)
	}
}

func newModelFixture(t *testing.T, endpoint string) intelligence.Model {
	t.Helper()
	model, err := intelligence.NewModel(intelligence.ModelConfig{URL: endpoint, Name: "fixture-model"}, []byte(modelFixtureKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = model.(io.Closer).Close() })
	return model
}

func modelCompletion(arguments string) []byte {
	value := map[string]any{"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{"id": "call-fixture", "type": "function", "function": map[string]any{"name": "browser_step", "arguments": arguments}}},
	}}}}
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
