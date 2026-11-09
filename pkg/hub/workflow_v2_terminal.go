package hub

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// readWorkflowV2TaskRunOutcome uses the pinned document, so edits to the workflow
// cannot change the classification when analytics is rebuilt later.
func readWorkflowV2TaskRunOutcome(db interface{ QueryRow(string, ...any) *sql.Row }, taskRunID string) (typesv2.TerminalOutcome, int64, error) {
	var state, workflowYAML string
	var finishedAt int64
	err := db.QueryRow(`SELECT state, workflow_yaml, finished_at FROM workflow_v2_runs
  WHERE task_run_id=? AND status IN ('completed','cancelled') ORDER BY created_at DESC, id DESC LIMIT 1`, taskRunID).Scan(&state, &workflowYAML, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	workflow, err := typesv2.ParseWorkflow([]byte(workflowYAML))
	if err != nil {
		// A pinned document the current parser rejects must not block event
		// recording or claw teardown; classify the run as having no outcome.
		log.Printf("[workflow-v2] cannot parse pinned workflow for task run %s; ignoring outcome: %v", taskRunID, err)
		return "", 0, nil
	}
	definition := workflow.States[state]
	if !definition.Terminal {
		return "", 0, nil
	}
	return definition.Outcome, finishedAt, nil
}

// materializeTerminalWorkflowV2TaskRuns is one-shot per run: a summary
// materialized after its v2 run finished has already seen the outcome, so it
// is not selected again on later reaper ticks.
func (s *Server) materializeTerminalWorkflowV2TaskRuns() (int, error) {
	// <= catches a summary materialized in the same millisecond the run
	// finished; recovery then moves materialized_at past finished_at so each
	// terminal run is recovered once. Any status qualifies: a summary closed
	// before its outcome was materialized must still pick the outcome up.
	rows, err := s.db.Query(`SELECT s.run_id, s.status, MAX(r.finished_at) FROM task_run_summaries s
  JOIN workflow_v2_runs r ON r.task_run_id=s.run_id
  WHERE s.outcome='' AND s.materialized_at <= r.finished_at
    AND r.status IN ('completed','cancelled')
  GROUP BY s.run_id`)
	if err != nil {
		return 0, err
	}
	type terminalRun struct {
		runID      string
		status     string
		finishedAt int64
	}
	var runs []terminalRun
	for rows.Next() {
		var run terminalRun
		if err := rows.Scan(&run.runID, &run.status, &run.finishedAt); err != nil {
			rows.Close()
			return 0, err
		}
		runs = append(runs, run)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, run := range runs {
		if run.status != taskRunStatusRunning {
			// A closed summary without an outcome was finished by the terminal
			// path itself; re-materializing it could reopen it as running.
			outcome, _, err := readWorkflowV2TaskRunOutcome(s.db, run.runID)
			if err != nil {
				log.Printf("[task-run-analytics] read terminal workflow outcome %s: %v", run.runID, err)
				continue
			}
			if outcome == "" {
				s.markTerminalWorkflowV2TaskRunRecovered(run.runID, run.finishedAt)
				continue
			}
		}
		if err := s.materializeTaskRun(run.runID); err != nil {
			log.Printf("[task-run-analytics] recover terminal workflow summary %s: %v", run.runID, err)
			continue
		}
		s.markTerminalWorkflowV2TaskRunRecovered(run.runID, run.finishedAt)
	}
	return len(runs), nil
}

func (s *Server) markTerminalWorkflowV2TaskRunRecovered(runID string, finishedAt int64) {
	if _, err := s.db.Exec(`UPDATE task_run_summaries SET materialized_at=? WHERE run_id=? AND materialized_at<=?`,
		finishedAt+1, runID, finishedAt); err != nil {
		log.Printf("[task-run-analytics] mark terminal workflow summary %s recovered: %v", runID, err)
	}
}

// maybeFinishWorkflowV2Parent checks whether a v2 workflow run has reached a
// terminal state and, if so, finishes the parent v1 task run and disconnects
// the claw. This is the bridge between the v2 deterministic state machine and
// the v1 claw/task-run lifecycle that still owns the agent VM.
func (s *Server) maybeFinishWorkflowV2Parent(ctx context.Context, runID string) {
	if s == nil || s.db == nil {
		return
	}
	store := workflowv2.NewStore(s.db)
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		log.Printf("[workflow-v2] cannot load run %s for parent cleanup: %v", runID, err)
		return
	}
	if run.Status != workflowv2.RunCompleted && run.Status != workflowv2.RunCancelled {
		return
	}
	if s.cronSchedulerV2 != nil && run.TriggerType == "cron" {
		s.cronSchedulerV2.finishRunByV2RunID(run.ID, run.Status)
	}
	s.releaseWorkflowV2CronSlot(ctx, run.ID)
	if strings.TrimSpace(run.TaskRunID) == "" {
		// No parent task run; nothing to finish.
		return
	}
	outcome, _, err := readWorkflowV2TaskRunOutcome(s.db, run.TaskRunID)
	if err != nil {
		// Still tear the claw down; analytics recovers the outcome on a later sweep.
		log.Printf("[workflow-v2] cannot read outcome for run %s: %v", runID, err)
		outcome = ""
	}
	if outcome != "" {
		if err := s.materializeTaskRun(run.TaskRunID); err != nil {
			// Still tear the claw down: materializeTerminalWorkflowV2TaskRuns
			// recovers the summary, and the orphan sweep leaves it alone until then.
			log.Printf("[workflow-v2] cannot materialize outcome for run %s: %v", runID, err)
		}
	}
	var clawID string
	if err := s.db.QueryRowContext(ctx, `SELECT claw_id FROM task_runs WHERE id=?`, run.TaskRunID).Scan(&clawID); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[workflow-v2] cannot resolve claw for task run %s: %v", run.TaskRunID, err)
		}
		return
	}
	if strings.TrimSpace(clawID) == "" {
		return
	}
	// Idempotent: finishClawTerminalTx is a no-op if the claw is already deleted.
	result := "success"
	if run.Status == workflowv2.RunCancelled {
		result = "cancelled"
	} else if outcome == typesv2.OutcomeFailed {
		result = "failed"
	}
	applied, err := s.finishClawTerminalTx(clawID, "deleted", "", result, "workflow v2 terminal state "+string(run.Status), terminalTxOpts{})
	if err != nil {
		log.Printf("[workflow-v2] failed to finish claw %s for terminal run %s: %v", clawID, runID, err)
		return
	}
	if !applied {
		// Already finished; avoid duplicate events.
		return
	}
	// A run cancelled before reaching a terminal outcome (e.g. reaper timeout)
	// did not complete its task.
	cancelledWithoutOutcome := outcome == "" && run.Status == workflowv2.RunCancelled
	// Eagerly update the task-run summary so the dashboard reflects the
	// terminal state without waiting for the analytics materializer.
	if outcome == "" {
		status, failureType := taskRunStatusClean, ""
		if cancelledWithoutOutcome || run.State == "prepare_failed" {
			// failure_type is CHECK-constrained; a state name such as
			// "cancelled" would make this UPDATE fail.
			status, failureType = taskRunStatusFailed, taskRunFailureUnknown
		}
		finishedAt := time.Now().UTC().UnixMilli()
		if _, err := s.db.ExecContext(ctx, `
		UPDATE task_run_summaries
		SET status=?, phase=?, failure_type=?, finished_at=?, updated_at=?
		WHERE run_id=?`, status, taskRunPhaseTerminal, failureType, finishedAt, finishedAt, run.TaskRunID); err != nil {
			log.Printf("[workflow-v2] failed to update task run summary %s for terminal run %s: %v", run.TaskRunID, runID, err)
		}
	}
	s.syncWorkflowVolumes(clawID)
	// The writable lease kept this claw's slot until the sync released it.
	go s.promotePendingClaws()
	if s.cronScheduler != nil {
		s.cronScheduler.releaseClawWorkflowSlot(clawID)
	}
	// task_completed marks a requires_pr=false run successful, so a cancelled
	// run must not record it; the orphan sweep closes its summary as failed.
	if !cancelledWithoutOutcome {
		if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
			EventKey:        "workflow_v2:" + runID + ":" + string(run.Status),
			Source:          taskRunSourceHub,
			EventType:       taskRunEventTaskCompleted,
			ActorType:       taskRunActorAgent,
			InteractionRole: taskRunInteractionTerminal,
			Detail:          map[string]any{"workflow_v2_run_id": runID, "terminal_state": run.State, "requires_pr": false, "outcome": outcome},
		}); err != nil {
			log.Printf("[workflow-v2] failed to record task completion for claw %s: %v", clawID, err)
		}
	}
	s.broadcastToUsers(run.TenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
	})
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "workflow v2 completed") // websocket.StatusNormalClosure = 1000
		delete(s.claws, clawID)
	}
	s.mu.Unlock()
}

