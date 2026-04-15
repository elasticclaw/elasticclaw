package hub

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	daytona "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	replicatedpkg "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
	vercelProvider "github.com/elasticclaw/elasticclaw/pkg/provider/vercel"
	"github.com/google/uuid"
	gossh "golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Server is the ElasticClaw hub.
type Server struct {
	db       *sql.DB
	addr     string
	hubCfg   *types.HubConfig
	identity *HubIdentity

	mu    sync.RWMutex
	claws map[string]*clawConn // claw_id -> conn
	users map[string]*userConn // tenant_id -> []conn (broadcast)
}

type clawConn struct {
	id           string
	tenantID     string
	conn         *websocket.Conn
	contextUsage int  // 0-100, updated from heartbeats
	gatewayReady bool // true once bridge reports gateway session established
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

type userConn struct {
	conn     *websocket.Conn
	tenantID string
}

// NewServer creates a hub server backed by a SQLite database at dbPath.
// identityDir is the directory where the hub's SSH keypair is stored (created if absent).
func NewServer(addr, dbPath, identityDir string, hubCfg *types.HubConfig) (*Server, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	if hubCfg == nil {
		hubCfg = &types.HubConfig{}
	}
	id, err := LoadOrCreateIdentity(identityDir)
	if err != nil {
		return nil, fmt.Errorf("hub identity: %w", err)
	}
	log.Printf("Hub SSH public key:\n%s", id.PublicKey)
	srv := &Server{
		db:       db,
		addr:     addr,
		hubCfg:   hubCfg,
		identity: id,
		claws:    make(map[string]*clawConn),
		users:    make(map[string]*userConn),
	}

	// Start background poller to keep provider VM status fresh
	go srv.pollProviderStatus()

	return srv, nil
}

// Run starts the HTTP server (blocking).
func (s *Server) Run() error {
	mux := http.NewServeMux()

	// Claw WebSocket
	mux.HandleFunc("/claw/ws", s.handleClawWS)

	// Browser WebSocket
	mux.HandleFunc("/api/ws", s.handleUserWS)

	// REST API
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/claws", s.withAuth(s.handleClaws))
	mux.HandleFunc("/api/claws/", s.withAuth(s.handleClawDetail))
	mux.HandleFunc("/api/terminal/", s.handleTerminal)
	mux.HandleFunc("/api/github/token/", s.handleGitHubToken) // credential helper endpoint (claw-token auth)
	mux.HandleFunc("/api/messages/", s.withAuth(s.handleMessages))

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Debug: dump in-memory claw state (auth required)
	mux.HandleFunc("/api/debug/claws", s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		type debugClaw struct {
			ID           string `json:"id"`
			GatewayReady bool   `json:"gateway_ready"`
			ContextUsage int    `json:"context_usage"`
		}
		out := make([]debugClaw, 0, len(s.claws))
		for id, cc := range s.claws {
			out = append(out, debugClaw{ID: id, GatewayReady: cc.gatewayReady, ContextUsage: cc.contextUsage})
		}
		s.mu.RUnlock()
		jsonOK(w, out)
	}))

	// Connect to relay if configured
	if s.hubCfg.RelayURL != "" {
		hubID := HubID(s.identity.PublicKey)
		relayToken := RelayToken(s.hubCfg.RelaySecret, hubID, s.hubCfg.ClawToken)
		log.Printf("[relay] hub ID: %s", hubID[:8]+"...")
		log.Printf("[relay] connecting to %s", s.hubCfg.RelayURL)
		go s.connectRelay(context.Background(), s.hubCfg.RelayURL, hubID, relayToken)
	}

	log.Printf("ElasticClaw Hub listening on %s", s.addr)
	return http.ListenAndServe(s.addr, corsMiddleware(mux))
}

// corsMiddleware adds permissive CORS headers so the web UI can connect
// from any origin (browser same-origin restrictions apply to REST + WS).
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Auth ────────────────────────────────────────────────────────────────────

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		tenantID, err := s.tenantByToken(token)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxTenantKey{}, tenantID))
		next(w, r)
	}
}

type ctxTenantKey struct{}

func tenantFromCtx(r *http.Request) string {
	v, _ := r.Context().Value(ctxTenantKey{}).(string)
	return v
}

func (s *Server) tenantByToken(token string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM tenants WHERE token = ?`, token).Scan(&id)
	return id, err
}

func (s *Server) tenantByClawToken(token string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM tenants WHERE claw_token = ?`, token).Scan(&id)
	return id, err
}

// ─── REST handlers ────────────────────────────────────────────────────────────

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	tenantID, err := s.tenantByToken(body.Token)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	jsonOK(w, map[string]string{"tenant_id": tenantID, "token": body.Token})
}

func (s *Server) handleClaws(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)

	if r.Method == http.MethodPost {
		s.handleCreateClaw(w, r, tenantID)
		return
	}

	rows, err := s.db.Query(
		`SELECT id, name, template, status, last_seen, created_at, ssh_host, ssh_port, ssh_user FROM claws WHERE tenant_id = ? ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		log.Printf("handleClaws query error: %v", err)
		http.Error(w, fmt.Sprintf("db error: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var out []types.Claw
	for rows.Next() {
		var c types.Claw
		var lastSeen sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.Template, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser); err != nil {
			continue
		}
		c.TenantID = tenantID
		if lastSeen.Valid {
			c.LastSeen = lastSeen.Time
		}
		s.mu.RLock()
		cc, online := s.claws[c.ID]
		s.mu.RUnlock()
		if online {
			// Claw is currently connected — show live status
			if cc.gatewayReady {
				c.Status = "connected"
			} else {
				c.Status = "starting"
			}
			c.ContextUsage = cc.contextUsage
		} else if c.Status != "provisioning" && c.Status != "error" {
			// Not currently connected and not in a terminal provisioning state —
			// DB status is stale (e.g. 'starting'/'connected' from before hub restart)
			c.Status = "offline"
		}
		out = append(out, c)
	}
	if out == nil {
		out = []types.Claw{}
	}
	jsonOK(w, out)
}

func (s *Server) handleCreateClaw(w http.ResponseWriter, r *http.Request, tenantID string) {
	var req types.CreateClawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Provider == "" {
		http.Error(w, "name and provider are required", http.StatusBadRequest)
		return
	}

	// Check provider is configured
	provCfg, ok := s.hubCfg.Providers[req.Provider]
	if !ok {
		http.Error(w, fmt.Sprintf("provider %q is not configured on this hub", req.Provider), http.StatusUnprocessableEntity)
		return
	}

	// Pre-register claw row so it exists before the workspace boots
	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(req.Files)

	// Store GitHub repos config from template if present
	var githubReposJSON string = "[]"
	if req.GitHub != nil && len(req.GitHub.Repos) > 0 {
		b, _ := json.Marshal(req.GitHub.Repos)
		githubReposJSON = string(b)
	}

	// Store Linear workspace label from template if present
	var linearWorkspace string
	if req.Linear != nil {
		linearWorkspace = req.Linear.Workspace
	}

	_, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,'provisioning',?)`,
		clawID, tenantID, req.Name, req.TemplateName, req.Provider, req.DefaultModel, string(filesJSON),
		githubReposJSON, linearWorkspace, now(),
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	// Build env to inject: hub connection info so the claw can register back
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":   s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":   clawID,
		"ELASTICCLAW_CLAW_TOKEN": s.hubCfg.ClawToken,
	}
	for k, v := range req.Env {
		env[k] = v
	}

	// Convert string files to bytes for the provider
	templateFiles := make(map[string][]byte, len(req.Files))
	for k, v := range req.Files {
		templateFiles[k] = []byte(v)
	}

	// Provision asynchronously so the HTTP request returns quickly
	go func() {
		log.Printf("Provisioning claw %s (%s) via %s...", req.Name, clawID, req.Provider)
		ctx := context.Background()
		var provErr error

		switch req.Provider {
		case "daytona":
			provErr = s.provisionDaytona(ctx, clawID, req, provCfg, templateFiles, env)
		case "vercel":
			provErr = s.provisionVercel(ctx, clawID, req, provCfg, templateFiles, env)
		case "local":
			provErr = s.provisionLocal(ctx, clawID, req, templateFiles, env)
		case "replicated":
			provErr = s.provisionReplicated(ctx, clawID, req, provCfg, env)
		default:
			provErr = fmt.Errorf("unsupported provider: %s", req.Provider)
		}

		if provErr != nil {
			log.Printf("provisioning failed for claw %s: %v", clawID, provErr)
			_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
			s.broadcastToUsers(tenantID, types.WSMessage{
				Type:    "claw_error",
				Payload: map[string]string{"claw_id": clawID, "error": provErr.Error()},
			})
		}
	}()

	claw := types.Claw{
		ID: clawID, TenantID: tenantID, Name: req.Name,
		Template: req.TemplateName, Status: "provisioning", CreatedAt: now(),
	}
	w.WriteHeader(http.StatusAccepted)
	jsonOK(w, claw)
}

