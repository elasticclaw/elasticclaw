package hub

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
)

// idleTestConn builds the in-memory connection state for a claw whose last
// turn finished `idleFor` ago, on a connection old enough that the
// connected-at floor never truncates the stretch.
func idleTestConn(clawID string, idleFor time.Duration) *clawConn {
	return &clawConn{
		id: clawID, tenantID: "test-tenant-id",
		connectedAt:        time.Now().Add(-idleFor - time.Hour),
		lastTurnFinishedAt: time.Now().Add(-idleFor),
	}
}

func clawIdleSince(t *testing.T, db *sql.DB, clawID string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow(`SELECT idle_since FROM claws WHERE id=?`, clawID).Scan(&v); err != nil {
		t.Fatalf("read idle_since for %s: %v", clawID, err)
	}
	return v
}

// setClawPipelineStage marks a claw as pipeline-driven, which is what makes an
// ad-hoc claw eligible for the agent_idle alert (an interactive claw with no
// automatic driver never alerts).
func setClawPipelineStage(t *testing.T, db *sql.DB, clawID, stage string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE claws SET pipeline_stage=? WHERE id=?`, stage, clawID); err != nil {
		t.Fatalf("set pipeline_stage for %s: %v", clawID, err)
	}
}

// stampAgentIdleBaseline pins the agent_idle baseline at an explicit moment
// and drops the in-memory cache so the next detection tick reads it.
func stampAgentIdleBaseline(t *testing.T, s *Server, at time.Time) {
	t.Helper()
	s.setNotifierStateInt64(agentIdleBaselineKey, at.UnixMilli())
	s.agentIdleBaselineMu.Lock()
	s.agentIdleBaselineAt = time.Time{}
	s.agentIdleBaselineMu.Unlock()
}

// backdateAgentIdleBaseline moves the agent_idle baseline a day into the past,
// simulating a hub where the feature has been enabled for a long time — so
// stretches provoked by the test count as post-enable and actually notify.
func backdateAgentIdleBaseline(t *testing.T, s *Server) {
	t.Helper()
	stampAgentIdleBaseline(t, s, time.Now().Add(-24*time.Hour))
}

// seedIdleTestBaseline marks everything that exists right now (including the
// just-inserted claws' agent_started state) as history, so the claw pass sends
// only what the test provokes afterwards.
func seedIdleTestBaseline(t *testing.T, s *Server) {
	t.Helper()
	setLifecycleClawBaseline(t, s)
	if err := s.seedLifecycleClawBaseline(); err != nil {
		t.Fatalf("seed claw baseline: %v", err)
	}
}

func agentIdleEventCount(t *testing.T, db *sql.DB, runID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type=?`, runID, taskRunEventAgentIdle).Scan(&n); err != nil {
		t.Fatalf("count agent_idle events: %v", err)
	}
	return n
}

