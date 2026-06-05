package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestSelectAIConfigProviderSupportsOllamaDefaultModel(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "openai-main", Provider: "openai", APIKey: "sk-test", Default: true},
		{Name: "ollama-main", Provider: "ollama", APIKey: "ollama-local"},
	}

	choice, err := selectAIConfigProvider(keys, "ollama/qwen2.5-coder:7b")
	if err != nil {
		t.Fatalf("select provider: %v", err)
	}

	if choice.Anthropic {
		t.Fatal("ollama should use OpenAI-compatible routing, not Anthropic")
	}
	if choice.Key.Provider != "ollama" {
		t.Fatalf("provider = %q, want ollama", choice.Key.Provider)
	}
	if choice.Provider.BaseURL != "http://ollama:11434/v1" {
		t.Fatalf("base URL = %q, want Ollama OpenAI-compatible URL", choice.Provider.BaseURL)
	}
	if choice.Model != "qwen2.5-coder:7b" {
		t.Fatalf("model = %q, want qwen2.5-coder:7b", choice.Model)
	}
}

func TestSelectAIConfigProviderFallsBackToExternalPriority(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "ollama-main", Provider: "ollama", APIKey: "ollama-local", Default: true},
		{Name: "openai-main", Provider: "openai", APIKey: "sk-test"},
	}

	choice, err := selectAIConfigProvider(keys, "")
	if err != nil {
		t.Fatalf("select provider: %v", err)
	}

	if choice.Key.Provider != "openai" {
		t.Fatalf("provider = %q, want openai priority before ollama", choice.Key.Provider)
	}
	if choice.Model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", choice.Model)
	}
}

func TestSelectAIConfigProviderUsesOllamaWhenOnlySupportedKey(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "ollama-main", Provider: "ollama", APIKey: "ollama-local", Default: true},
	}

	choice, err := selectAIConfigProvider(keys, "")
	if err != nil {
		t.Fatalf("select provider: %v", err)
	}

	if choice.Key.Provider != "ollama" || choice.Model != "qwen2.5-coder:1.5b" {
		t.Fatalf("choice = provider %q model %q, want ollama/qwen2.5-coder:1.5b", choice.Key.Provider, choice.Model)
	}
}

func TestSelectAIConfigProviderAllowsBlankOllamaAPIKey(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "ollama-main", Provider: "ollama", Default: true},
	}

	choice, err := selectAIConfigProvider(keys, "")
	if err != nil {
		t.Fatalf("select provider: %v", err)
	}

	if choice.Key.Provider != "ollama" || choice.Model != "qwen2.5-coder:1.5b" {
		t.Fatalf("choice = provider %q model %q, want ollama/qwen2.5-coder:1.5b", choice.Key.Provider, choice.Model)
	}
}

func TestSelectAIConfigProviderUsesAnthropicDefaultModel(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "anthropic-main", Provider: "anthropic", APIKey: "sk-ant-test", Default: true, DefaultModel: "claude-opus-4-1"},
	}

	choice, err := selectAIConfigProvider(keys, "anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("select provider: %v", err)
	}
	if !choice.Anthropic {
		t.Fatal("expected Anthropic provider")
	}
	if choice.Model != "claude-opus-4-1" {
		t.Fatalf("model = %q, want claude-opus-4-1", choice.Model)
	}
}

func TestSelectAIConfigProviderPrefersDefaultKeyForProvider(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "openai-first", Provider: "openai", APIKey: "sk-first", DefaultModel: "gpt-first"},
		{Name: "openai-default", Provider: "openai", APIKey: "sk-default", Default: true, DefaultModel: "gpt-default"},
	}

	choice, err := selectAIConfigProvider(keys, "openai/gpt-5.5")
	if err != nil {
		t.Fatalf("select provider: %v", err)
	}
	if choice.Key.Name != "openai-default" {
		t.Fatalf("key = %q, want openai-default", choice.Key.Name)
	}
	if choice.Model != "gpt-default" {
		t.Fatalf("model = %q, want gpt-default", choice.Model)
	}
}

func TestSelectAIConfigProviderRejectsBlankExternalAPIKey(t *testing.T) {
	keys := types.LLMKeysList{
		{Name: "openai-main", Provider: "openai", Default: true},
	}

	if _, err := selectAIConfigProvider(keys, ""); err == nil {
		t.Fatal("expected blank external API key to be rejected")
	}
}
