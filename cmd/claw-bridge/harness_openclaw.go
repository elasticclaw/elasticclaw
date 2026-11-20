package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/harness/openclaw"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func (gs *gatewaySession) RunTurn(ctx context.Context, message, messageID string, sink EventSink) (string, error) {
	reply, err := gs.SendMessage(ctx, message, messageID, sink.OnChunk, sink.OnActivity)
	if err != nil {
		return reply, &TurnError{Class: openclaw.ClassifyErrorText(err.Error(), true), Err: err}
	}
	return reply, nil
}

func (gs *gatewaySession) Steer(ctx context.Context, text string) (sent bool, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return gs.sendTurnScopedReq(reqCtx, "sessions.send", map[string]string{"key": gs.getSessionKey(), "message": text})
}

func (gs *gatewaySession) SessionKey() string {
	return gs.getSessionKey()
}

func (gs *gatewaySession) OnSessionReplaced(fn func(previousKey, interruptedMessageID string)) {
	gs.setOnSessionReplaced(fn)
}

func (gs *gatewaySession) RecoveryEdge(agentErr error, outcome string) types.SessionRecoveryEdge {
	return buildSessionRecoveryEdge(gs, agentErr, outcome)
}

func (gs *gatewaySession) Healthy() bool {
	return !gatewayProcessExited() && checkGateway(gs.client.addr)
}

func (gs *gatewaySession) RestartCount() int {
	return gatewayRestartBase + gatewayRestartCount()
}

func (gs *gatewaySession) RefreshUsage(ctx context.Context) {
	gs.refreshContextUsage(ctx)
}

func (gs *gatewaySession) Subagents(ctx context.Context) (int, error) {
	return gs.pollSubagentActivity(ctx)
}

// newOpenClawUsageAccumulator starts a usage epoch for this bridge process:
// a restart loses the running totals, so it must never reuse an epoch.
func newOpenClawUsageAccumulator() *openclaw.UsageAccumulator {
	epoch := make([]byte, 16)
	rand.Read(epoch) // never fails since Go 1.24
	return openclaw.NewUsageAccumulator(hex.EncodeToString(epoch))
}
