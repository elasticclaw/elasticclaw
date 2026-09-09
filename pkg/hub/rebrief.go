package hub

import (
	"fmt"
	"log"
	"strings"
)

// clearRebriefPending discards an armed re-brief without delivering it. Used
// when pipeline entry initialization has just briefed the fresh session with
// full task context (a claw that died before its first stage): leaving the
// flag armed would make a later benign bridge flap replay a "sandbox was
// replaced" brief into a live, intact session.
func (s *Server) clearRebriefPending(clawID string) {
	_, _ = s.db.Exec(`UPDATE claws SET rebrief_pending=0 WHERE id=?`, clawID)
}

// rebriefAfterRestoreIfNeeded re-briefs a claw whose sandbox was replaced by
// [claw-retry]. The replacement runs a brand-new OpenClaw session: the
// checkpoint restore brings back workspace files but not the conversation, so
// without a re-brief the fresh agent's first input is an unrelated
// watchdog/pr-watcher nag and it has no idea which ticket it owns.
// rebrief_pending is armed only by resetClawForRetry — a normal
// reconnect/bridge flap never sets it, so this is a no-op outside retries.
//
// The re-brief deliberately re-renders ONLY the current stage's inject text.
// Re-running on_enter side effects (run/gate/judge/move_issue/labels/notify/
// merge_pr) on a restore would double-post comments and re-trigger builds.
func (s *Server) rebriefAfterRestoreIfNeeded(cc *clawConn, clawID string) bool {
	// Consume the flag before doing any work so two racing reconnects cannot
	// deliver the brief twice: at-most-once beats at-least-once here.
	res, err := s.db.Exec(`UPDATE claws SET rebrief_pending=0 WHERE id=? AND rebrief_pending=1`, clawID)
	if err != nil {
		return false
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return false
	}

	// If the pipeline context or stage cannot be resolved anymore the flag
	// stays consumed — there is no task context to re-brief with, and the
	// normal wake path can take over on the next branch.
	ctx, ok := s.findPipelineContextForClaw(clawID)
	if !ok {
		return false
	}
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		return false
	}
	stage := pl.StageByID(s.getPipelineStage(clawID))
	if stage == nil {
		// The recorded stage no longer exists in the pipeline definition
		// (e.g. the workflow was edited); the entry stage's context is still
		// better than briefing with nothing.
		stage = pl.EntryStage()
	}
	if stage == nil {
		return false
	}

	var b strings.Builder
	b.WriteString("[hub] Your sandbox was replaced after an infrastructure failure and this is a fresh session — your previous conversation is gone. Re-read the task context below and continue from the current state of the workspace and PR; do not start over and do not ask which ticket this is.")
	b.WriteString(fmt.Sprintf("\n\nCurrent workflow stage: %s", stage.ID))
	if stage.Label != "" {
		b.WriteString(fmt.Sprintf(" (%s)", stage.Label))
	}
	if stage.OnEnter.Inject != "" {
		b.WriteString("\n\n" + s.renderStageInject(clawID, *stage, ctx))
	}
	for _, pr := range s.checkpointPRs(clawID) {
		b.WriteString(fmt.Sprintf("\nOpen PR: %s (%s, #%d)", pr.URL, pr.Repo, pr.Number))
	}

	s.injectHubMessageByID(clawID, b.String())
	log.Printf("[pipeline] re-briefed claw %s after restore (stage %q)", shortID(clawID), stage.ID)
	return true
}
