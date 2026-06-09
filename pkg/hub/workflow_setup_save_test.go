package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/elasticclaw/elasticclaw/pkg/workflowsetup"
)

func TestWorkflowSetupSaveRequiresPostAndAuth(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	unauth := workflowSetupAPIRequest(s, httpMethodPost, "/api/workflow-setup/save", workflowSetupSaveRequest("engineering", workflowSetupManualWorkflowYAML("manual-run"), workflowsetup.SaveModeCreate, false), false)
	if unauth.Code != 401 {
		t.Fatalf("unauthorized status = %d, want 401", unauth.Code)
	}

	wrongMethod := workflowSetupAPIRequest(s, "GET", "/api/workflow-setup/save", nil, true)
	if wrongMethod.Code != 405 {
		t.Fatalf("GET status = %d, want 405", wrongMethod.Code)
	}
}

func TestWorkflowSetupSaveBlocksCritical(t *testing.T) {
	workflowSetupSaveTestWorkspace(t, nil)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	req := workflowSetupSaveRequest("engineering", workflowSetupManualWorkflowYAML("manual-run"), workflowsetup.SaveModeCreate, true)
	rr := workflowSetupAPIRequest(s, httpMethodPost, "/api/workflow-setup/save", req, true)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(workflowSetupSavedWorkflowPath("engineering", "manual-run")); !os.IsNotExist(err) {
		t.Fatalf("workflow file err = %v, want not exist", err)
	}
}

func TestWorkflowSetupSaveRequiresAllowWarningsForWarning(t *testing.T) {
	existingRaw := workflowSetupGitHubIssueWorkflowYAML("existing-triage")
	workflowSetupSaveTestWorkspace(t, []*types.WorkflowConfig{{
		Name:      "existing-triage",
		RawConfig: existingRaw,
	}})
	SaveWorkspaceIssueTrackerForTest(t, "engineering", "github-issues", "default", "token", "webhook-secret")
	s, _ := NewTestServerWithConfig(t, workflowSetupSaveHubConfig(), "", "", "")

	overlappingRaw := workflowSetupGitHubIssueWorkflowYAML("new-triage")
	req := workflowSetupSaveRequest("engineering", overlappingRaw, workflowsetup.SaveModeCreate, false)
	rr := workflowSetupAPIRequest(s, httpMethodPost, "/api/workflow-setup/save", req, true)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(workflowSetupSavedWorkflowPath("engineering", "new-triage")); !os.IsNotExist(err) {
		t.Fatalf("workflow file err = %v, want not exist", err)
	}

	req.AllowWarnings = true
	rr = workflowSetupAPIRequest(s, httpMethodPost, "/api/workflow-setup/save", req, true)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(workflowSetupSavedWorkflowPath("engineering", "new-triage")); err != nil {
		t.Fatalf("workflow file after allowWarnings: %v", err)
	}
}

