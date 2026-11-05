package browsing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

// Setup retains non-secret model choices for each full Commander creation claim.
// Its zero value is ready for use and starts no browser or model connection.
type Setup struct {
	mu      sync.Mutex
	entries map[string]*modelSetup
}

type modelSetup struct {
	claim     sessions.CreationClaim
	gate      chan struct{}
	selection *intelligence.ModelConfig // Non-secret settings only; never API keys.
}

// modelSecretName binds custom endpoint credentials to their exact validated URL.
// A key for a standard provider or another endpoint is never an implicit fallback.
func modelSecretName(address string) string {
	digest := sha256.Sum256([]byte(address))
	return "browser-model-api-key-" + hex.EncodeToString(digest[:])
}

// Prepare resolves model choices and a current managed key for the bound session.
// The caller owns the returned key bytes and must clear them after use.
func (s *Setup) Prepare(ctx context.Context, scopeID, sessionID string) (intelligence.ModelConfig, []byte, error) {
	var empty intelligence.ModelConfig
	if err := ctx.Err(); err != nil {
		return empty, nil, err
	}
	binding, ok := action.SessionFromContext(ctx)
	if !ok || binding.Validate() != nil || scopeID != binding.ScopeID ||
		binding.Mode == sessions.Create && sessionID != "" ||
		binding.Mode == sessions.Reuse && sessionID != binding.ID {
		return empty, nil, errors.New("model setup requires the matching framework browser session binding")
	}
	s.mu.Lock()
	if s.entries == nil {
		s.entries = make(map[string]*modelSetup)
	}
	setup := s.entries[binding.ID]
	if setup == nil {
		setup = &modelSetup{claim: binding.Claim(), gate: make(chan struct{}, 1)}
		s.entries[binding.ID] = setup
	}
	conflict := setup.claim != binding.Claim()
	s.mu.Unlock()
	if conflict {
		return empty, nil, sessions.ErrConflict
	}
	select {
	case setup.gate <- struct{}{}:
		defer func() { <-setup.gate }()
	case <-ctx.Done():
		return empty, nil, ctx.Err()
	}
	var selection intelligence.ModelConfig
	if setup.selection != nil {
		selection = *setup.selection
	} else {
		var err error
		selection, err = selectBrowserModel(ctx)
		if err != nil {
			return empty, nil, err
		}
		// Retain valid operator choices even when credentials are still pending.
		// The framework owns saved question answers and operator/model eligibility.
		setup.selection = &selection
	}
	key, err := selectedModelKey(ctx, selection)
	return selection, key, err
}

func selectBrowserModel(ctx context.Context) (intelligence.ModelConfig, error) {
	var selection intelligence.ModelConfig
	question, err := action.Question(ctx, "browser_model_provider", "Which model provider should this browser session use? Select a provider, not an API key.",
		"OpenAI", "Anthropic", "Google Gemini", "More providers")
	if err != nil {
		return selection, err
	}
	answer := question.Answer
	if strings.EqualFold(strings.TrimSpace(answer), "More providers") {
		question, err = action.Question(ctx, "browser_model_provider_more", "Which additional model provider should this browser session use?",
			"xAI", "DeepSeek", "Other (OpenAI-compatible)")
		if err != nil {
			return selection, err
		}
		answer = question.Answer
	}
	provider, ok := providerID(answer)
	if !ok {
		return selection, errors.New("model provider answer must identify a supported provider")
	}
	selection.Provider = provider
	question, err = action.Question(ctx, "browser_model_name", "Which exact tool-calling model ID/version should this browser session use? Enter the model ID, not an API key.")
	if err != nil {
		return selection, err
	}
	selection.Name = strings.TrimSpace(question.Answer)
	if intelligence.ValidateModelName(selection.Name) != nil {
		return selection, errors.New("model answer must be a valid exact model ID/version")
	}
	if selection.Provider == "other" {
		question, err := action.Question(ctx, "browser_model_url", "What is the exact Chat Completions endpoint for this custom model provider? Enter an HTTPS URL, or local HTTP URL, without credentials, query parameters or fragment.")
		if err != nil {
			return selection, err
		}
		selection.URL = strings.TrimSpace(question.Answer)
	} else {
		preset, ok := intelligence.Provider(selection.Provider)
		if !ok {
			return selection, errors.New("model provider is not supported")
		}
		selection.URL = preset.URL
	}
	if intelligence.ValidateModelConfig(selection) != nil {
		return selection, errors.New("selected browser model configuration is invalid")
	}
	return selection, nil
}

func selectedModelKey(ctx context.Context, selection intelligence.ModelConfig) ([]byte, error) {
	name := modelSecretName(selection.URL)
	if selection.Provider != "other" {
		preset, ok := intelligence.Provider(selection.Provider)
		if !ok || selection.URL != preset.URL || preset.SecretName == "" {
			return nil, errors.New("model credential destination does not match its provider")
		}
		name = preset.SecretName
	}
	return action.RequestReusableSecret(ctx, name)
}

func providerID(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai", "open ai":
		return "openai", true
	case "anthropic", "claude":
		return "anthropic", true
	case "gemini", "google", "google gemini":
		return "gemini", true
	case "xai", "x.ai":
		return "xai", true
	case "deepseek":
		return "deepseek", true
	case "other", "other (openai-compatible)", "openai-compatible", "custom":
		return "other", true
	default:
		return "", false
	}
}