// An ad-hoc pipeline-driven claw past the threshold notifies exactly once per
// idle stretch, survives a simulated hub restart without re-notifying, and
// re-arms after a turn so a second stretch notifies again.
func TestAgentIdleAdhocNotifiesOncePerStretchAndRearms(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	const clawID = "idle-adhoc"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)
	d := testLifecycleDelivery(t, s)

	cc := idleTestConn(clawID, 10*time.Minute)
	s.checkAgentIdle(time.Now(), clawID, cc)
	firstLatch := clawIdleSince(t, db, clawID)
	if firstLatch == 0 {
		t.Fatal("idle claw past threshold did not latch idle_since")
	}
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("sent %d messages, want 1", fake.count())
	}
	if !strings.Contains(fake.request(0).Fallback, "Agent stalled") {
		t.Fatalf("unexpected message: %q", fake.request(0).Fallback)
	}

	// Same stretch again: no second latch, no second send.
	s.checkAgentIdle(time.Now(), clawID, cc)
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("same stretch re-notified: %d messages", fake.count())
	}

	// Simulated hub restart 6 minutes ago: a fresh clawConn loses
	// idleNotifiedAt, gets a fresh connectedAt, and the restored
	// lastTurnFinishedAt drifts by a couple of seconds. Once the post-restart
	// stretch passes the threshold, the durable latch must recognize the
	// stretch by its anchor and stay silent.
	restarted := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt:        time.Now().Add(-6 * time.Minute),
		lastTurnFinishedAt: cc.lastTurnFinishedAt.Add(2 * time.Second)}
	s.checkAgentIdle(time.Now(), clawID, restarted)
	if got := clawIdleSince(t, db, clawID); got != firstLatch {
		t.Fatalf("restart re-latched the same stretch: %d -> %d", firstLatch, got)
	}
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("restart re-notified the same stretch: %d messages", fake.count())
	}
	if restarted.idleNotifiedAt.IsZero() {
		t.Fatal("restart path did not restore the in-memory fast path")
	}

	// A running turn clears the latch...
	cc.mu.Lock()
	cc.awaitingResponse = true
	cc.mu.Unlock()
	s.checkAgentIdle(time.Now(), clawID, cc)
	if got := clawIdleSince(t, db, clawID); got != 0 {
		t.Fatalf("busy claw kept idle latch %d", got)
	}
	// ...and once the turn ends and a NEW stretch passes the threshold, the
	// alert fires again with a fresh delivery key.
	cc.mu.Lock()
	cc.finishTurnLocked()
	cc.lastTurnFinishedAt = time.Now().Add(-6 * time.Minute)
	cc.mu.Unlock()
	s.checkAgentIdle(time.Now(), clawID, cc)
	if got := clawIdleSince(t, db, clawID); got == 0 || got == firstLatch {
		t.Fatalf("second stretch latch = %d (first %d), want a fresh latch", got, firstLatch)
	}
	s.lifecycleClawPass(d)
	if fake.count() != 2 {
		t.Fatalf("second idle stretch sent %d total messages, want 2", fake.count())
	}
}

// Regression: a claw that never ran a turn used to anchor its stretch on
// connectedAt, which is re-stamped on every bridge reconnect and hub restart —
// so after any reconnect the same unchanged stall looked like a brand-new
// stretch and re-alerted ~idle_after later; a flaky bridge on a
// permanently-stalled claw produced duplicate alerts forever. A never-turn
// claw now anchors on claws.created_at, which never moves, so the durable
// latch recognizes the stall across reconnects (and the latch is cleared the
// moment a turn runs, re-arming genuinely new stretches).
func TestAgentIdleNeverTurnClawDoesNotReAlertOnReconnect(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	const clawID = "idle-noturn"
	// Created an hour ago: created_at must predate every connection, as it
	// does in production (a claw exists before its bridge first registers).
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", time.Hour)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)
	d := testLifecycleDelivery(t, s)

	// Never ran a turn: the anchor falls back to connectedAt.
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt: time.Now().Add(-10 * time.Minute)}
	s.checkAgentIdle(time.Now(), clawID, cc)
	firstLatch := clawIdleSince(t, db, clawID)
	if firstLatch == 0 {
		t.Fatal("never-turn claw past threshold did not latch")
	}
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("sent %d messages, want 1", fake.count())
	}

	// Bridge reconnects (or the hub restarts): the fresh clawConn gets a new
	// connectedAt far past the persisted latch, still no turn ever. Once the
	// post-reconnect stretch passes the threshold, the same stall must NOT
	// re-latch or re-alert.
	reconnected := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt: time.Now().Add(-6 * time.Minute)}
	s.checkAgentIdle(time.Now(), clawID, reconnected)
	if got := clawIdleSince(t, db, clawID); got != firstLatch {
		t.Fatalf("reconnect re-latched the same never-turn stall: %d -> %d", firstLatch, got)
	}
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("reconnect re-alerted the same never-turn stall: %d messages", fake.count())
	}
	if reconnected.idleNotifiedAt.IsZero() {
		t.Fatal("reconnect path did not restore the in-memory fast path")
	}

	// Once a turn actually runs, the latch clears and a genuinely new stretch
	// alerts again.
	reconnected.mu.Lock()
	reconnected.awaitingResponse = true
	reconnected.mu.Unlock()
	s.checkAgentIdle(time.Now(), clawID, reconnected)
	if got := clawIdleSince(t, db, clawID); got != 0 {
		t.Fatalf("busy claw kept idle latch %d", got)
	}
	reconnected.mu.Lock()
	reconnected.finishTurnLocked()
	reconnected.lastTurnFinishedAt = time.Now().Add(-6 * time.Minute)
	reconnected.mu.Unlock()
	s.checkAgentIdle(time.Now(), clawID, reconnected)
	if got := clawIdleSince(t, db, clawID); got == 0 || got == firstLatch {
		t.Fatalf("post-turn stretch latch = %d (first %d), want a fresh latch", got, firstLatch)
	}
	s.lifecycleClawPass(d)
	if fake.count() != 2 {
		t.Fatalf("new stretch after a real turn sent %d total messages, want 2", fake.count())
	}
}

