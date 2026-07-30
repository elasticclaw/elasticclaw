package hub

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Agent-idle detection: alert when an agent stopped making progress.
//
// The signal is deliberately NOT clawConn.lastStatusAt — status pongs measure
// whether the BRIDGE PROCESS is alive, and a healthy bridge with a stalled
// agent answers pongs forever (silent-death detection in watchdogAction owns
// that axis). Idleness is the agent itself doing nothing: no turn in flight
// (isBusyLocked is false) and no turn finished for at least the configured
// threshold (lastTurnFinishedAt, set by finishTurnLocked).
//
// Detection rides the status watchdog's 2-minute tick, so a threshold of T
// fires between T and T+2m. That slack is accepted by design; do not tighten
// the tick for this feature.
//
// Durability: firing writes claws.idle_since (epoch millis of the idle
// stretch's start) as a once-per-stretch latch, so a hub restart cannot
// re-notify the same stretch, and the claw-pass notifier — which can only see
// the database — learns about idle ad-hoc claws from it. Run-backed claws
// additionally get a task_run_events row that the existing task-run notifier
// pass delivers; ad-hoc claws are delivered by the claw pass reading
// idle_since. Exactly one of the two ever fires for a claw (the same
// task_run_id ownership rule the other lifecycle kinds follow).

const (
	lifecycleDefaultIdleAfter = 5 * time.Minute

	// agentIdleStretchSlack absorbs clock drift in the stretch-start value
	// across hub restarts: a reconnect seeds lastTurnFinishedAt from the last
	// claw message's created_at, which can differ from the pre-restart
	// finishTurnLocked timestamp by seconds. Two stretch starts within the
	// slack are treated as the same stretch (no re-notification); a genuinely
	// new stretch starts at least idle_after later, far outside it.
	agentIdleStretchSlack = time.Minute

	// agentIdleBaselineKey persists (in slack_notifier_state, epoch millis)
	// the moment from which idle stretches may notify. Stretches that BEGAN
	// before it are parked (latched + skipped) instead of delivered: on the
	// first deploy or first enable of the feature no claw has an idle_since
	// latch yet, so the baseline seeders have nothing to park and every
	// long-idle claw would otherwise flood the channel with "idle for 27
	// hours" the moment the watchdog first ticks. The key is cleared while
	// lifecycle notifications are disabled so a re-enable stamps a fresh
	// baseline — the disabled window must stay silent, same as the muted-
	// category rule.
	agentIdleBaselineKey = "agent_idle_baseline"
)

// lifecycleIdleAfter returns the configured idle threshold, defaulting to 5m.
// Values below the validation floor are ignored rather than honoured so a
// config that bypassed validation cannot turn the alert into a noise firehose.
func lifecycleIdleAfter(lc *types.LifecycleNotificationsConfig) time.Duration {
	if lc != nil && lc.IdleAfter != "" {
		if d, err := time.ParseDuration(lc.IdleAfter); err == nil && d >= time.Minute {
			return d
		}
	}
	return lifecycleDefaultIdleAfter
}

// agentIdleBaseline returns the moment from which idle stretches may notify,
// managing the persisted key (see agentIdleBaselineKey). While the feature is
// disabled it clears the key (once per disabled window) and returns zero; the
// first enabled call after that — or ever — stamps a fresh baseline. ok is
// false only on a state read failure, in which case the caller must skip
// detection for this tick rather than guess.
func (s *Server) agentIdleBaseline(nowAt time.Time, enabled bool) (baseline time.Time, ok bool) {
	s.agentIdleBaselineMu.Lock()
	defer s.agentIdleBaselineMu.Unlock()
	if !enabled {
		if !s.agentIdleBaselineCleared {
			s.clearNotifierState(agentIdleBaselineKey)
			s.agentIdleBaselineCleared = true
			s.agentIdleBaselineAt = time.Time{}
		}
		return time.Time{}, true
	}
	s.agentIdleBaselineCleared = false
	if !s.agentIdleBaselineAt.IsZero() {
		return s.agentIdleBaselineAt, true
	}
	v, found, err := s.notifierStateInt64(agentIdleBaselineKey)
	switch {
	case err != nil:
		log.Printf("[agent-idle] read baseline: %v", err)
		return time.Time{}, false
	case found && v > 0:
		s.agentIdleBaselineAt = time.UnixMilli(v)
	default:
		s.agentIdleBaselineAt = nowAt
		s.setNotifierStateInt64(agentIdleBaselineKey, nowAt.UnixMilli())
	}
	return s.agentIdleBaselineAt, true
}

