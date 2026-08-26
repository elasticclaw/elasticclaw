package hub

import (
	"bytes"
	"database/sql"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newIdleResumeTestServer builds a hub with NO notifications configured at
// all. That is the point: auto-resume is recovery, not an alert, and every
// test in this file must pass on a hub where nobody is listening to Slack.
//
// The resume baseline is backdated, i.e. these servers model a hub where the
// feature has been on for a long time. Baseline behaviour itself is covered by
// TestAgentIdleResumeParksStretchesOlderThanBaseline, which deliberately does
// not backdate.
func newIdleResumeTestServer(t *testing.T, liveness *types.LivenessConfig) (*Server, *sql.DB) {
	t.Helper()
	s, db := newIdleResumeTestServerAtEnable(t, liveness)
	backdateAgentIdleResumeBaseline(t, s)
	return s, db
}

// newIdleResumeTestServerAtEnable is the same hub with no baseline stamped
// yet: the next resume tick is the first one the feature ever ran.
func newIdleResumeTestServerAtEnable(t *testing.T, liveness *types.LivenessConfig) (*Server, *sql.DB) {
	t.Helper()
	return NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", Liveness: liveness}, "", "", "")
}

// backdateAgentIdleResumeBaseline moves the auto-resume baseline a day into
// the past and drops the in-memory cache, so stretches the test provokes count
// as post-enable and are actually resumed instead of parked.
func backdateAgentIdleResumeBaseline(t *testing.T, s *Server) {
	t.Helper()
	s.setNotifierStateInt64(agentIdleResumeBaselineKey, time.Now().Add(-24*time.Hour).UnixMilli())
	s.agentIdleResumeBaselineMu.Lock()
	s.agentIdleResumeBaselineAt = time.Time{}
	s.agentIdleResumeBaselineCleared = false
	s.agentIdleResumeBaselineMu.Unlock()
}

// idleResumeMessages counts the resume prompts injected into a claw's
// conversation.
func idleResumeMessages(t *testing.T, db *sql.DB, clawID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ?`, clawID, agentIdleResumePrefix+"%").Scan(&n); err != nil {
		t.Fatalf("count resume messages for %s: %v", clawID, err)
	}
	return n
}

func clawIdleResumeState(t *testing.T, db *sql.DB, clawID string) (at, count int64) {
	t.Helper()
	if err := db.QueryRow(`SELECT idle_resume_at, idle_resume_count FROM claws WHERE id=?`, clawID).Scan(&at, &count); err != nil {
		t.Fatalf("read idle_resume state for %s: %v", clawID, err)
	}
	return at, count
}

// The regression that matters most (NEXT-713): the hub notified about the
// stall and did nothing else. Auto-resume must fire on a hub with lifecycle
// notifications entirely absent — muting the alert channel must never disable
// recovery — exactly once per idle stretch, and re-arm once a turn has run.
func TestAgentIdleResumeFiresWithoutNotificationsOncePerStretchAndRearms(t *testing.T) {
	s, db := newIdleResumeTestServer(t, nil)
	const clawID = "resume-adhoc"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	if cfg := s.notificationsConfig(); cfg != nil {
		t.Fatal("test server unexpectedly has notifications configured")
	}

	cc := idleTestConn(clawID, 15*time.Minute)
	s.checkAgentIdleResume(time.Now(), clawID, cc)
	at, count := clawIdleResumeState(t, db, clawID)
	if at == 0 || count != 1 {
		t.Fatalf("first resume: idle_resume_at=%d count=%d, want a latch and count 1", at, count)
	}
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("injected %d resume messages, want 1", got)
	}

	// Same stretch, later tick: latched, so nothing more is sent.
	s.checkAgentIdleResume(time.Now(), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("same stretch resumed twice: %d messages", got)
	}
	if _, count := clawIdleResumeState(t, db, clawID); count != 1 {
		t.Fatalf("same stretch bumped the attempt counter to %d", count)
	}

	// A hub restart must not re-poke the stretch: the fresh clawConn loses all
	// in-memory state and lastTurnFinishedAt is re-seeded with a little drift.
	restarted := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt:        time.Now().Add(-12 * time.Minute),
		lastTurnFinishedAt: cc.lastTurnFinishedAt.Add(2 * time.Second),
		// A turn ended on the new connection, so the blind-window guard is
		// lifted and this exercises the durable latch, not the guard.
		turnBoundarySeen: true}
	s.checkAgentIdleResume(time.Now(), clawID, restarted)
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("restart re-resumed the same stretch: %d messages", got)
	}

	// A tick landing while a turn RESERVATION is in flight must not re-arm the
	// resume: abortTurnLocked (a failed WS delivery) releases the reservation
	// without ever moving lastTurnFinishedAt, so the stretch is unchanged and
	// clearing the latch here would resume it a second time. The embedded
	// minute count differs between the two prompts, so injectMessage's
	// identical-pending dedupe would not catch that.
	latchedAt, _ := clawIdleResumeState(t, db, clawID)
	cc.mu.Lock()
	cc.awaitingResponse = true
	cc.mu.Unlock()
	s.checkAgentIdleResume(time.Now(), clawID, cc)
	cc.mu.Lock()
	cc.abortTurnLocked()
	cc.mu.Unlock()
	s.checkAgentIdleResume(time.Now().Add(time.Minute), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("an aborted delivery re-resumed the same stretch: %d messages", got)
	}
	if at, _ := clawIdleResumeState(t, db, clawID); at != latchedAt {
		t.Fatalf("busy tick moved the resume latch to %d, want it unchanged at %d", at, latchedAt)
	}

	// A turn actually runs and finishes, and the claw stalls again: a
	// genuinely new stretch earns a second resume — and the latch re-arms even
	// though checkAgentIdle never runs on this notification-less hub.
	cc.mu.Lock()
	cc.finishTurnLocked()
	cc.lastTurnFinishedAt = time.Now().Add(-11 * time.Minute)
	cc.mu.Unlock()
	s.checkAgentIdleResume(time.Now(), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 2 {
		t.Fatalf("new stretch injected %d resume messages in total, want 2", got)
	}
	if _, count := clawIdleResumeState(t, db, clawID); count != 2 {
		t.Fatalf("attempt counter = %d after two resumes", count)
	}
}

// The resume inherits the alert's eligibility rule and adds the two states in
// which poking is actively wrong: a turn already in flight, and a claw the
// no-progress watchdog has deliberately stopped.
func TestAgentIdleResumeExclusions(t *testing.T) {
	base := int64(1760000000000)
	cases := []struct {
		name             string
		status           string
		runID            string // "" = ad-hoc
		phase            string
		withPR           bool
		pipeline         bool
		busy             bool
		noProgressPaused bool
		idleFor          time.Duration // zero = well past the threshold
		disabled         bool
		want             bool
	}{
		{name: "adhoc-pipeline-stalled", status: "connected", pipeline: true, want: true},
		{name: "run-agent-running-stalled", status: "connected", runID: "run-working", phase: taskRunPhaseAgentRunning, want: true},
		// Still inside the threshold: the alert may already have fired at 5m,
		// the resume waits for its own, longer escalation window.
		{name: "below-threshold", status: "connected", pipeline: true, idleFor: 6 * time.Minute},
		{name: "busy-turn", status: "connected", pipeline: true, busy: true},
		// The no-progress watchdog stopped automatic continuation on purpose;
		// resuming here would restart the loop it just broke.
		{name: "no-progress-paused", status: "connected", pipeline: true, noProgressPaused: true},
		// Delivered work waiting on a human is idle by definition.
		{name: "adhoc-with-open-pr", status: "connected", pipeline: true, withPR: true},
		{name: "run-pr-opened", status: "connected", runID: "run-propen", phase: taskRunPhasePROpened},
		{name: "run-waiting-for-merge", status: "connected", runID: "run-merge", phase: taskRunPhaseWaitingForMerge},
		{name: "run-terminal", status: "connected", runID: "run-done", phase: taskRunPhaseTerminal},
		// Nothing automatic drives an interactive claw, so a long pause is its
		// human thinking, not a stall.
		{name: "adhoc-interactive-human-paced", status: "connected"},
		{name: "not-connected", status: "error", pipeline: true},
		{name: "feature-disabled", status: "connected", pipeline: true, disabled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var liveness *types.LivenessConfig
			if tc.disabled {
				off := false
				liveness = &types.LivenessConfig{IdleResume: &off}
			}
			s, db := newIdleResumeTestServer(t, liveness)
			clawID := "claw-" + tc.name
			insertSlackTestClaw(t, db, clawID, tc.status, 0, tc.runID, oldEnough)
			if tc.runID != "" {
				insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
					RunID: tc.runID, AttemptID: "attempt-" + tc.runID, ClawID: clawID, TenantID: "test-tenant-id",
					OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: tc.phase, StartedAt: base,
				})
			}
			if tc.withPR {
				insertSlackTestClawPR(t, db, "pr-"+clawID, clawID, "acme/app", 7, "https://github.com/acme/app/pull/7")
			}
			if tc.pipeline {
				setClawPipelineStage(t, db, clawID, "implement")
			}
			idleFor := tc.idleFor
			if idleFor == 0 {
				idleFor = 15 * time.Minute
			}
			cc := idleTestConn(clawID, idleFor)
			cc.awaitingResponse = tc.busy
			cc.noProgressPaused = tc.noProgressPaused
			s.checkAgentIdleResume(time.Now(), clawID, cc)

			resumed := idleResumeMessages(t, db, clawID) > 0
			if resumed != tc.want {
				t.Fatalf("resumed=%v, want %v", resumed, tc.want)
			}
			if _, count := clawIdleResumeState(t, db, clawID); (count > 0) != tc.want {
				t.Fatalf("attempt counter = %d, want fired=%v", count, tc.want)
			}
		})
	}
}

// The runaway backstop: a claw that keeps waking, doing nothing, and idling
// again clears the per-stretch latch every time, so only the lifetime cap
// stops the poking. (The primary backstop is the no-progress watchdog, which
// pauses such a claw after three identical outcomes — see the exclusion test.)
func TestAgentIdleResumeStopsAtLifetimeCap(t *testing.T) {
	// Escalation is a distinct concern (TestAgentIdleResumeEscalatesAfterRepeatedFailures)
	// and is disabled here by pushing its threshold past the lifetime cap, so
	// this test can keep exercising the cap itself in isolation.
	beyondCap := agentIdleResumeMaxAttempts + 100
	s, db := newIdleResumeTestServer(t, &types.LivenessConfig{IdleResumeEscalateAfter: &beyondCap})
	const clawID = "resume-cap"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")

	for i := 0; i < agentIdleResumeMaxAttempts+3; i++ {
		// Each iteration is a genuinely distinct stretch: the claw woke, ran
		// an empty turn and stalled again, so lastTurnFinishedAt moves forward
		// every time and re-arms the per-stretch latch on its own. Only the
		// lifetime cap can stop the poking. The steps are wider than
		// agentIdleStretchSlack so no two stretches are mistaken for one.
		cc := idleTestConn(clawID, time.Hour-time.Duration(i)*2*time.Minute)
		s.checkAgentIdleResume(time.Now(), clawID, cc)
	}
	_, count := clawIdleResumeState(t, db, clawID)
	if count != agentIdleResumeMaxAttempts {
		t.Fatalf("attempt counter = %d, want the cap %d", count, agentIdleResumeMaxAttempts)
	}
	if got := idleResumeMessages(t, db, clawID); got != agentIdleResumeMaxAttempts {
		t.Fatalf("injected %d resume messages, want the cap %d", got, agentIdleResumeMaxAttempts)
	}
}

// TestAgentIdleResumeEscalatesAfterRepeatedFailures covers item 5 / finding
// A2: escalation must count CONSECUTIVE failed resumes inside ONE still-open
// stretch (a claw that never runs another turn after being poked), not a
// lifetime total. The claw connection is deliberately reused unchanged across
// every tick — lastTurnFinishedAt never moves — so this models exactly the
// NEXT-769 shape the plan describes: a fresh claw gets one resume, stays
// wedged, and (unlike the original, inverted implementation) must still be
// able to reach the threshold from repeat attempts against that same open
// stretch.
func TestAgentIdleResumeEscalatesAfterRepeatedFailures(t *testing.T) {
	threshold := 2
	idleAfter := "1m"
	s, db := newIdleResumeTestServer(t, &types.LivenessConfig{IdleResumeEscalateAfter: &threshold, IdleResumeAfter: idleAfter})
	const clawID = "resume-escalate"
	// bootstrap_ok=1 and a real provider/task-run so escalation's retry path
	// (escalateClawHealthFailure -> stopAgentWithReason) can actually act,
	// same prerequisites TestEscalateIdleResumeFailureSchedulesRetry uses.
	insertSlackTestClaw(t, db, clawID, "connected", 1, "", oldEnough)
	if _, err := db.Exec(`UPDATE claws SET provider='noop' WHERE id=?`, clawID); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	if _, _, err := s.ensureTaskRunForClaw(clawID, TaskRunStart{
		RunKind: taskRunKindPRTask, OwnerType: taskRunOwnerFactory, AnalyticsEnabled: true, RequiresPR: true, Tags: []string{"resume-escalate"},
	}); err != nil {
		t.Fatalf("ensure task run: %v", err)
	}
	setClawPipelineStage(t, db, clawID, "implement")

	// The SAME connection, the SAME stretch, on every tick: no turn ever
	// finishes, so this is one continuous wedge, not threshold-many distinct
	// stalls.
	cc := idleTestConn(clawID, 15*time.Minute)
	nowAt := time.Now()
	for i := 0; i < threshold; i++ {
		s.checkAgentIdleResume(nowAt, clawID, cc)
		nowAt = nowAt.Add(time.Minute)
	}
	if got := idleResumeMessages(t, db, clawID); got != threshold {
		t.Fatalf("injected %d resume messages before escalation, want %d", got, threshold)
	}

	// One more tick against the SAME unresolved stretch: this must escalate
	// instead of resuming again, leaving the message count exactly as it was.
	s.checkAgentIdleResume(nowAt, clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != threshold {
		t.Fatalf("injected %d resume messages after escalation, want unchanged %d", got, threshold)
	}
	// Escalation runs off the watchdog goroutine (M5: it must not block the
	// caller against a wedged checkpoint), so the status change lands
	// asynchronously; poll for it instead of reading immediately.
	status := waitForClawStatusChange(t, db, clawID, "connected")
	if status == "connected" {
		t.Fatalf("claw status=%q, want escalation to have moved it off connected", status)
	}
}

// waitForClawStatusChange polls until the claw's status differs from
// initial, or fails the test after a short deadline. Escalation paths that
// run off a goroutine (see escalateIdleResumeFailure) need this instead of a
// synchronous read.
func waitForClawStatusChange(t *testing.T, db *sql.DB, clawID, initial string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var status string
		if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
			t.Fatalf("read claw status: %v", err)
		}
		if status != initial || time.Now().After(deadline) {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestReplacementClawSurvivesInheritedStretchFailures is the regression for
// M6: resetClawForRetry must clear idle_resume_stretch_failures and
// idle_resume_last_attempt_at, or a replacement claw inherits its
// predecessor's failure count and can be torn down on its very first
// eligible tick having made zero resume attempts of its own -- exactly the
// false-positive class the plan names as the risk of this feature. This test
// would FAIL against the current branch tip, which never clears those
// columns on reset.
func TestReplacementClawSurvivesInheritedStretchFailures(t *testing.T) {
	threshold := 2
	idleAfter := "1m"
	s, db := newIdleResumeTestServer(t, &types.LivenessConfig{IdleResumeEscalateAfter: &threshold, IdleResumeAfter: idleAfter})
	const clawID = "resume-replace"
	insertSlackTestClaw(t, db, clawID, "error", 1, "", oldEnough)
	if _, err := db.Exec(`UPDATE claws SET provider='noop' WHERE id=?`, clawID); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	if _, _, err := s.ensureTaskRunForClaw(clawID, TaskRunStart{
		RunKind: taskRunKindPRTask, OwnerType: taskRunOwnerFactory, AnalyticsEnabled: true, RequiresPR: true, Tags: []string{"resume-replace"},
	}); err != nil {
		t.Fatalf("ensure task run: %v", err)
	}
	setClawPipelineStage(t, db, clawID, "implement")

	// Predecessor left the stretch counter at the escalation threshold, with
	// a stale last-attempt timestamp, right before it was replaced.
	if _, err := db.Exec(`UPDATE claws SET idle_resume_stretch_failures=?, idle_resume_last_attempt_at=? WHERE id=?`,
		threshold, time.Now().Add(-time.Hour).UnixMilli(), clawID); err != nil {
		t.Fatalf("seed inherited failures: %v", err)
	}

	reset, err := s.resetClawForRetry("test-tenant-id", clawID, "", "retrying")
	if err != nil || !reset {
		t.Fatalf("reset claw for retry: reset=%v err=%v", reset, err)
	}
	// The replacement reconnects.
	if _, err := db.Exec(`UPDATE claws SET status='connected' WHERE id=?`, clawID); err != nil {
		t.Fatalf("reconnect replacement: %v", err)
	}

	var stretchFailures, lastAttemptAt int64
	if err := db.QueryRow(`SELECT idle_resume_stretch_failures, idle_resume_last_attempt_at FROM claws WHERE id=?`, clawID).
		Scan(&stretchFailures, &lastAttemptAt); err != nil {
		t.Fatalf("read reset claw: %v", err)
	}
	if stretchFailures != 0 || lastAttemptAt != 0 {
		t.Fatalf("resetClawForRetry left stretch_failures=%d last_attempt_at=%d, want both cleared to 0", stretchFailures, lastAttemptAt)
	}

	// The replacement's first eligible tick, having made zero resume
	// attempts of its own, must resume normally rather than escalate.
	cc := idleTestConn(clawID, 15*time.Minute)
	s.checkAgentIdleResume(time.Now(), clawID, cc)
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatalf("read claw status: %v", err)
	}
	if status != "connected" {
		t.Fatalf("replacement claw status=%q after its first tick, want it to survive as connected", status)
	}
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("replacement injected %d resume messages, want 1 normal resume", got)
	}
}

// TestAgentIdleResumeDoesNotEscalateAcrossHealthySeparateStalls is the
// regression for finding A2(a): idle_resume_count is a LIFETIME counter that
// is deliberately never reset (TestAgentIdleResumeStopsAtLifetimeCap), but the
// escalation counter must be per-stretch. Three (or more) separate stalls,
// each one actually resolved by its resume before the next begins, must never
// tear the claw down — only a single stretch that keeps failing to close may
// escalate. This test would FAIL against a lifetime-scoped counter (it would
// escalate on the threshold-th distinct, successful stall).
func TestAgentIdleResumeDoesNotEscalateAcrossHealthySeparateStalls(t *testing.T) {
	threshold := 2
	s, db := newIdleResumeTestServer(t, &types.LivenessConfig{IdleResumeEscalateAfter: &threshold})
	const clawID = "resume-healthy-repeats"
	insertSlackTestClaw(t, db, clawID, "connected", 1, "", oldEnough)
	if _, err := db.Exec(`UPDATE claws SET provider='noop' WHERE id=?`, clawID); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	setClawPipelineStage(t, db, clawID, "implement")

	// Five genuinely distinct stretches, each with lastTurnFinishedAt moved
	// forward — i.e. a real turn ran and closed the previous stretch — well
	// past the escalation threshold in count, and well past
	// agentIdleResumeMaxAttempts too so the lifetime cap cannot be the reason
	// nothing extra happens here.
	for i := 0; i < threshold+3; i++ {
		cc := idleTestConn(clawID, time.Hour-time.Duration(i)*2*time.Minute)
		s.checkAgentIdleResume(time.Now(), clawID, cc)
	}
	if got := idleResumeMessages(t, db, clawID); got != threshold+3 {
		t.Fatalf("injected %d resume messages across healthy stalls, want %d", got, threshold+3)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatalf("read claw status: %v", err)
	}
	if status != "connected" {
		t.Fatalf("claw status=%q, want still connected: healthy separate stalls must never escalate", status)
	}
}

// TestAgentIdleResumeNeverEscalatesAnIneligibleClaw is the regression for
// finding A3: escalation must be gated behind agentIdleEligible, not run
// before it. A claw parked in pr_opened awaiting a human merge is idle by
// design (see agentIdleEligible's own doc comment) and must never be torn
// down, no matter how large its per-stretch failure count already is — which
// covers both a claw that legitimately parked after failed resumes, and the
// "first tick after deploy" mass-escalation risk of a stale count left over
// from before this fix.
func TestAgentIdleResumeNeverEscalatesAnIneligibleClaw(t *testing.T) {
	threshold := 1
	s, db := newIdleResumeTestServer(t, &types.LivenessConfig{IdleResumeEscalateAfter: &threshold})
	const clawID = "resume-parked-pr"
	const runID = "run-parked-pr"
	insertSlackTestClaw(t, db, clawID, "connected", 1, runID, oldEnough)
	if _, err := db.Exec(`UPDATE claws SET provider='noop' WHERE id=?`, clawID); err != nil {
		t.Fatalf("set provider: %v", err)
	}
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: runID, AttemptID: "attempt-" + runID, ClawID: clawID, TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, Factory: "bugfix", Phase: taskRunPhasePROpened, StartedAt: 1760000000000,
	})

	cc := idleTestConn(clawID, 20*time.Minute)
	// Simulate a failure count already at (or past) the escalation threshold
	// for the CURRENT stretch, as if this claw had racked up failed resumes
	// before it ever reached pr_opened, or as a stale value from before this
	// fix landed.
	anchor := agentIdleStretchAnchor(cc.lastTurnFinishedAt, 0).UnixMilli()
	if _, err := db.Exec(`UPDATE claws SET idle_resume_at=?, idle_resume_stretch_failures=?, idle_resume_last_attempt_at=? WHERE id=?`,
		anchor, threshold+5, anchor, clawID); err != nil {
		t.Fatalf("seed stretch failure count: %v", err)
	}

	s.checkAgentIdleResume(time.Now(), clawID, cc)

	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatalf("read claw status: %v", err)
	}
	if status != "connected" {
		t.Fatalf("claw status=%q, want unchanged: a pr_opened claw parked on a human must never be escalated", status)
	}
	if got := idleResumeMessages(t, db, clawID); got != 0 {
		t.Fatalf("injected %d resume messages into an ineligible claw, want 0", got)
	}
}

// The first tick after this feature reaches an existing hub must not poke
// every long-idle claw in the database at once — including pipeline claws a
// human deliberately walked away from hours ago. A stretch that began before
// the resume baseline is parked: latched so it is never re-examined, never
// injected, and never charged against the lifetime cap.
func TestAgentIdleResumeParksStretchesOlderThanBaseline(t *testing.T) {
	s, db := newIdleResumeTestServerAtEnable(t, nil)
	const clawID = "resume-pre-existing"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")

	// A claw that has been stalled for three hours, on the very first tick the
	// feature ever runs: it is eligible in every other respect.
	cc := idleTestConn(clawID, 3*time.Hour)
	enabledAt := time.Now()
	s.checkAgentIdleResume(enabledAt, clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 0 {
		t.Fatalf("a stretch that predates the feature injected %d resume prompts, want 0", got)
	}
	at, count := clawIdleResumeState(t, db, clawID)
	if at == 0 {
		t.Fatal("pre-baseline stretch was not parked: it will be reconsidered on every future tick")
	}
	if count != 0 {
		t.Fatalf("parking consumed %d of the lifetime cap, want 0", count)
	}

	// Still parked on later ticks, and across a hub restart (fresh clawConn,
	// re-seeded lastTurnFinishedAt with a little drift).
	s.checkAgentIdleResume(enabledAt.Add(5*time.Minute), clawID, cc)
	restarted := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt:        time.Now().Add(-30 * time.Minute),
		lastTurnFinishedAt: cc.lastTurnFinishedAt.Add(2 * time.Second)}
	s.checkAgentIdleResume(enabledAt.Add(10*time.Minute), clawID, restarted)
	if got := idleResumeMessages(t, db, clawID); got != 0 {
		t.Fatalf("parked stretch was resumed later anyway: %d messages", got)
	}

	// A turn runs after the baseline and the claw stalls again. That stretch
	// began where the feature could see it, so it is recovered normally —
	// parking costs a genuinely stuck claw at most one stretch.
	cc.mu.Lock()
	cc.finishTurnLocked()
	cc.mu.Unlock()
	s.checkAgentIdleResume(enabledAt.Add(20*time.Minute), clawID, cc)
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("post-baseline stretch injected %d resume prompts, want 1", got)
	}
}

// The wiring, not the logic. checkClawStatus is the only production caller of
// checkAgentIdleResume, and every other test here calls the check directly —
// so without this one, deleting that call site leaves the suite green and
// ships a recovery path that never fires, which is precisely the incident this
// feature exists to prevent. This test drives the real watchdog pass against a
// registered, stalled, eligible claw.
func TestCheckClawStatusAutoResumesStalledClaw(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-idle-resume"
	_ = watchdogClaw(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', pipeline_stage='implement' WHERE id=?`, clawID); err != nil {
		t.Fatalf("make claw eligible: %v", err)
	}
	backdateAgentIdleResumeBaseline(t, s)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.connectedAt = time.Now().Add(-2 * time.Hour)
	cc.lastTurnFinishedAt = time.Now().Add(-30 * time.Minute)
	cc.mu.Unlock()

	s.checkClawStatus()

	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("the watchdog pass injected %d resume prompts into a stalled claw, want 1 — is checkAgentIdleResume still called from checkClawStatus?", got)
	}
}

