package hub

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
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
	// Registration with gateway_ready triggers an async post-registration
	// sendWakeMessage/sendInitialPlanInstruction turn reservation (real hub
	// behavior, unrelated to most watchdog tests). It only fires when the
	// claw has no messages yet (clawHasMessages), so seed an already-delivered
	// row BEFORE registering — inserting after the ack races the async check
	// and lets the wake reserve the turn and write a stray message frame.
	// messages.claw_id has a foreign key on claws(id), so the claws row must
	// exist first; registration upserts it, so pre-creating is safe.
	// delivered_at is set so the seed can never be drained as a pending
	// message and interfere with queue-draining tests.
	if _, err := s.db.Exec(
		`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`,
		clawID, "test-tenant-id", "watchdog claw", "elasticclaw", "offline", time.Now(),
	); err != nil {
		t.Fatalf("pre-create claws row: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,?)`,
		"seed-"+clawID, clawID, "test-tenant-id", "system", "seed", "pre", time.Now(), time.Now(),
	); err != nil {
		t.Fatalf("seed wake-suppression message: %v", err)
	}
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
	// First observation after hub boot is a baseline, not a restart.
	beat(0)
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

// A bridge-process relaunch resets restart_count to 0 — equal to the zero
// value of an absent autoResumeRestartCounts entry, which a plain map read
// cannot distinguish from "never resumed".
func TestRestartCountResetMidTurnEnqueuesResume(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-restart-reset"
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
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ?`, clawID, restartResumePrefix+"%").Scan(&n)
		return n
	}
	// Historical baseline from before the relaunch, then the relaunched
	// bridge reports a reset counter mid-turn.
	beat(5)
	beat(0)
	eventuallyWatchdog(t, func() bool { return count() == 1 }, "resume after restart_count reset")
	beat(0)
	beat(0)
	if got := count(); got != 1 {
		t.Fatalf("resume count after duplicate reset beats = %d, want 1", got)
	}
}

func TestRestartWhileIdleDoesNotEnqueueResume(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-idle-restart"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=1 WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	// Baseline observation first: the initial restart_count after (re)boot is
	// recorded, not treated as a restart.
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{"gateway_healthy": true, "restart_count": 0}}); err != nil {
		t.Fatal(err)
	}
	eventuallyWatchdog(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		_, seen := s.gatewayRestartCounts[clawID]
		return seen
	}, "restart count baseline")
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

func TestFirstHeartbeatAfterHubBootDoesNotAutoResume(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-hub-boot-baseline"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=1 WHERE id=?`, clawID); err != nil {
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
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ?`, clawID, restartResumePrefix+"%").Scan(&n)
		return n
	}
	// A fresh Server has an empty gatewayRestartCounts map — exactly the state
	// after a hub restart. A historical nonzero restart_count on the first
	// heartbeat must be recorded as a baseline, not treated as a restart.
	beat(3)
	eventuallyWatchdog(t, func() bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.gatewayRestartCounts[clawID] == 3
	}, "baseline recorded")
	if got := count(); got != 0 {
		t.Fatalf("resume rows after baseline heartbeat=%d, want 0", got)
	}
	// A genuine restart after the baseline still triggers auto-resume.
	beat(4)
	eventuallyWatchdog(t, func() bool { return count() == 1 }, "resume after genuine restart")
}

func TestRestartDuringBootstrapDoesNotEnqueueResume(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-bootstrap-restart"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=0 WHERE id=?`, clawID); err != nil {
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
	beat(0)
	beat(1)
	time.Sleep(100 * time.Millisecond)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content LIKE ?`, clawID, restartResumePrefix+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("resume rows=%d err=%v, want 0", n, err)
	}
}

func TestSessionRotatedEnqueuesResume(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-session-rotated"
	conn := watchdogClaw(t, s, clawID)
	_ = watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=1, issue_title='Fix session', github_issue_id='owner/repo#42' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "session_rotated"}); err != nil {
		t.Fatalf("write session_rotated: %v", err)
	}
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ?`, clawID, sessionRotatedResumePrefix+"%").Scan(&n)
		return n
	}
	eventuallyWatchdog(t, func() bool { return count() == 1 }, "session rotated resume")
}

