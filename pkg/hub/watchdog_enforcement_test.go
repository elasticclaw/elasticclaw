package hub

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func watchdogClaw(t *testing.T, s *Server, clawID string) *websocket.Conn {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "done") })
	ready := true
	if err := wsjson.Write(ctx, conn, types.WSMessage{Type: "register", Payload: types.RegisterPayload{
		ClawID: clawID, Name: "watchdog claw", Template: "elasticclaw", Token: "claw-token", GatewayReady: &ready,
	}}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var ack types.WSMessage
	if err := wsjson.Read(ctx, conn, &ack); err != nil || ack.Type != "registered" {
		t.Fatalf("read registration ack: type=%q err=%v", ack.Type, err)
	}
	// Registration with gateway_ready races an async post-registration
	// sendWakeMessage/sendInitialPlanInstruction turn reservation (real hub
	// behavior, unrelated to most watchdog tests). It only fires when the
	// claw has no messages yet (clawHasMessages), so seed an already-delivered
	// row immediately to suppress it — delivered_at is set so it can never be
	// drained as a pending message and interfere with queue-draining tests.
	_, _ = s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,?)`,
		"seed-"+clawID, clawID, "test-tenant-id", "system", "seed", "pre", time.Now(), time.Now(),
	)
	return conn
}

func eventuallyWatchdog(t *testing.T, check func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func watchdogClawConn(t *testing.T, s *Server, clawID string) *clawConn {
	t.Helper()
	var cc *clawConn
	eventuallyWatchdog(t, func() bool {
		s.mu.RLock()
		cc = s.claws[clawID]
		s.mu.RUnlock()
		return cc != nil
	}, "claw registration")
	return cc
}

func TestWatchdogUnhealthyHeartbeatsScheduleClawRetry(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	s.cronScheduler = newCronScheduler(s)
	const clawID = "watchdog-unhealthy"
	conn := watchdogClaw(t, s, clawID)
	// Registration intentionally leaves newly seen claws in starting/bootstrap-pending.
	// The watchdog only escalates an active, bootstrapped claw, and retry scheduling
	// advances a task run (not the legacy workflow_runs record).
	if _, err := db.Exec(`UPDATE claws SET provider='noop', status='connected', bootstrap_ok=1 WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	runID, _, err := s.ensureTaskRunForClaw(clawID, TaskRunStart{RunKind: taskRunKindPRTask, OwnerType: taskRunOwnerFactory, AnalyticsEnabled: true, Tags: []string{}})
	if err != nil {
		t.Fatalf("create task run: %v", err)
	}
	for i := 0; i < defaultGatewayUnhealthyMax; i++ {
		if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{"gateway_healthy": false}}); err != nil {
			t.Fatalf("write unhealthy heartbeat %d: %v", i, err)
		}
	}
	eventuallyWatchdog(t, func() bool {
		var attempts, retryEvents int
		_ = db.QueryRow(`SELECT attempt_count FROM task_runs WHERE id=?`, runID).Scan(&attempts)
		_ = db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_key=?`, runID, "retry:"+clawID+":2").Scan(&retryEvents)
		return attempts == 2 && retryEvents == 1
	}, "replacement attempt scheduled for unhealthy claw")
}

func TestWatchdogHealthyHeartbeatResetsUnhealthyCounter(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-heartbeat-reset"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	writeHeartbeat := func(healthy bool) {
		t.Helper()
		if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{"gateway_healthy": healthy}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < defaultGatewayUnhealthyMax-1; i++ {
		writeHeartbeat(false)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.gatewayUnhealthyCount == defaultGatewayUnhealthyMax-1
	}, "unhealthy heartbeat count")
	writeHeartbeat(true)
	eventuallyWatchdog(t, func() bool { cc.mu.RLock(); defer cc.mu.RUnlock(); return cc.gatewayUnhealthyCount == 0 }, "healthy reset")
	for i := 0; i < defaultGatewayUnhealthyMax-1; i++ {
		writeHeartbeat(false)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.gatewayUnhealthyCount == defaultGatewayUnhealthyMax-1
	}, "post-reset unhealthy count")
}

func TestHeartbeatRestartCountChangeDetectedInBothDirections(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "heartbeat-restart-count"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	writeRestartCount := func(n int) {
		t.Helper()
		if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{"gateway_healthy": true, "restart_count": n}}); err != nil {
			t.Fatal(err)
		}
	}
	restartCount := func() int {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.gatewayRestartCount
	}
	writeRestartCount(3)
	eventuallyWatchdog(t, func() bool { return restartCount() == 3 }, "restart count increase recorded")
	// A shell-supervisor bridge relaunch resets the bridge's counter, so the
	// reported restart_count can drop below the stored baseline. The baseline
	// must follow the decrease or the relaunch goes permanently undetected.
	writeRestartCount(1)
	eventuallyWatchdog(t, func() bool { return restartCount() == 1 }, "restart count decrease (bridge relaunch) recorded")
}

func TestWatchdogForceFinishesStaleTurnAndDeliversQueue(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-force-finish"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-defaultBusyTurnMax - time.Minute)
	cc.streamingMsgID = "partial-turn"
	cc.streamingBuf.WriteString("partial response")
	cc.mu.Unlock()
	if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,NULL)`, "queued-message", clawID, "test-tenant-id", "user", "next request", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.checkClawStatus()
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// The connection bootstrap may deliver unrelated frames (e.g. an async
	// checkpoint_create request) before the queued message; skip those.
	var delivered types.WSMessage
	for {
		if err := wsjson.Read(readCtx, conn, &delivered); err != nil {
			t.Fatalf("read delivered queued message: %v", err)
		}
		if delivered.Type == "message" {
			break
		}
	}
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if !cc.awaitingResponse || cc.streamingStartedAt.IsZero() || cc.streamingMsgID != "" || cc.forcedFinishCount != 1 {
		t.Fatalf("stale turn not replaced cleanly: awaiting=%v streaming=%v message=%q forced=%d", cc.awaitingResponse, !cc.streamingStartedAt.IsZero(), cc.streamingMsgID, cc.forcedFinishCount)
	}
	var deliveredAt interface{}
	if err := db.QueryRow(`SELECT delivered_at FROM messages WHERE id=?`, "queued-message").Scan(&deliveredAt); err != nil || deliveredAt == nil {
		t.Fatalf("queued message not marked delivered: delivered_at=%v err=%v", deliveredAt, err)
	}
}

func TestWatchdogSecondForcedFinishStopsClaw(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-repeated-stuck"
	_ = watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	for i := 0; i < 2; i++ {
		cc.mu.Lock()
		cc.streamingStartedAt = time.Now().Add(-defaultBusyTurnMax - time.Minute)
		cc.mu.Unlock()
		s.checkClawStatus()
	}
	eventuallyWatchdog(t, func() bool {
		var status string
		_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status)
		return status == "error"
	}, "repeated stuck claw error")
}

