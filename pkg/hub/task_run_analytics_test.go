package hub

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestTaskRunSchemaCreatesIssue350TablesColumnsConstraintsAndIndexes(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	for _, table := range []string{
		"task_runs",
		"task_run_attempts",
		"task_run_events",
		"task_run_prs",
		"task_run_summaries",
	} {
		if !tableExists(t, db, table) {
			t.Fatalf("expected table %s to exist", table)
		}
	}

	for _, tc := range []struct {
		table  string
		column string
	}{
		{"claws", "task_run_id"},
		{"factory_triggers", "task_run_id"},
	} {
		if !columnExists(t, db, tc.table, tc.column) {
			t.Fatalf("expected %s.%s to exist", tc.table, tc.column)
		}
	}

	assertPrimaryKey(t, db, "task_runs", "id")
	assertPrimaryKey(t, db, "task_run_attempts", "id")
	assertPrimaryKey(t, db, "task_run_events", "id")
	assertPrimaryKey(t, db, "task_run_prs", "id")
	assertPrimaryKey(t, db, "task_run_summaries", "id")

	assertColumns(t, db, "task_runs", []string{
		"id", "tenant_id", "initial_attempt_id", "current_attempt_id", "attempt_count", "run_kind",
		"owner_type", "workspace_name", "workflow_name", "factory_name", "owner_id", "owner_display_name",
		"integration", "integration_workspace", "trigger_id", "external_trigger_id", "issue_id", "claw_id", "model", "llm_key", "tags",
		"analytics_enabled", "requires_pr", "excluded_reason", "timeout_at", "created_at", "updated_at",
	})
	assertColumns(t, db, "task_run_attempts", []string{
		"id", "tenant_id", "run_id", "attempt_id", "attempt_number", "trigger_id", "claw_id", "status",
		"failure_type", "restored_checkpoint_id", "started_at", "finished_at", "created_at", "updated_at",
	})
	assertColumns(t, db, "task_run_events", []string{
		"id", "tenant_id", "run_id", "attempt_id", "event_key", "source", "source_event_id",
		"source_delivery_id", "event_type", "event_time", "observed_at", "actor_type", "actor_source",
		"actor_id", "actor_login", "actor_display_name", "actor_classification_reason",
		"interaction_role", "target_type", "target_id", "target_url", "target_label",
		"warning_type", "failure_type", "detail", "created_at",
	})
	assertColumns(t, db, "task_run_prs", []string{
		"id", "tenant_id", "run_id", "repo", "pr_number", "pr_url", "head_sha", "head_branch",
		"last_agent_head_sha", "base_branch", "state", "merged", "opened_at", "closed_at", "merged_at",
		"merged_by_login", "created_at", "updated_at",
	})
	assertColumns(t, db, "task_run_summaries", []string{
		"id", "tenant_id", "run_id", "status", "phase", "attempt_count", "owner_type",
		"workspace_name", "workflow_name", "factory_name", "owner_id", "owner_display_name",
		"run_kind", "integration", "integration_workspace", "issue_id", "claw_id",
		"model", "llm_key", "repo", "primary_pr_url", "pr_count", "open_pr_count", "merged_pr_count",
		"closed_pr_count", "warning_types", "failure_type", "human_interaction_count",
		"started_at", "queued_at", "provision_started_at", "agent_started_at", "pr_opened_at",
		"merged_at", "finished_at", "timeout_at", "last_event_at", "materialized_at", "updated_at",
		"analytics_enabled", "requires_pr", "excluded_reason",
	})

	assertColumnDefault(t, db, "task_runs", "tags", "'[]'")

	now := int64(1760000000000)
	insertValidRun(t, db, "run-valid", now)
	insertValidAttempt(t, db, "attempt-valid", "run-valid", now)

	assertExecFails(t, db, `
		INSERT INTO task_runs(id, tenant_id, initial_attempt_id, current_attempt_id, run_kind, owner_type, tags, analytics_enabled, requires_pr, created_at, updated_at)
		VALUES('bad-run-kind','tenant','','','invalid','factory','[]',1,1,?,?)`, now, now)
	assertExecFails(t, db, `
		INSERT INTO task_runs(id, tenant_id, initial_attempt_id, current_attempt_id, run_kind, owner_type, tags, analytics_enabled, requires_pr, created_at, updated_at)
		VALUES('bad-tags','tenant','','','pr_task','factory','{}',1,1,?,?)`, now, now)
	assertExecSucceeds(t, db, `
		INSERT INTO task_runs(id, tenant_id, initial_attempt_id, current_attempt_id, run_kind, owner_type, tags, analytics_enabled, requires_pr, created_at, updated_at)
		VALUES('non-pr-analytics','tenant','attempt-non-pr-analytics','','code_task','factory','[]',1,0,?,?)`, now, now)
	assertExecFails(t, db, `
		INSERT INTO task_run_attempts(id, tenant_id, run_id, attempt_number, status, failure_type, started_at, created_at, updated_at)
		VALUES('bad-attempt-status','tenant','run-valid',2,'succeededish','',?,?,?)`, now, now, now)
	assertExecFails(t, db, `
		INSERT INTO task_run_attempts(id, tenant_id, run_id, attempt_number, status, failure_type, started_at, created_at, updated_at)
		VALUES('bad-attempt-failure','tenant','run-valid',2,'failed','not-real',?,?,?)`, now, now, now)
	assertExecFails(t, db, `
		INSERT INTO task_run_events(id, tenant_id, run_id, attempt_id, event_key, source, event_type, event_time, observed_at, actor_type, interaction_role, detail, created_at)
		VALUES('bad-event-source','tenant','run-valid','attempt-valid','event-1','not-real','task_start',?,?, 'system','neutral','{}',?)`, now, now, now)
	assertExecFails(t, db, `
		INSERT INTO task_run_events(id, tenant_id, run_id, attempt_id, event_key, source, event_type, event_time, observed_at, actor_type, interaction_role, detail, created_at)
		VALUES('bad-event-role','tenant','run-valid','attempt-valid','event-2','hub','task_start',?,?, 'system','comment','{}',?)`, now, now, now)
	assertExecFails(t, db, `
		INSERT INTO task_run_events(id, tenant_id, run_id, attempt_id, event_key, source, event_type, event_time, observed_at, actor_type, interaction_role, detail, created_at)
		VALUES('bad-event-detail','tenant','run-valid','attempt-valid','event-3','hub','task_start',?,?, 'system','neutral','not-json',?)`, now, now, now)
	assertExecFails(t, db, `
		INSERT INTO task_run_prs(id, tenant_id, run_id, repo, pr_number, state, merged, created_at, updated_at)
		VALUES('bad-pr-state','tenant','run-valid','elastic/claw',1,'merged',0,?,?)`, now, now)
	assertExecFails(t, db, `
		INSERT INTO task_run_summaries(id, tenant_id, run_id, status, phase, warning_types, started_at, last_event_at, materialized_at)
		VALUES('bad-summary','tenant','run-valid','clean','terminal','[]',?,?,?)`, now, now, now)

	for _, tc := range []struct {
		table string
		index string
	}{
		{"task_runs", "idx_task_runs_tenant_created"},
		{"task_runs", "idx_task_runs_claw"},
		{"task_runs", "idx_task_runs_trigger"},
		{"task_run_attempts", "idx_task_run_attempts_run_number"},
		{"task_run_events", "idx_task_run_events_run_time"},
		{"task_run_events", "idx_task_run_events_tenant_key"},
		{"task_run_events", "idx_task_run_events_tenant_run_time"},
		{"task_run_events", "idx_task_run_events_source_event"},
		{"task_run_events", "idx_task_run_events_observed"},
		{"task_run_prs", "idx_task_run_prs_run"},
		{"task_run_prs", "idx_task_run_prs_repo_pr"},
		{"task_run_prs", "idx_task_run_prs_tenant_run"},
		{"task_run_prs", "idx_task_run_prs_tenant_merged"},
		{"task_run_summaries", "idx_task_run_summaries_status"},
		{"task_run_summaries", "idx_task_run_summaries_owner_started"},
		{"task_run_summaries", "idx_task_run_summaries_run"},
		{"task_run_summaries", "idx_task_run_summaries_started_run"},
		{"task_run_summaries", "idx_task_run_summaries_workspace"},
		{"task_run_summaries", "idx_task_run_summaries_workflow"},
		{"task_run_summaries", "idx_task_run_summaries_factory"},
		{"task_run_summaries", "idx_task_run_summaries_model"},
		{"task_run_summaries", "idx_task_run_summaries_repo"},
		{"task_run_summaries", "idx_task_run_summaries_timeout"},
	} {
		if !indexExists(t, db, tc.table, tc.index) {
			t.Fatalf("expected index %s on %s", tc.index, tc.table)
		}
	}
}

