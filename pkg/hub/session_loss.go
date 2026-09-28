package hub

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type sessionLossQuerier interface {
	QueryRow(string, ...any) *sql.Row
}

// sessionLossProgressMark counts only completed replies, never persisted stream
// segments. The durable counter survives activity flushes and hub reconnects.
func (s *Server) sessionLossProgressMark(clawID string) string {
	mark, _, _ := readSessionLossProgressMark(s.db, clawID)
	return mark
}

func readSessionLossProgressMark(q sessionLossQuerier, clawID string) (mark, stage string, err error) {
	var completed int64
	err = q.QueryRow(`SELECT pipeline_stage, session_loss_completed_turns FROM claws WHERE id=?`, clawID).Scan(&stage, &completed)
	if err == nil {
		mark = stage + "|" + strconv.FormatInt(completed, 10)
	}
	return
}

func (s *Server) recordSessionLossCompletedTurn(clawID, reply string) {
	reply = strings.TrimSpace(reply)
	if reply == "" || strings.HasPrefix(reply, types.BridgeErrorPrefix) || strings.HasPrefix(reply, types.BridgeReplayErrorPrefix) {
		return
	}
	s.noProgressMu.Lock()
	defer s.noProgressMu.Unlock()
	if _, err := s.db.Exec(`UPDATE claws SET session_loss_completed_turns=session_loss_completed_turns+1 WHERE id=?`, clawID); err != nil {
		log.Printf("[watchdog] record completed turn for %s: %v", shortID(clawID), err)
	}
}

// sessionLossLoopGuard counts only eligible, unthrottled recoveries. Progress
// resets the streak; reaching the limit pauses delivery until human input.
func (s *Server) sessionLossLoopGuard(clawID, reason, recoveryNotice string) bool {
	max := s.livenessSettings().sessionLossMax
	if max <= 0 {
		return false
	}
	if reason == "" {
		reason = types.SessionLossReasonUnknown
	}
	s.noProgressMu.Lock()
	defer s.noProgressMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("[watchdog] session-loss guard for %s: %v", shortID(clawID), err)
		return false
	}
	defer tx.Rollback()
	var streak int
	var previous string
	var paused bool
	if err := tx.QueryRow(`SELECT session_loss_streak, session_loss_progress_mark, no_progress_paused != 0 FROM claws WHERE id=?`, clawID).Scan(&streak, &previous, &paused); err != nil {
		log.Printf("[watchdog] session-loss guard for %s: %v", shortID(clawID), err)
		return false
	}
	if paused {
		_ = tx.Rollback()
		if recoveryNotice != "" {
			s.recordPausedSessionLossNotice(clawID, "session-loss resume", recoveryNotice)
		}
		return true
	}
	mark, stage, err := readSessionLossProgressMark(tx, clawID)
	if err != nil {
		log.Printf("[watchdog] session-loss guard for %s: %v", shortID(clawID), err)
		return false
	}
	if mark != previous {
		streak = 1
	} else {
		streak++
	}
	// Latch the pause in the same transaction as the progress check, so a
	// stage change or completed reply cannot land between checking and pausing.
	// Save the recovery context atomically, before a human can see the pause.
	if _, err := tx.Exec(`UPDATE claws SET session_loss_streak=?, session_loss_progress_mark=?, no_progress_paused=?,
		pending_session_loss_notice=CASE WHEN ? AND pending_session_loss_notice='' THEN ? ELSE pending_session_loss_notice END
		WHERE id=?`, streak, mark, boolInt(streak >= max), streak >= max && recoveryNotice != "", recoveryNotice+"\n\n", clawID); err != nil {
		log.Printf("[watchdog] session-loss guard for %s: %v", shortID(clawID), err)
		return false
	}
	if err := tx.Commit(); err != nil {
		log.Printf("[watchdog] session-loss guard for %s: %v", shortID(clawID), err)
		return false
	}
	if streak < max {
		return false
	}
	notice := fmt.Sprintf("[hub] Automatic continuation paused: the agent's session was lost %d times in a row in stage %q (last cause: %s) with no observable progress in between — no new agent output and no stage change. A human needs to look at this claw. Send a message to resume.", streak, stage, reason)
	s.publishAutomaticContinuationPause(clawID, notice)
	if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
		EventKey: taskRunEventAgentIdle + ":session-loss-loop:" + strconv.Itoa(streak) + ":" + strconv.FormatInt(epochMillis(now()), 10),
		Source:   taskRunSourceHub, EventType: taskRunEventAgentIdle,
		ActorType: taskRunActorSystem, InteractionRole: taskRunInteractionNeutral,
		Detail:     map[string]any{"sessionLossStreak": streak, "sessionLossReason": reason, "pipelineStage": stage, "noProgressPaused": true},
		OccurredAt: now(),
	}); err != nil {
		log.Printf("[watchdog] record session-loss pause for %s: %v", shortID(clawID), err)
	}
	log.Printf("[watchdog] session-loss loop guard paused %s: streak=%d stage=%q reason=%s", shortID(clawID), streak, stage, reason)
	return true
}