func TestWatchdogSilentDeathStopsClaw(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-silent-death"
	_ = watchdogClaw(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET bootstrap_ok=1, status='connected' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.lastUserMessageAt = time.Now().Add(-defaultSilentDeathMax - time.Minute)
	cc.lastStatusAt = time.Now().Add(-defaultSilentDeathMax - time.Minute)
	cc.unresponsiveWarnedAt = time.Now().Add(-5 * time.Minute)
	cc.mu.Unlock()
	s.checkClawStatus()
	eventuallyWatchdog(t, func() bool {
		var status string
		_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status)
		return status == "error"
	}, fmt.Sprintf("silent claw %s error", clawID))
}

func TestTurnFinishDeletesPendingWatchdogNags(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-delete-on-finish"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now()
	cc.streamingMsgID = "stream"
	cc.mu.Unlock()
	for _, row := range []struct{ id, role, content string }{{"nag", "hub", streamingTimeoutNudge}, {"user", "user", "next request"}} {
		if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,NULL)`, row.id, clawID, "test-tenant-id", row.role, row.content, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "message", Payload: types.HubMessage{Content: "done"}}); err != nil {
		t.Fatal(err)
	}
	eventuallyWatchdog(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='nag'`).Scan(&n)
		return n == 0
	}, "stale nag deletion")
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		var got types.WSMessage
		if err := wsjson.Read(readCtx, conn, &got); err != nil {
			t.Fatal(err)
		}
		if got.Type == "message" {
			b, _ := got.Payload.(map[string]interface{})
			if b["content"] != "next request" {
				t.Fatalf("delivered content = %#v", b["content"])
			}
			break
		}
	}
}