func TestTaskRunMaterializationClassifiesCleanSuccess(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-clean")
	runID, attemptID := startTaskRunForTest(t, s, "claw-clean", "clean")

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "clean:claw-created",
		EventType: taskRunEventClawCreated,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceHub,
	})
	associatePRForTest(t, s, runID, "elastic/claw", 11, taskRunPRStateOpen)
	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "clean:pr-merged",
		EventType: taskRunEventPRMerged,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceGitHub,
		Detail:    map[string]any{"repo": "elastic/claw", "prNumber": 11},
	})

	assertTaskRunSummary(t, db, runID, taskRunStatusCleanSuccess, taskRunPhaseTerminal, "", "[]", 0, 1, 0, 1, 0)
	assertTaskRunAttempt(t, db, runID, "succeeded", "")
}

func TestTaskRunMaterializationClassifiesWarningSuccess(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-warning")
	runID, attemptID := startTaskRunForTest(t, s, "claw-warning", "warning")

	associatePRForTest(t, s, runID, "elastic/claw", 12, taskRunPRStateOpen)
	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:        "test-tenant-id",
		RunID:           runID,
		AttemptID:       attemptID,
		EventKey:        "warning:human-comment",
		EventType:       taskRunEventHumanPRComment,
		ActorType:       taskRunActorHuman,
		Source:          taskRunSourceGitHub,
		InteractionRole: taskRunInteractionWarning,
		WarningType:     taskRunWarningHumanPRComment,
		Detail:          map[string]any{"repo": "elastic/claw", "prNumber": 12},
	})
	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "warning:pr-merged",
		EventType: taskRunEventPRMerged,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceGitHub,
		Detail:    map[string]any{"repo": "elastic/claw", "prNumber": 12},
	})

	assertTaskRunSummary(t, db, runID, taskRunStatusWarningSuccess, taskRunPhaseTerminal, "", `["human_pr_comment"]`, 1, 1, 0, 1, 0)
}

func TestTaskRunClassificationFailsDoneWithoutPR(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-failed")
	runID, attemptID := startTaskRunForTest(t, s, "claw-failed", "failed")

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:    "test-tenant-id",
		RunID:       runID,
		AttemptID:   attemptID,
		EventKey:    "failed:done-without-pr",
		EventType:   taskRunEventDoneWithoutPR,
		ActorType:   taskRunActorAgent,
		Source:      taskRunSourceHub,
		FailureType: taskRunFailureDoneWithoutPR,
	})

	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureDoneWithoutPR, "[]", 0, 0, 0, 0, 0)
}

func TestTaskRunMaterializationClassifiesReplacedPRAsWarningSuccess(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-multi")
	runID, attemptID := startTaskRunForTest(t, s, "claw-multi", "multi")

	associatePRForTest(t, s, runID, "elastic/claw", 21, taskRunPRStateOpen)
	associatePRForTest(t, s, runID, "elastic/claw", 22, taskRunPRStateOpen)
	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "multi:pr21-merged",
		EventType: taskRunEventPRMerged,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceGitHub,
		Detail:    map[string]any{"repo": "elastic/claw", "prNumber": 21},
	})

	assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseWaitingForMerge, "", "[]", 0, 2, 1, 1, 0)

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "multi:pr22-closed",
		EventType: taskRunEventPRClosedUnmerged,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceGitHub,
		Detail:    map[string]any{"repo": "elastic/claw", "prNumber": 22},
	})

	assertTaskRunSummary(t, db, runID, taskRunStatusWarningSuccess, taskRunPhaseTerminal, "", `["pr_replaced"]`, 0, 2, 0, 1, 1)
}

