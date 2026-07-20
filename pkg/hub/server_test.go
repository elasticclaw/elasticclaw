package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestSanitizeBootstrapOutputDropsExportedEnvironment(t *testing.T) {
	raw := `declare -x DAYTONA_ORGANIZATION_ID="86d60eef-1b4a-4543-85a6-f402a4eeb1e4"
declare -x DAYTONA_SANDBOX_ID="f1526fda-fe90-4895-8492-2e4a67bd1359"
curl: (23) Failure writing output to destination
`
	got := sanitizeBootstrapOutput(raw)
	if strings.Contains(got, "declare -x") || strings.Contains(got, "DAYTONA_SANDBOX_ID") {
		t.Fatalf("expected environment lines to be removed, got %q", got)
	}
	if !strings.Contains(got, "curl: (23)") {
		t.Fatalf("expected useful curl error to remain, got %q", got)
	}
}

func TestSanitizeBootstrapOutputTruncatesLongOutput(t *testing.T) {
	got := sanitizeBootstrapOutput(strings.Repeat("x", 1400))
	if len(got) != 1200 {
		t.Fatalf("expected output to be truncated to 1200 bytes, got %d", len(got))
	}
}

func TestCleanWorkspaceFilePath(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		wantErr bool
	}{
		{name: "AGENTS.md", want: "AGENTS.md"},
		{name: "scripts/run_android_codebuild.py", want: "scripts/run_android_codebuild.py"},
		{name: "scripts/utils/helper.py", want: "scripts/utils/helper.py"},
		{name: "scripts/my script.py", want: "scripts/my script.py"},
		{name: "../secret", wantErr: true},
		{name: "scripts/../../secret", wantErr: true},
		{name: "/tmp/secret", wantErr: true},
		{name: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cleanWorkspaceFilePath(tt.name)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got path %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSSHHomeDir(t *testing.T) {
	tests := []struct {
		user    string
		want    string
		wantErr bool
	}{
		{user: "elasticclaw", want: "/home/elasticclaw"},
		{user: "root", want: "/root"},
		{user: " elasticclaw ", want: "/home/elasticclaw"},
		{user: "", wantErr: true},
		{user: "bad/user", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.user, func(t *testing.T) {
			got, err := sshHomeDir(tt.user)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got home %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("home = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestServeWebUIMapsWorkspaceSettingsRoutesToStaticPlaceholder(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	mux := http.NewServeMux()
	s.serveWebUI(mux, fstest.MapFS{
		"index.html":                                         &fstest.MapFile{Data: []byte("root")},
		"settings/index.html":                                &fstest.MapFile{Data: []byte("settings-root")},
		"settings/workflows/index.html":                      &fstest.MapFile{Data: []byte("legacy-workflows")},
		"settings/_workspace/index.html":                     &fstest.MapFile{Data: []byte("workspace-overview")},
		"settings/_workspace/issue-trackers/index.html":      &fstest.MapFile{Data: []byte("workspace-issue-trackers")},
		"settings/_workspace/workspace-analytics/index.html": &fstest.MapFile{Data: []byte("workspace-analytics")},
	})

	tests := []struct {
		path string
		want string
	}{
		{path: "/settings/elasticclaw", want: "workspace-overview"},
		{path: "/settings/elasticclaw/issue-trackers", want: "workspace-issue-trackers"},
		{path: "/settings/elasticclaw/workspace-analytics", want: "workspace-analytics"},
		{path: "/settings/workflows", want: "legacy-workflows"},
		{path: "/settings/elasticclaw/nonexistent", want: "root"},
		{path: "/settings/elasticclaw/runtimes", want: "root"},
		{path: "/settings/elasticclaw/issue-trackers/extra", want: "root"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			mux.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			if got := rr.Body.String(); got != tt.want {
				t.Fatalf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveDefaultModelForKey(t *testing.T) {
	tests := []struct {
		name          string
		hubCfg        *types.HubConfig
		key           *types.LLMKeyConfig
		expectedModel string
	}{
		{
			name: "hub default matches key provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-opus-4-5",
			},
			key: &types.LLMKeyConfig{
				Provider: "anthropic",
			},
			expectedModel: "anthropic/claude-opus-4-5",
		},
		{
			name: "hub default doesn't match - use provider default",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key: &types.LLMKeyConfig{
				Provider: "openai",
			},
			expectedModel: "openai/gpt-5.5",
		},
		{
			name: "no hub default - use provider default",
			hubCfg: &types.HubConfig{
				DefaultModel: "",
			},
			key: &types.LLMKeyConfig{
				Provider: "fireworks",
			},
			expectedModel: defaultFireworksModel,
		},
		{
			name: "unknown provider - fall back to hub default",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key: &types.LLMKeyConfig{
				Provider: "unknown-provider",
			},
			expectedModel: "anthropic/claude-sonnet-4-6",
		},
		{
			name: "nil key - return hub default",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key:           nil,
			expectedModel: "anthropic/claude-sonnet-4-6",
		},
		{
			name: "groq provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key: &types.LLMKeyConfig{
				Provider: "groq",
			},
			expectedModel: "groq/llama-3.3-70b-versatile",
		},
		{
			name: "codex provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "",
			},
			key: &types.LLMKeyConfig{
				Provider: "codex",
			},
			expectedModel: defaultCodexModel,
		},
		{
			name: "codex provider normalizes legacy namespaced model",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key: &types.LLMKeyConfig{
				Provider:     "codex",
				DefaultModel: "codex/gpt-5.5",
			},
			expectedModel: "openai/gpt-5.5",
		},
		{
			name: "codex provider normalizes bare model",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			key: &types.LLMKeyConfig{
				Provider:     "codex",
				DefaultModel: "gpt-5.6-terra",
			},
			expectedModel: "openai/gpt-5.6-terra",
		},
		{
			name: "grok provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "",
			},
			key: &types.LLMKeyConfig{
				Provider: "grok",
			},
			expectedModel: "grok/grok-build-0.1",
		},
		{
			name: "deepseek provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "",
			},
			key: &types.LLMKeyConfig{
				Provider: "deepseek",
			},
			expectedModel: "deepseek/deepseek-chat",
		},
		{
			name: "ollama provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "",
			},
			key: &types.LLMKeyConfig{
				Provider: "ollama",
			},
			expectedModel: "ollama/qwen2.5-coder:1.5b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveDefaultModelForKey(tt.hubCfg, tt.key)
			if result != tt.expectedModel {
				t.Errorf("expected %s, got %s", tt.expectedModel, result)
			}
		})
	}
}

func TestGetSettingsTreatsBlankOllamaKeyAsConfigured(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "local-ollama", Provider: "ollama", Default: true},
			{Name: "openai-missing", Provider: "openai"},
		},
	}, "", "", "")

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()

	s.getSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var view SettingsView
	if err := json.NewDecoder(rec.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.DefaultOpenClawImage != cliversion.OpenClawImage {
		t.Fatalf("default OpenClaw image = %q, want %q", view.DefaultOpenClawImage, cliversion.OpenClawImage)
	}
	byName := map[string]LLMKeyView{}
	for _, key := range view.LLMKeys {
		byName[key.Name] = key
	}
	if !byName["local-ollama"].KeySet {
		t.Fatalf("blank ollama key should be treated as configured: %#v", byName["local-ollama"])
	}
	if byName["openai-missing"].KeySet {
		t.Fatalf("blank external key should not be treated as configured: %#v", byName["openai-missing"])
	}
}

