package hub

import (
	"log"
	"time"
)

// getPipelineStage returns the current pipeline stage ID for a claw.
// Returns "" if the claw has no pipeline stage set.
func (s *Server) getPipelineStage(clawID string) string {
	var stage string
	_ = s.db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage)
	return stage
}

// claimPipelineStageTransition atomically updates the pipeline stage only if the
// claw is not already in that stage. It returns true when the caller won the
// transition and should run on_enter actions.
func (s *Server) claimPipelineStageTransition(clawID, stageID string) bool {
	// A won transition re-arms the idle auto-resume budget: idle_resume_count
	// is a per-work-unit runaway cap (agentIdleResumeMaxAttempts), and a new
	// stage is a new unit of work. Counting across stages let ten successful,
	// harmless pokes in a stage with nothing to do exhaust the cap, leaving the
	// claw with no recovery at all in the next stage (NEXT-647 sat 8h16m on a
	// sessions_yield in review_loop because the budget had been spent the day
	// before). The pipeline_stage<>? guard is what keeps this honest: a
	// re-entry into the same stage changes no row and so resets nothing.
	// idle_resume_at is deliberately left alone — it is the once-per-stretch
	// dedupe latch, not budget, and a stage whose on_enter runs nothing that
	// finishes a turn would otherwise have the SAME idle stretch poked twice.
	res, err := s.db.Exec(`UPDATE claws SET pipeline_stage=?, stage_entered_at=?, idle_resume_count=0 WHERE id=? AND pipeline_stage<>?`, stageID, time.Now().UnixMilli(), clawID, stageID)
	if err != nil {
		log.Printf("[pipeline] failed to set stage %q for claw %s: %v", stageID, clawID[:8], err)
		return false
	}
	rows, err := res.RowsAffected()
	if err != nil {
		log.Printf("[pipeline] failed to inspect stage transition for claw %s: %v", clawID[:8], err)
		return false
	}
	return rows > 0
}

// recordPipelineStageVisit records that a claw has visited a given pipeline stage.
// Used to prevent one-shot triggers (like output_matches) from re-firing.
func (s *Server) recordPipelineStageVisit(clawID, stageID string) {
	_, err := s.db.Exec(`
		INSERT INTO pipeline_stage_history(claw_id, stage_id, created_at)
		VALUES(?, ?, ?)
		ON CONFLICT(claw_id, stage_id) DO NOTHING`,
		clawID, stageID, now())
	if err != nil {
		log.Printf("[pipeline] failed to record stage visit for claw %s stage %s: %v", clawID[:8], stageID, err)
	}
}

// hasVisitedPipelineStage returns true if the claw has previously visited the
// given pipeline stage.
func (s *Server) hasVisitedPipelineStage(clawID, stageID string) bool {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM pipeline_stage_history
		WHERE claw_id=? AND stage_id=?`, clawID, stageID).Scan(&count)
	if err != nil {
		return false
	}
	return count > 0
}