func TestTaskRunIdempotencyDoesNotDuplicateEventRows(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-idempotent")
	runID, attemptID := startTaskRunForTest(t, s, "claw-idempotent", "idempotent")

	input := TaskRunEvent{
		TenantID:        "test-tenant-id",
		RunID:           runID,
		AttemptID:       attemptID,
		EventKey:        "idempotent:human-comment",
		EventType:       taskRunEventHumanPRComment,
		ActorType:       taskRunActorHuman,
		Source:          taskRunSourceGitHub,
		InteractionRole: taskRunInteractionWarning,
		Detail:          map[string]any{"body": "please revise"},
	}
	recordTaskRunEventForTest(t, s, input)
	recordTaskRunEventForTest(t, s, input)

	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_key=?`, runID, input.EventKey).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("expected one idempotent event row, got %d", events)
	}

	assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseAgentRunning, "", `["human_pr_comment"]`, 1, 0, 0, 0, 0)
}

func TestTaskRunMaterializationLateWarningReclassifiesCleanSuccess(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-late")
	runID, attemptID := startTaskRunForTest(t, s, "claw-late", "late")

	associatePRForTest(t, s, runID, "elastic/claw", 31, taskRunPRStateOpen)
	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "late:pr-merged",
		EventType: taskRunEventPRMerged,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceGitHub,
		Detail:    map[string]any{"repo": "elastic/claw", "prNumber": 31},
	})
	assertTaskRunSummary(t, db, runID, taskRunStatusCleanSuccess, taskRunPhaseTerminal, "", "[]", 0, 1, 0, 1, 0)

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:        "test-tenant-id",
		RunID:           runID,
		AttemptID:       attemptID,
		EventKey:        "late:human-comment",
		EventType:       taskRunEventHumanPRComment,
		ActorType:       taskRunActorHuman,
		Source:          taskRunSourceGitHub,
		InteractionRole: taskRunInteractionWarning,
		WarningType:     taskRunWarningHumanPRComment,
		OccurredAt:      time.Now().UTC().Add(-time.Second),
	})

	assertTaskRunSummary(t, db, runID, taskRunStatusWarningSuccess, taskRunPhaseTerminal, "", `["human_pr_comment"]`, 1, 1, 0, 1, 0)
}

func TestTaskRunMaterializationLateMergeOverridesPrePRFailure(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-late-merge")
	runID, attemptID := startTaskRunForTest(t, s, "claw-late-merge", "late-merge")

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:    "test-tenant-id",
		RunID:       runID,
		AttemptID:   attemptID,
		EventKey:    "late-merge:done-without-pr",
		EventType:   taskRunEventDoneWithoutPR,
		ActorType:   taskRunActorAgent,
		Source:      taskRunSourceHub,
		FailureType: taskRunFailureDoneWithoutPR,
	})
	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureDoneWithoutPR, "[]", 0, 0, 0, 0, 0)

	associatePRForTest(t, s, runID, "elastic/claw", 41, taskRunPRStateOpen)
	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "late-merge:pr-merged",
		EventType: taskRunEventPRMerged,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceGitHub,
		Detail:    map[string]any{"repo": "elastic/claw", "prNumber": 41},
	})

	assertTaskRunSummary(t, db, runID, taskRunStatusCleanSuccess, taskRunPhaseTerminal, "", "[]", 0, 1, 0, 1, 0)
	assertTaskRunAttempt(t, db, runID, "succeeded", "")
}

func TestTaskRunMaterializationTerminalStopDoesNotRegressToRunning(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-terminal")
	runID, attemptID := startTaskRunForTest(t, s, "claw-terminal", "terminal")

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:    "test-tenant-id",
		RunID:       runID,
		AttemptID:   attemptID,
		EventKey:    "terminal:stop",
		EventType:   taskRunEventManualStopBeforeDelivery,
		ActorType:   taskRunActorHuman,
		Source:      taskRunSourceDashboard,
		FailureType: taskRunFailureManualStopDelivery,
	})
	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureManualStopDelivery, `["unknown_human_interaction"]`, 1, 0, 0, 0, 0)

	associatePRForTest(t, s, runID, "elastic/claw", 51, taskRunPRStateOpen)
	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureManualStopDelivery, `["unknown_human_interaction"]`, 1, 1, 1, 0, 0)
}

func TestTaskRunMaterializationIgnoresUnknownWarningTypes(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-bad-warning")
	runID, attemptID := startTaskRunForTest(t, s, "claw-bad-warning", "bad-warning")

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:    "test-tenant-id",
		RunID:       runID,
		AttemptID:   attemptID,
		EventKey:    "bad-warning:event",
		EventType:   taskRunEventClawCreated,
		ActorType:   taskRunActorSystem,
		Source:      taskRunSourceHub,
		WarningType: "human_interaction",
	})

	assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseAgentRunning, "", "[]", 0, 0, 0, 0, 0)
}

func TestTaskRunMaterializationUsesClawLifecyclePhase(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		s, db := newTaskRunAnalyticsTestServer(t, "claw-pending-phase")
		if _, err := db.Exec(`UPDATE claws SET status='pending' WHERE id=?`, "claw-pending-phase"); err != nil {
			t.Fatalf("set claw status: %v", err)
		}
		runID, _ := startTaskRunForTest(t, s, "claw-pending-phase", "pending-phase")

		assertTaskRunEventExists(t, db, runID, taskRunEventRunQueued, taskRunInteractionNeutral)
		assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseQueued, "", "[]", 0, 0, 0, 0, 0)
	})

	t.Run("provisioning", func(t *testing.T) {
		s, db := newTaskRunAnalyticsTestServer(t, "claw-provision-phase")
		if _, err := db.Exec(`UPDATE claws SET status='provisioning' WHERE id=?`, "claw-provision-phase"); err != nil {
			t.Fatalf("set claw status: %v", err)
		}
		runID, _ := startTaskRunForTest(t, s, "claw-provision-phase", "provision-phase")

		assertTaskRunEventExists(t, db, runID, taskRunEventProvisionStarted, taskRunInteractionNeutral)
		assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseProvisioning, "", "[]", 0, 0, 0, 0, 0)
	})
}

func TestTaskRunEventInsertDoesNotIgnoreInvalidConstraints(t *testing.T) {
	s, _ := newTaskRunAnalyticsTestServer(t, "claw-invalid-event")
	runID, attemptID := startTaskRunForTest(t, s, "claw-invalid-event", "invalid-event")

	err := s.recordTaskRunEvent(TaskRunEvent{
		TenantID:        "test-tenant-id",
		RunID:           runID,
		AttemptID:       attemptID,
		EventKey:        "invalid-event:bad-source",
		EventType:       taskRunEventClawCreated,
		ActorType:       taskRunActorSystem,
		Source:          "bad-source",
		InteractionRole: taskRunInteractionNeutral,
	})
	if err == nil {
		t.Fatalf("expected invalid event source to return an error")
	}
}

func TestTaskRunPRLifecycleDoesNotRegressMergedPRToOpen(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-pr-regress")
	runID, attemptID := startTaskRunForTest(t, s, "claw-pr-regress", "pr-regress")

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "pr-regress:merged-first",
		EventType: taskRunEventPRMerged,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceGitHub,
		Detail:    map[string]any{"repo": "elastic/claw", "prNumber": 61},
	})
	associatePRForTest(t, s, runID, "elastic/claw", 61, taskRunPRStateOpen)

	assertTaskRunSummary(t, db, runID, taskRunStatusCleanSuccess, taskRunPhaseTerminal, "", "[]", 0, 1, 0, 1, 0)
}

func TestTaskRunPRLifecycleDoesNotRegressClosedPRToOpen(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-pr-closed-regress")
	runID, attemptID := startTaskRunForTest(t, s, "claw-pr-closed-regress", "pr-closed-regress")

	recordTaskRunEventForTest(t, s, TaskRunEvent{
		TenantID:  "test-tenant-id",
		RunID:     runID,
		AttemptID: attemptID,
		EventKey:  "pr-closed-regress:closed-first",
		EventType: taskRunEventPRClosedUnmerged,
		ActorType: taskRunActorSystem,
		Source:    taskRunSourceGitHub,
		Detail:    map[string]any{"repo": "elastic/claw", "prNumber": 62},
	})
	associatePRForTest(t, s, runID, "elastic/claw", 62, taskRunPRStateOpen)

	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailurePRClosedUnmerged, "[]", 0, 1, 0, 0, 1)
}

func TestTaskRunDetailSanitizationRedactsNestedText(t *testing.T) {
	detail, err := sanitizeTaskRunDetail(map[string]any{
		"repo": "elastic/claw",
		"comment": map[string]any{
			"body": "full private comment body",
		},
		"reviews": []any{
			map[string]any{"content": "inline review body"},
		},
	})
	if err != nil {
		t.Fatalf("sanitize detail: %v", err)
	}
	if strings.Contains(detail, "full private comment body") || strings.Contains(detail, "inline review body") {
		t.Fatalf("detail was not redacted: %s", detail)
	}
	if !strings.Contains(detail, `"redacted":true`) {
		t.Fatalf("detail missing redaction marker: %s", detail)
	}
}

func newTaskRunAnalyticsTestServer(t *testing.T, clawID string) (*Server, *sql.DB) {
	t.Helper()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	_, err := db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, status, created_at, tags)
		VALUES(?,?,?,?,?,?,?)`,
		clawID, "test-tenant-id", clawID, "elasticclaw", "running", now(), `["workspace:eng"]`,
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	return s, db
}

