package main

import (
	"context"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func (gs *gatewaySession) RunTurn(ctx context.Context, message, messageID string, sink EventSink) (string, error) {
	return gs.SendMessage(ctx, message, messageID, sink.OnChunk, sink.OnActivity)
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
