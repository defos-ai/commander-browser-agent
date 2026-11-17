// Package commander adapts browser capabilities to Commander execution services.
package commander

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/domain/sessions"
)

const (
	// ModelProviderSecretName selects the provider through deployment settings or Commander.
	ModelProviderSecretName = "BROWSER_MODEL_PROVIDER"
	// ModelNameSecretName selects the exact model through deployment settings or Commander.
	ModelNameSecretName = "BROWSER_MODEL_NAME"
	// ModelURLSecretName stores the custom provider's exact managed endpoint.
	ModelURLSecretName = "BROWSER_MODEL_URL"
)

// Setup retains validated non-secret choices for each full Commander creation claim.
// Configure ValidateModel before use; construction starts no connection.
type Setup struct {
	ValidateModel contracts.ModelValidator

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
	validate := s.ValidateModel
	if validate == nil {
		return empty, nil, errors.New("browser model setup requires a model validator")
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
	if setup.selection != nil {
		selection := *setup.selection
		key, err := selectedModelKey(ctx, selection)
		if err != nil {
			clear(key)
			return empty, nil, err
		}
		if !validModelKey(key) {
			clear(key)
			return empty, nil, errors.New("native browser model credentials are invalid")
		}
		return selection, key, nil
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return empty, nil, err
		}
		selection, err := requestModelConfig(ctx, attempt)
		if err != nil {
			if ctx.Err() != nil {
				return empty, nil, ctx.Err()
			}
			if errors.Is(err, errModelSelectionInvalid) {
				continue
			}
			return empty, nil, err
		}
		key, err := selectedModelKey(ctx, selection)
		if err != nil {
			clear(key)
			return empty, nil, err
		}
		if !validModelKey(key) {
			clear(key)
			return empty, nil, errors.New("native browser model credentials are invalid")
		}
		candidate := contracts.ModelConfig{Provider: selection.Provider, Model: selection.Name, Endpoint: selection.URL, APIKey: key}
		err = validate(ctx, candidate)
		if ctx.Err() != nil {
			clear(key)
			return empty, nil, ctx.Err()
		}
		if err != nil {
			clear(key)
			if modelFailureOnly(err) == contracts.ErrModelRequestRejected {
				continue
			}
			return empty, nil, safeModelValidationError(err)
		}
		if err := seedModelSettings(ctx, selection); err != nil {
			clear(key)
			return empty, nil, err
		}
		if err := ctx.Err(); err != nil {
			clear(key)
			return empty, nil, err
		}
		// Only a validated tuple with matching managed receipts is retained.
		// A later action resolves its canonical credential again.
		setup.selection = &selection
		return selection, key, nil
	}
	return empty, nil, errors.New("browser model setup failed after three selection attempts")
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