func startTaskRunForTest(t *testing.T, s *Server, clawID, keyPrefix string) (string, string) {
	t.Helper()
	runID, attemptID, err := s.ensureTaskRunForClaw(clawID, TaskRunStart{
		TenantID:         "test-tenant-id",
		RunKind:          taskRunKindPRTask,
		OwnerType:        taskRunOwnerFactory,
		OwnerName:        "factory-" + keyPrefix,
		WorkspaceName:    "eng",
		FactoryName:      "factory-" + keyPrefix,
		Integration:      "github",
		Model:            "gpt-5",
		LLMKey:           "openai:gpt-5",
		Source:           taskRunSourceFactory,
		IssueID:          "ISSUE-" + keyPrefix,
		AnalyticsEnabled: true,
		RequiresPR:       true,
		EventKey:         keyPrefix + ":task-start",
		Tags:             []string{"test:" + keyPrefix},
	})
	if err != nil {
		t.Fatalf("ensure task run: %v", err)
	}
	if runID == "" || attemptID == "" {
		t.Fatalf("expected runID and attemptID, got %q %q", runID, attemptID)
	}
	return runID, attemptID
}

func recordTaskRunEventForTest(t *testing.T, s *Server, input TaskRunEvent) {
	t.Helper()
	if input.OccurredAt.IsZero() {
		input.OccurredAt = now()
	}
	if err := s.recordTaskRunEvent(input); err != nil {
		t.Fatalf("record task run event %s: %v", input.EventKey, err)
	}
}

func associatePRForTest(t *testing.T, s *Server, runID, repo string, prNumber int, state string) {
	t.Helper()
	if err := s.associateTaskRunPR(TaskRunPR{
		TenantID:   "test-tenant-id",
		RunID:      runID,
		Repo:       repo,
		PRNumber:   prNumber,
		URL:        "https://github.com/" + repo + "/pull/" + strconv.Itoa(prNumber),
		State:      state,
		OccurredAt: now(),
	}); err != nil {
		t.Fatalf("associate task run PR #%d: %v", prNumber, err)
	}
}

func newTaskRunLifecycleTestServer(t *testing.T, factories []*types.FactoryConfig) (*Server, *sql.DB) {
	t.Helper()
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	return NewTestServerWithConfig(t, &types.HubConfig{
		Token:        "test-token",
		ClawToken:    "test-claw-token",
		DefaultModel: "gpt-5",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
		Factories: factories,
	}, "", "", "")
}

func taskRunLifecycleFactoryConfig(name, integration string) *types.FactoryConfig {
	return &types.FactoryConfig{
		Name:                name,
		Integration:         integration,
		Template:            "elasticclaw",
		Provider:            "noop",
		EnableManualTrigger: true,
		Inputs:              []types.FactoryInput{{Name: "task", Type: "string"}},
	}
}

func assertClawLinkedToTaskRun(t *testing.T, db *sql.DB, clawID string) string {
	t.Helper()
	var runID string
	if err := db.QueryRow(`SELECT COALESCE(task_run_id,'') FROM claws WHERE id=?`, clawID).Scan(&runID); err != nil {
		t.Fatalf("read claw task_run_id: %v", err)
	}
	if runID == "" {
		t.Fatalf("expected claw %s to be linked to a task run", clawID)
	}
	return runID
}

func assertTaskRunRow(t *testing.T, db *sql.DB, runID, ownerType, workspace, workflow, factory, runKind, integration string) {
	t.Helper()
	var gotOwnerType, gotWorkspace, gotWorkflow, gotFactory, gotRunKind, gotIntegration string
	if err := db.QueryRow(`
		SELECT owner_type, workspace_name, workflow_name, factory_name, run_kind, integration
		  FROM task_runs
		 WHERE id=?`, runID).Scan(&gotOwnerType, &gotWorkspace, &gotWorkflow, &gotFactory, &gotRunKind, &gotIntegration); err != nil {
		t.Fatalf("read task run row: %v", err)
	}
	if gotOwnerType != ownerType || gotWorkspace != workspace || gotWorkflow != workflow || gotFactory != factory || gotRunKind != runKind || gotIntegration != integration {
		t.Fatalf("task run row mismatch: got owner=%q workspace=%q workflow=%q factory=%q kind=%q integration=%q; want owner=%q workspace=%q workflow=%q factory=%q kind=%q integration=%q",
			gotOwnerType, gotWorkspace, gotWorkflow, gotFactory, gotRunKind, gotIntegration,
			ownerType, workspace, workflow, factory, runKind, integration)
	}
}

func assertTaskRunCounts(t *testing.T, db *sql.DB, runID string, wantRuns, wantAttempts int) {
	t.Helper()
	var runs, attempts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_runs WHERE id=?`, runID).Scan(&runs); err != nil {
		t.Fatalf("count task runs: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_attempts WHERE run_id=?`, runID).Scan(&attempts); err != nil {
		t.Fatalf("count task run attempts: %v", err)
	}
	if runs != wantRuns || attempts != wantAttempts {
		t.Fatalf("task run counts mismatch: got runs=%d attempts=%d, want runs=%d attempts=%d", runs, attempts, wantRuns, wantAttempts)
	}
}

func assertNoTaskRunOrphanClaws(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE COALESCE(task_run_id,'') = ''`).Scan(&count); err != nil {
		t.Fatalf("count orphan claws: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected no claw rows without task_run_id after analytics failure, got %d", count)
	}
}

func assertTaskRunEventExists(t *testing.T, db *sql.DB, runID, eventType, interactionRole string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type=? AND interaction_role=?`, runID, eventType, interactionRole).Scan(&count); err != nil {
		t.Fatalf("count task run event %s: %v", eventType, err)
	}
	if count == 0 {
		t.Fatalf("expected task run event %s/%s for run %s", eventType, interactionRole, runID)
	}
}

