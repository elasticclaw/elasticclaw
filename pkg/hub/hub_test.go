package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/elasticclaw/elasticclaw/pkg/types"
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
			expectedModel: "codex/gpt-5.5",
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

	// The deprecated ?token= query fallback was removed (Phase 2.6): even a
	// valid hub token in the query string is rejected.
	req = httptest.NewRequest(http.MethodPost, "/api/config?token=hub-token", nil)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected mutation method to reject hub query token with %d, got %d", http.StatusUnauthorized, rec.Code)
	}
	if calls != 3 {
		t.Fatalf("expected handler not to be called for hub query-token mutation, got %d calls", calls)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/config?token=test-token", nil)
	rec = httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected mutation method to reject tenant query token with %d, got %d", http.StatusUnauthorized, rec.Code)
	}
	if calls != 3 {
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

	s.userReg.set("allowed", &userConn{
		tenantID:    "test-tenant-id",
		githubLogin: "alice",
	})
	s.userReg.set("denied", &userConn{
		tenantID:    "test-tenant-id",
		githubLogin: "bob",
	})
	s.clawReg.Set("claw-1", &clawConn{ClawID: "claw-1", TenantID: "test-tenant-id", Tags: []string{"owner=alice"}})

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
	if activity, raw, ok := normalizeAgentActivityPayload(json.RawMessage(`{"kind":"tool","tool":"exec"}`)); !ok || activity["tool"] != "exec" || len(raw) == 0 {
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
		ClawID:               "claw-1",
		TenantID:             "test-tenant-id",
		StreamingStartedAt:   now(),
		StreamingTimeoutSent: true,
		ContextWarningSent:   true,
	}
	cc.Mu.Lock()
	defer cc.Mu.Unlock()
	if !cc.BusyLocked() {
		t.Fatal("expected activity-only turn to be busy")
	}
	cc.FinishTurnLocked()
	if cc.BusyLocked() {
		t.Fatal("expected finishTurnLocked to clear busy state")
	}
	if cc.StreamingTimeoutSent || cc.ContextWarningSent {
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
		ClawID:             "claw-1",
		TenantID:           "test-tenant-id",
		StreamingStartedAt: now(),
	}
	s.clawReg.Set("claw-1", cc)

	s.injectUserMessage("claw-1", "New greptile review comment on PR #339")

	cc.Mu.Lock()
	if len(cc.MessageQueue) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(cc.MessageQueue))
	}
	queued := cc.MessageQueue[0]
	cc.Mu.Unlock()
	if queued.Role != "user" || queued.Content != "New greptile review comment on PR #339" {
		t.Fatalf("queued message = %#v", queued)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=?`, "claw-1", queued.Content).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected persisted injected message, got count %d", count)
	}
}
