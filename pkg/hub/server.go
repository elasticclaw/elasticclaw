package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/internal/webui"

	daytona "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	replicatedpkg "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
	vercelProvider "github.com/elasticclaw/elasticclaw/pkg/provider/vercel"
	"github.com/elasticclaw/elasticclaw/pkg/types"
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
	mux      *http.ServeMux

	mu    sync.RWMutex
	claws map[string]*clawConn // claw_id -> conn
	users map[string]*userConn // tenant_id -> []conn (broadcast)

	fileAckMu       sync.Mutex
	fileAckWaiters  map[string]chan types.FileAck      // request_id -> waiter
	fileReadWaiters map[string]chan types.FileReadResp // request_id -> waiter

	// githubBaseURL overrides the GitHub API base for testing (default: https://api.github.com)
	githubBaseURL string
	// linearBaseURL overrides the Linear API base for testing (default: https://api.linear.app)
	linearBaseURL string
}

type clawConn struct {
	id                   string
	tenantID             string
	conn                 *websocket.Conn
	contextUsage         int             // 0-100, updated from heartbeats
	gatewayReady         bool            // true once bridge reports gateway session established
	streamingBuf         strings.Builder // accumulates chunks for current in-flight response
	streamingMsgID       string          // pre-assigned message ID for the current stream
	streamingStartedAt   time.Time       // when the current streaming turn started (zero if not streaming)
	streamingTimeoutSent bool            // true once the 12-min timeout message has been injected this turn
	contextWarningSent   bool            // true once the context-nearly-full warning has been injected this turn
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
		db:              db,
		addr:            addr,
		hubCfg:          hubCfg,
		identity:        id,
		claws:           make(map[string]*clawConn),
		users:           make(map[string]*userConn),
		fileAckWaiters:  make(map[string]chan types.FileAck),
		fileReadWaiters: make(map[string]chan types.FileReadResp),
	}

	// Start background poller to keep provider VM status fresh
	go srv.pollProviderStatus()
	srv.startPRWatcher()

	return srv, nil
}

// Run starts the HTTP server (blocking).
// RunOptions configures runtime behavior of the hub.
type RunOptions struct {
	NoWebUI bool // skip serving embedded web UI (use in dev when Next.js runs separately)
}

func (s *Server) Run(opts ...RunOptions) error {
	noWebUI := len(opts) > 0 && opts[0].NoWebUI
	mux := http.NewServeMux()
	s.mux = mux

	s.registerRoutes(mux)

	// Serve embedded web UI (static export)
	if noWebUI {
		log.Printf("[hub] web UI disabled (--no-web-ui)")
	} else if webFS, err := webui.FS(); err == nil {
		if _, indexErr := webFS.Open("index.html"); indexErr != nil {
			log.Printf("[hub] web UI not built — run: make build-web")
		} else {
			s.serveWebUI(mux, webFS)
			log.Printf("[hub] serving embedded web UI")
		}
	}

	// Connect to relay if configured
	s.mu.RLock()
	relayURL := s.hubCfg.RelayURL
	relaySecret := s.hubCfg.RelaySecret
	clawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	if relayURL != "" {
		hubID := HubID(s.identity.PublicKey)
		relayToken := RelayToken(relaySecret, hubID, clawToken)
		log.Printf("[relay] hub ID: %s", hubID[:8]+"...")
		log.Printf("[relay] connecting to %s", relayURL)
		go s.connectRelay(context.Background(), relayURL, hubID, relayToken)
	}

	log.Printf("ElasticClaw Hub listening on %s", s.addr)
	if s.hubCfg.UIPassword == "" {
		log.Printf("⚠️  Web UI password not set — using default: 'admin'. Set ui_password in hub.yaml to secure the UI.")
	}
	return http.ListenAndServe(s.addr, corsMiddleware(mux))
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// Claw WebSocket
	mux.HandleFunc("/claw/ws", s.handleClawWS)

	// Browser WebSocket
	mux.HandleFunc("/api/ws", s.handleUserWS)

	// REST API
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/auth/login", s.handleWebLogin)
	mux.HandleFunc("/api/auth/logout", s.handleWebLogout)
	mux.HandleFunc("/api/auth/me", s.withWebAuth(s.handleWebMe))
	mux.HandleFunc("/api/hub-config", s.withWebAuth(s.handleHubConfig))
	mux.HandleFunc("/api/settings", s.withWebAuth(s.handleSettings))
	mux.HandleFunc("/api/settings/status", s.withWebAuth(s.handleSettingsStatus))

	// Template store
	mux.HandleFunc("/api/templates", s.withWebAuth(s.handleTemplates))
	mux.HandleFunc("/api/templates/{name}", s.withWebAuth(s.handleTemplateDetail))

	// Integration webhooks (signature-validated, no session auth)
	mux.HandleFunc("/api/integrations/linear/webhook", s.handleLinearWebhook)
	mux.HandleFunc("/api/integrations/github/webhook", s.handleGitHubWebhook)
	mux.HandleFunc("/api/integrations/shortcut/webhook", s.handleShortcutWebhook)
	mux.HandleFunc("/api/factories/", s.withAuth(s.handleFactoryEvents)) // GET /api/factories/:name/events
	mux.HandleFunc("/api/factories", s.withAuth(s.handleFactoriesCRUD))  // factory CRUD (GET list, POST push)
	mux.HandleFunc("/api/secrets", s.withWebAuth(s.handleSecretsCRUD))   // secrets CRUD (GET names, PUT upsert, DELETE)
	mux.HandleFunc("/api/claws", s.withAuth(s.handleClaws))
	mux.HandleFunc("/api/claws/{id}", s.withAuth(s.handleClawDetail))
	mux.HandleFunc("/api/terminal/", s.handleTerminal)
	mux.HandleFunc("/api/github/token/", s.handleGitHubToken) // credential helper endpoint (claw-token auth)
	mux.HandleFunc("/api/messages/", s.withAuth(s.handleMessages))
	mux.HandleFunc("/api/files/", s.withAuth(s.handleFileUpload))
	mux.HandleFunc("/api/files/view/", s.withAuth(s.handleFileView))
	mux.HandleFunc("/api/claws/", s.withAuth(s.handleClawSubresource)) // /api/claws/:id/prs, /api/claws/:id/settings

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
}

// corsMiddleware adds permissive CORS headers so the web UI can connect
// from any origin (browser same-origin restrictions apply to REST + WS).
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, ngrok-skip-browser-warning")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
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

// ─── Web UI auth (for embedded/static web UI) ───────────────────────────────────
// These endpoints validate the UI password (ui_password) and return a session token
// the browser stores and sends as Authorization: Bearer <token>.

const webSessionHeader = "X-Elasticclaw-Session"

func (s *Server) resolveUIPassword() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hubCfg.UIPassword != "" {
		return s.hubCfg.UIPassword
	}
	return "admin"
}

func (s *Server) withWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get(webSessionHeader)
		}
		// Hub token doubles as the web session token — reject empty tokens even if hub token is unset
		s.mu.RLock()
		hubToken := s.hubCfg.Token
		s.mu.RUnlock()
		if token == "" || token != hubToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleWebLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if body.Password != s.resolveUIPassword() {
		http.Error(w, "invalid password", http.StatusUnauthorized)
		return
	}
	s.mu.RLock()
	hubToken := s.hubCfg.Token
	s.mu.RUnlock()
	jsonOK(w, map[string]interface{}{
		"ok":       true,
		"hubToken": hubToken, // hub API token — browser uses for all hub calls
	})
}