func TestSettingsStatusProviderReadiness(t *testing.T) {
	tests := []struct {
		name     string
		provider map[string]types.ProviderConfig
		want     bool
	}{
		{
			name:     "no providers",
			provider: nil,
			want:     false,
		},
		{
			name: "empty daytona",
			provider: map[string]types.ProviderConfig{
				"daytona": {},
			},
			want: false,
		},
		{
			name: "daytona with api key",
			provider: map[string]types.ProviderConfig{
				"daytona": {APIKey: "daytona-key"},
			},
			want: true,
		},
		{
			name: "empty replicated",
			provider: map[string]types.ProviderConfig{
				"replicated": {},
			},
			want: false,
		},
		{
			name: "replicated with token",
			provider: map[string]types.ProviderConfig{
				"replicated": {Token: "replicated-token"},
			},
			want: true,
		},
		{
			name: "docker does not require credentials",
			provider: map[string]types.ProviderConfig{
				"docker": {},
			},
			want: true,
		},
		{
			name: "exedev does not require configured credentials",
			provider: map[string]types.ProviderConfig{
				"exedev": {},
			},
			want: true,
		},
		{
			name: "lambda microvms requires image identifier",
			provider: map[string]types.ProviderConfig{
				"lambda-microvms": {},
			},
			want: false,
		},
		{
			name: "lambda microvms with image identifier",
			provider: map[string]types.ProviderConfig{
				"lambda-microvms": {ImageIdentifier: "arn:aws:lambda:us-east-1:123456789012:microvm-image/elasticclaw"},
			},
			want: true,
		},
		{
			name: "named provider uses explicit type",
			provider: map[string]types.ProviderConfig{
				"primary": {Type: "daytona", APIKey: "daytona-key"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := NewTestServerWithConfig(t, &types.HubConfig{
				Providers: tt.provider,
			}, "", "", "")

			req := httptest.NewRequest(http.MethodGet, "/api/settings/status", nil)
			rec := httptest.NewRecorder()
			s.handleSettingsStatus(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var status SettingsStatus
			if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
				t.Fatal(err)
			}
			if status.HasProvider != tt.want {
				t.Fatalf("HasProvider = %v, want %v", status.HasProvider, tt.want)
			}
		})
	}
}

func TestTemplateFlakeFiles(t *testing.T) {
	files := map[string]string{
		"flake.nix":  "{ description = \"test\"; }",
		"flake.lock": "{}",
		"AGENTS.md":  "instructions",
	}

	flakeFiles := templateFlakeFiles(files)
	if len(flakeFiles) != 2 {
		t.Fatalf("len(flakeFiles) = %d, want 2", len(flakeFiles))
	}
	if flakeFiles["flake.nix"] != files["flake.nix"] {
		t.Fatalf("flake.nix = %q, want %q", flakeFiles["flake.nix"], files["flake.nix"])
	}
	if flakeFiles["flake.lock"] != files["flake.lock"] {
		t.Fatalf("flake.lock = %q, want %q", flakeFiles["flake.lock"], files["flake.lock"])
	}
	if _, ok := flakeFiles["AGENTS.md"]; ok {
		t.Fatal("AGENTS.md should not be included in flake staging files")
	}
}

func TestCheckDefaultModel(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")

	tests := []struct {
		name        string
		hubCfg      *types.HubConfig
		expectOK    bool
		expectTitle string
	}{
		{
			name: "hub default model set",
			hubCfg: &types.HubConfig{
				DefaultModel: "anthropic/claude-sonnet-4-6",
			},
			expectOK:    true,
			expectTitle: "Default model configured",
		},
		{
			name: "no hub default but LLM key has default_model",
			hubCfg: &types.HubConfig{
				LLMKeys: []*types.LLMKeyConfig{
					{Name: "fireworks-prod", Provider: "fireworks", APIKey: "fw_...", Default: true, DefaultModel: "fireworks/accounts/fireworks/models/kimi-k2p6"},
				},
			},
			expectOK:    true,
			expectTitle: "Default model configured",
		},
		{
			name: "no hub default and no key default_model — provider fallback available",
			hubCfg: &types.HubConfig{
				LLMKeys: []*types.LLMKeyConfig{
					{Name: "fireworks-prod", Provider: "fireworks", APIKey: "fw_...", Default: true},
				},
			},
			expectOK:    true,
			expectTitle: "Default model configured",
		},
		{
			name: "no hub default and no LLM keys at all",
			hubCfg: &types.HubConfig{
				LLMKeys: []*types.LLMKeyConfig{},
			},
			expectOK:    false,
			expectTitle: "No default model configured",
		},
		{
			name: "invalid default model format",
			hubCfg: &types.HubConfig{
				DefaultModel: "claude-sonnet",
			},
			expectOK:    false,
			expectTitle: "Default model format is invalid",
		},
		{
			name: "no explicit default key — first key used as fallback",
			hubCfg: &types.HubConfig{
				LLMKeys: []*types.LLMKeyConfig{
					{Name: "anthropic-prod", Provider: "anthropic", APIKey: "sk-..."}, // not marked default
				},
			},
			expectOK:    true,
			expectTitle: "Default model configured",
		},
		{
			name: "provider without built-in fallback and no key default_model",
			hubCfg: &types.HubConfig{
				LLMKeys: []*types.LLMKeyConfig{
					{Name: "google-prod", Provider: "google", APIKey: "g-...", Default: true}, // google has no fallback
				},
			},
			expectOK:    false,
			expectTitle: "No default model configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := s.checkDefaultModel(tt.hubCfg)
			if len(checks) != 1 {
				t.Fatalf("expected 1 check, got %d", len(checks))
			}
			if checks[0].OK != tt.expectOK {
				t.Errorf("expected OK=%v, got OK=%v (title=%q)", tt.expectOK, checks[0].OK, checks[0].Title)
			}
			if checks[0].Title != tt.expectTitle {
				t.Errorf("expected title %q, got %q", tt.expectTitle, checks[0].Title)
			}
		})
	}
}

