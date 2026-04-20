package config

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestMigrateOldLLMKeys_DeterministicDefault(t *testing.T) {
	// Old format YAML with multiple keys
	oldYAML := []byte(`
llm_keys:
  fireworks: sk-fw-123
  anthropic: sk-ant-456
  openai: sk-openai-789
`)

	cfg := &types.HubConfig{}
	err := migrateOldLLMKeys(cfg, oldYAML)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	if len(cfg.LLMKeys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(cfg.LLMKeys))
	}

	// First key in alphabetical order (anthropic) should be default
	if cfg.LLMKeys[0].Name != "anthropic" {
		t.Errorf("expected first key to be 'anthropic', got '%s'", cfg.LLMKeys[0].Name)
	}
	if !cfg.LLMKeys[0].Default {
		t.Error("first key should be marked as default")
	}

	// Other keys should not be default
	for i := 1; i < len(cfg.LLMKeys); i++ {
		if cfg.LLMKeys[i].Default {
			t.Errorf("key %d (%s) should not be default", i, cfg.LLMKeys[i].Name)
		}
	}

	// Verify deterministic ordering (alphabetical)
	expected := []string{"anthropic", "fireworks", "openai"}
	for i, k := range cfg.LLMKeys {
		if k.Name != expected[i] {
			t.Errorf("key %d: expected %s, got %s", i, expected[i], k.Name)
		}
		if k.Provider != expected[i] {
			t.Errorf("key %d: expected provider %s, got %s", i, expected[i], k.Provider)
		}
	}
}

func TestMigrateOldLLMKeys_SkipsIfNewFormatPresent(t *testing.T) {
	oldYAML := []byte(`
llm_keys:
  anthropic: sk-ant-456
`)

	cfg := &types.HubConfig{
		LLMKeys: []*types.LLMKeyConfig{
			{Name: "existing", Provider: "openai", APIKey: "sk-existing", Default: true},
		},
	}

	err := migrateOldLLMKeys(cfg, oldYAML)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Should not have changed
	if len(cfg.LLMKeys) != 1 {
		t.Errorf("expected 1 key (unchanged), got %d", len(cfg.LLMKeys))
	}
	if cfg.LLMKeys[0].Name != "existing" {
		t.Errorf("existing key should be unchanged")
	}
}

func TestMigrateOldLLMKeys_HandlesEmptyKeys(t *testing.T) {
	oldYAML := []byte(`
llm_keys:
  anthropic: sk-ant-456
  fireworks: ""
  openai: sk-openai-789
`)

	cfg := &types.HubConfig{}
	err := migrateOldLLMKeys(cfg, oldYAML)
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}

	// Should only have 2 keys (empty fireworks skipped)
	if len(cfg.LLMKeys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(cfg.LLMKeys))
	}

	// Keys should be anthropic and openai in alphabetical order
	if cfg.LLMKeys[0].Name != "anthropic" || cfg.LLMKeys[1].Name != "openai" {
		t.Errorf("expected anthropic and openai, got %s and %s", cfg.LLMKeys[0].Name, cfg.LLMKeys[1].Name)
	}
}