// checkAgentIdle runs once per claw per watchdog tick. It decides whether the
// claw's current idle stretch deserves an agent_idle notification and, if so,
// persists the durable signal for the notifier passes to deliver.
func (s *Server) checkAgentIdle(nowAt time.Time, clawID string, cc *clawConn) {
	cfg := s.notificationsConfig()
	enabled := cfg != nil && cfg.Lifecycle.IsEnabled()
	baseline, ok := s.agentIdleBaseline(nowAt, enabled)
	if !enabled || !ok {
		// Feature off: no detection state is written at all (the baseline
		// call above clears the enable stamp so a re-enable starts fresh).
		// Per-event toggles are honoured by the notifier passes, not here,
		// so muting agent_idle parks events instead of replaying them.
		return
	}

	cc.mu.RLock()
	busy := cc.isBusyLocked()
	lastTurn := cc.lastTurnFinishedAt
	connectedAt := cc.connectedAt
	noProgressPaused := cc.noProgressPaused
	notifiedAt := cc.idleNotifiedAt
	cc.mu.RUnlock()

	if busy {
		// A running turn ends the idle stretch.
		s.clearAgentIdleLatch(clawID, cc)
		return
	}
	// The stretch starts at the end of the last real turn, or — for a claw
	// that never ran one — the moment this connection registered. A claw that
	// came up connected and was never prompted at all (workflow start failed,
	// pipeline entry stage never injected, trigger dropped the issue) is the
	// most common real stall, so "no turn ever" must alert too.
	//
	// The stretch cannot extend into time the claw was not connected:
	// lastTurnFinishedAt is re-seeded from message history on registration
	// with no time bound, so without this floor a bridge that was offline
	// overnight — or a claw restored from a checkpoint — would be alerted on
	// its first tick for idleness that predates the connection.
	stretchStartAt := lastTurn
	if stretchStartAt.IsZero() {
		stretchStartAt = connectedAt
	}
	if stretchStartAt.IsZero() {
		return // no usable clock at all (registration always stamps connectedAt)
	}
	if stretchStartAt.Before(connectedAt) {
		stretchStartAt = connectedAt
	}
	idleFor := nowAt.Sub(stretchStartAt)
	if idleFor < lifecycleIdleAfter(cfg.Lifecycle) {
		return
	}
	if !notifiedAt.IsZero() {
		return // already fired for this stretch (in-memory fast path)
	}

	stretchStart := stretchStartAt.UnixMilli()
	var status, taskRunID, pipelineStage string
	var idleSince, createdAtSec int64
	err := s.db.QueryRow(`SELECT status, COALESCE(task_run_id,''), COALESCE(pipeline_stage,''), idle_since, CAST(strftime('%s', created_at) AS INTEGER) FROM claws WHERE id=?`, clawID).Scan(&status, &taskRunID, &pipelineStage, &idleSince, &createdAtSec)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[agent-idle] read claw %s: %v", shortID(clawID), err)
		}
		return
	}
	// The durable latch says this stretch was already handled (the in-memory
	// flag was lost to a hub restart). The anchor — not the floored start —
	// carries the stretch's identity across restarts: a reconnect moves
	// connectedAt but re-seeds lastTurnFinishedAt to (roughly) its old value,
	// and a latched stretch must stay silent through hub restarts.
	//
	// A claw that has never run a turn anchors on claws.created_at instead:
	// connectedAt is re-stamped on every registration, so it cannot name a
	// stretch across bridge reconnects or hub restarts — and a claw that has
	// never finished a turn has by definition been stalled continuously since
	// it was created, so a reconnect does not start a new stall. The stable
	// anchor keeps the ordinary comparison working (created_at never moves,
	// so it always predates a live latch); the latch clears the moment a turn
	// runs (clearAgentIdleLatch), re-arming the alert for real new stretches.
	// Accepted consequence: a never-turn stall that began before the
	// notification baseline is parked once and stays parked across reconnects
	// — the "first enable never replays history" rule applied consistently.
	anchor := lastTurn
	if anchor.IsZero() {
		anchor = time.Unix(createdAtSec, 0)
	}
	slackMs := agentIdleStretchSlack.Milliseconds()
	if idleSince != 0 && anchor.UnixMilli() <= idleSince+slackMs {
		cc.mu.Lock()
		cc.idleNotifiedAt = nowAt
		cc.mu.Unlock()
		return
	}
	autoDriven := false
	if taskRunID == "" {
		autoDriven = pipelineStage != "" || s.agentIdleHasActiveWorkflow(clawID)
	}
	if !agentIdleEligible(status, taskRunID, s.agentIdleRunPhase(taskRunID), s.agentIdleHasClawPRs(clawID), autoDriven) {
		return
	}
	if stretchStartAt.Before(baseline) {
		// The stretch began before notifications could have observed it
		// (first deploy/enable of the feature, or during a disabled window):
		// park it — latch without delivering — so enabling the feature never
		// replays pre-existing history. Only stretches that begin after the
		// baseline alert.
		s.parkAgentIdleStretch(nowAt, clawID, cc, taskRunID, stretchStart)
		return
	}

	detail := map[string]any{
		"idleSince":   stretchStart,
		"idleMinutes": int(idleFor.Minutes()),
	}
	if noProgressPaused {
		// noProgressPaused is deliberately folded into this alert rather than
		// given its own event type: both mean "the agent is not moving and
		// nobody has been told", the operator's response is the same (go
		// look), and a second type would double the toggle/migration surface
		// for a distinction the message body already carries.
		detail["noProgressPaused"] = true
	}
	if taskRunID != "" {
		// Run-backed: one durable event row, delivered by the task-run pass.
		// The key embeds the stretch start so each stretch is one event and
		// re-detections of the same stretch dedupe on the event-key conflict.
		if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
			EventKey:        taskRunEventAgentIdle + ":" + strconv.FormatInt(stretchStart, 10),
			Source:          taskRunSourceHub,
			EventType:       taskRunEventAgentIdle,
			ActorType:       taskRunActorSystem,
			InteractionRole: taskRunInteractionNeutral,
			Detail:          detail,
			OccurredAt:      nowAt,
		}); err != nil {
			log.Printf("[agent-idle] record event for claw %s: %v", shortID(clawID), err)
			return // leave the latch unset so the next tick retries
		}
	}
	// The latch is written after the event so a failed event write retries;
	// for ad-hoc claws setting it IS the notification trigger (the claw pass
	// keys its delivery row on the idle_since value).
	if _, err := s.db.Exec(`UPDATE claws SET idle_since=? WHERE id=?`, stretchStart, clawID); err != nil {
		log.Printf("[agent-idle] latch claw %s: %v", shortID(clawID), err)
		// The run-backed event (if any) is already recorded and its key
		// dedupes; the ad-hoc kind simply retries next tick.
		return
	}
	cc.mu.Lock()
	cc.idleNotifiedAt = nowAt
	cc.mu.Unlock()
}

