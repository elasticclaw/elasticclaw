package hub

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func lostTurnHeartbeat(t *testing.T, conn *websocket.Conn, cc *clawConn, active *bool, sequence int) {
	t.Helper()
	payload := map[string]interface{}{"gateway_healthy": true, "context_usage": sequence}
	if active != nil {
		payload["turn_active"] = *active
	}
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	eventuallyWatchdog(t, func() bool {
		cc.mu.RLock()
		defer cc.mu.RUnlock()
		return cc.contextUsage == sequence
	}, "turn heartbeat applied")
}

func lostTurnRecoveryCount(t *testing.T, s *Server, clawID string) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE '%<!-- lost-turn-recovery:%'`, clawID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestLostTurnOldBridgeDoesNotFinishEarly(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "lost-turn-old-bridge"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-2 * defaultLostTurnGrace)
	cc.mu.Unlock()
	lostTurnHeartbeat(t, conn, cc, nil, 1)
	s.checkClawStatus()
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.streamingStartedAt.IsZero() || !cc.turnInactiveSince.IsZero() || cc.forcedFinishCount != 0 {
		t.Fatal("old bridge's open turn changed without explicit inactivity evidence")
	}
	if got := lostTurnRecoveryCount(t, s, clawID); got != 0 {
		t.Fatalf("old bridge received %d recoveries", got)
	}
}

func TestLostTurnFinishesAndInjectsRecovery(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "lost-turn-recovery"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-2 * defaultLostTurnGrace)
	cc.streamingMsgID = "lost-turn-partial"
	cc.streamingBuf.WriteString("partial response")
	cc.forcedFinishCount = 1
	cc.mu.Unlock()
	if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`, "lost-turn-nag", clawID, cc.tenantID, "hub", streamingTimeoutNudge, time.Now()); err != nil {
		t.Fatal(err)
	}
	inactive := false
	lostTurnHeartbeat(t, conn, cc, &inactive, 1)
	cc.mu.Lock()
	cc.turnInactiveSince = time.Now().Add(-defaultLostTurnGrace - time.Second)
	cc.mu.Unlock()
	// Further false reports must preserve the beginning of the inactive run.
	lostTurnHeartbeat(t, conn, cc, &inactive, 2)
	s.checkClawStatus()
	cc.mu.RLock()
	if cc.lastTurnFinishedAt.IsZero() || cc.forcedFinishCount != 1 || cc.streamingBuf.Len() != 0 || !cc.turnInactiveSince.IsZero() {
		t.Errorf("lost turn not finished cleanly: finished=%v forced=%d inactive=%v", cc.lastTurnFinishedAt, cc.forcedFinishCount, cc.turnInactiveSince)
	}
	cc.mu.RUnlock()
	var partial string
	if err := db.QueryRow(`SELECT content FROM messages WHERE id='lost-turn-partial'`).Scan(&partial); err != nil || partial != "partial response [interrupted]" {
		t.Fatalf("partial response = %q, err=%v", partial, err)
	}
	var nags int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id='lost-turn-nag'`).Scan(&nags); err != nil || nags != 0 {
		t.Fatalf("stale nags=%d, err=%v", nags, err)
	}
	if got := lostTurnRecoveryCount(t, s, clawID); got != 1 {
		t.Fatalf("recoveries=%d, want 1", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		var msg types.WSMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type != "message" {
			continue
		}
		payload, ok := msg.Payload.(map[string]interface{})
		if !ok {
			t.Fatalf("unexpected payload: %#v", msg.Payload)
		}
		content, _ := payload["content"].(string)
		for _, want := range []string{"previous turn ended", "git status", "git log", "Do not redo finished work", "resend that stage's completion signal"} {
			if !strings.Contains(content, want) {
				t.Errorf("recovery lacks %q: %s", want, content)
			}
		}
		break
	}
}

func TestLostTurnFlappingHeartbeatRestartsGrace(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "lost-turn-flapping"
	conn := watchdogClaw(t, s, clawID)
	cc := watchdogClawConn(t, s, clawID)
	cc.mu.Lock()
	cc.streamingStartedAt = time.Now().Add(-2 * defaultLostTurnGrace)
	cc.mu.Unlock()
	inactive, active := false, true
	lostTurnHeartbeat(t, conn, cc, &inactive, 1)
	cc.mu.Lock()
	cc.turnInactiveSince = time.Now().Add(-2 * defaultLostTurnGrace)
	cc.mu.Unlock()
	lostTurnHeartbeat(t, conn, cc, &active, 2)
	cc.mu.RLock()
	cleared := cc.turnInactiveSince.IsZero()
	cc.mu.RUnlock()
	if !cleared {
		t.Fatal("active heartbeat did not clear inactivity evidence")
	}
	lostTurnHeartbeat(t, conn, cc, &inactive, 3)
	s.checkClawStatus()
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.streamingStartedAt.IsZero() || !cc.lastTurnFinishedAt.IsZero() || time.Since(cc.turnInactiveSince) >= defaultLostTurnGrace {
		t.Fatal("flapping report must start a new grace period")
	}
	if got := lostTurnRecoveryCount(t, s, clawID); got != 0 {
		t.Fatalf("recoveries=%d, want 0", got)
	}
}

func TestLostTurnRecoveryCapResetsOnlyOnStageTransition(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "lost-turn-stage-cap"
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", "lost turn", "elasticclaw", "connected", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Keep the connection out of s.claws: recovery messages are persisted,
	// and this unit test needs no listening socket or delivery goroutine.
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id"}
	if !s.claimPipelineStageTransition(clawID, "implementation") {
		t.Fatal("initial stage transition failed")
	}
	markLost := func() {
		cc.mu.Lock()
		cc.finishTurnLocked()
		cc.streamingStartedAt = time.Now().Add(-2 * defaultLostTurnGrace)
		cc.turnInactiveSince = time.Now().Add(-defaultLostTurnGrace - time.Second)
		cc.lastHeartbeatAt = time.Now()
		cc.mu.Unlock()
	}
	for i := 0; i < lostTurnRecoveryMax; i++ {
		markLost()
		s.recoverLostTurn(time.Now(), clawID, cc, defaultLostTurnGrace)
		if got := lostTurnRecoveryCount(t, s, clawID); got != i+1 {
			t.Fatalf("recoveries=%d, want %d", got, i+1)
		}
	}
	if s.claimPipelineStageTransition(clawID, "implementation") {
		t.Fatal("same stage must not reset budget")
	}
	markLost()
	s.recoverLostTurn(time.Now(), clawID, cc, defaultLostTurnGrace)
	if got := lostTurnRecoveryCount(t, s, clawID); got != lostTurnRecoveryMax {
		t.Fatalf("cap exceeded: recoveries=%d", got)
	}
	cc.mu.RLock()
	if cc.streamingStartedAt.IsZero() || cc.forcedFinishCount != 0 {
		t.Error("capped lost turn should remain open without escalation")
	}
	cc.mu.RUnlock()
	var persisted int
	if err := db.QueryRow(`SELECT lost_turn_recovery_count FROM claws WHERE id=?`, clawID).Scan(&persisted); err != nil || persisted != lostTurnRecoveryMax {
		t.Fatalf("durable cap=%d, err=%v", persisted, err)
	}
	if !s.claimPipelineStageTransition(clawID, "build_check") {
		t.Fatal("next stage transition failed")
	}
	s.recoverLostTurn(time.Now(), clawID, cc, defaultLostTurnGrace)
	if got := lostTurnRecoveryCount(t, s, clawID); got != lostTurnRecoveryMax+1 {
		t.Fatalf("new stage did not re-arm budget: recoveries=%d", got)
	}
}

func TestLostTurnGraceSettings(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"", 5 * time.Minute},
		{"1m", 2 * time.Minute},
		{"8m", 8 * time.Minute},
		{"invalid", 5 * time.Minute},
		{"0", 5 * time.Minute},
		{"-1m", 5 * time.Minute},
	} {
		t.Run(fmt.Sprintf("grace=%s", tt.value), func(t *testing.T) {
			s := &Server{hubCfg: &types.HubConfig{Liveness: &types.LivenessConfig{LostTurnGrace: tt.value}}}
			if got := s.livenessSettings().lostTurnGrace; got != tt.want {
				t.Fatalf("grace=%s, want %s", got, tt.want)
			}
		})
	}
}

func TestTurnHeartbeatEvidenceResetsAcrossGapsAndBoundaries(t *testing.T) {
	at := time.Now()
	inactive, active := false, true
	cc := &clawConn{streamingStartedAt: at.Add(-2 * defaultLostTurnGrace), lastHeartbeatAt: at}
	cc.applyTurnHeartbeatLocked(&inactive, at, defaultLostTurnGrace)
	if !cc.turnInactiveSince.Equal(at) {
		t.Fatal("first inactive report must start grace")
	}
	later := at.Add(time.Minute)
	cc.applyTurnHeartbeatLocked(&inactive, later, defaultLostTurnGrace)
	if !cc.turnInactiveSince.Equal(at) {
		t.Fatal("continuous inactive report restarted grace")
	}
	later = at.Add(2 * defaultLostTurnGrace)
	cc.applyTurnHeartbeatLocked(&inactive, later, defaultLostTurnGrace)
	if !cc.turnInactiveSince.Equal(later) {
		t.Fatal("heartbeat gap must restart grace")
	}
	cc.applyTurnHeartbeatLocked(&active, later, defaultLostTurnGrace)
	if !cc.turnInactiveSince.IsZero() {
		t.Fatal("active report did not clear evidence")
	}
	cc.applyTurnHeartbeatLocked(&inactive, later, defaultLostTurnGrace)
	cc.applyTurnHeartbeatLocked(nil, later, defaultLostTurnGrace)
	if !cc.turnInactiveSince.IsZero() {
		t.Fatal("missing report must invalidate old evidence")
	}
	cc.applyTurnHeartbeatLocked(&inactive, later, defaultLostTurnGrace)
	cc.finishTurnLocked()
	if !cc.turnInactiveSince.IsZero() {
		t.Fatal("finished turn retained evidence")
	}
	cc.applyTurnHeartbeatLocked(&inactive, later, defaultLostTurnGrace)
	if !cc.turnInactiveSince.IsZero() {
		t.Fatal("idle claw must not accumulate evidence for a future turn")
	}
}

func TestLostTurnRecoveryBudgetMigration(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "lost-turn-migration"
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", "migration", "elasticclaw", "connected", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE claws DROP COLUMN lost_turn_recovery_count`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT lost_turn_recovery_count FROM claws WHERE id=?`, clawID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migrated budget=%d, err=%v", count, err)
	}
	if _, err := db.Exec(`UPDATE claws SET lost_turn_recovery_count=? WHERE id=?`, lostTurnRecoveryMax, clawID); err != nil {
		t.Fatal(err)
	}
	if err := migrate(s.db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT lost_turn_recovery_count FROM claws WHERE id=?`, clawID).Scan(&count); err != nil || count != lostTurnRecoveryMax {
		t.Fatalf("budget after restart=%d, err=%v", count, err)
	}
}
