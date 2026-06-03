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
			expectedModel: "fireworks/accounts/fireworks/models/kimi-k2p6",
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
			name: "deepseek provider",
			hubCfg: &types.HubConfig{
				DefaultModel: "",
			},
			key: &types.LLMKeyConfig{
				Provider: "deepseek",
			},
			expectedModel: "deepseek/deepseek-chat",
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
