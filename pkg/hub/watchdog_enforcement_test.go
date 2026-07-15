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
	for i := 0; i < gatewayUnhealthyMax; i++ {
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
	for i := 0; i < gatewayUnhealthyMax-1; i++ {
		writeHeartbeat(false)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.gatewayUnhealthyCount == gatewayUnhealthyMax-1
	}, "unhealthy heartbeat count")
	writeHeartbeat(true)
	eventuallyWatchdog(t, func() bool { cc.mu.RLock(); defer cc.mu.RUnlock(); return cc.gatewayUnhealthyCount == 0 }, "healthy reset")
	for i := 0; i < gatewayUnhealthyMax-1; i++ {
		writeHeartbeat(false)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.gatewayUnhealthyCount == gatewayUnhealthyMax-1
	}, "post-reset unhealthy count")
}

func TestWatchdogForceFinishesStaleTurnAndDeliversQueue(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-force-finish"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-busyTurnMax - time.Minute)
	cc.streamingMsgID = "partial-turn"
	cc.streamingBuf.WriteString("partial response")
	cc.messageQueue = []types.HubMessage{{ID: "queued-message", ClawID: clawID, TenantID: "test-tenant-id", Role: "user", Content: "next request", CreatedAt: time.Now()}}
	cc.mu.Unlock()
	s.checkClawStatus()
	readCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var delivered types.WSMessage
	if err := wsjson.Read(readCtx, conn, &delivered); err != nil {
		t.Fatalf("read delivered queued message: %v", err)
	}
	if delivered.Type != "message" {
		t.Fatalf("delivered type = %q, want message", delivered.Type)
	}
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.isBusyLocked() || cc.forcedFinishCount != 1 || len(cc.messageQueue) != 0 {
		t.Fatalf("turn not force-finished cleanly: busy=%v forced=%d queued=%d", cc.isBusyLocked(), cc.forcedFinishCount, len(cc.messageQueue))
	}
}

func TestWatchdogSecondForcedFinishStopsClaw(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-repeated-stuck"
	_ = watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	for i := 0; i < 2; i++ {
		cc.mu.Lock()
		cc.streamingStartedAt = time.Now().Add(-busyTurnMax - time.Minute)
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
	cc.lastUserMessageAt = time.Now().Add(-silentDeathMax - time.Minute)
	cc.lastStatusAt = time.Now().Add(-silentDeathMax - time.Minute)
	cc.unresponsiveWarnedAt = time.Now().Add(-5 * time.Minute)
	cc.mu.Unlock()
	s.checkClawStatus()
	eventuallyWatchdog(t, func() bool {
		var status string
		_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status)
		return status == "error"
	}, fmt.Sprintf("silent claw %s error", clawID))
}
