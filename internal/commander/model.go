package commander

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/defos-ai/commander-browser-agent/browser/contracts"
	"github.com/defos-ai/commander-browser-agent/internal/sensitive"
	"github.com/defos-ai/commander/framework/agent/action"
	"github.com/defos-ai/commander/framework/agent/environment"
)

// PrepareModel validates fresh choices with a model-only request before seeding
// shared settings. Reuse resolves the current managed key without another probe.
// The caller clears APIKey after the capability has joined its use.
func (s *Setup) PrepareModel(ctx context.Context) (contracts.ModelConfig, error) {
	selection, key, err := s.prepare(ctx)
	if err != nil {
		clear(key)
		return contracts.ModelConfig{}, err
	}
	return contracts.ModelConfig{Provider: selection.Provider, Model: selection.Name, Endpoint: selection.URL, APIKey: key}, nil
}

var errModelSelectionInvalid = errors.New("browser model selection is invalid")

func requestModelConfig(ctx context.Context, attempt int) (modelConfig, error) {
	provider := initialModelSetting(ModelProviderSecretName, attempt)
	if provider == "" {
		question, err := action.Question(ctx, fmt.Sprintf("browser_model_provider_%d", attempt),
			"Which model provider should this browser session use? Select a provider, not an API key.", "OpenAI", "Other (OpenAI-compatible)")
		if err != nil {
			return modelConfig{}, err
		}
		switch strings.ToLower(strings.TrimSpace(question.Answer)) {
		case "openai", "open ai":
			provider = "openai"
		case "other", "other (openai-compatible)":
			provider = "other"
		}
	}
	if provider != "openai" && provider != "other" {
		return modelConfig{}, errModelSelectionInvalid
	}
	model := initialModelSetting(ModelNameSecretName, attempt)
	if model == "" {
		question, err := action.Question(ctx, fmt.Sprintf("browser_model_%d", attempt),
			"Which exact tool-calling model ID/version should this browser session use? Enter the model ID, not an API key.")
		if err != nil {
			return modelConfig{}, err
		}
		model = question.Answer
	}
	if validateModelName(model) != nil {
		return modelConfig{}, errModelSelectionInvalid
	}
	selection := modelConfig{Provider: provider, Name: model, URL: openAIEndpoint}
	if provider == "other" {
		question, err := action.Question(ctx, fmt.Sprintf("browser_model_url_%d", attempt),
			"What is the exact Chat Completions endpoint for this custom provider? Enter HTTPS, or loopback HTTP, without credentials, query parameters or fragment.")
		if err != nil {
			return modelConfig{}, err
		}
		selection.URL = question.Answer
	}
	if acceptHermesModel(selection) != nil {
		return modelConfig{}, errModelSelectionInvalid
	}
	return selection, nil
}

func initialModelSetting(name string, attempt int) string {
	if attempt != 1 {
		return ""
	}
	local := environment.Local{}
	value, _ := local.LookupEnv(name)
	return value
}

func validModelKey(key []byte) bool {
	if len(key) == 0 || len(key) > sensitive.MaxValueBytes {
		return false
	}
	for _, value := range key {
		if value < 0x21 || value > 0x7e {
			return false
		}
	}
	return true
}

func seedModelSettings(ctx context.Context, selection modelConfig) error {
	if err := seedModelSetting(ctx, ModelProviderSecretName, selection.Provider); err != nil {
		return err
	}
	if selection.Provider == "other" {
		if err := seedModelSetting(ctx, ModelURLSecretName, selection.URL); err != nil {
			return err
		}
	}
	return seedModelSetting(ctx, ModelNameSecretName, selection.Name)
}

func seedModelSetting(ctx context.Context, name, value string) error {
	seed := []byte(value)
	defer clear(seed)
	saved, err := action.RequestReusableSecret(ctx, name, action.SecretRequestOptions{Default: seed})
	defer clear(saved)
	if err != nil {
		return err
	}
	if !bytes.Equal(saved, seed) {
		return errors.New("browser model settings conflict with existing managed values; update the shared settings before retrying")
	}
	return nil
}

func safeModelValidationError(err error) error {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded, contracts.ErrModelCredentials,
		contracts.ErrModelRateLimited, contracts.ErrModelUnavailable, contracts.ErrModelResponse,
		contracts.ErrModelRequestPolicy, contracts.ErrModelRequestLimit} {
		if errors.Is(err, failure) {
			return failure
		}
	}
	return errors.New("browser model validation failed")
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
