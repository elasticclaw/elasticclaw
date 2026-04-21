package config

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

// parseLLMKeys is a helper that unmarshals a hub.yaml snippet and returns the LLMKeys.
func parseLLMKeys(t *testing.T, yamlStr string) types.LLMKeysList {
	t.Helper()
	cfg := &types.HubConfig{}
	if err := yaml.Unmarshal([]byte(yamlStr), cfg); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}
	return cfg.LLMKeys
}

func TestLLMKeysList_OldFormat_DeterministicDefault(t *testing.T) {
	// Old format YAML with multiple keys
	keys := parseLLMKeys(t, `
llm_keys:
  fireworks: sk-fw-123
  anthropic: sk-ant-456
  openai: sk-openai-789
`)

	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}

	// First key in alphabetical order (anthropic) should be default
	if keys[0].Name != "anthropic" {
		t.Errorf("expected first key to be 'anthropic', got '%s'", keys[0].Name)
	}
	if !keys[0].Default {
		t.Error("first key should be marked as default")
	}

	// Other keys should not be default
	for i := 1; i < len(keys); i++ {
		if keys[i].Default {
			t.Errorf("key %d (%s) should not be default", i, keys[i].Name)
		}
	}

	// Verify deterministic ordering (alphabetical)
	expected := []string{"anthropic", "fireworks", "openai"}
	for i, k := range keys {
		if k.Name != expected[i] {
			t.Errorf("key %d: expected %s, got %s", i, expected[i], k.Name)
		}
		if k.Provider != expected[i] {
			t.Errorf("key %d: expected provider %s, got %s", i, expected[i], k.Provider)
		}
	}
}

func TestLLMKeysList_NewFormat(t *testing.T) {
	keys := parseLLMKeys(t, `
llm_keys:
  - name: anthropic-prod
    provider: anthropic
    api_key: sk-ant-456
    default: true
  - name: fireworks-prod
    provider: fireworks
    api_key: sk-fw-123
`)

	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	if keys[0].Name != "anthropic-prod" {
		t.Errorf("expected first key to be 'anthropic-prod', got '%s'", keys[0].Name)
	}
	if !keys[0].Default {
		t.Error("first key should be default")
	}
	if keys[1].Name != "fireworks-prod" {
		t.Errorf("expected second key to be 'fireworks-prod', got '%s'", keys[1].Name)
	}
}

func TestLLMKeysList_OldFormat_HandlesEmptyKeys(t *testing.T) {
	keys := parseLLMKeys(t, `
llm_keys:
  anthropic: sk-ant-456
  fireworks: ""
  openai: sk-openai-789
`)

	// Should only have 2 keys (empty fireworks skipped)
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}

	// Keys should be anthropic and openai in alphabetical order
	if keys[0].Name != "anthropic" || keys[1].Name != "openai" {
		t.Errorf("expected anthropic and openai, got %s and %s", keys[0].Name, keys[1].Name)
	}
}

func TestLLMKeysList_Empty(t *testing.T) {
	keys := parseLLMKeys(t, `url: https://example.com`)
	if len(keys) != 0 {
		t.Errorf("expected 0 keys when llm_keys absent, got %d", len(keys))
	}
}
