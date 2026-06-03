package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

func TestSaveHubConfigUsesExplicitEnvPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom", "hub.yaml")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)

	if err := SaveHubConfig(&types.HubConfig{Token: "test-token"}); err != nil {
		t.Fatalf("SaveHubConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env hub config: %v", err)
	}
	if !strings.Contains(string(data), "token: test-token") {
		t.Fatalf("saved config = %s", string(data))
	}
}

func TestLegacyProfilesRejectTraversalNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"../config", "a/b", ".."} {
		if _, err := LoadProfile(name); err == nil {
			t.Fatalf("LoadProfile(%q) succeeded, want error", name)
		}
		if err := SaveProfile(&types.Profile{Name: name}); err == nil {
			t.Fatalf("SaveProfile(%q) succeeded, want error", name)
		}
		if err := DeleteProfile(name); err == nil {
			t.Fatalf("DeleteProfile(%q) succeeded, want error", name)
		}
	}
}

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

func TestReadTemplateFilesIncludesScripts(t *testing.T) {
	dir := t.TempDir()

	// Write elasticclaw-config.yaml (required for template)
	configYAML := `
provider: replicated
`
	if err := os.WriteFile(filepath.Join(dir, "elasticclaw-config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatalf("write elasticclaw-config.yaml: %v", err)
	}

	// Write a regular file (SOUL.md is a known template file)
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("# Test"), 0644); err != nil {
		t.Fatalf("write SOUL.md: %v", err)
	}

	// Write scripts directory with files
	scriptsDir := filepath.Join(dir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "analyze.py"), []byte("print('hello')"), 0644); err != nil {
		t.Fatalf("write analyze.py: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "deploy.sh"), []byte("#!/bin/bash\necho 'deploying'"), 0644); err != nil {
		t.Fatalf("write deploy.sh: %v", err)
	}

	// Write nested file in scripts
	subDir := filepath.Join(scriptsDir, "utils")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("mkdir utils: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "helper.py"), []byte("# helper"), 0644); err != nil {
		t.Fatalf("write helper.py: %v", err)
	}

	files, err := ReadTemplateFiles(dir)
	if err != nil {
		t.Fatalf("ReadTemplateFiles: %v", err)
	}

	// Should include known files and all scripts
	expected := map[string]bool{
		"elasticclaw-config.yaml": false,
		"SOUL.md":                 false,
		"scripts/analyze.py":      false,
		"scripts/deploy.sh":       false,
	}

	for path := range files {
		if _, ok := expected[path]; ok {
			expected[path] = true
		} else {
			t.Errorf("unexpected file: %s", path)
		}
	}

	for path, found := range expected {
		if !found {
			t.Errorf("expected file not found: %s", path)
		}
	}
}
