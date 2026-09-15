package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestAgentBootstrapConfigExecutesBothProviderModels(t *testing.T) {
	for _, tc := range []struct{ name, provider, model string }{
		{"api", "openai", "openai/worker"},
		{"custom", "grok", "grok/worker"},
		{"local", "ollama", "ollama/worker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{
				{Name: "main", Provider: "anthropic", APIKey: "main-token"},
				{Name: "unused", Provider: tc.provider, APIKey: "wrong-token", Default: true},
				{Name: "worker", Provider: tc.provider, APIKey: "worker-token"},
			}}
			plan, err := buildAgentBootstrapPlan(cfg, "main", "anthropic/principal", &types.SubagentConfig{Model: tc.model, LLMKey: "worker", MaxConcurrent: 3})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plan.LLMKeyEnv, "wrong-token") || !strings.Contains(plan.LLMKeyEnv, "worker-token") {
				t.Fatalf("worker credential was not selected")
			}
			home := t.TempDir()
			cmd := exec.Command("bash", "-c", plan.ProviderConfig)
			cmd.Env = append(os.Environ(), "HOME="+home, "OPENCLAW_DEFAULT_MODEL=anthropic/principal")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("patch config: %v: %s", err, out)
			}
			raw, err := os.ReadFile(filepath.Join(home, ".openclaw/openclaw.json"))
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			defaults := config["agents"].(map[string]any)["defaults"].(map[string]any)
			if defaults["model"] != "anthropic/principal" {
				t.Fatal("principal model changed")
			}
			child := defaults["subagents"].(map[string]any)
			if child["model"] != tc.model || child["maxConcurrent"] != float64(3) {
				t.Fatalf("child config: %v", child)
			}
			if tc.provider == "grok" || tc.provider == "ollama" {
				providers := config["models"].(map[string]any)["providers"].(map[string]any)
				if providers[tc.provider] == nil {
					t.Fatalf("missing child provider catalog")
				}
			}
		})
	}
}

func TestAgentBootstrapRestoresWorkerOAuthWithPrincipalAPI(t *testing.T) {
	cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{
		{Name: "main", Provider: "anthropic", APIKey: "main-token"},
		{Name: "worker", Provider: "grok", AuthProfile: "worker-oauth"},
	}, ModelAuthProfiles: []*types.ModelAuthProfileConfig{
		{Name: "worker-oauth", Provider: "grok", AuthState: testGrokAuthState(t, "access", "refresh", time.Now().Add(time.Hour))},
	}}
	plan, err := buildAgentBootstrapPlan(cfg, "main", "anthropic/principal", &types.SubagentConfig{Model: "grok/worker", LLMKey: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cmd := exec.Command("bash", "-c", buildModelAuthRestoreShell(plan.ModelAuthEnv))
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".grok/auth.json")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.APIKeyAuthSync, "--provider anthropic") || !strings.Contains(plan.OAuthAuthSync, "--provider xai") {
		t.Fatal("both auth sync paths are required")
	}
	if !strings.Contains(plan.ProviderConfig, `['model'] = "xai/worker"`) {
		t.Fatal("OAuth child should select native xAI")
	}
	if strings.Contains(plan.ProviderConfig, "'maxConcurrent': 0") {
		t.Fatal("omitted concurrency must use OpenClaw default")
	}
}

func TestAgentBootstrapPrefersAPIKeyOverStaleOAuthProfile(t *testing.T) {
	cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{{Name: "main", Provider: "anthropic", APIKey: "key", AuthProfile: "missing-profile"}}}
	plan, err := buildAgentBootstrapPlan(cfg, "main", "anthropic/principal", &types.SubagentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ModelAuthEnv != "" {
		t.Fatal("API key selection must not restore OAuth credentials")
	}
}

