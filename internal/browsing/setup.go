package browsing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

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
	selection *modelConfig // Non-secret settings only; never API keys.
}

// modelSecretName binds custom endpoint credentials to their exact validated URL.
// A key for a standard provider or another endpoint is never an implicit fallback.
func modelSecretName(address string) string {
	digest := sha256.Sum256([]byte(address))
	return "browser-model-api-key-" + hex.EncodeToString(digest[:])
}

func (s *Setup) prepare(ctx context.Context) (modelConfig, []byte, error) {
	var empty modelConfig
	if err := ctx.Err(); err != nil {
		return empty, nil, err
	}
	binding, err := action.OpenSession(ctx)
	if err != nil {
		return empty, nil, err
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
	var selection modelConfig
	if setup.selection != nil {
		selection = *setup.selection
	} else {
		var err error
		selection, err = selectHermesModel(ctx)
		if err != nil {
			return empty, nil, err
		}
	}
	if err := acceptHermesModel(selection); err != nil {
		return empty, nil, err
	}
	if setup.selection == nil {
		// Retain valid operator choices even when credentials are still pending.
		// The framework owns saved question answers and operator/model eligibility.
		setup.selection = &selection
	}
	key, err := selectedModelKey(ctx, selection)
	return selection, key, err
}

func selectModelDetails(ctx context.Context, provider string) (modelConfig, error) {
	selection := modelConfig{Provider: provider}
	question, err := action.Question(ctx, "browser_model_name", "Which exact tool-calling model ID/version should this browser session use? Enter the model ID, not an API key.")
	if err != nil {
		return selection, err
	}
	selection.Name = strings.TrimSpace(question.Answer)
	if validateModelName(selection.Name) != nil {
		return selection, errors.New("model answer must be a valid exact model ID/version")
	}
	if selection.Provider == "other" {
		question, err := action.Question(ctx, "browser_model_url", "What is the exact Chat Completions endpoint for this custom model provider? Enter an HTTPS URL, or local HTTP URL, without credentials, query parameters or fragment.")
		if err != nil {
			return selection, err
		}
		selection.URL = strings.TrimSpace(question.Answer)
	} else if selection.Provider == "openai" {
		selection.URL = openAIEndpoint
	} else {
		return selection, errors.New("model provider is not supported")
	}
	if validateModelConfig(selection) != nil {
		return selection, errors.New("selected browser model configuration is invalid")
	}
	return selection, nil
}

func selectedModelKey(ctx context.Context, selection modelConfig) ([]byte, error) {
	var name string
	switch selection.Provider {
	case "other":
		name = modelSecretName(selection.URL)
	case "openai":
		if selection.URL != openAIEndpoint {
			return nil, errors.New("model credential destination does not match its provider")
		}
		name = openAISecretName
	default:
		return nil, errors.New("model credential destination does not match its provider")
	}
	return action.RequestReusableSecret(ctx, name)
}

func providerID(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai", "open ai":
		return "openai", true
	case "other", "other (openai-compatible)", "openai-compatible", "custom":
		return "other", true
	default:
		return "", false
	}
}