// The message must stay hub-generic (the hub serves other factories) while
// carrying both instructions the incident needed: stop waiting on background
// work that never reported, and recover from the workspace instead of
// starting over.
func TestAgentIdleResumeMessageContent(t *testing.T) {
	msg := agentIdleResumeMessage(12 * time.Minute)
	for _, want := range []string{"12 minutes", "background or spawned work", "degradation path", "git log --oneline -15", "Do not start over"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("resume message missing %q: %s", want, msg)
		}
	}
}

func TestLivenessIdleResumeSettings(t *testing.T) {
	settings := func(l *types.LivenessConfig) livenessSettings {
		s, _ := newIdleResumeTestServer(t, l)
		return s.livenessSettings()
	}
	if got := settings(nil); !got.idleResumeEnabled || got.idleResumeAfter != defaultIdleResumeAfter {
		t.Fatalf("defaults: enabled=%v after=%v, want true/%v", got.idleResumeEnabled, got.idleResumeAfter, defaultIdleResumeAfter)
	}
	if got := settings(&types.LivenessConfig{IdleResumeAfter: "20m"}); got.idleResumeAfter != 20*time.Minute {
		t.Fatalf("idle_resume_after 20m = %v", got.idleResumeAfter)
	}
	// Below the floor or unparsable: clamped/ignored, never honoured.
	if got := settings(&types.LivenessConfig{IdleResumeAfter: "5s"}); got.idleResumeAfter != minIdleResumeAfter {
		t.Fatalf("sub-minute idle_resume_after honoured: %v", got.idleResumeAfter)
	}
	if got := settings(&types.LivenessConfig{IdleResumeAfter: "bogus"}); got.idleResumeAfter != defaultIdleResumeAfter {
		t.Fatalf("unparsable idle_resume_after = %v, want the default", got.idleResumeAfter)
	}
	off := false
	if got := settings(&types.LivenessConfig{IdleResume: &off}); got.idleResumeEnabled {
		t.Fatal("idle_resume: false did not disable auto-resume")
	}
}