func TestAgentBootstrapKeepsBothCustomProviderModels(t *testing.T) {
	cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{{Name: "grok", Provider: "grok", APIKey: "key"}}}
	plan, err := buildAgentBootstrapPlan(cfg, "grok", "grok/principal", &types.SubagentConfig{Model: "grok/worker"})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openclaw"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".openclaw/openclaw.json"), []byte(`{"agents":{"defaults":{"subagents":{"runTimeoutSeconds":120,"maxSpawnDepth":2}}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", "-c", plan.ProviderConfig)
	cmd.Env = append(os.Environ(), "HOME="+home, "OPENCLAW_DEFAULT_MODEL=grok/principal")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("patch: %v: %s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".openclaw/openclaw.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	models := config["models"].(map[string]any)["providers"].(map[string]any)["grok"].(map[string]any)["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("expected both custom model definitions, got %v", models)
	}
	defaults := config["agents"].(map[string]any)["defaults"].(map[string]any)
	child := defaults["subagents"].(map[string]any)
	if child["runTimeoutSeconds"] != float64(120) || child["maxSpawnDepth"] != float64(2) {
		t.Fatalf("unrelated child settings changed: %v", child)
	}

	if _, restricted := defaults["models"]; restricted {
		t.Fatal("native Grok models must remain unrestricted")
	}
	if _, ok := defaults["subagents"].(map[string]any)["maxConcurrent"]; ok {
		t.Fatal("omitted concurrency should preserve runtime default")
	}
}

