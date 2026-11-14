package commander

import (
	"context"
	"errors"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander/framework/agent/action"
)

// PrepareModel resolves the session's non-secret choices and current managed key.
// The caller clears APIKey after the capability has joined its use. This starts
// no model connection, native agent or browser.
func (s *Setup) PrepareModel(ctx context.Context) (contracts.ModelConfig, error) {
	selection, key, err := s.prepare(ctx)
	if err != nil {
		clear(key)
		return contracts.ModelConfig{}, err
	}
	return contracts.ModelConfig{Provider: selection.Provider, Model: selection.Name, Endpoint: selection.URL, APIKey: key}, nil
}

func selectHermesModel(ctx context.Context) (modelConfig, error) {
	question, err := action.Question(ctx, "browser_model_provider", "Which model provider should this browser session use? Select a provider, not an API key.",
		"OpenAI", "Other (OpenAI-compatible)")
	if err != nil {
		return modelConfig{}, err
	}
	provider, ok := providerID(question.Answer)
	if !ok || provider != "openai" && provider != "other" {
		return modelConfig{}, errors.New("Hermes requires OpenAI or an explicitly selected OpenAI-compatible provider")
	}
	return selectModelDetails(ctx, provider)
}

func acceptHermesModel(selection modelConfig) error {
	if selection.Provider != "openai" && selection.Provider != "other" {
		return errors.New("selected model provider has no verified Hermes wire profile")
	}
	if validateModelConfig(selection) != nil {
		return errors.New("selected Hermes model configuration is invalid")
	}
	if selection.Provider == "openai" {
		if selection.URL != openAIEndpoint {
			return errors.New("Hermes model credential destination does not match its provider")
		}
	}
	return nil
}
