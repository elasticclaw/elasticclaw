package hub

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
	res, err := s.db.Exec(`UPDATE claws SET pipeline_stage=? WHERE id=? AND pipeline_stage<>?`, stageID, clawID, stageID)
	if err != nil {
		logf("[pipeline] failed to set stage %q for claw %s: %v", stageID, clawID[:8], err)
		return false
	}
	rows, err := res.RowsAffected()
	if err != nil {
		logf("[pipeline] failed to inspect stage transition for claw %s: %v", clawID[:8], err)
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
		logf("[pipeline] failed to record stage visit for claw %s stage %s: %v", clawID[:8], stageID, err)
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