// agentIdleEligible is the exclusion rule: an idle agent only alerts when it
// is plausibly STUCK, never when it is simply done or simply waiting for its
// human. An agent whose work is delivered and waiting on humans (PR review,
// merge) is idle by definition; alerting on it would train operators to mute
// the feature.
//
//   - status: only 'connected' claws alert. Terminal and protected statuses
//     (idle/completed/deleted — see isProtectedClawStatus) plus
//     error/offline/provisioning are all either finished or covered by the
//     failure notifications.
//   - any claw_prs row excludes, run-backed or not. claw_prs rows exist only
//     while a delivered PR is being tracked (they are deleted on close/merge/
//     teardown), so a row means "PR out, awaiting CI/review/merge". For
//     run-backed claws this is deliberately checked in ADDITION to the run
//     phase: associateTaskRunPR can fail (and is never retried), leaving the
//     phase at agent_running forever even though the PR was delivered.
//   - run-backed claws: the run's materialized phase must not be past the
//     working stage. 'pr_opened' and 'waiting_for_merge' mean a PR exists and
//     humans own the next move; 'terminal' means the run is over. A missing
//     summary row (” phase) does not exclude — the claw is connected and
//     demonstrably ran a turn, so materialization lag must not hide a stall.
//   - ad-hoc claws: alert only with positive evidence that something
//     AUTOMATIC drives the claw (a pipeline stage or a live workflow run).
//     An interactive claw a human chats with is prompted by nothing but that
//     human, so every pause longer than idle_after would be a structural
//     false positive — the exact noise this rule exists to prevent.
func agentIdleEligible(status, taskRunID, runPhase string, hasClawPRs, autoDriven bool) bool {
	if status != "connected" || hasClawPRs {
		return false
	}
	if taskRunID != "" {
		switch runPhase {
		case taskRunPhasePROpened, taskRunPhaseWaitingForMerge, taskRunPhaseTerminal:
			return false
		}
		return true
	}
	return autoDriven
}

