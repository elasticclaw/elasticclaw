package hub

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

const (
	defaultLostTurnGrace = 5 * time.Minute
	minLostTurnGrace     = 2 * time.Minute
	lostTurnRecoveryMax  = 3
)

func (cc *clawConn) applyTurnHeartbeatLocked(active *bool, at time.Time, grace time.Duration) {
	if active == nil || *active || cc.streamingStartedAt.IsZero() {
		cc.turnInactiveSince = time.Time{}
		return
	}
	if cc.turnInactiveSince.IsZero() || at.Sub(cc.lastHeartbeatAt) > grace {
		cc.turnInactiveSince = at
	}
}

// recoverLostTurn uses explicit bridge evidence, independently of the much
// longer busy-turn timeout. Its durable budget is re-armed on stage transitions.
func (s *Server) recoverLostTurn(at time.Time, clawID string, cc *clawConn, grace time.Duration) bool {
	cc.mu.Lock()
	if cc.workflowV2Controlled || cc.streamingStartedAt.IsZero() || cc.turnInactiveSince.IsZero() {
		cc.mu.Unlock()
		return false
	}
	if at.Sub(cc.streamingStartedAt) < grace || at.Sub(cc.turnInactiveSince) < grace {
		cc.mu.Unlock()
		log.Printf("[watchdog] %s lost-turn recovery waiting for grace %s", shortID(clawID), grace)
		return false
	}
	// Silence is not a continuous inactive report. Leave disconnected bridges
	// to the existing health watchdog instead of starting duplicate work.
	if cc.lastHeartbeatAt.IsZero() || at.Sub(cc.lastHeartbeatAt) > grace {
		cc.mu.Unlock()
		log.Printf("[watchdog] %s lost-turn recovery skipped: heartbeat is stale", shortID(clawID))
		return false
	}
	var count int
	var stage string
	err := s.db.QueryRow(`UPDATE claws SET lost_turn_recovery_count=lost_turn_recovery_count+1
		WHERE id=? AND lost_turn_recovery_count<?
		RETURNING lost_turn_recovery_count, pipeline_stage`, clawID, lostTurnRecoveryMax).Scan(&count, &stage)
	if err != nil {
		cc.mu.Unlock()
		if err == sql.ErrNoRows {
			log.Printf("[watchdog] %s lost-turn recovery skipped: per-stage cap %d reached", shortID(clawID), lostTurnRecoveryMax)
		} else {
			log.Printf("[watchdog] %s lost-turn recovery budget failed: %v", shortID(clawID), err)
		}
		return false
	}
	var content, messageID string
	if cc.streamingBuf.Len() > 0 {
		content = cc.streamingBuf.String() + " [interrupted]"
		messageID = cc.streamingMsgID
		if messageID == "" {
			messageID = uuid.New().String()
		}
	}
	tenantID := cc.tenantID
	cc.finishTurnLocked()
	cc.mu.Unlock()

	s.deleteStaleWatchdogNags(clawID)
	if content != "" {
		_, _ = s.db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET content=excluded.content, delivered_at=excluded.delivered_at`, messageID, clawID, tenantID, "claw", content, at, at)
		s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: types.HubMessage{ID: messageID, ClawID: clawID, TenantID: tenantID, Role: "claw", Content: content, CreatedAt: at}})
	}
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "agent_typing", Payload: map[string]string{"claw_id": clawID, "status": "idle"}})
	log.Printf("[watchdog] %s lost turn finished; injecting recovery %d/%d for stage %q", shortID(clawID), count, lostTurnRecoveryMax, stage)
	s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] Your previous turn ended before its reply reached the hub. Recover your current state from the workspace: inspect git status and git log before continuing. Do not redo finished work. If the current stage's work is already done, resend that stage's completion signal. Current pipeline stage: %q.\n\n<!-- lost-turn-recovery:%s -->", stage, uuid.New().String()))
	return true
}
