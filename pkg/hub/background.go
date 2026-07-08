// Background maintenance loops: provider status polling and the status watchdog.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"fmt"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket/wsjson"
)

// ─── Provider status polling ──────────────────────────────────────────────────

// pollProviderStatus runs forever, polling providers every 30s for VMs in
// non-terminal states and updating claw status accordingly.
func (s *Server) pollProviderStatus() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.syncReplicatedVMs()
	}
}

// pruneAnalytics runs a daily cleanup of factory_analytics rows older than 1 year.
func (s *Server) pruneAnalytics() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.st().Analytics().PruneFactoryAnalytics(context.Background()); err != nil {
			logf("[db] factory analytics prune error: %v", err)
		}
	}
}

// statusWatchdog runs every 2 minutes to check claw health and request status
// updates from the status channel. It also detects silent deaths and alerts the user.
func (s *Server) statusWatchdog() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.checkClawStatus()
	}
}

// checkClawStatus queries active claws, sends status requests via the status channel,
// and detects claws that have gone silent (no status response, no user message recently).
func (s *Server) checkClawStatus() {
	now := time.Now()

	s.mu.RLock()
	var clawIDs []string
	for id := range s.claws {
		clawIDs = append(clawIDs, id)
	}
	s.mu.RUnlock()

	for _, id := range clawIDs {
		s.mu.RLock()
		cc, ok := s.claws[id]
		s.mu.RUnlock()
		if !ok {
			continue
		}

		cc.Mu.RLock()
		lastUserMessageAt := cc.LastUserMessageAt
		lastStatusAt := cc.LastStatusAt
		lastStatusBroadcastAt := cc.LastStatusBroadcastAt
		statusConn := cc.StatusConn
		gatewayReady := cc.GatewayReady
		contextUsage := cc.ContextUsage
		contextWarningSent := cc.ContextWarningSent
		tenantID := cc.TenantID
		cc.Mu.RUnlock()

		// If user sent a message in the last 2 minutes, skip status broadcast
		if now.Sub(lastUserMessageAt) < 2*time.Minute {
			continue
		}

		// If we have a status channel, ping it (hold lock during write)
		if statusConn != nil {
			cc.Mu.RLock()
			sc := cc.StatusConn
			cc.Mu.RUnlock()
			if sc != nil {
				_ = wsjson.Write(context.Background(), sc, types.WSMessage{
					Type: "status_ping",
					Payload: mustJSONRaw(map[string]interface{}{
						"claw_id": id,
						"ts":      now.Unix(),
					}),
				})
			}
		}

		var name string
		_ = s.db.QueryRow(`SELECT name FROM claws WHERE id=?`, id).Scan(&name)

		// Detect silent death: no status response AND no user message for >5 min
		// while the claw is supposedly connected and gateway was ready
		if gatewayReady &&
			now.Sub(lastStatusAt) > 5*time.Minute &&
			now.Sub(lastUserMessageAt) > 5*time.Minute &&
			now.Sub(lastStatusBroadcastAt) > 5*time.Minute {
			msg := fmt.Sprintf("🚨 Agent %s appears unresponsive (no status in 5m). It may have crashed.", name)
			logf("[watchdog] %s", msg)
			// Inject as system message so user sees it in the chat stream
			s.broadcastToUsers(tenantID, types.WSMessage{
				Type: "message",
				Payload: map[string]interface{}{
					"role":    "system",
					"content": msg,
					"claw_id": id,
				},
			})
			// Update lastStatusBroadcastAt under per-claw lock so we don't spam
			cc.Mu.Lock()
			cc.LastStatusBroadcastAt = now
			cc.Mu.Unlock()
		}

		// Context usage warning (>90%) — skip if a streaming turn is in progress
		// so the heartbeat's more-urgent 95% in-streaming warning isn't suppressed.
		cc.Mu.RLock()
		streaming := !cc.StreamingStartedAt.IsZero()
		cc.Mu.RUnlock()
		if contextUsage > 90 && !contextWarningSent && !streaming {
			msg := fmt.Sprintf("⚠️ Agent %s is at %d%% context usage. It should wrap up soon or restart.", name, contextUsage)
			logf("[watchdog] %s", msg)
			s.broadcastToUsers(tenantID, types.WSMessage{
				Type: "message",
				Payload: map[string]interface{}{
					"role":    "system",
					"content": msg,
					"claw_id": id,
				},
			})
			// Update contextWarningSent under per-claw lock
			cc.Mu.Lock()
			cc.ContextWarningSent = true
			cc.Mu.Unlock()
		}
	}
}
