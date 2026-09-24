package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
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
	activityCount := 0
	stateCount := 0
	for _, m := range messages {
		switch m.Role {
		case "activity":
			activityCount++
		case "state":
			stateCount++
		}
	}
	if activityCount != 2 {
		t.Fatalf("activity messages = %d, want 2", activityCount)
	}
	if stateCount == 0 {
		t.Fatalf("expected state transition messages, got none")
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
	activityCount := 0
	stateCount := 0
	for _, m := range messages {
		switch m.Role {
		case "activity":
			activityCount++
		case "state":
			stateCount++
		}
	}
	if activityCount != 1 {
		t.Fatalf("activity messages = %d, want 1", activityCount)
	}
	if stateCount == 0 {
		t.Fatalf("expected state transition messages, got none")
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
	// Bind a time.Time like production message writes do. datetime('now')
	// would truncate to whole seconds, and second-resolution timestamps sort
	// before a sub-second attempt started_at bound under the window predicates.
	if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		"msg-"+clawID+"-"+content, clawID, "test-tenant-id", "activity", content, time.Now().UTC()); err != nil {
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

const workflowV2ExecAPIWorkspace = `
schema_version: 2
name: engineering
repositories:
  primary:
    provider: github
    repository: org/repo
execution:
  provider: daytona
`

const workflowV2ExecAPIWorkflow = `
schema_version: 2
name: delivery
enabled: true
initial_state: detect
states:
  detect:
    phase: setup
    on_enter:
      effects:
        - exec.run:
            command: make test
            timeout: 1m
  done:
    phase: done
    terminal: true
transitions:
  detected:
    from: detect
    on: exec.run.completed
    to: done
`

// seedWorkflowV2ExecRun creates a run whose exec.run effect went through the
// production lifecycle: claimed, materialized as a command task, and completed
// with a failed receipt carrying stdout/stderr. It also records a finished
// agent task so inspection covers every enriched section.
func seedWorkflowV2ExecRun(t *testing.T, s *Server, db *sql.DB, runID, clawID string) workflowv2.Run {
	t.Helper()
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            runID,
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2ExecAPIWorkspace),
		WorkflowYAML:  []byte(workflowV2ExecAPIWorkflow),
		InitialClawID: clawID,
	})
	if err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, clawID)

	claim, err := store.ClaimEffect(context.Background(), "log-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	assign, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "log-worker")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	receipt, err := json.Marshal(map[string]interface{}{
		"exit_code": 2,
		"succeeded": false,
		"stdout":    "compiling main.go\nbuild failed",
		"stderr":    "make: *** No rule to make target 'test'",
		"error":     "exit code 2",
	})
	if err != nil {
		t.Fatal(err)
	}
	version := run.StateVersion
	if _, err := store.ApplyCommandReceipt(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "receipt-" + runID,
		Kind:                 typesv2.MessageExecRunFailed,
		RunID:                run.ID,
		AttemptID:            run.CurrentAttemptID,
		TaskID:               assign.TaskID,
		ExpectedStateVersion: &version,
		Payload:              receipt,
	}); err != nil {
		t.Fatalf("apply receipt: %v", err)
	}

	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,allowed_actions,required_artifacts,
		heartbeat_deadline,deadline,terminal_reason,created_at,updated_at,finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"task-"+runID, runID, "", "", run.State, run.StateVersion, "failed", "Fix the build", "[]", "[]",
		now, now, "gateway is not ready", now, now, now); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestWorkflowV2AttemptLogsAreScopedToTheAttemptSession(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	base := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	current := base
	store.SetClock(func() time.Time { return current })

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-attempt-scope",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2ExecAPIWorkspace),
		WorkflowYAML:  []byte(workflowV2ExecAPIWorkflow),
		InitialClawID: "claw-attempt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-attempt-1")
	insertTestClaw(t, db, "claw-attempt-2")

	// Attempt 1's session: the exec effect is claimed, materialized, and
	// completes successfully at T1, which also fires the detected transition.
	current = base.Add(time.Minute)
	claim, err := store.ClaimEffect(context.Background(), "scope-worker", 5*time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	assign, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "scope-worker")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	receipt, err := json.Marshal(map[string]interface{}{
		"exit_code": 0, "succeeded": true, "stdout": "tests passed", "stderr": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	version := run.StateVersion
	if _, err := store.ApplyCommandReceipt(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "receipt-attempt-scope",
		Kind:                 typesv2.MessageExecRunCompleted,
		RunID:                run.ID,
		AttemptID:            run.CurrentAttemptID,
		TaskID:               assign.TaskID,
		ExpectedStateVersion: &version,
		Payload:              receipt,
	}); err != nil {
		t.Fatalf("apply receipt: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,allowed_actions,required_artifacts,
		heartbeat_deadline,deadline,terminal_reason,created_at,updated_at,finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"task-attempt-1", run.ID, "", run.CurrentAttemptID, run.State, run.StateVersion, "completed",
		"Fix the build", "[]", "[]", current.UnixMilli(), current.UnixMilli(), "", current.UnixMilli(), current.UnixMilli(), current.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Attempt 1 ends and attempt 2 takes over at T2.
	current = base.Add(2 * time.Minute)
	if _, err := db.Exec(`UPDATE workflow_v2_attempts SET status='failed', finished_at=? WHERE id=?`,
		current.UnixMilli(), run.CurrentAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_attempts(
		id,run_id,claw_id,number,status,started_at,heartbeat_at,finished_at,reason)
		VALUES(?,?,?,2,'active',?,?,0,'')`,
		"attempt-2-scope", run.ID, "claw-attempt-2", current.UnixMilli(), current.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	// Attempt 2's session gets its own agent task at T3.
	current = base.Add(3 * time.Minute)
	if _, err := db.Exec(`INSERT INTO workflow_v2_agent_tasks(
		id,run_id,effect_id,attempt_id,state,state_version,status,instructions,allowed_actions,required_artifacts,
		heartbeat_deadline,deadline,terminal_reason,created_at,updated_at,finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,0)`,
		"task-attempt-2", run.ID, "", "attempt-2-scope", run.State, run.StateVersion, "assigned",
		"Retry the build", "[]", "[]", current.UnixMilli(), current.UnixMilli(), current.UnixMilli(), current.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	fetchLogs := func(path string) []types.HubMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, rr.Code, rr.Body.String())
		}
		var messages []types.HubMessage
		if err := json.NewDecoder(rr.Body).Decode(&messages); err != nil {
			t.Fatal(err)
		}
		return messages
	}
	effectLines := func(messages []types.HubMessage, kind, phase string) int {
		count := 0
		for _, m := range messages {
			if m.Role != "effect" {
				continue
			}
			if event, ok := types.ParseWorkflowEffectFormat(m.Format); ok && event.Kind == kind && event.Phase == phase {
				count++
			}
		}
		return count
	}
	roleCount := func(messages []types.HubMessage, role string) int {
		count := 0
		for _, m := range messages {
			if m.Role == role {
				count++
			}
		}
		return count
	}
	containsInstructions := func(messages []types.HubMessage, want string) bool {
		for _, m := range messages {
			if m.Role != "effect" {
				continue
			}
			if event, ok := types.ParseWorkflowEffectFormat(m.Format); ok && strings.Contains(event.Instructions, want) {
				return true
			}
		}
		return false
	}

	attempt1 := fetchLogs("/api/v2/workflow-runs/run-attempt-scope/attempts/" + run.CurrentAttemptID + "/logs")
	if got := effectLines(attempt1, "exec.run", "started"); got != 1 {
		t.Fatalf("attempt 1 exec.run starts = %d, want 1", got)
	}
	if got := effectLines(attempt1, "exec.run", "dispatched"); got != 1 {
		t.Fatalf("attempt 1 exec.run dispatched = %d, want 1 (assignment)", got)
	}
	if got := effectLines(attempt1, "exec.run", "finished"); got != 1 {
		t.Fatalf("attempt 1 exec.run finished = %d, want 1 (outcome only — the assignment is a separate dispatched line)", got)
	}
	if got := effectLines(attempt1, "agent.task", "assigned"); got != 1 {
		t.Fatalf("attempt 1 agent.task assigned = %d, want 1", got)
	}
	if got := effectLines(attempt1, "agent.task", "finished"); got != 1 {
		t.Fatalf("attempt 1 agent.task finished = %d, want 1", got)
	}
	if containsInstructions(attempt1, "Retry the build") {
		t.Fatal("attempt 1 logs include attempt 2's agent task instructions")
	}
	if got := roleCount(attempt1, "state"); got != 2 {
		t.Fatalf("attempt 1 state transitions = %d, want 2 (initial + detected)", got)
	}

	attempt2 := fetchLogs("/api/v2/workflow-runs/run-attempt-scope/attempts/attempt-2-scope/logs")
	if got := effectLines(attempt2, "agent.task", "assigned"); got != 1 {
		t.Fatalf("attempt 2 agent.task assigned = %d, want 1", got)
	}
	if !containsInstructions(attempt2, "Retry the build") {
		t.Fatal("attempt 2 logs missing its own agent task instructions")
	}
	if got := effectLines(attempt2, "exec.run", "started"); got != 0 {
		t.Fatalf("attempt 2 exec.run starts = %d, want 0 (attempt 1 session)", got)
	}
	if got := effectLines(attempt2, "exec.run", "dispatched"); got != 0 {
		t.Fatalf("attempt 2 exec.run dispatched = %d, want 0 (attempt 1 session)", got)
	}
	if got := effectLines(attempt2, "exec.run", "finished"); got != 0 {
		t.Fatalf("attempt 2 exec.run finished = %d, want 0 (attempt 1 session)", got)
	}
	if got := effectLines(attempt2, "agent.task", "finished"); got != 0 {
		t.Fatalf("attempt 2 agent.task finished = %d, want 0 (attempt 1 session)", got)
	}
	if got := roleCount(attempt2, "state"); got != 0 {
		t.Fatalf("attempt 2 state transitions = %d, want 0 (both predate attempt 2)", got)
	}

	runLogs := fetchLogs("/api/v2/workflow-runs/run-attempt-scope/logs")
	if got := effectLines(runLogs, "agent.task", "assigned"); got != 2 {
		t.Fatalf("run-level agent.task assigned = %d, want 2 (complete record)", got)
	}
	if got := effectLines(runLogs, "exec.run", "dispatched"); got != 1 {
		t.Fatalf("run-level exec.run dispatched = %d, want 1 (complete record)", got)
	}
	if got := effectLines(runLogs, "exec.run", "finished"); got != 1 {
		t.Fatalf("run-level exec.run finished = %d, want 1 (outcome only, complete record)", got)
	}
	if got := roleCount(runLogs, "state"); got != 2 {
		t.Fatalf("run-level state transitions = %d, want 2 (complete record)", got)
	}
}

func TestWorkflowV2AttemptLogsScopeActivityForReusedClaw(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	current := base
	store.SetClock(func() time.Time { return current })

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-reused-claw",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2ExecAPIWorkspace),
		WorkflowYAML:  []byte(workflowV2ExecAPIWorkflow),
		InitialClawID: "claw-reused",
	})
	if err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-reused")
	insertActivityAt := func(id, content string, at time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
			id, "claw-reused", "test-tenant-id", "activity", content, at); err != nil {
			t.Fatal(err)
		}
	}

	// Attempt 1 activity lands inside its session.
	insertActivityAt("msg-reuse-1", "first-era activity", base.Add(time.Minute))

	// Attempt 1 fails and a retry reuses the same claw.
	current = base.Add(2 * time.Minute)
	if _, err := db.Exec(`UPDATE workflow_v2_attempts SET status='failed', finished_at=? WHERE id=?`,
		current.UnixMilli(), run.CurrentAttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_v2_attempts(
		id,run_id,claw_id,number,status,started_at,heartbeat_at,finished_at,reason)
		VALUES(?,?,?,2,'active',?,?,0,'retry')`,
		"attempt-2-reuse", run.ID, "claw-reused", current.UnixMilli(), current.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	insertActivityAt("msg-reuse-2", "retry-era activity", base.Add(3*time.Minute))

	fetchLogs := func(path string) []types.HubMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, rr.Code, rr.Body.String())
		}
		var messages []types.HubMessage
		if err := json.NewDecoder(rr.Body).Decode(&messages); err != nil {
			t.Fatal(err)
		}
		return messages
	}
	activityContents := func(messages []types.HubMessage) []string {
		var contents []string
		for _, m := range messages {
			if m.Role == "activity" {
				contents = append(contents, m.Content)
			}
		}
		return contents
	}

	attempt2 := fetchLogs("/api/v2/workflow-runs/run-reused-claw/attempts/attempt-2-reuse/logs")
	if contents := activityContents(attempt2); len(contents) != 1 || contents[0] != "retry-era activity" {
		t.Fatalf("attempt 2 activity = %v, want only retry-era activity", contents)
	}
	attempt1 := fetchLogs("/api/v2/workflow-runs/run-reused-claw/attempts/" + run.CurrentAttemptID + "/logs")
	if contents := activityContents(attempt1); len(contents) != 1 || contents[0] != "first-era activity" {
		t.Fatalf("attempt 1 activity = %v, want only first-era activity", contents)
	}
}

