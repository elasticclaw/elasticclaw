package hub

import "log"

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
