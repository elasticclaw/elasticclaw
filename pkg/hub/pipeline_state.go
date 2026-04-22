package hub

import "log"

// getPipelineStage returns the current pipeline stage ID for a claw.
// Returns "" if the claw has no pipeline stage set.
func (s *Server) getPipelineStage(clawID string) string {
	var stage string
	_ = s.db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage)
	return stage
}

// setPipelineStage updates the pipeline stage ID for a claw.
func (s *Server) setPipelineStage(clawID, stageID string) {
	if _, err := s.db.Exec(`UPDATE claws SET pipeline_stage=? WHERE id=?`, stageID, clawID); err != nil {
		log.Printf("[pipeline] failed to set stage %q for claw %s: %v", stageID, clawID[:8], err)
	}
}
