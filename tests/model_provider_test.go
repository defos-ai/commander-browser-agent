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

func TestModelProviderPresetsAreFreshAndContainNoModelDefaults(t *testing.T) {
	want := map[string]struct {
		endpoint string
		secret   string
	}{
		"openai":    {"https://api.openai.com/v1/chat/completions", "OPENAI_API_KEY"},
		"anthropic": {"https://api.anthropic.com/v1/chat/completions", "ANTHROPIC_API_KEY"},
		"gemini":    {"https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", "GEMINI_API_KEY"},
		"xai":       {"https://api.x.ai/v1/chat/completions", "XAI_API_KEY"},
		"deepseek":  {"https://api.deepseek.com/v1/chat/completions", "DEEPSEEK_API_KEY"},
		"other":     {},
	}
	providers := intelligence.Providers()
	if len(providers) != len(want) {
		t.Fatalf("provider count = %d, want %d", len(providers), len(want))
	}
	for _, provider := range providers {
		expected, ok := want[provider.ID]
		if !ok || provider.Name == "" || provider.URL != expected.endpoint || provider.SecretName != expected.secret {
			t.Fatalf("unexpected provider preset: %+v", provider)
		}
		if err := intelligence.ValidateModelConfig(intelligence.ModelConfig{Provider: provider.ID, URL: "http://127.0.0.1/exact-endpoint", Name: "fixture-model-2026-11"}); err != nil {
			t.Fatalf("provider wire profile rejected a valid endpoint: %v", err)
		}
	}
	providers[0].ID, providers[0].SecretName = "changed", "CHANGED_KEY"
	provider, ok := intelligence.Provider("openai")
	if !ok || provider.SecretName != "OPENAI_API_KEY" {
		t.Fatal("provider catalog retained a caller's mutations")
	}
	provider.SecretName = "CHANGED_AGAIN"
	if fresh, _ := intelligence.Provider("openai"); fresh.SecretName != "OPENAI_API_KEY" {
		t.Fatal("provider lookup retained a caller's mutation")
	}
	for _, unsupported := range []string{"OpenAI", "unknown", " openai"} {
		if _, ok := intelligence.Provider(unsupported); ok {
			t.Fatal("provider lookup accepted a noncanonical identity")
		}
		if err := intelligence.ValidateModelConfig(intelligence.ModelConfig{Provider: unsupported, URL: "https://models.example/chat", Name: "fixture-model"}); err == nil {
			t.Fatal("model configuration accepted an unknown provider")
		}
	}
}

func TestModelPartialConfigurationValidatorsDoNotNeedOtherSettings(t *testing.T) {
	if err := intelligence.ValidateModelURL("https://models.example/exact/chat/completions"); err != nil {
		t.Fatal(err)
	}
	if err := intelligence.ValidateModelName("fixture-model-2026-11"); err != nil {
		t.Fatal(err)
	}
	if err := intelligence.ValidateModelURL("https://operator:private-url-canary@models.example/chat"); err == nil || strings.Contains(err.Error(), "private-url-canary") {
		t.Fatal("partial endpoint validation accepted or exposed embedded credentials")
	}
	if err := intelligence.ValidateModelName("private-name-canary\n"); err == nil || strings.Contains(err.Error(), "private-name-canary") {
		t.Fatal("partial model validation accepted or exposed invalid configuration")
	}
}