func TestWorkflowV2RunLogsPaginateLifecycleLinesWithCursor(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	base := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	current := base
	store.SetClock(func() time.Time { return current })

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-lifecycle-cursor",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2ExecAPIWorkspace),
		WorkflowYAML:  []byte(workflowV2ExecAPIWorkflow),
		InitialClawID: "claw-lifecycle-cursor",
	})
	if err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-lifecycle-cursor")

	// Lifecycle records (initial transition at T0, effect + failed exec
	// outcome at T1 — a failed receipt fires no transition).
	current = base.Add(time.Minute)
	claim, err := store.ClaimEffect(context.Background(), "cursor-worker", 5*time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	assign, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "cursor-worker")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	receipt, err := json.Marshal(map[string]interface{}{
		"exit_code": 1, "succeeded": false, "stdout": "boom", "stderr": "make: *** error",
	})
	if err != nil {
		t.Fatal(err)
	}
	version := run.StateVersion
	if _, err := store.ApplyCommandReceipt(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "receipt-lifecycle-cursor",
		Kind:                 typesv2.MessageExecRunFailed,
		RunID:                run.ID,
		AttemptID:            run.CurrentAttemptID,
		TaskID:               assign.TaskID,
		ExpectedStateVersion: &version,
		Payload:              receipt,
	}); err != nil {
		t.Fatalf("apply receipt: %v", err)
	}

	// Activity rows newer than every lifecycle record.
	for i := 2; i <= 5; i++ {
		if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
			fmt.Sprintf("msg-cursor-%d", i), "claw-lifecycle-cursor", "test-tenant-id", "activity",
			fmt.Sprintf("activity %d", i), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	fetchLogs := func(path string) []types.HubMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, rr.Code, rr.Body.String())
		}
		var messages []types.HubMessage
		if err := json.NewDecoder(rr.Body).Decode(&messages); err != nil {
			t.Fatal(err)
		}
		return messages
	}
	countRoles := func(messages []types.HubMessage) (activity, effect, state int) {
		for _, m := range messages {
			switch m.Role {
			case "activity":
				activity++
			case "effect":
				effect++
			case "state":
				state++
			}
		}
		return
	}

	// Page 1 (newest first): a full activity page must not crowd out or repeat
	// lifecycle records — they are simply older, so they belong to page 2.
	page1 := fetchLogs("/api/v2/workflow-runs/run-lifecycle-cursor/logs?limit=3&order=desc")
	if activity, effect, state := countRoles(page1); len(page1) != 3 || activity != 3 || effect != 0 || state != 0 {
		t.Fatalf("page 1 = %d rows (activity %d, effect %d, state %d), want 3 activity only", len(page1), activity, effect, state)
	}

	// Page 2 continues from the oldest row of page 1 and must include the
	// lifecycle records exactly once, on the page that covers their timestamps.
	before := base.Add(3 * time.Minute).UTC().Format(time.RFC3339Nano)
	page2 := fetchLogs("/api/v2/workflow-runs/run-lifecycle-cursor/logs?limit=3&order=desc&before=" + before)
	if activity, effect, state := countRoles(page2); len(page2) != 3 || activity != 1 || effect != 2 || state != 0 {
		t.Fatalf("page 2 = %d rows (activity %d, effect %d, state %d), want activity 1 + effect 2", len(page2), activity, effect, state)
	}
	for _, m := range page2 {
		if m.Role == "activity" && m.Content != "activity 2" {
			t.Fatalf("page 2 activity = %q, want only the row older than the cursor", m.Content)
		}
	}
}