func TestGitHubAccessChecksReturnNotFoundForMissingClaw(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Auth: &types.AuthConfig{
			Access: &types.AccessConfig{InteractRequiresTags: []string{"owner={user}"}},
		},
	}, "", "", "")

	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "patch claw", method: http.MethodPatch, path: "/api/claws/missing", body: `{"name":"new"}`},
		{name: "delete claw", method: http.MethodDelete, path: "/api/claws/missing"},
		{name: "post message", method: http.MethodPost, path: "/api/messages/missing", body: `{"content":"hello"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
			req = req.WithContext(context.WithValue(req.Context(), ctxGitHubLoginKey{}, "octocat"))
			rec := httptest.NewRecorder()

			switch tt.path {
			case "/api/claws/missing":
				req.SetPathValue("id", "missing")
				s.handleClawDetail(rec, req)
			default:
				s.handleMessages(rec, req)
			}

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
			}
		})
	}
}

func TestDeleteClawSoftDeletesAndHidesFromAPI(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at, status) VALUES(?,?,?,?,datetime('now'),?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, "connected",
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/claws/claw-1", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	req.SetPathValue("id", "claw-1")
	rec := httptest.NewRecorder()

	s.handleClawDetail(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, rec.Code)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, "claw-1").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" {
		t.Fatalf("expected claw status deleted, got %q", status)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/claws", nil)
	listReq = listReq.WithContext(context.WithValue(listReq.Context(), ctxTenantKey{}, "test-tenant-id"))
	listRec := httptest.NewRecorder()
	s.handleClaws(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d", http.StatusOK, listRec.Code)
	}
	var claws []types.Claw
	if err := json.NewDecoder(listRec.Body).Decode(&claws); err != nil {
		t.Fatal(err)
	}
	if len(claws) != 0 {
		t.Fatalf("expected deleted claw to be hidden from list, got %#v", claws)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/claws/claw-1", nil)
	getReq = getReq.WithContext(context.WithValue(getReq.Context(), ctxTenantKey{}, "test-tenant-id"))
	getReq.SetPathValue("id", "claw-1")
	getRec := httptest.NewRecorder()
	s.handleClawDetail(getRec, getReq)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("expected detail status %d, got %d", http.StatusNotFound, getRec.Code)
	}
}

func TestClawAPIReturnsGitHubIssueLink(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at, status, github_issue_id) VALUES(?,?,?,?,datetime('now'),?,?)`,
		"claw-1", "test-tenant-id", "elasticclaw/elasticclaw/342", `[]`, "connected", "elasticclaw/elasticclaw/342",
	)
	if err != nil {
		t.Fatal(err)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/claws", nil)
	listReq = listReq.WithContext(context.WithValue(listReq.Context(), ctxTenantKey{}, "test-tenant-id"))
	listRec := httptest.NewRecorder()
	s.handleClaws(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("expected list status %d, got %d", http.StatusOK, listRec.Code)
	}
	var claws []types.Claw
	if err := json.NewDecoder(listRec.Body).Decode(&claws); err != nil {
		t.Fatal(err)
	}
	if len(claws) != 1 {
		t.Fatalf("expected 1 claw, got %d", len(claws))
	}
	if claws[0].GitHubIssueID != "elasticclaw/elasticclaw/342" {
		t.Fatalf("github_issue_id = %q", claws[0].GitHubIssueID)
	}
	if claws[0].GitHubIssueURL != "https://github.com/elasticclaw/elasticclaw/issues/342" {
		t.Fatalf("github_issue_url = %q", claws[0].GitHubIssueURL)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/claws/claw-1", nil)
	getReq = getReq.WithContext(context.WithValue(getReq.Context(), ctxTenantKey{}, "test-tenant-id"))
	getReq.SetPathValue("id", "claw-1")
	getRec := httptest.NewRecorder()
	s.handleClawDetail(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected detail status %d, got %d", http.StatusOK, getRec.Code)
	}
	var claw types.Claw
	if err := json.NewDecoder(getRec.Body).Decode(&claw); err != nil {
		t.Fatal(err)
	}
	if claw.GitHubIssueURL != "https://github.com/elasticclaw/elasticclaw/issues/342" {
		t.Fatalf("detail github_issue_url = %q", claw.GitHubIssueURL)
	}
}

func TestGitHubIssueURLRequiresNumericIssueNumber(t *testing.T) {
	if got := githubIssueURL("elasticclaw/elasticclaw/342"); got != "https://github.com/elasticclaw/elasticclaw/issues/342" {
		t.Fatalf("githubIssueURL(valid) = %q", got)
	}
	if got := githubIssueURL("elasticclaw/elasticclaw/not-a-number"); got != "" {
		t.Fatalf("githubIssueURL(invalid) = %q, want empty", got)
	}
}

func TestClawSubresourceRequiresTagAccessForGitHubSession(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Auth: &types.AuthConfig{
			Access: &types.AccessConfig{
				ViewRequiresTags:     []string{"owner={user}"},
				InteractRequiresTags: []string{"owner={user}"},
			},
		},
	}, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-1", "test-tenant-id", "claw 1", `["owner=alice"]`,
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "prs", method: http.MethodGet, path: "/api/claws/claw-1/prs"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
			req = req.WithContext(context.WithValue(req.Context(), ctxGitHubLoginKey{}, "bob"))
			rec := httptest.NewRecorder()

			s.handleClawSubresource(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected status %d, got %d", http.StatusForbidden, rec.Code)
			}
		})
	}
}

func TestGitHubWritesRequireViewAndInteractAccess(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Auth: &types.AuthConfig{
			Access: &types.AccessConfig{
				ViewRequiresTags: []string{"owner={user}"},
			},
		},
	}, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-1", "test-tenant-id", "claw 1", `["owner=alice"]`,
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "patch claw", method: http.MethodPatch, path: "/api/claws/claw-1", body: `{"name":"new"}`},
		{name: "delete claw", method: http.MethodDelete, path: "/api/claws/claw-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
			req = req.WithContext(context.WithValue(req.Context(), ctxGitHubLoginKey{}, "bob"))
			rec := httptest.NewRecorder()

			switch tt.path {
			case "/api/claws/claw-1":
				req.SetPathValue("id", "claw-1")
				s.handleClawDetail(rec, req)
			case "/api/messages/claw-1":
				s.handleMessages(rec, req)
			default:
				s.handleClawSubresource(rec, req)
			}

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected status %d, got %d", http.StatusForbidden, rec.Code)
			}
		})
	}
}

func TestGitHubMessagesRequireInteractAccessOnly(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Auth: &types.AuthConfig{
			Access: &types.AccessConfig{
				ViewRequiresTags:     []string{"viewer={user}"},
				InteractRequiresTags: []string{"operator={user}"},
			},
		},
	}, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-1", "test-tenant-id", "claw 1", `["operator=bob"]`,
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/messages/claw-1", strings.NewReader(`{"content":"hello"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	req = req.WithContext(context.WithValue(req.Context(), ctxGitHubLoginKey{}, "bob"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
}