func assertTaskRunAttempt(t *testing.T, db *sql.DB, runID, wantStatus, wantFailure string) {
	t.Helper()
	var gotStatus, gotFailure string
	if err := db.QueryRow(`
		SELECT status, failure_type
		  FROM task_run_attempts
		 WHERE run_id=?`, runID).Scan(&gotStatus, &gotFailure); err != nil {
		t.Fatalf("read task run attempt: %v", err)
	}
	if gotStatus != wantStatus || gotFailure != wantFailure {
		t.Fatalf("task run attempt mismatch: got status=%q failure=%q, want status=%q failure=%q", gotStatus, gotFailure, wantStatus, wantFailure)
	}
}

func assertTaskRunPR(t *testing.T, db *sql.DB, runID, repo string, prNumber int, state string, merged bool) {
	t.Helper()
	var gotState string
	var gotMerged int
	if err := db.QueryRow(`
		SELECT state, merged
		  FROM task_run_prs
		 WHERE run_id=? AND repo=? AND pr_number=?`, runID, repo, prNumber).Scan(&gotState, &gotMerged); err != nil {
		t.Fatalf("read task run PR: %v", err)
	}
	if gotState != state || gotMerged != boolInt(merged) {
		t.Fatalf("task run PR mismatch: got state=%q merged=%d, want state=%q merged=%d", gotState, gotMerged, state, boolInt(merged))
	}
}

func assertTaskRunSummary(
	t *testing.T,
	db *sql.DB,
	runID, wantStatus, wantPhase, wantFailure, wantWarnings string,
	wantHumanInteractions, wantPRCount, wantOpenPRs, wantMergedPRs, wantClosedPRs int,
) {
	t.Helper()
	var gotStatus, gotPhase, gotFailure, gotWarnings string
	var gotHumanInteractions, gotPRCount, gotOpenPRs, gotMergedPRs, gotClosedPRs int
	err := db.QueryRow(`
		SELECT status, phase, failure_type, warning_types, human_interaction_count, pr_count, open_pr_count, merged_pr_count, closed_pr_count
		  FROM task_run_summaries
		 WHERE run_id=?`, runID).Scan(
		&gotStatus, &gotPhase, &gotFailure, &gotWarnings, &gotHumanInteractions,
		&gotPRCount, &gotOpenPRs, &gotMergedPRs, &gotClosedPRs,
	)
	if err != nil {
		t.Fatalf("read task run summary: %v", err)
	}
	if gotStatus != wantStatus || gotPhase != wantPhase || gotFailure != wantFailure || gotWarnings != wantWarnings ||
		gotHumanInteractions != wantHumanInteractions || gotPRCount != wantPRCount || gotOpenPRs != wantOpenPRs ||
		gotMergedPRs != wantMergedPRs || gotClosedPRs != wantClosedPRs {
		t.Fatalf(
			"summary mismatch: got status=%s phase=%s failure=%s warnings=%s human=%d pr=%d open=%d merged=%d closed=%d; want status=%s phase=%s failure=%s warnings=%s human=%d pr=%d open=%d merged=%d closed=%d",
			gotStatus, gotPhase, gotFailure, gotWarnings, gotHumanInteractions, gotPRCount, gotOpenPRs, gotMergedPRs, gotClosedPRs,
			wantStatus, wantPhase, wantFailure, wantWarnings, wantHumanInteractions, wantPRCount, wantOpenPRs, wantMergedPRs, wantClosedPRs,
		)
	}
}

func TestTaskRunSummaryUsesStartModelAndLLMKey(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-model")
	runID, _ := startTaskRunForTest(t, s, "claw-model", "model")

	var model, llmKey string
	if err := db.QueryRow(`SELECT model, llm_key FROM task_run_summaries WHERE run_id=?`, runID).Scan(&model, &llmKey); err != nil {
		t.Fatalf("read summary model: %v", err)
	}
	if model != "gpt-5" || llmKey != "openai:gpt-5" {
		t.Fatalf("summary model mismatch: got %q/%q", model, llmKey)
	}
}

func TestTaskRunFactoryCreationInstrumentationCreatesRunAttemptEventAndLinksClaw(t *testing.T) {
	s, db := newTaskRunLifecycleTestServer(t, []*types.FactoryConfig{taskRunLifecycleFactoryConfig("manual-factory", "linear")})

	clawID, pending, err := s.createClawFromFactory(s.hubCfg.Factories[0], "", map[string]string{"task": "Fix login"}, nil, "manual trigger")
	if err != nil {
		t.Fatalf("create claw from factory: %v", err)
	}
	if pending {
		t.Fatal("expected factory claw to start immediately")
	}

	runID := assertClawLinkedToTaskRun(t, db, clawID)
	assertTaskRunRow(t, db, runID, taskRunOwnerFactory, "", "", "manual-factory", taskRunKindCodeTask, "linear")
	assertTaskRunCounts(t, db, runID, 1, 1)
	assertTaskRunEventExists(t, db, runID, taskRunEventTaskStart, taskRunInteractionAllowedStart)
}

func TestTaskRunCreationInstrumentationHonorsAnalyticsContract(t *testing.T) {
	s, db := newTaskRunLifecycleTestServer(t, []*types.FactoryConfig{{
		Name:                "external-non-pr",
		Integration:         "external",
		Template:            "elasticclaw",
		Provider:            "noop",
		EnableManualTrigger: true,
		Inputs:              []types.FactoryInput{{Name: "task", Type: "string"}},
	}})

	clawID, _, err := s.createClawFromFactory(s.hubCfg.Factories[0], "", map[string]string{"task": "Notify"}, nil, "manual trigger")
	if err != nil {
		t.Fatalf("create external claw: %v", err)
	}
	runID := assertClawLinkedToTaskRun(t, db, clawID)

	var analyticsEnabled, requiresPR int
	var excludedReason string
	if err := db.QueryRow(`
		SELECT analytics_enabled, requires_pr, excluded_reason
		  FROM task_runs
		 WHERE id=?`, runID).Scan(&analyticsEnabled, &requiresPR, &excludedReason); err != nil {
		t.Fatalf("read analytics contract: %v", err)
	}
	if analyticsEnabled != 0 || requiresPR != 0 || excludedReason != "analytics_disabled" {
		t.Fatalf("analytics contract mismatch: enabled=%d requires_pr=%d excluded=%q", analyticsEnabled, requiresPR, excludedReason)
	}
}