func (s *Server) handleClawDetail(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	clawID := strings.TrimPrefix(r.URL.Path, "/api/claws/")

	if r.Method == http.MethodDelete {
		// Look up provider info before deleting so we can terminate the VM
		var provider, providerID string
		_ = s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&provider, &providerID)

		// Delete messages first (FK constraint)
		_, _ = s.db.Exec(`DELETE FROM messages WHERE claw_id = ?`, clawID)
		_, err := s.db.Exec(`DELETE FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID)
		if err != nil {
			log.Printf("kill: db delete error for claw %s: %v", clawID, err)
			http.Error(w, fmt.Sprintf("db error: %v", err), http.StatusInternalServerError)
			return
		}
		// Disconnect WebSocket if online
		s.mu.Lock()
		if cc, ok := s.claws[clawID]; ok {
			cc.conn.Close(websocket.StatusNormalClosure, "killed")
			delete(s.claws, clawID)
		}
		s.mu.Unlock()
		// Terminate the provider VM asynchronously
		if provider == "replicated" && providerID != "" {
			go s.terminateReplicatedVM(providerID)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var c types.Claw
	var lastSeen sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, name, template, status, last_seen, created_at, ssh_host, ssh_port, ssh_user FROM claws WHERE id = ? AND tenant_id = ?`,
		clawID, tenantID,
	).Scan(&c.ID, &c.Name, &c.Template, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	c.TenantID = tenantID
	if lastSeen.Valid {
		c.LastSeen = lastSeen.Time
	}
	s.mu.RLock()
	cc, online := s.claws[c.ID]
	s.mu.RUnlock()
	if online {
		if cc.gatewayReady {
			c.Status = "connected"
		} else {
			c.Status = "starting"
		}
		c.ContextUsage = cc.contextUsage
	} else if c.Status != "provisioning" && c.Status != "error" {
		c.Status = "offline"
	}
	jsonOK(w, c)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	clawID := strings.TrimPrefix(r.URL.Path, "/api/messages/")

	if r.Method == http.MethodPost {
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		msg := types.HubMessage{
			ID: uuid.New().String(), ClawID: clawID, TenantID: tenantID,
			Role: "user", Content: body.Content, CreatedAt: now(),
		}
		if _, err := s.db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
			msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.CreatedAt,
		); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		// Forward to claw if connected
		s.mu.RLock()
		cc := s.claws[clawID]
		s.mu.RUnlock()
		if cc != nil {
			_ = wsjson.Write(r.Context(), cc.conn, types.WSMessage{Type: "message", Payload: msg})
		}
		jsonOK(w, msg)
		return
	}

	rows, err := s.db.Query(
		`SELECT id, claw_id, tenant_id, role, content, created_at FROM messages WHERE claw_id = ? AND tenant_id = ? ORDER BY created_at ASC`,
		clawID, tenantID,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var msgs []types.HubMessage
	for rows.Next() {
		var m types.HubMessage
		if err := rows.Scan(&m.ID, &m.ClawID, &m.TenantID, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []types.HubMessage{}
	}
	jsonOK(w, msgs)
}

// ─── Claw WebSocket ───────────────────────────────────────────────────────────

func (s *Server) handleClawWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}

	ctx := r.Context()

	// First message must be registration
	var reg types.WSMessage
	if err := wsjson.Read(ctx, conn, &reg); err != nil || reg.Type != "register" {
		conn.Close(websocket.StatusPolicyViolation, "expected register")
		return
	}

	payload, _ := json.Marshal(reg.Payload)
	var rp types.RegisterPayload
	if err := json.Unmarshal(payload, &rp); err != nil {
		conn.Close(websocket.StatusPolicyViolation, "invalid register payload")
		return
	}

	tenantID, err := s.tenantByClawToken(rp.Token)
	if err != nil {
		conn.Close(websocket.StatusPolicyViolation, "invalid token")
		return
	}

	clawID := rp.ClawID
	if clawID == "" {
		clawID = uuid.New().String()
	}

	// Upsert claw
	_, _ = s.db.Exec(
		`INSERT INTO claws(id,tenant_id,name,template,status,last_seen,created_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, template=excluded.template, status=excluded.status, last_seen=excluded.last_seen`,
		clawID, tenantID, rp.Name, rp.Template, initialStatus(rp.GatewayReady), now(), now(),
	)

	cc := &clawConn{id: clawID, tenantID: tenantID, conn: conn, gatewayReady: gatewayReadyBool(rp.GatewayReady)}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()

	log.Printf("[bridge] ✓ connected: %s (%s) gateway_ready=%v", rp.Name, clawID[:8], cc.gatewayReady)

	// Ack
	_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "registered", Payload: map[string]string{"claw_id": clawID}})

	// Broadcast initial status to user sessions
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": initialStatus(rp.GatewayReady)}})

	// Read loop — claw sends messages back to users
	defer func() {
		s.mu.Lock()
		delete(s.claws, clawID)
		s.mu.Unlock()
		_, _ = s.db.Exec(`UPDATE claws SET status='offline', last_seen=? WHERE id=?`, now(), clawID)
		s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": "offline"}})
		log.Printf("[bridge] ✗ disconnected: %s (%s)", rp.Name, clawID[:8])
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.db.Exec(`UPDATE claws SET last_seen=? WHERE id=?`, now(), clawID)
		default:
			var msg types.WSMessage
			conn.SetReadLimit(1 << 20) // 1MB
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			if msg.Type == "heartbeat" {
				payload, _ := json.Marshal(msg.Payload)
				var hb struct {
					GatewayHealthy bool  `json:"gateway_healthy"`
					GatewayReady   *bool `json:"gateway_ready,omitempty"`
					ContextUsage   int   `json:"context_usage"`
				}
				if err := json.Unmarshal(payload, &hb); err == nil {
					log.Printf("heartbeat from %s: gateway_healthy=%v gateway_ready=%v context_usage=%d", clawID[:8], hb.GatewayHealthy, gatewayReadyBool(hb.GatewayReady), hb.ContextUsage)
					s.mu.Lock()
					if cc, ok := s.claws[clawID]; ok {
						cc.contextUsage = hb.ContextUsage
						// Promote from 'starting' to 'connected' once gateway is ready.
						// nil means field absent (old bridge) — treat as ready.
						if gatewayReadyBool(hb.GatewayReady) && !cc.gatewayReady {
							cc.gatewayReady = true
							_, _ = s.db.Exec(`UPDATE claws SET status='connected' WHERE id=?`, clawID)
							s.broadcastToUsers(tenantID, types.WSMessage{
								Type:    "claw_status",
								Payload: map[string]string{"claw_id": clawID, "status": "connected"},
							})
							log.Printf("[bridge] ✓ ready: %s (%s)", rp.Name, clawID[:8])
						}
					}
					s.mu.Unlock()
				}
			} else if msg.Type == "chunk" {
				// Streaming chunk — forward to users immediately without persisting
				payload, _ := json.Marshal(msg.Payload)
				var chunk struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal(payload, &chunk); err == nil && chunk.Content != "" {
					s.broadcastToUsers(tenantID, types.WSMessage{
						Type: "chunk",
						Payload: map[string]string{"claw_id": clawID, "content": chunk.Content},
					})
				}
			} else if msg.Type == "message" {
				// Complete message — store and forward to users
				payload, _ := json.Marshal(msg.Payload)
				var hm types.HubMessage
				if err := json.Unmarshal(payload, &hm); err != nil {
					continue
				}
				hm.ID = uuid.New().String()
				hm.ClawID = clawID
				hm.TenantID = tenantID
				hm.Role = "claw"
				hm.CreatedAt = now()
				_, _ = s.db.Exec(
					`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
					hm.ID, hm.ClawID, hm.TenantID, hm.Role, hm.Content, hm.CreatedAt,
				)
				s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: hm})
			}
		}
	}
}

// ─── User WebSocket ───────────────────────────────────────────────────────────

func (s *Server) handleUserWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	tenantID, err := s.tenantByToken(token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}

	uc := &userConn{conn: conn, tenantID: tenantID}
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

	// Send current connected-claw statuses immediately so the client
	// doesn't have to wait for the next event to know who is online.
	s.mu.RLock()
	for _, cc := range s.claws {
		if cc.tenantID != tenantID {
			continue
		}
		status := "connected"
		if !cc.gatewayReady {
			status = "starting"
		}
		_ = wsjson.Write(ctx, conn, types.WSMessage{
			Type: "claw_status",
			Payload: map[string]interface{}{
				"claw_id":       cc.id,
				"status":        status,
				"context_usage": cc.contextUsage,
			},
		})
	}
	s.mu.RUnlock()

	// Read loop (user sends messages via REST, but we keep WS open for server-push)
	for {
		var msg types.WSMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		// Forward user messages to the specified claw
		if msg.Type == "message" {
			payload, _ := json.Marshal(msg.Payload)
			var hm types.HubMessage
			if err := json.Unmarshal(payload, &hm); err != nil {
				continue
			}
			hm.ID = uuid.New().String()
			hm.TenantID = tenantID
			hm.Role = "user"
			hm.CreatedAt = now()
			_, _ = s.db.Exec(
				`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
				hm.ID, hm.ClawID, hm.TenantID, hm.Role, hm.Content, hm.CreatedAt,
			)
			s.mu.RLock()
			cc := s.claws[hm.ClawID]
			s.mu.RUnlock()
			if cc != nil {
				_ = wsjson.Write(ctx, cc.conn, types.WSMessage{Type: "message", Payload: hm})
			}
		}
	}
}

