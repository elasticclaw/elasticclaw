package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCheckLLMKeysRecognizesOllama(t *testing.T) {
	s := &Server{}
	checks := s.checkLLMKeys(&types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", APIKey: "ollama-local", Default: true},
		},
	})

	for _, check := range checks {
		if strings.Contains(check.Title, "Unknown LLM provider") {
			t.Fatalf("ollama was reported as unknown: %#v", check)
		}
	}
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("expected one passing LLM key check, got %#v", checks)
	}
}

func TestCheckLLMKeysAllowsBlankOllamaAPIKey(t *testing.T) {
	s := &Server{}
	checks := s.checkLLMKeys(&types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", Default: true},
		},
	})

	for _, check := range checks {
		if strings.Contains(check.Title, "has no API key") {
			t.Fatalf("blank ollama key was reported as invalid: %#v", check)
		}
	}
	if len(checks) != 1 || !checks[0].OK {
		t.Fatalf("expected one passing LLM key check, got %#v", checks)
	}
}
