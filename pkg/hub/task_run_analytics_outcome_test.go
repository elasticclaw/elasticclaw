package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
)

const analyticsOutcomeWorkspace = `schema_version: 2
name: analytics
repositories:
  primary: {provider: github, repository: org/repo}
`

func TestWorkflowV2AnalyticsLoaderOverrides(t *testing.T) {
	for _, tc := range []struct {
		analytics, kind     string
		enabled, requiresPR bool
	}{
		{"", "code_task", true, true},
		{"analytics: {}", "code_task", true, true},
		{"analytics: {run_kind: maintenance, requires_pr: false}", "maintenance", true, false},
		{"analytics: {enabled: false}", "code_task", false, true},
		{"analytics: {run_kind: code_task, enabled: false, requires_pr: false}", "code_task", false, false},
	} {
		t.Run(tc.analytics, func(t *testing.T) {
			doc := "schema_version: 2\nname: analytics\nenabled: true\ninitial_state: working\nstates:\n  working: {phase: build}\n" + tc.analytics
			workflow, err := loadExternalWorkflowDocument("analytics.yaml", []byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			enabled, requiresPR, _ := taskRunAnalyticsContractForWorkflow(workflow)
			if !*workflow.Enabled || taskRunKindForWorkflow(workflow) != tc.kind || enabled != tc.enabled || requiresPR != tc.requiresPR {
				t.Fatalf("unexpected analytics contract: %+v, enabled=%t requiresPR=%t", workflow, enabled, requiresPR)
			}
		})
	}
}

func TestWorkflowV2AnalyticsTerminalOutcomesSurviveMaterialization(t *testing.T) {
	for _, tc := range []struct{ state, outcome, status, warning string }{
		{"unchanged", "no_changes", taskRunStatusClean, ""},
		{"cancelled", "delivered", taskRunStatusClean, ""},
		{"done", "closed", taskRunStatusClean, taskRunWarningPRClosedUnmerged},
		{"failed", "failed", taskRunStatusFailed, ""},
		{"done", "cancelled", taskRunStatusCancelled, ""},
		{"failed", "", taskRunStatusRunning, ""},
	} {
		t.Run(tc.state+"/"+tc.outcome, func(t *testing.T) {
			s, db := newTaskRunAnalyticsTestServer(t, "outcome-claw")
			runID, _ := startTaskRunForTest(t, s, "outcome-claw", "outcome")
			doc := fmt.Sprintf(`schema_version: 2
name: outcomes
enabled: true
initial_state: working
states:
  working: {phase: build}
  %s: {phase: done, terminal: true, outcome: %q}
transitions:
  finish: {from: working, on: workflow.finished, to: %s}
`, tc.state, tc.outcome, tc.state)
			store := workflowv2.NewStore(db)
			run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
				TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace), WorkflowYAML: []byte(doc),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.materializeTaskRun(runID); err != nil {
				t.Fatal(err)
			}
			var status string
			if err := db.QueryRow(`SELECT status FROM task_run_summaries WHERE run_id=?`, runID).Scan(&status); err != nil || status != taskRunStatusRunning {
				t.Fatalf("active run status=%s, err=%v", status, err)
			}
			// Keep the pre-finish summary strictly older than the v2 finish time.
			assertExecSucceeds(t, db, `UPDATE task_run_summaries SET materialized_at=materialized_at-1 WHERE run_id=?`, runID)
			if _, err := store.ApplyEvent(context.Background(), run.ID, workflowv2.EventInput{ID: "finish", MessageID: "finish", Kind: "workflow.finished", Producer: workflowv2.ProducerHub}); err != nil {
				t.Fatal(err)
			}
			if tc.outcome != "" && tc.outcome != "no_changes" {
				associatePRForTest(t, s, runID, "org/repo", 1, "open")
			}
			// A deleted claw must not prevent terminal classification or replay.
			assertExecSucceeds(t, db, `UPDATE claws SET status='deleted' WHERE id='outcome-claw'`)
			if tc.outcome != "" {
				s.closeOrphanedTaskRunSummaries()
				if err := db.QueryRow(`SELECT status FROM task_run_summaries WHERE run_id=?`, runID).Scan(&status); err != nil || status != tc.status {
					t.Fatalf("recovered status=%s err=%v", status, err)
				}
			}
			s.maybeFinishWorkflowV2Parent(context.Background(), run.ID)
			for i := 0; i < 2; i++ {
				if err := s.materializeTaskRun(runID); err != nil {
					t.Fatal(err)
				}
				if tc.outcome != "" {
					s.closeOrphanedTaskRunSummaries()
				}
				var outcome, phase, warnings string
				var finishedAt int64
				if err := db.QueryRow(`SELECT status, phase, outcome, warning_types, finished_at FROM task_run_summaries WHERE run_id=?`, runID).Scan(&status, &phase, &outcome, &warnings, &finishedAt); err != nil {
					t.Fatal(err)
				}
				if status != tc.status || outcome != tc.outcome {
					t.Fatalf("status/outcome=%s/%s, want %s/%s", status, outcome, tc.status, tc.outcome)
				}
				if tc.outcome != "" && (phase != taskRunPhaseTerminal || finishedAt == 0) {
					t.Fatalf("terminal phase/time: %s/%d", phase, finishedAt)
				}
				if tc.warning != "" && !strings.Contains(warnings, tc.warning) {
					t.Fatalf("missing warning in %s", warnings)
				}
			}
		})
	}
}

