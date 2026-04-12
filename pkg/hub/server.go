package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	replicatedpkg "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
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
	id       string
	tenantID string
	conn     *websocket.Conn
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
	mux.HandleFunc("/api/messages/", s.withAuth(s.handleMessages))

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("ElasticClaw Hub listening on %s", s.addr)
	return http.ListenAndServe(s.addr, mux)
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
		`SELECT id, name, template, status, last_seen, created_at FROM claws WHERE tenant_id = ? ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var out []types.Claw
	for rows.Next() {
		var c types.Claw
		var lastSeen sql.NullTime
		if err := rows.Scan(&c.ID, &c.Name, &c.Template, &c.Status, &lastSeen, &c.CreatedAt); err != nil {
			continue
		}
		c.TenantID = tenantID
		if lastSeen.Valid {
			c.LastSeen = lastSeen.Time
		}
		s.mu.RLock()
		if _, online := s.claws[c.ID]; online {
			c.Status = "connected"
		}
		s.mu.RUnlock()
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
	_, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, status, created_at) VALUES(?,?,?,?,?,?,?,'provisioning',?)`,
		clawID, tenantID, req.Name, req.TemplateName, req.Provider, req.DefaultModel, string(filesJSON), now(),
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
		`SELECT id, name, template, status, last_seen, created_at FROM claws WHERE id = ? AND tenant_id = ?`,
		clawID, tenantID,
	).Scan(&c.ID, &c.Name, &c.Template, &c.Status, &lastSeen, &c.CreatedAt)
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
		`INSERT INTO claws(id,tenant_id,name,template,status,last_seen,created_at) VALUES(?,?,?,?,'connected',?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, template=excluded.template, status='connected', last_seen=excluded.last_seen`,
		clawID, tenantID, rp.Name, rp.Template, now(), now(),
	)

	cc := &clawConn{id: clawID, tenantID: tenantID, conn: conn}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()

	log.Printf("claw connected: %s (%s)", rp.Name, clawID)

	// Ack
	_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "registered", Payload: map[string]string{"claw_id": clawID}})

	// Broadcast claw online to user sessions
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": "connected"}})

	// Read loop — claw sends messages back to users
	defer func() {
		s.mu.Lock()
		delete(s.claws, clawID)
		s.mu.Unlock()
		_, _ = s.db.Exec(`UPDATE claws SET status='offline', last_seen=? WHERE id=?`, now(), clawID)
		s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": "offline"}})
		log.Printf("claw disconnected: %s", clawID)
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
			if msg.Type == "message" {
				// Claw is sending a message — store and forward to users
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
	createReq := types.CreateRequest{
		Name:          req.Name,
		FromImage:     req.Image,
		TemplateFiles: files,
		Env:           env,
	}
	instance, err := p.Create(ctx, createReq)
	if err != nil {
		return fmt.Errorf("daytona create: %w", err)
	}
	log.Printf("daytona workspace created: %s (claw %s)", instance.ID, clawID)
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

const defaultBridgeImage = "ghcr.io/elasticclaw/claw-bridge:latest"

// bootstrapReplicated SSHes into a newly-running Replicated VM, pulls the
// claw-bridge binary from OCI, and starts it with hub connection env vars.
func (s *Server) bootstrapReplicated(clawID, clawName, vmID string, cfg types.ProviderConfig) {
	var filesJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&filesJSON)
	var files map[string]string
	_ = json.Unmarshal([]byte(filesJSON), &files)
	// Resolve model: template override wins over hub default
	var templateDefaultModel string
	_ = s.db.QueryRow(`SELECT COALESCE(default_model,'') FROM claws WHERE id=?`, clawID).Scan(&templateDefaultModel)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = s.hubCfg.DefaultModel
	}
	bridgeImage := s.hubCfg.BridgeImage
	if bridgeImage == "" {
		bridgeImage = defaultBridgeImage
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

	// Build the bootstrap script
	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail

# ── LLM API keys (injected first so all steps can use them) ───────────────
export OPENCLAW_DEFAULT_MODEL="%s"
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
sudo apt-get install -y nodejs
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

# ── Install oras if not present ───────────────────────────────────────────────
if ! command -v oras &>/dev/null; then
  echo "Installing oras..."
  curl -sL https://github.com/oras-project/oras/releases/download/v1.2.2/oras_1.2.2_linux_amd64.tar.gz | tar xz -C /tmp
  sudo mv /tmp/oras /usr/local/bin/oras
fi

# Pull claw-bridge binary from OCI
echo "Pulling claw-bridge from %s..."
mkdir -p /tmp/claw-bridge-dl
cd /tmp/claw-bridge-dl
oras pull %s
# oras preserves the annotated path (bin/claw-bridge-linux-amd64), find and install it
BINARY=$(find /tmp/claw-bridge-dl -name 'claw-bridge-linux-amd64' -type f | head -1)
if [ -z "$BINARY" ]; then
  echo "ERROR: claw-bridge binary not found after oras pull"
  ls -la /tmp/claw-bridge-dl/
  exit 1
fi
chmod +x "$BINARY"
sudo mv "$BINARY" /usr/local/bin/claw-bridge

# Export env vars then start claw-bridge
export ELASTICCLAW_HUB_URL="%s"
export ELASTICCLAW_CLAW_ID="%s"
export ELASTICCLAW_CLAW_TOKEN="%s"
export ELASTICCLAW_CLAW_NAME="%s"
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
		defaultModel, buildLLMKeyEnv(s.hubCfg.LLMKeys), // top-of-script exports
		bridgeImage, bridgeImage,
		s.clawHubURL(), clawID, s.hubCfg.ClawToken, clawName,
		defaultModel,
		buildLLMKeyEnv(s.hubCfg.LLMKeys),
	)

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


// clawHubURL returns the URL claws should use to connect back.
// Uses public_url if set, otherwise falls back to url.
func (s *Server) clawHubURL() string {
	if s.hubCfg.PublicURL != "" {
		return s.hubCfg.PublicURL
	}
	return s.hubCfg.URL
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