func TestTaskRunCreationInstrumentationKeepsEnabledNonPRContract(t *testing.T) {
	enabled := true
	requiresPR := false
	s, db := newTaskRunLifecycleTestServer(t, []*types.FactoryConfig{{
		Name:                "linear-code-task",
		Integration:         "linear",
		Template:            "elasticclaw",
		Provider:            "noop",
		EnableManualTrigger: true,
		AnalyticsEnabled:    &enabled,
		RequiresPR:          &requiresPR,
		Inputs:              []types.FactoryInput{{Name: "task", Type: "string"}},
	}})

	clawID, _, err := s.createClawFromFactory(s.hubCfg.Factories[0], "ELA-123", map[string]string{"task": "Notify"}, nil, "manual trigger")
	if err != nil {
		t.Fatalf("create non-pr claw: %v", err)
	}
	runID := assertClawLinkedToTaskRun(t, db, clawID)

	var analyticsEnabled, gotRequiresPR int
	var excludedReason string
	if err := db.QueryRow(`
		SELECT analytics_enabled, requires_pr, excluded_reason
		  FROM task_runs
		 WHERE id=?`, runID).Scan(&analyticsEnabled, &gotRequiresPR, &excludedReason); err != nil {
		t.Fatalf("read analytics contract: %v", err)
	}
	if analyticsEnabled != 1 || gotRequiresPR != 0 || excludedReason != "non_pr_producing" {
		t.Fatalf("analytics contract mismatch: enabled=%d requires_pr=%d excluded=%q", analyticsEnabled, gotRequiresPR, excludedReason)
	}
}

func TestTaskRunWorkflowCreationInstrumentationCreatesWorkflowOwnerRun(t *testing.T) {
	s, db := newTaskRunLifecycleTestServer(t, nil)
	workspace := &types.WorkspaceConfig{Name: "eng", Files: map[string]string{"README.md": "test workspace"}}
	workflow := &types.WorkflowConfig{
		Name:                "review-workflow",
		Provider:            "noop",
		Integration:         "github-issues",
		EnableManualTrigger: true,
		Inputs:              []types.FactoryInput{{Name: "task", Type: "string"}},
	}

	clawID, pending, err := s.createClawFromWorkflow(workspace, workflow, map[string]string{"task": "Review PR"}, "manual workflow trigger")
	if err != nil {
		t.Fatalf("create claw from workflow: %v", err)
	}
	if pending {
		t.Fatal("expected workflow claw to start immediately")
	}

	runID := assertClawLinkedToTaskRun(t, db, clawID)
	assertTaskRunRow(t, db, runID, taskRunOwnerWorkflow, "eng", "review-workflow", "", taskRunKindCodeTask, "github-issues")
	assertTaskRunCounts(t, db, runID, 1, 1)
	assertTaskRunEventExists(t, db, runID, taskRunEventTaskStart, taskRunInteractionAllowedStart)
}

func TestTaskRunCreationInstrumentationSurfacesStartAnalyticsFailure(t *testing.T) {
	s, db := newTaskRunLifecycleTestServer(t, []*types.FactoryConfig{taskRunLifecycleFactoryConfig("manual-factory", "linear")})
	if _, err := db.Exec(`DROP TABLE task_runs`); err != nil {
		t.Fatalf("drop task_runs: %v", err)
	}

	if _, _, err := s.createClawFromFactory(s.hubCfg.Factories[0], "", map[string]string{"task": "Fix login"}, nil, "manual trigger"); err == nil || !strings.Contains(err.Error(), "task run analytics") {
		t.Fatalf("expected factory creation to surface analytics error, got %v", err)
	}
	assertNoTaskRunOrphanClaws(t, db)

	s2, db2 := newTaskRunLifecycleTestServer(t, nil)
	if _, err := db2.Exec(`DROP TABLE task_runs`); err != nil {
		t.Fatalf("drop workflow task_runs: %v", err)
	}
	workspace := &types.WorkspaceConfig{Name: "eng", Files: map[string]string{"README.md": "test workspace"}}
	workflow := &types.WorkflowConfig{Name: "review-workflow", Provider: "noop", Integration: "github-issues"}
	if _, _, err := s2.createClawFromWorkflow(workspace, workflow, nil, "manual workflow trigger"); err == nil || !strings.Contains(err.Error(), "task run analytics") {
		t.Fatalf("expected workflow creation to surface analytics error, got %v", err)
	}
	assertNoTaskRunOrphanClaws(t, db2)
}

func TestTaskRunPRMentionInstrumentationAssociatesPROpenedEvent(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-pr-mention")
	runID, _ := startTaskRunForTest(t, s, "claw-pr-mention", "mention")

	s.storePRMention("claw-pr-mention", "elastic/claw", 71, "https://github.com/elastic/claw/pull/71")

	assertTaskRunPR(t, db, runID, "elastic/claw", 71, taskRunPRStateOpen, false)
	assertTaskRunEventExists(t, db, runID, taskRunEventPRAssociated, taskRunInteractionNeutral)
	assertTaskRunEventExists(t, db, runID, taskRunEventPROpened, taskRunInteractionNeutral)
}