func (s *Server) handleWebLogout(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]bool{"ok": true})
}

func (s *Server) handleWebMe(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]string{"status": "ok"})
}

func (s *Server) serveWebUI(mux *http.ServeMux, staticFS fs.FS) {
	// Register MIME types that may not be set on the host OS
	// (important for embedded static files served from Go)
	for ext, mimeType := range map[string]string{
		".js":    "application/javascript",
		".mjs":   "application/javascript",
		".css":   "text/css",
		".html":  "text/html",
		".json":  "application/json",
		".svg":   "image/svg+xml",
		".png":   "image/png",
		".ico":   "image/x-icon",
		".woff2": "font/woff2",
		".woff":  "font/woff",
	} {
		mime.AddExtensionType(ext, mimeType)
	}
	// Log what's in the embedded FS for debugging
	if entries, err2 := fs.ReadDir(staticFS, "."); err2 == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		log.Printf("[webui] embedded files: %v", names)
	}

	// Wrap file server to serve index.html for directory requests
	// (needed for Next.js static export with trailingSlash: true)
	serveFile := func(w http.ResponseWriter, r *http.Request, path string) {
		content, err := fs.ReadFile(staticFS, path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		ext := filepath.Ext(path)
		if ct, ok := map[string]string{
			".html": "text/html; charset=utf-8",
			".js":   "application/javascript",
			".css":  "text/css",
			".json": "application/json",
			".svg":  "image/svg+xml",
			".png":  "image/png",
			".ico":  "image/x-icon",
			".txt":  "text/plain",
		}[ext]; ok {
			w.Header().Set("Content-Type", ct)
		}
		w.Write(content)
	}

	fileServer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		// Try exact path first
		if f, err := staticFS.Open(p); err == nil {
			stat, _ := f.Stat()
			f.Close()
			if stat != nil && !stat.IsDir() {
				serveFile(w, r, p)
				return
			}
			// It's a dir — try index.html inside
			serveFile(w, r, strings.TrimRight(p, "/")+"/index.html")
			return
		}
		// Unknown path — serve root index.html (SPA fallback)
		serveFile(w, r, "index.html")
	})

	// Serve static files openly — auth is enforced client-side (sessionStorage)
	// and on the API endpoints (withAuth middleware).
	// Static HTML/JS/CSS files don't contain secrets so no server-side gate needed.
	mux.Handle("/", fileServer)
}