func (s *Server) broadcastToUsers(tenantID string, msg types.WSMessage) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, uc := range s.users {
		if uc.tenantID == tenantID {
			_ = wsjson.Write(context.Background(), uc.conn, msg)
		}
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Provision creates a default tenant (for alpha single-user setup).
func (s *Server) Provision(token, clawToken string) (string, error) {
	id := uuid.New().String()
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		id, "default", token, clawToken, now(),
	)
	if err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	// Return existing if token already exists
	var existingID string
	_ = s.db.QueryRow(`SELECT id FROM tenants WHERE token = ?`, token).Scan(&existingID)
	if existingID != "" {
		return existingID, nil
	}
	return id, nil
}

// ─── Provisioning ─────────────────────────────────────────────────────────────

func (s *Server) provisionDaytona(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		return fmt.Errorf("daytona init: %w", err)
	}
	// Resolve snapshot: template snapshot > hub default_snapshot
	snapshot := req.Snapshot
	if snapshot == "" {
		snapshot = cfg.DefaultSnapshot
	}
	createReq := types.CreateRequest{
		Name:          req.Name,
		FromImage:     snapshot, // Daytona: snapshot name (e.g. "daytona-medium")
		TemplateFiles: files,
		Env:           env,
	}
	instance, err := p.Create(ctx, createReq)
	if err != nil {
		return fmt.Errorf("daytona create: %w", err)
	}
	log.Printf("daytona workspace created: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='daytona', provider_id=? WHERE id=?`, instance.ID, clawID)

	// Bootstrap: install OpenClaw + claw-bridge via exec
	go func() {
		if err := s.bootstrapDaytona(context.Background(), clawID, instance.ID, p, env); err != nil {
			log.Printf("daytona bootstrap failed for claw %s: %v", clawID, err)
			_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
		}
	}()
	return nil
}

func (s *Server) bootstrapDaytona(ctx context.Context, clawID, instanceID string, p *daytona.Provider, env map[string]string) error {
	log.Printf("[daytona] bootstrapping claw %s (instance %s)", clawID, instanceID)

	exec := func(label string, timeout time.Duration, cmd string) error {
		log.Printf("[daytona] %s...", label)
		// Prefix HOME so commands run in the sandbox user's home, not the caller's
		result, err := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", "export HOME=/home/daytona; " + cmd}, timeout)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("%s failed (exit %d): %s", label, result.ExitCode, result.Stdout)
		}
		log.Printf("[daytona] %s done", label)
		return nil
	}

	// Step 1: Upgrade OpenClaw to latest.
	// Run install in background and poll — avoids the 60s HTTP client timeout
	// that kills synchronous long-running commands.
	if err := exec("prepare openclaw upgrade", 30*time.Second,
		`export NVM_DIR="/usr/local/share/nvm"; \
[ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"; \
sudo chown -R daytona:daytona "$NVM_DIR/current/lib" 2>/dev/null || true; \
npm install -g openclaw@latest --ignore-scripts --force > /tmp/openclaw-install.log 2>&1 & \
echo $! > /tmp/openclaw-install.pid && echo 'install started'`); err != nil {
		return err
	}

	// Poll until npm install finishes (up to 5 min, checking every 15s)
	installed := false
	for i := 0; i < 20; i++ {
		time.Sleep(15 * time.Second)
		checkCmd := "PID=$(cat /tmp/openclaw-install.pid 2>/dev/null); " +
			"if [ -n \"$PID\" ] && kill -0 \"$PID\" 2>/dev/null; then echo running; " +
			"else echo done; fi"
		r, err := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", checkCmd}, 20*time.Second)
		if err != nil {
			log.Printf("[daytona] poll error: %v", err)
			continue
		}
		if strings.TrimSpace(r.Stdout) == "done" {
			log.Printf("[daytona] openclaw install complete")
			installed = true
			break
		}
		log.Printf("[daytona] waiting for openclaw install... (%d/20)", i+1)
	}
	if !installed {
		log.Printf("[daytona] warning: openclaw install poll timed out, proceeding anyway")
	}

	if err := exec("verify openclaw", 20*time.Second,
		"export HOME=/home/daytona; openclaw --version"); err != nil {
		return err
	}

	// Step 2: Onboard (configure OpenClaw)
	if err := exec("onboard openclaw", 2*time.Minute,
		"openclaw onboard --non-interactive --accept-risk --skip-daemon 2>&1 || true"); err != nil {
		return err
	}

	// Step 2b: Patch OpenClaw config with model + LLM API keys
	anthropicKey := env["ANTHROPIC_API_KEY"]
	defaultModel := env["OPENCLAW_DEFAULT_MODEL"]
	if defaultModel == "" {
		defaultModel = s.hubCfg.DefaultModel
	}
	if defaultModel == "" {
		defaultModel = "anthropic/claude-sonnet-4-6"
	}
	if anthropicKey == "" {
		for k, v := range s.hubCfg.LLMKeys {
			if k == "anthropic" {
				anthropicKey = v
				break
			}
		}
	}
	if anthropicKey != "" {
		configPatch := fmt.Sprintf(`
export HOME=/home/daytona; python3 - <<'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(path) as f: cfg = json.load(f)
cfg.setdefault('agents', {}).setdefault('defaults', {})['model'] = %q
cfg['models'] = {
  'providers': {
    'anthropic': {
      'enabled': True,
      'api': 'anthropic-messages',
      'apiKey': %q,
      'models': [{'id': 'claude-sonnet-4-6', 'name': 'Claude Sonnet 4.6', 'api': 'anthropic-messages'}]
    }
  }
}
with open(path, 'w') as f: json.dump(cfg, f, indent=2)
print('model config updated')
PYEOF`, defaultModel, anthropicKey)
		if err := exec("configure openclaw model", 30*time.Second, configPatch); err != nil {
			log.Printf("[daytona] warning: failed to configure model: %v", err)
		}
	}

	// Step 2c: Configure gateway bind/port and start it.
	// Use token auth (what onboard sets up) — don't override auth mode.
	gatewaySetup := `
python3 - <<'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(path) as f: cfg = json.load(f)
cfg.setdefault('gateway', {})['bind'] = 'loopback'
cfg['gateway']['port'] = 18789
# Keep token auth that onboard generated - don't change auth mode
with open(path, 'w') as f: json.dump(cfg, f, indent=2)
print('gateway config updated')
PYEOF
export NVM_DIR="/usr/local/share/nvm"; [ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"
setsid nohup openclaw gateway run >> ~/.openclaw/gateway.log 2>&1 </dev/null &
sleep 8
curl -sf http://localhost:18789/healthz && echo 'gateway ready' || echo 'gateway not ready yet'`
	if err := exec("start openclaw gateway", 2*time.Minute, gatewaySetup); err != nil {
		return err
	}

	// Step 3: Download and start claw-bridge
	// Download synchronously first, then background the process.
	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		return fmt.Errorf("claw-bridge URL not configured: set bridge_image in hub.yaml (e.g. bridge_image: ttl.sh/your/claw-bridge:tag) or build a tagged release")
	}
	var downloadCmd string
	if strings.HasPrefix(bridgeURL, "http://") || strings.HasPrefix(bridgeURL, "https://") {
		downloadCmd = fmt.Sprintf(`curl -fsSL %q -o /tmp/claw-bridge && chmod +x /tmp/claw-bridge && echo downloaded`, bridgeURL)
	} else {
		// OCI ref (ttl.sh or ghcr) — use oras
		downloadCmd = fmt.Sprintf(`
if ! command -v oras &>/dev/null; then
  curl -sL https://github.com/oras-project/oras/releases/download/v1.2.2/oras_1.2.2_linux_amd64.tar.gz | tar xz -C /tmp && sudo mv /tmp/oras /usr/local/bin/oras
fi
mkdir -p /tmp/bridge-dl && cd /tmp/bridge-dl && oras pull %q
BIN=$(find /tmp/bridge-dl -name 'claw-bridge*' -type f | head -1)
cp "$BIN" /tmp/claw-bridge && chmod +x /tmp/claw-bridge && echo downloaded`, bridgeURL)
	}
	if err := exec("download claw-bridge", 3*time.Minute, downloadCmd); err != nil {
		return err
	}

	// Start the bridge — it reads the gateway token from openclaw.json automatically.
	// Use setsid to detach from exec session so it survives after exec returns.
	startCmd := fmt.Sprintf(
		`export HOME=/home/daytona; \
ELASTICCLAW_HUB_URL=%q ELASTICCLAW_CLAW_ID=%q ELASTICCLAW_CLAW_TOKEN=%q \
setsid nohup /tmp/claw-bridge >> /tmp/claw-bridge.log 2>&1 </dev/null &
echo started`,
		s.clawHubURL(), clawID, s.hubCfg.ClawToken)
	if err := exec("start claw-bridge", 30*time.Second, startCmd); err != nil {
		return err
	}

	log.Printf("[daytona] bootstrap complete for claw %s", clawID)
	return nil
}

