package browsing

import (
	"context"
	"errors"

	"github.com/defos-ai/commander-browser-agent/browser/intelligence"
	"github.com/defos-ai/commander-browser-agent/internal/hermes"
	"github.com/defos-ai/commander/framework/agent/action"
)

// PrepareHermes shares the session's non-secret setup cache and managed-key
// resolution. The caller fills execution objectives/limits and clears APIKey
// after the worker has joined its use. No model or browser is started here.
func (s *Setup) PrepareHermes(ctx context.Context) (hermes.Request, error) {
	selection, key, err := s.prepare(ctx, selectHermesModel, acceptHermesModel)
	if err != nil {
		clear(key)
		return hermes.Request{}, err
	}
	return hermes.Request{Provider: selection.Provider, Model: selection.Name, Endpoint: selection.URL, APIKey: key}, nil
}

func selectHermesModel(ctx context.Context) (intelligence.ModelConfig, error) {
	question, err := action.Question(ctx, "browser_model_provider", "Which model provider should this browser session use? Select a provider, not an API key.",
		"OpenAI", "Other (OpenAI-compatible)")
	if err != nil {
		return intelligence.ModelConfig{}, err
	}
	provider, ok := providerID(question.Answer)
	if !ok || provider != "openai" && provider != "other" {
		return intelligence.ModelConfig{}, errors.New("Hermes requires OpenAI or an explicitly selected OpenAI-compatible provider")
	}
	return selectModelDetails(ctx, provider)
}

func acceptHermesModel(selection intelligence.ModelConfig) error {
	if selection.Provider != "openai" && selection.Provider != "other" {
		return errors.New("selected model provider has no verified Hermes wire profile")
	}
	if intelligence.ValidateModelConfig(selection) != nil {
		return errors.New("selected Hermes model configuration is invalid")
	}
	if selection.Provider == "openai" {
		preset, ok := intelligence.Provider("openai")
		if !ok || selection.URL != preset.URL {
			return errors.New("Hermes model credential destination does not match its provider")
		}
	}
	return nil
}