func TestHandleMessagesFiltersWakeMarkers(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-1", "test-tenant-id", "claw 1", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range []types.HubMessage{
		{ID: "wake-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "system", Content: wakeMessageMarker, CreatedAt: now()},
		{ID: "plan-required-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "system", Content: initialPlanRequiredMarker, CreatedAt: now()},
		{ID: "plan-accepted-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "system", Content: initialPlanAcceptedMarker, CreatedAt: now()},
		{ID: "plan-correction-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "system", Content: initialPlanCorrectionSentMarker, CreatedAt: now()},
		{ID: "user-1", ClawID: "claw-1", TenantID: "test-tenant-id", Role: "user", Content: "hello", CreatedAt: now()},
	} {
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
			msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.CreatedAt,
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != "user-1" {
		t.Fatalf("expected only user message, got %#v", msgs)
	}
}

func TestMessageTimelineSummarizesActivityWithoutCrowdingConversation(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	insertMessage := func(id, role, content string, offsetSeconds int) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, "claw-1", "test-tenant-id", role, content, "", base.Add(time.Duration(offsetSeconds)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertMessage("user-1", "user", "start", 1)
	for i := 0; i < 120; i++ {
		insertMessage(fmt.Sprintf("activity-%03d", i), "activity", "tool", 2+i)
	}
	insertMessage("claw-1", "claw", "done", 200)

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/timeline", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("timeline len = %d, want 3: %#v", len(msgs), msgs)
	}
	if msgs[0].ID != "user-1" || msgs[1].Role != "activity_summary" || msgs[2].ID != "claw-1" {
		t.Fatalf("timeline did not preserve conversation with summary: %#v", msgs)
	}
	if !strings.Contains(msgs[1].Format, `"count":120`) {
		t.Fatalf("summary format missing count: %s", msgs[1].Format)
	}
}

func TestMessageActivityEndpointExpandsSummaryRange(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("activity-%d", i), "claw-1", "test-tenant-id", "activity", "tool", `activity:{"kind":"tool"}`, base.Add(time.Duration(i+1)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/activity?limit=2", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != "activity-0" || msgs[1].ID != "activity-1" {
		t.Fatalf("activity messages = %#v, want first two", msgs)
	}
}

func TestMessageActivityEndpointCanReturnNewestActivities(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("activity-%d", i), "claw-1", "test-tenant-id", "activity", "tool", `activity:{"kind":"tool"}`, base.Add(time.Duration(i+1)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/activity?limit=2&order=desc", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != "activity-5" || msgs[1].ID != "activity-4" {
		t.Fatalf("activity messages = %#v, want newest two in descending order", msgs)
	}
}

func TestMessageTimelineIncludesActivityBeforeFirstConversationMessage(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("activity-%d", i), "claw-1", "test-tenant-id", "activity", "tool", `activity:{"kind":"tool"}`, base.Add(time.Duration(i+1)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
		"hub-1", "claw-1", "test-tenant-id", "hub", "Injected proceed message", "", base.Add(10*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/timeline", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "activity_summary" || msgs[1].ID != "hub-1" {
		t.Fatalf("timeline = %#v, want pre-message activity summary then hub message", msgs)
	}
	meta := decodeActivitySummaryMeta(t, msgs[0])
	if meta.Count != 4 {
		t.Fatalf("pre-message summary count = %d, want 4", meta.Count)
	}
}

func TestMessageTimelinePreservesDisplayedStateAcrossActivityRuns(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	offset := 0
	insertMessage := func(id, role, content, format string) {
		t.Helper()
		offset++
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, "claw-1", "test-tenant-id", role, content, format, base.Add(time.Duration(offset)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertActivityRun := func(prefix string, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			insertMessage(fmt.Sprintf("%s-%03d", prefix, i), "activity", "tool", `activity:{"kind":"tool","tool":"exec"}`)
		}
	}

	insertMessage("hub-1", "hub", "Injected proceed message", "")
	insertActivityRun("activity-a", 35)
	insertMessage("claw-1", "claw", "Assistant message 1", "")
	insertActivityRun("activity-b", 65)
	insertMessage("claw-2", "claw", "Assistant message 2", "")
	insertActivityRun("activity-c", 150)
	insertMessage("claw-3", "claw", "Assistant message 3", "")

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/timeline", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var timeline []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&timeline); err != nil {
		t.Fatal(err)
	}

	type expectedItem struct {
		role    string
		id      string
		content string
		count   int
	}
	want := []expectedItem{
		{role: "hub", id: "hub-1", content: "Injected proceed message"},
		{role: "activity_summary", count: 35},
		{role: "claw", id: "claw-1", content: "Assistant message 1"},
		{role: "activity_summary", count: 65},
		{role: "claw", id: "claw-2", content: "Assistant message 2"},
		{role: "activity_summary", count: 150},
		{role: "claw", id: "claw-3", content: "Assistant message 3"},
	}
	if len(timeline) != len(want) {
		t.Fatalf("timeline len = %d, want %d: %#v", len(timeline), len(want), timeline)
	}

	for i, wantItem := range want {
		got := timeline[i]
		if got.Role != wantItem.role {
			t.Fatalf("timeline[%d].role = %q, want %q; item=%#v", i, got.Role, wantItem.role, got)
		}
		if wantItem.id != "" && got.ID != wantItem.id {
			t.Fatalf("timeline[%d].id = %q, want %q", i, got.ID, wantItem.id)
		}
		if wantItem.content != "" && got.Content != wantItem.content {
			t.Fatalf("timeline[%d].content = %q, want %q", i, got.Content, wantItem.content)
		}
		if wantItem.count > 0 {
			meta := decodeActivitySummaryMeta(t, got)
			if meta.Count != wantItem.count {
				t.Fatalf("timeline[%d] summary count = %d, want %d", i, meta.Count, wantItem.count)
			}
			expanded := getActivityMessagesForSummary(t, s, got)
			if len(expanded) != wantItem.count {
				t.Fatalf("timeline[%d] expanded activity len = %d, want %d", i, len(expanded), wantItem.count)
			}
		}
	}
}

func TestStreamingSegmentsPersistAroundActivityForRefreshTimeline(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cc := &clawConn{id: "claw-1", tenantID: "test-tenant-id"}
	persistSegment := func(id, content string, offsetSeconds int) {
		t.Helper()
		cc.mu.Lock()
		cc.streamingMsgID = id
		cc.streamingBuf.WriteString(content)
		cc.mu.Unlock()
		if err := s.flushStreamingSegment("claw-1", "test-tenant-id", cc); err != nil {
			t.Fatal(err)
		}
		_, err := db.Exec(`UPDATE messages SET created_at=? WHERE id=?`, base.Add(time.Duration(offsetSeconds)*time.Second), id)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertActivity := func(id string, offsetSeconds int) {
		t.Helper()
		_, err := db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, "claw-1", "test-tenant-id", "activity", "exec", `activity:{"kind":"tool","tool":"exec"}`, base.Add(time.Duration(offsetSeconds)*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	persistSegment("seg-1", "Assistant segment 1", 1)
	insertActivity("activity-1", 2)
	persistSegment("seg-2", "Assistant segment 2", 3)
	insertActivity("activity-2", 4)
	_, err = db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
		"seg-3", "claw-1", "test-tenant-id", "claw", "Assistant segment 3", "", base.Add(5*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-1/timeline", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var timeline []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&timeline); err != nil {
		t.Fatal(err)
	}
	if len(timeline) != 5 {
		t.Fatalf("timeline len = %d, want 5: %#v", len(timeline), timeline)
	}
	wantRoles := []string{"claw", "activity_summary", "claw", "activity_summary", "claw"}
	wantIDs := []string{"seg-1", "", "seg-2", "", "seg-3"}
	for i := range wantRoles {
		if timeline[i].Role != wantRoles[i] {
			t.Fatalf("timeline[%d].role = %q, want %q; timeline=%#v", i, timeline[i].Role, wantRoles[i], timeline)
		}
		if wantIDs[i] != "" && timeline[i].ID != wantIDs[i] {
			t.Fatalf("timeline[%d].id = %q, want %q", i, timeline[i].ID, wantIDs[i])
		}
		if timeline[i].Role == "activity_summary" {
			meta := decodeActivitySummaryMeta(t, timeline[i])
			if meta.Count != 1 {
				t.Fatalf("timeline[%d] activity count = %d, want 1", i, meta.Count)
			}
		}
	}
}

func TestClawRegistrationPreservesTemplateWhenBridgeOmitsIt(t *testing.T) {
	ready := true
	clawID := "claw-empty-template-registration"
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "AMB-6", "adversarylabs", "starting",
	); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw ws: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "done") })
	if err := wsjson.Write(ctx, conn, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID:       clawID,
			Name:         "AMB-6",
			Template:     "",
			Token:        "claw-token",
			GatewayReady: &ready,
		},
	}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(ctx, conn, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}

	var templateName string
	if err := db.QueryRow(`SELECT template FROM claws WHERE id=?`, clawID).Scan(&templateName); err != nil {
		t.Fatal(err)
	}
	if templateName != "adversarylabs" {
		t.Fatalf("template = %q, want adversarylabs", templateName)
	}
}

func TestClawWorkspaceNameFallsBackToTags(t *testing.T) {
	tests := []struct {
		name         string
		templateName string
		tagsJSON     string
		want         string
	}{
		{name: "database value", templateName: "stored-workspace", tagsJSON: `["workspace:tagged-workspace"]`, want: "stored-workspace"},
		{name: "workflow workspace tags", tagsJSON: `["template:legacy-workspace","workspace:adversarylabs","workflow:linear-todo"]`, want: "adversarylabs"},
		{name: "unpaired workspace tag", tagsJSON: `["workspace:adversarylabs","linear"]`, want: ""},
		{name: "invalid tags", tagsJSON: `{`, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := clawWorkspaceName(test.templateName, test.tagsJSON); got != test.want {
				t.Fatalf("clawWorkspaceName(%q, %q) = %q, want %q", test.templateName, test.tagsJSON, got, test.want)
			}
		})
	}
}

func TestSplitStreamingTurnDoesNotBroadcastGhostFinalMessage(t *testing.T) {
	ready := true
	clawID := "claw-ghost-final-message"
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	userCtx, cancelUser := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelUser()
	userWS, _, err := websocket.Dial(userCtx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws?token=test-token", nil)
	if err != nil {
		t.Fatalf("dial user ws: %v", err)
	}
	t.Cleanup(func() { _ = userWS.Close(websocket.StatusNormalClosure, "done") })

	clawCtx, cancelClaw := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClaw()
	clawWS, _, err := websocket.Dial(clawCtx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw ws: %v", err)
	}
	t.Cleanup(func() { _ = clawWS.Close(websocket.StatusNormalClosure, "done") })
	if err := wsjson.Write(clawCtx, clawWS, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID:       clawID,
			Name:         "claw 1",
			Template:     "elasticclaw",
			Token:        "claw-token",
			GatewayReady: &ready,
		},
	}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(clawCtx, clawWS, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}
	if registered.Type != "registered" {
		t.Fatalf("registration ack type = %q, want registered", registered.Type)
	}

	if err := wsjson.Write(clawCtx, clawWS, types.WSMessage{
		Type:    "chunk",
		Payload: map[string]string{"content": "Assistant segment 1"},
	}); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	if err := wsjson.Write(clawCtx, clawWS, types.WSMessage{
		Type: "agent_activity",
		Payload: map[string]interface{}{
			"kind":    "tool",
			"tool":    "exec",
			"command": "echo split",
		},
	}); err != nil {
		t.Fatalf("write activity: %v", err)
	}
	finalContent := "Assistant segment 1\nAssistant segment 2"
	if err := wsjson.Write(clawCtx, clawWS, types.WSMessage{
		Type: "message",
		Payload: types.HubMessage{
			Content: finalContent,
		},
	}); err != nil {
		t.Fatalf("write final message: %v", err)
	}

	seenIdle := false
	seenGhostMessage := false
	readUntil := time.Now().Add(2 * time.Second)
	for time.Now().Before(readUntil) && !seenIdle {
		readCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		var msg types.WSMessage
		err := wsjson.Read(readCtx, userWS, &msg)
		cancel()
		if err != nil {
			continue
		}
		switch msg.Type {
		case "message":
			payload, _ := json.Marshal(msg.Payload)
			var hm types.HubMessage
			if err := json.Unmarshal(payload, &hm); err == nil && hm.Content == finalContent {
				seenGhostMessage = true
			}
		case "agent_typing":
			payload, _ := json.Marshal(msg.Payload)
			var typing struct {
				ClawID string `json:"claw_id"`
				Status string `json:"status"`
			}
			if err := json.Unmarshal(payload, &typing); err == nil && typing.ClawID == clawID && typing.Status == "idle" {
				seenIdle = true
			}
		}
	}
	if !seenIdle {
		t.Fatal("did not observe final idle typing event")
	}
	if seenGhostMessage {
		t.Fatal("observed unpersisted final full-response message over user websocket")
	}

	var finalRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='claw' AND content=?`, clawID, finalContent).Scan(&finalRows); err != nil {
		t.Fatal(err)
	}
	if finalRows != 0 {
		t.Fatalf("final full-response rows = %d, want 0", finalRows)
	}
	var segmentRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='claw' AND content=?`, clawID, "Assistant segment 1").Scan(&segmentRows); err != nil {
		t.Fatal(err)
	}
	if segmentRows != 1 {
		t.Fatalf("persisted segment rows = %d, want 1", segmentRows)
	}
}

func TestClawWSPipelineDoneTriggerTracksAnalytics(t *testing.T) {
	ready := true
	clawID := "claw-pipeline-done-ws"
	cfg := &types.HubConfig{
		Token:     "test-token",
		ClawToken: "claw-token",
		Factories: []*types.FactoryConfig{
			{
				Name:     "faster_apps",
				Template: "elasticclaw",
				PipelineYAML: `
stages:
  - id: working
    label: Working
    entry: true
  - id: android_validation
    label: Android Validation
    triggers:
      - message_contains: "[DONE]"
    on_enter:
      inject: "Android validation started"
`,
			},
		},
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, linear_issue_id, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "NEXT-257", "elasticclaw", "connected", `["factory:faster_apps"]`, "NEXT-257", "working",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clawWS, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw ws: %v", err)
	}
	t.Cleanup(func() { _ = clawWS.Close(websocket.StatusNormalClosure, "done") })
	if err := wsjson.Write(ctx, clawWS, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID:       clawID,
			Name:         "NEXT-257",
			Template:     "elasticclaw",
			Token:        "claw-token",
			GatewayReady: &ready,
		},
	}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(ctx, clawWS, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}
	if registered.Type != "registered" {
		t.Fatalf("registration ack type = %q, want registered", registered.Type)
	}
	if err := wsjson.Write(ctx, clawWS, types.WSMessage{
		Type: "message",
		Payload: types.HubMessage{
			Content: "[DONE]",
		},
	}); err != nil {
		t.Fatalf("write done message: %v", err)
	}

	var stage string
	var analyticsCount int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage)
		_ = db.QueryRow(`SELECT COUNT(*) FROM factory_analytics WHERE claw_id=? AND issue_id=? AND factory_name=? AND action='done_signal'`, clawID, "NEXT-257", "factory:faster_apps").Scan(&analyticsCount)
		if stage == "android_validation" && analyticsCount == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if stage != "android_validation" {
		t.Fatalf("pipeline_stage = %q, want android_validation", stage)
	}
	if analyticsCount != 1 {
		t.Fatalf("done_signal analytics count = %d, want 1", analyticsCount)
	}

	var noPRWarnings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user' AND content LIKE '%no PR URLs%'`, clawID).Scan(&noPRWarnings); err != nil {
		t.Fatalf("count no-pr warnings: %v", err)
	}
	if noPRWarnings != 0 {
		t.Fatalf("expected no PR URL warning to be suppressed, got %d", noPRWarnings)
	}
}

