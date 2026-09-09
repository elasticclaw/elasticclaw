package hub

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const stageProgressBaselineKey = "stage_progress_baseline"

// lifecycleStageProgressAfter is deliberately opt-in: unlike agent_idle, an
// empty value means no detector runs at all.
func lifecycleStageProgressAfter(lc *types.LifecycleNotificationsConfig) (time.Duration, bool) {
	if lc == nil || lc.StageProgressAfter == "" {
		return 0, false
	}
	d, err := time.ParseDuration(lc.StageProgressAfter)
	return d, err == nil && d >= time.Minute
}

func (s *Server) stageProgressBaseline(nowAt time.Time, enabled bool) (time.Time, bool) {
	s.stageProgressBaselineMu.Lock()
	defer s.stageProgressBaselineMu.Unlock()
	if !enabled {
		if !s.stageProgressBaselineCleared {
			s.clearNotifierState(stageProgressBaselineKey)
			s.stageProgressBaselineCleared = true
			s.stageProgressBaselineAt = time.Time{}
		}
		return time.Time{}, true
	}
	s.stageProgressBaselineCleared = false
	if !s.stageProgressBaselineAt.IsZero() {
		return s.stageProgressBaselineAt, true
	}
	v, found, err := s.notifierStateInt64(stageProgressBaselineKey)
	if err != nil {
		log.Printf("[stage-progress] read baseline: %v", err)
		return time.Time{}, false
	}
	if found && v > 0 {
		s.stageProgressBaselineAt = time.UnixMilli(v)
	} else {
		s.stageProgressBaselineAt = nowAt
		s.setNotifierStateInt64(stageProgressBaselineKey, nowAt.UnixMilli())
	}
	return s.stageProgressBaselineAt, true
}

type stageProgressSnapshot struct {
	lastTurn, lastUser, subagents, streamingStarted time.Time
	subagentsActive                                 bool
}

func stageProgressSnapshotOf(nowAt time.Time, cc *clawConn) stageProgressSnapshot {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return stageProgressSnapshot{cc.lastTurnFinishedAt, cc.lastUserMessageAt, cc.subagentsActiveAt, cc.streamingStartedAt, !cc.subagentsActiveAt.IsZero() && nowAt.Sub(cc.subagentsActiveAt) < subagentsActiveFreshFor}
}

func latestStageProgress(stageEntered int64, snap stageProgressSnapshot) time.Time {
	latest := time.UnixMilli(stageEntered)
	if stageEntered == 0 {
		latest = time.Time{}
	}
	for _, t := range []time.Time{snap.lastTurn, snap.lastUser, snap.subagents} {
		if t.After(latest) {
			latest = t
		}
	}
	return latest
}

// checkStageProgress detects a pipeline stage which is still current but has
// had no turn completion, user message, subagent activity, or stage entry.
// Heartbeat/activity rows intentionally never participate in this calculation.
func (s *Server) checkStageProgress(nowAt time.Time, clawID string, cc *clawConn) {
	cfg := s.notificationsConfig()
	threshold, configured := lifecycleStageProgressAfter(func() *types.LifecycleNotificationsConfig {
		if cfg == nil {
			return nil
		}
		return cfg.Lifecycle
	}())
	baseline, ok := s.stageProgressBaseline(nowAt, configured && cfg.Lifecycle.IsEnabled())
	if !configured || cfg == nil || !cfg.Lifecycle.IsEnabled() || !ok {
		return
	}
	snap := stageProgressSnapshotOf(nowAt, cc)
	if snap.subagentsActive || (!snap.streamingStarted.IsZero() && nowAt.Sub(snap.streamingStarted) < threshold) {
		return
	}
	var status, runID, stage string
	var entered sql.NullInt64
	var latched int64
	err := s.db.QueryRow(`SELECT status, COALESCE(task_run_id,''), COALESCE(pipeline_stage,''), stage_entered_at, stage_stalled_since FROM claws WHERE id=?`, clawID).Scan(&status, &runID, &stage, &entered, &latched)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[stage-progress] read claw %s: %v", shortID(clawID), err)
		}
		return
	}
	if stage == "" || !agentIdleEligible(status, runID, s.agentIdleRunPhase(runID), s.agentIdleHasClawPRs(clawID), true) {
		return
	}
	progress := latestStageProgress(entered.Int64, snap)
	if progress.IsZero() {
		return
	}
	// Any meaningful progress after the latched episode re-arms it.
	if latched != 0 && progress.UnixMilli() > latched {
		if _, err := s.db.Exec(`UPDATE claws SET stage_stalled_since=0 WHERE id=?`, clawID); err != nil {
			log.Printf("[stage-progress] clear latch for %s: %v", shortID(clawID), err)
		}
		latched = 0
	}
	if nowAt.Sub(progress) < threshold || latched != 0 {
		return
	}
	if progress.Before(baseline) {
		s.parkStageProgress(nowAt, clawID, runID, progress.UnixMilli())
		return
	}
	detail := map[string]any{"stage": stage, "lastProgressMinutes": int(nowAt.Sub(progress).Minutes())}
	if entered.Valid && entered.Int64 > 0 {
		detail["stageAgeMinutes"] = int(nowAt.Sub(time.UnixMilli(entered.Int64)).Minutes())
	}
	if runID != "" {
		if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{EventKey: taskRunEventStageStalled + ":" + strconv.FormatInt(progress.UnixMilli(), 10), Source: taskRunSourceHub, EventType: taskRunEventStageStalled, ActorType: taskRunActorSystem, InteractionRole: taskRunInteractionNeutral, Detail: detail, OccurredAt: nowAt}); err != nil {
			log.Printf("[stage-progress] record %s: %v", shortID(clawID), err)
			return
		}
	}
	if _, err := s.db.Exec(`UPDATE claws SET stage_stalled_since=? WHERE id=?`, progress.UnixMilli(), clawID); err != nil {
		log.Printf("[stage-progress] latch %s: %v", shortID(clawID), err)
	}
}

func (s *Server) parkStageProgress(nowAt time.Time, clawID, runID string, key int64) {
	if runID == "" {
		if _, err := s.db.Exec(`INSERT INTO slack_notification_deliveries(event_id,run_id,delivered_at,message_ts,status) VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, lifecycleClawStageStalledKey(clawID, key), lifecycleClawRunKey(clawID), epochMillis(nowAt), "", notificationDeliveryStatusSkipped); err != nil {
			return
		}
	}
	_, _ = s.db.Exec(`UPDATE claws SET stage_stalled_since=? WHERE id=?`, key, clawID)
}

func stageProgressDurationLabel(v map[string]any, key string) string {
	n := intFromDetail(v, key)
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d minute%s", n, pluralS(n))
}