func (s *Server) provisionVercel(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	p, err := vercelProvider.New(vercelProvider.Config{
		AccessToken: cfg.AccessToken,
		TeamID:      cfg.TeamID,
		ProjectID:   cfg.ProjectID,
	})
	if err != nil {
		return fmt.Errorf("vercel init: %w", err)
	}

	// Merge hub env (API keys etc.) into sandbox env
	sandboxEnv := make(map[string]string)
	for k, v := range env {
		sandboxEnv[k] = v
	}

	sandboxID, err := p.CreateSandbox(ctx, req.Name, sandboxEnv)
	if err != nil {
		return fmt.Errorf("vercel create: %w", err)
	}
	log.Printf("vercel sandbox created: %s (claw %s)", sandboxID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='vercel', provider_id=? WHERE id=?`, sandboxID, clawID)

	// Bootstrap asynchronously
	go func() {
		if err := s.bootstrapVercel(context.Background(), clawID, sandboxID, p, files); err != nil {
			log.Printf("vercel bootstrap failed for claw %s: %v", clawID, err)
			_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
			s.broadcastToUsers("", types.WSMessage{
				Type:    "claw_error",
				Payload: map[string]string{"claw_id": clawID, "error": err.Error()},
			})
		}
	}()

	return nil
}

func (s *Server) bootstrapVercel(ctx context.Context, clawID, sandboxID string, p *vercelProvider.Provider, files map[string][]byte) error {
	log.Printf("[vercel] bootstrapping claw %s (sandbox %s)", clawID, sandboxID)

	// Write template files into the sandbox workspace
	workdir := "/vercel/sandbox/workspace"
	if _, _, err := p.Exec(ctx, sandboxID, "mkdir -p "+workdir); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}
	for path, content := range files {
		fullPath := workdir + "/" + path
		if err := p.WriteFile(ctx, sandboxID, fullPath, content); err != nil {
			log.Printf("[vercel] warning: failed to write %s: %v", path, err)
		}
	}

	// Install OpenClaw
	installScript := `
set -e
npm install -g openclaw@latest --ignore-scripts 2>&1 | tail -5
openclaw onboard --non-interactive --accept-risk --skip-daemon 2>&1 || true
openclaw gateway run --port 18789 --auth password --password "$(cat ~/.openclaw/openclaw.json | python3 -c 'import sys,json; print(json.load(sys.stdin)["gateway"]["auth"]["token"])' 2>/dev/null || echo changeme)" &
sleep 8
echo "OpenClaw ready"
`
	out, code, err := p.Exec(ctx, sandboxID, "bash -c '"+strings.ReplaceAll(installScript, "'", "'\"'\"'")+"'")
	if err != nil || code != 0 {
		return fmt.Errorf("openclaw install failed (exit %d): %s", code, out)
	}
	log.Printf("[vercel] OpenClaw installed: %s", sandboxID)

	// Install and start claw-bridge
	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		return fmt.Errorf("claw-bridge URL not configured: set bridge_image in hub.yaml or build a tagged release")
	}
	bridgeScript := fmt.Sprintf(`
curl -fsSL "%s" -o /tmp/claw-bridge && chmod +x /tmp/claw-bridge
ELASTICCLAW_HUB_URL=%q ELASTICCLAW_CLAW_ID=%q ELASTICCLAW_CLAW_TOKEN=%q nohup /tmp/claw-bridge >> /tmp/claw-bridge.log 2>&1 &
echo "claw-bridge started"
`, bridgeURL, s.clawHubURL(), clawID, s.hubCfg.ClawToken)
	out, code, err = p.Exec(ctx, sandboxID, "bash -c '"+strings.ReplaceAll(bridgeScript, "'", "'\"'\"'")+"'")
	if err != nil || code != 0 {
		return fmt.Errorf("claw-bridge install failed (exit %d): %s", code, out)
	}
	log.Printf("[vercel] claw-bridge started: %s", sandboxID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting' WHERE id=?`, clawID)
	return nil
}