func decodeActivitySummaryMeta(t *testing.T, msg types.HubMessage) activitySummaryMeta {
	t.Helper()
	if !strings.HasPrefix(msg.Format, "activity_summary:") {
		t.Fatalf("summary format = %q, want activity_summary prefix", msg.Format)
	}
	var meta activitySummaryMeta
	if err := json.Unmarshal([]byte(strings.TrimPrefix(msg.Format, "activity_summary:")), &meta); err != nil {
		t.Fatalf("decode summary meta: %v", err)
	}
	return meta
}

func getActivityMessagesForSummary(t *testing.T, s *Server, msg types.HubMessage) []types.HubMessage {
	t.Helper()
	meta := decodeActivitySummaryMeta(t, msg)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/messages/%s/activity?from=%s&to=%s&limit=500", msg.ClawID, url.QueryEscape(meta.From), url.QueryEscape(meta.To)), nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, msg.TenantID))
	rec := httptest.NewRecorder()

	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var msgs []types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestInitialPlanWakePromptRequiresVisiblePlanBeforeWork(t *testing.T) {
	required := []string{
		"Initial plan required before implementation",
		"Before editing files, running builds, or doing broad tool exploration",
		"Your understanding of the issue or task",
		"The likely area of the codebase or behavior involved",
		"A rough implementation plan",
		"What you will verify or test",
		"Tool calls, activity rows, and update_plan do not count",
		"wait for the hub's proceed message",
	}
	for _, want := range required {
		if !strings.Contains(initialPlanWakeContent, want) {
			t.Fatalf("initial plan wake content missing %q:\n%s", want, initialPlanWakeContent)
		}
	}
}

func TestIsValidInitialPlanRequiresUnderstandingPlanAreaAndVerification(t *testing.T) {
	valid := `I understand the issue is that automated workflow agents can spend too long working before the user sees a useful summary. The likely code area is the hub startup and message handling code, especially the wake prompt and bridge or server files that manage workflow claws. My plan is to add an initial planning checkpoint, persist its state, validate the first visible assistant message, and only then send a proceed instruction. I will verify this with focused hub tests and a package test run.`
	if !isValidInitialPlan(valid) {
		t.Fatalf("valid initial plan was rejected")
	}
	invalid := "Good, build passes. Now let me read the existing test files."
	if isValidInitialPlan(invalid) {
		t.Fatalf("invalid initial plan was accepted")
	}
}

func TestHandleInitialPlanResponseMarksAcceptedOrCorrection(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-plan", "test-tenant-id", "claw plan", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	s.insertSystemMarker("claw-plan", "test-tenant-id", initialPlanRequiredMarker)
	s.handleInitialPlanResponse("claw-plan", "test-tenant-id", "Good, build passes. Now let me read the existing test files.")
	if !s.hasSystemMarker("claw-plan", initialPlanCorrectionSentMarker) {
		t.Fatalf("invalid initial plan did not mark correction sent")
	}
	if s.hasSystemMarker("claw-plan", initialPlanAcceptedMarker) {
		t.Fatalf("invalid initial plan was accepted")
	}

	_, err = db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-valid-plan", "test-tenant-id", "claw valid plan", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	s.insertSystemMarker("claw-valid-plan", "test-tenant-id", initialPlanRequiredMarker)
	valid := `I understand the issue is that the agent is not reliably sending a visible plan before it starts implementation. The likely code area is the hub server message flow and workflow wake handling code. My plan is to add persisted plan-required state, validate the first assistant message, and send a proceed instruction only after the plan is accepted. I will verify the change with focused server tests and the hub package tests.`
	s.handleInitialPlanResponse("claw-valid-plan", "test-tenant-id", valid)
	if !s.hasSystemMarker("claw-valid-plan", initialPlanAcceptedMarker) {
		t.Fatalf("valid initial plan was not accepted")
	}
	if s.hasSystemMarker("claw-valid-plan", initialPlanCorrectionSentMarker) {
		t.Fatalf("valid initial plan marked correction sent")
	}
}

func TestHandleInitialPlanActivityMarksCorrectionOnToolBeforePlan(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-tool-before-plan", "test-tenant-id", "claw tool before plan", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	s.insertSystemMarker("claw-tool-before-plan", "test-tenant-id", initialPlanRequiredMarker)
	s.handleInitialPlanActivity("claw-tool-before-plan", "test-tenant-id", map[string]interface{}{"kind": "tool", "tool": "exec"})
	if !s.hasSystemMarker("claw-tool-before-plan", initialPlanCorrectionSentMarker) {
		t.Fatalf("tool activity before initial plan did not mark correction sent")
	}
}

func TestInsertSystemMarkerReportsOnlyFirstInsert(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-marker", "test-tenant-id", "claw marker", `[]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !s.insertSystemMarker("claw-marker", "test-tenant-id", initialPlanRequiredMarker) {
		t.Fatalf("first marker insert returned false")
	}
	if s.insertSystemMarker("claw-marker", "test-tenant-id", initialPlanRequiredMarker) {
		t.Fatalf("duplicate marker insert returned true")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='system' AND content=?`, "claw-marker", initialPlanRequiredMarker).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one marker row, got %d", count)
	}
}

func TestWebAdminAuthRequiresAccessAdminForGitHubSession(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "hub-token",
		Auth: &types.AuthConfig{
			SessionSecret: "session-secret",
			GitHubOAuth:   &types.GitHubOAuthConfig{ClientSecret: "oauth-secret"},
			Access:        &types.AccessConfig{Admins: []string{"admin-user"}},
		},
	}, "", "", "")

	forgedSession, err := signGitHubSession("hub-token", "admin-user", "", "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer "+forgedSession)
	rec := httptest.NewRecorder()

	s.withWebAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	})(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, rec.Code)
	}

	oauthSecretSession, err := signGitHubSession("oauth-secret", "admin-user", "", "")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer "+oauthSecretSession)
	rec = httptest.NewRecorder()

	s.withWebAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	})(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, rec.Code)
	}

	session, err := signGitHubSession("session-secret", "regular-user", "", "")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer "+session)
	rec = httptest.NewRecorder()

	s.withWebAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler should not be called")
	})(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d", http.StatusForbidden, rec.Code)
	}

	adminSession, err := signGitHubSession("session-secret", "admin-user", "", "")
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.Header.Set("Authorization", "Bearer "+adminSession)
	rec = httptest.NewRecorder()

	s.withWebAdminAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, rec.Code)
	}
}

func TestAdminForMethodsRequiresAdminForMutations(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "hub-token",
		Auth: &types.AuthConfig{
			SessionSecret: "session-secret",
			GitHubOAuth:   &types.GitHubOAuthConfig{ClientSecret: "oauth-secret"},
			Access:        &types.AccessConfig{Admins: []string{"admin-user"}},
		},
	}, "", "", "")

	regularSession, err := signGitHubSession("session-secret", "regular-user", "", "")
	if err != nil {
		t.Fatal(err)
	}
	adminSession, err := signGitHubSession("session-secret", "admin-user", "", "")
	if err != nil {
		t.Fatal(err)
	}

	var calls int
	handler := s.withAdminForMethods(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	}, http.MethodPost)

	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("Authorization", "Bearer "+regularSession)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected read method to allow regular user, got %d", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("expected handler to be called once, got %d", calls)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/config", nil)
	req.Header.Set("Authorization", "Bearer "+regularSession)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected mutation method to reject regular user with %d, got %d", http.StatusForbidden, rec.Code)
	}
	if calls != 1 {
		t.Fatalf("expected handler not to be called for rejected mutation, got %d calls", calls)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/config", nil)
	req.Header.Set("Authorization", "Bearer "+adminSession)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected mutation method to allow admin user, got %d", rec.Code)
	}
	if calls != 2 {
		t.Fatalf("expected handler to be called for admin mutation, got %d calls", calls)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/config", nil)
	req.Header.Set("Authorization", "Bearer hub-token")
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected mutation method to allow hub token, got %d", rec.Code)
	}
	if calls != 3 {
		t.Fatalf("expected handler to be called for hub-token mutation, got %d calls", calls)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/config?token=hub-token", nil)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected mutation method to allow hub query token, got %d", rec.Code)
	}
	if calls != 4 {
		t.Fatalf("expected handler to be called for hub query-token mutation, got %d calls", calls)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/config?token=test-token", nil)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected mutation method to reject tenant query token with %d, got %d", http.StatusUnauthorized, rec.Code)
	}
	if calls != 4 {
		t.Fatalf("expected handler not to be called for tenant query-token mutation, got %d calls", calls)
	}
}

func TestConfigMutationRoutesRequireWebAdminForGitHubSessions(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "hub-token",
		Auth: &types.AuthConfig{
			SessionSecret: "session-secret",
			GitHubOAuth:   &types.GitHubOAuthConfig{ClientSecret: "oauth-secret"},
			Access:        &types.AccessConfig{Admins: []string{"admin-user"}},
		},
	}, "", "", "")
	session, err := signGitHubSession("session-secret", "regular-user", "", "")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "factory push", method: http.MethodPost, path: "/api/factories", body: `{"factories":[]}`},
		{name: "factory delete", method: http.MethodDelete, path: "/api/factories?name=demo"},
		{name: "workspace push", method: http.MethodPost, path: "/api/workspaces", body: `{"workspaces":[]}`},
		{name: "workspace delete", method: http.MethodDelete, path: "/api/workspaces?name=demo"},
		{name: "workflow push", method: http.MethodPost, path: "/api/workspaces/demo/workflows", body: `{"workflows":[]}`},
		{name: "workflow patch", method: http.MethodPatch, path: "/api/workspaces/demo/workflows/build", body: `{"enabled":true}`},
		{name: "workspace secret upsert", method: http.MethodPut, path: "/api/workspaces/demo/secrets", body: `{"name":"TOKEN","value":"secret"}`},
		{name: "workspace secret delete", method: http.MethodDelete, path: "/api/workspaces/demo/secrets?name=TOKEN"},
		{name: "workspace github app upsert", method: http.MethodPost, path: "/api/workspaces/demo/github-apps", body: `{"name":"app","appId":1}`},
		{name: "workspace github app delete", method: http.MethodDelete, path: "/api/workspaces/demo/github-apps?name=app"},
		{name: "workspace issue tracker upsert", method: http.MethodPost, path: "/api/workspaces/demo/issue-trackers", body: `{"type":"linear","workspace":"eng","token":"token"}`},
		{name: "workspace issue tracker delete", method: http.MethodDelete, path: "/api/workspaces/demo/issue-trackers?type=linear&workspace=eng"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer "+session)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected status %d, got %d: %s", http.StatusForbidden, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestProvisionReplicatedDefersEnvInjectionToBootstrap(t *testing.T) {
	var createRequests int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/vm" {
			http.NotFound(w, r)
			return
		}
		createRequests++
		jsonOK(w, map[string]interface{}{
			"vms": []map[string]string{{"id": "vm-test-1"}},
		})
	}))
	t.Cleanup(api.Close)

	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Providers: map[string]types.ProviderConfig{
			"replicated": {
				Token:  "replicated-token",
				APIURL: api.URL,
			},
		},
	}, "", "", "")
	s.identity = &HubIdentity{PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest elasticclaw@hub"}
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, provider, status, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		"claw-replicated-env", "test-tenant-id", "replicated-env", "template", "replicated", "provisioning",
	)
	if err != nil {
		t.Fatal(err)
	}

	err = s.provisionReplicated(
		context.Background(),
		"claw-replicated-env",
		types.CreateClawRequest{Name: "replicated-env", ProviderName: "ec-replicated-env"},
		s.hubCfg.Providers["replicated"],
		map[string]string{"ELASTICCLAW_CLAW_TOKEN": "agent-token"},
	)
	if err != nil {
		t.Fatalf("provisionReplicated with env: %v", err)
	}
	if createRequests != 1 {
		t.Fatalf("replicated create requests = %d, want 1", createRequests)
	}
	var providerID string
	if err := db.QueryRow(`SELECT provider_id FROM claws WHERE id=?`, "claw-replicated-env").Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if providerID != "vm-test-1" {
		t.Fatalf("provider_id = %q, want vm-test-1", providerID)
	}
}

func TestBrandingEndpointIsPublicAndDoesNotExposeToken(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "hub-token",
		Branding: &types.BrandingConfig{
			AppName: "Customer Claw",
			LogoURL: "https://example.com/logo.png",
		},
	}, "", "", "")

	req := httptest.NewRequest(http.MethodGet, "/api/branding", nil)
	rec := httptest.NewRecorder()

	s.handleBranding(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["appName"] != "Customer Claw" {
		t.Fatalf("appName = %q", body["appName"])
	}
	if body["logoUrl"] != "https://example.com/logo.png" {
		t.Fatalf("logoUrl = %q", body["logoUrl"])
	}
	if _, ok := body["token"]; ok {
		t.Fatalf("branding response exposed token: %#v", body)
	}
}

func TestBroadcastToUsersFiltersGitHubSessionsByClawTags(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Auth: &types.AuthConfig{
			Access: &types.AccessConfig{ViewRequiresTags: []string{"owner={user}"}},
		},
	}, "", "", "")

	s.users["allowed"] = &userConn{
		tenantID:    "test-tenant-id",
		githubLogin: "alice",
	}
	s.users["denied"] = &userConn{
		tenantID:    "test-tenant-id",
		githubLogin: "bob",
	}
	s.claws["claw-1"] = &clawConn{id: "claw-1", tenantID: "test-tenant-id", tags: []string{"owner=alice"}}

	recipients := s.broadcastRecipients("test-tenant-id", types.WSMessage{
		Type:    "chunk",
		Payload: map[string]string{"claw_id": "claw-1", "content": "secret"},
	})

	if len(recipients) != 1 {
		t.Fatalf("expected 1 recipient, got %d", len(recipients))
	}
	if recipients[0].githubLogin != "alice" {
		t.Fatalf("expected alice recipient, got %q", recipients[0].githubLogin)
	}
}

func TestNormalizeAgentActivityPayloadRejectsNull(t *testing.T) {
	if activity, raw, ok := normalizeAgentActivityPayload(nil); ok || activity != nil || raw != nil {
		t.Fatalf("nil payload normalized to activity=%v raw=%q ok=%v", activity, raw, ok)
	}
	if activity, raw, ok := normalizeAgentActivityPayload(map[string]interface{}{"kind": "tool", "tool": "exec"}); !ok || activity["tool"] != "exec" || len(raw) == 0 {
		t.Fatalf("valid payload normalized to activity=%v raw=%q ok=%v", activity, raw, ok)
	}
}

func TestBusyAgentActivitySignals(t *testing.T) {
	tests := []struct {
		name     string
		activity map[string]interface{}
		want     bool
	}{
		{
			name:     "model started",
			activity: map[string]interface{}{"kind": "model_started"},
			want:     true,
		},
		{
			name:     "tool running",
			activity: map[string]interface{}{"kind": "tool", "phase": "running"},
			want:     true,
		},
		{
			name:     "tool completed",
			activity: map[string]interface{}{"kind": "tool", "phase": "completed"},
			want:     false,
		},
		{
			name:     "session error",
			activity: map[string]interface{}{"kind": "session_error"},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBusyAgentActivity(tt.activity); got != tt.want {
				t.Fatalf("isBusyAgentActivity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFinishTurnClearsActivityOnlyBusyState(t *testing.T) {
	cc := &clawConn{
		id:                   "claw-1",
		tenantID:             "test-tenant-id",
		streamingStartedAt:   now(),
		streamingTimeoutSent: true,
		contextWarningSent:   true,
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if !cc.isBusyLocked() {
		t.Fatal("expected activity-only turn to be busy")
	}
	cc.finishTurnLocked()
	if cc.isBusyLocked() {
		t.Fatal("expected finishTurnLocked to clear busy state")
	}
	if cc.streamingTimeoutSent || cc.contextWarningSent {
		t.Fatal("expected finishTurnLocked to clear turn warnings")
	}
}

func TestInjectUserMessageQueuesWhenActivityOnlyTurnIsBusy(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at, status) VALUES(?,?,?,?,datetime('now'),?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, "connected",
	)
	if err != nil {
		t.Fatal(err)
	}
	cc := &clawConn{
		id:                 "claw-1",
		tenantID:           "test-tenant-id",
		streamingStartedAt: now(),
	}
	s.claws["claw-1"] = cc

	s.injectUserMessage("claw-1", "New greptile review comment on PR #339")

	var role, content string
	var deliveredAt interface{}
	if err := db.QueryRow(`SELECT role, content, delivered_at FROM messages WHERE claw_id=? AND content=?`, "claw-1", "New greptile review comment on PR #339").Scan(&role, &content, &deliveredAt); err != nil {
		t.Fatal(err)
	}
	if role != "user" || content != "New greptile review comment on PR #339" || deliveredAt != nil {
		t.Fatalf("pending message = role=%q content=%q delivered_at=%v", role, content, deliveredAt)
	}
}

func TestReconnectDeliversQueuedMessagesInOrder(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "claw-reconnect-pending"
	insertPendingMessage(t, db, clawID, "first", now().Add(-time.Second))
	insertPendingMessage(t, db, clawID, "second", now())

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	clawWS := connectTestClaw(t, ts, clawID)
	t.Cleanup(func() { _ = clawWS.Close(websocket.StatusNormalClosure, "done") })

	if got := readTestHubMessage(t, clawWS).Content; got != "first" {
		t.Fatalf("first delivered content = %q, want first", got)
	}
	// A completed agent turn is the normal driver for the next queued message.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, clawWS, types.WSMessage{Type: "message", Payload: types.HubMessage{Content: "turn complete"}}); err != nil {
		t.Fatalf("write completed turn: %v", err)
	}
	if got := readTestHubMessage(t, clawWS).Content; got != "second" {
		t.Fatalf("second delivered content = %q, want second", got)
	}
	waitForMessagesDelivered(t, db, clawID, 2)
}

func TestReconnectRedeliversMessageUnmarkedAfterSuccessfulWrite(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "claw-redeliver-pending"
	insertPendingMessage(t, db, clawID, "deliver me again", now())

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	firstWS := connectTestClaw(t, ts, clawID)
	if got := readTestHubMessage(t, firstWS).Content; got != "deliver me again" {
		t.Fatalf("initial delivered content = %q", got)
	}
	// The hub marks delivered_at after the WS write, so the frame can arrive
	// here before the UPDATE runs. Wait for the mark before resetting it, or
	// the hub's late UPDATE (WHERE delivered_at IS NULL) would re-mark the row.
	waitForMessagesDelivered(t, db, clawID, 1)
	// Model a hub crash after the bridge accepted the write but before the
	// delivered_at update was committed.
	if _, err := db.Exec(`UPDATE messages SET delivered_at=NULL WHERE claw_id=?`, clawID); err != nil {
		t.Fatalf("restore unmarked delivery: %v", err)
	}
	_ = firstWS.Close(websocket.StatusNormalClosure, "restart hub")
	waitForTestClawDisconnect(t, s, clawID)

	clawWS := connectTestClaw(t, ts, clawID)
	t.Cleanup(func() { _ = clawWS.Close(websocket.StatusNormalClosure, "done") })
	if got := readTestHubMessage(t, clawWS).Content; got != "deliver me again" {
		t.Fatalf("redelivered content = %q", got)
	}
	waitForMessagesDelivered(t, db, clawID, 1)
}

func TestSendNextQueuedMessageKeepsPendingAfterWriteFailure(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "claw-write-failure"

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	failedWS := connectTestClaw(t, ts, clawID)
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	insertPendingMessage(t, db, clawID, "retry me", now())
	// Sever the client side abruptly (no close handshake) so the server's
	// next write to this connection fails, without touching cc.conn directly
	// (which would race the server's own read/close goroutines).
	if err := failedWS.CloseNow(); err != nil {
		t.Fatalf("close client websocket: %v", err)
	}
	waitForTestClawDisconnect(t, s, clawID)
	// The closed peer makes the write fail; the row must remain eligible.
	s.sendNextQueuedMessage(cc)
	assertMessagesDelivered(t, db, clawID, 0)

	retryWS := connectTestClaw(t, ts, clawID)
	t.Cleanup(func() { _ = retryWS.Close(websocket.StatusNormalClosure, "done") })
	if got := readTestHubMessage(t, retryWS).Content; got != "retry me" {
		t.Fatalf("retried content = %q", got)
	}
	waitForMessagesDelivered(t, db, clawID, 1)
}

func insertPendingMessage(t *testing.T, db *sql.DB, clawID, content string, createdAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO claws(id,tenant_id,name,tags,created_at,status) VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, `[]`, createdAt, "offline"); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`, "pending-"+content, clawID, "test-tenant-id", "user", content, createdAt); err != nil {
		t.Fatalf("insert pending message: %v", err)
	}
}

