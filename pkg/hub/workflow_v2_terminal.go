package hub

import (
	"context"
	"database/sql"
	"log"
	"strings"

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
