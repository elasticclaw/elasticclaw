package hub

import (
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
