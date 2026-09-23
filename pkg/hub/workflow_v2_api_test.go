package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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

	var effectStarts, effectFinishes, execOutcomes, taskAssigned, taskFinished int
	var outcome types.WorkflowEffectEvent
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
		case event.Kind == "exec.run" && event.Phase == "finished" && event.Stdout != "":
			execOutcomes++
			outcome = event
		case event.Kind == "exec.run" && event.Phase == "finished":
			effectFinishes++
		case event.Kind == "agent.task" && event.Phase == "assigned":
			taskAssigned++
			assigned = event
		case event.Kind == "agent.task" && event.Phase == "finished":
			taskFinished++
			finishedTask = event
		}
	}
	if effectStarts != 1 || effectFinishes != 1 {
		t.Fatalf("effect starts = %d, assignment finishes = %d (want 1 each)", effectStarts, effectFinishes)
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
