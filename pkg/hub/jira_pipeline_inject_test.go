package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestRunOnEnterInjectRendersJiraIssueDetails(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	var hitJira bool
	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/issue/KAN-2547" {
			http.NotFound(w, r)
			return
		}
		hitJira = true
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"key": "KAN-2547",
			"fields": map[string]interface{}{
				"summary":     "Break down epic",
				"description": "Plan the child issues",
				"status":      map[string]string{"name": "Ready for Agent"},
			},
		})
	}))
	defer jira.Close()

	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "engineering",
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion: "v1",
			Name:          "jira-epic-plan",
			Integration:  "jira",
			Workspace:    "default",
			Trigger: &types.WorkflowTrigger{
				Jira: &types.JiraWorkflowTrigger{
					Workspace: "default",
					Event:     "status_changed",
					Projects:  []string{"KAN"},
					States:    []string{"Ready for Agent"},
				},
			},
			Stages: []types.WorkflowStage{{
				ID:    "working",
				Label: "Working",
				Entry: true,
			}},
		}},
	)
	SaveWorkspaceIssueTrackerWithBaseForTest(t, "engineering", "jira", "default", jira.URL, "jira-user", "jira-token", "")

	const clawID = "claw-jira-inject-render"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, jira_issue_id, tags, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "KAN-2547", "base", "connected", "KAN-2547", `["workspace:engineering","workflow:jira-epic-plan"]`,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	workspace, workflow, ok := loadWorkflowPipelineContext("engineering", "jira-epic-plan")
	if !ok {
		t.Fatal("expected jira workflow context")
	}
	stage := pipeline.Stage{
		ID:    "working",
		Label: "Working",
		OnEnter: pipeline.OnEnter{
			Inject: "Work on {{.Issue.Identifier}}: {{.Issue.Title}}\n{{.Issue.URL}}\n{{.Issue.Description}}",
		},
	}
	ctx := pipelineContext{
		Workspace: workspace,
		Workflow:  workflow,
		IssueID:   "KAN-2547",
	}
	if _, err := s.runOnEnter(clawID, stage, ctx); err != nil {
		t.Fatalf("runOnEnter: %v", err)
	}
	if !hitJira {
		t.Fatal("expected Jira issue fetch for inject template rendering")
	}

	var content string
	if err := db.QueryRow(
		`SELECT COALESCE(group_concat(content, '\n'),'') FROM messages WHERE claw_id=?`,
		clawID,
	).Scan(&content); err != nil {
		t.Fatalf("load inject message: %v", err)
	}
	if !strings.Contains(content, "Work on KAN-2547: Break down epic") {
		t.Fatalf("inject missing rendered title: %q", content)
	}
	if !strings.Contains(content, jira.URL+"/browse/KAN-2547") {
		t.Fatalf("inject missing browse URL: %q", content)
	}
	if !strings.Contains(content, "Plan the child issues") {
		t.Fatalf("inject missing description: %q", content)
	}
	if strings.Contains(content, "no Linear issue tracker") {
		t.Fatalf("inject path incorrectly warned about Linear: %q", content)
	}
}

func TestRunOnEnterInjectJiraFallbackWarnsAboutJiraNotLinear(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "engineering",
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion: "v1",
			Name:          "jira-backend",
			Integration:  "jira",
			Workspace:    "default",
			Trigger: &types.WorkflowTrigger{
				Jira: &types.JiraWorkflowTrigger{
					Workspace: "default",
					Event:     "status_changed",
					Projects:  []string{"KAN"},
					States:    []string{"Ready for Agent"},
				},
			},
			Stages: []types.WorkflowStage{{
				ID:    "working",
				Label: "Working",
				Entry: true,
			}},
		}},
	)

	const clawID = "claw-jira-inject-fallback"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, jira_issue_id, tags, created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "KAN-1", "base", "connected", "KAN-1", `["workspace:engineering","workflow:jira-backend"]`,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	workspace, workflow, ok := loadWorkflowPipelineContext("engineering", "jira-backend")
	if !ok {
		t.Fatal("expected jira workflow context")
	}
	stage := pipeline.Stage{
		ID: "working",
		OnEnter: pipeline.OnEnter{
			Inject: "Issue {{.Issue.Identifier}}",
		},
	}
	if _, err := s.runOnEnter(clawID, stage, pipelineContext{
		Workspace: workspace,
		Workflow:  workflow,
		IssueID:   "KAN-1",
	}); err != nil {
		t.Fatalf("runOnEnter: %v", err)
	}

	rows, err := db.Query(`SELECT content FROM messages WHERE claw_id=? ORDER BY id`, clawID)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	var messages []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan: %v", err)
		}
		messages = append(messages, content)
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "no Jira tracker configured") {
		t.Fatalf("expected Jira tracker warning, got: %q", joined)
	}
	if strings.Contains(joined, "no Linear issue tracker") {
		t.Fatalf("should not warn about Linear for jira workflows: %q", joined)
	}
	if !strings.Contains(joined, "Issue KAN-1") {
		t.Fatalf("expected fallback inject with issue identifier: %q", joined)
	}
}

func TestLoadIssueTextForJudgeUsesJira(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")

	jira := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"key": "KAN-99",
			"fields": map[string]interface{}{
				"summary":     "Judge me",
				"description": "Acceptance criteria",
			},
		})
	}))
	defer jira.Close()

	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{Name: "engineering", Files: map[string]string{"elasticclaw-config.yaml": "schema_version: v1\nname: engineering\n"}},
		[]*types.WorkflowConfig{{
			Name:         "jira-backend",
			Integration:  "jira",
			Workspace:    "default",
			PipelineYAML: "stages:\n  - id: working\n    entry: true\n",
			Trigger:      &types.WorkflowTrigger{Jira: &types.JiraWorkflowTrigger{Workspace: "default"}},
		}},
	)
	SaveWorkspaceIssueTrackerWithBaseForTest(t, "engineering", "jira", "default", jira.URL, "user", "token", "")

	workspace, workflow, ok := loadWorkflowPipelineContext("engineering", "jira-backend")
	if !ok {
		t.Fatal("expected workflow")
	}
	got := s.loadIssueTextForJudge("unused", pipelineContext{
		Workspace: workspace,
		Workflow:  workflow,
		IssueID:   "KAN-99",
	})
	if !strings.Contains(got, "KAN-99: Judge me") || !strings.Contains(got, "Acceptance criteria") {
		t.Fatalf("judge issue text = %q", got)
	}
	if strings.HasPrefix(got, "Linear issue:") {
		t.Fatalf("expected Jira issue text, got Linear fallback: %q", got)
	}
}
