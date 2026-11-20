package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/harness"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestHarnessBootstrapGolden(t *testing.T) {
	anthropic := &types.LLMKeyConfig{Name: "anthropic-main", Provider: "anthropic", APIKey: "test-key", Default: true}
	codex := &types.LLMKeyConfig{Name: "codex-main", Provider: "codex", AuthProfile: "codex-oauth"}
	type bootstrapCase struct {
		name         string
		keys         []*types.LLMKeyConfig
		selectedKey  string
		defaultModel string
	}
	cases := []bootstrapCase{
		{name: "no_keys"},
		{name: "anthropic", keys: []*types.LLMKeyConfig{anthropic}},
		{name: "codex_oauth", keys: []*types.LLMKeyConfig{codex}, defaultModel: "openai/gpt-5.6-sol"},
		{name: "grok_oauth", keys: []*types.LLMKeyConfig{{Name: "grok-main", Provider: "grok", AuthProfile: "grok-oauth"}}, defaultModel: "grok/grok-build-0.1"},
		{name: "ollama", keys: []*types.LLMKeyConfig{{Name: "ollama-main", Provider: "ollama", DefaultModel: "ollama/model's-name"}}, defaultModel: "ollama/ignored"},
		{name: "ollama_default_model", keys: []*types.LLMKeyConfig{{Name: "ollama-main", Provider: "ollama", DefaultModel: "openai/ignored"}}, defaultModel: "ollama/qwen2.5-coder:7b"},
		{name: "ollama_fallback", keys: []*types.LLMKeyConfig{{Name: "ollama-main", Provider: "ollama"}}, defaultModel: "anthropic/ignored"},
		{name: "selected_over_default", keys: []*types.LLMKeyConfig{anthropic, codex}, selectedKey: "codex-main"},
		{name: "default_over_first", keys: []*types.LLMKeyConfig{codex, anthropic}},
		{name: "unknown_selected", keys: []*types.LLMKeyConfig{codex, anthropic}, selectedKey: "missing"},
		{name: "first_fallback", keys: []*types.LLMKeyConfig{codex, {Name: "grok-main", Provider: "grok", AuthProfile: "grok-oauth"}}},
		{name: "blank_selected", keys: []*types.LLMKeyConfig{{Name: "empty", Provider: "openai", Default: true}, anthropic}, selectedKey: "empty"},
		{name: "all_blank", keys: []*types.LLMKeyConfig{{Name: "empty", Provider: "openai", Default: true}}},
	}
	for _, provider := range []string{"openai", "codex", "grok", "fireworks", "groq", "deepseek", "custom"} {
		cases = append(cases, bootstrapCase{
			name: provider + "_api_key",
			keys: []*types.LLMKeyConfig{{Name: provider + "-main", Provider: provider, APIKey: "test-key", AuthProfile: "ignored-oauth"}},
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := openClawBootstrap().Config(harness.BootstrapInput{
				LLMKeys: tc.keys, SelectedKey: tc.selectedKey, DefaultModel: tc.defaultModel,
			})
			wrapped := harness.BootstrapConfig{
				ProviderConfig: buildOpenClawProviderConfig(tc.keys, tc.selectedKey),
				APIKeyAuthSync: buildOpenClawAPIKeyAuthSyncShell(tc.keys, tc.selectedKey),
				OAuthAuthSync:  buildOpenClawOAuthAuthSyncShell(tc.keys, tc.selectedKey),
				OnboardFlags:   buildOnboardFlags(tc.keys, tc.selectedKey, tc.defaultModel),
			}
			if got != wrapped {
				t.Fatal("builder wrappers differ from the registered bootstrap")
			}
			data, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			assertHarnessBootstrapGolden(t, tc.name, string(data)+"\n")
		})
	}
}

func TestHarnessBootstrapLauncherGolden(t *testing.T) {
	keys := []*types.LLMKeyConfig{{Name: "anthropic-main", Provider: "anthropic", APIKey: "test-key"}}
	p := baseParams()
	p.Nix = true
	p.Docker = true
	p.LLMProvider = "anthropic"
	p.ProviderConfig = buildOpenClawProviderConfig(keys, "anthropic-main")
	p.APIKeyAuthSync = buildOpenClawAPIKeyAuthSyncShell(keys, "anthropic-main")
	p.OAuthAuthSync = buildOpenClawOAuthAuthSyncShell(keys, "anthropic-main")
	p.OnboardFlags = buildOnboardFlags(keys, "anthropic-main", p.DefaultModel)
	p.Env = map[string]string{"Z_CUSTOM": "last", "A_CUSTOM": "value with 'quotes'"}
	assertHarnessBootstrapGolden(t, "replicated_launcher", GenerateReplicatedBootstrapScript(p))
}

func assertHarnessBootstrapGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "harness_bootstrap", name+".golden")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("bootstrap output differs from %s\n got: %q\nwant: %q", path, got, want)
	}
}