// agentIdleHasActiveWorkflow reports whether a workflow run is still driving
// the claw (pending or running — every other status is final).
func (s *Server) agentIdleHasActiveWorkflow(clawID string) bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM workflow_runs WHERE claw_id=? AND status IN ('pending','running')`, clawID).Scan(&n); err != nil {
		log.Printf("[agent-idle] count workflow runs for %s: %v", shortID(clawID), err)
		return false
	}
	return n > 0
}

// parkAgentIdleStretch latches a stretch as handled WITHOUT delivering it:
// the skipped delivery row (ad-hoc) suppresses the claw pass, and for
// run-backed claws no task_run_events row is written at all, so neither pass
// has anything to send while the latch dedupes every future re-detection of
// the stretch.
func (s *Server) parkAgentIdleStretch(nowAt time.Time, clawID string, cc *clawConn, taskRunID string, stretchStart int64) {
	if taskRunID == "" {
		// The skipped row must land BEFORE the latch: the claw pass keys its
		// candidates on idle_since, so latch-first would race a concurrent
		// notifier tick into delivering the stretch being parked.
		if _, err := s.db.Exec(`
			INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
			VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`,
			lifecycleClawIdleKey(clawID, stretchStart), lifecycleClawThreadKey(clawID),
			epochMillis(nowAt), "", notificationDeliveryStatusSkipped); err != nil {
			log.Printf("[agent-idle] park claw %s: %v", shortID(clawID), err)
			return // retry next tick; nothing latched yet
		}
	}
	if _, err := s.db.Exec(`UPDATE claws SET idle_since=? WHERE id=?`, stretchStart, clawID); err != nil {
		log.Printf("[agent-idle] park latch claw %s: %v", shortID(clawID), err)
		return
	}
	cc.mu.Lock()
	cc.idleNotifiedAt = nowAt
	cc.mu.Unlock()
}

// agentIdleRunPhase reads the run's materialized phase; "" when there is no
// run or no summary yet.
func (s *Server) agentIdleRunPhase(taskRunID string) string {
	if taskRunID == "" {
		return ""
	}
	var phase string
	err := s.db.QueryRow(`SELECT phase FROM task_run_summaries WHERE run_id=?`, taskRunID).Scan(&phase)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("[agent-idle] read run phase for %s: %v", taskRunID, err)
	}
	return phase
}

// agentIdleHasClawPRs reports whether the claw currently tracks any delivered PR.
func (s *Server) agentIdleHasClawPRs(clawID string) bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&n); err != nil {
		log.Printf("[agent-idle] count claw prs for %s: %v", shortID(clawID), err)
		return false
	}
	return n > 0
}

// clearAgentIdleLatch drops the durable latch when the claw is busy again,
// re-arming the alert for the next idle stretch.
func (s *Server) clearAgentIdleLatch(clawID string, cc *clawConn) {
	cc.mu.Lock()
	cc.idleNotifiedAt = time.Time{}
	cc.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE claws SET idle_since=0 WHERE id=? AND idle_since != 0`, clawID); err != nil {
		log.Printf("[agent-idle] clear latch for claw %s: %v", shortID(clawID), err)
	}
}

// agentIdleDurationLabel renders the idle duration carried in an event's
// detail for humans ("7 minutes", "1 hour 12 minutes"). Empty when the detail
// carries no usable duration (e.g. a hand-written test send).
func agentIdleDurationLabel(detail map[string]any) string {
	minutes := intFromDetail(detail, "idleMinutes", "idle_minutes")
	if minutes <= 0 {
		return ""
	}
	if minutes < 60 {
		return fmt.Sprintf("%d minute%s", minutes, pluralS(minutes))
	}
	hours, rest := minutes/60, minutes%60
	if rest == 0 {
		return fmt.Sprintf("%d hour%s", hours, pluralS(hours))
	}
	return fmt.Sprintf("%d hour%s %d minute%s", hours, pluralS(hours), rest, pluralS(rest))
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