func (s *Server) provisionLocal(ctx context.Context, clawID string, req types.CreateClawRequest, files map[string][]byte, env map[string]string) error {
	p := newLocalProvider()
	createReq := types.CreateRequest{
		Name:          req.Name,
		TemplateFiles: files,
		Env:           env,
	}
	instance, err := p.Create(ctx, createReq)
	if err != nil {
		return fmt.Errorf("local create: %w", err)
	}
	log.Printf("local instance created: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting' WHERE id=?`, clawID)
	return nil
}

func (s *Server) provisionReplicated(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, env map[string]string) error {
	// Hub's generated key is always included; append any extra debug keys from hub config.
	cfg.SSHPublicKey = s.identity.PublicKey
	cfg.ExtraSSHPublicKeys = s.hubCfg.SSHPublicKeys
	p, err := newReplicatedProvider(cfg)
	if err != nil {
		return fmt.Errorf("replicated init: %w", err)
	}

	vmID, err := p.ProvisionClaw(ctx, replicatedpkg.VMCreateRequest{
		Name:         req.Name,
		InstanceType: req.InstanceType,
		TTL:          req.TTL,
	}, nil, env)
	if err != nil {
		return fmt.Errorf("replicated provision: %w", err)
	}
	// Store vm_id in the claw record for later operations (destroy, SSH, etc.)
	_, _ = s.db.Exec(
		`UPDATE claws SET status='starting', provider='replicated', provider_id=? WHERE id=?`, vmID, clawID,
	)

	instanceType := req.InstanceType
	if instanceType == "" {
		instanceType = cfg.DefaultInstanceType
		if instanceType == "" {
			instanceType = replicatedpkg.DefaultInstanceType
		}
	}
	ttl := req.TTL
	if ttl == "" {
		ttl = cfg.DefaultTTL
		if ttl == "" {
			ttl = replicatedpkg.DefaultTTL
		}
	}

	log.Printf("Replicated VM provisioned")
	log.Printf("  Claw:          %s (%s)", req.Name, clawID)
	log.Printf("  VM ID:         %s", vmID)
	log.Printf("  Instance type: %s", instanceType)
	log.Printf("  TTL:           %s", ttl)
	log.Printf("  SSH:           ssh %s", replicatedpkg.VMHostname(vmID))
	log.Printf("  Status:        starting (waiting for claw to register)")
	return nil
}

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

func (s *Server) syncReplicatedVMs() {
	replicatedCfg, ok := s.hubCfg.Providers["replicated"]
	if !ok || replicatedCfg.Token == "" {
		return
	}

	// Find claws provisioned on Replicated that aren't in a terminal state
	rows, err := s.db.Query(`
		SELECT id, tenant_id, name, provider_id, status
		FROM claws
		WHERE provider = 'replicated'
		  AND provider_id != ''
		  AND status NOT IN ('failed', 'error', 'offline')
	`)
	if err != nil {
		log.Printf("pollProviderStatus: query error: %v", err)
		return
	}
	defer rows.Close()

	type clawRow struct {
		id, tenantID, name, providerID, status string
	}
	var pending []clawRow
	for rows.Next() {
		var c clawRow
		if err := rows.Scan(&c.id, &c.tenantID, &c.name, &c.providerID, &c.status); err != nil {
			continue
		}
		pending = append(pending, c)
	}
	rows.Close()

	if len(pending) == 0 {
		return
	}

	p, err := newReplicatedProvider(replicatedCfg)
	if err != nil {
		log.Printf("pollProviderStatus: provider init error: %v", err)
		return
	}

	for _, c := range pending {
		vm, err := p.GetVM(context.Background(), c.providerID)
		if err != nil {
			log.Printf("pollProviderStatus: get VM %s error: %v", c.providerID, err)
			continue
		}
		// Only log if status changed or there's a problem
		if vm.Status != c.status && vm.Status != "running" {
			log.Printf("Claw %s (%s): VM %s %s → %s", c.name, c.id[:8], c.providerID, c.status, vm.Status)
		}

		// Map Replicated VM status to claw status
		var newStatus string
		switch vm.Status {
		case "running":
			newStatus = "starting"
			// First time we see running — trigger bootstrap
			if c.status == "provisioning" {
				log.Printf("Claw %s (%s): VM running, bootstrapping...", c.name, c.id[:8])
				go s.bootstrapReplicated(c.id, c.name, c.providerID, replicatedCfg)
			}
		case "terminated", "error":
			newStatus = "offline"
			log.Printf("Replicated VM %s for claw %s (%s) terminated", c.providerID, c.name, c.id)
			// Disconnect claw WebSocket if still connected
			s.mu.Lock()
			if cc, ok := s.claws[c.id]; ok {
				cc.conn.Close(websocket.StatusGoingAway, "VM terminated")
				delete(s.claws, c.id)
			}
			s.mu.Unlock()
		default:
			// assigned, pending, etc — still coming up
			newStatus = "provisioning"
		}

		if newStatus != c.status {
			_, _ = s.db.Exec(`UPDATE claws SET status=? WHERE id=?`, newStatus, c.id)
			log.Printf("Claw %s (%s): status %s → %s (VM %s: %s)",
				c.name, c.id[:8], c.status, newStatus, c.providerID, vm.Status)
			s.broadcastToUsers(c.tenantID, types.WSMessage{
				Type:    "claw_status",
				Payload: map[string]string{"claw_id": c.id, "status": newStatus},
			})
		}
	}
}

// ─── Bootstrap ────────────────────────────────────────────────────────────────

const githubReleasesBase = "https://github.com/elasticclaw/elasticclaw/releases/download"

// Version is set by cmd at startup so the hub can construct versioned download URLs.
var Version = "dev"

// bridgeDownloadURL returns the URL to download the claw-bridge binary.
// Uses hub.yaml bridge_image if set, otherwise constructs the GitHub releases URL
// from the hub's own version. Returns empty string if version is 'dev' and no
// bridge_image is configured — caller must check and fail appropriately.
func (s *Server) bridgeDownloadURL() string {
	if s.hubCfg.BridgeImage != "" {
		return s.hubCfg.BridgeImage
	}
	if Version == "dev" || Version == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/claw-bridge-linux-amd64", githubReleasesBase, Version)
}

// bootstrapReplicated SSHes into a newly-running Replicated VM, pulls the
// claw-bridge binary from GitHub Releases, and starts it with hub connection env vars.
func (s *Server) bootstrapReplicated(clawID, clawName, vmID string, cfg types.ProviderConfig) {
	var filesJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&filesJSON)
	var files map[string]string
	_ = json.Unmarshal([]byte(filesJSON), &files)

	// Load github repos config for this claw
	var githubReposJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(github_repos,'[]') FROM claws WHERE id=?`, clawID).Scan(&githubReposJSON)
	var githubRepos []types.GitHubRepoAccess
	_ = json.Unmarshal([]byte(githubReposJSON), &githubRepos)

	// Resolve Linear token for this claw
	var linearWorkspace string
	_ = s.db.QueryRow(`SELECT COALESCE(linear_workspace,'') FROM claws WHERE id=?`, clawID).Scan(&linearWorkspace)
	linearToken := resolveLinearToken(s.hubCfg, linearWorkspace)
	// Resolve model: template override wins over hub default
	var templateDefaultModel string
	_ = s.db.QueryRow(`SELECT COALESCE(default_model,'') FROM claws WHERE id=?`, clawID).Scan(&templateDefaultModel)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = s.hubCfg.DefaultModel
	}
	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		log.Printf("[bootstrap] ERROR: bridge_image not set and hub version is 'dev' — set bridge_image in hub.yaml")
		_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
		return
	}

	// Get the direct SSH endpoint from Replicated (IP:port, user is always root)
	cp, err := newReplicatedProvider(cfg)
	if err != nil {
		log.Printf("bootstrap: provider init error: %v", err)
		return
	}
	vm, err := cp.GetVM(context.Background(), vmID)
	if err != nil || vm.DirectSSHEndpoint == "" || vm.DirectSSHPort == 0 {
		log.Printf("bootstrap: could not get direct SSH endpoint for VM %s: %v", vmID, err)
		return
	}
	// Replicated uses the comment from the SSH public key as the Linux username.
	// Our key comment is "elasticclaw@hub", so the username is "elasticclaw".
	sshUser := replicatedpkg.SSHUserFromPublicKey(s.identity.PublicKey)
	sshHost := fmt.Sprintf("%s:%d", vm.DirectSSHEndpoint, vm.DirectSSHPort)
	log.Printf("Bootstrap SSH: %s@%s", sshUser, sshHost)
	// Store SSH connection details in the DB for terminal access
	_, _ = s.db.Exec(
		`UPDATE claws SET ssh_host=?, ssh_port=?, ssh_user=? WHERE id=?`,
		vm.DirectSSHEndpoint, vm.DirectSSHPort, sshUser, clawID,
	)

	// Generate a random gateway password for this VM so claw-bridge can connect with full scopes
	gatewayPassword := randomHex(16)

	// Build the bootstrap script
	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail

# ── LLM API keys + service tokens (injected first so all steps can use them) ───
export OPENCLAW_DEFAULT_MODEL="%s"
export ELASTICCLAW_GATEWAY_PASSWORD="%s"
%s
%s
# ── Install Node.js 24 via nodesource ─────────────────────────────────────────────
echo "Installing Node.js 24..."
sudo apt-get update -qq
sudo apt-get install -y curl ca-certificates
sudo mkdir -p /etc/apt/keyrings
# Use gpg batch mode (no /dev/tty needed)
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | \
  sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/nodesource.gpg
echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main" | sudo tee /etc/apt/sources.list.d/nodesource.list > /dev/null
sudo apt-get update -qq
sudo apt-get install -y nodejs git
echo "Node: $(node --version)"

# ── Install OpenClaw (sudo so it lands in /usr/bin/openclaw) ──────────────────
echo "Installing OpenClaw..."
sudo npm install -g openclaw@latest --ignore-scripts
echo "OpenClaw: $(openclaw --version)"

# ── Configure OpenClaw ──────────────────────────────────────────────────────────────
mkdir -p "$HOME/.openclaw/workspace"
if [ ! -f "$HOME/.openclaw/openclaw.json" ]; then
  echo "Configuring OpenClaw..."
  # Use onboard --non-interactive to generate valid config (no TTY needed with these flags)
  ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-placeholder}" \
  openclaw onboard \
    --non-interactive --accept-risk \
    --auth-choice anthropic-api-key \
    --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}" \
    --gateway-bind loopback --gateway-port 18789 \
    --skip-daemon 2>/dev/null || true
  # Patch config to add required models.providers fields
  python3 << 'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
try:
    with open(path) as f:
        config = json.load(f)
except:
    config = {}
model = os.environ.get('OPENCLAW_DEFAULT_MODEL', 'anthropic/claude-sonnet-4-6')
apiKey = os.environ.get('ANTHROPIC_API_KEY', 'placeholder')
config.setdefault('agents', {}).setdefault('defaults', {})['model'] = model
config['models'] = {
    'providers': {
        'anthropic': {
            'apiKey': apiKey,
            'baseUrl': 'https://api.anthropic.com',
            'api': 'anthropic-messages',
            'models': [
                {'id': 'claude-sonnet-4-6', 'name': 'Claude Sonnet 4.6', 'api': 'anthropic-messages'},
                {'id': 'claude-opus-4-5', 'name': 'Claude Opus 4.5', 'api': 'anthropic-messages'},
                {'id': 'claude-sonnet-4-5', 'name': 'Claude Sonnet 4.5', 'api': 'anthropic-messages'}
            ]
        }
    }
}
config.setdefault('gateway', {})['bind'] = 'loopback'
config['gateway']['port'] = 18789
# Use password auth so claw-bridge can connect with full scopes
# (token auth grants limited scopes by default)
gw_password = os.environ.get('ELASTICCLAW_GATEWAY_PASSWORD', '')
if gw_password:
    config['gateway']['auth'] = {'mode': 'password', 'password': gw_password}
with open(path, 'w') as f:
    json.dump(config, f, indent=2)
print('OpenClaw config patched')
PYEOF
fi

# ── Start OpenClaw gateway ──────────────────────────────────────────────────────────────
echo "Starting OpenClaw gateway..."
export OPENCLAW_NO_RESPAWN=1
nohup openclaw gateway run >> "$HOME/openclaw-gateway.log" 2>&1 &
# Wait up to 30s for gateway to open :18789
for i in $(seq 1 30); do
  sleep 1
  if curl -sf http://localhost:18789/healthz &>/dev/null; then
    echo "OpenClaw gateway ready after ${i}s"
    break
  fi
  if [ "$i" = "30" ]; then
    echo "WARNING: gateway did not respond in 30s"
    tail -10 "$HOME/openclaw-gateway.log" 2>/dev/null || true
  fi
done

# ── Install claw-bridge ─────────────────────────────────────────────────────
BRIDGE_SRC="%s"
echo "Installing claw-bridge from $BRIDGE_SRC..."
if echo "$BRIDGE_SRC" | grep -qE '^https?://'; then
  # Plain HTTP(S) URL — use curl (default: GitHub Releases)
  curl -fsSL "$BRIDGE_SRC" -o /tmp/claw-bridge
else
  # OCI ref — use oras (dev override via bridge_image in hub.yaml)
  if ! command -v oras &>/dev/null; then
    echo "Installing oras..."
    curl -sL https://github.com/oras-project/oras/releases/download/v1.2.2/oras_1.2.2_linux_amd64.tar.gz | tar xz -C /tmp
    sudo mv /tmp/oras /usr/local/bin/oras
  fi
  mkdir -p /tmp/claw-bridge-dl && cd /tmp/claw-bridge-dl
  oras pull "$BRIDGE_SRC"
  BINARY=$(find /tmp/claw-bridge-dl -name 'claw-bridge*' -type f | head -1)
  if [ -z "$BINARY" ]; then
    echo "ERROR: claw-bridge binary not found after oras pull"
    ls -la /tmp/claw-bridge-dl/
    exit 1
  fi
  cp "$BINARY" /tmp/claw-bridge
  cd -
fi
chmod +x /tmp/claw-bridge
sudo mv /tmp/claw-bridge /usr/local/bin/claw-bridge
echo "claw-bridge installed"

# ── GitHub credential helper ───────────────────────────────────────────────
# Installs a git credential helper that fetches a fresh GitHub installation
# token from the hub on demand. Token never expires on disk.
%s
# Export env vars then start claw-bridge
export ELASTICCLAW_HUB_URL="%s"
export ELASTICCLAW_CLAW_ID="%s"
export ELASTICCLAW_CLAW_TOKEN="%s"
export ELASTICCLAW_CLAW_NAME="%s"
export ELASTICCLAW_GATEWAY_PASSWORD="%s"
%s
export OPENCLAW_DEFAULT_MODEL="%s"
%s
echo "Starting claw-bridge (HUB_URL=$ELASTICCLAW_HUB_URL)..."
nohup /usr/local/bin/claw-bridge >> "$HOME/claw-bridge.log" 2>&1 &

BRIDGE_PID=$!
echo "claw-bridge started (PID $BRIDGE_PID)"
sleep 2
if kill -0 $BRIDGE_PID 2>/dev/null; then
  echo "claw-bridge is running"
  tail -5 "$HOME/claw-bridge.log" 2>/dev/null || echo "(no log yet)"
else
  echo "ERROR: claw-bridge died immediately"
  cat "$HOME/claw-bridge.log" 2>/dev/null
  exit 1
fi
`,
		defaultModel, gatewayPassword, buildLLMKeyEnv(s.hubCfg.LLMKeys), buildLinearEnv(linearToken), // top-of-script exports
		bridgeURL,
		buildGitHubCredentialHelper(s.hubCfg, s.clawHubURL(), clawID, githubRepos),
		s.clawHubURL(), clawID, s.hubCfg.ClawToken, clawName, gatewayPassword,
		buildRelayEnv(s.hubCfg, s.identity.PublicKey),
		defaultModel,
		buildLLMKeyEnv(s.hubCfg.LLMKeys),
	)

	// Inject GitHub tools context into TOOLS.md if GitHub is configured
	if len(s.hubCfg.GitHubApps) > 0 && len(githubRepos) > 0 {
		repoLines := ""
		for _, r := range githubRepos {
			repoLines += fmt.Sprintf("- `%s` (%s)\n", r.Repo, r.Permissions)
		}
		githubSection := fmt.Sprintf(`
## GitHub Access

This claw has authenticated access to the following repositories via a GitHub App installation token. The token is fetched automatically — you don't need to configure anything.

%s
**git** and **gh CLI** are pre-configured and will work without any additional auth setup:

`+"```bash\n"+`# These just work:
git clone https://github.com/owner/repo
gh pr create
gh issue list
`+"```\n"+`
Tokens are short-lived and refreshed automatically on each git/gh operation.
`, repoLines)
		if existing, ok := files["TOOLS.md"]; ok {
			files["TOOLS.md"] = existing + "\n" + githubSection
		} else {
			files["TOOLS.md"] = githubSection
		}
	}

	// Write template files to workspace via separate SSH sessions
	if len(files) > 0 {
		for attempt := 1; attempt <= 5; attempt++ {
			if err := s.sshWriteFiles(sshUser, sshHost, "$HOME/.openclaw/workspace", files); err == nil {
				log.Printf("Template files written for claw %s", clawName)
				break
			} else if attempt == 5 {
				log.Printf("Warning: failed to write template files: %v", err)
			} else {
				time.Sleep(10 * time.Second)
			}
		}
	}

	// Retry SSH up to 5 times with 10s delay — VM may report 'running' before SSH is ready
	var sshErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			log.Printf("Bootstrap retry %d/5 for claw %s in 10s...", attempt, clawName)
			time.Sleep(10 * time.Second)
		}
		if sshErr = s.sshRun(sshUser, sshHost, script); sshErr == nil {
			break
		}
		log.Printf("Bootstrap attempt %d/5 failed: %v", attempt, sshErr)
	}
	if sshErr != nil {
		log.Printf("Bootstrap failed for claw %s after 5 attempts: %v", clawID, sshErr)
		_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
		return
	}
	log.Printf("Bootstrap complete for claw %s (%s)", clawName, clawID[:8])
}


