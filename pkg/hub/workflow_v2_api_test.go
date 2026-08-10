package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestWorkflowV2RunInspectionAPIIsTenantScoped(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	_, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-inspect-api",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace),
		WorkflowYAML:  []byte(workflowV2APIWorkflow),
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-inspect-api", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var inspection workflowv2.Inspection
	if err := json.NewDecoder(rr.Body).Decode(&inspection); err != nil {
		t.Fatal(err)
	}
	if inspection.Run.ID != "run-inspect-api" || len(inspection.Waiting) != 1 || inspection.Waiting[0].Kind != "effect" {
		t.Fatalf("inspection = %#v", inspection)
	}

	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,datetime('now'))`,
		"other-tenant", "other", "other-token", "other-claw-token"); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-inspect-api", nil)
	req.Header.Set("Authorization", "Bearer other-token")
	rr = httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestWorkflowV2RunInspectionAPIRejectsOtherMethods(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/api/v2/workflow-runs/run-id", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status/allow = %d/%q", rr.Code, rr.Header().Get("Allow"))
	}
}

func TestWorkflowV2RunsAPIListsAttempts(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-history-api",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace),
		WorkflowYAML:  []byte(workflowV2APIWorkflow),
		TriggerType:   "manual",
		InitialClawID: "claw-history",
	}); err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-history")
	insertTestActivityMessage(t, db, "claw-history", "planning activity")

	req := httptest.NewRequest(http.MethodGet, "/api/v2/workspaces/engineering/workflows/delivery/runs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Runs  []workflowv2.RunAttemptHistory `json:"runs"`
		Count int                            `json:"count"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || len(result.Runs) != 1 {
		t.Fatalf("count = %d, runs = %d", result.Count, len(result.Runs))
	}
	row := result.Runs[0]
	if row.RunID != "run-history-api" || row.AttemptNumber != 1 || row.ClawID != "claw-history" {
		t.Fatalf("row = %#v", row)
	}
	if row.TriggerType != "manual" {
		t.Fatalf("trigger_type = %q", row.TriggerType)
	}
}

func TestWorkflowV2RunAttemptsAPIReturnsAttempts(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-attempts-api",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace),
		WorkflowYAML:  []byte(workflowV2APIWorkflow),
		InitialClawID: "claw-attempts",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-attempts-api/attempts", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Attempts []workflowv2.Attempt `json:"attempts"`
		Count    int                  `json:"count"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || len(result.Attempts) != 1 || result.Attempts[0].Number != 1 {
		t.Fatalf("attempts = %#v", result.Attempts)
	}
}

func TestWorkflowV2RunLogsAPIReturnsActivityMessages(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-logs-api",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace),
		WorkflowYAML:  []byte(workflowV2APIWorkflow),
		InitialClawID: "claw-logs",
	}); err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-logs")
	insertTestActivityMessage(t, db, "claw-logs", "log line one")
	insertTestActivityMessage(t, db, "claw-logs", "log line two")

	req := httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-logs-api/logs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var messages []types.HubMessage
	if err := json.NewDecoder(rr.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d", len(messages))
	}
}

func TestWorkflowV2AttemptLogsAPIReturnsActivityMessages(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-attempt-logs-api",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace),
		WorkflowYAML:  []byte(workflowV2APIWorkflow),
		InitialClawID: "claw-attempt-logs",
	}); err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-attempt-logs")
	insertTestActivityMessage(t, db, "claw-attempt-logs", "attempt log")

	var attemptID string
	if err := db.QueryRow(`SELECT id FROM workflow_v2_attempts WHERE run_id=?`, "run-attempt-logs-api").Scan(&attemptID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-attempt-logs-api/attempts/"+attemptID+"/logs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var messages []types.HubMessage
	if err := json.NewDecoder(rr.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d", len(messages))
	}
}

func TestWorkflowV2RunsAPIIsTenantScoped(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-tenant-scope",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace),
		WorkflowYAML:  []byte(workflowV2APIWorkflow),
		InitialClawID: "claw-tenant-scope",
	}); err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-tenant-scope")
	insertTestActivityMessage(t, db, "claw-tenant-scope", "private")

	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,datetime('now'))`,
		"other-tenant", "other", "other-token", "other-claw-token"); err != nil {
		t.Fatal(err)
	}

	// List endpoint returns an empty list for the other tenant.
	req := httptest.NewRequest(http.MethodGet, "/api/v2/workspaces/engineering/workflows/delivery/runs", nil)
	req.Header.Set("Authorization", "Bearer other-token")
	rr := httptest.NewRecorder()
	s.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list cross-tenant status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var listResult struct {
		Runs  []workflowv2.RunAttemptHistory `json:"runs"`
		Count int                            `json:"count"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&listResult); err != nil {
		t.Fatal(err)
	}
	if listResult.Count != 0 || len(listResult.Runs) != 0 {
		t.Fatalf("list cross-tenant count = %d, runs = %d", listResult.Count, len(listResult.Runs))
	}

	// Single-resource endpoints must return 404 for the other tenant.
	paths := []string{
		"/api/v2/workflow-runs/run-tenant-scope/logs",
		"/api/v2/workflow-runs/run-tenant-scope/attempts",
	}
	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer other-token")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("path %s cross-tenant status = %d, body = %s", path, rr.Code, rr.Body.String())
		}
	}
}

func insertTestClaw(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,created_at) VALUES(?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", clawID); err != nil {
		t.Fatal(err)
	}
}

func insertTestActivityMessage(t *testing.T, db *sql.DB, clawID, content string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		"msg-"+clawID+"-"+content, clawID, "test-tenant-id", "activity", content); err != nil {
		t.Fatal(err)
	}
}

const workflowV2APIWorkspace = `
schema_version: 2
name: engineering
repositories:
  primary:
    provider: github
    repository: org/repo
`

const workflowV2APIWorkflow = `
schema_version: 2
name: delivery
enabled: true
initial_state: planning
states:
  planning:
    phase: plan
    on_enter:
      effects:
        - agent.task:
            prompt: Make a plan.
  done:
    phase: done
    terminal: true
transitions:
  planned:
    from: planning
    on: agent.task.completed
    to: done
`
