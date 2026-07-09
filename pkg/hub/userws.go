// User-facing WebSocket connections, broadcast fan-out, and JSON response helpers.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type userConn struct {
	conn        *websocket.Conn
	send        func(context.Context, types.WSMessage) error
	tenantID    string
	githubLogin string
}

// userRegistry is the user-session registry: conn_id -> *userConn, guarded
// by its own RWMutex (per-subsystem locking, phase-2 item 2.3). The zero
// value is ready to use; write paths lazily initialize the map. Its lock is
// a leaf lock: no other subsystem lock is acquired while holding it.
type userRegistry struct {
	mu    sync.RWMutex
	conns map[string]*userConn
}

func (r *userRegistry) set(id string, uc *userConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns == nil {
		r.conns = make(map[string]*userConn)
	}
	r.conns[id] = uc
}

func (r *userRegistry) delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, id)
}

// snapshot returns a copy of the current user connections.
func (r *userRegistry) snapshot() []*userConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*userConn, 0, len(r.conns))
	for _, uc := range r.conns {
		out = append(out, uc)
	}
	return out
}

// handleDebugClaws dumps the in-memory claw state (auth required).
func (s *Server) handleDebugClaws(w http.ResponseWriter, r *http.Request) {
	type debugClaw struct {
		ID           string `json:"id"`
		GatewayReady bool   `json:"gateway_ready"`
		ContextUsage int    `json:"context_usage"`
	}
	var out []debugClaw
	s.clawReg.DoRead(func(conns map[string]*clawConn) {
		out = make([]debugClaw, 0, len(conns))
		for id, cc := range conns {
			cc.Mu.RLock()
			out = append(out, debugClaw{ID: id, GatewayReady: cc.GatewayReady, ContextUsage: cc.ContextUsage})
			cc.Mu.RUnlock()
		}
	})
	jsonOK(w, out)
}

// ─── Claw WebSocket ───────────────────────────────────────────────────────────

// ─── User WebSocket ───────────────────────────────────────────────────────────

func (s *Server) handleUserWS(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	ghLogin := githubLoginFromContext(r.Context())
	var accessCfg *types.AccessConfig
	if ghLogin != "" {
		s.cfgMu.RLock()
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.cfgMu.RUnlock()
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}

	uc := &userConn{
		conn:        conn,
		tenantID:    tenantID,
		githubLogin: ghLogin,
	}
	connID := uuid.New().String()

	s.userReg.set(connID, uc)

	ctx := r.Context()
	defer s.userReg.delete(connID)

	// Send current claw statuses immediately on connect.
	// First, emit DB rows for claws not yet bridge-connected (provisioning/starting/error).
	dbClaws := s.st().Claws().ListStatusRows(ctx, tenantID)
	connectedIDs := make(map[string]bool)
	s.clawReg.DoRead(func(conns map[string]*clawConn) {
		for _, cc := range conns {
			if cc.TenantID != tenantID {
				continue
			}
			// Apply tag-based view filter for GitHub OAuth users
			if ghLogin != "" && !canViewClaw(accessCfg, ghLogin, cc.Tags) {
				continue
			}
			connectedIDs[cc.ClawID] = true
			// Snapshot live fields under the per-connection lock
			// (heartbeats mutate them); don't hold it during the write.
			cc.Mu.RLock()
			gatewayReady := cc.GatewayReady
			contextUsage := cc.ContextUsage
			cc.Mu.RUnlock()
			status := "connected"
			if !gatewayReady {
				status = "starting"
			}
			_ = wsjson.Write(ctx, conn, types.WSMessage{
				Type: "claw_status",
				Payload: map[string]interface{}{
					"claw_id":       cc.ClawID,
					"status":        status,
					"context_usage": contextUsage,
				},
			})
		}
	})
	// Emit DB-only claws (still bootstrapping, not yet bridge-connected)
	for _, c := range dbClaws {
		if connectedIDs[c.ID] {
			continue // already sent above
		}
		// Apply tag-based view filter for GitHub OAuth users
		if ghLogin != "" {
			var clawTags []string
			_ = json.Unmarshal([]byte(c.TagsJSON), &clawTags)
			if !canViewClaw(accessCfg, ghLogin, clawTags) {
				continue
			}
		}
		_ = wsjson.Write(ctx, conn, types.WSMessage{
			Type: "claw_status",
			Payload: map[string]interface{}{
				"claw_id":              c.ID,
				"name":                 c.Name,
				"status":               c.Status, // provisioning / starting / error
				"bootstrap_status":     c.BootstrapStatus,
				"bootstrap_diagnostic": c.BootstrapDiagnostic,
				"github_issue_id":      c.GitHubIssueID,
				"github_issue_url":     githubIssueURL(c.GitHubIssueID),
			},
		})
	}

	// Read loop (user sends messages via REST, but we keep WS open for server-push)
	for {
		var msg types.WSMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		s.metrics.wsMessage("in", "user")
		// Forward user messages to the specified claw
		if msg.Type == "message" {
			payload, _ := json.Marshal(msg.Payload)
			var hm types.HubMessage
			if err := json.Unmarshal(payload, &hm); err != nil {
				continue
			}
			// Apply tag-based interact filter for GitHub OAuth users
			if ghLogin != "" {
				clawTags, _ := s.st().Claws().Tags(ctx, hm.ClawID, tenantID)
				var currentAccessCfg *types.AccessConfig
				s.cfgMu.RLock()
				if s.hubCfg.Auth != nil {
					currentAccessCfg = s.hubCfg.Auth.Access
				}
				s.cfgMu.RUnlock()
				if !canInteractWithClaw(currentAccessCfg, ghLogin, clawTags) {
					continue
				}
			}
			hm.ID = uuid.New().String()
			hm.TenantID = tenantID
			hm.Role = "user"
			hm.CreatedAt = now()
			_ = s.st().Messages().Insert(ctx, hm)
			s.recordTaskRunDashboardMessage(hm.ClawID, ghLogin, hm.ID)
			cc := s.clawReg.Lookup(hm.ClawID)
			if cc != nil {
				_ = wsjson.Write(ctx, cc.WS, types.WSMessage{Type: "message", Payload: hm})
				s.metrics.wsMessage("out", "claw")
			}
		}
	}
}

