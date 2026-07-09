package claws

import (
	"context"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Conn is the hub's per-claw WebSocket connection state (the former
// pkg/hub clawConn, moved here in the phase-2 claws extraction). Fields are
// exported because pkg/hub — where the registry map, the user-WS fanout,
// the status watchdog and the hand-built test servers still live — keeps
// accessing the exact same fields under the exact same mutex as before the
// extraction; pkg/hub aliases the type (type clawConn = claws.Conn).
type Conn struct {
	Mu sync.RWMutex // protects mutable fields below

	ClawID                string
	TenantID              string
	WS                    *websocket.Conn
	Tags                  []string        // cached from DB at registration time for access-control checks
	ContextUsage          int             // 0-100, updated from heartbeats
	GatewayReady          bool            // true once bridge reports gateway session established
	GatewayUnhealthyCount int             // consecutive unhealthy heartbeats
	WorkflowStartPending  bool            // true while initial volume attach / wake is in flight
	WorkflowStartDone     bool            // true once initial volume attach / wake has completed
	StreamingBuf          strings.Builder // accumulates chunks for current in-flight response
	StreamingMsgID        string          // pre-assigned message ID for the current stream
	StreamingSplit        bool            // true once activity has split this turn into multiple persisted segments
	StreamingStartedAt    time.Time       // when the current streaming turn started (zero if not streaming)
	StreamingTimeoutSent  bool            // true once the 12-min timeout message has been injected this turn
	ContextWarningSent    bool            // true once the context-nearly-full warning has been injected this turn
	StreamingFlushedLen   int             // buffer length already persisted by the periodic partial commit
	StreamingFlushedAt    time.Time       // when the partial buffer was last persisted

	// Message queue for when claw is busy processing
	MessageQueue []types.HubMessage // queued messages waiting to be sent

	PendingCheckpointReason string // coalesced checkpoint request to run after current turn
	PendingCheckpointID     string
	PendingCheckpointBy     string
	CheckpointInProgress    bool

	// Status channel for watchdog / progress reporting (second session on bridge)
	StatusConn            *websocket.Conn // separate WS for lightweight status queries
	LastStatusAt          time.Time       // when we last got a status response
	LastUserMessageAt     time.Time       // when the user last sent a message (for idle detection)
	LastStatusBroadcastAt time.Time       // when we last broadcast status to user
}

// initialStatus returns the claw status string to use on bridge registration.
// A nil pointer means the field was absent (old bridge) — treat as ready for backward compat.
func initialStatus(gatewayReady *bool) string {
	if gatewayReady == nil || *gatewayReady {
		return "connected"
	}
	return "starting"
}

func gatewayReadyBool(v *bool) bool {
	return v == nil || *v
}

// BusyLocked reports whether the agent is mid-turn (the former
// isBusyLocked); must be called with the connection lock held.
func (cc *Conn) BusyLocked() bool {
	return !cc.StreamingStartedAt.IsZero() || cc.StreamingMsgID != ""
}

// FinishTurnLocked resets the per-turn streaming state (the former
// finishTurnLocked); must be called with the connection lock held.
func (cc *Conn) FinishTurnLocked() {
	cc.StreamingMsgID = ""
	cc.StreamingBuf.Reset()
	cc.StreamingSplit = false
	cc.StreamingStartedAt = time.Time{}
	cc.StreamingTimeoutSent = false
	cc.ContextWarningSent = false
	cc.StreamingFlushedLen = 0
	cc.StreamingFlushedAt = time.Time{}
}

// AllowWakeBeforeBootstrap reports whether a claw on the given provider may
// be promoted/woken before its bootstrap completed (moved from pkg/hub
// server.go; the hub aliases it for the call sites and tests that stayed).
func AllowWakeBeforeBootstrap(provider string, bootstrapOK int) bool {
	switch provider {
	case "daytona", "replicated", "exedev":
		return bootstrapOK == 1
	default:
		return true
	}
}

// Accessor methods below implement the checkpoints.Conn and workflows.Conn
// dependency interfaces (moved from the pkg/hub checkpoints/workflows
// bridges, where they lived on *clawConn before the type moved here).
// Methods with the Locked suffix must be called with the connection lock
// held (Lock or RLock for getters, Lock for setters).

func (cc *Conn) ID() string { return cc.ClawID }

func (cc *Conn) Lock()    { cc.Mu.Lock() }
func (cc *Conn) Unlock()  { cc.Mu.Unlock() }
func (cc *Conn) RLock()   { cc.Mu.RLock() }
func (cc *Conn) RUnlock() { cc.Mu.RUnlock() }

func (cc *Conn) LastUserMessageAtLocked() time.Time { return cc.LastUserMessageAt }

func (cc *Conn) SetLastUserMessageAtLocked(t time.Time) { cc.LastUserMessageAt = t }

func (cc *Conn) StreamingLocked() bool { return cc.BusyLocked() }

func (cc *Conn) CheckpointInProgressLocked() bool { return cc.CheckpointInProgress }

func (cc *Conn) SetCheckpointInProgressLocked(v bool) { cc.CheckpointInProgress = v }

func (cc *Conn) PendingCheckpointIDLocked() string { return cc.PendingCheckpointID }

func (cc *Conn) PendingCheckpointReasonLocked() string { return cc.PendingCheckpointReason }

func (cc *Conn) SetPendingCheckpointReasonLocked(reason string) {
	cc.PendingCheckpointReason = reason
}

func (cc *Conn) SetPendingCheckpointLocked(id, reason, by string) {
	cc.PendingCheckpointID = id
	cc.PendingCheckpointReason = reason
	cc.PendingCheckpointBy = by
}

func (cc *Conn) AppendMessageQueueLocked(msg types.HubMessage) int {
	cc.MessageQueue = append(cc.MessageQueue, msg)
	return len(cc.MessageQueue)
}

func (cc *Conn) PrependMessageQueueLocked(msg types.HubMessage) int {
	cc.MessageQueue = append([]types.HubMessage{msg}, cc.MessageQueue...)
	return len(cc.MessageQueue)
}

func (cc *Conn) WriteWS(ctx context.Context, msg types.WSMessage) error {
	return wsjson.Write(ctx, cc.WS, msg)
}
