package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
)

// CheckModel is a deterministic connection/access check, not a reasoning loop
// or a guarantee of browser-tool quality. Provider text is always discarded.
func CheckModel(ctx context.Context, model contracts.ModelConfig) (checkErr error) {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	values := sensitive.New()
	defer values.Clear()
	protected := sensitive.WithValues(ctx, values)
	if sensitive.Remember(protected, model.APIKey) != nil {
		return contracts.ErrModelRequestPolicy
	}
	if sensitive.HasValues(ctx) && sensitive.Remember(ctx, model.APIKey) != nil {
		return contracts.ErrModelRequestPolicy
	}
	redact := func(text string) string { return sensitive.Redact(protected, sensitive.Redact(ctx, text)) }
	if redact(model.Endpoint) != model.Endpoint || redact(model.Model) != model.Model {
		return contracts.ErrModelRequestPolicy
	}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	transport, err := startModelTransport(bounded, Request{Provider: model.Provider, Model: model.Model,
		Endpoint: model.Endpoint, APIKey: model.APIKey, MaxIterations: 1, MaxTokens: 128, TimeoutSeconds: 20}, redact)
	if err != nil {
		return contracts.ErrModelRequestPolicy
	}
	defer func() {
		closeErr := transport.Close()
		if ctx.Err() != nil {
			checkErr = ctx.Err()
		} else if bounded.Err() != nil {
			checkErr = contracts.ErrModelUnavailable
		}
		if closeErr != nil {
			checkErr = errors.Join(checkErr, contracts.ErrModelUnavailable, ErrUncertain)
		}
	}()
	allowance := "max_tokens"
	if model.Provider == "openai" || modelUsesCompletionTokens(model.Model) {
		allowance = "max_completion_tokens"
	}
	body, err := json.Marshal(map[string]any{"model": model.Model, "stream": false, allowance: 128,
		"messages": []any{map[string]string{"role": "user", "content": "Reply OK to confirm the browser model connection."}}})
	if err != nil {
		return contracts.ErrModelRequestPolicy
	}
	defer clear(body)
	input := &modelBody{reader: bytes.NewReader(body), closed: make(chan struct{})}
	request, err := http.NewRequestWithContext(bounded, http.MethodPost, transport.baseURL+"/chat/completions", input)
	if err != nil {
		_ = input.Close()
		return contracts.ErrModelRequestPolicy
	}
	request.Header.Set("Authorization", "Bearer "+string(transport.token))
	request.Header.Set("Content-Type", "application/json")
	localHTTP := &http.Transport{DisableKeepAlives: true, DisableCompression: true}
	defer localHTTP.CloseIdleConnections()
	client := &http.Client{Transport: localHTTP, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	defer func() { <-input.closed }()
	if err != nil {
		return contracts.ErrModelUnavailable
	}
	defer response.Body.Close()
	output, err := io.ReadAll(io.LimitReader(response.Body, modelResponseLimit+1))
	defer clear(output)
	if failure := transport.modelError(); failure != nil {
		return failure
	}
	if err != nil || len(output) > modelResponseLimit || response.StatusCode != http.StatusOK {
		return contracts.ErrModelResponse
	}
	var completion struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(output, &completion) != nil || len(completion.Choices) != 1 {
		return contracts.ErrModelResponse
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(completion.Choices[0].Message, &message) != nil || message == nil {
		return contracts.ErrModelResponse
	}
	var role string
	if json.Unmarshal(message["role"], &role) != nil || role != "assistant" {
		return contracts.ErrModelResponse
	}
	return nil
}

// Mirror the pinned Hermes fallback for the Other wire profile. The private
// proxy hides the provider hostname from its SDK; no model ID is rewritten.
func modelUsesCompletionTokens(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.LastIndex(model, "/"); index >= 0 {
		model = model[index+1:]
	}
	for _, prefix := range []string{"gpt-4o", "gpt-4.1", "gpt-5", "o1", "o3", "o4"} {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}
