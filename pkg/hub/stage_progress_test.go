package hub

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func clawStageStalledSince(t *testing.T, db *sql.DB, clawID string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow(`SELECT stage_stalled_since FROM claws WHERE id=?`, clawID).Scan(&v); err != nil {
		t.Fatalf("read stage_stalled_since for %s: %v", clawID, err)
	}
	return v
}

func backdateStageProgressBaseline(t *testing.T, s *Server) {
	t.Helper()
	s.setNotifierStateInt64(stageProgressBaselineKey, time.Now().Add(-24*time.Hour).UnixMilli())
	s.stageProgressBaselineMu.Lock()
	s.stageProgressBaselineAt = time.Time{}
	s.stageProgressBaselineMu.Unlock()
}

// Disabled by default: with no stage_progress_after configured, a
// long-stalled ad-hoc claw must not latch or notify.
func TestStageProgressDisabledByDefaultDoesNothing(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	const clawID = "stage-disabled"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")

	cc := idleTestConn(clawID, 2*time.Hour)
	s.checkStageProgress(time.Now(), clawID, cc)
	if got := clawStageStalledSince(t, db, clawID); got != 0 {
		t.Fatalf("disabled stage-progress alert latched: %d", got)
	}
}

// Enabling the alert for the first time must park every already-stale stage
// instead of flooding on the first check.
func TestStageProgressFirstEnableParksPreexistingStretches(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(lc *types.LifecycleNotificationsConfig) {
		lc.StageProgressAfter = "5m"
	})
	const clawID = "stage-preexisting"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	setSlackWatermark(t, s, 0)
	d := testLifecycleDelivery(t, s)

	// Stage was already stalled for an hour before the feature was ever
	// enabled: the baseline parks it (latching it silently), so it must not
	// notify.
	cc := idleTestConn(clawID, time.Hour)
	s.checkStageProgress(time.Now(), clawID, cc)
	if got := clawStageStalledSince(t, db, clawID); got == 0 {
		t.Fatal("pre-existing stale stage was not parked")
	}
	s.lifecycleClawPass(d)
	if fake.count() != 0 {
		t.Fatalf("pre-existing stale stage notified on first enable: %d messages", fake.count())
	}
}

// A stall that begins after the baseline notifies exactly once per episode,
// and heartbeat-only activity (no clock update) does not re-arm it.
func TestStageProgressNotifiesOncePerStallEpisode(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(lc *types.LifecycleNotificationsConfig) {
		lc.StageProgressAfter = "5m"
	})
	const clawID = "stage-stalls"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	backdateStageProgressBaseline(t, s)
	setSlackWatermark(t, s, 0)
	d := testLifecycleDelivery(t, s)

	cc := idleTestConn(clawID, 10*time.Minute)
	now := time.Now()
	s.checkStageProgress(now, clawID, cc)
	first := clawStageStalledSince(t, db, clawID)
	if first == 0 {
		t.Fatal("stalled stage past threshold did not latch")
	}
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("sent %d messages, want 1", fake.count())
	}
	if !strings.Contains(fake.request(0).Fallback, "stalled") {
		t.Fatalf("unexpected message: %q", fake.request(0).Fallback)
	}

	// Re-checking the same stretch (no new progress) must not re-latch or
	// re-notify.
	s.checkStageProgress(now.Add(time.Minute), clawID, cc)
	if got := clawStageStalledSince(t, db, clawID); got != first {
		t.Fatalf("re-check re-latched: %d -> %d", first, got)
	}
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("same stall episode re-notified: %d messages", fake.count())
	}

	// Meaningful progress (a finished turn) re-arms the latch, and once the
	// new stretch itself passes the threshold it notifies again.
	cc.mu.Lock()
	cc.lastTurnFinishedAt = now.Add(-6 * time.Minute)
	cc.mu.Unlock()
	s.checkStageProgress(now, clawID, cc)
	if got := clawStageStalledSince(t, db, clawID); got == 0 || got == first {
		t.Fatalf("second stretch latch = %d (first %d), want a fresh latch", got, first)
	}
	s.lifecycleClawPass(d)
	if fake.count() != 2 {
		t.Fatalf("second stall episode sent %d total messages, want 2", fake.count())
	}
}

// Active subagent work suppresses the alert even past the threshold.
func TestStageProgressSuppressedWhileSubagentsActive(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(lc *types.LifecycleNotificationsConfig) {
		lc.StageProgressAfter = "5m"
	})
	const clawID = "stage-subagents"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	backdateStageProgressBaseline(t, s)

	cc := idleTestConn(clawID, 10*time.Minute)
	cc.mu.Lock()
	cc.subagentsActiveAt = time.Now()
	cc.mu.Unlock()
	s.checkStageProgress(time.Now(), clawID, cc)
	if got := clawStageStalledSince(t, db, clawID); got != 0 {
		t.Fatalf("subagent-active stage latched: %d", got)
	}
}

// The stage_progress_after floor rejects sub-minute durations, matching
// idle_after.
func TestLifecycleStageProgressAfterFloor(t *testing.T) {
	cfg := &types.NotificationsConfig{Lifecycle: &types.LifecycleNotificationsConfig{
		Via: testNotifierName, StageProgressAfter: "30s",
	}}
	if err := types.ValidateNotificationsConfig(cfg); err == nil {
		t.Fatal("expected sub-minute stage_progress_after to be rejected")
	}
}
