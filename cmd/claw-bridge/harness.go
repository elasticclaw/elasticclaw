// The harness seam: hub-facing bridge code (runHubLoop, the workflow control
// supervisor, session recovery reporting) drives the agent runtime through the
// Harness interface instead of reaching into the OpenClaw gateway session.
// Adapter internals (gateway process supervision, the read loop, SendMessage)
// stay concrete in main.go.

package main

import (
	"context"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// harnessName identifies the agent runtime this bridge drives in its hub registration.
const harnessName = "openclaw"

// heartbeatPayload builds the heartbeat status fields. The gateway_* names and
// restart_count are kept for hubs older than the harness_* names.
func heartbeatPayload(healthy, ready bool, contextUsage, restartCount int) map[string]interface{} {
	return map[string]interface{}{
		"gateway_healthy": healthy, "gateway_ready": ready,
		"context_usage": contextUsage, "restart_count": restartCount,
		"harness_healthy": healthy, "harness_ready": ready, "harness_restart_count": restartCount,
	}
}

// EventSink receives a turn's streamed output.
type EventSink struct {
	OnChunk    func(string)
	OnActivity func(agentActivity)
}

// Harness is the agent runtime the bridge drives. OpenClaw (*gatewaySession) is the only implementation.
type Harness interface {
	RunTurn(ctx context.Context, message, messageID string, sink EventSink) (string, error)
	Steer(ctx context.Context, text string) (sent bool, err error)
	IsReady() bool
	SessionKey() string
	OnSessionReplaced(fn func(previousKey, interruptedMessageID string))
	RecoveryEdge(agentErr error, outcome string) types.SessionRecoveryEdge
	Healthy() bool
	RestartCount() int
	RefreshUsage(ctx context.Context)
	ContextUsage() int
	Usage() gatewayUsage
	Subagents(ctx context.Context) (int, error)
}

var _ Harness = (*gatewaySession)(nil)

// TurnError preserves the underlying failure while identifying its recovery class.
type TurnError struct {
	Class types.TurnErrorClass
	Err   error
}

func (e *TurnError) Error() string { return e.Err.Error() }
func (e *TurnError) Unwrap() error { return e.Err }
