package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/workflowsetup"
)

func TestWorkflowSetupAPIPatterns(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	unauth := workflowSetupAPIRequest(s, http.MethodGet, "/api/workflow-setup/patterns", nil, false)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauth.Code, http.StatusUnauthorized)
	}

	rr := workflowSetupAPIRequest(s, http.MethodGet, "/api/workflow-setup/patterns", nil, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var gotJSON interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &gotJSON); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	wantBytes, err := json.Marshal(workflowsetup.Patterns())
	if err != nil {
		t.Fatalf("marshal expected patterns: %v", err)
	}
	var wantJSON interface{}
	if err := json.Unmarshal(wantBytes, &wantJSON); err != nil {
		t.Fatalf("decode expected patterns: %v", err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Fatalf("patterns response mismatch\n got: %#v\nwant: %#v", gotJSON, wantJSON)
	}
}

func TestWorkflowSetupAPIContextMasksWorkspaceData(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "engineering",
			Repositories: []types.GitHubRepoAccess{{
				Repo:        "elasticclaw/elasticclaw",
				Permissions: "write",
			}},
			Env: types.WorkspaceEnv{
				"SAFE_INLINE": {Value: "inline-env-value"},
				"SECRET_ENV":  {Secret: "workspace_token"},
			},
			Secrets:        []string{"declared_secret"},
			WebhookSecrets: []string{"workspace_hook_name"},
		},
		nil,
	)
	if err := saveWorkspaceSecret("engineering", "workspace_token", "workspace-secret-value"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}
	if err := saveWorkspaceIssueTracker("engineering", "github-issues", "issues-main", workspaceIssueTracker{
		Token:         "workspace-issues-token",
		WebhookSecret: "workspace-issues-hook-secret",
	}); err != nil {
		t.Fatalf("save workspace issue tracker: %v", err)
	}
	if err := saveWorkspaceGitHubApp("engineering", "eng-app", workspaceGitHubApp{
		AppID:         4321,
		URL:           "https://github.com/apps/eng-app",
		Installation:  "elasticclaw",
		PrivateKeyPEM: "workspace-private-key-secret",
	}); err != nil {
		t.Fatalf("save workspace GitHub App: %v", err)
	}

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token:        "test-token",
		ClawToken:    "claw-token-secret",
		DefaultModel: "anthropic/claude-sonnet-4-6",
		Providers: map[string]types.ProviderConfig{
			"daytona": {APIKey: "daytona-api-key-secret"},
		},
		LLMKeys: types.LLMKeysList{
			{Name: "anthropic-main", Provider: "anthropic", APIKey: "llm-api-key-secret", Default: true, DefaultModel: "claude-sonnet-4-6"},
		},
		Secrets: map[string]string{
			"hub_api_key": "hub-secret-value",
		},
		Integrations: &types.IntegrationsConfig{
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{
				Workspace:     "hub-issues",
				Token:         "hub-issues-token",
				WebhookSecret: "hub-issues-hook-secret",
			}},
		},
		GitHubApps: []*types.GitHubAppConfig{{
			AppID:         9876,
			URL:           "https://github.com/apps/hub-app",
			PrivateKeyPEM: "hub-private-key-secret",
		}},
		ConcurrencyGroups: []*types.ConcurrencyGroup{{Name: "deploys", Limit: 1}},
	}, "", "", "")

	rr := workflowSetupAPIRequest(s, http.MethodGet, "/api/workflow-setup/workspaces/engineering/context", nil, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Workspace struct {
			Name                string                          `json:"name"`
			Repositories        []types.GitHubRepoAccess        `json:"repositories"`
			EnvNames            []string                        `json:"envNames"`
			SecretNames         []string                        `json:"secretNames"`
			DeclaredSecretNames []string                        `json:"declaredSecretNames"`
			WebhookSecretNames  []string                        `json:"webhookSecretNames"`
			IssueTrackers       []workflowsetup.IssueTrackerRef `json:"issueTrackers"`
			GitHubApps          []workflowsetup.GitHubAppRef    `json:"githubApps"`
		} `json:"workspace"`
		Hub struct {
			SecretNames   []string                        `json:"secretNames"`
			IssueTrackers []workflowsetup.IssueTrackerRef `json:"issueTrackers"`
			GitHubApps    []workflowsetup.GitHubAppRef    `json:"githubApps"`
		} `json:"hub"`
		Readiness struct {
			ClawTokenSet      bool                                `json:"clawTokenSet"`
			Providers         []workflowsetup.ProviderRef         `json:"providers"`
			DefaultProvider   string                              `json:"defaultProvider"`
			ProviderReady     bool                                `json:"providerReady"`
			DefaultModel      string                              `json:"defaultModel"`
			LLMKeys           []workflowsetup.LLMKeyRef           `json:"llmKeys"`
			ModelReady        bool                                `json:"modelReady"`
			ConcurrencyGroups []workflowsetup.ConcurrencyGroupRef `json:"concurrencyGroups"`
		} `json:"readiness"`
		ConcurrencyGroups []workflowsetup.ConcurrencyGroupRef `json:"concurrencyGroups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode context: %v", err)
	}

	if resp.Workspace.Name != "engineering" {
		t.Fatalf("workspace name = %q", resp.Workspace.Name)
	}
	if got, want := resp.Workspace.Repositories, []types.GitHubRepoAccess{{Repo: "elasticclaw/elasticclaw", Permissions: "write"}}; !slices.Equal(got, want) {
		t.Fatalf("repositories = %#v, want %#v", got, want)
	}
	if got, want := resp.Workspace.EnvNames, []string{"SAFE_INLINE", "SECRET_ENV"}; !slices.Equal(got, want) {
		t.Fatalf("env names = %#v, want %#v", got, want)
	}
	if got, want := resp.Workspace.SecretNames, []string{"workspace_token"}; !slices.Equal(got, want) {
		t.Fatalf("workspace secret names = %#v, want %#v", got, want)
	}
	if got, want := resp.Workspace.DeclaredSecretNames, []string{"declared_secret"}; !slices.Equal(got, want) {
		t.Fatalf("declared secret names = %#v, want %#v", got, want)
	}
	if got, want := resp.Workspace.WebhookSecretNames, []string{"workspace_hook_name"}; !slices.Equal(got, want) {
		t.Fatalf("webhook secret names = %#v, want %#v", got, want)
	}
	if len(resp.Workspace.IssueTrackers) != 1 || !resp.Workspace.IssueTrackers[0].TokenSet || !resp.Workspace.IssueTrackers[0].WebhookSecretSet {
		t.Fatalf("workspace issue trackers = %#v", resp.Workspace.IssueTrackers)
	}
	if len(resp.Workspace.GitHubApps) != 1 || resp.Workspace.GitHubApps[0].Name != "eng-app" || !resp.Workspace.GitHubApps[0].PrivateKeySet {
		t.Fatalf("workspace GitHub Apps = %#v", resp.Workspace.GitHubApps)
	}
	if got, want := resp.Hub.SecretNames, []string{"hub_api_key"}; !slices.Equal(got, want) {
		t.Fatalf("hub secret names = %#v, want %#v", got, want)
	}
	if len(resp.Hub.IssueTrackers) != 1 || !resp.Hub.IssueTrackers[0].TokenSet || !resp.Hub.IssueTrackers[0].WebhookSecretSet {
		t.Fatalf("hub issue trackers = %#v", resp.Hub.IssueTrackers)
	}
	if len(resp.Hub.GitHubApps) != 1 || !resp.Hub.GitHubApps[0].PrivateKeySet {
		t.Fatalf("hub GitHub Apps = %#v", resp.Hub.GitHubApps)
	}
	if !resp.Readiness.ClawTokenSet || resp.Readiness.DefaultProvider != "daytona" || !resp.Readiness.ProviderReady {
		t.Fatalf("provider readiness = %#v", resp.Readiness)
	}
	if resp.Readiness.DefaultModel != "anthropic/claude-sonnet-4-6" || !resp.Readiness.ModelReady {
		t.Fatalf("model readiness = %#v", resp.Readiness)
	}
	if len(resp.Readiness.Providers) != 1 || !resp.Readiness.Providers[0].APIKeySet {
		t.Fatalf("provider refs = %#v", resp.Readiness.Providers)
	}
	if len(resp.Readiness.LLMKeys) != 1 || !resp.Readiness.LLMKeys[0].KeySet {
		t.Fatalf("llm refs = %#v", resp.Readiness.LLMKeys)
	}
	if got, want := resp.ConcurrencyGroups, []workflowsetup.ConcurrencyGroupRef{{Name: "deploys", Limit: 1}}; !slices.Equal(got, want) {
		t.Fatalf("concurrency groups = %#v, want %#v", got, want)
	}

	for _, secret := range []string{
		"claw-token-secret",
		"daytona-api-key-secret",
		"llm-api-key-secret",
		"hub-secret-value",
		"hub-issues-token",
		"hub-issues-hook-secret",
		"hub-private-key-secret",
		"inline-env-value",
		"workspace-secret-value",
		"workspace-issues-token",
		"workspace-issues-hook-secret",
		"workspace-private-key-secret",
	} {
		if strings.Contains(rr.Body.String(), secret) {
			t.Fatalf("context response contains sensitive value %q: %s", secret, rr.Body.String())
		}
	}
}

func TestWorkflowSetupAPIContextReturnsArraysForEmptyCollections(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "empty",
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: empty\n",
			},
		},
		nil,
	)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	rr := workflowSetupAPIRequest(s, http.MethodGet, "/api/workflow-setup/workspaces/empty/context", nil, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode context: %v", err)
	}

	workspace := raw["workspace"].(map[string]interface{})
	hub := raw["hub"].(map[string]interface{})
	readiness := raw["readiness"].(map[string]interface{})
	for _, tt := range []struct {
		name  string
		value interface{}
	}{
		{"workspace.repositories", workspace["repositories"]},
		{"workspace.envNames", workspace["envNames"]},
		{"workspace.secretNames", workspace["secretNames"]},
		{"workspace.declaredSecretNames", workspace["declaredSecretNames"]},
		{"workspace.webhookSecretNames", workspace["webhookSecretNames"]},
		{"workspace.issueTrackers", workspace["issueTrackers"]},
		{"workspace.githubApps", workspace["githubApps"]},
		{"hub.secretNames", hub["secretNames"]},
		{"hub.issueTrackers", hub["issueTrackers"]},
		{"hub.githubApps", hub["githubApps"]},
		{"readiness.providers", readiness["providers"]},
		{"readiness.llmKeys", readiness["llmKeys"]},
		{"readiness.concurrencyGroups", readiness["concurrencyGroups"]},
		{"concurrencyGroups", raw["concurrencyGroups"]},
	} {
		if _, ok := tt.value.([]interface{}); !ok {
			t.Fatalf("%s encoded as %T (%#v), want JSON array; body = %s", tt.name, tt.value, tt.value, rr.Body.String())
		}
	}
}

func TestWorkflowSetupAPIContextTreatsExedevWithoutCredentialsAsReady(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "engineering",
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\nprovider: exedev\n",
			},
		},
		nil,
	)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Providers: map[string]types.ProviderConfig{
			"exedev": {},
		},
	}, "", "", "")

	rr := workflowSetupAPIRequest(s, http.MethodGet, "/api/workflow-setup/workspaces/engineering/context", nil, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Readiness struct {
			ProviderReady bool                        `json:"providerReady"`
			Providers     []workflowsetup.ProviderRef `json:"providers"`
		} `json:"readiness"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode context: %v", err)
	}
	if !resp.Readiness.ProviderReady {
		t.Fatalf("providerReady = false, want true for provisionable exedev with default SSH agent: %#v", resp.Readiness.Providers)
	}
	if len(resp.Readiness.Providers) != 1 || resp.Readiness.Providers[0].Type != "exedev" || !resp.Readiness.Providers[0].Provisionable || resp.Readiness.Providers[0].CredentialsSet {
		t.Fatalf("provider refs = %#v, want exedev provisionable without explicit credentials", resp.Readiness.Providers)
	}
}

