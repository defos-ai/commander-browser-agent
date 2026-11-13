package browsing

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const openAIEndpoint = "https://api.openai.com/v1/chat/completions"
const openAISecretName = "OPENAI_API_KEY"

// modelConfig holds only the operator's non-secret provider/model selection.
// It does not create a model client or implement a reasoning loop.
type modelConfig struct {
	Provider string
	URL      string
	Name     string
}

func validateModelConfig(config modelConfig) error {
	if config.Provider != "openai" && config.Provider != "other" {
		return errors.New("model provider is not supported")
	}
	if err := validateModelURL(config.URL); err != nil {
		return err
	}
	return validateModelName(config.Name)
}

// validateModelURL checks the exact endpoint without contacting it or exposing
// its value. Custom credentials remain bound to this unmodified validated URL.
func validateModelURL(raw string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || len(raw) == 0 || len(raw) > 2048 ||
		strings.TrimSpace(raw) != raw || endpoint.Hostname() == "" ||
		endpoint.Opaque != "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.ForceQuery || strings.Contains(raw, "#") || strings.HasSuffix(endpoint.Host, ":") {
		return errors.New("model endpoint must be an absolute HTTPS URL or HTTP loopback URL without credentials, query or fragment")
	}
	if endpoint.Scheme != "https" {
		address := net.ParseIP(endpoint.Hostname())
		if endpoint.Scheme != "http" || !strings.EqualFold(endpoint.Hostname(), "localhost") && (address == nil || !address.IsLoopback()) {
			return errors.New("model endpoint must use HTTPS or an explicit HTTP loopback address")
		}
	}
	if endpoint.Port() != "" {
		port, err := strconv.Atoi(endpoint.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("model endpoint port is invalid")
		}
	}
	return nil
}

func validateModelName(name string) error {
	if !utf8.ValidString(name) || len(name) == 0 || len(name) > 200 ||
		strings.TrimSpace(name) != name || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("model name must contain 1 to 200 bytes without surrounding whitespace or control characters")
	}
	return nil
}
