package hub

// ModelInfo describes a single LLM model available for a provider.
type ModelInfo struct {
	ID   string `json:"id"`   // Full model ID, e.g. "anthropic/claude-sonnet-4-6"
	Name string `json:"name"` // Human-readable name, e.g. "Claude Sonnet 4.6"
	API  string `json:"api,omitempty"` // API type, e.g. "anthropic-messages", "openai-completions"
}

// ProviderModels maps a provider name to its available models.
var ProviderModels = map[string][]ModelInfo{
	"anthropic": {
		{ID: "anthropic/claude-sonnet-4-6", Name: "Claude Sonnet 4.6", API: "anthropic-messages"},
		{ID: "anthropic/claude-opus-4-5", Name: "Claude Opus 4.5", API: "anthropic-messages"},
		{ID: "anthropic/claude-sonnet-4-5", Name: "Claude Sonnet 4.5", API: "anthropic-messages"},
	},
	"fireworks": {
		{ID: "fireworks/accounts/fireworks/models/kimi-k2p6", Name: "Kimi K2", API: "openai-completions"},
		{ID: "fireworks/accounts/fireworks/models/llama-v3p3-70b-instruct", Name: "Llama 3.3 70B", API: "openai-completions"},
		{ID: "fireworks/accounts/fireworks/models/deepseek-v3", Name: "DeepSeek V3", API: "openai-completions"},
	},
	"openai": {
		{ID: "openai/gpt-4o", Name: "GPT-4o", API: "openai-completions"},
		{ID: "openai/gpt-4o-mini", Name: "GPT-4o Mini", API: "openai-completions"},
	},
	"groq": {
		{ID: "groq/llama-3.3-70b-versatile", Name: "Llama 3.3 70B", API: "openai-completions"},
	},
	"deepseek": {
		{ID: "deepseek/deepseek-chat", Name: "DeepSeek Chat", API: "openai-completions"},
	},
}

// ProviderInfo describes a configured LLM provider for the models API.
type ProviderInfo struct {
	Name   string      `json:"name"`   // Provider identifier, e.g. "anthropic"
	Label  string      `json:"label"`  // Human-readable label, e.g. "Anthropic"
	Models []ModelInfo `json:"models"` // Available models for this provider
}

// ModelsResponse is the JSON response for GET /api/models.
type ModelsResponse struct {
	Providers []ProviderInfo `json:"providers"`
}

// providerLabels maps provider IDs to human-readable labels.
var providerLabels = map[string]string{
	"anthropic":  "Anthropic",
	"fireworks":  "Fireworks",
	"openai":     "OpenAI",
	"groq":       "Groq",
	"deepseek":   "DeepSeek",
	"moonshot":   "Moonshot",
}

// getProviderLabel returns the human-readable label for a provider, or the ID if unknown.
func getProviderLabel(name string) string {
	if label, ok := providerLabels[name]; ok {
		return label
	}
	return name
}

// buildModelsResponse returns a ModelsResponse with all configured providers
// that have models defined. If no keys are configured, returns all known providers.
func buildModelsResponse(configuredProviders []string) ModelsResponse {
	if len(configuredProviders) == 0 {
		// Return all known providers
		configuredProviders = make([]string, 0, len(ProviderModels))
		for name := range ProviderModels {
			configuredProviders = append(configuredProviders, name)
		}
	}

	resp := ModelsResponse{Providers: make([]ProviderInfo, 0, len(configuredProviders))}
	for _, name := range configuredProviders {
		models, ok := ProviderModels[name]
		if !ok {
			// Unknown provider — include with empty model list so client can show it
			models = []ModelInfo{}
		}
		resp.Providers = append(resp.Providers, ProviderInfo{
			Name:   name,
			Label:  getProviderLabel(name),
			Models: models,
		})
	}
	return resp
}