func TestWorkflowSetupAPIRenderReturnsConfigHash(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	body := workflowsetup.RenderRequest{
		WorkflowName: "manual-run",
		PatternID:    workflowsetup.PatternManualTask,
		Config: map[string]interface{}{
			"inputs": []map[string]interface{}{
				{"name": "task", "type": "string", "required": true},
			},
		},
	}
	rr := workflowSetupAPIRequest(s, http.MethodPost, "/api/workflow-setup/render", body, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp workflowsetup.RenderResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode render: %v", err)
	}
	if resp.WorkflowName != "manual-run" {
		t.Fatalf("workflowName = %q", resp.WorkflowName)
	}
	if !strings.Contains(resp.Config, "name: manual-run") {
		t.Fatalf("rendered config missing workflow name: %s", resp.Config)
	}
	if got, want := resp.ConfigHash, workflowsetup.ConfigHash(resp.Config); got != want {
		t.Fatalf("configHash = %q, want %q", got, want)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", resp.Warnings)
	}
}

func TestWorkflowSetupAPIValidateLoadsWorkspaceAndReturnsCriticalFailure(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	workspaceRaw := strings.Join([]string{
		"schema_version: v1",
		"name: engineering",
		"repositories:",
		"  - repo: elasticclaw/elasticclaw",
		"    permissions: write",
		"env:",
		"  API_TOKEN:",
		"    secret: workspace_token",
		"",
	}, "\n")
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "engineering",
			Repositories: []types.GitHubRepoAccess{{
				Repo:        "elasticclaw/elasticclaw",
				Permissions: "write",
			}},
			Env:   types.WorkspaceEnv{"API_TOKEN": {Secret: "workspace_token"}},
			Files: map[string]string{"elasticclaw-config.yaml": workspaceRaw},
		},
		nil,
	)
	if err := saveWorkspaceSecret("engineering", "workspace_token", "workspace-secret-value"); err != nil {
		t.Fatalf("save workspace secret: %v", err)
	}

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token:        "test-token",
		ClawToken:    "claw-token-secret",
		DefaultModel: "anthropic/claude-sonnet-4-6",
		Providers: map[string]types.ProviderConfig{
			"daytona": {APIKey: "daytona-api-key-secret"},
		},
		LLMKeys: types.LLMKeysList{
			{Name: "anthropic-main", Provider: "anthropic", APIKey: "llm-api-key-secret", Default: true, DefaultModel: "claude-sonnet-4-6"},
		},
		ConcurrencyGroups: []*types.ConcurrencyGroup{{Name: "global", Limit: 0}},
	}, "", "", "")

	workflowRaw := strings.Join([]string{
		"schema_version: v1",
		"name: manual-run",
		"enable_manual_trigger: true",
		"secret_refs:",
		"  API_TOKEN: workspace_token",
		"concurrency_group: deploys",
		"stages:",
		"  - id: done",
		"    entry: true",
		"    terminal: true",
		"",
	}, "\n")
	rr := workflowSetupAPIRequest(s, http.MethodPost, "/api/workflow-setup/validate", map[string]interface{}{
		"workspace":    "engineering",
		"workflowName": "manual-run",
		"config":       workflowRaw,
	}, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp workflowsetup.ValidateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode validate: %v", err)
	}
	if resp.OK {
		t.Fatalf("validate OK = true, want false: %#v", resp)
	}
	if got, want := resp.ConfigHash, workflowsetup.ConfigHash(workflowRaw); got != want {
		t.Fatalf("configHash = %q, want %q", got, want)
	}
	if !workflowSetupHasCheck(resp.Checks, "readiness-concurrency-group-missing") {
		t.Fatalf("missing critical concurrency check: %#v", resp.Checks)
	}
	if workflowSetupHasCheck(resp.Checks, "readiness-secret-ref-missing") {
		t.Fatalf("workspace secret was not resolved through loaded workspace config: %#v", resp.Checks)
	}
	if strings.Contains(rr.Body.String(), "workspace-secret-value") {
		t.Fatalf("validate response contains workspace secret value: %s", rr.Body.String())
	}
}

func workflowSetupAPIRequest(s *Server, method, path string, body interface{}, auth bool) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer test-token")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func workflowSetupHasCheck(checks []workflowsetup.Diagnostic, id string) bool {
	for _, check := range checks {
		if check.ID == id {
			return true
		}
	}
	return false
}