// The stable-anchor trade, asserted deliberately: a never-turn stall that
// began before the notification baseline is parked once and stays parked
// across bridge reconnects. connectedAt moves on every registration, but the
// claw's actual situation — created, connected, never prompted — has not
// changed, so a reconnect must not promote the parked pre-baseline stall into
// a fresh alertable stretch (the "first enable never replays history" rule
// applied consistently). Before the created_at anchor, a reconnect made the
// same stall look new and either re-alerted it or re-parked it forever.
func TestAgentIdlePreBaselineNeverTurnStallStaysParkedAcrossReconnect(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	const clawID = "idle-prebaseline-noturn"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", 3*time.Hour)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	// The feature was enabled 30 minutes ago; the claw has been connected and
	// never prompted since well before that.
	stampAgentIdleBaseline(t, s, time.Now().Add(-30*time.Minute))
	setSlackWatermark(t, s, 0)
	d := testLifecycleDelivery(t, s)

	cc := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt: time.Now().Add(-2 * time.Hour)}
	s.checkAgentIdle(time.Now(), clawID, cc)
	firstLatch := clawIdleSince(t, db, clawID)
	if firstLatch == 0 {
		t.Fatal("pre-baseline never-turn stall was not parked")
	}
	s.lifecycleClawPass(d)
	if fake.count() != 0 {
		t.Fatalf("pre-baseline stall was announced: %d messages", fake.count())
	}

	// Bridge reconnects: connectedAt is re-stamped after the baseline, but the
	// claw is the same never-prompted stall that predates it — still silent.
	reconnected := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt: time.Now().Add(-10 * time.Minute)}
	s.checkAgentIdle(time.Now(), clawID, reconnected)
	if got := clawIdleSince(t, db, clawID); got != firstLatch {
		t.Fatalf("reconnect re-latched a parked pre-baseline stall: %d -> %d", firstLatch, got)
	}
	s.lifecycleClawPass(d)
	if fake.count() != 0 {
		t.Fatalf("reconnect replayed a parked pre-baseline stall: %d messages", fake.count())
	}
}