// releaseWorkflowV2CronSlot releases the in-memory overlap slot for a terminal
// v2 cron run exactly once. It does not update the cron history row; callers
// that need to record a failure reason should finish that row separately. The
// release is idempotent via cron_slot_released.
func (s *Server) releaseWorkflowV2CronSlot(ctx context.Context, runID string) {
	if s == nil || s.db == nil || s.cronSchedulerV2 == nil || runID == "" {
		return
	}
	// Load and validate the run before consuming the idempotency marker. If the
	// read fails, a retry can still finish the release because the marker has not
	// been flipped.
	run, err := workflowv2.NewStore(s.db).GetRun(ctx, runID)
	if err != nil {
		log.Printf("[workflow-v2] cannot load run %s for slot release: %v", runID, err)
		return
	}
	if run.TriggerType != "cron" {
		return
	}
	if run.Status != workflowv2.RunCompleted && run.Status != workflowv2.RunCancelled {
		return
	}
	res, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_runs SET cron_slot_released=1 WHERE id=? AND cron_slot_released=0`, runID)
	if err != nil {
		log.Printf("[workflow-v2] failed to mark cron slot released for run %s: %v", runID, err)
		return
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return
	}
	s.cronSchedulerV2.decrementRunning(run.WorkspaceName + "/" + run.WorkflowName)
}

// cancelWorkflowV2RunForClaw cancels the active/suspended workflow v2 run bound
// to the given claw, including all child effects/tasks so nothing remains
// assigned to the dead claw. It is used when a claw fails before the v2 run
// can reach a terminal state on its own (e.g. provisioning failed).
func (s *Server) cancelWorkflowV2RunForClaw(ctx context.Context, clawID, reason string) {
	if s == nil || s.db == nil {
		return
	}
	var runID string
	err := s.db.QueryRowContext(ctx, `
		SELECT a.run_id FROM workflow_v2_attempts a
		JOIN workflow_v2_runs r ON r.id=a.run_id
		WHERE a.claw_id=? AND a.status='active' AND r.status IN ('active','suspended')
		LIMIT 1`, clawID).Scan(&runID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[workflow-v2] failed to find active run for claw %s: %v", shortID(clawID), err)
		}
		return
	}
	if err := workflowv2.NewStore(s.db).CancelActivation(ctx, runID, reason); err != nil {
		log.Printf("[workflow-v2] failed to cancel run %s for claw %s: %v", shortID(runID), shortID(clawID), err)
		return
	}
	log.Printf("[workflow-v2] cancelled run %s because claw %s %s", shortID(runID), shortID(clawID), reason)
	s.maybeFinishWorkflowV2Parent(ctx, runID)
}
