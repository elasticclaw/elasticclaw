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
		"scripts/utils/helper.py": false,
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

// ─── Consolidated hub boot loader tests ───────────────────────────────────────

// writeHubYAML writes a hub.yaml into a temp dir and points
// ELASTICCLAW_HUB_CONFIG at it. Returns the path.
func writeHubYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write hub.yaml: %v", err)
	}
	t.Setenv("ELASTICCLAW_HUB_CONFIG", path)
	return path
}

// TestHubBootPrecedenceMatrix covers the documented precedence:
// flag > env (ELASTICCLAW_*) > hub.yaml > default.
func TestHubBootPrecedenceMatrix(t *testing.T) {
	yamlWithSettings := "listen_addr: \":7001\"\ndb_path: /yaml/hub.db\nlog_level: warn\n"

	cases := []struct {
		name      string
		yaml      string
		env       map[string]string
		flags     HubBootFlags
		wantAddr  string
		wantDB    string
		wantLevel string
	}{
		{
			name:      "defaults when nothing is set",
			yaml:      "",
			wantAddr:  DefaultHubAddr,
			wantDB:    "",
			wantLevel: DefaultHubLogLevel,
		},
		{
			name:      "yaml beats default",
			yaml:      yamlWithSettings,
			wantAddr:  ":7001",
			wantDB:    "/yaml/hub.db",
			wantLevel: "warn",
		},
		{
			name: "env beats yaml",
			yaml: yamlWithSettings,
			env: map[string]string{
				EnvHubAddr:     ":7002",
				EnvHubDBPath:   "/env/hub.db",
				EnvHubLogLevel: "error",
			},
			wantAddr:  ":7002",
			wantDB:    "/env/hub.db",
			wantLevel: "error",
		},
		{
			name: "flag beats env and yaml",
			yaml: yamlWithSettings,
			env: map[string]string{
				EnvHubAddr:     ":7002",
				EnvHubDBPath:   "/env/hub.db",
				EnvHubLogLevel: "error",
			},
			flags:     HubBootFlags{Addr: ":7003", DBPath: "/flag/hub.db", LogLevel: "debug"},
			wantAddr:  ":7003",
			wantDB:    "/flag/hub.db",
			wantLevel: "debug",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeHubYAML(t, tc.yaml)
			// Ensure a clean env for the vars under test.
			for _, key := range []string{EnvHubAddr, EnvHubDBPath, EnvHubLogLevel, EnvHubAllowedOrigins} {
				t.Setenv(key, "")
				os.Unsetenv(key)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			boot, err := LoadHubBootConfig(tc.flags)
			if err != nil {
				t.Fatalf("LoadHubBootConfig: %v", err)
			}
			if boot.Addr != tc.wantAddr {
				t.Errorf("Addr = %q, want %q", boot.Addr, tc.wantAddr)
			}
			if boot.DBPath != tc.wantDB {
				t.Errorf("DBPath = %q, want %q", boot.DBPath, tc.wantDB)
			}
			if boot.LogLevel != tc.wantLevel {
				t.Errorf("LogLevel = %q, want %q", boot.LogLevel, tc.wantLevel)
			}
		})
	}
}

func TestHubBootAllowedOriginsPrecedence(t *testing.T) {
	writeHubYAML(t, "allowed_origins:\n  - https://yaml.example.com\n")

	t.Setenv(EnvHubAllowedOrigins, "")
	os.Unsetenv(EnvHubAllowedOrigins)
	boot, err := LoadHubBootConfig(HubBootFlags{})
	if err != nil {
		t.Fatalf("LoadHubBootConfig: %v", err)
	}
	if len(boot.AllowedOrigins) != 1 || boot.AllowedOrigins[0] != "https://yaml.example.com" {
		t.Errorf("yaml origins = %v", boot.AllowedOrigins)
	}

	t.Setenv(EnvHubAllowedOrigins, "https://a.example.com, https://b.example.com")
	boot, err = LoadHubBootConfig(HubBootFlags{})
	if err != nil {
		t.Fatalf("LoadHubBootConfig: %v", err)
	}
	if len(boot.AllowedOrigins) != 2 || boot.AllowedOrigins[0] != "https://a.example.com" || boot.AllowedOrigins[1] != "https://b.example.com" {
		t.Errorf("env origins = %v, want env to beat yaml", boot.AllowedOrigins)
	}
}