func (s *Server) handleHubConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	hubURL := s.hubCfg.URL
	if s.hubCfg.PublicURL != "" {
		hubURL = s.hubCfg.PublicURL
	}
	token := s.hubCfg.Token
	var appName, logoURL string
	if s.hubCfg.Branding != nil {
		appName = s.hubCfg.Branding.AppName
		logoURL = s.hubCfg.Branding.LogoURL
	}
	s.mu.RUnlock()
	if hubURL == "" {
		hubURL = "http://localhost:8080"
	}
	jsonOK(w, map[string]interface{}{
		"token":   token,
		"hubUrl":  hubURL,
		"version": Version,
		"appName": appName,
		"logoUrl": logoURL,
	})
}

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
		`SELECT id, name, template, status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,'') FROM claws WHERE tenant_id = ? AND status != 'deleted' ORDER BY created_at DESC`,
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
		var tagsJSON string
		if err := rows.Scan(&c.ID, &c.Name, &c.Template, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color); err != nil {
			continue
		}
		_ = json.Unmarshal([]byte(tagsJSON), &c.Tags)
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
		} else if c.Status != "provisioning" && c.Status != "starting" && c.Status != "error" {
			// Not currently connected and not in an active provisioning state —
			// DB status is stale (e.g. 'connected' from before hub restart)
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
	s.mu.RLock()
	provCfg, ok := s.hubCfg.Providers[req.Provider]
	s.mu.RUnlock()
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

	nixEnabled := 0
	if req.Nix {
		nixEnabled = 1
	}
	log.Printf("[create] claw %s: req.Nix=%v nixEnabled=%d", req.Name, req.Nix, nixEnabled)

	// Resolve default model: explicit > llm_key lookup > default key > hub default
	defaultModel := req.DefaultModel
	if defaultModel == "" {
		s.mu.RLock()
		var activeKey *types.LLMKeyConfig
		for _, k := range s.hubCfg.LLMKeys {
			if k.Name == req.LLMKey {
				activeKey = k
				break
			}
		}
		// If no explicit key selected, fall back to the default key
		if activeKey == nil {
			for _, k := range s.hubCfg.LLMKeys {
				if k.Default {
					activeKey = k
					break
				}
			}
		}
		if activeKey != nil {
			defaultModel = resolveDefaultModelForKey(s.hubCfg, activeKey)
		} else {
			defaultModel = s.hubCfg.DefaultModel
		}
		s.mu.RUnlock()
	}
	req.DefaultModel = defaultModel

	tags := mergeTags(req.TemplateName, req.Tags, nil) // CLI tags already merged client-side
	tagsJSON, _ := json.Marshal(tags)
	color := resolveColor(req.Color, req.Name)

	// Template can opt out of auto-watching; default is on (1)
	autoFixCI := 1
	if req.AutoWatchCI != nil && !*req.AutoWatchCI {
		autoFixCI = 0
	}
	autoFixBugbot := 1
	if req.AutoWatchBugbot != nil && !*req.AutoWatchBugbot {
		autoFixBugbot = 0
	}

	_, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, tags, color, llm_key, auto_fix_ci, auto_fix_bugbot, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'provisioning',?)`,
		clawID, tenantID, req.Name, req.TemplateName, req.Provider, req.DefaultModel, string(filesJSON),
		githubReposJSON, linearWorkspace, nixEnabled, string(tagsJSON), color, req.LLMKey, autoFixCI, autoFixBugbot, now(),
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	// Build env to inject: hub connection info so the claw can register back
	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":    clawID,
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
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
	// Use a stable short ID as the provider-side name so renaming the claw
	// doesn't require a provider API call.
	req.ProviderName = "ec-" + clawID[:8]
	go func() {
		log.Printf("Provisioning claw %s (%s) via %s (provider name: %s)...", req.Name, clawID, req.Provider, req.ProviderName)
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
	clawID := r.PathValue("id")
	if clawID == "" {
		clawID = strings.TrimPrefix(r.URL.Path, "/api/claws/")
	}

	if r.Method == http.MethodPatch {
		var body struct {
			Name  *string   `json:"name"`
			Tags  *[]string `json:"tags"`
			Color *string   `json:"color"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			_, _ = s.db.Exec(`UPDATE claws SET name = ? WHERE id = ? AND tenant_id = ?`, strings.TrimSpace(*body.Name), clawID, tenantID)
		}
		if body.Tags != nil {
			// Normalize tags to k=v format
			normalized := make([]string, 0, len(*body.Tags))
			seen := make(map[string]bool)
			for _, t := range *body.Tags {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if !seen[t] {
					seen[t] = true
					normalized = append(normalized, t)
				}
			}
			tagsJSON, _ := json.Marshal(normalized)
			_, _ = s.db.Exec(`UPDATE claws SET tags = ?, updated_at = datetime('now') WHERE id = ? AND tenant_id = ?`, string(tagsJSON), clawID, tenantID)
		}
		if body.Color != nil {
			color := resolveColor(*body.Color, clawID)
			_, _ = s.db.Exec(`UPDATE claws SET color = ? WHERE id = ? AND tenant_id = ?`, color, clawID, tenantID)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method == http.MethodDelete {
		// Resolve short ID prefix to full UUID
		var fullID string
		_ = s.db.QueryRow(`SELECT id FROM claws WHERE tenant_id = ? AND (id = ? OR id LIKE ?)`, tenantID, clawID, clawID+"%").Scan(&fullID)
		if fullID != "" {
			clawID = fullID
		}

		// Look up provider info before deleting so we can terminate the VM
		var provider, providerID string
		_ = s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&provider, &providerID)

		// Delete messages first (FK constraint)
		_, _ = s.db.Exec(`DELETE FROM messages WHERE claw_id = ?`, clawID)
		_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id = ?`, clawID)
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
	var tagsJSON string
	err := s.db.QueryRow(
		`SELECT id, name, template, status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,'') FROM claws WHERE id = ? AND tenant_id = ?`,
		clawID, tenantID,
	).Scan(&c.ID, &c.Name, &c.Template, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color)
	_ = json.Unmarshal([]byte(tagsJSON), &c.Tags)
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

	// Pagination: ?before=<created_at>&limit=<n> for older messages
	// ?after=<created_at>&limit=<n> for newer messages
	// Default: last 100 messages
	const defaultLimit = 100
	limit := defaultLimit
	before := r.URL.Query().Get("before") // ISO timestamp — return messages older than this
	after := r.URL.Query().Get("after")   // ISO timestamp — return messages newer than this

	var rows *sql.Rows
	var err error
	if before != "" {
		// Fetch older messages — return in ASC order after fetching DESC
		rows, err = s.db.Query(
			`SELECT id, claw_id, tenant_id, role, content, created_at FROM messages
			 WHERE claw_id = ? AND tenant_id = ? AND created_at < ?
			 ORDER BY created_at DESC LIMIT ?`,
			clawID, tenantID, before, limit,
		)
	} else if after != "" {
		rows, err = s.db.Query(
			`SELECT id, claw_id, tenant_id, role, content, created_at FROM messages
			 WHERE claw_id = ? AND tenant_id = ? AND created_at > ?
			 ORDER BY created_at ASC LIMIT ?`,
			clawID, tenantID, after, limit,
		)
	} else {
		// Default: last N messages
		rows, err = s.db.Query(
			`SELECT id, claw_id, tenant_id, role, content, created_at FROM messages
			 WHERE claw_id = ? AND tenant_id = ?
			 ORDER BY created_at DESC LIMIT ?`,
			clawID, tenantID, limit,
		)
	}
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
	// Reverse DESC results to get ASC order
	if before != "" || (before == "" && after == "") {
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
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

	// Upsert claw and keep terminal/watching states sticky across reconnects.
	desiredStatus := initialStatus(rp.GatewayReady)
	currentStatus := desiredStatus
	_ = s.db.QueryRow(
		`INSERT INTO claws(id,tenant_id,name,template,status,last_seen,created_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, template=excluded.template,
		 status=CASE WHEN claws.status IN ('idle','deleted') THEN claws.status ELSE excluded.status END,
		 last_seen=excluded.last_seen
		 RETURNING status`,
		clawID, tenantID, rp.Name, rp.Template, desiredStatus, now(), now(),
	).Scan(&currentStatus)
	if currentStatus == "deleted" {
		conn.Close(websocket.StatusPolicyViolation, "claw deleted")
		return
	}

	cc := &clawConn{id: clawID, tenantID: tenantID, conn: conn, gatewayReady: gatewayReadyBool(rp.GatewayReady)}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()

	log.Printf("[bridge] ✓ connected: %s (%s) gateway_ready=%v", rp.Name, clawID[:8], cc.gatewayReady)

	// Ack
	_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "registered", Payload: map[string]string{"claw_id": clawID}})

	// Broadcast initial status to user sessions
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": currentStatus}})

	// Initialize entry pipeline stage only after bridge connects so on_enter inject
	// can be delivered over WS.
	usedPipelineEntryInject := false
	if cc.gatewayReady && currentStatus == "connected" {
		usedPipelineEntryInject = s.initializePipelineEntryIfNeeded(clawID)
	}
	// If no pipeline entry inject was sent, fire the default wake message.
	// But don't re-wake claws that already have a pipeline stage (hub restart reconnect).
	if cc.gatewayReady && currentStatus == "connected" && !usedPipelineEntryInject {
		if s.getPipelineStage(clawID) == "" {
			go s.sendWakeMessage(cc, clawID)
		}
	}

	// Read loop — claw sends messages back to users
	defer func() {
		s.mu.Lock()
		// Flush any partial streaming buffer as an interrupted message
		if partialCC, ok := s.claws[clawID]; ok && partialCC.streamingBuf.Len() > 0 {
			partialContent := partialCC.streamingBuf.String() + " [interrupted]"
			partialMsgID := partialCC.streamingMsgID
			if partialMsgID == "" {
				partialMsgID = uuid.New().String()
			}
			partialCC.streamingBuf.Reset()
			partialCC.streamingMsgID = ""
			s.mu.Unlock()
			_, _ = s.db.Exec(
				`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)
				 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
				partialMsgID, clawID, tenantID, "claw", partialContent, now(),
			)
			s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: types.HubMessage{
				ID: partialMsgID, ClawID: clawID, TenantID: tenantID, Role: "claw",
				Content: partialContent, CreatedAt: now(),
			}})
			s.mu.Lock()
		}
		delete(s.claws, clawID)
		s.mu.Unlock()
		var currentStatus string
		_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
		// Don't overwrite terminal/watching states — idle means the claw sent [DONE]
		// and is watching for PR merge; deleted means it's being cleaned up.
		if currentStatus != "completed" && currentStatus != "deleted" && currentStatus != "idle" {
			_, _ = s.db.Exec(`UPDATE claws SET status='offline', last_seen=? WHERE id=?`, now(), clawID)
			s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": "offline"}})
		}
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
			conn.SetReadLimit(32 << 20) // 32MB (file uploads ride this channel)
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
					var wakeConn *clawConn
					var shouldWake bool
					var prevUsage int
					s.mu.Lock()
					if cc, ok := s.claws[clawID]; ok {
						// Log only on status changes, not every heartbeat
						prevHealthy := cc.gatewayReady
						prevUsage = cc.contextUsage
						cc.contextUsage = hb.ContextUsage
						// Promote from 'starting' to 'connected' once gateway is ready.
						// nil means field absent (old bridge) — treat as ready.
						if gatewayReadyBool(hb.GatewayReady) && !cc.gatewayReady {
							cc.gatewayReady = true
							res, execErr := s.db.Exec(`UPDATE claws SET status='connected' WHERE id=? AND status='starting'`, clawID)
							var rowsUpdated int64
							if execErr == nil {
								rowsUpdated, _ = res.RowsAffected()
							}
							if rowsUpdated > 0 {
								s.broadcastToUsers(tenantID, types.WSMessage{
									Type:    "claw_status",
									Payload: map[string]string{"claw_id": clawID, "status": "connected"},
								})
								log.Printf("[bridge] ✓ ready: %s (%s)", rp.Name, clawID[:8])
								shouldWake = true
								wakeConn = cc
							}
						} else if !hb.GatewayHealthy && prevHealthy {
							// Gateway went unhealthy
							log.Printf("[heartbeat] %s (%s): gateway unhealthy", rp.Name, clawID[:8])
						} else if hb.ContextUsage != prevUsage && (hb.ContextUsage >= 80 || prevUsage >= 80) {
							// Log context usage only when crossing 80% threshold
							log.Printf("[heartbeat] %s (%s): context_usage=%d%%", rp.Name, clawID[:8], hb.ContextUsage)
						}
					}
					// Inject context warning once per streaming turn when usage is >=95%
					var shouldWarnContext bool
					if cc2, ok2 := s.claws[clawID]; ok2 && hb.ContextUsage >= 95 && !cc2.contextWarningSent {
						cc2.contextWarningSent = true
						shouldWarnContext = true
					}
					s.mu.Unlock()
					if shouldWarnContext {
						s.mu.RLock()
						warnCC := s.claws[clawID]
						s.mu.RUnlock()
						if warnCC != nil {
							go s.injectHubMessage(ctx, warnCC, "[hub] Context window is nearly full. Summarize your progress briefly and send [DONE] with any PR URL, or ask the user what to do next.")
						}
					}
					if shouldWake {
						if !s.initializePipelineEntryIfNeeded(clawID) && s.getPipelineStage(clawID) == "" {
							go s.sendWakeMessage(wakeConn, clawID)
						}
					}
					// Check for streaming turn timeout (12 minutes)
					s.mu.Lock()
					if cc, ok := s.claws[clawID]; ok &&
						!cc.streamingStartedAt.IsZero() &&
						!cc.streamingTimeoutSent &&
						time.Since(cc.streamingStartedAt) > 12*time.Minute {
						cc.streamingTimeoutSent = true
						s.mu.Unlock()
						go s.injectHubMessage(ctx, cc, "[hub] Your current response has been running for over 12 minutes. Please wrap up and send your response.")
					} else {
						s.mu.Unlock()
					}
				}
			} else if msg.Type == "chunk" {
				// Streaming chunk — forward to users immediately AND buffer server-side
				payload, _ := json.Marshal(msg.Payload)
				var chunk struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal(payload, &chunk); err == nil && chunk.Content != "" {
					s.broadcastToUsers(tenantID, types.WSMessage{
						Type:    "chunk",
						Payload: map[string]string{"claw_id": clawID, "content": chunk.Content},
					})
					// Buffer chunk and upsert partial message to DB so refreshes don't lose it
					s.mu.Lock()
					if cc, ok := s.claws[clawID]; ok {
						if cc.streamingMsgID == "" {
							cc.streamingMsgID = uuid.New().String()
							cc.streamingStartedAt = time.Now()
							cc.streamingTimeoutSent = false
							cc.contextWarningSent = false
						}
						cc.streamingBuf.WriteString(chunk.Content)
						msgID := cc.streamingMsgID
						bufContent := cc.streamingBuf.String()
						s.mu.Unlock()
						// Upsert — insert on first chunk, update content on subsequent
						_, _ = s.db.Exec(
							`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)
							 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
							msgID, clawID, tenantID, "claw", bufContent, now(),
						)
					} else {
						s.mu.Unlock()
					}
				}
			} else if msg.Type == "message" {
				// Complete message — finalize the buffered stream or store fresh
				payload, _ := json.Marshal(msg.Payload)
				var hm types.HubMessage
				if err := json.Unmarshal(payload, &hm); err != nil {
					continue
				}
				// Use the streaming message ID if we already started buffering
				s.mu.Lock()
				if cc, ok := s.claws[clawID]; ok && cc.streamingMsgID != "" {
					hm.ID = cc.streamingMsgID
					cc.streamingMsgID = ""
					cc.streamingBuf.Reset()
					cc.streamingStartedAt = time.Time{}
					cc.streamingTimeoutSent = false
				} else {
					hm.ID = uuid.New().String()
				}
				s.mu.Unlock()
				hm.ClawID = clawID
				hm.TenantID = tenantID
				hm.Role = "claw"
				hm.CreatedAt = now()
				// Skip empty messages — never store or broadcast
				if strings.TrimSpace(hm.Content) == "" {
					continue
				}
				_, _ = s.db.Exec(
					`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)
					 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
					hm.ID, hm.ClawID, hm.TenantID, hm.Role, hm.Content, hm.CreatedAt,
				)
				s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: hm})
				// Check for [DONE] signal from a factory-created claw
				if strings.Contains(hm.Content, "[DONE]") {
					go s.handleClawDoneSignal(clawID, hm.Content)
				}
				// Detect and store any PR URLs mentioned by the agent
				go s.scanMessageForPRs(clawID, hm.Content)
				// Detect tool error loops and inject a corrective message
				if detectToolLoop(hm.Content) {
					s.mu.RLock()
					loopCC := s.claws[clawID]
					s.mu.RUnlock()
					if loopCC != nil {
						go s.injectHubMessage(ctx, loopCC, "[hub] You've hit the same tool error 3+ times in a row. Stop retrying. Take a completely different approach or ask for help.")
					}
				}
			} else if msg.Type == "file_ack" {
				raw, _ := json.Marshal(msg.Payload)
				var ack types.FileAck
				if err := json.Unmarshal(raw, &ack); err == nil && ack.RequestID != "" {
					s.fileAckMu.Lock()
					ch := s.fileAckWaiters[ack.RequestID]
					delete(s.fileAckWaiters, ack.RequestID)
					s.fileAckMu.Unlock()
					if ch != nil {
						select {
						case ch <- ack:
						default:
						}
					}
				}
			} else if msg.Type == "file_read_resp" {
				raw, _ := json.Marshal(msg.Payload)
				var resp types.FileReadResp
				if err := json.Unmarshal(raw, &resp); err == nil && resp.RequestID != "" {
					s.fileAckMu.Lock()
					ch := s.fileReadWaiters[resp.RequestID]
					delete(s.fileReadWaiters, resp.RequestID)
					s.fileAckMu.Unlock()
					if ch != nil {
						select {
						case ch <- resp:
						default:
						}
					}
				}
			} else if msg.Type == "http_proxy_req" {
				// Proxy an HTTP request from the bridge to the hub's internal API.
				// This allows tools in the sandbox to reach hub APIs without a public URL.
				go func(rawPayload json.RawMessage, conn *websocket.Conn) {
					var req struct {
						ReqID  string            `json:"req_id"`
						Method string            `json:"method"`
						Path   string            `json:"path"`
						Query  string            `json:"query"`
						Body   string            `json:"body"`
						Header map[string]string `json:"header"`
					}
					if err := json.Unmarshal(rawPayload, &req); err != nil {
						return
					}
					// Build an internal HTTP request
					urls := req.Path
					if req.Query != "" {
						urls += "?" + req.Query
					}
					httpReq, err := http.NewRequest(req.Method, "http://localhost"+urls, strings.NewReader(req.Body))
					if err != nil {
						s.sendHTTPProxyRes(ctx, conn, req.ReqID, 400, "bad request")
						return
					}
					for k, v := range req.Header {
						httpReq.Header.Set(k, v)
					}
					// Inject claw_token auth so withAuth middleware passes
					s.mu.RLock()
					clawToken := s.hubCfg.ClawToken
					s.mu.RUnlock()
					httpReq.Header.Set("X-Claw-Token", clawToken)
					// Execute against internal mux
					w := &proxyResponseWriter{header: make(http.Header)}
					s.mux.ServeHTTP(w, httpReq)
					s.sendHTTPProxyRes(ctx, conn, req.ReqID, w.status, string(w.body))
				}(mustJSONRaw(msg.Payload), conn)
			}
		}
	}
}

