package hub

import (
	"context"
	"encoding/json"
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

	allowed := defaults["models"].(map[string]any)
	if allowed["grok/principal"] == nil || allowed["grok/worker"] == nil {
		t.Fatalf("principal and worker must both be allowed: %v", allowed)
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