func TestWorkflowV2AnalyticsNonPRCompletionAndOrphans(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprint(completed), func(t *testing.T) {
			s, db := newTaskRunAnalyticsTestServer(t, "non-pr-claw")
			runID, attemptID := startTaskRunWithRequiresPRForTest(t, s, "non-pr-claw", "non-pr", false)
			recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: runID, AttemptID: attemptID, EventKey: "no-pr", EventType: taskRunEventDoneWithoutPR, FailureType: taskRunFailureDoneWithoutPR, Source: taskRunSourceHub, ActorType: taskRunActorSystem})
			if completed {
				recordTaskRunEventForTest(t, s, TaskRunEvent{RunID: runID, AttemptID: attemptID, EventKey: "completed", EventType: taskRunEventTaskCompleted, Source: taskRunSourceHub, ActorType: taskRunActorSystem})
			}
			assertExecSucceeds(t, db, `UPDATE claws SET status='deleted' WHERE id='non-pr-claw'`)
			for i := 0; i < 2; i++ {
				if err := s.materializeTaskRun(runID); err != nil {
					t.Fatal(err)
				}
				s.closeOrphanedTaskRunSummaries()
				var status string
				if err := db.QueryRow(`SELECT status FROM task_run_summaries WHERE run_id=?`, runID).Scan(&status); err != nil {
					t.Fatal(err)
				}
				// A dead claw without a completion is an orphan, PR or not.
				want := taskRunStatusFailed
				if completed {
					want = taskRunStatusClean
				}
				if status != want {
					t.Fatalf("status=%s, want %s", status, want)
				}
			}
		})
	}
}