func TestWorkflowSetupSaveRejectsStaleHash(t *testing.T) {
	workflowSetupSaveTestWorkspace(t, nil)
	s, _ := NewTestServerWithConfig(t, workflowSetupSaveHubConfig(), "", "", "")

	req := workflowSetupSaveRequest("engineering", workflowSetupManualWorkflowYAML("manual-run"), workflowsetup.SaveModeCreate, false)
	req.ValidatedConfigHash = workflowsetup.ConfigHash(req.Workflow.Config + "\nchanged")
	rr := workflowSetupAPIRequest(s, httpMethodPost, "/api/workflow-setup/save", req, true)
	if rr.Code != 409 {
		t.Fatalf("status = %d, want 409, body = %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(workflowSetupSavedWorkflowPath("engineering", "manual-run")); !os.IsNotExist(err) {
		t.Fatalf("workflow file err = %v, want not exist", err)
	}
}

func TestWorkflowSetupSavePersistsAuthoredYAMLWithoutDerivedRuntimeFieldsAndPreservesManagedFiles(t *testing.T) {
	workflowSetupSaveTestWorkspace(t, nil)
	SaveWorkspaceIssueTrackerForTest(t, "engineering", "github-issues", "default", "token", "webhook-secret")
	s, _ := NewTestServerWithConfig(t, workflowSetupSaveHubConfig(), "", "", "")

	authoredRaw := strings.Join([]string{
		"schema_version: v1",
		"name: manual-run",
		"provider: daytona",
		"enable_manual_trigger: true",
		"stages:",
		"  - id: done",
		"    entry: true",
		"    terminal: true",
		"",
	}, "\n")
	rr := workflowSetupAPIRequest(s, httpMethodPost, "/api/workflow-setup/save", workflowSetupSaveRequest("engineering", authoredRaw, workflowsetup.SaveModeCreate, false), true)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	data, err := os.ReadFile(workflowSetupSavedWorkflowPath("engineering", "manual-run"))
	if err != nil {
		t.Fatalf("read saved workflow: %v", err)
	}
	if got := string(data); got != authoredRaw {
		t.Fatalf("saved workflow YAML mismatch\n got:\n%s\nwant:\n%s", got, authoredRaw)
	}
	if strings.Contains(string(data), "pipeline_yaml:") || strings.Contains(string(data), "integration:") {
		t.Fatalf("saved workflow contains derived runtime fields:\n%s", string(data))
	}

	managedData, err := os.ReadFile(workspaceIssueTrackersPath("engineering"))
	if err != nil {
		t.Fatalf("read managed issue trackers: %v", err)
	}
	if !strings.Contains(string(managedData), "webhook-secret") {
		t.Fatalf("managed issue tracker file was not preserved:\n%s", string(managedData))
	}
}

func TestWorkflowSetupSaveCreateBlocksCollisionAndUpsertReplaces(t *testing.T) {
	oldRaw := workflowSetupManualWorkflowYAML("manual-run")
	workflowSetupSaveTestWorkspace(t, []*types.WorkflowConfig{{
		Name:      "manual-run",
		RawConfig: oldRaw,
	}})
	s, _ := NewTestServerWithConfig(t, workflowSetupSaveHubConfig(), "", "", "")

	newRaw := strings.Replace(oldRaw, "terminal: true", "label: Updated\n    terminal: true", 1)
	createReq := workflowSetupSaveRequest("engineering", newRaw, workflowsetup.SaveModeCreate, false)
	rr := workflowSetupAPIRequest(s, httpMethodPost, "/api/workflow-setup/save", createReq, true)
	if rr.Code != 409 {
		t.Fatalf("create collision status = %d, want 409, body = %s", rr.Code, rr.Body.String())
	}
	data, err := os.ReadFile(workflowSetupSavedWorkflowPath("engineering", "manual-run"))
	if err != nil {
		t.Fatalf("read workflow after create collision: %v", err)
	}
	if string(data) != oldRaw {
		t.Fatalf("create collision changed workflow\n got:\n%s\nwant:\n%s", string(data), oldRaw)
	}

	upsertReq := workflowSetupSaveRequest("engineering", newRaw, workflowsetup.SaveModeUpsert, false)
	rr = workflowSetupAPIRequest(s, httpMethodPost, "/api/workflow-setup/save", upsertReq, true)
	if rr.Code != 200 {
		t.Fatalf("upsert status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	data, err = os.ReadFile(workflowSetupSavedWorkflowPath("engineering", "manual-run"))
	if err != nil {
		t.Fatalf("read workflow after upsert: %v", err)
	}
	if string(data) != newRaw {
		t.Fatalf("upsert did not replace workflow\n got:\n%s\nwant:\n%s", string(data), newRaw)
	}
}

const httpMethodPost = "POST"

func workflowSetupSaveTestWorkspace(t *testing.T, workflows []*types.WorkflowConfig) {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	raw := strings.Join([]string{
		"schema_version: v1",
		"name: engineering",
		"repositories:",
		"  - repo: elasticclaw/elasticclaw",
		"    permissions: write",
		"",
	}, "\n")
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{
		SchemaVersion: "v1",
		Name:          "engineering",
		Repositories: []types.GitHubRepoAccess{{
			Repo:        "elasticclaw/elasticclaw",
			Permissions: "write",
		}},
		Files: map[string]string{"elasticclaw-config.yaml": raw},
	}, workflows)
}

func workflowSetupSaveHubConfig() *types.HubConfig {
	return &types.HubConfig{
		Token:        "test-token",
		ClawToken:    "claw-token-secret",
		DefaultModel: "anthropic/claude-sonnet-4-6",
		Providers: map[string]types.ProviderConfig{
			"daytona": {APIKey: "daytona-api-key-secret"},
		},
		LLMKeys: types.LLMKeysList{
			{Name: "anthropic-main", Provider: "anthropic", APIKey: "llm-api-key-secret", Default: true, DefaultModel: "claude-sonnet-4-6"},
		},
	}
}

func workflowSetupSaveRequest(workspace, config string, mode workflowsetup.SaveMode, allowWarnings bool) workflowsetup.SaveRequest {
	return workflowsetup.SaveRequest{
		Workspace: strings.TrimSpace(workspace),
		Workflow: workflowsetup.SaveWorkflow{
			Name:   workflowSetupWorkflowNameFromRaw(config),
			Config: config,
		},
		Mode:                mode,
		ValidatedConfigHash: workflowsetup.ConfigHash(config),
		AllowWarnings:       allowWarnings,
	}
}

func workflowSetupWorkflowNameFromRaw(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		}
	}
	return ""
}

func workflowSetupManualWorkflowYAML(name string) string {
	return strings.Join([]string{
		"schema_version: v1",
		"name: " + name,
		"provider: daytona",
		"enable_manual_trigger: true",
		"stages:",
		"  - id: done",
		"    entry: true",
		"    terminal: true",
		"",
	}, "\n")
}

func workflowSetupGitHubIssueWorkflowYAML(name string) string {
	return strings.Join([]string{
		"schema_version: v1",
		"name: " + name,
		"provider: daytona",
		"trigger:",
		"  github_issues:",
		"    event: issue_labeled",
		"    repositories:",
		"      - elasticclaw/elasticclaw",
		"    states:",
		"      - open",
		"    labels:",
		"      - agent-ready",
		"    labelers:",
		"      - \"*\"",
		"stages:",
		"  - id: working",
		"    entry: true",
		"    terminal: true",
		"",
	}, "\n")
}

func workflowSetupSavedWorkflowPath(workspace, workflow string) string {
	return filepath.Join(workspacesDir(), workspace, "workflows", strings.ToLower(workflow)+".yaml")
}
