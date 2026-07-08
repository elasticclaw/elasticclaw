// User-facing WebSocket connections, broadcast fan-out, and JSON response helpers.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"encoding/json"
	"net/http"

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

// handleDebugClaws dumps the in-memory claw state (auth required).
func (s *Server) handleDebugClaws(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	type debugClaw struct {
		ID           string `json:"id"`
		GatewayReady bool   `json:"gateway_ready"`
		ContextUsage int    `json:"context_usage"`
	}
	out := make([]debugClaw, 0, len(s.claws))
	for id, cc := range s.claws {
		out = append(out, debugClaw{ID: id, GatewayReady: cc.GatewayReady, ContextUsage: cc.ContextUsage})
	}
	s.mu.RUnlock()
	jsonOK(w, out)
}

// ─── Claw WebSocket ───────────────────────────────────────────────────────────

// ─── User WebSocket ───────────────────────────────────────────────────────────

func (s *Server) handleUserWS(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	ghLogin := githubLoginFromContext(r.Context())
	var accessCfg *types.AccessConfig
	if ghLogin != "" {
		s.mu.RLock()
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()
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

	s.mu.Lock()
	s.users[connID] = uc
	s.mu.Unlock()

	ctx := r.Context()
	defer func() {
		s.mu.Lock()
		delete(s.users, connID)
		s.mu.Unlock()
	}()

	// Send current claw statuses immediately on connect.
	// First, emit DB rows for claws not yet bridge-connected (provisioning/starting/error).
	type dbClaw struct {
		id, name, status, tagsJSON, bootstrapStatus, bootstrapDiagnostic, githubIssueID string
	}
	var dbClaws []dbClaw
	rows, _ := s.db.QueryContext(ctx, `SELECT id, name, status, COALESCE(tags,'[]'), COALESCE(bootstrap_status,''), COALESCE(bootstrap_diagnostic,''), COALESCE(github_issue_id,'') FROM claws WHERE tenant_id=? AND status NOT IN ('offline')`, tenantID)
	if rows != nil {
		for rows.Next() {
			var c dbClaw
			_ = rows.Scan(&c.id, &c.name, &c.status, &c.tagsJSON, &c.bootstrapStatus, &c.bootstrapDiagnostic, &c.githubIssueID)
			dbClaws = append(dbClaws, c)
		}
		_ = rows.Close()
	}
	s.mu.RLock()
	connectedIDs := make(map[string]bool)
	for _, cc := range s.claws {
		if cc.TenantID != tenantID {
			continue
		}
		// Apply tag-based view filter for GitHub OAuth users
		if ghLogin != "" && !canViewClaw(accessCfg, ghLogin, cc.Tags) {
			continue
		}
		connectedIDs[cc.ClawID] = true
		status := "connected"
		if !cc.GatewayReady {
			status = "starting"
		}
		_ = wsjson.Write(ctx, conn, types.WSMessage{
			Type: "claw_status",
			Payload: map[string]interface{}{
				"claw_id":       cc.ClawID,
				"status":        status,
				"context_usage": cc.ContextUsage,
			},
		})
	}
	s.mu.RUnlock()
	// Emit DB-only claws (still bootstrapping, not yet bridge-connected)
	for _, c := range dbClaws {
		if connectedIDs[c.id] {
			continue // already sent above
		}
		// Apply tag-based view filter for GitHub OAuth users
		if ghLogin != "" {
			var clawTags []string
			_ = json.Unmarshal([]byte(c.tagsJSON), &clawTags)
			if !canViewClaw(accessCfg, ghLogin, clawTags) {
				continue
			}
		}
		_ = wsjson.Write(ctx, conn, types.WSMessage{
			Type: "claw_status",
			Payload: map[string]interface{}{
				"claw_id":              c.id,
				"name":                 c.name,
				"status":               c.status, // provisioning / starting / error
				"bootstrap_status":     c.bootstrapStatus,
				"bootstrap_diagnostic": c.bootstrapDiagnostic,
				"github_issue_id":      c.githubIssueID,
				"github_issue_url":     githubIssueURL(c.githubIssueID),
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
				var tagsJSON string
				_ = s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, hm.ClawID, tenantID).Scan(&tagsJSON)
				var clawTags []string
				_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
				var currentAccessCfg *types.AccessConfig
				s.mu.RLock()
				if s.hubCfg.Auth != nil {
					currentAccessCfg = s.hubCfg.Auth.Access
				}
				s.mu.RUnlock()
				if !canInteractWithClaw(currentAccessCfg, ghLogin, clawTags) {
					continue
				}
			}
			hm.ID = uuid.New().String()
			hm.TenantID = tenantID
			hm.Role = "user"
			hm.CreatedAt = now()
			_, _ = s.db.Exec(
				`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
				hm.ID, hm.ClawID, hm.TenantID, hm.Role, hm.Content, hm.CreatedAt,
			)
			s.recordTaskRunDashboardMessage(hm.ClawID, ghLogin, hm.ID)
			s.mu.RLock()
			cc := s.claws[hm.ClawID]
			s.mu.RUnlock()
			if cc != nil {
				_ = wsjson.Write(ctx, cc.WS, types.WSMessage{Type: "message", Payload: hm})
				s.metrics.wsMessage("out", "claw")
			}
		}
	}
}

func (s *Server) broadcastToUsers(tenantID string, msg types.WSMessage) {
	for _, uc := range s.broadcastRecipients(tenantID, msg) {
		_ = wsjson.Write(context.Background(), uc.conn, msg)
		s.metrics.wsMessage("out", "user")
	}
}

func (s *Server) broadcastRecipients(tenantID string, msg types.WSMessage) []*userConn {
	clawID := clawIDFromWSMessage(msg)
	clawTags := []string(nil)
	if clawID != "" {
		clawTags = s.clawTagsForBroadcast(tenantID, clawID)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	recipients := make([]*userConn, 0, len(s.users))
	for _, uc := range s.users {
		if uc.tenantID != tenantID {
			continue
		}
		if uc.githubLogin != "" && clawID != "" {
			var accessCfg *types.AccessConfig
			if s.hubCfg.Auth != nil {
				accessCfg = s.hubCfg.Auth.Access
			}
			if !canViewClaw(accessCfg, uc.githubLogin, clawTags) {
				continue
			}
		}
		recipients = append(recipients, uc)
	}
	return recipients
}

func (s *Server) clawTagsForBroadcast(tenantID, clawID string) []string {
	s.mu.RLock()
	if cc := s.claws[clawID]; cc != nil && cc.TenantID == tenantID {
		tags := append([]string(nil), cc.Tags...)
		s.mu.RUnlock()
		return tags
	}
	s.mu.RUnlock()

	var tagsJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSON)
	var tags []string
	_ = json.Unmarshal([]byte(tagsJSON), &tags)
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
