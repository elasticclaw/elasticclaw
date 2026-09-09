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
)

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
		// Only release the in-memory overlap slot once. A run observed as
		// terminal multiple times must not decrement the counter for another
		// active cron execution of the same workflow.
		res, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_runs SET cron_slot_released=1 WHERE id=? AND cron_slot_released=0`, run.ID)
		if err != nil {
			log.Printf("[workflow-v2] failed to mark cron slot released for run %s: %v", runID, err)
		} else if changed, _ := res.RowsAffected(); changed == 1 {
			s.cronSchedulerV2.decrementRunning(run.WorkspaceName + "/" + run.WorkflowName)
		}
	}
	if strings.TrimSpace(run.TaskRunID) == "" {
		// No parent task run; nothing to finish.
		return
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
	// Eagerly update the task-run summary so the dashboard reflects the
	// terminal state without waiting for the analytics materializer.
	status, failureType := taskRunStatusClean, ""
	if run.Status == workflowv2.RunCancelled || run.State == "prepare_failed" {
		status, failureType = taskRunStatusFailed, run.State
	}
	finishedAt := time.Now().UTC().UnixMilli()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE task_run_summaries
		SET status=?, phase=?, failure_type=?, finished_at=?, updated_at=?
		WHERE run_id=?`, status, taskRunPhaseTerminal, failureType, finishedAt, finishedAt, run.TaskRunID); err != nil {
		log.Printf("[workflow-v2] failed to update task run summary %s for terminal run %s: %v", run.TaskRunID, runID, err)
	}
	s.syncWorkflowVolumes(clawID)
	if s.cronScheduler != nil {
		s.cronScheduler.releaseClawWorkflowSlot(clawID)
	}
	if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
		EventKey:        "workflow_v2:" + runID + ":" + string(run.Status),
		Source:          taskRunSourceHub,
		EventType:       taskRunEventTaskCompleted,
		ActorType:       taskRunActorAgent,
		InteractionRole: taskRunInteractionTerminal,
		Detail:          map[string]any{"workflow_v2_run_id": runID, "terminal_state": run.State, "requires_pr": false},
	}); err != nil {
		log.Printf("[workflow-v2] failed to record task completion for claw %s: %v", clawID, err)
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
			log.Printf("[workflow-v2] failed to find active run for claw %s: %v", clawID[:8], err)
		}
		return
	}
	if err := workflowv2.NewStore(s.db).CancelActivation(ctx, runID, reason); err != nil {
		log.Printf("[workflow-v2] failed to cancel run %s for claw %s: %v", runID[:8], clawID[:8], err)
		return
	}
	log.Printf("[workflow-v2] cancelled run %s because claw %s %s", runID[:8], clawID[:8], reason)
	s.maybeFinishWorkflowV2Parent(ctx, runID)
}
