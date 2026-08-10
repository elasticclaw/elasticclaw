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

func TestReadWorkflowFilesPreservesV2Document(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "delivery.yaml")
	raw := `schema_version: 2
name: delivery
enabled: false
initial_state: planning
states:
  planning:
    phase: plan
  done:
    phase: done
    terminal: true
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	workflows, err := readWorkflowFiles([]string{path})
	if err != nil {
		t.Fatalf("read v2 workflow: %v", err)
	}
	if len(workflows) != 1 {
		t.Fatalf("workflow count = %d, want 1", len(workflows))
	}
	wf := workflows[0]
	if wf.SchemaVersion != "2" || wf.Name != "delivery" || wf.Enabled == nil || *wf.Enabled {
		t.Fatalf("v2 projection = %#v", wf)
	}
	if wf.RawConfig != raw {
		t.Fatalf("authored v2 YAML was not preserved:\n%s", wf.RawConfig)
	}
	if len(wf.Stages) != 0 || wf.PipelineYAML != "" {
		t.Fatalf("v2 workflow entered v1 normalization: %#v", wf)
	}
}

func TestSanitizeWorkflowResultForTable(t *testing.T) {
	tests := []struct {
		name     string
		result   string
		maxRunes int
		want     string
	}{
		{"empty", "", 80, "—"},
		{"simple", "provisioning failed", 80, "provisioning failed"},
		{"newlines", "line1\nline2\r\nline3", 80, "line1 line2 line3"},
		{"tabs", "a\tb\tc", 80, "a b c"},
		{"multispace", "a  b   c", 80, "a b c"},
		{"multibyte truncate", "🚀" + strings.Repeat("a", 80), 80, "🚀" + strings.Repeat("a", 76) + "..."},
		{"ascii truncate", strings.Repeat("a", 100), 80, strings.Repeat("a", 77) + "..."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeWorkflowResultForTable(tc.result, tc.maxRunes)
			if got != tc.want {
				t.Fatalf("sanitizeWorkflowResultForTable(%q, %d) = %q, want %q", tc.result, tc.maxRunes, got, tc.want)
			}
		})
	}
}

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

func TestRunWorkflowLogsFetchesRunAndActivityMessages(t *testing.T) {
	started := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/default/workflows/dependency-update":
			_ = json.NewEncoder(w).Encode(workflowCLIView{
				Name:          "dependency-update",
				WorkspaceName: "default",
				SchemaVersion: "v1",
			})
		case "/api/workspaces/default/workflows/dependency-update/cron/runs/run-1":
			_ = json.NewEncoder(w).Encode(types.WorkflowRun{
				ID:            "run-1",
				WorkflowName:  "dependency-update",
				WorkspaceName: "default",
				TriggerType:   "cron",
				Status:        "failed",
				Result:        "provisioning timed out",
				ClawID:        "claw-1234567890",
				StartedAt:     &started,
				CreatedAt:     started,
			})
		case "/api/messages/claw-1234567890/activity":
			_ = json.NewEncoder(w).Encode([]types.HubMessage{
				{
					ID:        "msg-1",
					ClawID:    "claw-1234567890",
					Role:      "activity",
					Format:    `activity:{"kind":"tool","tool":"bash","command":"ls -la"}`,
					CreatedAt: started,
				},
				{
					ID:        "msg-2",
					ClawID:    "claw-1234567890",
					Role:      "activity",
					Format:    `activity:{"kind":"tool","tool":"bash","error":"command failed","detail":"exit status 1"}`,
					CreatedAt: started.Add(time.Second),
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("ELASTICCLAW_HUB_URL", server.URL)
	t.Setenv("ELASTICCLAW_CLAW_TOKEN", "test-token")

	out, err := captureStdout(func() error {
		return runWorkflowLogs("default", "dependency-update", "run-1", "")
	})
	if err != nil {
		t.Fatalf("runWorkflowLogs returned error: %v", err)
	}
	for _, want := range []string{"Agent logs for run run-1", "claw-123", "failed", "[bash]", "cmd: ls -la", "error: command failed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintCollapsedActivityMessages(t *testing.T) {
	started := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	mk := func(kind, message string) types.HubMessage {
		return types.HubMessage{
			Format:    "activity:" + mustJSON(t, agentActivity{Kind: kind, Message: message}),
			CreatedAt: started,
		}
	}
	mkTool := func(phase, command string) types.HubMessage {
		return types.HubMessage{
			Format:    "activity:" + mustJSON(t, agentActivity{Kind: "tool", Tool: "bash", Phase: phase, Command: command}),
			CreatedAt: started,
		}
	}
	mkPlain := func(content string) types.HubMessage {
		return types.HubMessage{Content: content, CreatedAt: started}
	}

	messages := []types.HubMessage{
		mk("activity", "The"),
		mk("activity", "The user wants"),
		mk("activity", "The user wants to run updates"),
		mkTool("running", "git status"),
		mkTool("running", "git status"),
		mkTool("completed", "git status"),
		mkPlain("plain hub message"),
		mk("activity", "Now thinking about tests"),
		mk("activity", "Now thinking about tests and validation"),
	}

	out, err := captureStdout(func() error {
		printCollapsedActivityMessages(messages)
		return nil
	})
	if err != nil {
		t.Fatalf("captureStdout failed: %v", err)
	}

	// The three prefix-extended reasoning messages should collapse to one.
	if !strings.Contains(out, "The user wants to run updates") {
		t.Fatalf("output missing final collapsed reasoning message:\n%s", out)
	}
	if !strings.Contains(out, "(+ 2 similar)") {
		t.Fatalf("output missing collapse count for first reasoning burst:\n%s", out)
	}
	// The duplicate running tool messages should be collapsed.
	if strings.Count(out, "[bash]") != 2 {
		t.Fatalf("want 2 bash lines (running + completed), got %d:\n%s", strings.Count(out, "[bash]"), out)
	}
	if !strings.Contains(out, "(+ 1 similar)") {
		t.Fatalf("output missing collapse count for duplicate tool start:\n%s", out)
	}
	// Non-activity content should appear as-is.
	if !strings.Contains(out, "plain hub message") {
		t.Fatalf("output missing plain message:\n%s", out)
	}
	// The final reasoning burst should be present.
	if !strings.Contains(out, "Now thinking about tests and validation") {
		t.Fatalf("output missing final reasoning message:\n%s", out)
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestRunWorkflowRunsListsRunsAndShortAgentID(t *testing.T) {
	started := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	finished := started.Add(5 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedPath := "/api/workspaces/default/workflows/dependency-update/cron/runs"
		viewPath := "/api/workspaces/default/workflows/dependency-update"
		if r.URL.Path == viewPath {
			_ = json.NewEncoder(w).Encode(workflowCLIView{
				Name:          "dependency-update",
				WorkspaceName: "default",
				SchemaVersion: "v1",
			})
			return
		}
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
	for _, want := range []string{"RUN ID", "run-1", "failed", "cron", "claw-123", "provisioning timed out", "Showing 1 run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunWorkflowV2RunsListsAttempts(t *testing.T) {
	started := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	finished := started.Add(5 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/default/workflows/delivery":
			_ = json.NewEncoder(w).Encode(workflowCLIView{
				Name:          "delivery",
				WorkspaceName: "default",
				SchemaVersion: "2",
			})
		case "/api/v2/workspaces/default/workflows/delivery/runs":
			if r.URL.Query().Get("limit") != "10" {
				http.Error(w, "unexpected limit", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"runs": []workflowV2RunHistoryRow{{
					RunID:         "run-v2-1",
					AttemptNumber: 1,
					RunStatus:     "active",
					AttemptStatus: "active",
					DisplayPhase:  "plan",
					TriggerType:   "manual",
					ClawID:        "claw-1234567890",
					StartedAt:     started,
					FinishedAt:    &finished,
				}},
				"count": 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("ELASTICCLAW_HUB_URL", server.URL)
	t.Setenv("ELASTICCLAW_CLAW_TOKEN", "test-token")

	out, err := captureStdout(func() error {
		return runWorkflowRuns("default", "delivery", 10)
	})
	if err != nil {
		t.Fatalf("runWorkflowRuns returned error: %v", err)
	}
	for _, want := range []string{"RUN ID", "run-v2-1", "1", "active", "manual", "claw-123", "plan", "Showing 1 run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunWorkflowV2LogsFetchesAttemptLogs(t *testing.T) {
	started := time.Date(2026, 7, 24, 9, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/default/workflows/delivery":
			_ = json.NewEncoder(w).Encode(workflowCLIView{
				Name:          "delivery",
				WorkspaceName: "default",
				SchemaVersion: "2",
			})
		case "/api/v2/workflow-runs/run-v2-1/logs":
			_ = json.NewEncoder(w).Encode([]types.HubMessage{{
				ID:        "msg-1",
				ClawID:    "claw-1234567890",
				Role:      "activity",
				Format:    `activity:{"kind":"tool","tool":"bash","command":"ls -la"}`,
				CreatedAt: started,
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("ELASTICCLAW_HUB_URL", server.URL)
	t.Setenv("ELASTICCLAW_CLAW_TOKEN", "test-token")

	out, err := captureStdout(func() error {
		return runWorkflowLogs("default", "delivery", "run-v2-1", "")
	})
	if err != nil {
		t.Fatalf("runWorkflowLogs returned error: %v", err)
	}
	for _, want := range []string{"Agent logs for run run-v2-1", "[bash]", "cmd: ls -la"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}