func (s *Server) sendHTTPProxyRes(ctx context.Context, conn *websocket.Conn, reqID string, status int, body string) {
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":    "http_proxy_res",
		"payload": map[string]interface{}{"req_id": reqID, "status": status, "body": body},
	})
}

// proxyResponseWriter captures an HTTP handler's response.
type proxyResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *proxyResponseWriter) Header() http.Header {
	return w.header
}
func (w *proxyResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}
func (w *proxyResponseWriter) WriteHeader(status int) {
	w.status = status
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

	// Send current claw statuses immediately on connect.
	// First, emit DB rows for claws not yet bridge-connected (provisioning/starting/error).
	type dbClaw struct {
		id, name, status string
	}
	var dbClaws []dbClaw
	rows, _ := s.db.QueryContext(ctx, `SELECT id, name, status FROM claws WHERE tenant_id=? AND status NOT IN ('offline')`, tenantID)
	if rows != nil {
		for rows.Next() {
			var c dbClaw
			_ = rows.Scan(&c.id, &c.name, &c.status)
			dbClaws = append(dbClaws, c)
		}
		_ = rows.Close()
	}
	s.mu.RLock()
	connectedIDs := make(map[string]bool)
	for _, cc := range s.claws {
		if cc.tenantID != tenantID {
			continue
		}
		connectedIDs[cc.id] = true
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
	// Emit DB-only claws (still bootstrapping, not yet bridge-connected)
	for _, c := range dbClaws {
		if connectedIDs[c.id] {
			continue // already sent above
		}
		_ = wsjson.Write(ctx, conn, types.WSMessage{
			Type: "claw_status",
			Payload: map[string]interface{}{
				"claw_id": c.id,
				"name":    c.name,
				"status":  c.status, // provisioning / starting / error
			},
		})
	}

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

func mustJSONRaw(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}

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
		Name:          req.ProviderName, // stable ec-<shortid>, decoupled from display name
		FromImage:     snapshot,
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
	clawName := req.Name
	go func() {
		if err := s.bootstrapDaytona(context.Background(), clawID, clawName, instance.ID, p, env); err != nil {
			log.Printf("daytona bootstrap failed for claw %s: %v", clawID, err)
			_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
		}
	}()
	return nil
}

func (s *Server) bootstrapDaytona(ctx context.Context, clawID, clawName, instanceID string, p *daytona.Provider, env map[string]string) error {
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
	// Uninstall old openclaw then reinstall latest (ensures nvm current symlink is updated)
	if err := exec("uninstall old openclaw", 20*time.Second,
		`NPM="/usr/local/share/nvm/current/bin/npm"; \
sudo "$NPM" uninstall -g openclaw 2>/dev/null || true; \
echo uninstalled`); err != nil {
		log.Printf("[daytona] warning: uninstall failed (ok if not installed): %v", err)
	}

	if err := exec("start openclaw install", 20*time.Second,
		`NPM="/usr/local/share/nvm/current/bin/npm"; \
NODE_VER=$(ls /usr/local/share/nvm/versions/node/ | head -1); \
PREFIX="/usr/local/share/nvm/versions/node/${NODE_VER}"; \
sudo "$NPM" install -g openclaw@latest --prefix "$PREFIX" --ignore-scripts > /tmp/openclaw-install.log 2>&1 & \
echo $! > /tmp/openclaw-install.pid && echo 'install started'`); err != nil {
		return err
	}

	// Wait for npm install to complete — install takes ~35s, 90s is a safe margin
	log.Printf("[daytona] waiting for openclaw install...")
	time.Sleep(90 * time.Second)
	log.Printf("[daytona] openclaw install wait done")

	if err := exec("verify openclaw", 20*time.Second,
		"export HOME=/home/daytona; /usr/local/share/nvm/current/bin/openclaw --version || openclaw --version"); err != nil {
		return err
	}

	// Step 2: Onboard (configure OpenClaw) with the correct auth provider
	var llmKeyNameDaytona string
	_ = s.db.QueryRow(`SELECT COALESCE(llm_key,'') FROM claws WHERE id=?`, clawID).Scan(&llmKeyNameDaytona)
	s.mu.RLock()
	activeKeyDaytona := resolveActiveKey(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	defaultModelDaytona := resolveDefaultModelForKey(s.hubCfg, activeKeyDaytona)
	llmKeyEnvDaytona := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	onboardFlags := buildOnboardFlags(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	providerConfigScript := buildOpenClawProviderConfig(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	s.mu.RUnlock()
	onboardCmd := fmt.Sprintf(
		"%sexport NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; openclaw onboard --non-interactive --accept-risk --skip-daemon %s 2>&1 || true",
		llmKeyEnvDaytona,
		onboardFlags,
	)
	if err := exec("onboard openclaw", 2*time.Minute, onboardCmd); err != nil {
		return err
	}

	// Step 2b: Patch OpenClaw config with dynamic provider model list
	if providerConfigScript != "" {
		configPatch := fmt.Sprintf("export HOME=/home/daytona; export OPENCLAW_DEFAULT_MODEL=%q; ", defaultModelDaytona) + llmKeyEnvDaytona + providerConfigScript
		if err := exec("configure openclaw model", 30*time.Second, configPatch); err != nil {
			log.Printf("[daytona] warning: failed to configure model: %v", err)
		}
	}
	// Let openclaw self-heal any remaining config schema issues
	_ = exec("openclaw doctor fix", 20*time.Second,
		"export HOME=/home/daytona NVM_DIR=/usr/local/share/nvm; "+
			"export PATH=$NVM_DIR/current/bin:$PATH; "+
			"openclaw doctor --fix 2>&1 || true")

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
export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; setsid nohup openclaw gateway run >> ~/.openclaw/gateway.log 2>&1 </dev/null &
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
	hubID := HubID(s.identity.PublicKey)
	s.mu.RLock()
	relayURL := s.hubCfg.RelayURL
	relaySecret := s.hubCfg.RelaySecret
	clawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	relayToken := RelayToken(relaySecret, hubID, clawToken)

	startCmd := fmt.Sprintf(
		`export HOME=/home/daytona; \
ELASTICCLAW_HUB_URL=%q ELASTICCLAW_CLAW_ID=%q ELASTICCLAW_CLAW_TOKEN=%q ELASTICCLAW_CLAW_NAME=%q \
ELASTICCLAW_RELAY_URL=%q ELASTICCLAW_HUB_ID=%q ELASTICCLAW_RELAY_TOKEN=%q \
setsid nohup /tmp/claw-bridge >> /tmp/claw-bridge.log 2>&1 </dev/null &
echo started`,
		s.clawHubURL(), clawID, clawToken, clawName,
		relayURL, hubID, relayToken)
	if err := exec("start claw-bridge", 30*time.Second, startCmd); err != nil {
		return err
	}

	// Write template files (SOUL.md, AGENTS.md, etc.) to the workspace
	var filesJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&filesJSON)
	var templateFiles map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &templateFiles); err == nil && len(templateFiles) > 0 {
		for name, content := range templateFiles {
			name := name
			content := content
			writeCmd := fmt.Sprintf(
				`export HOME=/home/daytona; mkdir -p ~/.openclaw/workspace && cat > ~/.openclaw/workspace/%s << 'ELASTICCLAW_EOF'
%s
ELASTICCLAW_EOF`,
				name, content)
			if err := exec("write "+name, 15*time.Second, writeCmd); err != nil {
				log.Printf("[daytona] warning: failed to write %s: %v", name, err)
			}
		}
		log.Printf("[daytona] template files written for claw %s", clawID)
	}

	// Step 5: GitHub credential helper (if GitHub Apps configured)
	s.mu.RLock()
	hasGitHubApps := len(s.hubCfg.GitHubApps) > 0
	s.mu.RUnlock()
	if hasGitHubApps {
		var githubRepos []types.GitHubRepoAccess
		var reposJSON string
		_ = s.db.QueryRow(`SELECT COALESCE(github_repos,'[]') FROM claws WHERE id=?`, clawID).Scan(&reposJSON)
		_ = json.Unmarshal([]byte(reposJSON), &githubRepos)

		// Use local HTTP proxy (bridge listens on 18790, proxies to hub)
		tokenURL := fmt.Sprintf("http://localhost:18790/api/github/token/%s?claw_token=%s", clawID, clawToken)

		// Step 5a: write the credential helper binary
		credHelperScript := fmt.Sprintf(`export HOME=/home/daytona
sudo tee /usr/local/bin/elasticclaw-git-credentials > /dev/null << 'CREDEOF'
#!/bin/bash
response=$(curl -sf %q)
if [ $? -ne 0 ] || [ -z "$response" ]; then exit 1; fi
token=$(echo "$response" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "protocol=https"
echo "host=github.com"
echo "username=x-access-token"
echo "password=$token"
CREDEOF
sudo chmod +x /usr/local/bin/elasticclaw-git-credentials
git config --global credential.helper /usr/local/bin/elasticclaw-git-credentials
echo 'credential helper installed'`, tokenURL)
		// Wait for the bridge to connect and register with hub (needed for HTTP proxy)
		log.Printf("[daytona] waiting for bridge to connect...")
		for i := 0; i < 20; i++ {
			s.mu.RLock()
			_, connected := s.claws[clawID]
			s.mu.RUnlock()
			if connected {
				break
			}
			time.Sleep(3 * time.Second)
		}

		if err := exec("install git credential helper", 20*time.Second, credHelperScript); err != nil {
			log.Printf("[daytona] warning: credential helper install failed: %v", err)
		} else {
			// Step 5b: authenticate gh CLI
			ghAuthScript := `export HOME=/home/daytona
if command -v gh &>/dev/null; then
  GH_TOKEN=$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | grep ^password | cut -d= -f2)
  if [ -n "$GH_TOKEN" ]; then
    echo "$GH_TOKEN" | gh auth login --with-token 2>/dev/null && echo 'gh CLI authenticated' || echo 'gh auth failed'
    printf 'export GH_TOKEN=$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | grep ^password | cut -d= -f2)\n' | sudo tee /etc/profile.d/elasticclaw-github.sh > /dev/null
    sudo chmod +x /etc/profile.d/elasticclaw-github.sh
  fi
else
  echo 'gh not installed'
fi`
			if err := exec("auth gh cli", 20*time.Second, ghAuthScript); err != nil {
				log.Printf("[daytona] warning: gh auth failed: %v", err)
			}

			// Step 5c: clone repos into workspace
			if len(githubRepos) > 0 {
				cloneScript := buildGitHubCloneScript(githubRepos)
				if cloneScript != "" {
					if err := exec("clone repos", 2*time.Minute, "export HOME=/home/daytona; cd ~/.openclaw/workspace; "+cloneScript); err != nil {
						log.Printf("[daytona] warning: repo clone failed: %v", err)
					}
				}
			}
		}
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
	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	bridgeScript := fmt.Sprintf(`
curl -fsSL "%s" -o /tmp/claw-bridge && chmod +x /tmp/claw-bridge
ELASTICCLAW_HUB_URL=%q ELASTICCLAW_CLAW_ID=%q ELASTICCLAW_CLAW_TOKEN=%q nohup /tmp/claw-bridge >> /tmp/claw-bridge.log 2>&1 &
echo "claw-bridge started"
`, bridgeURL, s.clawHubURL(), clawID, clawToken)
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
		Name:         req.ProviderName, // stable ec-<shortid>
		InstanceType: req.InstanceType,
		TTL:          req.TTL,
	}, nil, env)
	if err != nil {
		return fmt.Errorf("replicated provision: %w", err)
	}
	// Store vm_id in the claw record — skip if already deleted (factory terminated mid-provision)
	_, _ = s.db.Exec(
		`UPDATE claws SET status='starting', provider='replicated', provider_id=? WHERE id=? AND status != 'deleted'`, vmID, clawID,
	)
	// If deleted, clean up the VM and bail
	var currentStatus string
	_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
	if currentStatus == "deleted" {
		log.Printf("[provision] claw %s deleted mid-provision, destroying VM %s", clawID[:8], vmID)
		_ = p.DeleteVM(ctx, vmID)
		return fmt.Errorf("claw deleted mid-provision")
	}

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
	s.mu.RLock()
	replicatedCfg, ok := s.hubCfg.Providers["replicated"]
	s.mu.RUnlock()
	if !ok || replicatedCfg.Token == "" {
		return
	}

	// Find claws provisioned on Replicated that are still in a VM-managed state.
	// Exclude hub-managed statuses (idle, connected) — those claws don't need VM polling.
	rows, err := s.db.Query(`
		SELECT id, tenant_id, name, provider_id, status
		FROM claws
		WHERE provider = 'replicated'
		  AND provider_id != ''
		  AND status IN ('provisioning', 'starting')
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

		// Only overwrite provisioning/starting statuses — never clobber hub-managed
		// statuses (idle, connected, deleted, error) which have higher semantic meaning.
		// Use a conditional UPDATE so we race-safely check the current DB value.
		if newStatus != c.status {
			res, execErr := s.db.Exec(
				`UPDATE claws SET status=? WHERE id=? AND status IN ('provisioning','starting')`,
				newStatus, c.id)
			if execErr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					log.Printf("Claw %s (%s): VM %s %s → hub status %s",
						c.name, c.id[:8], c.providerID, vm.Status, newStatus)
					s.broadcastToUsers(c.tenantID, types.WSMessage{
						Type:    "claw_status",
						Payload: map[string]string{"claw_id": c.id, "status": newStatus},
					})
				}
			}
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
// sendWakeMessage sends a silent system message to wake the agent.
// For factory claws, it sends a task-specific prompt.
// Not stored in DB — invisible to the user but triggers the agent to respond.
func (s *Server) sendWakeMessage(cc *clawConn, clawID string) {
	wakeContent := "Introduce yourself briefly and let the user know you're ready to help."
	if factory, _ := s.findFactoryForClaw(clawID); factory != nil {
		wakeContent = `Read your BOOTSTRAP.md now. Then:
1. Send a short intro message to the user: your name, the issue you're working on, and your plan.
2. Start working. As you go, narrate your progress — what you're exploring, what you're trying, why.
3. If you hit something interesting or unexpected, say so.
4. When you open a PR, summarize what you did and what the PR contains.
5. Do NOT ask for permission at any point. Just work and keep the user informed.`
	}
	wakeMsg := types.HubMessage{
		ID:      uuid.New().String(),
		ClawID:  clawID,
		Role:    "system",
		Content: wakeContent,
	}
	_ = wsjson.Write(context.Background(), cc.conn, types.WSMessage{Type: "message", Payload: wakeMsg})
}

func (s *Server) bootstrapReplicated(clawID, clawName, vmID string, cfg types.ProviderConfig) {
	// Bail immediately if claw was deleted while VM was spinning up
	var checkStatus string
	_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&checkStatus)
	if checkStatus == "deleted" {
		log.Printf("[bootstrap] claw %s deleted before bootstrap, destroying VM %s", clawID[:8], vmID)
		p, _ := newReplicatedProvider(cfg)
		if p != nil {
			_ = p.DeleteVM(context.Background(), vmID)
		}
		return
	}

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
	// Read nix flag
	var nixEnabled int
	if err := s.db.QueryRow(`SELECT nix FROM claws WHERE id=?`, clawID).Scan(&nixEnabled); err != nil {
		log.Printf("[bootstrap] warning: could not read nix flag for claw %s: %v", clawID[:8], err)
	}
	log.Printf("[bootstrap] claw %s nix=%d", clawID[:8], nixEnabled)
	// Read llm_key selection
	var llmKeyName string
	_ = s.db.QueryRow(`SELECT COALESCE(llm_key,'') FROM claws WHERE id=?`, clawID).Scan(&llmKeyName)

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

	s.mu.RLock()
	// Inject all configured LLM keys, prioritizing the selected key if specified
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	relayEnv := buildRelayEnv(s.hubCfg, s.identity.PublicKey)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.mu.RUnlock()

	script := GenerateReplicatedBootstrapScript(BootstrapParams{
		ClawID:          clawID,
		ClawName:        clawName,
		ClawToken:       clawToken,
		HubURL:          s.clawHubURL(),
		DefaultModel:    defaultModel,
		GatewayPassword: gatewayPassword,
		BridgeURL:       bridgeURL,
		Nix:             nixEnabled != 0,
		HubCfg:          hubCfg,
		GitHubRepos:     githubRepos,
		LLMKeyEnv:       llmKeyEnv,
		LinearEnv:       buildLinearEnv(linearToken),
		RelayEnv:        relayEnv,
		ProviderConfig:  buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName),
		OnboardFlags:    buildOnboardFlags(hubCfg.LLMKeys, llmKeyName),
	})
	// Inject GitHub tools context into TOOLS.md if GitHub is configured
	s.mu.RLock()
	hasGitHubApps2 := len(s.hubCfg.GitHubApps) > 0
	s.mu.RUnlock()
	if hasGitHubApps2 && len(githubRepos) > 0 {
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

	// Run bootstrap script first — this installs OpenClaw and initializes the workspace.
	// Template files must be written AFTER the script completes so openclaw onboard
	// doesn't overwrite BOOTSTRAP.md and other workspace files.
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

	// Write template files AFTER bootstrap — openclaw onboard initializes the workspace
	// and would overwrite BOOTSTRAP.md if we wrote it before the script ran.
	if len(files) > 0 {
		fileNames := make([]string, 0, len(files))
		for k := range files { fileNames = append(fileNames, k) }
		log.Printf("[bootstrap] writing %d template files for claw %s: %v", len(files), clawName, fileNames)
		for attempt := 1; attempt <= 3; attempt++ {
			if err := s.sshWriteFiles(sshUser, sshHost, "$HOME/.openclaw/workspace", files); err == nil {
				log.Printf("Template files written for claw %s", clawName)
				break
			} else if attempt == 3 {
				log.Printf("Warning: failed to write template files: %v", err)
			} else {
				time.Sleep(5 * time.Second)
			}
		}
	}

	log.Printf("Bootstrap complete for claw %s (%s)", clawName, clawID[:8])
}

// randomHex returns a random hex string of n bytes (2*n hex chars).
// mergeTags combines tags from all sources in priority order:
// 1. auto tag (template:<name>)
// 2. template config tags (elasticclaw-config.yaml)
// 3. CLI --tag flags
// Deduplicates while preserving order.
var clawColors = []string{
	"slate", "red", "orange", "amber", "lime", "green", "emerald", "teal",
	"cyan", "sky", "blue", "indigo", "violet", "purple", "pink", "rose",
}

var clawColorSet = func() map[string]bool {
	m := make(map[string]bool, len(clawColors))
	for _, c := range clawColors {
		m[c] = true
	}
	return m
}()

// resolveColor returns the color for a claw.
// Uses the requested color if valid, otherwise auto-assigns from the claw name.
func resolveColor(requested, clawName string) string {
	if requested != "" && clawColorSet[requested] {
		return requested
	}
	// Hash name → deterministic color
	var h uint32
	for _, c := range clawName {
		h = h*31 + uint32(c)
	}
	return clawColors[h%uint32(len(clawColors))]
}

func mergeTags(templateName string, configTags []string, cliTags []string) []string {
	seen := make(map[string]bool)
	var result []string
	add := func(t string) {
		if t == "" {
			return
		}
		if !seen[t] {
			seen[t] = true
			result = append(result, t)
		}
	}
	add("template:" + templateName)
	for _, t := range configTags {
		add(t)
	}
	for _, t := range cliTags {
		add(t)
	}
	return result
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// clawHubURL returns the URL claws should use to connect back.
// Uses public_url if set, otherwise falls back to url.
func (s *Server) clawHubURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hubCfg.PublicURL != "" {
		return s.hubCfg.PublicURL
	}
	return s.hubCfg.URL
}

// buildNixInstall returns shell script lines to install Determinate Systems Nix.
// Returns an empty comment when nix is not enabled.
func buildNixInstall(enabled bool) string {
	if !enabled {
		return "# Nix not enabled for this template"
	}
	return `# ── Install Nix (Determinate Systems) ──────────────────────────────────────────
echo "Installing Nix (Determinate Systems)..."
# Use systemd init if available, otherwise start daemon manually
NIX_INIT_MODE="none"
if command -v systemctl &>/dev/null && systemctl is-system-running --quiet 2>/dev/null; then
  NIX_INIT_MODE="systemd"
fi
curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix | \
  sh -s -- install linux --no-confirm --init "$NIX_INIT_MODE"
# Source profile to get nix in PATH
if [ -e /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh ]; then
  . /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh
fi
# Start daemon manually when systemd is not available
if [ "$NIX_INIT_MODE" = "none" ]; then
  echo "Starting nix daemon (no systemd)..."
  sudo /nix/var/nix/profiles/default/bin/nix-daemon &
  sleep 3
  export NIX_REMOTE=daemon
fi
# Persist Nix in PATH for all future shells
echo '. /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true' | sudo tee /etc/profile.d/nix.sh > /dev/null
echo "Nix: $(nix --version 2>/dev/null || echo 'installed')"`
}

// buildRelayEnv returns shell lines that export relay env vars for the bridge.
// When relay is not configured, returns an empty comment.
func buildRelayEnv(cfg *types.HubConfig, publicKey string) string {
	relayURL := cfg.RelayURL
	if relayURL == "" {
		return "# Relay not configured"
	}
	hubID := HubID(publicKey)
	relayToken := RelayToken(cfg.RelaySecret, hubID, cfg.ClawToken)
	return fmt.Sprintf("export ELASTICCLAW_RELAY_URL=%q\nexport ELASTICCLAW_HUB_ID=%q\nexport ELASTICCLAW_RELAY_TOKEN=%q",
		relayURL, hubID, relayToken)
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

// buildLLMKeyEnv converts llm_keys slice to shell env var export lines.
// If selectedKeyName is non-empty, the selected key is prioritized over default keys.
// All keys are exported so each claw has access to whichever provider it needs.
func buildLLMKeyEnv(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	if len(keys) == 0 {
		return ""
	}
	var b strings.Builder
	seen := map[string]bool{}

	// First pass: export the selected key if specified
	if selectedKeyName != "" {
		for _, k := range keys {
			if k.Name == selectedKeyName {
				envVar := k.EnvVarName()
				seen[envVar] = true
				fmt.Fprintf(&b, "export %s=%q\n", envVar, k.APIKey)
				break
			}
		}
	}

	// Second pass: export default keys for providers not yet seen
	for _, k := range keys {
		if !k.Default {
			continue
		}
		envVar := k.EnvVarName()
		if seen[envVar] {
			continue
		}
		seen[envVar] = true
		fmt.Fprintf(&b, "export %s=%q\n", envVar, k.APIKey)
	}
	// Third pass: export non-default keys for providers not yet seen
	for _, k := range keys {
		envVar := k.EnvVarName()
		if seen[envVar] {
			continue
		}
		seen[envVar] = true
		fmt.Fprintf(&b, "export %s=%q\n", envVar, k.APIKey)
	}
	return b.String()
}

// resolveDefaultModelForKey returns the effective model for a given LLM key.
// If the hub's default model matches the key's provider, use it; otherwise construct a provider-specific default.
func resolveDefaultModelForKey(hubCfg *types.HubConfig, key *types.LLMKeyConfig) string {
	if key == nil {
		return hubCfg.DefaultModel
	}

	// Use per-key default model if set; normalize to include provider prefix
	if key.DefaultModel != "" {
		prefix := key.Provider + "/"
		if !strings.HasPrefix(key.DefaultModel, prefix) {
			return prefix + key.DefaultModel
		}
		return key.DefaultModel
	}

	// Check if hub's DefaultModel matches this key's provider
	if hubCfg.DefaultModel != "" && strings.HasPrefix(hubCfg.DefaultModel, key.Provider+"/") {
		return hubCfg.DefaultModel
	}

	// Construct a provider-specific default model
	switch key.Provider {
	case "anthropic":
		return "anthropic/claude-sonnet-4-6"
	case "openai":
		return "openai/gpt-4o"
	case "fireworks":
		return "fireworks/accounts/fireworks/models/llama-v3p3-70b-instruct"
	case "groq":
		return "groq/llama-3.3-70b-versatile"
	case "deepseek":
		return "deepseek/deepseek-chat"
	case "moonshot":
		return "moonshot/moonshot-v1-8k"
	default:
		// Fall back to hub default even if provider doesn't match
		return hubCfg.DefaultModel
	}
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
		fmt.Fprintf(&b, "if [ ! -d %q ]; then git clone https://github.com/%s %s && echo 'Cloned %s' || FAILED=1; else git -C %s pull --ff-only && echo 'Updated %s' || FAILED=1; fi\n",
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
if command -v gh &>/dev/null; then
  # Write GH_TOKEN to /etc/profile.d so it's available in ALL shells
  printf 'export GH_TOKEN=$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | grep ^password | cut -d= -f2)\n' | sudo tee /etc/profile.d/elasticclaw-github.sh > /dev/null
  sudo chmod +x /etc/profile.d/elasticclaw-github.sh
  echo "GitHub profile.d configured"
fi
echo "GitHub credential helper installed"

# Clone repos — non-fatal: token may not be available until bridge connects
# The agent can clone manually if this fails
cd "$HOME/.openclaw/workspace" || true
(
set +e
FAILED=0
%s
exit $FAILED
) || echo "Warning: repo clone failed — agent can retry after bridge connects"`, tokenURL, buildGitHubCloneScript(repos))
}

// syncedWriter wraps a bytes.Buffer with a mutex to make it safe for concurrent writes.
type syncedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *syncedWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
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

	// Pipe the script to bash via stdin — avoids the server's default shell (/bin/sh,
	// often dash on Ubuntu) which may not support bash-specific syntax.
	var buf bytes.Buffer
	var mu sync.Mutex
	syncWriter := &syncedWriter{buf: &buf, mu: &mu}
	sess.Stdout = syncWriter
	sess.Stderr = syncWriter
	sess.Stdin = strings.NewReader(script)
	if err := sess.Run("/bin/bash"); err != nil {
		mu.Lock()
		output := buf.String()
		mu.Unlock()
		return fmt.Errorf("ssh script failed: %w\noutput: %s", err, output)
	}
	mu.Lock()
	output := buf.String()
	mu.Unlock()
	log.Printf("bootstrap output:\n%s", output)
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
			Cols uint32 `json:"cols"`
			Rows uint32 `json:"rows"`
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

// terminateVM terminates a provider VM by type and ID.
func (s *Server) terminateVM(provider, vmID string) {
	if vmID == "" {
		return
	}
	switch provider {
	case "replicated":
		s.terminateReplicatedVM(vmID)
	case "daytona":
		s.terminateDaytonaVM(vmID)
	default:
		log.Printf("terminateVM: unsupported provider %q for VM %s", provider, vmID)
	}
}

// terminateDaytonaVM destroys a Daytona workspace by ID.
func (s *Server) terminateDaytonaVM(workspaceID string) {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["daytona"]
	s.mu.RUnlock()
	if !ok {
		return
	}
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		log.Printf("terminateDaytonaVM: provider init error: %v", err)
		return
	}
	if err := p.Destroy(context.Background(), workspaceID, false); err != nil {
		log.Printf("terminateDaytonaVM: failed to destroy workspace %s: %v", workspaceID, err)
		return
	}
	log.Printf("Daytona workspace %s terminated", workspaceID)
}

