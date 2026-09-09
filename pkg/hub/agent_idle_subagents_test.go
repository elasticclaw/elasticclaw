package hub

import (
	"context"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket/wsjson"
)

// A claw parked on sessions_yield looks exactly like a stalled one to turn
// state (no turn in flight, lastTurnFinishedAt old) while its spawned work is
// still running. While the bridge's subagent-activity report is fresh the
// alert must stay quiet and leave every latch untouched; once the report goes
// stale (bridge died or stopped reporting) the suppression lifts and the
// alert fires normally.
func TestAgentIdleSuppressedWhileSubagentsActive(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	const clawID = "idle-subagents"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)
	d := testLifecycleDelivery(t, s)

	cc := idleTestConn(clawID, 10*time.Minute)
	cc.subagentsActiveAt = time.Now()
	cc.subagentActiveCount = 2

	s.checkAgentIdle(time.Now(), clawID, cc)
	if got := clawIdleSince(t, db, clawID); got != 0 {
		t.Fatalf("fresh subagent activity latched idle_since=%d, want untouched (0)", got)
	}
	if !cc.idleNotifiedAt.IsZero() {
		t.Fatal("fresh subagent activity stamped idleNotifiedAt")
	}
	s.lifecycleClawPass(d)
	if fake.count() != 0 {
		t.Fatalf("sent %d messages while subagents active, want 0", fake.count())
	}

	// The report goes stale without a zero-subagents heartbeat ever arriving:
	// the claw may be genuinely stuck and the alert must not stay silenced.
	cc.mu.Lock()
	cc.subagentsActiveAt = time.Now().Add(-subagentsActiveFreshFor - time.Second)
	cc.mu.Unlock()
	s.checkAgentIdle(time.Now(), clawID, cc)
	if clawIdleSince(t, db, clawID) == 0 {
		t.Fatal("stale subagent state kept suppressing the alert")
	}
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("sent %d messages after the state went stale, want 1", fake.count())
	}
}

// A heartbeat reporting zero active subagents is positive evidence that the
// spawned work is gone: it clears the suppression and the alert fires on the
// next tick like it always did.
func TestAgentIdleZeroSubagentHeartbeatClearsSuppression(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	const clawID = "idle-subagents-zero"
	insertSlackTestClaw(t, db, clawID, "connected", 0, "", oldEnough)
	setClawPipelineStage(t, db, clawID, "implement")
	seedIdleTestBaseline(t, s)
	backdateAgentIdleBaseline(t, s)
	setSlackWatermark(t, s, 0)
	d := testLifecycleDelivery(t, s)

	cc := idleTestConn(clawID, 10*time.Minute)
	cc.subagentsActiveAt = time.Now()
	cc.subagentActiveCount = 1
	s.checkAgentIdle(time.Now(), clawID, cc)
	if got := clawIdleSince(t, db, clawID); got != 0 {
		t.Fatalf("suppressed tick latched idle_since=%d", got)
	}

	inactive, zero := false, 0
	cc.mu.Lock()
	cc.applySubagentHeartbeatLocked(&inactive, &zero)
	cc.mu.Unlock()
	if !cc.subagentsActiveAt.IsZero() || cc.subagentActiveCount != 0 {
		t.Fatalf("zero-subagents heartbeat did not clear state: at=%v count=%d", cc.subagentsActiveAt, cc.subagentActiveCount)
	}

	s.checkAgentIdle(time.Now(), clawID, cc)
	if clawIdleSince(t, db, clawID) == 0 {
		t.Fatal("alert did not fire after subagent state was cleared")
	}
	s.lifecycleClawPass(d)
	if fake.count() != 1 {
		t.Fatalf("sent %d messages, want 1", fake.count())
	}
}

// The heartbeat fields are optional precisely so an old bridge stays
// distinguishable from one reporting "no subagents": absent fields carry no
// information and must leave the state untouched in either direction.
func TestApplySubagentHeartbeatAbsentFieldsLeaveStateAlone(t *testing.T) {
	cc := &clawConn{}

	// Old bridge from the start: state stays zero, so agentIdleSnapshotOf
	// reports subagentsActive=false and behaviour is exactly pre-change.
	cc.applySubagentHeartbeatLocked(nil, nil)
	if !cc.subagentsActiveAt.IsZero() || cc.subagentActiveCount != 0 {
		t.Fatal("absent fields on a fresh connection touched the state")
	}
	if agentIdleSnapshotOf(cc).subagentsActive {
		t.Fatal("zero state reported subagentsActive")
	}

	active, three := true, 3
	cc.applySubagentHeartbeatLocked(&active, &three)
	if cc.subagentsActiveAt.IsZero() || cc.subagentActiveCount != 3 {
		t.Fatalf("active report not recorded: at=%v count=%d", cc.subagentsActiveAt, cc.subagentActiveCount)
	}
	if !agentIdleSnapshotOf(cc).subagentsActive {
		t.Fatal("fresh active state not reported by the snapshot")
	}

	// A later heartbeat without the fields (bridge poll failed mid-flight)
	// must not clear the recorded activity — only positive evidence may.
	recordedAt := cc.subagentsActiveAt
	cc.applySubagentHeartbeatLocked(nil, nil)
	if !cc.subagentsActiveAt.Equal(recordedAt) || cc.subagentActiveCount != 3 {
		t.Fatal("absent fields cleared previously recorded activity")
	}
}

// The suppression only works if the bridge's JSON keys and the hub's struct
// tags actually agree, and every other test here sets clawConn state directly.
// This one sends real heartbeat frames over the claw websocket — a typo on
// either side of the wire contract fails here and nowhere else.
func TestHeartbeatWireCarriesSubagentActivity(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "subagent-wire"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)

	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{
		"gateway_healthy": true, "subagents_active": true, "subagent_active_count": 2,
	}}); err != nil {
		t.Fatal(err)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return !cc.subagentsActiveAt.IsZero() && cc.subagentActiveCount == 2
	}, "subagent activity ingested from a wire heartbeat")

	// An old-bridge heartbeat without the keys must leave the state alone.
	// context_usage doubles as the processed marker: it is written in the same
	// critical section, so once it changes the heartbeat has been fully applied.
	cc.mu.RLock()
	recordedAt := cc.subagentsActiveAt
	cc.mu.RUnlock()
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{
		"gateway_healthy": true, "context_usage": 55,
	}}); err != nil {
		t.Fatal(err)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.contextUsage == 55
	}, "field-less heartbeat processed")
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if !cc.subagentsActiveAt.Equal(recordedAt) || cc.subagentActiveCount != 2 {
		t.Fatalf("field-less heartbeat touched subagent state: at=%v count=%d", cc.subagentsActiveAt, cc.subagentActiveCount)
	}
}
