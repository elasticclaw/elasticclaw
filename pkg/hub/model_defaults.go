package hub

import "strings"

const defaultCodexModel = "openai/gpt-5.6-sol"

func canonicalModelProvider(provider string) string {
	if provider == "codex" {
		return "openai"
	}
	return provider
}

func modelMatchesProvider(provider, model string) bool {
	canonicalProvider := canonicalModelProvider(provider)
	if strings.HasPrefix(model, canonicalProvider+"/") {
		return true
	}
	return provider == "codex" && strings.HasPrefix(model, "codex/")
}

func normalizeModelForProvider(provider, model string) string {
	canonicalProvider := canonicalModelProvider(provider)
	if strings.HasPrefix(model, canonicalProvider+"/") {
		return model
	}
	if provider == "codex" && strings.HasPrefix(model, "codex/") {
		return canonicalProvider + "/" + strings.TrimPrefix(model, "codex/")
	}
	return canonicalProvider + "/" + model
}