// randomHex returns a random hex string of n bytes (2*n hex chars).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// clawHubURL returns the URL claws should use to connect back.
// Uses public_url if set, otherwise falls back to url.
func (s *Server) clawHubURL() string {
	if s.hubCfg.PublicURL != "" {
		return s.hubCfg.PublicURL
	}
	return s.hubCfg.URL
}

// buildRelayEnv returns shell lines that export relay env vars for the bridge.
// When relay is not configured, returns an empty comment.
func buildRelayEnv(cfg *types.HubConfig, publicKey string) string {
	if cfg.RelayURL == "" {
		return "# Relay not configured — bridge uses direct hub connection"
	}
	hubID := HubID(publicKey)
	relayToken := RelayToken(cfg.RelaySecret, hubID, cfg.ClawToken)
	return fmt.Sprintf("export ELASTICCLAW_RELAY_URL=%q\nexport ELASTICCLAW_HUB_ID=%q\nexport ELASTICCLAW_RELAY_TOKEN=%q",
		cfg.RelayURL, hubID, relayToken)
}

// resolveLinearToken finds the Linear API token for the given workspace label.
// If workspace is empty or not found, returns the first token if only one is configured.
func resolveLinearToken(cfg *types.HubConfig, workspace string) string {
	if len(cfg.Linear) == 0 {
		return ""
	}
	for _, l := range cfg.Linear {
		if workspace != "" && l.Workspace == workspace {
			return l.Token
		}
	}
	// Default: first entry (when workspace is empty or no match)
	return cfg.Linear[0].Token
}