func TestWorkflowV2RunLogsCompoundCursorSurvivesTieGroups(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	base := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	current := base
	store.SetClock(func() time.Time { return current })

	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-tie-groups",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2ExecAPIWorkspace),
		WorkflowYAML:  []byte(workflowV2ExecAPIWorkflow),
		InitialClawID: "claw-tie-groups",
	})
	if err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-tie-groups")

	// Three lifecycle records share the T1 millisecond: effect start, effect
	// finish, and the failed exec outcome (which fires no transition).
	current = base.Add(time.Minute)
	claim, err := store.ClaimEffect(context.Background(), "tie-worker", 5*time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	assign, err := store.MaterializeCommandTask(context.Background(), claim.Effect.ID, claim.AttemptID, "tie-worker")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	receipt, err := json.Marshal(map[string]interface{}{
		"exit_code": 1, "succeeded": false, "stdout": "boom", "stderr": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	version := run.StateVersion
	if _, err := store.ApplyCommandReceipt(context.Background(), typesv2.ControlEnvelope{
		ProtocolVersion:      typesv2.ControlProtocolVersion,
		MessageID:            "receipt-tie-groups",
		Kind:                 typesv2.MessageExecRunFailed,
		RunID:                run.ID,
		AttemptID:            run.CurrentAttemptID,
		TaskID:               assign.TaskID,
		ExpectedStateVersion: &version,
		Payload:              receipt,
	}); err != nil {
		t.Fatalf("apply receipt: %v", err)
	}

	// Four activity rows newer than every lifecycle record, plus the initial
	// transition at T0: eight rows total, with a three-row tie group at T1.
	for i := 2; i <= 5; i++ {
		if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
			fmt.Sprintf("msg-tie-%d", i), "claw-tie-groups", "test-tenant-id", "activity",
			fmt.Sprintf("activity %d", i), base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	fetchPage := func(before, beforeID string) []types.HubMessage {
		t.Helper()
		path := "/api/v2/workflow-runs/run-tie-groups/logs?limit=3&order=desc"
		if before != "" {
			path += "&before=" + before + "&before_id=" + beforeID
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rr := httptest.NewRecorder()
		s.mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body = %s", path, rr.Code, rr.Body.String())
		}
		var messages []types.HubMessage
		if err := json.NewDecoder(rr.Body).Decode(&messages); err != nil {
			t.Fatal(err)
		}
		return messages
	}

	// Walk pages exactly like the client does: the compound cursor is the
	// oldest row (created_at, id) of the previous page.
	var seen []types.HubMessage
	before, beforeID := "", ""
	for page := 1; page <= 4; page++ {
		messages := fetchPage(before, beforeID)
		seen = append(seen, messages...)
		if len(messages) < 3 {
			break
		}
		oldest := messages[len(messages)-1]
		before = oldest.CreatedAt.Format(time.RFC3339Nano)
		beforeID = oldest.ID
	}

	ids := map[string]bool{}
	activity, effect, state := 0, 0, 0
	for _, m := range seen {
		if ids[m.ID] {
			t.Fatalf("row %s returned on multiple pages", m.ID)
		}
		ids[m.ID] = true
		switch m.Role {
		case "activity":
			activity++
		case "effect":
			effect++
		case "state":
			state++
		}
	}
	if len(seen) != 8 || activity != 4 || effect != 3 || state != 1 {
		t.Fatalf("pages returned %d rows (activity %d, effect %d, state %d), want 8 (4/3/1) — tie-group rows were skipped", len(seen), activity, effect, state)
	}
}

func TestWorkflowV2RunLogsIncludeEffectTaskAndExecOutcomeLines(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	seedWorkflowV2ExecRun(t, s, db, "run-logs-effects", "claw-logs-effects")

	req := httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-logs-effects/logs", nil)
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

	var effectStarts, effectDispatched, execOutcomes, taskAssigned, taskFinished int
	var outcome, dispatched types.WorkflowEffectEvent
	var assigned, finishedTask types.WorkflowEffectEvent
	for _, m := range messages {
		if m.Role != "effect" {
			continue
		}
		event, ok := types.ParseWorkflowEffectFormat(m.Format)
		if !ok {
			t.Fatalf("unparseable effect format %q", m.Format)
		}
		switch {
		case event.Kind == "exec.run" && event.Phase == "started":
			effectStarts++
			if event.Command != "make test" {
				t.Fatalf("start command = %q", event.Command)
			}
		case event.Kind == "exec.run" && event.Phase == "dispatched":
			effectDispatched++
			dispatched = event
		case event.Kind == "exec.run" && event.Phase == "finished":
			execOutcomes++
			outcome = event
		case event.Kind == "agent.task" && event.Phase == "assigned":
			taskAssigned++
			assigned = event
		case event.Kind == "agent.task" && event.Phase == "finished":
			taskFinished++
			finishedTask = event
		}
	}
	if effectStarts != 1 || effectDispatched != 1 {
		t.Fatalf("effect starts = %d, dispatched = %d (want 1 each)", effectStarts, effectDispatched)
	}
	var assignmentTaskID string
	if err := db.QueryRow(`SELECT json_extract(receipt_json, '$.task_id') FROM workflow_v2_effect_attempts`).Scan(&assignmentTaskID); err != nil {
		t.Fatal(err)
	}
	if dispatched.TaskID != assignmentTaskID {
		t.Fatalf("dispatched task_id = %q, want assignment task %q", dispatched.TaskID, assignmentTaskID)
	}
	if execOutcomes != 1 {
		t.Fatalf("exec outcome lines = %d, want 1", execOutcomes)
	}
	if outcome.Stdout != "compiling main.go\nbuild failed" || outcome.Stderr != "make: *** No rule to make target 'test'" {
		t.Fatalf("outcome stdout/stderr = %q/%q", outcome.Stdout, outcome.Stderr)
	}
	if outcome.ExitCode == nil || *outcome.ExitCode != 2 || outcome.Succeeded == nil || *outcome.Succeeded {
		t.Fatalf("outcome exit/succeeded = %v/%v", outcome.ExitCode, outcome.Succeeded)
	}
	if taskAssigned != 1 || assigned.Instructions != "Fix the build" {
		t.Fatalf("task assigned = %d, instructions = %q", taskAssigned, assigned.Instructions)
	}
	if taskFinished != 1 || finishedTask.TerminalReason != "gateway is not ready" {
		t.Fatalf("task finished = %d, reason = %q", taskFinished, finishedTask.TerminalReason)
	}
}

func TestWorkflowV2RunInspectionIncludesPayloadsReceiptsAndInstructions(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	seedWorkflowV2ExecRun(t, s, db, "run-inspect-effects", "claw-inspect-effects")

	req := httptest.NewRequest(http.MethodGet, "/api/v2/workflow-runs/run-inspect-effects", nil)
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

	if len(inspection.Effects) == 0 {
		t.Fatal("expected effects in inspection")
	}
	effect := inspection.Effects[0]
	if command, _ := effect.Payload["command"].(string); command != "make test" {
		t.Fatalf("effect payload command = %q", command)
	}
	if taskID, _ := effect.Receipt["task_id"].(string); taskID == "" {
		t.Fatalf("effect receipt task_id missing: %#v", effect.Receipt)
	}

	if len(inspection.AgentTasks) == 0 || inspection.AgentTasks[0].Instructions != "Fix the build" {
		t.Fatalf("agent task instructions missing: %#v", inspection.AgentTasks)
	}

	if len(inspection.RecentEvents) == 0 {
		t.Fatal("expected recent events in inspection")
	}
	var receiptEvent *workflowv2.EventRecord
	for i := range inspection.RecentEvents {
		if inspection.RecentEvents[i].Kind == "exec.run.failed" {
			receiptEvent = &inspection.RecentEvents[i]
			break
		}
	}
	if receiptEvent == nil {
		t.Fatalf("exec.run.failed event missing: %#v", inspection.RecentEvents)
	}
	if !json.Valid(receiptEvent.Facts) {
		t.Fatalf("event facts not valid JSON: %s", receiptEvent.Facts)
	}
	var eventFacts map[string]interface{}
	if err := json.Unmarshal(receiptEvent.Facts, &eventFacts); err != nil {
		t.Fatal(err)
	}
	if stdout, _ := eventFacts["exec.last_run.stdout"].(string); stdout != "compiling main.go\nbuild failed" {
		t.Fatalf("event facts stdout = %q", stdout)
	}

	execFacts, _ := inspection.Facts["exec"].(map[string]interface{})
	lastRun, _ := execFacts["last_run"].(map[string]interface{})
	if stdout, _ := lastRun["stdout"].(string); stdout != "compiling main.go\nbuild failed" {
		t.Fatalf("run facts stdout = %q", stdout)
	}
	if exitCode, _ := lastRun["exit_code"].(float64); exitCode != 2 {
		t.Fatalf("run facts exit_code = %v", exitCode)
	}
}

func TestMessageTimelineIncludesV2WorkflowStateTransitions(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID:            "run-timeline-state",
		TenantID:      "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace),
		WorkflowYAML:  []byte(workflowV2APIWorkflow),
		InitialClawID: "claw-timeline-state",
	}); err != nil {
		t.Fatal(err)
	}
	insertTestClaw(t, db, "claw-timeline-state")
	if _, err := db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		"msg-timeline-state", "claw-timeline-state", "test-tenant-id", "user", "hello"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/messages/claw-timeline-state/timeline", nil)
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
	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Role)
	}
	if !slices.Contains(roles, "state") {
		t.Fatalf("expected state transition in timeline, got roles %v", roles)
	}
	if !slices.Contains(roles, "user") {
		t.Fatalf("expected user message in timeline, got roles %v", roles)
	}
}
