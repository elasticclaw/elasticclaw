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

// agentIdleResumeBlindGrace bounds the window in which the hub must not
// auto-resume a claw because it cannot see whether a turn is running.
//
// A clawConn carries no in-flight turn state across a fresh registration —
// neither a hub restart nor a bridge reconnect hands it over — while the
// gateway on the sandbox keeps running the turn it already had. In that window
// isBusyLocked() reports false for a claw that is genuinely mid-turn, and
// injecting into it is not a nudge: pinned OpenClaw treats a mid-turn injection
// as a session takeover and aborts the turn with
// EmbeddedAttemptSessionTakeoverError. NEXT-724 lost a 16-minute turn exactly
// this way, 46 seconds after an auto-resume that fired 6 minutes into a fresh
// connection following a hub restart.
//
// The grace tracks minBusyTurnMax rather than the bridge cap it is derived
// from. The bridge's cap (agentTurnTimeout, 1h) ends the WAIT, but teardown —
// abortActiveSession, then createFreshSession — still runs before the error
// reply reaches the hub, so a turn that began just before a reconnect can be
// unwinding past connectedAt+1h. minBusyTurnMax already encodes exactly this
// slack for the busy-turn watchdog; the three constants move together.
//
// It is an upper bound, not a wait: the guard lifts the moment a turn ends
// where this connection can see it, which any working agent does within a turn
// or two.
//
// Two limits worth stating rather than implying. The cap is a timer inside the
// bridge PROCESS — if that process dies mid-turn the timer dies with it, and
// whether the gateway-side run also stops depends on OpenClaw, which is
// upstream and not verifiable here. And a claw that never produces a boundary
// is NOT covered by claw_retry: that escalates on 12 consecutive
// gateway_healthy=false heartbeats, while a stalled agent behind a healthy
// gateway heartbeats healthy forever — the exact shape of NEXT-713. Between
// the two mechanisms sits a gap where only the human agent_idle alert fires.
const agentIdleResumeBlindGrace = minBusyTurnMax