// The exclusion rule: idle-but-done and idle-but-human-paced agents never
// alert. Covers protected and terminal claw statuses, delivered-PR waiting
// states for both claw kinds, interactive ad-hoc claws with no automatic
// driver, never-prompted claws, and connections too fresh to have stalled.
func TestAgentIdleExclusions(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setLifecycleClawBaseline(t, s)
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)

	base := int64(1760000000000)
	newRun := func(runID, clawID, phase string) {
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
			RunID: runID, AttemptID: "attempt-" + runID, ClawID: clawID, TenantID: "test-tenant-id",
			OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: phase, StartedAt: base,
		})
	}

	cases := []struct {
		name         string
		status       string
		runID        string // "" = ad-hoc
		phase        string
		withPR       bool   // claw_prs row exists (both kinds — finding: PR delivered but run association lost)
		pipeline     bool   // ad-hoc: pipeline_stage set
		workflow     string // ad-hoc: workflow_runs row with this status
		busy         bool
		noTurn       bool
		connectedAgo time.Duration // zero = default (well before the stretch)
		eligible     bool
	}{
		{name: "adhoc-pipeline-stalled", status: "connected", pipeline: true, eligible: true},
		{name: "adhoc-workflow-stalled", status: "connected", workflow: "running", eligible: true},
		// An interactive claw is prompted by nothing but its human: every
		// pause would alert, so no automatic driver means no alert.
		{name: "adhoc-interactive-human-paced", status: "connected"},
		{name: "adhoc-workflow-finished", status: "connected", workflow: "completed"},
		{name: "adhoc-with-open-pr", status: "connected", pipeline: true, withPR: true},
		{name: "protected-idle", status: "idle", pipeline: true},
		{name: "protected-completed", status: "completed", pipeline: true},
		{name: "protected-deleted", status: "deleted", pipeline: true},
		{name: "terminal-error", status: "error", pipeline: true},
		{name: "busy-turn", status: "connected", pipeline: true, busy: true},
		// Never prompted at all is the most common real stall: the stretch
		// starts at registration.
		{name: "never-ran-a-turn-stalled", status: "connected", runID: "run-noturn", phase: taskRunPhaseAgentRunning, noTurn: true, eligible: true},
		{name: "never-ran-a-turn-fresh-connection", status: "connected", runID: "run-noturn2", phase: taskRunPhaseAgentRunning, noTurn: true, connectedAgo: 2 * time.Minute},
		// A claw whose bridge just reconnected cannot have stalled during its
		// offline window: the stretch is floored at connection time.
		{name: "reconnected-after-offline-gap", status: "connected", runID: "run-reconnect", phase: taskRunPhaseAgentRunning, connectedAgo: 2 * time.Minute},
		{name: "run-agent-running-stalled", status: "connected", runID: "run-working", phase: taskRunPhaseAgentRunning, eligible: true},
		// A tracked claw_prs row means the PR was delivered even when the run
		// association failed and the phase never left agent_running.
		{name: "run-with-tracked-pr", status: "connected", runID: "run-trackedpr", phase: taskRunPhaseAgentRunning, withPR: true},
		{name: "run-pr-opened", status: "connected", runID: "run-propen", phase: taskRunPhasePROpened},
		{name: "run-waiting-for-merge", status: "connected", runID: "run-merge", phase: taskRunPhaseWaitingForMerge},
		{name: "run-terminal", status: "connected", runID: "run-done", phase: taskRunPhaseTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clawID := "claw-" + tc.name
			insertSlackTestClaw(t, db, clawID, tc.status, 0, tc.runID, oldEnough)
			if tc.runID != "" {
				newRun(tc.runID, clawID, tc.phase)
			}
			if tc.withPR {
				insertSlackTestClawPR(t, db, "pr-"+clawID, clawID, "acme/app", 7, "https://github.com/acme/app/pull/7")
			}
			if tc.pipeline {
				setClawPipelineStage(t, db, clawID, "implement")
			}
			if tc.workflow != "" {
				insertSlackTestWorkflowRun(t, db, "wf-"+clawID, clawID, tc.workflow, time.Now().UTC().Add(-time.Hour))
			}
			cc := idleTestConn(clawID, 10*time.Minute)
			if tc.busy {
				cc.awaitingResponse = true
			}
			if tc.noTurn {
				cc.lastTurnFinishedAt = time.Time{}
			}
			if tc.connectedAgo != 0 {
				cc.connectedAt = time.Now().Add(-tc.connectedAgo)
			}
			s.checkAgentIdle(time.Now(), clawID, cc)

			latched := clawIdleSince(t, db, clawID) != 0
			if latched != tc.eligible {
				t.Fatalf("latched=%v, want %v", latched, tc.eligible)
			}
			if tc.runID != "" {
				wantEvents := 0
				if tc.eligible {
					wantEvents = 1
				}
				if got := agentIdleEventCount(t, db, tc.runID); got != wantEvents {
					t.Fatalf("agent_idle events = %d, want %d", got, wantEvents)
				}
			}
		})
	}
}