func TestManagedGrokCredentialUsesWorkerProfile(t *testing.T) {
	cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{
		{Name: "main", Provider: "anthropic", APIKey: "key"},
		{Name: "worker", Provider: "grok", AuthProfile: "worker-oauth"},
	}, ModelAuthProfiles: []*types.ModelAuthProfileConfig{
		{Name: "worker-oauth", Provider: "grok", AuthState: testGrokAuthState(t, "worker-access", "worker-refresh", time.Now().Add(time.Hour))},
	}}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,llm_key,subagents_config,status,created_at) VALUES(?,?,?,?,?,?,datetime('now'))`, "worker-oauth-claw", "test-tenant-id", "worker oauth", "main", `{"model":"grok/worker","llm_key":"worker"}`, "connected"); err != nil {
		t.Fatal(err)
	}
	credential, err := s.managedGrokCredential(context.Background(), "worker-oauth-claw")
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil {
		t.Fatal("expected managed worker credential")
	}
}

func executeAgentConfigPatch(t *testing.T, script, model, existing string) map[string]any {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openclaw"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".openclaw/openclaw.json"), []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "HOME="+home, "OPENCLAW_DEFAULT_MODEL="+model)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config patch: %v: %s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".openclaw/openclaw.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestAgentBootstrapLegacyChildSettingsDoNotRequireModel(t *testing.T) {
	cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{{Name: "main", Provider: "anthropic", APIKey: "key"}}}
	plan, err := buildAgentBootstrapPlan(cfg, "main", "anthropic/principal", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range []string{`{"maxConcurrent":3}`, `{"model":{"primary":"grok/worker"}}`, `{"model":42}`, `null`, `"legacy"`, `{"model":""}`} {
		t.Run(child, func(t *testing.T) {
			config := executeAgentConfigPatch(t, plan.ProviderConfig, "anthropic/principal", `{"agents":{"defaults":{"subagents":`+child+`}}}`)
			defaults := config["agents"].(map[string]any)["defaults"].(map[string]any)
			got, _ := json.Marshal(defaults["subagents"])
			if string(got) != child {
				t.Fatalf("legacy settings changed: %s", got)
			}
		})
	}
}

func TestAgentBootstrapKeepsUnrelatedProviderCredentials(t *testing.T) {
	cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{
		{Name: "main", Provider: "openai", APIKey: "principal-key"},
		{Name: "worker-default", Provider: "grok", APIKey: "wrong-worker-key", Default: true},
		{Name: "worker", Provider: "grok", APIKey: "selected-worker-key"},
		{Name: "other", Provider: "anthropic", APIKey: "unrelated-key"},
	}}
	before, _ := json.Marshal(cfg)
	plan, err := buildAgentBootstrapPlan(cfg, "main", "openai/principal", &types.SubagentConfig{LLMKey: "worker", Model: "grok/worker"})
	if err != nil {
		t.Fatal(err)
	}
	// Execute exports to check actual values, rather than matching script text.
	cmd := exec.Command("bash", "-c", plan.LLMKeyEnv+`python3 -c 'import os,json; print(json.dumps([os.getenv("OPENAI_API_KEY"),os.getenv("XAI_API_KEY"),os.getenv("ANTHROPIC_API_KEY")]))'`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("env: %v: %s", err, out)
	}
	var values []string
	if err := json.Unmarshal(out, &values); err != nil {
		t.Fatal(err)
	}
	if strings.Join(values, ",") != "principal-key,selected-worker-key,unrelated-key" {
		t.Fatalf("credential precedence: %v", values)
	}
	if !strings.Contains(plan.ProviderConfig, "anthropic:default") {
		t.Fatal("unrelated Anthropic compatibility auth patch missing")
	}
	after, _ := json.Marshal(cfg)
	if string(before) != string(after) {
		t.Fatal("configuration mutated")
	}
}

func TestAgentBootstrapPreservesNativeModelAccess(t *testing.T) {
	for _, provider := range []string{"anthropic", "grok", "ollama", "openai"} {
		for _, existingMap := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%v", provider, existingMap), func(t *testing.T) {
				cfg := &types.HubConfig{LLMKeys: types.LLMKeysList{{Name: "main", Provider: "anthropic", APIKey: "key"}, {Name: "worker", Provider: provider, APIKey: "worker-key"}}}
				// Same provider must use the same credential by contract.
				childKey := "worker"
				if provider == "anthropic" {
					childKey = "main"
				}
				plan, err := buildAgentBootstrapPlan(cfg, "main", "anthropic/principal", &types.SubagentConfig{LLMKey: childKey, Model: provider + "/worker"})
				if err != nil {
					t.Fatal(err)
				}
				existing := `{}`
				if existingMap {
					existing = `{"agents":{"defaults":{"models":{"anthropic/other":{}}}}}`
				}
				config := executeAgentConfigPatch(t, plan.ProviderConfig, "anthropic/principal", existing)
				defaults := config["agents"].(map[string]any)["defaults"].(map[string]any)
				models, hasMap := defaults["models"].(map[string]any)
				if !existingMap && provider != "openai" {
					if hasMap {
						t.Fatal("native models unexpectedly restricted")
					}
					return
				}
				if !hasMap || models["anthropic/principal"] == nil || models[provider+"/worker"] == nil {
					t.Fatalf("required model entries missing: %v", models)
				}
				if existingMap && models["anthropic/other"] == nil {
					t.Fatal("existing allowed model removed")
				}
			})
		}
	}
}

func TestResolveDaytonaBootstrapModelCompatibility(t *testing.T) {
	cfg := &types.HubConfig{DefaultModel: "openai/hub"}
	key := &types.LLMKeyConfig{Provider: "anthropic", DefaultModel: "anthropic/key-default"}
	for _, tc := range []struct {
		stored, want string
		mismatch     bool
	}{
		{"anthropic/pinned", "anthropic/pinned", false},
		{"openai/legacy", "anthropic/key-default", true},
		{"", "anthropic/key-default", false},
		{"unpinned-prefix", "anthropic/unpinned-prefix", false},
	} {
		model, mismatch := resolveDaytonaBootstrapModel(cfg, key, tc.stored)
		if model != tc.want || mismatch != tc.mismatch {
			t.Fatalf("stored %q: got %q mismatch=%v", tc.stored, model, mismatch)
		}
	}
}