// buildLinearEnv returns a shell export line for LINEAR_API_KEY if a token is set.
func buildLinearEnv(token string) string {
	if token == "" {
		return "# Linear not configured"
	}
	return fmt.Sprintf("export LINEAR_API_KEY=%q", token)
}

// buildLLMKeyEnv converts the llm_keys map to shell env var lines for the bootstrap script.
// e.g. {"anthropic": "sk-ant-..."} -> "  ANTHROPIC_API_KEY=\"sk-ant-...\" \\\n"
func buildLLMKeyEnv(keys map[string]string) string {
	if len(keys) == 0 {
		return ""
	}
	var b strings.Builder
	for provider, key := range keys {
		envVar := strings.ToUpper(provider) + "_API_KEY"
		fmt.Fprintf(&b, "export %s=%q\n", envVar, key)
	}
	return b.String()
}
// buildGitHubCloneScript returns shell lines that clone repos into the current directory.
func buildGitHubCloneScript(repos []types.GitHubRepoAccess) string {
	if len(repos) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range repos {
		parts := strings.SplitN(r.Repo, "/", 2)
		repoName := r.Repo
		if len(parts) == 2 {
			repoName = parts[1]
		}
		fmt.Fprintf(&b, "if [ ! -d %q ]; then git clone https://github.com/%s %s && echo 'Cloned %s'; else git -C %s pull --ff-only && echo 'Updated %s'; fi\n",
			repoName, r.Repo, repoName, r.Repo, repoName, r.Repo)
	}
	return b.String()
}

// buildGitHubCredentialHelper returns shell script lines that install a git
// credential helper on the VM if GitHub App is configured on the hub.
func buildGitHubCredentialHelper(cfg *types.HubConfig, hubURL, clawID string, repos []types.GitHubRepoAccess) string {
	if len(cfg.GitHubApps) == 0 {
		return "# GitHub App not configured — skipping credential helper"
	}
	clawToken := cfg.ClawToken
	tokenURL := fmt.Sprintf("%s/api/github/token/%s?claw_token=%s", hubURL, clawID, clawToken)
	return fmt.Sprintf(`# Install GitHub credential helper
sudo tee /usr/local/bin/elasticclaw-git-credentials > /dev/null << 'CREDEOF'
#!/bin/bash
# Git credential helper — fetches a fresh GitHub App installation token from the hub.
response=$(curl -sf %q)
if [ $? -ne 0 ] || [ -z "$response" ]; then
  exit 1
fi
token=$(echo "$response" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "protocol=https"
echo "host=github.com"
echo "username=x-access-token"
echo "password=$token"
CREDEOF
sudo chmod +x /usr/local/bin/elasticclaw-git-credentials

# Install git + gh CLI
if ! command -v git &>/dev/null || ! command -v gh &>/dev/null; then
  echo "Installing git and gh CLI..."
  sudo apt-get update -qq
  sudo apt-get install -y git 2>/dev/null || true
  if ! command -v gh &>/dev/null; then
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg 2>/dev/null
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null
    sudo apt-get update -qq && sudo apt-get install -y gh 2>/dev/null || echo "gh install failed, continuing"
  fi
fi

# Configure git to use the credential helper
if command -v git &>/dev/null; then
  git config --global credential.helper /usr/local/bin/elasticclaw-git-credentials
fi

# Configure gh to use the credential helper via GH_TOKEN env (set in a wrapper)
# We don't pre-auth here because the claw isn't registered with the hub yet.
# Instead, create a gh wrapper that fetches a token on each call.
if command -v gh &>/dev/null; then
  # Write GH_TOKEN to /etc/profile.d so it's available in ALL shells
  # (both interactive and non-interactive, which is what agents use)
  sudo tee /etc/profile.d/elasticclaw-github.sh > /dev/null << 'PROFEOF'
export GH_TOKEN=$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | grep ^password | cut -d= -f2)
PROFEOF
  sudo chmod +x /etc/profile.d/elasticclaw-github.sh

  # Also configure gh auth now that the claw is registered and credential helper works
  GH_TOKEN=$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | grep ^password | cut -d= -f2)
  if [ -n "$GH_TOKEN" ]; then
    echo "$GH_TOKEN" | gh auth login --with-token 2>/dev/null && echo "gh CLI authenticated" || echo "gh auth failed (will retry via profile.d)"
  fi
fi
echo "GitHub credential helper installed"

# Clone repos into workspace so the agent starts with them already present
cd "$HOME/.openclaw/workspace"
%s
echo "Repos cloned"`, tokenURL, buildGitHubCloneScript(repos))
}

// sshRun connects to host via the hub's SSH identity and runs a script.
func (s *Server) sshRun(user, host, script string) error {
	pubKeyType := s.identity.PrivateKey.PublicKey().Type()
	pubKeyFP := gossh.FingerprintSHA256(s.identity.PrivateKey.PublicKey())
	log.Printf("SSH attempting: user=%s host=%s key-type=%s fingerprint=%s", user, host, pubKeyType, pubKeyFP)
	log.Printf("SSH public key being used:\n%s", s.identity.PublicKey)

	sshCfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.identity.PrivateKey)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}

	client, err := gossh.Dial("tcp", host, sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", host, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(script)
	if err != nil {
		return fmt.Errorf("ssh script failed: %w\noutput: %s", err, string(out))
	}
	log.Printf("bootstrap output:\n%s", string(out))
	return nil
}