// The threshold is configurable via idle_after and honoured by detection.
func TestAgentIdleThresholdConfigurable(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(lc *types.LifecycleNotificationsConfig) {
		lc.IdleAfter = "8m"
	})
	const clawID = "idle-threshold"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	backdateAgentIdleBaseline(t, s)

	s.checkAgentIdle(time.Now(), clawID, idleTestConn(clawID, 6*time.Minute))
	if clawIdleSince(t, db, clawID) != 0 {
		t.Fatal("claw idle 6m latched with an 8m threshold")
	}
	s.checkAgentIdle(time.Now(), clawID, idleTestConn(clawID, 9*time.Minute))
	if clawIdleSince(t, db, clawID) == 0 {
		t.Fatal("claw idle 9m did not latch with an 8m threshold")
	}
}

func TestLifecycleIdleAfterDefaultsAndFloors(t *testing.T) {
	if got := lifecycleIdleAfter(nil); got != 5*time.Minute {
		t.Fatalf("default idle_after = %v, want 5m", got)
	}
	if got := lifecycleIdleAfter(&types.LifecycleNotificationsConfig{IdleAfter: "10m"}); got != 10*time.Minute {
		t.Fatalf("idle_after 10m = %v", got)
	}
	// Below the validation floor (or unparsable): fall back rather than honour.
	if got := lifecycleIdleAfter(&types.LifecycleNotificationsConfig{IdleAfter: "30s"}); got != 5*time.Minute {
		t.Fatalf("sub-minute idle_after honoured: %v", got)
	}
	if got := lifecycleIdleAfter(&types.LifecycleNotificationsConfig{IdleAfter: "bogus"}); got != 5*time.Minute {
		t.Fatalf("unparsable idle_after = %v, want default", got)
	}
}

