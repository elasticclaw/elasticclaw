package claws

// Per-claw message delivery (hub-role injection and queue draining), moved
// verbatim from pkg/hub server.go.

import (
	"context"
	"time"

	"github.com/google/uuid"
	"nhooyr.io/websocket/wsjson"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// injectHubMessage sends a hub-role message to the claw over its WebSocket
// connection and persists it to the DB so it appears in the message history.
// Hub messages are visually distinct from user messages in the UI.
func (s *Service) injectHubMessage(ctx context.Context, cc *Conn, text string) {
	msg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    cc.ClawID,
		TenantID:  cc.TenantID,
		Role:      "hub",
		Content:   text,
		Format:    "pre",
		CreatedAt: now(),
	}
	_ = s.st.Messages().Insert(ctx, msg)
	_ = wsjson.Write(ctx, cc.WS, types.WSMessage{Type: "message", Payload: msg})
	s.metrics.wsMessage("out", "claw")
	s.broadcastToUsers(cc.TenantID, types.WSMessage{Type: "message", Payload: msg})
}

// InjectHubMessage is the exported entry point for the pkg/hub bridge.
func (s *Service) InjectHubMessage(ctx context.Context, cc *Conn, text string) {
	s.injectHubMessage(ctx, cc, text)
}

// sendNextQueuedMessage checks if there are queued messages for a claw and sends
// the next one if the claw is not currently busy. Must be called with s.mu unlocked.
func (s *Service) sendNextQueuedMessage(cc *Conn) {
	cc.Mu.Lock()

	// Check if there's anything to send
	if len(cc.MessageQueue) == 0 {
		cc.Mu.Unlock()
		return
	}

	// Check if claw is still busy
	if cc.BusyLocked() {
		cc.Mu.Unlock()
		return
	}

	// Send the next queued message - copy fields needed for sending
	msg := cc.MessageQueue[0]
	cc.MessageQueue = cc.MessageQueue[1:]
	remainingCount := len(cc.MessageQueue)
	conn := cc.WS
	tenantID := cc.TenantID
	clawID := cc.ClawID
	cc.LastUserMessageAt = time.Now()
	cc.Mu.Unlock()

	// Send via WebSocket (outside of lock to avoid blocking other goroutines)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := wsjson.Write(ctx, conn, types.WSMessage{Type: "message", Payload: msg})
	if err == nil {
		s.metrics.wsMessage("out", "claw")
	}

	if err != nil {
		// Write failed - re-enqueue the message at the front so it can be retried
		logf("[hub] failed to send queued message to %s: %v, re-enqueueing", clawID[:8], err)
		s.mu.RLock()
		if currentCC, ok := s.claws()[clawID]; ok {
			currentCC.Mu.Lock()
			// Prepend the message back to the front of the queue
			currentCC.MessageQueue = append([]types.HubMessage{msg}, currentCC.MessageQueue...)
			currentCC.Mu.Unlock()
		}
		s.mu.RUnlock()
		return
	}

	// Signal to UI that agent is working
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type: "agent_typing",
		Payload: map[string]string{
			"claw_id": clawID,
			"status":  "typing",
		},
	})

	logf("[hub] sent queued message to %s (%d remaining in queue)", clawID[:8], remainingCount)
}

// SendNextQueuedMessage is the exported entry point for the pkg/hub bridge.
func (s *Service) SendNextQueuedMessage(cc *Conn) {
	s.sendNextQueuedMessage(cc)
}
