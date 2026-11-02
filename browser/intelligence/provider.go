package intelligence

// ProviderPreset describes a supported provider's exact Chat Completions
// endpoint and managed-secret name. It contains no model defaults or keys.
type ProviderPreset struct {
	ID         string
	Name       string
	URL        string
	SecretName string
}

// Providers returns fresh presets. Callers must bind standard provider keys to
// these endpoints before requesting credentials. Other requires an explicitly
// configured endpoint and credentials bound to that endpoint.
func Providers() []ProviderPreset {
	return []ProviderPreset{
		{ID: "openai", Name: "OpenAI", URL: "https://api.openai.com/v1/chat/completions", SecretName: "OPENAI_API_KEY"},
		{ID: "anthropic", Name: "Anthropic", URL: "https://api.anthropic.com/v1/chat/completions", SecretName: "ANTHROPIC_API_KEY"},
		{ID: "gemini", Name: "Google Gemini", URL: "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", SecretName: "GEMINI_API_KEY"},
		{ID: "xai", Name: "xAI", URL: "https://api.x.ai/v1/chat/completions", SecretName: "XAI_API_KEY"},
		{ID: "deepseek", Name: "DeepSeek", URL: "https://api.deepseek.com/v1/chat/completions", SecretName: "DEEPSEEK_API_KEY"},
		{ID: "other", Name: "Other (OpenAI-compatible)"},
	}
}

func Provider(id string) (ProviderPreset, bool) {
	for _, provider := range Providers() {
		if provider.ID == id {
			return provider, true
		}
	}
	return ProviderPreset{}, false
}

type modelWireProfile struct {
	tokenField   string
	toolChoice   string
	parallelFlag bool
	storeFlag    bool
	allowText    bool
}

func wireProfile(provider string) modelWireProfile {
	switch provider {
	case "anthropic":
		// The compatibility layer supports these fields, but some models
		// reject forced tool use. Unsupported store is deliberately omitted.
		// https://platform.claude.com/docs/en/cli-sdks-libraries/libraries/openai-sdk
		return modelWireProfile{tokenField: "max_completion_tokens", toolChoice: "auto", parallelFlag: true, allowText: true}
	case "gemini":
		// Google's compatibility endpoint accepts the common token limit.
		// Its documented tool interface has no parallel or storage flag here;
		// client validation still admits exactly one complete tool call.
		// https://aistudio.google.com/docs/openai
		return modelWireProfile{tokenField: "max_tokens", toolChoice: "auto", allowText: true}
	case "deepseek":
		// Thinking mode defaults on and does not accept forced tool choice.
		// Do not invent provider model defaults or override reasoning policy.
		// https://api-docs.deepseek.com/api/create-chat-completion/
		return modelWireProfile{tokenField: "max_tokens", toolChoice: "auto", allowText: true}
	case "xai":
		// xAI documents the completion allowance and parallel-call control,
		// but its Chat Completions reference has no storage switch.
		// https://docs.x.ai/developers/rest-api-reference/inference/chat-completions
		return modelWireProfile{tokenField: "max_completion_tokens", toolChoice: "required", parallelFlag: true}
	default: // Explicit OpenAI and the backwards-compatible Other profile.
		return modelWireProfile{tokenField: "max_completion_tokens", toolChoice: "required", parallelFlag: true, storeFlag: true}
	}
}