func connectTestClaw(t *testing.T, ts *httptest.Server, clawID string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw websocket: %v", err)
	}
	ready := true
	if err := wsjson.Write(ctx, conn, types.WSMessage{Type: "register", Payload: types.RegisterPayload{ClawID: clawID, Name: clawID, Template: "elasticclaw", Token: "claw-token", GatewayReady: &ready}}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(ctx, conn, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}
	if registered.Type != "registered" {
		t.Fatalf("registration ack type = %q, want registered", registered.Type)
	}
	return conn
}

func readTestHubMessage(t *testing.T, conn *websocket.Conn) types.HubMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The connection bootstrap may deliver unrelated frames (e.g. an async
	// checkpoint_create request) before the queued message; skip those.
	var message types.WSMessage
	for {
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			t.Fatalf("read delivered message: %v", err)
		}
		if message.Type == "message" {
			break
		}
	}
	payload, err := json.Marshal(message.Payload)
	if err != nil {
		t.Fatalf("marshal message payload: %v", err)
	}
	var hubMessage types.HubMessage
	if err := json.Unmarshal(payload, &hubMessage); err != nil {
		t.Fatalf("decode message payload: %v", err)
	}
	return hubMessage
}

func assertMessagesDelivered(t *testing.T, db *sql.DB, clawID string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user' AND delivered_at IS NOT NULL`, clawID).Scan(&got); err != nil {
		t.Fatalf("count delivered messages: %v", err)
	}
	if got != want {
		t.Fatalf("delivered messages = %d, want %d", got, want)
	}
}