func TestForcedFinishDeletesPendingWatchdogNags(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-delete-forced"
	_ = watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-defaultBusyTurnMax - time.Minute)
	cc.mu.Unlock()
	if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,NULL)`, "nag", clawID, "test-tenant-id", "hub", streamingTimeoutNudge, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.checkClawStatus()
	eventuallyWatchdog(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='nag'`).Scan(&n)
		return n == 0
	}, "forced finish nag deletion")
}

func TestRestartMidTurnEnqueuesExactlyOneResume(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-restart-resume"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=1, issue_title='Fix watchdog', github_issue_id='owner/repo#42' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now()
	cc.mu.Unlock()
	beat := func(n int) {
		if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{"gateway_healthy": true, "restart_count": n}}); err != nil {
			t.Fatal(err)
		}
	}
	beat(1)
	beat(1)
	beat(1)
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ?`, clawID, restartResumePrefix+"%").Scan(&n)
		return n
	}
	eventuallyWatchdog(t, func() bool { return count() == 1 }, "one restart resume")
	beat(2)
	eventuallyWatchdog(t, func() bool { return count() == 2 }, "second restart resume")
}

func TestRestartWhileIdleDoesNotEnqueueResume(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-idle-restart"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=1 WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	// watchdogClaw already seeded a delivered message to suppress the async
	// post-registration wake-turn reservation. Also force the claw back to a
	// deterministic idle state in case that reservation raced ahead of it.
	cc.mu.Lock()
	cc.streamingStartedAt = time.Time{}
	cc.awaitingResponse = false
	cc.lastTurnFinishedAt = time.Time{}
	cc.mu.Unlock()
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{"gateway_healthy": true, "restart_count": 1}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content LIKE ?`, clawID, restartResumePrefix+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("resume rows=%d err=%v", n, err)
	}
}

func TestLongTurnInterleavedSegmentsNagsExactlyOnce(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-interleaved-segments"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	write := func(m types.WSMessage) {
		if err := wsjson.Write(context.Background(), conn, m); err != nil {
			t.Fatal(err)
		}
	}
	write(types.WSMessage{Type: "chunk", Payload: map[string]string{"content": "first"}})
	eventuallyWatchdog(t, func() bool { cc.mu.RLock(); defer cc.mu.RUnlock(); return !cc.streamingStartedAt.IsZero() }, "first chunk")
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-13 * time.Minute)
	cc.mu.Unlock()
	write(types.WSMessage{Type: "agent_activity", Payload: map[string]any{"kind": "tool", "phase": "started"}})
	write(types.WSMessage{Type: "chunk", Payload: map[string]string{"content": "second"}})
	write(types.WSMessage{Type: "heartbeat", Payload: map[string]any{"gateway_healthy": true}})
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=?`, clawID, streamingTimeoutNudge).Scan(&n)
		return n
	}
	eventuallyWatchdog(t, func() bool { return count() == 1 }, "one timeout nudge")
	for i := 0; i < 2; i++ {
		write(types.WSMessage{Type: "agent_activity", Payload: map[string]any{"kind": "tool", "phase": "started"}})
		write(types.WSMessage{Type: "chunk", Payload: map[string]string{"content": "more"}})
		write(types.WSMessage{Type: "heartbeat", Payload: map[string]any{"gateway_healthy": true}})
	}
	time.Sleep(100 * time.Millisecond)
	if got := count(); got != 1 {
		t.Fatalf("timeout nudge rows=%d, want 1", got)
	}
}
