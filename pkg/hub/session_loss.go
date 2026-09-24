package hub

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type sessionLossQuerier interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// sessionLossProgressMark uses stage and authored output, independent of the
// workspace's tools. Row identity distinguishes messages with equal timestamps.
func (s *Server) sessionLossProgressMark(clawID string) string {
	mark, _, _ := readSessionLossProgressMark(s.db, clawID, "")
	return mark
}

func readSessionLossProgressMark(q sessionLossQuerier, clawID, interruptedMsgID string) (mark, stage string, err error) {
	if err = q.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage); err != nil {
		return
	}
	mark = stage + "|"
	rows, err := q.Query(`SELECT content, created_at, rowid FROM messages WHERE claw_id=? AND role='claw' AND id <> ? ORDER BY created_at DESC, rowid DESC LIMIT 50`, clawID, interruptedMsgID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		var at time.Time
		var rowID int64
		if err = rows.Scan(&content, &at, &rowID); err != nil {
			return
		}
		content = strings.TrimSpace(content)
		if content != "" && !strings.HasPrefix(content, types.BridgeErrorPrefix) && !strings.HasPrefix(content, types.BridgeReplayErrorPrefix) {
			mark += at.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(rowID, 10)
			return
		}
	}
	err = rows.Err()
	return
}

// sessionLossLoopGuard counts only eligible, unthrottled recoveries. Progress
// resets the streak; reaching the limit pauses delivery until human input.
func (s *Server) sessionLossLoopGuard(clawID, reason, interruptedMsgID string) bool {
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
		// Release the transaction before resolving hints and storing the notice.
		_ = tx.Rollback()
		s.recordPausedSessionLossNotice(clawID, "session-loss resume")
		return true
	}
	mark, stage, err := readSessionLossProgressMark(tx, clawID, interruptedMsgID)
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
	// stage change or authored message cannot land between checking and pausing.
	if _, err := tx.Exec(`UPDATE claws SET session_loss_streak=?, session_loss_progress_mark=?, no_progress_paused=? WHERE id=?`, streak, mark, boolInt(streak >= max), clawID); err != nil {
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