func waitForMessagesDelivered(t *testing.T, db *sql.DB, clawID string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user' AND delivered_at IS NOT NULL`, clawID).Scan(&got); err != nil {
			t.Fatalf("count delivered messages: %v", err)
		}
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d delivered messages", want)
}

func waitForTestClawDisconnect(t *testing.T, s *Server, clawID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		_, connected := s.claws[clawID]
		s.mu.RUnlock()
		if !connected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for claw disconnect")
}

func TestInaccessibleGitHubReposMessage(t *testing.T) {
	repos := sortedStringKeys(map[string]bool{
		"praetoriandigital/ws-notification-lambdas":         true,
		"praetoriandigital/accreditation-workbench-lambdas": true,
	})

	got := inaccessibleGitHubReposMessage(repos)
	want := "GitHub App cannot access: praetoriandigital/accreditation-workbench-lambdas, praetoriandigital/ws-notification-lambdas"
	if got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

func TestMergeDockerContainerEnvPreservesWorkflowSecrets(t *testing.T) {
	requestEnv := map[string]string{
		"JIRA_API_KEY":        "workspace-secret",
		"ELASTICCLAW_HUB_URL": "http://untrusted.example",
	}
	managedEnv := map[string]string{
		"ELASTICCLAW_HUB_URL": "http://host.docker.internal:8080",
		"ELASTICCLAW_CLAW_ID": "claw-123",
	}

	got := mergeDockerContainerEnv(requestEnv, managedEnv)

	if got["JIRA_API_KEY"] != "workspace-secret" {
		t.Fatalf("JIRA_API_KEY = %q, want workspace secret", got["JIRA_API_KEY"])
	}
	if got["ELASTICCLAW_HUB_URL"] != managedEnv["ELASTICCLAW_HUB_URL"] {
		t.Fatalf("ELASTICCLAW_HUB_URL = %q, want managed value %q", got["ELASTICCLAW_HUB_URL"], managedEnv["ELASTICCLAW_HUB_URL"])
	}
	if got["ELASTICCLAW_CLAW_ID"] != "claw-123" {
		t.Fatalf("ELASTICCLAW_CLAW_ID = %q, want managed claw ID", got["ELASTICCLAW_CLAW_ID"])
	}
}