// TestIdleResumeEscalateAfterAtOrAboveLifetimeCapWarns is the regression for
// L3: setting idle_resume_escalate_after at or above the lifetime auto-resume
// cap silently disables escalation (the lifetime cap always halts resumes
// first, so the stretch-failure counter can never reach the threshold), and
// an operator raising the knob "to be safe" deserves a startup warning rather
// than silence. This test would FAIL against the current branch tip, which
// accepts the value without any validation or log line.
func TestIdleResumeEscalateAfterAtOrAboveLifetimeCapWarns(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	tooHigh := agentIdleResumeMaxAttempts
	s, _ := newIdleResumeTestServer(t, &types.LivenessConfig{IdleResumeEscalateAfter: &tooHigh})
	s.livenessSettings()

	if !strings.Contains(logs.String(), "idle_resume_escalate_after") || !strings.Contains(logs.String(), "disabled") {
		t.Fatalf("expected a warning about idle_resume_escalate_after effectively disabling escalation, got log: %q", logs.String())
	}
}

// NEXT-724: a fresh connection carries no in-flight turn state, so a claw that
// is mid-turn on the sandbox looks idle to the hub. Injecting there is a
// session takeover, not a nudge — it aborted a 16-minute turn in production.
func TestAgentIdleResumeWaitsOutTheBlindWindow(t *testing.T) {
	s, db := newIdleResumeTestServer(t, nil)
	const clawID = "resume-blind"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")

	// Connected 20 minutes ago and idle 20 by the stretch clock — comfortably
	// past the resume threshold, so the ONLY thing that can hold the resume back
	// is the blind-window guard. Still inside the grace, and no turn has ended
	// where this connection could see it: exactly the post-restart shape.
	blind := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt:        time.Now().Add(-20 * time.Minute),
		lastTurnFinishedAt: time.Now().Add(-30 * time.Minute)}
	s.checkAgentIdleResume(time.Now(), clawID, blind)
	if got := idleResumeMessages(t, db, clawID); got != 0 {
		t.Fatalf("resumed inside the blind window: %d messages", got)
	}
	if at, count := clawIdleResumeState(t, db, clawID); at != 0 || count != 0 {
		t.Fatalf("blind window must not latch or spend an attempt: at=%d count=%d", at, count)
	}

	// A turn ends where the hub can see it: turn tracking is live, guard lifts.
	blind.mu.Lock()
	blind.finishTurnLocked()
	blind.mu.Unlock()
	blind.lastTurnFinishedAt = time.Now().Add(-15 * time.Minute) // idle again
	s.checkAgentIdleResume(time.Now(), clawID, blind)
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("an observed turn boundary must lift the guard, got %d messages", got)
	}
}