// Exclusivity: a run-backed claw is notified by the task-run pass, an ad-hoc
// claw by the claw pass — and running both passes repeatedly never produces a
// second message for the same idle stretch of either claw.
func TestAgentIdleRunBackedAndAdhocExclusivity(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setLifecycleClawBaseline(t, s)
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)

	base := int64(1760000000000)
	insertSlackTestClaw(t, db, "claw-run", "connected", 0, "run-1", oldEnough)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-run", TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: taskRunPhaseAgentRunning, StartedAt: base,
	})
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, "claw-adhoc", "implement")
	seedIdleTestBaseline(t, s)

	s.checkAgentIdle(time.Now(), "claw-run", idleTestConn("claw-run", 10*time.Minute))
	s.checkAgentIdle(time.Now(), "claw-adhoc", idleTestConn("claw-adhoc", 10*time.Minute))

	if got := agentIdleEventCount(t, db, "run-1"); got != 1 {
		t.Fatalf("run-backed claw recorded %d agent_idle events, want 1", got)
	}
	var adhocEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE event_type=?`, taskRunEventAgentIdle).Scan(&adhocEvents); err != nil {
		t.Fatal(err)
	}
	if adhocEvents != 1 {
		t.Fatalf("ad-hoc claw leaked into task_run_events: %d total agent_idle events", adhocEvents)
	}

	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("sent %d messages, want exactly 2 (one per claw)", fake.count())
	}
	// Re-running both passes must not duplicate either delivery.
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("second tick duplicated deliveries: %d messages", fake.count())
	}
}

// agent_idle is a soft warning: it must not mark the run failed or advance its
// phase, no matter how often the run is re-materialized afterwards.
func TestAgentIdleDoesNotFailRunOrAdvancePhase(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	backdateAgentIdleBaseline(t, s)
	base := int64(1760000000000)
	insertSlackTestClaw(t, db, "claw-1", "connected", 0, "run-1", oldEnough)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: taskRunPhaseAgentRunning, StartedAt: base,
	})
	// Establish the working phase through the real event flow, then stall.
	if err := s.recordTaskRunEventForClaw("claw-1", TaskRunEvent{
		EventKey: "agent_started:run-1", Source: taskRunSourceHub, EventType: taskRunEventAgentStarted,
		ActorType: taskRunActorSystem, InteractionRole: taskRunInteractionNeutral, OccurredAt: now(),
	}); err != nil {
		t.Fatalf("record agent_started: %v", err)
	}
	s.checkAgentIdle(time.Now(), "claw-1", idleTestConn("claw-1", 10*time.Minute))
	if got := agentIdleEventCount(t, db, "run-1"); got != 1 {
		t.Fatalf("agent_idle events = %d, want 1", got)
	}

	var status, phase, failureType, warnings string
	if err := db.QueryRow(`SELECT status, phase, failure_type, warning_types FROM task_run_summaries WHERE run_id='run-1'`).
		Scan(&status, &phase, &failureType, &warnings); err != nil {
		t.Fatal(err)
	}
	if status != taskRunStatusRunning || phase != taskRunPhaseAgentRunning || failureType != "" {
		t.Fatalf("agent_idle changed the run: status=%q phase=%q failure=%q", status, phase, failureType)
	}
	if strings.Contains(warnings, taskRunEventAgentIdle) {
		t.Fatalf("agent_idle leaked into warning_types: %s", warnings)
	}
	var attemptStatus string
	if err := db.QueryRow(`SELECT status FROM task_run_attempts WHERE id='attempt-run-1'`).Scan(&attemptStatus); err != nil {
		t.Fatal(err)
	}
	if attemptStatus != "running" {
		t.Fatalf("agent_idle changed the attempt status to %q", attemptStatus)
	}
}

// The agent_idle toggle is honoured by both passes, and muting then re-enabling
// never replays the muted window.
func TestAgentIdleToggleParksBothPassesWithoutReplay(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(lc *types.LifecycleNotificationsConfig) {
		lc.Events = &types.LifecycleEventToggles{AgentIdle: boolPtr(false)}
	})
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)

	base := int64(1760000000000)
	insertSlackTestClaw(t, db, "claw-run", "connected", 0, "run-1", oldEnough)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-run", TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: taskRunPhaseAgentRunning, StartedAt: base,
	})
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, "claw-adhoc", "implement")
	seedIdleTestBaseline(t, s)

	// Detection still runs while the kind is muted; delivery is what parks.
	s.checkAgentIdle(time.Now(), "claw-run", idleTestConn("claw-run", 10*time.Minute))
	s.checkAgentIdle(time.Now(), "claw-adhoc", idleTestConn("claw-adhoc", 10*time.Minute))
	adhocLatch := clawIdleSince(t, db, "claw-adhoc")
	if adhocLatch == 0 {
		t.Fatal("muted toggle suppressed detection; parking has nothing to park")
	}

	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("muted agent_idle sent %d messages", fake.count())
	}
	// Both sources are parked as skipped.
	if status, ok := slackDeliveryStatus(t, db, lifecycleClawIdleKey("claw-adhoc", adhocLatch)); !ok || status != notificationDeliveryStatusSkipped {
		t.Fatalf("muted ad-hoc idle delivery = %q, %v; want skipped", status, ok)
	}
	var eventID string
	if err := db.QueryRow(`SELECT id FROM task_run_events WHERE run_id='run-1' AND event_type=?`, taskRunEventAgentIdle).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if status, ok := slackDeliveryStatus(t, db, eventID); !ok || status != notificationDeliveryStatusSkipped {
		t.Fatalf("muted run-backed idle delivery = %q, %v; want skipped", status, ok)
	}

	// Re-enable: the muted window stays muted.
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Events.AgentIdle = boolPtr(true)
	s.mu.Unlock()
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("re-enabling agent_idle replayed the muted window: %d messages", fake.count())
	}
}

// First enable (or first deploy of the feature): idle stretches that began
// before the baseline are parked — latched and marked skipped — never
// announced, for both the run-backed and the ad-hoc source. Only a stretch
// that begins after the baseline alerts.
func TestAgentIdleFirstEnableParksPreexistingStretches(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	// The baseline is deliberately NOT backdated: it was stamped at server
	// boot, so both claws' 10-minute-old stretches predate it — exactly a
	// first deploy over a hub with already-idle claws.
	base := int64(1760000000000)
	insertSlackTestClaw(t, db, "claw-run", "connected", 0, "run-1", oldEnough)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-run", TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: taskRunPhaseAgentRunning, StartedAt: base,
	})
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, "claw-adhoc", "implement")
	seedIdleTestBaseline(t, s)
	setSlackWatermark(t, s, 0)

	adhocConn := idleTestConn("claw-adhoc", 10*time.Minute)
	s.checkAgentIdle(time.Now(), "claw-run", idleTestConn("claw-run", 10*time.Minute))
	s.checkAgentIdle(time.Now(), "claw-adhoc", adhocConn)

	// Both stretches are remembered (latched)...
	if clawIdleSince(t, db, "claw-run") == 0 {
		t.Fatal("pre-baseline run-backed stretch was not latched")
	}
	adhocLatch := clawIdleSince(t, db, "claw-adhoc")
	if adhocLatch == 0 {
		t.Fatal("pre-baseline ad-hoc stretch was not latched")
	}
	// ...but nothing is delivered: no event row for the run-backed claw, a
	// skipped delivery row for the ad-hoc one, zero messages.
	if got := agentIdleEventCount(t, db, "run-1"); got != 0 {
		t.Fatalf("pre-baseline stretch wrote %d task_run_events rows, want 0", got)
	}
	if status, ok := slackDeliveryStatus(t, db, lifecycleClawIdleKey("claw-adhoc", adhocLatch)); !ok || status != notificationDeliveryStatusSkipped {
		t.Fatalf("pre-baseline ad-hoc delivery = %q, %v; want skipped", status, ok)
	}
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("first enable replayed pre-existing idle stretches: %d messages", fake.count())
	}
	// Re-detection of the same parked stretches stays silent too.
	s.checkAgentIdle(time.Now(), "claw-run", idleTestConn("claw-run", 12*time.Minute))
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("re-detection of a parked stretch notified: %d messages", fake.count())
	}

	// A stretch that begins AFTER the baseline alerts normally. (The baseline
	// is moved behind the new stretch because test time is compressed — in
	// production the new stretch simply starts later than the enable.)
	backdateAgentIdleBaseline(t, s)
	adhocConn.mu.Lock()
	adhocConn.finishTurnLocked()
	adhocConn.lastTurnFinishedAt = time.Now().Add(-6 * time.Minute)
	adhocConn.mu.Unlock()
	s.checkAgentIdle(time.Now(), "claw-adhoc", adhocConn)
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("post-baseline stretch sent %d messages, want 1", fake.count())
	}
}

// Disabling lifecycle notifications drops the agent_idle baseline, so idle
// stretches that begin inside the disabled window are parked — not announced —
// when the feature is re-enabled.
func TestAgentIdleDisabledWindowDoesNotReplayOnReenable(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, "claw-adhoc", "implement")
	seedIdleTestBaseline(t, s)
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)

	// Disable the whole lifecycle block; the claw then goes idle during the
	// disabled window. Detection writes nothing and drops the enable stamp.
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Enabled = boolPtr(false)
	s.mu.Unlock()
	cc := idleTestConn("claw-adhoc", 10*time.Minute)
	s.checkAgentIdle(time.Now(), "claw-adhoc", cc)
	if got := clawIdleSince(t, db, "claw-adhoc"); got != 0 {
		t.Fatalf("disabled detection latched idle_since=%d", got)
	}
	if _, found, err := s.notifierStateInt64(agentIdleBaselineKey); err != nil || found {
		t.Fatalf("disabled window kept the baseline stamp (found=%v err=%v)", found, err)
	}

	// Re-enable: a fresh baseline is stamped, so the stretch that began
	// entirely inside the disabled window is parked, not announced.
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Enabled = nil
	s.mu.Unlock()
	s.checkAgentIdle(time.Now(), "claw-adhoc", cc)
	latch := clawIdleSince(t, db, "claw-adhoc")
	if latch == 0 {
		t.Fatal("re-enable did not park the disabled-window stretch")
	}
	if status, ok := slackDeliveryStatus(t, db, lifecycleClawIdleKey("claw-adhoc", latch)); !ok || status != notificationDeliveryStatusSkipped {
		t.Fatalf("disabled-window stretch delivery = %q, %v; want skipped", status, ok)
	}
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("re-enable replayed the disabled window: %d messages", fake.count())
	}
}

// closedTestWSConn returns a websocket connection whose writes always fail,
// standing in for a wedged/half-open bridge socket.
func closedTestWSConn(t *testing.T) *websocket.Conn {
	t.Helper()
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = c.Close(websocket.StatusNormalClosure, "test closing")
	}))
	t.Cleanup(wsServer.Close)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "closed before delivery")
	return conn
}

// A failed prompt delivery must release the turn reservation WITHOUT touching
// the idle clock: no turn ran. Otherwise a claw with a wedged socket and one
// pending message would reset lastTurnFinishedAt on every watchdog tick and
// never cross the agent_idle threshold — the exact stuck state the alert
// exists for.
func TestFailedDeliveryDoesNotRewindIdleClock(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	insertSlackTestClaw(t, db, "claw-wedged", "connected", 0, "", oldEnough)
	if _, err := db.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES('m-pending','claw-wedged','test-tenant-id','user','hello',?)`, now()); err != nil {
		t.Fatalf("insert pending message: %v", err)
	}

	lastTurn := time.Now().Add(-10 * time.Minute)
	cc := &clawConn{id: "claw-wedged", tenantID: "test-tenant-id", conn: closedTestWSConn(t),
		connectedAt: lastTurn.Add(-time.Hour), lastTurnFinishedAt: lastTurn}
	s.sendNextQueuedMessage(cc)

	cc.mu.RLock()
	got := cc.lastTurnFinishedAt
	busy := cc.isBusyLocked()
	cc.mu.RUnlock()
	if busy {
		t.Fatal("failed delivery left the turn reserved")
	}
	if !got.Equal(lastTurn) {
		t.Fatalf("failed delivery rewound the idle clock: %v -> %v", lastTurn, got)
	}
	var delivered sql.NullString
	if err := db.QueryRow(`SELECT delivered_at FROM messages WHERE id='m-pending'`).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.Valid {
		t.Fatal("failed delivery marked the message delivered")
	}
}

// The manual test endpoint renders agent_idle like the other types.
func TestSlackTestEndpointAgentIdleDryRun(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)
	rec := postSlackTest(t, s, `{"event_type":"agent_idle","dry_run":true}`)
	if rec.Code != 200 {
		t.Fatalf("dry-run agent_idle status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Agent stalled") || !strings.Contains(body, "9 minutes") {
		t.Fatalf("dry-run payload missing idle rendering: %s", body)
	}
	if fake.count() != 0 {
		t.Fatal("dry run must not send")
	}
}