func TestTaskRunPRMentionAnalyticsFailureDoesNotRollbackPRTracking(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-pr-mention-failure")
	startTaskRunForTest(t, s, "claw-pr-mention-failure", "mention-failure")
	if _, err := db.Exec(`DROP TABLE task_run_prs`); err != nil {
		t.Fatalf("drop task_run_prs: %v", err)
	}

	s.storePRMention("claw-pr-mention-failure", "elastic/claw", 76, "https://github.com/elastic/claw/pull/76")

	var count int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		  FROM claw_prs
		 WHERE claw_id=? AND repo=? AND pr_number=? AND pr_url=?`,
		"claw-pr-mention-failure", "elastic/claw", 76, "https://github.com/elastic/claw/pull/76",
	).Scan(&count); err != nil {
		t.Fatalf("count tracked PR: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected PR tracking row to survive analytics failure, got %d", count)
	}
}

func TestTaskRunPRMergeAndCloseInstrumentationMaterializesStatus(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-pr-lifecycle")
	runID, _ := startTaskRunForTest(t, s, "claw-pr-lifecycle", "pr-lifecycle")
	associatePRForTest(t, s, runID, "elastic/claw", 72, taskRunPRStateOpen)

	s.trackPRMerged("factory-pr-lifecycle", "ISSUE-pr-lifecycle", "claw-pr-lifecycle", "elastic/claw", 72)

	assertTaskRunEventExists(t, db, runID, taskRunEventPRMerged, taskRunInteractionTerminal)
	assertTaskRunPR(t, db, runID, "elastic/claw", 72, taskRunPRStateClosed, true)
	assertTaskRunSummary(t, db, runID, taskRunStatusCleanSuccess, taskRunPhaseTerminal, "", "[]", 0, 1, 0, 1, 0)

	s2, db2 := newTaskRunAnalyticsTestServer(t, "claw-pr-closed")
	closedRunID, _ := startTaskRunForTest(t, s2, "claw-pr-closed", "pr-closed")
	associatePRForTest(t, s2, closedRunID, "elastic/claw", 73, taskRunPRStateOpen)

	s2.trackPRClosed("factory-pr-closed", "ISSUE-pr-closed", "claw-pr-closed", "elastic/claw", 73)

	assertTaskRunEventExists(t, db2, closedRunID, taskRunEventPRClosedUnmerged, taskRunInteractionTerminal)
	assertTaskRunPR(t, db2, closedRunID, "elastic/claw", 73, taskRunPRStateClosed, false)
	assertTaskRunSummary(t, db2, closedRunID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailurePRClosedUnmerged, "[]", 0, 1, 0, 0, 1)
}

func TestTaskRunPRClosedFailureSurvivesOperationalStop(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-pr-closed-stop")
	runID, _ := startTaskRunForTest(t, s, "claw-pr-closed-stop", "pr-closed-stop")
	associatePRForTest(t, s, runID, "elastic/claw", 74, taskRunPRStateOpen)

	s.trackPRClosed("factory-pr-closed-stop", "ISSUE-pr-closed-stop", "claw-pr-closed-stop", "elastic/claw", 74)
	s.stopAgentWithReason("claw-pr-closed-stop", "PR closed without merge", false)

	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailurePRClosedUnmerged, "[]", 0, 1, 0, 0, 1)
}

func TestTaskRunStopAgentWithReasonInstrumentationRecordsTerminalFailure(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-agent-stop")
	runID, _ := startTaskRunForTest(t, s, "claw-agent-stop", "agent-stop")

	s.stopAgentWithReason("claw-agent-stop", "sandbox terminated unexpectedly", true)

	assertTaskRunEventExists(t, db, runID, taskRunEventAgentStopped, taskRunInteractionTerminal)
	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureAgentStopped, "[]", 0, 0, 0, 0, 0)
}

func TestTaskRunHumanInteractionInstrumentationRecordsWarnings(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-human-events")
	runID, _ := startTaskRunForTest(t, s, "claw-human-events", "human-events")
	pr := clawPR{
		clawID:              "claw-human-events",
		repo:                "elastic/claw",
		prNumber:            75,
		prURL:               "https://github.com/elastic/claw/pull/75",
		lastCommentID:       10,
		lastReviewCommentID: 20,
	}

	s.checkPRComments(pr, []interface{}{
		map[string]interface{}{
			"id":       float64(11),
			"user":     map[string]interface{}{"login": "octo", "type": "User"},
			"body":     "please adjust naming",
			"html_url": "https://github.com/elastic/claw/pull/75#issuecomment-11",
		},
	}, prCommentOptions{skipBugbot: true, skipGreptile: true, forward: true})
	s.checkGreptileReviewComments(pr, []interface{}{
		map[string]interface{}{
			"id":       float64(21),
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
			"body":     "inline concern",
			"html_url": "https://github.com/elastic/claw/pull/75#discussion_r21",
			"path":     "pkg/hub/server.go",
			"line":     float64(42),
		},
	})
	s.recordTaskRunDashboardMessage("claw-human-events", "dashboard-user", "msg-1")

	assertTaskRunEventExists(t, db, runID, taskRunEventHumanPRComment, taskRunInteractionWarning)
	assertTaskRunEventExists(t, db, runID, taskRunEventHumanReviewComment, taskRunInteractionWarning)
	assertTaskRunEventExists(t, db, runID, taskRunEventHumanDashboardMessage, taskRunInteractionWarning)
	assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseAgentRunning, "", `["human_dashboard_message","human_pr_comment","human_review_comment"]`, 3, 0, 0, 0, 0)
}

func TestTaskRunHumanPRCommentInstrumentationRecordsWithoutForwarding(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-human-pr-comment")
	runID, _ := startTaskRunForTest(t, s, "claw-human-pr-comment", "human-pr-comment")
	pr := clawPR{
		clawID:        "claw-human-pr-comment",
		repo:          "elastic/claw",
		prNumber:      76,
		prURL:         "https://github.com/elastic/claw/pull/76",
		lastCommentID: 100,
	}

	s.checkPRComments(pr, []interface{}{
		map[string]interface{}{
			"id":       float64(101),
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
			"body":     "normal PR comment",
			"html_url": "https://github.com/elastic/claw/pull/76#issuecomment-101",
		},
	}, prCommentOptions{skipBugbot: true, skipGreptile: true})

	assertTaskRunEventExists(t, db, runID, taskRunEventHumanPRComment, taskRunInteractionWarning)
	assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseAgentRunning, "", `["human_pr_comment"]`, 1, 0, 0, 0, 0)

	var forwarded int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user'`, "claw-human-pr-comment").Scan(&forwarded); err != nil {
		t.Fatalf("count forwarded messages: %v", err)
	}
	if forwarded != 0 {
		t.Fatalf("expected analytics-only comment path not to forward messages, got %d", forwarded)
	}
}

func TestTaskRunRequestedChangesInstrumentationRecordsWarning(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-requested-changes")
	runID, _ := startTaskRunForTest(t, s, "claw-requested-changes", "requested-changes")
	pr := clawPR{
		clawID:   "claw-requested-changes",
		repo:     "elastic/claw",
		prNumber: 77,
		prURL:    "https://github.com/elastic/claw/pull/77",
	}

	s.checkPRReviews(pr, []interface{}{
		map[string]interface{}{
			"id":    float64(201),
			"state": "APPROVED",
			"user":  map[string]interface{}{"login": "approver", "type": "User"},
		},
		map[string]interface{}{
			"id":       float64(202),
			"state":    "CHANGES_REQUESTED",
			"html_url": "https://github.com/elastic/claw/pull/77#pullrequestreview-202",
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		map[string]interface{}{
			"id":    float64(203),
			"state": "CHANGES_REQUESTED",
			"user":  map[string]interface{}{"login": "ci-bot[bot]", "type": "Bot"},
		},
	})

	assertTaskRunEventExists(t, db, runID, taskRunEventHumanRequestedChanges, taskRunInteractionWarning)
	assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseAgentRunning, "", `["human_requested_changes"]`, 1, 0, 0, 0, 0)

	var requestedChanges int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type=?`, runID, taskRunEventHumanRequestedChanges).Scan(&requestedChanges); err != nil {
		t.Fatalf("count requested changes events: %v", err)
	}
	if requestedChanges != 1 {
		t.Fatalf("expected bot requested-changes review to be ignored, got %d events", requestedChanges)
	}
}

func TestTaskRunRequestedChangesInstrumentationIgnoresHistoricalReviews(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-requested-changes-history")
	runID, _ := startTaskRunForTest(t, s, "claw-requested-changes-history", "requested-changes-history")
	pr := clawPR{
		clawID:       "claw-requested-changes-history",
		repo:         "elastic/claw",
		prNumber:     78,
		prURL:        "https://github.com/elastic/claw/pull/78",
		lastReviewID: 300,
	}

	s.checkPRReviews(pr, []interface{}{
		map[string]interface{}{
			"id":    float64(299),
			"state": "CHANGES_REQUESTED",
			"user":  map[string]interface{}{"login": "historical-reviewer", "type": "User"},
		},
		map[string]interface{}{
			"id":    float64(301),
			"state": "CHANGES_REQUESTED",
			"user":  map[string]interface{}{"login": "new-reviewer", "type": "User"},
		},
	})

	var requestedChanges int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type=?`, runID, taskRunEventHumanRequestedChanges).Scan(&requestedChanges); err != nil {
		t.Fatalf("count requested changes events: %v", err)
	}
	if requestedChanges != 1 {
		t.Fatalf("expected only new requested-changes review to be recorded, got %d events", requestedChanges)
	}
}