func (s *Server) broadcastToUsers(tenantID string, msg types.WSMessage) {
	for _, uc := range s.broadcastRecipients(tenantID, msg) {
		_ = wsjson.Write(s.base(), uc.conn, msg)
		s.metrics.wsMessage("out", "user")
	}
}

func (s *Server) broadcastRecipients(tenantID string, msg types.WSMessage) []*userConn {
	clawID := clawIDFromWSMessage(msg)
	clawTags := []string(nil)
	if clawID != "" {
		clawTags = s.clawTagsForBroadcast(tenantID, clawID)
	}

	s.cfgMu.RLock()
	var accessCfg *types.AccessConfig
	if s.hubCfg.Auth != nil {
		accessCfg = s.hubCfg.Auth.Access
	}
	s.cfgMu.RUnlock()

	conns := s.userReg.snapshot()
	recipients := make([]*userConn, 0, len(conns))
	for _, uc := range conns {
		if uc.tenantID != tenantID {
			continue
		}
		if uc.githubLogin != "" && clawID != "" {
			if !canViewClaw(accessCfg, uc.githubLogin, clawTags) {
				continue
			}
		}
		recipients = append(recipients, uc)
	}
	return recipients
}

func (s *Server) clawTagsForBroadcast(tenantID, clawID string) []string {
	if cc := s.clawReg.Lookup(clawID); cc != nil && cc.TenantID == tenantID {
		return append([]string(nil), cc.Tags...)
	}

	tags, _ := s.st().Claws().Tags(s.base(), clawID, tenantID)
	return tags
}

func clawIDFromWSMessage(msg types.WSMessage) string {
	payload, err := json.Marshal(msg.Payload)
	if err != nil {
		return ""
	}
	var envelope struct {
		ClawID string `json:"claw_id"`
	}
	_ = json.Unmarshal(payload, &envelope)
	return envelope.ClawID
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func mustJSONRaw(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}

// jsonOK, jsonError and writeErr are thin aliases over the httpserver
// response helpers, kept so the ~250 existing call sites in this package do
// not churn during the httpserver extraction. New code should call the
// httpserver package directly; handlers drop the alias as they migrate to
// their own subpackages.
func jsonOK(w http.ResponseWriter, v interface{}) {
	httpserver.JSONOK(w, v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	httpserver.JSONError(w, status, msg)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	httpserver.WriteErr(w, status, code, msg)
}
