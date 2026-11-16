// Package commander adapts browser capabilities to Commander execution services.
package commander

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

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
		selection, err = requestModelConfig(ctx)
		if err != nil {
			return empty, nil, err
		}
	}
	if err := acceptHermesModel(selection); err != nil {
		return empty, nil, err
	}
	if setup.selection == nil {
		// Bind valid settings to this session even while its key is pending.
		// New sessions resolve the current settings again.
		setup.selection = &selection
	}
	key, err := selectedModelKey(ctx, selection)
	return selection, key, err
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
