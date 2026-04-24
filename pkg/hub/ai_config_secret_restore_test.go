package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

func TestRestoreMaskedSecretsFromDisk_LLMKeysReorderedByName(t *testing.T) {
	diskCfg := &types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{
				Name:     "anthropic-prod",
				Provider: "anthropic",
				APIKey:   "anthropic-secret",
			},
			{
				Name:     "openai-prod",
				Provider: "openai",
				APIKey:   "openai-secret",
			},
		},
	}

	proposed := `
llm_keys:
  - name: openai-prod
    provider: openai
    api_key: "***"
  - name: anthropic-prod
    provider: anthropic
    api_key: "***"
`

	restoredYAML, err := restoreMaskedSecretsFromDisk(proposed, diskCfg)
	if err != nil {
		t.Fatalf("restoreMaskedSecretsFromDisk() error = %v", err)
	}

	var restored types.HubConfig
	if err := yaml.Unmarshal([]byte(restoredYAML), &restored); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}

	gotByName := map[string]string{}
	for _, key := range restored.LLMKeys {
		gotByName[key.Name] = key.APIKey
	}

	if gotByName["openai-prod"] != "openai-secret" {
		t.Fatalf("openai-prod api_key = %q, want %q", gotByName["openai-prod"], "openai-secret")
	}
	if gotByName["anthropic-prod"] != "anthropic-secret" {
		t.Fatalf("anthropic-prod api_key = %q, want %q", gotByName["anthropic-prod"], "anthropic-secret")
	}
}

func TestRestoreMaskedSecretsFromDisk_GitHubAppsReorderedByAppID(t *testing.T) {
	diskCfg := &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{
			{
				AppID:         101,
				URL:           "https://github.com/apps/a",
				PrivateKeyPEM: "private-key-101",
			},
			{
				AppID:         202,
				URL:           "https://github.com/apps/b",
				PrivateKeyPEM: "private-key-202",
			},
		},
	}

	proposed := `
github:
  - app_id: 202
    url: https://github.com/apps/b
    private_key_pem: "***"
  - app_id: 101
    url: https://github.com/apps/a
    private_key_pem: "***"
`

	restoredYAML, err := restoreMaskedSecretsFromDisk(proposed, diskCfg)
	if err != nil {
		t.Fatalf("restoreMaskedSecretsFromDisk() error = %v", err)
	}

	var restored types.HubConfig
	if err := yaml.Unmarshal([]byte(restoredYAML), &restored); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}

	gotByAppID := map[int64]string{}
	for _, app := range restored.GitHubApps {
		gotByAppID[app.AppID] = app.PrivateKeyPEM
	}

	if gotByAppID[101] != "private-key-101" {
		t.Fatalf("app_id 101 private_key_pem = %q, want %q", gotByAppID[101], "private-key-101")
	}
	if gotByAppID[202] != "private-key-202" {
		t.Fatalf("app_id 202 private_key_pem = %q, want %q", gotByAppID[202], "private-key-202")
	}
}

func TestSubstitutePlaceholders_PreservesYAMLMetacharactersInSecrets(t *testing.T) {
	proposed := `
token: __HUB_TOKEN__
`
	secrets := map[string]string{
		"HUB_TOKEN": "sk-abc123 #note",
	}

	substituted, err := substitutePlaceholders(proposed, secrets)
	if err != nil {
		t.Fatalf("substitutePlaceholders() error = %v", err)
	}

	var restored types.HubConfig
	if err := yaml.Unmarshal([]byte(substituted), &restored); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}

	if restored.Token != "sk-abc123 #note" {
		t.Fatalf("token = %q, want %q", restored.Token, "sk-abc123 #note")
	}
}

func TestSubstitutePlaceholders_PreservesNewlinesInSecrets(t *testing.T) {
	proposed := `
secrets:
  webhook_secret: __WEBHOOK_SECRET__
`
	secrets := map[string]string{
		"WEBHOOK_SECRET": "line1\nline2: value #tag",
	}

	substituted, err := substitutePlaceholders(proposed, secrets)
	if err != nil {
		t.Fatalf("substitutePlaceholders() error = %v", err)
	}

	var restored types.HubConfig
	if err := yaml.Unmarshal([]byte(substituted), &restored); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}

	if restored.Secrets["webhook_secret"] != "line1\nline2: value #tag" {
		t.Fatalf("webhook_secret = %q, want %q", restored.Secrets["webhook_secret"], "line1\nline2: value #tag")
	}
}

func TestValidateHubConfig_RequiresToken(t *testing.T) {
	cfg := &types.HubConfig{
		URL:       "https://hub.example.com",
		ClawToken: "claw-token",
	}

	err := validateHubConfig(cfg)
	if err == nil {
		t.Fatal("validateHubConfig() error = nil, want token validation error")
	}
	if err.Error() != "token is required" {
		t.Fatalf("validateHubConfig() error = %q, want %q", err.Error(), "token is required")
	}
}

func TestCheckMaskedValues_RejectsMaskedIntegrationAndFactorySecrets(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *types.HubConfig
		wantErr string
	}{
		{
			name: "linear token",
			cfg: &types.HubConfig{
				Integrations: &types.IntegrationsConfig{
					Linear: []*types.LinearIntegrationConfig{
						{Workspace: "acme", Token: "***"},
					},
				},
			},
			wantErr: "integrations.linear[0] (acme): token contains unresolved mask value",
		},
		{
			name: "linear webhook secret",
			cfg: &types.HubConfig{
				Integrations: &types.IntegrationsConfig{
					Linear: []*types.LinearIntegrationConfig{
						{Workspace: "acme", WebhookSecret: "***"},
					},
				},
			},
			wantErr: "integrations.linear[0] (acme): webhook_secret contains unresolved mask value",
		},
		{
			name: "shortcut token",
			cfg: &types.HubConfig{
				Integrations: &types.IntegrationsConfig{
					Shortcut: []*types.ShortcutIntegrationConfig{
						{Workspace: "ops", Token: "***"},
					},
				},
			},
			wantErr: "integrations.shortcut[0] (ops): token contains unresolved mask value",
		},
		{
			name: "factory webhook secret",
			cfg: &types.HubConfig{
				Factories: []*types.FactoryConfig{
					{Name: "factory-a", WebhookSecret: "***"},
				},
			},
			wantErr: "factories[0] (factory-a): webhook_secret contains unresolved mask value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkMaskedValues(tt.cfg)
			if err == nil {
				t.Fatalf("checkMaskedValues() error = nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkMaskedValues() error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
