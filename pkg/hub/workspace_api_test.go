package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestWorkspacesEndpointReturnsPersistedWorkspacesOnly(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Factories: []*types.FactoryConfig{
			{Name: "legacy", Integration: "github", Template: "bugbot", EnableManualTrigger: true},
		},
	}, "", "", "")

	req := httptest.NewRequest(http.MethodGet, "/api/workspaces", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var workspaces []WorkspaceView
	if err := json.Unmarshal(rr.Body.Bytes(), &workspaces); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(workspaces) != 0 {
		t.Fatalf("expected no workspaces, got %#v", workspaces)
	}
}

func TestWorkflowPushPersistsWorkspaceWorkflows(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", configDir+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	body := `{"workspaces":[{"name":"engineering","repositories":["elasticclaw/elasticclaw"],"secrets":["openai_api_key"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"name":"bugfix","integration":"github","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/bugfix", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var workflow WorkflowView
	if err := json.Unmarshal(rr.Body.Bytes(), &workflow); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if workflow.Name != "bugfix" || workflow.WorkspaceName != "engineering" || workflow.Source != "workflow" {
		t.Fatalf("unexpected workflow: %#v", workflow)
	}

	body = `{"workflows":[{"name":"second","integration":"github","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/bugfix", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("existing workflow status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows/second", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second workflow detail status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"name":"Bugfix","integration":"github","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("case-variant workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/workspaces/engineering/workflows", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow list status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var workflows []WorkflowView
	if err := json.Unmarshal(rr.Body.Bytes(), &workflows); err != nil {
		t.Fatalf("decode workflows: %v", err)
	}
	if len(workflows) != 2 {
		t.Fatalf("workflow count = %d, want 2: %#v", len(workflows), workflows)
	}

	staleWorkflowPath := filepath.Join(configDir, "workspaces", "engineering", "workflows", "bugfix.yaml")
	if err := os.WriteFile(staleWorkflowPath, []byte("schema_version: v1\nname: bugfix\njobs:\n  - id: old\n"), 0640); err != nil {
		t.Fatalf("write stale workflow: %v", err)
	}

	body = `{"workflows":[{"name":"bugfix","integration":"github","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("repair workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestWorkflowPushAcceptsNestedGitHubIssuesTrigger(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", configDir+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	body := `{"workspaces":[{"name":"engineering","repositories":["elasticclaw/elasticclaw"]}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workspace push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"schemaVersion":"v1","name":"github-issue","trigger":{"github_issues":{"event":"issue_labeled","repositories":["autoci-ai/autoci"],"states":["open"],"labels":["todo"],"labelers":["marccampbell"]}},"stages":[{"id":"working","entry":true}]}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	workspace, err := loadExternalWorkspace("engineering")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(workspace.Workflows) != 1 {
		t.Fatalf("workflow count = %d, want 1", len(workspace.Workflows))
	}
	trigger := workspace.Workflows[0].Trigger
	if trigger == nil || trigger.GitHubIssues == nil {
		t.Fatalf("github_issues trigger not preserved: %#v", trigger)
	}
	if got := trigger.GitHubIssues.Labelers; len(got) != 1 || got[0] != "marccampbell" {
		t.Fatalf("labelers = %#v, want marccampbell", got)
	}
	if workspace.Workflows[0].Integration != "github-issues" {
		t.Fatalf("integration = %q, want github-issues", workspace.Workflows[0].Integration)
	}
}

func TestWorkspaceWorkflowPatchUpdatesTriggerControls(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", configDir+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	body := `{"workspaces":[{"name":"engineering"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workspace push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"name":"github-issue","trigger":{"github_issues":{"event":"issue_labeled","repositories":["elasticclaw/elasticclaw"],"labels":["agent-ready"],"labelers":["*"]}},"stages":[{"id":"working","entry":true}]}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPatch, "/api/workspaces/engineering/workflows/github-issue", strings.NewReader(`{"enabled":false,"enableManualTrigger":true}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var workflow WorkflowView
	if err := json.Unmarshal(rr.Body.Bytes(), &workflow); err != nil {
		t.Fatalf("decode patched workflow: %v", err)
	}
	if workflow.Enabled || !workflow.EnableManualTrigger {
		t.Fatalf("patched controls = enabled:%v manual:%v, want false/true", workflow.Enabled, workflow.EnableManualTrigger)
	}

	loaded, err := loadExternalWorkspace("engineering")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if len(loaded.Workflows) != 1 {
		t.Fatalf("workflow count = %d, want 1", len(loaded.Workflows))
	}
	persisted := loaded.Workflows[0]
	if persisted.Enabled == nil || *persisted.Enabled || !persisted.EnableManualTrigger {
		t.Fatalf("persisted controls = enabled:%v manual:%v, want false/true", persisted.Enabled, persisted.EnableManualTrigger)
	}
	if persisted.Trigger == nil || persisted.Trigger.GitHubIssues == nil || len(persisted.Stages) != 1 {
		t.Fatalf("patch did not preserve trigger/stages: %#v", persisted)
	}
}

func TestWorkspaceWorkflowPatchRejectsDuplicateManualTriggerFields(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", configDir+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	body := `{"workspaces":[{"name":"engineering"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workspace push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"name":"github-issue","enable_manual_trigger":true}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPatch, "/api/workspaces/engineering/workflows/github-issue", strings.NewReader(`{"enableManualTrigger":true,"enable_manual_trigger":false}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("patch status = %d, want %d, body = %s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "provide only one of enableManualTrigger or enable_manual_trigger") {
		t.Fatalf("unexpected error body: %s", rr.Body.String())
	}
}

func TestWorkspaceWorkflowTriggerUsesWorkflowRules(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	body := `{"workspaces":[{"name":"engineering"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/workspaces", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	body = `{"workflows":[{"name":"disabled","enable_manual_trigger":false}]}`
	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("workflow push status = %d, body = %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows/disabled/trigger", strings.NewReader(`{"inputs":{}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d, body = %s", rr.Code, http.StatusForbidden, rr.Body.String())
	}
}

func TestWorkspaceWorkflowTriggerAllowsPausedManualWorkflow(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	enabled := false
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}, "", "", "")
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "engineering",
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\nprovider: noop\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion:       "v1",
			Name:                "manual-only",
			Enabled:             &enabled,
			EnableManualTrigger: true,
		}},
	)

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows/manual-only/trigger", strings.NewReader(`{"inputs":{}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rr.Code, http.StatusOK, rr.Body.String())
	}
}