func TestTaskRunManualStopInstrumentationRecordsTerminalFailure(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-manual-stop")
	runID, _ := startTaskRunForTest(t, s, "claw-manual-stop", "manual-stop")

	s.recordTaskRunManualStopBeforeDelivery("claw-manual-stop", "operator")

	assertTaskRunEventExists(t, db, runID, taskRunEventManualStopBeforeDelivery, taskRunInteractionTerminal)
	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureManualStopDelivery, `["human_manual_stop_or_resume"]`, 1, 0, 0, 0, 0)
}

func TestTaskRunDoneWithoutPRInstrumentationPreservesRetryMessage(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-done-no-pr")
	runID, _ := startTaskRunForTest(t, s, "claw-done-no-pr", "done-no-pr")
	_, err := db.Exec(`UPDATE claws SET linear_issue_id=? WHERE id=?`, "ELA-999", "claw-done-no-pr")
	if err != nil {
		t.Fatalf("set issue id: %v", err)
	}

	s.handleClawDoneSignal("claw-done-no-pr", "[DONE]")

	assertTaskRunEventExists(t, db, runID, taskRunEventDoneWithoutPR, taskRunInteractionTerminal)
	assertTaskRunSummary(t, db, runID, taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureDoneWithoutPR, "[]", 0, 0, 0, 0, 0)

	var injected string
	if err := db.QueryRow(`SELECT content FROM messages WHERE claw_id=? AND role='user' ORDER BY created_at DESC LIMIT 1`, "claw-done-no-pr").Scan(&injected); err != nil {
		t.Fatalf("read injected retry message: %v", err)
	}
	if !strings.Contains(injected, "received with no PR URLs") {
		t.Fatalf("expected no-PR retry message, got %q", injected)
	}
}

func TestTaskRunDoneWithoutPRAllowsNonPRRuns(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "claw-done-non-pr")
	runID, _ := startTaskRunForTest(t, s, "claw-done-non-pr", "done-non-pr")
	if _, err := db.Exec(`UPDATE claws SET linear_issue_id=? WHERE id=?`, "ELA-998", "claw-done-non-pr"); err != nil {
		t.Fatalf("set issue id: %v", err)
	}
	if _, err := db.Exec(`UPDATE task_runs SET requires_pr=0, excluded_reason='non_pr_producing' WHERE id=?`, runID); err != nil {
		t.Fatalf("set non-pr contract: %v", err)
	}

	s.handleClawDoneSignal("claw-done-non-pr", "[DONE]")

	var failures int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type=?`, runID, taskRunEventDoneWithoutPR).Scan(&failures); err != nil {
		t.Fatalf("count done_without_pr events: %v", err)
	}
	if failures != 0 {
		t.Fatalf("expected no done_without_pr failure event for non-pr run, got %d", failures)
	}
	assertTaskRunEventExists(t, db, runID, taskRunEventTaskCompleted, taskRunInteractionTerminal)
	assertTaskRunSummary(t, db, runID, taskRunStatusCleanSuccess, taskRunPhaseTerminal, "", "[]", 0, 0, 0, 0, 0)

	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, "claw-done-non-pr").Scan(&status); err != nil {
		t.Fatalf("read claw status: %v", err)
	}
	if status != "deleted" {
		t.Fatalf("expected non-pr claw to be deleted after completion, got %q", status)
	}
	var injected int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user' AND content LIKE '%received with no PR URLs%'`, "claw-done-non-pr").Scan(&injected); err != nil {
		t.Fatalf("count injected retry messages: %v", err)
	}
	if injected != 0 {
		t.Fatalf("expected no PR retry message for non-pr run, got %d", injected)
	}
}

func insertValidRun(t *testing.T, db *sql.DB, runID string, ts int64) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO task_runs(
			id, tenant_id, initial_attempt_id, current_attempt_id, run_kind, owner_type, tags,
			analytics_enabled, requires_pr, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		runID, "tenant", "", "", "pr_task", "factory", `[]`, 1, 1, ts, ts,
	)
	if err != nil {
		t.Fatalf("insert valid run: %v", err)
	}
}

func insertValidAttempt(t *testing.T, db *sql.DB, attemptID, runID string, ts int64) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO task_run_attempts(
			id, tenant_id, run_id, attempt_id, attempt_number, status, failure_type, started_at, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		attemptID, "tenant", runID, attemptID, 1, "running", "", ts, ts, ts,
	)
	if err != nil {
		t.Fatalf("insert valid attempt: %v", err)
	}
}

func tableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	return name == table
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	return tableColumn(t, db, table, column).name == column
}

type tableColumnInfo struct {
	name         string
	defaultValue string
	pk           int
}

func tableColumn(t *testing.T, db *sql.DB, table, column string) tableColumnInfo {
	t.Helper()
	// PRAGMA table_info does not parameterize identifiers; callers pass test-controlled table constants.
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma table_info %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan column info %s: %v", table, err)
		}
		if name == column {
			return tableColumnInfo{name: name, defaultValue: defaultValue.String, pk: pk}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate column info %s: %v", table, err)
	}
	return tableColumnInfo{}
}

func assertPrimaryKey(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()
	info := tableColumn(t, db, table, column)
	if info.name != column || info.pk == 0 {
		t.Fatalf("expected %s.%s to be primary key", table, column)
	}
}

func assertColumns(t *testing.T, db *sql.DB, table string, columns []string) {
	t.Helper()
	for _, column := range columns {
		if !columnExists(t, db, table, column) {
			t.Fatalf("expected %s.%s to exist", table, column)
		}
	}
}

func assertColumnDefault(t *testing.T, db *sql.DB, table, column, want string) {
	t.Helper()
	if got := tableColumn(t, db, table, column).defaultValue; got != want {
		t.Fatalf("expected %s.%s default %s, got %s", table, column, want, got)
	}
}

func indexExists(t *testing.T, db *sql.DB, table, index string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA index_list(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma index_list %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin, partial string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index info %s: %v", table, err)
		}
		if strings.EqualFold(name, index) {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index info %s: %v", table, err)
	}
	return false
}

func assertExecFails(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err == nil {
		t.Fatalf("expected query to fail: %s", query)
	}
}

func assertExecSucceeds(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("expected query to succeed: %v\n%s", err, query)
	}
}