// terminateReplicatedVM terminates a Replicated CMX VM by ID.
func (s *Server) terminateReplicatedVM(vmID string) {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["replicated"]
	s.mu.RUnlock()
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
	s.mu.RLock()
	hasGitHubApps := len(s.hubCfg.GitHubApps) > 0
	s.mu.RUnlock()
	if !hasGitHubApps {
		http.Error(w, "no github apps configured", http.StatusNotImplemented)
		return
	}

	clawToken := r.URL.Query().Get("claw_token")
	if clawToken == "" {
		clawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	// Single-tenant: validate against hub's claw_token directly
	s.mu.RLock()
	hubClawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	if clawToken != hubClawToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	clawID := strings.TrimPrefix(r.URL.Path, "/api/github/token/")
	if clawID == "" {
		http.Error(w, "missing claw id", http.StatusBadRequest)
		return
	}

	var reposJSON string
	err := s.db.QueryRow(
		`SELECT github_repos FROM claws WHERE id = ?`,
		clawID,
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
	s.mu.RLock()
	githubApps := s.hubCfg.GitHubApps
	s.mu.RUnlock()
	for i, appCfg := range githubApps {
		provider, err := NewGitHubTokenProvider(appCfg)
		if err != nil {
			log.Printf("github app[%d] (app_id=%d url=%s) config error: %v", i, appCfg.AppID, appCfg.URL, err)
			continue
		}
		token, expiresAt, err := provider.InstallationToken(r.Context(), 0, repos)
		if err != nil {
			// Debug-level only — expected when multiple apps configured and only one matches
			log.Printf("[github] app[%d] app_id=%d: no match for repos (trying next): %v", i, appCfg.AppID, err)
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

// detectToolLoop returns true if the same class of tool error appears 3+ times
// in the content of a completed assistant turn.
func detectToolLoop(content string) bool {
	patterns := []string{"edit failed:", "write failed:", "read failed:"}
	for _, p := range patterns {
		if strings.Count(strings.ToLower(content), p) >= 3 {
			return true
		}
	}
	return false
}

// injectHubMessage sends a hub-role message to the claw over its WebSocket
// connection and persists it to the DB so it appears in the message history.
// Hub messages are visually distinct from user messages in the UI.
func (s *Server) injectHubMessage(ctx context.Context, cc *clawConn, text string) {
	msg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    cc.id,
		TenantID:  cc.tenantID,
		Role:      "hub",
		Content:   text,
		CreatedAt: now(),
	}
	_, _ = s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.CreatedAt,
	)
	_ = wsjson.Write(ctx, cc.conn, types.WSMessage{Type: "message", Payload: msg})
	s.broadcastToUsers(cc.tenantID, types.WSMessage{Type: "message", Payload: msg})
}