func TestWorkspaceWorkflowTriggerCreatesGitHubIssueWorkflowFromIssueNumber(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	var authHeaders []string
	var requestPaths []string
	ghi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		requestPaths = append(requestPaths, r.URL.Path)
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/repos/testorg/testrepo/issues/42":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"number": 42,
				"title": "Manual trigger issue",
				"body": "Issue body",
				"html_url": "https://github.com/testorg/testrepo/issues/42",
				"state": "open",
				"labels": [{"name": "dev-only"}],
				"user": {"login": "testuser"}
			}`))
		case "/repos/testorg/testrepo/issues/42/comments":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{
				"id": 501,
				"body": "First manual comment",
				"html_url": "https://github.com/testorg/testrepo/issues/42#issuecomment-501",
				"created_at": "2026-06-05T20:37:00Z",
				"user": {"login": "manual-reviewer", "type": "User"}
			}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ghi.Close)
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}, ghi.URL, "", "")
	one := 1.0
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "engineering",
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\nprovider: noop\n",
				"CONTEXT.md":              "Workspace context\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion:       "v1",
			Name:                "github-issue",
			EnableManualTrigger: true,
			Inputs: []types.FactoryInput{{
				Name:     "issue_number",
				Type:     "number",
				Required: true,
				Min:      &one,
			}},
			Trigger: &types.WorkflowTrigger{
				GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
					Event:        "issue_labeled",
					Repositories: []string{"testorg/testrepo"},
					States:       []string{"open"},
					Labels:       []string{"agent-ready"},
					Labelers:     []string{"*"},
				},
			},
			Stages: []types.WorkflowStage{{
				ID:    "working",
				Label: "Working",
				Entry: true,
				OnEnter: map[string]interface{}{
					"inject": "Issue: {{.Issue.Identifier}} - {{.Issue.Title}}\n",
				},
			}},
		}},
	)
	SaveWorkspaceIssueTrackerForTest(t, "engineering", "github-issues", "default", "test-github-issues-token", "")

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows/github-issue/trigger", strings.NewReader(`{"inputs":{"issue_number":42}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["claw_id"] == "" {
		t.Fatalf("missing claw_id in response: %#v", resp)
	}
	var issueID, filesJSON string
	if err := db.QueryRow(`SELECT github_issue_id, template_files FROM claws WHERE id=?`, resp["claw_id"]).Scan(&issueID, &filesJSON); err != nil {
		t.Fatalf("load created claw: %v", err)
	}
	if issueID != "testorg/testrepo/42" {
		t.Fatalf("github_issue_id = %q, want testorg/testrepo/42", issueID)
	}
	if !strings.Contains(filesJSON, "Manual trigger issue") || !strings.Contains(filesJSON, "Issue body") || !strings.Contains(filesJSON, "First manual comment") {
		t.Fatalf("template_files missing GitHub issue context: %s", filesJSON)
	}
	if len(authHeaders) != 2 || authHeaders[0] != "Bearer test-github-issues-token" || authHeaders[1] != "Bearer test-github-issues-token" {
		t.Fatalf("GitHub issue fetch auth headers = %#v, want two bearer token calls", authHeaders)
	}
	if strings.Join(requestPaths, ",") != "/repos/testorg/testrepo/issues/42,/repos/testorg/testrepo/issues/42/comments" {
		t.Fatalf("GitHub issue fetch paths = %#v, want issue then comments", requestPaths)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/workspaces/engineering/workflows/github-issue/trigger", strings.NewReader(`{"inputs":{"issue_number":42}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("duplicate trigger status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var duplicateResp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &duplicateResp); err != nil {
		t.Fatalf("decode duplicate response: %v", err)
	}
	if duplicateResp["claw_id"] != resp["claw_id"] || duplicateResp["status"] != "existing" {
		t.Fatalf("duplicate response = %#v, want same claw id with existing status", duplicateResp)
	}
}