// The guard is an upper bound, not a permanent veto: past the grace a claw that
// never produced a boundary is resumable again.
//
// This is a spec test, not a regression test: it passes with the guard deleted
// too. It guards against the guard becoming a permanent veto, which is the
// opposite failure from the one TestAgentIdleResumeWaitsOutTheBlindWindow
// covers.
func TestAgentIdleResumeFiresAfterTheBlindWindowElapses(t *testing.T) {
	s, db := newIdleResumeTestServer(t, nil)
	const clawID = "resume-blind-elapsed"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")

	old := &clawConn{id: clawID, tenantID: "test-tenant-id",
		connectedAt:        time.Now().Add(-agentIdleResumeBlindGrace - time.Minute),
		lastTurnFinishedAt: time.Now().Add(-15 * time.Minute)}
	s.checkAgentIdleResume(time.Now(), clawID, old)
	if got := idleResumeMessages(t, db, clawID); got != 1 {
		t.Fatalf("past the blind window the resume must fire, got %d messages", got)
	}
}

// An ordinary bridge reconnect must not re-blind the hub: the old connection
// knew turn tracking was live and had no reservation open, so the new one
// inherits that knowledge. Without this a claw whose bridge flaps more often
// than the grace would never be resumed at all.
func TestAgentIdleResumeCarriesTurnVisibilityAcrossReconnect(t *testing.T) {
	prev := &clawConn{id: "c", turnBoundarySeen: true}
	next := &clawConn{id: "c"}
	next.turnBoundarySeen = prev.turnBoundarySeen && !prev.isBusyLocked()
	if !next.turnBoundarySeen {
		t.Fatal("a reconnect from an idle, turn-aware connection must stay unblinded")
	}

	// But a connection that had a turn reservation open may have lost sight of
	// that turn, so the new one starts blind.
	busy := &clawConn{id: "c", turnBoundarySeen: true, awaitingResponse: true}
	after := &clawConn{id: "c"}
	after.turnBoundarySeen = busy.turnBoundarySeen && !busy.isBusyLocked()
	if after.turnBoundarySeen {
		t.Fatal("a reconnect from a busy connection must start blind")
	}
}
