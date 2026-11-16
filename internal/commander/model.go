package commander

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
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

func requestModelConfig(ctx context.Context) (modelConfig, error) {
	provider, err := modelSetting(ctx, ModelProviderSecretName, true)
	if err != nil {
		return modelConfig{}, err
	}
	if provider != "openai" && provider != "other" {
		return modelConfig{}, errors.New("BROWSER_MODEL_PROVIDER must be openai or other")
	}
	model, err := modelSetting(ctx, ModelNameSecretName, true)
	if err != nil {
		return modelConfig{}, err
	}
	if validateModelName(model) != nil {
		return modelConfig{}, errors.New("BROWSER_MODEL_NAME must contain a valid exact model ID/version")
	}
	selection := modelConfig{Provider: provider, Name: model, URL: openAIEndpoint}
	if provider == "other" {
		selection.URL, err = modelSetting(ctx, ModelURLSecretName, false)
		if err != nil {
			return modelConfig{}, err
		}
		if validateModelURL(selection.URL) != nil {
			return modelConfig{}, errors.New("BROWSER_MODEL_URL must be an exact HTTPS or loopback HTTP completion endpoint without credentials, query or fragment")
		}
	}
	return selection, nil
}

func modelSetting(ctx context.Context, name string, allowEnvironment bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if allowEnvironment {
		local := environment.Local{}
		if value, ok := local.LookupEnv(name); ok && value != "" {
			return value, nil
		}
	}
	raw, err := action.RequestReusableSecret(ctx, name)
	defer clear(raw)
	if err != nil {
		return "", err
	}
	if len(raw) > 8<<10 || !utf8.Valid(raw) {
		return "", errors.New("browser model setting must contain a bounded UTF-8 string")
	}
	return string(raw), nil
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