func TestSessionRotatedResumeThrottled(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-session-rotated-throttled"
	conn := watchdogClaw(t, s, clawID)
	_ = watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=1, issue_title='Fix session' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	// A resume prompt already went out moments ago — a second rotation inside
	// the throttle window must not enqueue another one.
	if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		uuid.NewString(), clawID, "test-tenant-id", "hub", sessionRotatedResumePrefix+" earlier resume", now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "session_rotated"}); err != nil {
		t.Fatalf("write session_rotated: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ?`, clawID, sessionRotatedResumePrefix+"%").Scan(&n); err != nil || n != 1 {
		t.Fatalf("resume rows=%d err=%v, want 1 (throttled)", n, err)
	}
}

func TestSessionRotatedResumeThrottleWindowExpires(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-session-rotated-throttle-expired"
	conn := watchdogClaw(t, s, clawID)
	_ = watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='connected', bootstrap_ok=1, issue_title='Fix session' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	// The previous resume is older than the throttle window, so a new
	// rotation must enqueue a fresh resume prompt.
	if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		uuid.NewString(), clawID, "test-tenant-id", "hub", sessionRotatedResumePrefix+" earlier resume", now().Add(-sessionRotatedResumeThrottle-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "session_rotated"}); err != nil {
		t.Fatalf("write session_rotated: %v", err)
	}
	count := func() int {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ?`, clawID, sessionRotatedResumePrefix+"%").Scan(&n)
		return n
	}
	eventuallyWatchdog(t, func() bool { return count() == 2 }, "session rotated resume after throttle window")
}

func TestSessionRotatedNotConnectedDoesNotEnqueueResume(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-session-rotated-not-connected"
	conn := watchdogClaw(t, s, clawID)
	_ = watchdogClawConn(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET status='starting', bootstrap_ok=1, issue_title='Fix session', github_issue_id='owner/repo#42' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "session_rotated"}); err != nil {
		t.Fatalf("write session_rotated: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content LIKE ?`, clawID, sessionRotatedResumePrefix+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("resume rows=%d err=%v, want 0", n, err)
	}
}

func TestSilentProgressProbeGoesOverStatusChannelWithoutTranscript(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "progress-probe-silent"
	_ = watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	statusConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = statusConn.Close(websocket.StatusNormalClosure, "done") })
	if err := wsjson.Write(ctx, statusConn, types.WSMessage{Type: "register", Payload: types.RegisterPayload{
		ClawID: clawID, Name: "probe claw", Template: "elasticclaw", Token: "claw-token", Channel: "status",
	}}); err != nil {
		t.Fatal(err)
	}
	var ack types.WSMessage
	if err := wsjson.Read(ctx, statusConn, &ack); err != nil || ack.Type != "registered" {
		t.Fatalf("status channel ack: type=%q err=%v", ack.Type, err)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.statusConn != nil
	}, "status channel attach")

	// Turn already long enough for a probe.
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-2 * time.Minute)
	cc.streamingMsgID = "stream"
	cc.lastProgressProbeAt = time.Time{}
	cc.mu.Unlock()

	s.maybeSendProgressProbe(cc, time.Now())

	var got types.WSMessage
	if err := wsjson.Read(ctx, statusConn, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "nudge" {
		t.Fatalf("status channel message type=%q, want nudge", got.Type)
	}
	payload, _ := got.Payload.(map[string]interface{})
	if payload["content"] != progressProbeContent {
		t.Fatalf("nudge content=%#v, want progress probe", payload["content"])
	}
	// Must never appear in the chat transcript (no message rows).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=?`, clawID, progressProbeContent).Scan(&n); err != nil || n != 0 {
		t.Fatalf("transcript rows=%d err=%v, want 0 (silent probe)", n, err)
	}
	// Rate-limited: immediate second call must not send another nudge.
	s.maybeSendProgressProbe(cc, time.Now())
	// Non-blocking check: no second message within a short window.
	readCtx, readCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer readCancel()
	var second types.WSMessage
	if err := wsjson.Read(readCtx, statusConn, &second); err == nil {
		t.Fatalf("unexpected second nudge: %#v", second)
	}
}