func TestWorkflowV2AnalyticsNonPRTerminalRecoversWithoutCompletionEvent(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "non-pr-terminal")
	runID, _ := startTaskRunWithRequiresPRForTest(t, s, "non-pr-terminal", "terminal", false)
	_, err := workflowv2.NewStore(db).CreateRun(context.Background(), workflowv2.CreateRunRequest{
		TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace),
		WorkflowYAML: []byte("schema_version: 2\nname: terminal\nenabled: true\ninitial_state: done\nstates:\n  done: {phase: done, terminal: true}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertExecSucceeds(t, db, `UPDATE claws SET status='deleted' WHERE id='non-pr-terminal'`)
	s.closeOrphanedTaskRunSummaries()
	var status string
	if err := db.QueryRow(`SELECT status FROM task_run_summaries WHERE run_id=?`, runID).Scan(&status); err != nil || status != taskRunStatusClean {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestWorkflowV2AnalyticsMaintenanceFiltersAndTicketExclusion(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	for _, id := range []string{"maintenance", "code"} {
		kind, requiresPR := "code_task", true
		if id == "maintenance" {
			kind, requiresPR = "maintenance", false
		}
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: id, AttemptID: id + "-attempt", ClawID: id + "-claw", TenantID: "test-tenant-id", Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow, StartedAt: 1760000000000, FinishedAt: 1760000001000, RequiresPR: boolPtr(requiresPR)})
		assertExecSucceeds(t, db, `UPDATE task_run_summaries SET run_kind=?, outcome='no_changes', issue_id=?, estimated_cost_usd=10 WHERE run_id=?`, kind, id, id)
	}
	filters := taskRunAnalyticsFilters{TenantID: "test-tenant-id"}
	for _, aggregates := range []bool{true, false} {
		effect, err := s.readTaskRunAnalyticsEffectivenessWithTicketAggregates(filters, aggregates)
		if err != nil {
			t.Fatal(err)
		}
		if effect.SuccessRate != 1 || effect.UniqueTickets != 1 || effect.TicketSuccessRate != 1 || effect.TicketCostUSD != 10 {
			t.Fatalf("effectiveness: %+v", effect)
		}
	}
	for _, alias := range []string{"run_kind", "runKind"} {
		r := httptest.NewRequest(http.MethodGet, "/?"+alias+"=maintenance", nil)
		parsed, err := parseTaskRunAnalyticsFilters(r)
		if err != nil {
			t.Fatal(err)
		}
		parsed.TenantID = filters.TenantID
		if !taskRunAnalyticsCostsUseRunFilters(parsed) {
			t.Fatal("costs ignore run_kind")
		}
		where, args := taskRunAnalyticsSummaryWhere(parsed)
		rows, err := db.Query(`SELECT `+taskRunAnalyticsRunColumns()+` FROM task_run_summaries `+where, args...)
		if err != nil {
			t.Fatal(err)
		}
		runs, err := scanTaskRunAnalyticsRuns(rows)
		rows.Close()
		if err != nil || len(runs) != 1 || runs[0].RunID != "maintenance" || runs[0].Outcome != "no_changes" {
			t.Fatalf("runs=%+v, err=%v", runs, err)
		}
		groups, total, _, _, err := s.readTaskRunAnalyticsTicketGroupsPage(parsed, 0, "", 10)
		if err != nil || total != 0 || len(groups) != 0 {
			t.Fatalf("maintenance tickets: groups=%+v total=%d err=%v", groups, total, err)
		}
	}
}

func TestWorkflowV2AnalyticsTicketOutcomesOverridePRs(t *testing.T) {
	for _, tc := range []struct{ outcome, status, prState, want string }{
		{"no_changes", taskRunStatusClean, "", "delivered"},
		{"delivered", taskRunStatusClean, "open", "delivered"},
		{"closed", taskRunStatusClean, "open", "delivered"},
		{"failed", taskRunStatusFailed, "closed", "failed"},
		{"failed", taskRunStatusFailed, "open", "failed"},
		{"cancelled", taskRunStatusCancelled, "closed", "failed"},
		{"", taskRunStatusFailed, "closed", "delivered"},
	} {
		t.Run(tc.outcome+"/"+tc.prState, func(t *testing.T) {
			runs := []taskRunAnalyticsTicketRunSummary{{RunID: "run", Outcome: tc.outcome, Status: tc.status}}
			prs := []taskRunAnalyticsTicketPRView{{RunID: "run", taskRunAnalyticsPRView: taskRunAnalyticsPRView{State: tc.prState}}}
			if got := deriveTaskRunAnalyticsTicketStatus(runs, prs); got != tc.want {
				t.Fatalf("status=%s, want %s", got, tc.want)
			}
		})
	}
	runs := []taskRunAnalyticsTicketRunSummary{{RunID: "failed", Outcome: "failed", Status: taskRunStatusFailed}, {RunID: "legacy", Status: taskRunStatusClean}}
	prs := []taskRunAnalyticsTicketPRView{{RunID: "legacy", taskRunAnalyticsPRView: taskRunAnalyticsPRView{State: "closed"}}}
	if got := deriveTaskRunAnalyticsTicketStatus(runs, prs); got != "delivered" {
		t.Fatalf("independent successful run lost: %s", got)
	}
}

func TestWorkflowV2AnalyticsTerminalRecoveryIsOneShot(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "one-shot-claw")
	runID, _ := startTaskRunForTest(t, s, "one-shot-claw", "one-shot")
	_, err := workflowv2.NewStore(db).CreateRun(context.Background(), workflowv2.CreateRunRequest{
		TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace),
		WorkflowYAML: []byte("schema_version: 2\nname: terminal\nenabled: true\ninitial_state: done\nstates:\n  done: {phase: done, terminal: true}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A PR-producing run that ended without a PR or outcome stays running.
	assertExecSucceeds(t, db, `UPDATE task_run_summaries SET materialized_at=0 WHERE run_id=?`, runID)
	for i, want := range []int{1, 0} {
		got, err := s.materializeTerminalWorkflowV2TaskRuns()
		if err != nil || got != want {
			t.Fatalf("call %d selected %d runs, want %d (err=%v)", i, got, want, err)
		}
	}
}

func TestWorkflowV2AnalyticsMaintenanceRunFromLoadedWorkflow(t *testing.T) {
	doc := "schema_version: 2\nname: deps\nenabled: true\ninitial_state: working\nstates:\n  working: {phase: build}\nanalytics: {run_kind: maintenance, requires_pr: false}\n"
	workflow, err := loadExternalWorkflowDocument("deps.yaml", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	s, db := newTaskRunAnalyticsTestServer(t, "maintenance-claw")
	enabled, requiresPR, excludedReason := taskRunAnalyticsContractForWorkflow(workflow)
	if !enabled || requiresPR || excludedReason != "" {
		t.Fatalf("contract enabled=%t requiresPR=%t excludedReason=%q, want true/false/\"\"", enabled, requiresPR, excludedReason)
	}
	runID, _, err := s.ensureTaskRunForClaw("maintenance-claw", TaskRunStart{
		TenantID: "test-tenant-id", RunKind: taskRunKindForWorkflow(workflow), OwnerType: taskRunOwnerWorkflow,
		OwnerName: "eng/deps", WorkspaceName: "eng", WorkflowName: workflow.Name, Source: taskRunSourceWorkflow,
		AnalyticsEnabled: enabled, RequiresPR: requiresPR, ExcludedReason: excludedReason, EventKey: "deps:task-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	var kind, summaryKind, status, failureType string
	var taskRequiresPR, summaryRequiresPR int
	if err := db.QueryRow(`SELECT run_kind, requires_pr FROM task_runs WHERE id=?`, runID).Scan(&kind, &taskRequiresPR); err != nil {
		t.Fatal(err)
	}
	if err := s.materializeTaskRun(runID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT run_kind, requires_pr, status FROM task_run_summaries WHERE run_id=?`, runID).Scan(&summaryKind, &summaryRequiresPR, &status); err != nil {
		t.Fatal(err)
	}
	if kind != taskRunKindMaintenance || summaryKind != taskRunKindMaintenance || taskRequiresPR != 0 || summaryRequiresPR != 0 || status != taskRunStatusRunning {
		t.Fatalf("task_run=%s/%d summary=%s/%d status=%s", kind, taskRequiresPR, summaryKind, summaryRequiresPR, status)
	}
	// Reaped mid-run with no outcome: the orphan sweep closes it.
	assertExecSucceeds(t, db, `UPDATE claws SET status='deleted' WHERE id='maintenance-claw'`)
	s.closeOrphanedTaskRunSummaries()
	if err := db.QueryRow(`SELECT status, failure_type FROM task_run_summaries WHERE run_id=?`, runID).Scan(&status, &failureType); err != nil {
		t.Fatal(err)
	}
	if status != taskRunStatusFailed || failureType != taskRunFailureAgentStopped {
		t.Fatalf("reaped maintenance run status=%s failure=%s", status, failureType)
	}
}

func TestWorkflowV2AnalyticsDisabledOutcomeKeepsExclusionSentinel(t *testing.T) {
	meta := taskRunMeta{analyticsEnabled: 0, requiresPR: 1, outcome: "delivered", outcomeAt: 10, updatedAt: 5}
	status, phase, failureType, _, _ := computeTaskRunStatus(meta, prCountSummary{}, 0, 0, taskRunPhaseTimeSummary{}, "", 0, 0, "", 0)
	if status != taskRunStatusFailed || phase != taskRunPhaseTerminal || failureType != taskRunFailureUnknown {
		t.Fatalf("disabled run status=%s phase=%s failure=%s", status, phase, failureType)
	}
}

func TestWorkflowV2FailedOutcomeKeepsDefinitiveFailureType(t *testing.T) {
	meta := taskRunMeta{analyticsEnabled: 1, requiresPR: 1, outcome: "failed", outcomeAt: 10}
	for _, tc := range []struct{ definitive, want string }{
		{"", taskRunFailureUnknown},
		{taskRunFailureManualStopDelivery, taskRunFailureManualStopDelivery},
	} {
		status, phase, failureType, _, finishedAt := computeTaskRunStatus(meta, prCountSummary{}, 0, 0, taskRunPhaseTimeSummary{}, tc.definitive, 5, 0, "", 0)
		if status != taskRunStatusFailed || phase != taskRunPhaseTerminal || failureType != tc.want || finishedAt != 10 {
			t.Fatalf("definitive=%q: status=%s phase=%s failure=%s finishedAt=%d", tc.definitive, status, phase, failureType, finishedAt)
		}
	}
}

func createTerminalOutcomeRunForTest(t *testing.T, s *Server, clawID, outcome string) (string, string) {
	t.Helper()
	runID, _ := startTaskRunWithRequiresPRForTest(t, s, clawID, clawID, false)
	doc := fmt.Sprintf("schema_version: 2\nname: terminal\nenabled: true\ninitial_state: done\nstates:\n  done: {phase: done, terminal: true, outcome: %q}\n", outcome)
	run, err := workflowv2.NewStore(s.db).CreateRun(context.Background(), workflowv2.CreateRunRequest{
		TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace), WorkflowYAML: []byte(doc),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The summary predates the v2 finish, as if the hub restarted in between.
	assertExecSucceeds(t, s.db, `UPDATE task_run_summaries SET materialized_at=0 WHERE run_id=?`, runID)
	return runID, run.ID
}

func taskRunSummaryStatusOutcomeForTest(t *testing.T, s *Server, runID string) (string, string) {
	t.Helper()
	var status, outcome string
	if err := s.db.QueryRow(`SELECT status, outcome FROM task_run_summaries WHERE run_id=?`, runID).Scan(&status, &outcome); err != nil {
		t.Fatal(err)
	}
	return status, outcome
}

func TestWorkflowV2TerminalOutcomeMaterializeFailureStillTearsDownAndRecovers(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "fail-claw")
	runID, v2RunID := createTerminalOutcomeRunForTest(t, s, "fail-claw", "no_changes")
	assertExecSucceeds(t, db, `CREATE TRIGGER fail_outcome BEFORE UPDATE OF outcome ON task_run_summaries
  WHEN NEW.outcome<>'' BEGIN SELECT RAISE(ABORT, 'materialize failed'); END`)

	s.maybeFinishWorkflowV2Parent(context.Background(), v2RunID)
	var clawStatus string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='fail-claw'`).Scan(&clawStatus); err != nil || clawStatus != "deleted" {
		t.Fatalf("claw status=%s err=%v; teardown must not depend on materialization", clawStatus, err)
	}

	// Recovery keeps failing: the orphan sweep must not fail the pending outcome.
	s.closeOrphanedTaskRunSummaries()
	if status, outcome := taskRunSummaryStatusOutcomeForTest(t, s, runID); status != taskRunStatusRunning || outcome != "" {
		t.Fatalf("pending outcome was closed: status=%s outcome=%s", status, outcome)
	}

	assertExecSucceeds(t, db, `DROP TRIGGER fail_outcome`)
	s.closeOrphanedTaskRunSummaries()
	if status, outcome := taskRunSummaryStatusOutcomeForTest(t, s, runID); status != taskRunStatusClean || outcome != "no_changes" {
		t.Fatalf("recovered status=%s outcome=%s", status, outcome)
	}
}

func TestWorkflowV2TerminalRecoveryCoversClosedSummaries(t *testing.T) {
	t.Run("outcome", func(t *testing.T) {
		s, db := newTaskRunAnalyticsTestServer(t, "closed-claw")
		runID, _ := createTerminalOutcomeRunForTest(t, s, "closed-claw", "no_changes")
		assertExecSucceeds(t, db, `UPDATE task_run_summaries SET status=?, phase=?, failure_type=? WHERE run_id=?`,
			taskRunStatusFailed, taskRunPhaseTerminal, taskRunFailureAgentStopped, runID)
		if got, err := s.materializeTerminalWorkflowV2TaskRuns(); err != nil || got != 1 {
			t.Fatalf("selected %d err=%v", got, err)
		}
		if status, outcome := taskRunSummaryStatusOutcomeForTest(t, s, runID); status != taskRunStatusClean || outcome != "no_changes" {
			t.Fatalf("status=%s outcome=%s", status, outcome)
		}
	})
	t.Run("no outcome", func(t *testing.T) {
		s, db := newTaskRunAnalyticsTestServer(t, "legacy-claw")
		runID, _ := createTerminalOutcomeRunForTest(t, s, "legacy-claw", "")
		assertExecSucceeds(t, db, `UPDATE task_run_summaries SET status=?, phase=? WHERE run_id=?`, taskRunStatusClean, taskRunPhaseTerminal, runID)
		for i, want := range []int{1, 0} {
			if got, err := s.materializeTerminalWorkflowV2TaskRuns(); err != nil || got != want {
				t.Fatalf("call %d selected %d, want %d (err=%v)", i, got, want, err)
			}
		}
		// The terminal path closed it; recovery must not reopen it as running.
		if status, _ := taskRunSummaryStatusOutcomeForTest(t, s, runID); status != taskRunStatusClean {
			t.Fatalf("status=%s", status)
		}
	})
}

func TestTaskRunAnalyticsDashboardDefaultScopeExcludesNonPRCodeTasks(t *testing.T) {
	_, db := newTaskRunAnalyticsAPITestServer(t)
	for _, fixture := range []struct {
		id, kind   string
		requiresPR bool
	}{{"pr", "code_task", true}, {"maintenance", "maintenance", false}, {"non-pr", "code_task", false}} {
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: fixture.id, AttemptID: fixture.id + "-attempt", ClawID: fixture.id + "-claw", TenantID: "test-tenant-id", Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow, StartedAt: 1760000000000, RequiresPR: boolPtr(fixture.requiresPR)})
		assertExecSucceeds(t, db, `UPDATE task_run_summaries SET run_kind=? WHERE run_id=?`, fixture.kind, fixture.id)
	}
	for _, tc := range []struct {
		name    string
		filters taskRunAnalyticsFilters
		want    string
	}{
		{"default", taskRunAnalyticsFilters{}, "maintenance,pr"},
		{"dashboard analytics_enabled=true", taskRunAnalyticsFilters{AnalyticsEnabled: boolPtr(true)}, "maintenance,pr"},
		{"analytics_enabled=false", taskRunAnalyticsFilters{AnalyticsEnabled: boolPtr(false)}, ""},
		{"requires_pr=false", taskRunAnalyticsFilters{RequiresPR: boolPtr(false)}, "maintenance,non-pr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.filters.TenantID = "test-tenant-id"
			where, args := taskRunAnalyticsSummaryWhere(tc.filters)
			rows, err := db.Query(`SELECT run_id FROM task_run_summaries `+where+` ORDER BY run_id`, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			if got := strings.Join(ids, ","); got != tc.want {
				t.Fatalf("runs=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkflowV2TerminalNoOutcomeFailureUsesValidFailureType(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "prepare-claw")
	runID, _ := startTaskRunForTest(t, s, "prepare-claw", "prepare")
	doc := "schema_version: 2\nname: prepare\nenabled: true\ninitial_state: prepare_failed\nstates:\n  prepare_failed: {phase: done, terminal: true}\n"
	run, err := workflowv2.NewStore(db).CreateRun(context.Background(), workflowv2.CreateRunRequest{
		TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace), WorkflowYAML: []byte(doc),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Record every terminal write: later re-materialization rewrites the row.
	assertExecSucceeds(t, db, `CREATE TABLE eager_writes(status TEXT, failure_type TEXT)`)
	assertExecSucceeds(t, db, `CREATE TRIGGER record_eager_write AFTER UPDATE OF failure_type ON task_run_summaries
  WHEN NEW.phase='terminal' BEGIN INSERT INTO eager_writes VALUES(NEW.status, NEW.failure_type); END`)

	s.maybeFinishWorkflowV2Parent(context.Background(), run.ID)

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM eager_writes WHERE status=? AND failure_type=?`,
		taskRunStatusFailed, taskRunFailureUnknown).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("eager terminal update for a no-outcome failure state was rejected or skipped")
	}
}

func TestWorkflowV2TerminalUnparseablePinnedWorkflowStillTearsDown(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "broken-claw")
	runID, v2RunID := createTerminalOutcomeRunForTest(t, s, "broken-claw", "no_changes")
	assertExecSucceeds(t, db, `UPDATE workflow_v2_runs SET workflow_yaml='states: [' WHERE id=?`, v2RunID)

	if terminal, err := readWorkflowV2TaskRunTerminal(db, runID); err != nil || terminal.outcome != "" {
		t.Fatalf("outcome=%q err=%v; an unparseable pinned workflow must read as no outcome", terminal.outcome, err)
	}
	s.maybeFinishWorkflowV2Parent(context.Background(), v2RunID)
	var clawStatus string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id='broken-claw'`).Scan(&clawStatus); err != nil || clawStatus != "deleted" {
		t.Fatalf("claw status=%s err=%v", clawStatus, err)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_key=?`,
		runID, "workflow_v2:"+v2RunID+":completed").Scan(&events); err != nil || events != 1 {
		t.Fatalf("terminal event count=%d err=%v", events, err)
	}
}

func TestWorkflowV2TimedOutNonPRRunWithoutOutcomeStaysFailed(t *testing.T) {
	for _, openPR := range []bool{false, true} {
		t.Run(fmt.Sprintf("open_pr=%t", openPR), func(t *testing.T) {
			s, db := newTaskRunAnalyticsTestServer(t, "timeout-claw")
			runID, _ := startTaskRunWithRequiresPRForTest(t, s, "timeout-claw", "timeout", false)
			store := workflowv2.NewStore(db)
			run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
				TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace),
				WorkflowYAML: []byte("schema_version: 2\nname: timeout\nenabled: true\ninitial_state: working\nstates:\n  working: {phase: build}\n"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if openPR {
				associatePRForTest(t, s, runID, "org/repo", 1, "open")
			}
			// The reaper's timeout path: cancel in a non-terminal state, then finish the parent.
			assertExecSucceeds(t, db, `UPDATE workflow_v2_runs SET timeout_at=1 WHERE id=?`, run.ID)
			s.reapWorkflowV2RunTimeouts(time.Now(), func() bool { return true })
			for i := 0; i < 2; i++ {
				if err := s.materializeTaskRun(runID); err != nil {
					t.Fatal(err)
				}
				// Re-materialization alone must keep the cancellation, before any sweep.
				if status, _ := taskRunSummaryStatusOutcomeForTest(t, s, runID); status != taskRunStatusFailed {
					t.Fatalf("pass %d after materialize: status=%s, want %s", i, status, taskRunStatusFailed)
				}
				s.closeOrphanedTaskRunSummaries()
				if status, _ := taskRunSummaryStatusOutcomeForTest(t, s, runID); status != taskRunStatusFailed {
					t.Fatalf("pass %d after sweep: status=%s, want %s", i, status, taskRunStatusFailed)
				}
			}
		})
	}
}

func TestWorkflowV2CancelledNonPRRunKeepsDefinitiveFailureType(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "stop-claw")
	runID, _ := startTaskRunWithRequiresPRForTest(t, s, "stop-claw", "stop", false)
	run, err := workflowv2.NewStore(db).CreateRun(context.Background(), workflowv2.CreateRunRequest{
		TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace),
		WorkflowYAML: []byte("schema_version: 2\nname: stop\nenabled: true\ninitial_state: working\nstates:\n  working: {phase: build}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.recordTaskRunManualStopBeforeDelivery("stop-claw", "alice")
	assertExecSucceeds(t, db, `UPDATE workflow_v2_runs SET timeout_at=1 WHERE id=?`, run.ID)
	s.reapWorkflowV2RunTimeouts(time.Now(), func() bool { return true })
	if err := s.materializeTaskRun(runID); err != nil {
		t.Fatal(err)
	}
	var status, failureType string
	if err := db.QueryRow(`SELECT status, failure_type FROM task_run_summaries WHERE run_id=?`, runID).Scan(&status, &failureType); err != nil {
		t.Fatal(err)
	}
	if status != taskRunStatusFailed || failureType != taskRunFailureManualStopDelivery {
		t.Fatalf("status=%s failure_type=%s, want %s/%s", status, failureType, taskRunStatusFailed, taskRunFailureManualStopDelivery)
	}
}

func TestWorkflowV2NonPRCancelledTerminalStateWithoutOutcomeStaysFailed(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "cancel-claw")
	runID, _ := startTaskRunWithRequiresPRForTest(t, s, "cancel-claw", "cancel", false)
	run, err := workflowv2.NewStore(db).CreateRun(context.Background(), workflowv2.CreateRunRequest{
		TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace),
		WorkflowYAML: []byte("schema_version: 2\nname: cancel\nenabled: true\ninitial_state: cancelled\nstates:\n  cancelled: {phase: done, terminal: true}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.maybeFinishWorkflowV2Parent(context.Background(), run.ID)
	for i := 0; i < 2; i++ {
		if err := s.materializeTaskRun(runID); err != nil {
			t.Fatal(err)
		}
		if status, _ := taskRunSummaryStatusOutcomeForTest(t, s, runID); status != taskRunStatusFailed {
			t.Fatalf("pass %d: status=%s, want %s (eager and materialized results must agree)", i, status, taskRunStatusFailed)
		}
	}
}

func TestWorkflowV2TerminalRecoveryIgnoresNewerStaleMaterialization(t *testing.T) {
	s, db := newTaskRunAnalyticsTestServer(t, "race-claw")
	runID, _ := startTaskRunWithRequiresPRForTest(t, s, "race-claw", "race", false)
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		TenantID: "test-tenant-id", TaskRunID: runID, WorkspaceYAML: []byte(analyticsOutcomeWorkspace),
		WorkflowYAML: []byte(`schema_version: 2
name: race
enabled: true
initial_state: working
states:
  working: {phase: build}
  unchanged: {phase: done, terminal: true, outcome: no_changes}
transitions:
  finish: {from: working, on: workflow.finished, to: unchanged}
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	// A materialization that saw the run active but stamped a time later than
	// the v2 finish (ApplyEvent takes its clock before its transaction).
	if err := s.materializeTaskRun(runID); err != nil {
		t.Fatal(err)
	}
	assertExecSucceeds(t, db, `UPDATE task_run_summaries SET materialized_at=? WHERE run_id=?`, time.Now().Add(time.Hour).UnixMilli(), runID)
	if _, err := store.ApplyEvent(context.Background(), run.ID, workflowv2.EventInput{ID: "finish", MessageID: "finish", Kind: "workflow.finished", Producer: workflowv2.ProducerHub}); err != nil {
		t.Fatal(err)
	}
	// The hub restarted before the parent finished, and the claw was reaped.
	assertExecSucceeds(t, db, `UPDATE claws SET status='deleted' WHERE id='race-claw'`)
	s.closeOrphanedTaskRunSummaries()
	if status, outcome := taskRunSummaryStatusOutcomeForTest(t, s, runID); status != taskRunStatusClean || outcome != "no_changes" {
		t.Fatalf("status=%s outcome=%s, want %s/no_changes", status, outcome, taskRunStatusClean)
	}
}