func TestHubBootValidation(t *testing.T) {
	writeHubYAML(t, "")

	if _, err := LoadHubBootConfig(HubBootFlags{LogLevel: "loud"}); err == nil {
		t.Error("expected error for invalid log level")
	}
	if _, err := LoadHubBootConfig(HubBootFlags{Addr: "8080"}); err == nil {
		t.Error("expected error for address without a colon")
	}
	if _, err := LoadHubBootConfig(HubBootFlags{Addr: "127.0.0.1:8080", LogLevel: "WARN"}); err != nil {
		t.Errorf("expected valid boot config, got %v", err)
	}
}

func TestParseLogLevel(t *testing.T) {
	for input, want := range map[string]string{
		"":      "INFO",
		"debug": "DEBUG",
		"info":  "INFO",
		"warn":  "WARN",
		"error": "ERROR",
		"WARN":  "WARN",
	} {
		lvl, err := ParseLogLevel(input)
		if err != nil {
			t.Errorf("ParseLogLevel(%q): %v", input, err)
			continue
		}
		if lvl.String() != want {
			t.Errorf("ParseLogLevel(%q) = %s, want %s", input, lvl, want)
		}
	}
	if _, err := ParseLogLevel("verbose"); err == nil {
		t.Error("expected error for invalid level")
	}
}

func TestApplyHotReloadSafeSubset(t *testing.T) {
	current := &types.HubConfig{
		Token:          "boot-token",
		ListenAddr:     ":8080",
		DBPath:         "/var/lib/elasticclaw/hub.db",
		LogLevel:       "info",
		AllowedOrigins: []string{"https://old.example.com"},
	}
	fresh := &types.HubConfig{
		Token:          "edited-token",
		ListenAddr:     ":9999",           // restart required — must be rejected
		DBPath:         "/tmp/other.db",   // restart required — must be rejected
		LogLevel:       "debug",           // hot-reloadable
		AllowedOrigins: []string{"https://new.example.com"}, // hot-reloadable
		Branding:       &types.BrandingConfig{AppName: "NewBrand"},
		Integrations:   &types.IntegrationsConfig{},
	}

	merged, applied, rejected := ApplyHotReload(current, fresh)

	if merged.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", merged.LogLevel)
	}
	if len(merged.AllowedOrigins) != 1 || merged.AllowedOrigins[0] != "https://new.example.com" {
		t.Errorf("AllowedOrigins = %v", merged.AllowedOrigins)
	}
	if merged.Branding == nil || merged.Branding.AppName != "NewBrand" {
		t.Errorf("Branding not applied: %+v", merged.Branding)
	}
	if merged.Integrations == nil {
		t.Error("Integrations not applied")
	}
	// Restart-required fields keep the running values.
	if merged.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080 (restart required)", merged.ListenAddr)
	}
	if merged.DBPath != "/var/lib/elasticclaw/hub.db" {
		t.Errorf("DBPath = %q, want running value (restart required)", merged.DBPath)
	}
	// Fields outside the safe subset are not touched by SIGHUP reload.
	if merged.Token != "boot-token" {
		t.Errorf("Token = %q, want running value", merged.Token)
	}

	wantApplied := map[string]bool{"log_level": true, "allowed_origins": true, "branding": true, "integrations": true}
	if len(applied) != len(wantApplied) {
		t.Errorf("applied = %v", applied)
	}
	for _, f := range applied {
		if !wantApplied[f] {
			t.Errorf("unexpected applied field %q", f)
		}
	}
	wantRejected := map[string]bool{"listen_addr": true, "db_path": true}
	if len(rejected) != len(wantRejected) {
		t.Errorf("rejected = %v", rejected)
	}
	for _, f := range rejected {
		if !wantRejected[f] {
			t.Errorf("unexpected rejected field %q", f)
		}
	}

	// The inputs must not be mutated.
	if current.LogLevel != "info" {
		t.Error("ApplyHotReload mutated the current config")
	}
}

func TestApplyHotReloadNoChanges(t *testing.T) {
	current := &types.HubConfig{LogLevel: "info", Branding: &types.BrandingConfig{AppName: "X"}}
	fresh := &types.HubConfig{LogLevel: "info", Branding: &types.BrandingConfig{AppName: "X"}}
	_, applied, rejected := ApplyHotReload(current, fresh)
	if len(applied) != 0 || len(rejected) != 0 {
		t.Errorf("applied = %v, rejected = %v, want none", applied, rejected)
	}
}