// sshWriteFiles writes a map of filename->content to a remote directory via SSH.
func (s *Server) sshWriteFiles(user, host, dir string, files map[string]string) error {
	sshCfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.identity.PrivateKey)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	client, err := gossh.Dial("tcp", host, sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	for name, content := range files {
		sess, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("ssh session: %w", err)
		}
		// Use cat with heredoc to write the file safely
		cmd := fmt.Sprintf("mkdir -p %s && cat > %s/%s << 'ELASTICCLAW_EOF'\n%s\nELASTICCLAW_EOF", dir, dir, name, content)
		out, err := sess.CombinedOutput(cmd)
		sess.Close()
		if err != nil {
			return fmt.Errorf("write %s: %w\n%s", name, err, string(out))
		}
	}
	return nil
}

// ─── Terminal WebSocket ───────────────────────────────────────────────────────

// handleTerminal proxies a WebSocket connection to an SSH PTY on the claw's VM.
// Route: GET /api/terminal/{clawID}?token=...
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	// Auth via token query param
	token := r.URL.Query().Get("token")
	if token == "" {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	tenantID, err := s.tenantByToken(token)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	clawID := strings.TrimPrefix(r.URL.Path, "/api/terminal/")
	if clawID == "" {
		http.Error(w, "missing claw id", http.StatusBadRequest)
		return
	}

	// Look up SSH details, verify tenant owns the claw
	var sshHost string
	var sshPort int
	var sshUser string
	err = s.db.QueryRow(
		`SELECT ssh_host, ssh_port, ssh_user FROM claws WHERE id = ? AND tenant_id = ?`,
		clawID, tenantID,
	).Scan(&sshHost, &sshPort, &sshUser)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if sshHost == "" || sshPort == 0 {
		http.Error(w, "ssh not available for this claw", http.StatusServiceUnavailable)
		return
	}

	// Upgrade to WebSocket
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	ctx := r.Context()

	// Connect to SSH
	sshCfg := &gossh.ClientConfig{
		User:            sshUser,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.identity.PrivateKey)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	sshAddr := fmt.Sprintf("%s:%d", sshHost, sshPort)
	sshClient, err := gossh.Dial("tcp", sshAddr, sshCfg)
	if err != nil {
		log.Printf("terminal: ssh dial %s: %v", sshAddr, err)
		_ = conn.Close(websocket.StatusInternalError, "ssh connection failed")
		return
	}
	defer sshClient.Close()

	sshSess, err := sshClient.NewSession()
	if err != nil {
		log.Printf("terminal: ssh session: %v", err)
		_ = conn.Close(websocket.StatusInternalError, "ssh session failed")
		return
	}
	defer sshSess.Close()

	// Request PTY
	if err := sshSess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{
		gossh.ECHO:          1,
		gossh.TTY_OP_ISPEED: 14400,
		gossh.TTY_OP_OSPEED: 14400,
	}); err != nil {
		log.Printf("terminal: request pty: %v", err)
		_ = conn.Close(websocket.StatusInternalError, "pty failed")
		return
	}

	// Start shell
	sshStdin, err := sshSess.StdinPipe()
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "stdin pipe failed")
		return
	}
	sshStdout, err := sshSess.StdoutPipe()
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "stdout pipe failed")
		return
	}
	sshSess.Stderr = sshSess.Stdout // merge stderr

	if err := sshSess.Shell(); err != nil {
		log.Printf("terminal: shell: %v", err)
		_ = conn.Close(websocket.StatusInternalError, "shell failed")
		return
	}

	// SSH stdout → WebSocket (in goroutine)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := sshStdout.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageText, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// WebSocket → SSH stdin (resize handling)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		// Check for resize message
		var resizeMsg struct {
			Type string `json:"type"`
			Cols  uint32 `json:"cols"`
			Rows  uint32 `json:"rows"`
		}
		if len(data) > 0 && data[0] == '{' {
			if json.Unmarshal(data, &resizeMsg) == nil && resizeMsg.Type == "resize" {
				_ = sshSess.WindowChange(int(resizeMsg.Rows), int(resizeMsg.Cols))
				continue
			}
		}
		if _, err := io.WriteString(sshStdin, string(data)); err != nil {
			return
		}
	}
}

// terminateReplicatedVM terminates a Replicated CMX VM by ID.
func (s *Server) terminateReplicatedVM(vmID string) {
	cfg, ok := s.hubCfg.Providers["replicated"]
	if !ok {
		log.Printf("terminateReplicatedVM: no replicated provider configured")
		return
	}
	p, err := newReplicatedProvider(cfg)
	if err != nil {
		log.Printf("terminateReplicatedVM: provider init error: %v", err)
		return
	}
	if err := p.DeleteVM(context.Background(), vmID); err != nil {
		log.Printf("terminateReplicatedVM: failed to delete VM %s: %v", vmID, err)
		return
	}
	log.Printf("Replicated VM %s terminated", vmID)
}

// ─── GitHub Token Endpoint ────────────────────────────────────────────────────

// handleGitHubToken mints a fresh GitHub installation token for the claw.
// Auth: ?claw_token= query param (the claw's hub token, same as registration).
// URL: GET /api/github/token/:clawId
// Used by the git credential helper on the VM.
func (s *Server) handleGitHubToken(w http.ResponseWriter, r *http.Request) {
	if len(s.hubCfg.GitHubApps) == 0 {
		http.Error(w, "no github apps configured", http.StatusNotImplemented)
		return
	}

	clawToken := r.URL.Query().Get("claw_token")
	if clawToken == "" {
		clawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	tenantID, err := s.tenantByClawToken(clawToken)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	clawID := strings.TrimPrefix(r.URL.Path, "/api/github/token/")
	if clawID == "" {
		http.Error(w, "missing claw id", http.StatusBadRequest)
		return
	}

	var reposJSON string
	err = s.db.QueryRow(
		`SELECT github_repos FROM claws WHERE id = ? AND tenant_id = ?`,
		clawID, tenantID,
	).Scan(&reposJSON)
	if err != nil {
		http.Error(w, "claw not found", http.StatusNotFound)
		return
	}

	var repos []RepoAccess
	if reposJSON != "" && reposJSON != "[]" {
		// Support both old (capitalized) and new (lowercase) JSON key formats.
		// Old format: [{"Repo":"owner/repo","Permissions":"write"}]
		// New format: [{"repo":"owner/repo","permissions":"write"}]
		var rawRepos []struct {
			Repo        string `json:"repo"`        // new format
			RepoOld     string `json:"Repo"`        // old format (no json tags)
			Permissions string `json:"permissions"` // new format
			PermsOld    string `json:"Permissions"` // old format
		}
		if err := json.Unmarshal([]byte(reposJSON), &rawRepos); err == nil {
			for _, r := range rawRepos {
				repo := r.Repo
				if repo == "" {
					repo = r.RepoOld // fall back to old capitalized key
				}
				perm := r.Permissions
				if perm == "" {
					perm = r.PermsOld
				}
				if perm == "" {
					perm = "read"
				}
				if repo != "" {
					repos = append(repos, RepoAccess{Repo: repo, Permissions: perm})
				}
			}
		}
	}

	// Try each configured GitHub App in order; use the first that finds an installation
	for i, appCfg := range s.hubCfg.GitHubApps {
		provider, err := NewGitHubTokenProvider(appCfg)
		if err != nil {
			log.Printf("github app[%d] (app_id=%d url=%s) config error: %v", i, appCfg.AppID, appCfg.URL, err)
			continue
		}
		token, expiresAt, err := provider.InstallationToken(r.Context(), 0, repos)
		if err != nil {
			log.Printf("github app[%d] (app_id=%d url=%s): no installation for repos %v: %v", i, appCfg.AppID, appCfg.URL, repos, err)
			continue
		}
		log.Printf("github token issued via app_id=%d (url=%s) for claw %s", appCfg.AppID, appCfg.URL, clawID[:8])
		jsonOK(w, map[string]interface{}{
			"token":      token,
			"expires_at": expiresAt,
		})
		return
	}

	log.Printf("no github app found with installation for repos %v (claw %s)", repos, clawID[:8])
	http.Error(w, "no github installation found for the requested repos", http.StatusNotFound)
}
