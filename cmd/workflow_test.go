package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestExampleGitHubIssueWorkflowPublishesNestedTrigger(t *testing.T) {
	path := filepath.Join("..", "examples", "workflows", "github-issue.yaml")

	workflows, err := readWorkflowFiles([]string{path})
	if err != nil {
		t.Fatalf("read workflow files: %v", err)
	}
	if len(workflows) != 1 {
		t.Fatalf("workflow count = %d, want 1", len(workflows))
	}
	workflow := workflows[0]
	if err := workflow.Validate(); err != nil {
		t.Fatalf("validate workflow: %v", err)
	}
	if workflow.Trigger == nil || workflow.Trigger.GitHubIssues == nil {
		t.Fatalf("github_issues trigger missing: %#v", workflow.Trigger)
	}
	if workflow.Integration != "github-issues" {
		t.Fatalf("integration = %q, want github-issues", workflow.Integration)
	}
	if workflow.TriggerStatus != "open" {
		t.Fatalf("trigger status = %q, want open", workflow.TriggerStatus)
	}
	if len(workflow.Stages) != 5 {
		t.Fatalf("stage count = %d, want 5", len(workflow.Stages))
	}

	payload, err := json.Marshal(map[string]interface{}{"workflows": workflows})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	if !strings.Contains(string(payload), `"github_issues"`) {
		t.Fatalf("payload missing github_issues source: %s", payload)
	}
	if strings.Contains(string(payload), `"githubIssues"`) {
		t.Fatalf("payload used camelCase githubIssues source: %s", payload)
	}
}

func TestCronWorkflowFileValidatesForPush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dependency-update.yaml")
	data := []byte(`
schema_version: v1
name: dependency-update
trigger:
  cron:
    schedule: "0 9 * * 1"
    timezone: America/Chicago
    overlap_policy: skip
stages:
  - id: working
    entry: true
    on_enter:
      inject: start
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	workflows, err := readWorkflowFiles([]string{path})
	if err != nil {
		t.Fatalf("read workflow files: %v", err)
	}
	if len(workflows) != 1 {
		t.Fatalf("workflow count = %d, want 1", len(workflows))
	}
	workflow := workflows[0]
	if err := workflow.Validate(); err != nil {
		t.Fatalf("validate workflow: %v", err)
	}
	if workflow.Integration != "cron" {
		t.Fatalf("integration = %q, want cron", workflow.Integration)
	}
	if workflow.Trigger == nil || workflow.Trigger.Cron == nil {
		t.Fatalf("cron trigger missing: %#v", workflow.Trigger)
	}
}

func TestRunWorkflowRunsListsRunsAndShortAgentID(t *testing.T) {
	started := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	finished := started.Add(5 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedPath := "/api/workspaces/default/workflows/dependency-update/cron/runs"
		if r.URL.Path != expectedPath {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("limit") != "10" {
			http.Error(w, "unexpected limit", http.StatusBadRequest)
			return
		}
		runs := map[string]interface{}{
			"runs": []types.WorkflowRun{{
				ID:            "run-1",
				WorkflowName:  "dependency-update",
				WorkspaceName: "default",
				TriggerType:   "cron",
				Status:        "failed",
				Result:        "provisioning timed out",
				ClawID:        "claw-1234567890",
				StartedAt:     &started,
				FinishedAt:    &finished,
				CreatedAt:     started,
			}},
			"count": 1,
		}
		_ = json.NewEncoder(w).Encode(runs)
	}))
	defer server.Close()

	t.Setenv("ELASTICCLAW_HUB_URL", server.URL)
	t.Setenv("ELASTICCLAW_CLAW_TOKEN", "test-token")

	out, err := captureStdout(func() error {
		return runWorkflowRuns("default", "dependency-update", 10)
	})
	if err != nil {
		t.Fatalf("runWorkflowRuns returned error: %v", err)
	}
	for _, want := range []string{"failed", "cron", "claw-123", "provisioning timed out", "Showing 1 run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