func TestSilentProgressProbeSkippedWithoutStatusChannel(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "progress-probe-no-status"
	_ = watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-2 * time.Minute)
	cc.streamingMsgID = "stream"
	cc.statusConn = nil
	cc.mu.Unlock()
	s.maybeSendProgressProbe(cc, time.Now())
	// Must not fall back to chat queue.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=?`, clawID, progressProbeContent).Scan(&n); err != nil || n != 0 {
		t.Fatalf("chat rows=%d err=%v, want 0 (no fallback)", n, err)
	}
}

func TestStreamingNudgeGoesOverStatusChannel(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-nudge-status-channel"
	_ = watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)

	// Connect the status channel like the bridge does.
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	statusConn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = statusConn.Close(websocket.StatusNormalClosure, "done") })
	if err := wsjson.Write(ctx, statusConn, types.WSMessage{Type: "register", Payload: types.RegisterPayload{
		ClawID: clawID, Name: "watchdog claw", Template: "elasticclaw", Token: "claw-token", Channel: "status",
	}}); err != nil {
		t.Fatal(err)
	}
	var ack types.WSMessage
	if err := wsjson.Read(ctx, statusConn, &ack); err != nil || ack.Type != "registered" {
		t.Fatalf("status channel ack: type=%q err=%v", ack.Type, err)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.statusConn != nil
	}, "status channel attach")

	cc.mu.Lock()
	cc.streamingStartedAt = time.Now()
	cc.streamingMsgID = "stream"
	cc.mu.Unlock()
	s.sendStreamingNudge(cc, streamingTimeoutNudge)

	// The nudge must arrive over the status channel, not the message queue.
	var got types.WSMessage
	if err := wsjson.Read(ctx, statusConn, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "nudge" {
		t.Fatalf("status channel message type=%q, want nudge", got.Type)
	}
	payload, _ := got.Payload.(map[string]interface{})
	if payload["claw_id"] != clawID || payload["content"] != streamingTimeoutNudge {
		t.Fatalf("nudge payload=%#v", payload)
	}
	// The UI row is recorded as already delivered — never queued for the agent.
	var delivered, pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=? AND delivered_at IS NOT NULL`, clawID, streamingTimeoutNudge).Scan(&delivered); err != nil || delivered != 1 {
		t.Fatalf("delivered rows=%d err=%v, want 1", delivered, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=? AND delivered_at IS NULL`, clawID, streamingTimeoutNudge).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending rows=%d err=%v, want 0", pending, err)
	}
}

func TestStreamingNudgeFallsBackToQueueWithoutStatusChannel(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-nudge-fallback"
	_ = watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now()
	cc.streamingMsgID = "stream"
	cc.statusConn = nil
	cc.mu.Unlock()
	s.sendStreamingNudge(cc, streamingTimeoutNudge)
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=? AND delivered_at IS NULL`, clawID, streamingTimeoutNudge).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending rows=%d err=%v, want 1", pending, err)
	}
}

func TestStreamingNudgeDroppedWhenTurnAlreadyEnded(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "watchdog-nudge-stale-drop"
	_ = watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	// The nudge goroutine may run after the turn it targeted already finished;
	// it must be dropped entirely, not queued as the next turn's prompt.
	cc.mu.Lock()
	cc.finishTurnLocked()
	cc.mu.Unlock()
	s.sendStreamingNudge(cc, streamingTimeoutNudge)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content=?`, clawID, streamingTimeoutNudge).Scan(&n); err != nil || n != 0 {
		t.Fatalf("nudge rows=%d err=%v, want 0", n, err)
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
	// The flag must survive segment flushes: if the chunk handler re-armed it
	// per segment (the original bug), interleaved chunks would reset it to
	// false and the row-count check alone could still pass via dedup.
	cc.mu.RLock()
	timeoutSent := cc.streamingTimeoutSent
	cc.mu.RUnlock()
	if !timeoutSent {
		t.Fatal("streamingTimeoutSent was re-armed by an interleaved segment; it must stay true for the whole turn")
	}
}