func TestModelProviderProfilesSendTheirSupportedWireFields(t *testing.T) {
	for _, test := range []struct {
		provider string
		tokens   string
		choice   string
		parallel bool
		store    bool
	}{
		{provider: "openai", tokens: "max_completion_tokens", choice: "required", parallel: true, store: true},
		{provider: "anthropic", tokens: "max_completion_tokens", choice: "auto", parallel: true},
		{provider: "gemini", tokens: "max_tokens", choice: "auto"},
		{provider: "xai", tokens: "max_completion_tokens", choice: "required", parallel: true},
		{provider: "deepseek", tokens: "max_tokens", choice: "auto"},
		{provider: "other", tokens: "max_completion_tokens", choice: "required", parallel: true, store: true},
	} {
		t.Run(test.provider, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				if request.Method != http.MethodPost || request.URL.Path != "/configured/completions" || request.Header.Get("Authorization") != "Bearer "+modelFixtureKey {
					t.Error("provider request changed its exact endpoint, method or authentication header")
				}
				body, err := io.ReadAll(io.LimitReader(request.Body, (64<<10)+1))
				if err != nil || len(body) > 64<<10 || bytes.Contains(body, []byte(modelFixtureKey)) {
					t.Error("provider request exceeded its limit or placed the key in its body")
				}
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Error(err)
				}
				if string(payload["model"]) != `"fixture-model-2026-11"` || string(payload[test.tokens]) != "321" || string(payload["tool_choice"]) != `"`+test.choice+`"` {
					t.Error("provider did not preserve the requested model, completion allowance or supported tool mode")
				}
				otherTokens := "max_tokens"
				if test.tokens == otherTokens {
					otherTokens = "max_completion_tokens"
				}
				if _, present := payload[otherTokens]; present {
					t.Error("provider sent conflicting completion allowance fields")
				}
				for name, supported := range map[string]bool{"parallel_tool_calls": test.parallel, "store": test.store} {
					value, present := payload[name]
					if present != supported || present && string(value) != "false" {
						t.Errorf("provider field %s: present=%v, value=%s", name, present, value)
					}
				}
				for _, unsupported := range []string{"temperature", "thinking", "reasoning_effort", "stream"} {
					if _, present := payload[unsupported]; present {
						t.Errorf("provider introduced an unsolicited %s setting", unsupported)
					}
				}
				var tools []struct {
					Type     string `json:"type"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if json.Unmarshal(payload["tools"], &tools) != nil || len(tools) != 1 || tools[0].Type != "function" || tools[0].Function.Name != "browser_step" {
					t.Error("provider did not offer exactly one typed browser tool")
				}
				body = modelCompletion(`{"action":"extract"}`)
				if test.choice == "auto" {
					body = bytes.Replace(body, []byte(`"role":"assistant"`), []byte(`"content":"I will collect the page evidence.","role":"assistant"`), 1)
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			model := newProviderModelFixture(t, test.provider, server.URL+"/configured/completions")
			command, err := model.Next(t.Context(), intelligence.Turn{Objective: "Collect fixture evidence"}, 321)
			if err != nil || command.Action != "extract" || calls.Load() != 1 {
				t.Fatalf("provider did not return its single typed command: action=%s calls=%d error=%v", command.Action, calls.Load(), err)
			}
		})
	}
}

func TestModelProviderProfilesKeepStrictCommandAndSecretBoundaries(t *testing.T) {
	valid := modelCompletion(`{"action":"extract"}`)
	for _, provider := range intelligence.Providers() {
		t.Run(provider.ID, func(t *testing.T) {
			for name, body := range map[string][]byte{
				"no tool call":         []byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"No command"}}]}`),
				"multiple calls":       bytes.Replace(valid, []byte(`"tool_calls":[`), []byte(`"tool_calls":[{},`), 1),
				"truncation":           bytes.Replace(valid, []byte(`"finish_reason":"tool_calls"`), []byte(`"finish_reason":"length"`), 1),
				"refusal":              bytes.Replace(valid, []byte(`"role":"assistant"`), []byte(`"refusal":"declined","role":"assistant"`), 1),
				"untyped command":      modelCompletion(`{"action":"extract","script":"ignore schema"}`),
				"credential in args":   modelCompletion(`{"action":"type_text","target":1,"text":"\u0066ixture-api-credential-must-stay-private"}`),
				"credential in text":   bytes.Replace(valid, []byte(`"role":"assistant"`), []byte(`"content":"`+modelFixtureKey+`","role":"assistant"`), 1),
				"credential reasoning": bytes.Replace(valid, []byte(`"role":"assistant"`), []byte(`"reasoning_content":"`+modelFixtureKey+`","role":"assistant"`), 1),
				"oversized response":   append(bytes.Repeat([]byte(" "), 64<<10), valid...),
			} {
				t.Run(name, func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { _, _ = w.Write(body) }))
					defer server.Close()
					_, err := newProviderModelFixture(t, provider.ID, server.URL).Next(t.Context(), intelligence.Turn{}, 123)
					if err == nil || strings.Contains(err.Error(), modelFixtureKey) || strings.Contains(err.Error(), server.URL) {
						t.Fatal("provider accepted an unsafe command or exposed protected values")
					}
				})
			}
		})
	}
}

func TestModelProviderProfilesNeverForwardCredentialsOrRetry(t *testing.T) {
	for _, provider := range intelligence.Providers() {
		t.Run(provider.ID, func(t *testing.T) {
			var forwarded, calls atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { forwarded.Add(1) }))
			defer destination.Close()
			for _, status := range []int{http.StatusTemporaryRedirect, http.StatusUnauthorized, http.StatusTooManyRequests} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					calls.Add(1)
					w.Header().Set("Location", destination.URL)
					w.WriteHeader(status)
					_, _ = w.Write([]byte(modelFixtureKey + " private provider failure"))
				}))
				_, err := newProviderModelFixture(t, provider.ID, server.URL).Next(t.Context(), intelligence.Turn{}, 123)
				server.Close()
				if err == nil || strings.Contains(err.Error(), modelFixtureKey) || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "private provider failure") {
					t.Fatal("provider failure exposed protected response data")
				}
			}
			if forwarded.Load() != 0 || calls.Load() != 3 {
				t.Fatal("provider request followed a credential-bearing redirect or retried")
			}
		})
	}
}

func newProviderModelFixture(t *testing.T, provider, endpoint string) intelligence.Model {
	t.Helper()
	model, err := intelligence.NewModel(intelligence.ModelConfig{Provider: provider, URL: endpoint, Name: "fixture-model-2026-11"}, []byte(modelFixtureKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = model.(io.Closer).Close() })
	return model
}