const (
	lifecycleDefaultIdleAfter = 5 * time.Minute

	// bridgeHeartbeatInterval is the bridge's heartbeat period — the 15s
	// ticker in startHubKeepalives (cmd/claw-bridge). Named here because the
	// freshness window below must be expressed in heartbeats, not in an
	// unrelated wall-clock guess; if the bridge's ticker changes, this must
	// change with it (defaultGatewayUnhealthyMax's comment carries the same
	// dependency).
	bridgeHeartbeatInterval = 15 * time.Second

	// subagentsActiveFreshFor bounds how long one "subagents active" report
	// keeps suppressing the agent_idle alert without being renewed. A live
	// bridge re-reports every bridgeHeartbeatInterval, so the state is
	// normally refreshed long before this expires; three intervals tolerate a
	// couple of dropped or delayed heartbeats without flapping, while a bridge
	// that dies (or stops being able to poll the gateway) can silence the
	// alert for at most ~45s past its last word — stale state must never
	// suppress it indefinitely.
	subagentsActiveFreshFor = 3 * bridgeHeartbeatInterval

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

// agentIdleSnapshot is one consistent read of the connection state both the
// alert (checkAgentIdle) and the recovery (checkAgentIdleResume) reason about.
// The stretch-start rule is subtle enough that a second copy of it would drift:
// the two paths must agree on when a stretch began or the resume could fire for
// a stretch the alert never considered started.
type agentIdleSnapshot struct {
	busy             bool
	noProgressPaused bool
	// subagentsActive is true while the bridge RECENTLY reported spawned
	// subagent sessions with a run in flight (sessions.list, ridden on the
	// heartbeat). Freshness matters: the report suppresses the alert, so a
	// stale one — a bridge that died mid-spawn — must expire (see
	// subagentsActiveFreshFor) rather than silence the alert forever.
	subagentsActive bool
	lastTurn        time.Time
	notifiedAt      time.Time
	connectedAt     time.Time
	// turnBoundarySeen is false while the hub has never watched a turn end on
	// this connection, which is exactly when it cannot tell an idle agent from
	// one whose turn it simply cannot see.
	turnBoundarySeen bool
	// stretchStartAt is zero when the connection carries no usable clock at
	// all (registration always stamps connectedAt, so this is unreachable in
	// production and simply means "do not judge this claw").
	stretchStartAt time.Time
}

// agentIdleSnapshotOf computes the idle stretch start under a single read lock.
//
// The stretch starts at the end of the last real turn, or — for a claw that
// never ran one — the moment this connection registered. A claw that came up
// connected and was never prompted at all (workflow start failed, pipeline
// entry stage never injected, trigger dropped the issue) is the most common
// real stall, so "no turn ever" must count too.
//
// The stretch cannot extend into time the claw was not connected:
// lastTurnFinishedAt is re-seeded from message history on registration with no
// time bound, so without this floor a bridge that was offline overnight — or a
// claw restored from a checkpoint — would be judged on its first tick for
// idleness that predates the connection.
func agentIdleSnapshotOf(cc *clawConn) agentIdleSnapshot {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	snap := agentIdleSnapshot{
		busy:             cc.isBusyLocked(),
		noProgressPaused: cc.noProgressPaused,
		subagentsActive:  !cc.subagentsActiveAt.IsZero() && time.Since(cc.subagentsActiveAt) < subagentsActiveFreshFor,
		lastTurn:         cc.lastTurnFinishedAt,
		notifiedAt:       cc.idleNotifiedAt,
		connectedAt:      cc.connectedAt,
		turnBoundarySeen: cc.turnBoundarySeen,
	}
	start := cc.lastTurnFinishedAt
	if start.IsZero() {
		start = cc.connectedAt
	}
	if start.IsZero() {
		return snap
	}
	if start.Before(cc.connectedAt) {
		start = cc.connectedAt
	}
	snap.stretchStartAt = start
	return snap
}

// agentIdleStretchAnchor names a stretch across hub restarts and bridge
// reconnects. lastTurnFinishedAt is re-seeded (roughly) to its old value on
// registration, so it survives; a claw that never ran a turn anchors on
// claws.created_at instead, because connectedAt is re-stamped on every
// registration and would make an unchanged stall look like a brand-new one.
func agentIdleStretchAnchor(lastTurn time.Time, createdAtSec int64) time.Time {
	if !lastTurn.IsZero() {
		return lastTurn
	}
	return time.Unix(createdAtSec, 0)
}

// agentIdleAutoDriven reports whether something automatic drives an ad-hoc
// claw (a pipeline stage or a live workflow run). Run-backed claws are driven
// by their run and never ask.
func (s *Server) agentIdleAutoDriven(clawID, taskRunID, pipelineStage string) bool {
	if taskRunID != "" {
		return false
	}
	return pipelineStage != "" || s.agentIdleHasActiveWorkflow(clawID)
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

	snap := agentIdleSnapshotOf(cc)
	lastTurn, noProgressPaused, notifiedAt := snap.lastTurn, snap.noProgressPaused, snap.notifiedAt

	if snap.busy {
		// A running turn ends the idle stretch.
		s.clearAgentIdleLatch(clawID, cc)
		return
	}
	if snap.subagentsActive {
		// Spawned subagent sessions are still running on the gateway. An agent
		// that calls sessions_yield after sessions_spawn ENDS ITS TURN on
		// purpose — the spawned work's completion arrives as the next message —
		// so by turn state alone it is indistinguishable from a stall, and
		// notifying here pages a human about an agent that is working exactly
		// as designed. This is NOT the busy case above: busy means a turn is in
		// flight and legitimately clears the latch, whereas here the claw is
		// neither confirmed idle nor confirmed to have run a turn. So leave
		// every latch exactly as it is — idle_since untouched, idleNotifiedAt
		// unstamped — and re-decide on the next tick, once the completion turn
		// runs (clearing the latch through the busy path) or the activity
		// report goes stale.
		return
	}
	stretchStartAt := snap.stretchStartAt
	if stretchStartAt.IsZero() {
		return // no usable clock at all (registration always stamps connectedAt)
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
	anchor := agentIdleStretchAnchor(lastTurn, createdAtSec)
	slackMs := agentIdleStretchSlack.Milliseconds()
	if idleSince != 0 && anchor.UnixMilli() <= idleSince+slackMs {
		cc.mu.Lock()
		cc.idleNotifiedAt = nowAt
		cc.mu.Unlock()
		return
	}
	autoDriven := s.agentIdleAutoDriven(clawID, taskRunID, pipelineStage)
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

// Idle auto-resume (NEXT-713). The alert above tells a human; this recovers.
//
// The incident: the agent called a tool that waits for spawned work
// (sessions_yield in the pinned OpenClaw) and that call never returned a
// result. The call ENDS THE TURN, so to the hub the claw looked idle and
// perfectly healthy — status pongs fine, no turn in flight, nothing queued —
// and every existing recovery path missed it: the busy-turn watchdog needs an
// open turn, sendStreamingNudge drops when no turn is open, and the
// restart/session-rotation resumes need a restart or a rotation (a clean
// bridge reconnect is neither). The claw sat for 1h44m and the only thing that
// ever moved it was a human typing "continue". This injects that "continue".
//
// It is deliberately NOT part of checkAgentIdle: that function returns early
// when lifecycle notifications are disabled or muted, and recovery must not
// depend on whether anyone is listening to Slack.
const (
	defaultIdleResumeAfter = 10 * time.Minute
	// minIdleResumeAfter is the floor for idle_resume_after. It is NOT what
	// keeps a stalled claw from being poked repeatedly — the once-per-stretch
	// idle_resume_at latch is, and it works at any threshold, including ones
	// below the 2-minute watchdog tick. The floor exists because below a
	// minute the threshold stops describing a stall at all: a claw that pauses
	// for seconds between turns is simply working, and resuming it interrupts
	// the very progress the recovery is meant to restore.
	minIdleResumeAfter = time.Minute

	// agentIdleResumeBaselineKey persists (in slack_notifier_state, epoch
	// millis) the moment from which idle stretches may be auto-resumed.
	// Stretches that BEGAN before it are parked (latched, never injected).
	// Without it the first watchdog tick after this feature reaches an
	// existing hub would poke every connected, eligible, long-idle claw at
	// once — including claws a human deliberately walked away from hours ago,
	// which is a thundering herd of prompts, not a recovery.
	//
	// This mirrors agentIdleBaselineKey but is deliberately independent state:
	// the two features must not be able to move each other's baseline, and
	// this one is cleared only while the RESUME itself is disabled. Lifecycle
	// notifications being off must never touch it — the resume does not read
	// the notification config at all, by design.
	agentIdleResumeBaselineKey = "agent_idle_resume_baseline"

	// agentIdleResumeMaxAttempts bounds the resumes a single claw can ever
	// receive. The PRIMARY backstop is the existing no-progress watchdog: a
	// resume that produces the same empty outcome three times sets
	// no_progress_paused, which this check honours (see below). The cap covers
	// the pathological case that watchdog cannot see — turns whose outcomes
	// keep differing just enough to reset its observation window — so a
	// wake-do-nothing-idle loop cannot poke forever.
	agentIdleResumeMaxAttempts = 10

	agentIdleResumePrefix = "[hub] No turn has been running for"
)

// agentIdleResumeBaseline returns the moment from which idle stretches may be
// auto-resumed, managing the persisted key (see agentIdleResumeBaselineKey).
// While the resume is disabled it clears the key (once per disabled window)
// and returns zero, so a re-enable stamps a fresh baseline and the disabled
// window is never replayed. ok is false only on a state read failure, in which
// case the caller must skip the resume for this tick rather than guess — the
// guess would be a poke.
//
// Deliberately takes the resume's OWN enabled flag: unlike agentIdleBaseline
// this must never be driven by the notification config.
func (s *Server) agentIdleResumeBaseline(nowAt time.Time, enabled bool) (baseline time.Time, ok bool) {
	s.agentIdleResumeBaselineMu.Lock()
	defer s.agentIdleResumeBaselineMu.Unlock()
	if !enabled {
		if !s.agentIdleResumeBaselineCleared {
			s.clearNotifierState(agentIdleResumeBaselineKey)
			s.agentIdleResumeBaselineCleared = true
			s.agentIdleResumeBaselineAt = time.Time{}
		}
		return time.Time{}, true
	}
	s.agentIdleResumeBaselineCleared = false
	if !s.agentIdleResumeBaselineAt.IsZero() {
		return s.agentIdleResumeBaselineAt, true
	}
	v, found, err := s.notifierStateInt64(agentIdleResumeBaselineKey)
	switch {
	case err != nil:
		log.Printf("[agent-idle] read resume baseline: %v", err)
		return time.Time{}, false
	case found && v > 0:
		s.agentIdleResumeBaselineAt = time.UnixMilli(v)
	default:
		s.agentIdleResumeBaselineAt = nowAt
		s.setNotifierStateInt64(agentIdleResumeBaselineKey, nowAt.UnixMilli())
	}
	return s.agentIdleResumeBaselineAt, true
}

// checkAgentIdleResume runs on the same watchdog tick as checkAgentIdle and
// injects a resume prompt into a claw that has been idle past
// liveness.idle_resume_after. It shares the eligibility rule and the
// stretch-start computation with the alert; only the threshold, the latch and
// the delivery differ.
func (s *Server) checkAgentIdleResume(nowAt time.Time, clawID string, cc *clawConn) {
	cfg := s.livenessSettings()
	baseline, ok := s.agentIdleResumeBaseline(nowAt, cfg.idleResumeEnabled)
	if !cfg.idleResumeEnabled || !ok {
		return
	}
	snap := agentIdleSnapshotOf(cc)
	// snap.subagentsActive is deliberately NOT consulted here: the auto-resume
	// still fires for a claw parked on sessions_yield with spawned work
	// running. Only the notification is suppressed by that signal.
	if snap.busy {
		// A turn is running: nothing to resume.
		//
		// The latch is deliberately NOT cleared here. A turn RESERVATION in
		// flight is not a turn that finished: abortTurnLocked (WS write
		// failure) releases the reservation without moving
		// lastTurnFinishedAt, so a tick that lands inside a delivery which
		// then aborts would clear the latch while the stretch anchor is
		// unchanged — and the same stretch would be resumed twice. Re-arming
		// is driven by lastTurnFinishedAt actually moving past the latched
		// stretch instead (see below).
		return
	}
	if !snap.turnBoundarySeen && nowAt.Sub(snap.connectedAt) < agentIdleResumeBlindGrace {
		// No turn has ended where this connection could see it, so "idle" here
		// may mean "mid-turn and invisible". Injecting would abort that turn.
		return
	}
	if snap.noProgressPaused {
		// The no-progress watchdog has deliberately stopped automatic
		// continuation for this claw and told its human so. Poking it here
		// would fight that backstop and restart the exact loop it stopped.
		return
	}
	if snap.stretchStartAt.IsZero() {
		return
	}
	idleFor := nowAt.Sub(snap.stretchStartAt)
	if idleFor < cfg.idleResumeAfter {
		return
	}

	var status, taskRunID, pipelineStage string
	var resumeAt, resumeCount, createdAtSec int64
	err := s.db.QueryRow(`SELECT status, COALESCE(task_run_id,''), COALESCE(pipeline_stage,''), idle_resume_at, idle_resume_count, CAST(strftime('%s', created_at) AS INTEGER) FROM claws WHERE id=?`, clawID).
		Scan(&status, &taskRunID, &pipelineStage, &resumeAt, &resumeCount, &createdAtSec)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("[agent-idle] read claw %s for resume: %v", shortID(clawID), err)
		}
		return
	}
	// Same stretch identity rule as the alert's latch: the anchor, not the
	// floored start, so a hub restart or bridge reconnect cannot re-poke a
	// stretch that was already resumed or parked.
	anchor := agentIdleStretchAnchor(snap.lastTurn, createdAtSec)
	if resumeAt != 0 {
		if anchor.UnixMilli() <= resumeAt+agentIdleStretchSlack.Milliseconds() {
			return // this stretch was already handled
		}
		// lastTurnFinishedAt has moved past the latched stretch, so a turn
		// genuinely ran and finished (an aborted reservation cannot produce
		// this — it never stamps lastTurnFinishedAt). This is a new stretch:
		// re-arm the durable latch. The comparison above would already let
		// this tick through, but clearing keeps the stored state honest for
		// the paths below that return without re-latching (cap reached,
		// no longer eligible, parked).
		s.clearAgentIdleResumeLatch(clawID)
	}
	if resumeCount >= agentIdleResumeMaxAttempts {
		return // cap already reached and already logged (see below)
	}
	if !agentIdleEligible(status, taskRunID, s.agentIdleRunPhase(taskRunID), s.agentIdleHasClawPRs(clawID), s.agentIdleAutoDriven(clawID, taskRunID, pipelineStage)) {
		return
	}

	stretchStart := snap.stretchStartAt.UnixMilli()
	if snap.stretchStartAt.Before(baseline) {
		// The stretch began before the resume could have observed it (first
		// deploy or first enable, or during a disabled window): park it —
		// latch without injecting, without consuming the lifetime cap — so
		// turning the feature on never replays history. This is what keeps
		// the first tick against an existing database from poking every
		// long-idle claw on it at once. A real turn re-arms the claw
		// normally, so parking costs a stalled claw at most one stretch.
		s.parkAgentIdleResumeStretch(clawID, stretchStart)
		return
	}

	// Latch before injecting: if the UPDATE fails we send nothing and retry on
	// the next tick, whereas injecting first would risk a second poke for the
	// same stretch on every tick until the write finally succeeded.
	if _, err := s.db.Exec(`UPDATE claws SET idle_resume_at=?, idle_resume_count=idle_resume_count+1 WHERE id=?`, stretchStart, clawID); err != nil {
		log.Printf("[agent-idle] latch resume for claw %s: %v", shortID(clawID), err)
		return
	}
	log.Printf("[agent-idle] auto-resuming claw %s after %d minutes idle (attempt %d/%d)", shortID(clawID), int(idleFor.Minutes()), resumeCount+1, agentIdleResumeMaxAttempts)
	if resumeCount+1 >= agentIdleResumeMaxAttempts {
		log.Printf("[agent-idle] claw %s reached the lifetime auto-resume cap of %d; no further resumes will be sent", shortID(clawID), agentIdleResumeMaxAttempts)
	}
	// What makes the resume once-per-stretch is the idle_resume_at latch
	// written just above, NOT injectMessage's dedupe: the message embeds the
	// idle minute count, so two injections are only "identical" when their
	// minute counts happen to match. The dedupe is still worth having for the
	// case it does cover — successive stretches whose prompts are byte-equal
	// while an earlier one sits undelivered on a wedged socket — so no
	// uniqueness marker is appended to defeat it.
	s.injectHubMessageByID(clawID, agentIdleResumeMessage(idleFor))
}

// parkAgentIdleResumeStretch latches a stretch as handled WITHOUT injecting a
// prompt and without consuming an attempt from the lifetime cap: nothing is
// delivered, so nothing was spent. The latch alone dedupes every future
// re-detection of the stretch, including across hub restarts.
func (s *Server) parkAgentIdleResumeStretch(clawID string, stretchStart int64) {
	if _, err := s.db.Exec(`UPDATE claws SET idle_resume_at=? WHERE id=?`, stretchStart, clawID); err != nil {
		log.Printf("[agent-idle] park resume latch for claw %s: %v", shortID(clawID), err)
	}
}

// agentIdleResumeMessage is deliberately hub-generic: the hub serves other
// factories, so it must not name this workspace's stages or tools. It states
// the observation, gives the blocked-on-background case its own instruction
// (the incident's actual shape: waiting on spawned work that never reported),
// and otherwise repeats the workspace-recovery wording of
// enqueueSessionLostResume so a resumed agent never starts over.
func agentIdleResumeMessage(idleFor time.Duration) string {
	return fmt.Sprintf("%s %d minutes. If you are blocked waiting on background or spawned work that has not reported back, stop waiting for it and follow your task's degradation path instead of waiting longer. Otherwise, recover your state from the workspace: run git status and git log --oneline -15, check which branch you are on and whether there are uncommitted changes. Then resume the current stage from where the workspace shows it stopped. Do not start over and do not discard existing work.", agentIdleResumePrefix, int(idleFor.Minutes()))
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
//   - any UNRESOLVED claw_prs row (state not merged/closed) excludes,
//     run-backed or not. Rows are deleted only when the claw is finalized;
//     until then resolved rows survive so the all-PRs-resolved teardown gate
//     can count them, and only an unresolved row still means "PR out,
//     awaiting CI/review/merge". For
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
			lifecycleClawIdleKey(clawID, stretchStart), lifecycleClawRunKey(clawID),
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

// agentIdleHasClawPRs reports whether the claw currently tracks any
// unresolved DELIVERED PR. Resolved rows (state merged/closed) survive until
// the claw is finalized so the all-PRs-resolved gate can count them — a
// resolved row must never read as "awaiting humans" here, or a retried claw
// carrying one would have its stuck alert suppressed for life. Mention-only
// rows are excluded for the same reason, matching clawOpenPRCount: a PR the
// agent merely linked is not "PR out, awaiting humans", and since the watcher
// never finalizes a claw with zero delivered rows, counting a mention here
// would make a hung mention-only claw simultaneously immortal (never torn
// down) and invisible (stuck alert suppressed).
func (s *Server) agentIdleHasClawPRs(clawID string) bool {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=? AND state NOT IN ('merged','closed') AND mention_only=0`, clawID).Scan(&n); err != nil {
		log.Printf("[agent-idle] count claw prs for %s: %v", shortID(clawID), err)
		return false
	}
	return n > 0
}

// clearAgentIdleLatch drops the alert's durable latch when the claw is busy
// again, re-arming the alert for the next idle stretch.
//
// It deliberately does NOT touch the resume latch. This runs on observing a
// turn in flight, and a reservation that aborts (abortTurnLocked, on a WS
// write failure) never becomes a finished turn; clearing the resume latch here
// would re-arm a stretch that never ended and poke it a second time. The
// resume re-arms itself in checkAgentIdleResume, from lastTurnFinishedAt
// actually moving.
func (s *Server) clearAgentIdleLatch(clawID string, cc *clawConn) {
	cc.mu.Lock()
	cc.idleNotifiedAt = time.Time{}
	cc.mu.Unlock()
	if _, err := s.db.Exec(`UPDATE claws SET idle_since=0 WHERE id=? AND idle_since != 0`, clawID); err != nil {
		log.Printf("[agent-idle] clear latch for claw %s: %v", shortID(clawID), err)
	}
}

// clearAgentIdleResumeLatch re-arms the auto-resume for the next idle stretch.
// The single caller is the "a turn actually finished" branch of
// checkAgentIdleResume; see clearAgentIdleLatch for why observing busy is not
// good enough.
//
// idle_resume_count is deliberately NOT reset. A resume that produces an empty
// turn still ends the stretch, so resetting the counter on every turn would
// make the lifetime cap unreachable in precisely the runaway case it exists to
// bound (wake, do nothing, idle, repeat).
func (s *Server) clearAgentIdleResumeLatch(clawID string) {
	if _, err := s.db.Exec(`UPDATE claws SET idle_resume_at=0 WHERE id=? AND idle_resume_at != 0`, clawID); err != nil {
		log.Printf("[agent-idle] clear resume latch for claw %s: %v", shortID(clawID), err)
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
