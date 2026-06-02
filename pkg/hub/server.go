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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/internal/webui"

	daytona "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	exedevProvider "github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	replicatedpkg "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
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
	// one-time oauth_code -> signed GitHub session token

	fileAckMu       sync.Mutex
	fileAckWaiters  map[string]chan types.FileAck      // request_id -> waiter
	fileReadWaiters map[string]chan types.FileReadResp // request_id -> waiter

	checkpointMu      sync.Mutex
	checkpointWaiters map[string]chan error // checkpoint_id -> waiter

	// githubBaseURL overrides the GitHub API base for testing (default: https://api.github.com)
	githubBaseURL string
	// linearBaseURL overrides the Linear API base for testing (default: https://api.linear.app)
	linearBaseURL string
	// shortcutBaseURL overrides the Shortcut API base for testing (default: https://api.app.shortcut.com)
	shortcutBaseURL string

	// webhookDedup prevents duplicate Linear webhook deliveries from creating
	// duplicate claws. Keyed by issue transition fingerprint; entries expire after 30s.
	webhookDedup   map[string]time.Time
	webhookDedupMu sync.Mutex

	// promoteMu serializes promotePendingClaws to prevent TOCTOU race where
	// multiple terminating claws each read active < max and promote, exceeding limit.
	promoteMu sync.Mutex
}

type clawConn struct {
	mu sync.RWMutex // protects mutable fields below

	id                    string
	tenantID              string
	conn                  *websocket.Conn
	tags                  []string        // cached from DB at registration time for access-control checks
	contextUsage          int             // 0-100, updated from heartbeats
	gatewayReady          bool            // true once bridge reports gateway session established
	gatewayUnhealthyCount int             // consecutive unhealthy heartbeats
	streamingBuf          strings.Builder // accumulates chunks for current in-flight response
	streamingMsgID        string          // pre-assigned message ID for the current stream
	streamingStartedAt    time.Time       // when the current streaming turn started (zero if not streaming)
	streamingTimeoutSent  bool            // true once the 12-min timeout message has been injected this turn
	contextWarningSent    bool            // true once the context-nearly-full warning has been injected this turn

	// Message queue for when claw is busy processing
	messageQueue []types.HubMessage // queued messages waiting to be sent

	pendingCheckpointReason string // coalesced checkpoint request to run after current turn
	pendingCheckpointID     string
	pendingCheckpointBy     string
	checkpointInProgress    bool

	// Status channel for watchdog / progress reporting (second session on bridge)
	statusConn            *websocket.Conn // separate WS for lightweight status queries
	lastStatusAt          time.Time       // when we last got a status response
	lastUserMessageAt     time.Time       // when the user last sent a message (for idle detection)
	lastStatusBroadcastAt time.Time       // when we last broadcast status to user
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
	conn        *websocket.Conn
	send        func(context.Context, types.WSMessage) error
	tenantID    string
	githubLogin string
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
		db:                db,
		addr:              addr,
		hubCfg:            hubCfg,
		identity:          id,
		claws:             make(map[string]*clawConn),
		users:             make(map[string]*userConn),
		fileAckWaiters:    make(map[string]chan types.FileAck),
		fileReadWaiters:   make(map[string]chan types.FileReadResp),
		checkpointWaiters: make(map[string]chan error),
		webhookDedup:      make(map[string]time.Time),
	}

	// Start background poller to keep provider VM status fresh
	go srv.pollProviderStatus()
	go srv.keepAliveDaytonaSandboxes()
	go srv.pruneAnalytics()
	go srv.statusWatchdog()
	go srv.checkpointScheduler()
	srv.startPRWatcher()
	srv.startIntegrationPoller()

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
	mux.HandleFunc("/api/ws", s.withAuth(s.handleUserWS))

	// REST API
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/auth/login", s.handleWebLogin)
	mux.HandleFunc("/api/auth/logout", s.handleWebLogout)
	mux.HandleFunc("/api/auth/me", s.withWebAuth(s.handleWebMe))
	mux.HandleFunc("/api/auth/config", s.handleAuthConfig)               // public — no auth required
	mux.HandleFunc("/api/auth/github/client-id", s.handleGitHubClientID) // public
	mux.HandleFunc("/api/auth/github/exchange", s.handleGitHubOAuthExchange)
	mux.HandleFunc("/api/hub-config", s.withWebAdminAuth(s.handleHubConfig))
	mux.HandleFunc("/api/settings", s.withWebAdminAuth(s.handleSettings))
	mux.HandleFunc("/api/settings/status", s.withWebAdminAuth(s.handleSettingsStatus))
	mux.HandleFunc("/api/settings/github/test", s.withWebAdminAuth(s.handleGitHubAppTest))

	// Template store
	mux.HandleFunc("/api/templates", s.withWebAuth(s.handleTemplates))
	mux.HandleFunc("/api/templates/{name}", s.withWebAuth(s.handleTemplateDetail))

	// Integration webhooks (signature-validated, no session auth)
	mux.HandleFunc("/api/integrations/linear/webhook", s.handleLinearWebhook)
	mux.HandleFunc("/api/integrations/github/webhook", s.handleGitHubWebhook)
	mux.HandleFunc("/api/integrations/github-issues/webhook", s.handleGitHubIssuesWebhook)
	mux.HandleFunc("/api/integrations/shortcut/webhook", s.handleShortcutWebhook)
	mux.HandleFunc("/api/integrations/external/webhook", s.handleExternalWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/linear", s.handleLinearWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/github", s.handleGitHubWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/github-issues", s.handleGitHubIssuesWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/shortcut", s.handleShortcutWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/external", s.handleExternalWebhook)
	mux.HandleFunc("/api/factories/", s.withAuth(s.handleFactoryEvents))                    // GET /api/factories/:name/events
	mux.HandleFunc("/api/factories/{name}/trigger", s.withAuth(s.handleFactoryTrigger))     // POST manual trigger
	mux.HandleFunc("/api/factories/{name}/analytics", s.withAuth(s.handleFactoryAnalytics)) // GET factory analytics
	mux.HandleFunc("/api/factories", s.withAuth(s.handleFactoriesCRUD))                     // factory CRUD (GET list, POST push)
	mux.HandleFunc("/api/analytics/factories", s.withAuth(s.handleAllFactoriesAnalytics))   // GET all factories analytics
	mux.HandleFunc("/api/workspaces", s.withAuth(s.handleWorkspacesCRUD))                   // workspace CRUD
	mux.HandleFunc("/api/workspaces/{name}/workflows", s.withAuth(s.handleWorkspaceWorkflowsList))
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}", s.withAuth(s.handleWorkspaceWorkflowDetail))
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/trigger", s.withAuth(s.handleWorkspaceWorkflowTrigger))
	mux.HandleFunc("/api/workspaces/{workspace}/secrets", s.withAuth(s.handleWorkspaceSecretsCRUD))
	mux.HandleFunc("/api/workspaces/{workspace}/github-apps", s.withAuth(s.handleWorkspaceGitHubAppsCRUD))
	mux.HandleFunc("/api/workspaces/{workspace}/issue-trackers", s.withAuth(s.handleWorkspaceIssueTrackersCRUD))
	mux.HandleFunc("/api/secrets", s.withWebAdminAuth(s.handleSecretsCRUD)) // secrets CRUD (GET names, PUT upsert, DELETE)
	mux.HandleFunc("/api/mcp", s.withWebAdminAuth(s.handleMCPCrud))         // MCP server CRUD (GET list, PUT upsert, DELETE)
	mux.HandleFunc("/api/claws", s.withAuth(s.handleClaws))
	mux.HandleFunc("/api/claws/{id}", s.withAuth(s.handleClawDetail))
	mux.HandleFunc("/api/checkpoints/blob/", s.handleCheckpointBlobUpload)
	mux.HandleFunc("/api/checkpoints/", s.handleCheckpointInternal)
	mux.HandleFunc("/api/terminal/", s.handleTerminal)
	mux.HandleFunc("/api/github/token/", s.handleGitHubToken) // credential helper endpoint (claw-token auth)
	mux.HandleFunc("/api/messages/", s.withAuth(s.handleMessages))
	mux.HandleFunc("/api/files/", s.withAuth(s.handleFileUpload))
	mux.HandleFunc("/api/files/view/", s.withAuth(s.handleFileView))
	mux.HandleFunc("/api/claws/", s.withAuth(s.handleClawSubresource)) // /api/claws/:id/prs, /api/claws/:id/settings

	// AI Config
	// AI Config — register sub-paths before the bare path so Go's exact-match
	// ServeMux routes them correctly (avoids 404 on specific sub-paths).
	mux.HandleFunc("/api/settings/ai-config/apply", s.withWebAdminAuth(s.handleAIConfigApply))
	mux.HandleFunc("/api/settings/ai-config/revert", s.withWebAdminAuth(s.handleAIConfigRevert))
	mux.HandleFunc("/api/settings/ai-config/backup", s.withWebAdminAuth(s.handleAIConfigBackup))
	mux.HandleFunc("/api/settings/ai-config/stream", s.withWebAdminAuth(s.handleAIConfigStream))
	mux.HandleFunc("/api/settings/ai-config/current-config", s.withWebAdminAuth(s.handleAIConfigCurrentConfig))
	mux.HandleFunc("/api/settings/ai-config", s.withWebAdminAuth(s.handleAIConfig))

	// Doctor / Troubleshoot
	mux.HandleFunc("/api/doctor", s.withWebAdminAuth(s.handleDoctor))
	mux.HandleFunc("/api/troubleshoot/stream", s.withWebAdminAuth(s.handleTroubleshootStream))

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if bridgePath, bridgeToken := os.Getenv("ELASTICCLAW_E2E_BRIDGE_BINARY"), os.Getenv("ELASTICCLAW_E2E_BRIDGE_TOKEN"); bridgePath != "" && bridgeToken != "" {
		mux.HandleFunc("/__elasticclaw_e2e/claw-bridge-linux-amd64", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if r.URL.Query().Get("token") != bridgeToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.ServeFile(w, r, bridgePath)
		})
	}

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
		tenantID, githubLogin, ok := s.resolveAuthToken(token)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxTenantKey{}, tenantID)
		if githubLogin != "" {
			ctx = context.WithValue(ctx, ctxGitHubLoginKey{}, githubLogin)
		}
		r = r.WithContext(ctx)
		next(w, r)
	}
}

type ctxTenantKey struct{}

func tenantFromCtx(r *http.Request) string {
	v, _ := r.Context().Value(ctxTenantKey{}).(string)
	return v
}

// resolveAuthToken accepts either a tenant token (legacy/password auth)
// or a GitHub OAuth session token and returns the resolved tenant/login.
func (s *Server) resolveAuthToken(token string) (tenantID, githubLogin string, ok bool) {
	if token == "" {
		return "", "", false
	}
	// Accept the hub config token directly — this means a token change in hub.yaml
	// takes effect immediately without requiring a DB migration.
	s.mu.RLock()
	hubCfgToken := s.hubCfg.Token
	s.mu.RUnlock()
	if token == hubCfgToken && hubCfgToken != "" {
		if tid, err := s.githubTenantID(); err == nil {
			return tid, "", true
		}
	}
	if tenantID, err := s.tenantByToken(token); err == nil {
		return tenantID, "", true
	}
	sessionSecret := s.webSessionSecret()
	if sessionSecret == "" {
		return "", "", false
	}
	payload, valid := verifyGitHubSession(sessionSecret, token)
	if !valid {
		return "", "", false
	}
	tenantID, err := s.githubTenantID()
	if err != nil {
		return "", "", false
	}
	return tenantID, payload.Login, true
}

// githubTenantID resolves the tenant backing GitHub OAuth sessions.
func (s *Server) githubTenantID() (string, error) {
	s.mu.RLock()
	hubToken := s.hubCfg.Token
	s.mu.RUnlock()
	if hubToken != "" {
		if tenantID, err := s.tenantByToken(hubToken); err == nil {
			return tenantID, nil
		}
	}
	var tenantID string
	err := s.db.QueryRow(`SELECT id FROM tenants ORDER BY created_at ASC LIMIT 1`).Scan(&tenantID)
	return tenantID, err
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
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.mu.RLock()
		hubToken := s.hubCfg.Token
		s.mu.RUnlock()
		sessionSecret := s.webSessionSecret()

		// Accept shared hub token (existing behavior)
		if token == hubToken {
			next(w, r)
			return
		}

		// Try GitHub OAuth session token
		if sessionSecret != "" {
			if payload, ok := verifyGitHubSession(sessionSecret, token); ok {
				ctx := context.WithValue(r.Context(), ctxGitHubLoginKey{}, payload.Login)
				ctx = context.WithValue(ctx, ctxGitHubSessionPayloadKey{}, payload)
				r = r.WithContext(ctx)
				next(w, r)
				return
			}
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func isAccessAdmin(cfg *types.AccessConfig, login string) bool {
	if login == "" || cfg == nil {
		return false
	}
	for _, admin := range cfg.Admins {
		if strings.EqualFold(admin, login) {
			return true
		}
	}
	return false
}

func (s *Server) withWebAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get(webSessionHeader)
		}
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		s.mu.RLock()
		hubToken := s.hubCfg.Token
		var accessCfg *types.AccessConfig
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()
		sessionSecret := s.webSessionSecret()

		// Password-authenticated sessions keep existing admin access.
		if token == hubToken {
			next(w, r)
			return
		}

		if sessionSecret != "" {
			if payload, ok := verifyGitHubSession(sessionSecret, token); ok {
				if !isAccessAdmin(accessCfg, payload.Login) {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				ctx := context.WithValue(r.Context(), ctxGitHubLoginKey{}, payload.Login)
				r = r.WithContext(ctx)
				next(w, r)
				return
			}
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (s *Server) handleWebLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	disablePassword := s.hubCfg.Auth != nil && s.hubCfg.Auth.DisablePasswordAuth
	s.mu.RUnlock()
	if disablePassword {
		http.Error(w, "password login disabled", http.StatusForbidden)
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
	if payload := githubSessionPayloadFromContext(r.Context()); payload != nil {
		s.mu.RLock()
		var accessCfg *types.AccessConfig
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()
		jsonOK(w, map[string]interface{}{
			"login":       payload.Login,
			"name":        payload.Name,
			"avatar_url":  payload.AvatarURL,
			"auth_method": "github",
			"is_admin":    isAccessAdmin(accessCfg, payload.Login),
		})
		return
	}
	jsonOK(w, map[string]interface{}{"auth_method": "password", "is_admin": true})
}

// handleAuthConfig returns public auth config (no auth required).
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	githubOAuthEnabled := s.hubCfg.Auth != nil && s.hubCfg.Auth.GitHubOAuth != nil && s.hubCfg.Auth.GitHubOAuth.ClientID != ""
	passwordAuthEnabled := s.hubCfg.Token != "" && !(s.hubCfg.Auth != nil && s.hubCfg.Auth.DisablePasswordAuth)
	s.mu.RUnlock()
	jsonOK(w, map[string]bool{
		"github_oauth_enabled":  githubOAuthEnabled,
		"password_auth_enabled": passwordAuthEnabled,
	})
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
		// Try exact path first (file or directory)
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
		// embed.FS doesn't support Open() on directories — the dir check above
		// may have failed. Try index.html at this path before falling back.
		if !strings.HasSuffix(p, "/index.html") {
			idxPath := strings.TrimRight(p, "/") + "/index.html"
			if f, err := staticFS.Open(idxPath); err == nil {
				f.Close()
				serveFile(w, r, idxPath)
				return
			}
		}
		if workspacePath, ok := settingsWorkspaceStaticPath(p); ok {
			if f, err := staticFS.Open(workspacePath); err == nil {
				f.Close()
				serveFile(w, r, workspacePath)
				return
			}
		}
		// Unknown path — serve root index.html (SPA fallback)
		serveFile(w, r, "index.html")
	})

	// Serve static files openly — auth is enforced client-side (sessionStorage)
	// and on the API endpoints (withAuth middleware).
	// Static HTML/JS/CSS files don't contain secrets so no server-side gate needed.
	mux.Handle("/", fileServer)
}

func settingsWorkspaceStaticPath(requestPath string) (string, bool) {
	p := strings.Trim(strings.TrimPrefix(requestPath, "/"), "/")
	if p == "" {
		return "", false
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "settings" {
		return "", false
	}
	if parts[1] == "" || settingsStaticSection(parts[1]) || strings.HasPrefix(parts[1], "_") {
		return "", false
	}
	if len(parts) == 2 {
		return "settings/_workspace/index.html", true
	}
	if !settingsStaticSection(parts[2]) {
		return "", false
	}
	return "settings/_workspace/" + parts[2] + "/index.html", true
}

func settingsStaticSection(section string) bool {
	switch section {
	case "runtimes",
		"models",
		"github",
		"authentication",
		"issue-trackers",
		"workspaces",
		"workflows",
		"workspace-analytics",
		"secrets",
		"ai-config",
		"mcp-servers",
		"analytics",
		"doctor",
		"troubleshoot":
		return true
	default:
		return false
	}
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
		`SELECT id, name, template, COALESCE(provider,''), COALESCE(provider_id,''), status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,''), COALESCE(bootstrap_status,'') FROM claws WHERE tenant_id = ? AND status != 'deleted' ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		log.Printf("handleClaws query error: %v", err)
		http.Error(w, fmt.Sprintf("db error: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	// Resolve access config and GitHub login for tag-based filtering
	s.mu.RLock()
	var accessCfg *types.AccessConfig
	if s.hubCfg.Auth != nil {
		accessCfg = s.hubCfg.Auth.Access
	}
	s.mu.RUnlock()
	ghLogin := githubLoginFromContext(r.Context())

	var out []types.Claw
	for rows.Next() {
		var c types.Claw
		var lastSeen sql.NullTime
		var tagsJSON string
		if err := rows.Scan(&c.ID, &c.Name, &c.Template, &c.Provider, &c.ProviderID, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color, &c.BootstrapStatus); err != nil {
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
		} else if c.Status != "provisioning" && c.Status != "starting" && c.Status != "error" && c.Status != "pending" {
			// Not currently connected and not in an active provisioning state —
			// DB status is stale (e.g. 'connected' from before hub restart)
			c.Status = "offline"
		}
		// Apply tag-based view filter (only applies to GitHub OAuth users)
		if ghLogin != "" && !canViewClaw(accessCfg, ghLogin, c.Tags) {
			continue
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

	// Build env to inject: hub connection info so the claw can register back
	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	hubSecrets := s.hubCfg.Secrets
	s.mu.RUnlock()
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":    clawID,
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
	}
	for k, v := range req.Env {
		env[k] = v
	}
	for envName, secretRef := range req.SecretRefs {
		if val, ok := hubSecrets[secretRef]; ok {
			env[envName] = val
			log.Printf("[create] injected secret_ref %s as %s into claw env", secretRef, envName)
		} else {
			log.Printf("[create] WARNING: secret_ref %q not found in hub secrets", secretRef)
		}
	}
	req.Env = env
	req.Files = injectFigmaAPIDocs(req.Files, env)
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
	dockerEnabled := 0
	if req.Docker {
		dockerEnabled = 1
	}
	log.Printf("[create] claw %s: nix=%d docker=%d", req.Name, nixEnabled, dockerEnabled)

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

	_, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, req.Name, req.TemplateName, req.Provider, req.DefaultModel, string(filesJSON),
		githubReposJSON, linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), color, req.LLMKey, "provisioning", now(),
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	// Convert string files to bytes for the provider
	templateFiles := make(map[string][]byte, len(req.Files))
	for k, v := range req.Files {
		templateFiles[k] = []byte(v)
	}

	// Resolve MCP server configs from template + hub config
	var mcpConfigs []*types.MCPConfig
	if len(req.MCPs) > 0 {
		s.mu.RLock()
		hubMCPServers := s.hubCfg.MCPServers
		hubSecrets := s.hubCfg.Secrets
		s.mu.RUnlock()
		for _, mcpRef := range req.MCPs {
			var hubMCP *types.MCPServerHubConfig
			for _, hm := range hubMCPServers {
				if hm.Name == mcpRef.Name {
					hubMCP = hm
					break
				}
			}
			if hubMCP == nil {
				log.Printf("[create] MCP server %q not found in hub config, skipping", mcpRef.Name)
				continue
			}
			if !hubMCP.Enabled {
				log.Printf("[create] MCP server %q is disabled, skipping", mcpRef.Name)
				continue
			}
			// Build command
			cmd := hubMCP.Command
			if len(cmd) == 0 {
				switch hubMCP.Source {
				case types.MCPSourceNpx:
					cmd = []string{"npx", "-y", hubMCP.Package}
				case types.MCPSourceUvx:
					cmd = []string{"uvx", hubMCP.Package}
				case types.MCPSourceSmithery:
					cmd = []string{"npx", "-y", "@smithery/cli@latest", "run", hubMCP.Package}
				case types.MCPSourceDocker:
					cmd = []string{"docker", "run", "-i", "--rm", hubMCP.Image}
				case types.MCPSourceSSE:
					// SSE is remote — no local command, skip for now
					log.Printf("[create] SSE MCP server %q not yet supported for local stdio", mcpRef.Name)
					continue
				}
			}
			if len(cmd) == 0 {
				log.Printf("[create] MCP server %q has no command, skipping", mcpRef.Name)
				continue
			}
			// Build env: merge hub config + template overrides + resolved secrets
			mcpEnv := make(map[string]string)
			for k, v := range hubMCP.Config {
				mcpEnv[k] = v
			}
			// Template-level overrides (from MCPRef.Config) take precedence over hub-level config
			for k, v := range mcpRef.Env {
				mcpEnv[k] = v
			}
			for envVar, secretRef := range hubMCP.Secrets {
				if val, ok := hubSecrets[secretRef]; ok {
					mcpEnv[envVar] = val
				}
			}
			mcpConfigs = append(mcpConfigs, &types.MCPConfig{
				Name:    mcpRef.Name,
				Command: cmd,
				Env:     mcpEnv,
			})
		}
	}
	req.MCPs = mcpConfigs

	// Provision asynchronously so the HTTP request returns quickly
	// Use a stable short ID as the provider-side name so renaming the claw
	// doesn't require a provider API call.
	providerNamePrefix := strings.TrimSpace(os.Getenv("ELASTICCLAW_PROVIDER_NAME_PREFIX"))
	if providerNamePrefix == "" {
		providerNamePrefix = "ec-"
	}
	req.ProviderName = providerNamePrefix + clawID[:8]
	go func() {
		log.Printf("Provisioning claw %s (%s) via %s (provider name: %s)...", req.Name, clawID, req.Provider, req.ProviderName)
		ctx := context.Background()
		var provErr error

		switch req.Provider {
		case "daytona":
			provErr = s.provisionDaytona(ctx, clawID, req, provCfg, templateFiles, env)
		case "replicated":
			provErr = s.provisionReplicated(ctx, clawID, req, provCfg, env)
		case "exedev":
			provErr = s.provisionExedev(ctx, clawID, req, provCfg, templateFiles, env)
		default:
			provErr = fmt.Errorf("unsupported provider: %s", req.Provider)
		}

		if provErr != nil {
			log.Printf("provisioning failed for claw %s: %v", clawID, provErr)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Provisioning failed: %v", provErr), false)
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
	ghLogin := githubLoginFromContext(r.Context())
	var accessCfg *types.AccessConfig
	if ghLogin != "" {
		s.mu.RLock()
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()
	}

	if r.Method == http.MethodPatch {
		if ghLogin != "" {
			var tagsJSON string
			if err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSON); err != nil {
				if err == sql.ErrNoRows {
					http.Error(w, "not found", http.StatusNotFound)
				} else {
					http.Error(w, "db error", http.StatusInternalServerError)
				}
				return
			}
			var clawTags []string
			_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
			if !canModifyClaw(accessCfg, ghLogin, clawTags) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
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
			// Update in-memory cache so WS broadcast filtering stays current
			s.mu.Lock()
			if cc, ok := s.claws[clawID]; ok {
				cc.tags = normalized
			}
			s.mu.Unlock()
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
		if ghLogin != "" {
			var tagsJSON string
			if err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSON); err != nil {
				if err == sql.ErrNoRows {
					http.Error(w, "not found", http.StatusNotFound)
				} else {
					http.Error(w, "db error", http.StatusInternalServerError)
				}
				return
			}
			var clawTags []string
			_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
			if !canModifyClaw(accessCfg, ghLogin, clawTags) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}

		// Look up provider info before marking deleted so we can terminate the VM.
		var provider, providerID string
		_ = s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&provider, &providerID)

		// Post a comment on the linked issue/story when a factory-created claw is killed manually
		factory, issueID := s.findFactoryForClaw(clawID)
		if factory != nil && issueID != "" {
			switch factory.Integration {
			case "linear":
				token := s.resolveLinearTokenForFactory(factory)
				if token != "" {
					if err := s.commentLinearIssue(token, issueID, "Agent stopped: killed manually via dashboard"); err != nil {
						log.Printf("[kill] failed to comment Linear issue %s: %v", issueID, err)
					} else {
						log.Printf("[kill] commented Linear issue %s", issueID)
					}
				}
			case "shortcut":
				token := s.resolveShortcutToken(factory.Workspace)
				if token != "" {
					if err := commentShortcutIssue(s.resolveShortcutBaseURL(), token, issueID, "Agent stopped: killed manually via dashboard"); err != nil {
						log.Printf("[kill] failed to comment Shortcut story %s: %v", issueID, err)
					} else {
						log.Printf("[kill] commented Shortcut story %s", issueID)
					}
				}
			case "github-issues":
				parts := strings.Split(issueID, "/")
				if len(parts) == 3 {
					token := s.resolveGitHubIssuesTokenForFactory(factory)
					if token != "" {
						repo := parts[0] + "/" + parts[1]
						var issueNum int
						if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
							if err := commentGitHubIssue(token, repo, issueNum, "Agent stopped: killed manually via dashboard"); err != nil {
								log.Printf("[kill] failed to comment GitHub issue %s: %v", issueID, err)
							} else {
								log.Printf("[kill] commented GitHub issue %s", issueID)
							}
						}
					}
				}
			}
		}

		_, err := s.db.Exec(`UPDATE claws SET status='deleted', bootstrap_status='' WHERE id = ? AND tenant_id = ?`, clawID, tenantID)
		if err != nil {
			log.Printf("kill: db soft-delete error for claw %s: %v", clawID, err)
			http.Error(w, fmt.Sprintf("db error: %v", err), http.StatusInternalServerError)
			return
		}
		// Notify dashboards before provider cleanup so the card disappears immediately.
		s.broadcastToUsers(tenantID, types.WSMessage{
			Type:    "claw_status",
			Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
		})
		// Disconnect WebSocket if online
		s.mu.Lock()
		if cc, ok := s.claws[clawID]; ok {
			cc.conn.Close(websocket.StatusNormalClosure, "killed")
			delete(s.claws, clawID)
		}
		s.mu.Unlock()
		go func() {
			s.checkpointBeforeTermination(clawID, "manual-kill")
			if providerID != "" {
				s.terminateVM(provider, providerID)
			}
			_, _ = s.db.Exec(`DELETE FROM messages WHERE claw_id = ?`, clawID)
			_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id = ?`, clawID)
		}()
		// Promote any pending claws now that a slot is free
		go s.promotePendingClaws()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var c types.Claw
	var lastSeen sql.NullTime
	var tagsJSON string
	err := s.db.QueryRow(
		`SELECT id, name, template, COALESCE(provider,''), COALESCE(provider_id,''), status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,''), COALESCE(bootstrap_status,'') FROM claws WHERE id = ? AND tenant_id = ? AND status != 'deleted'`,
		clawID, tenantID,
	).Scan(&c.ID, &c.Name, &c.Template, &c.Provider, &c.ProviderID, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color, &c.BootstrapStatus)
	_ = json.Unmarshal([]byte(tagsJSON), &c.Tags)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if ghLogin != "" && !canViewClaw(accessCfg, ghLogin, c.Tags) {
		http.Error(w, "forbidden", http.StatusForbidden)
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
	ghLoginMsg := githubLoginFromContext(r.Context())
	var accessCfgMsg *types.AccessConfig
	if ghLoginMsg != "" {
		s.mu.RLock()
		if s.hubCfg.Auth != nil {
			accessCfgMsg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()
	}

	if r.Method == http.MethodPost {
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == "" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		// Apply tag-based interact filter for GitHub OAuth users
		if ghLoginMsg != "" {
			// Fetch claw tags to check interact permission
			var tagsJSONMsg string
			if err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSONMsg); err != nil {
				if err == sql.ErrNoRows {
					http.Error(w, "not found", http.StatusNotFound)
				} else {
					http.Error(w, "db error", http.StatusInternalServerError)
				}
				return
			}
			var clawTagsMsg []string
			_ = json.Unmarshal([]byte(tagsJSONMsg), &clawTagsMsg)
			if !canInteractWithClaw(accessCfgMsg, ghLoginMsg, clawTagsMsg) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
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
		// Forward to claw if connected (or queue if busy)
		s.mu.RLock()
		cc := s.claws[clawID]
		s.mu.RUnlock()
		if cc != nil {
			cc.mu.Lock()
			cc.lastUserMessageAt = time.Now()
			// Check if claw is currently streaming/processing
			isBusy := !cc.streamingStartedAt.IsZero() || cc.streamingMsgID != ""
			if isBusy {
				// Queue the message for later delivery
				cc.messageQueue = append(cc.messageQueue, msg)
				queueLen := len(cc.messageQueue)
				cc.mu.Unlock()
				log.Printf("[hub] message queued for %s (queue length: %d)", clawID[:8], queueLen)
			} else {
				cc.mu.Unlock()
				// Send immediately
				_ = wsjson.Write(r.Context(), cc.conn, types.WSMessage{Type: "message", Payload: msg})
				// Immediately signal to UI that agent is working, before first chunk arrives
				s.broadcastToUsers(tenantID, types.WSMessage{
					Type: "agent_typing",
					Payload: map[string]string{
						"claw_id": clawID,
						"status":  "typing",
					},
				})
			}
		}
		jsonOK(w, msg)
		return
	}
	if ghLoginMsg != "" {
		var tagsJSONMsg string
		err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSONMsg)
		if err == sql.ErrNoRows {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var clawTagsMsg []string
		_ = json.Unmarshal([]byte(tagsJSONMsg), &clawTagsMsg)
		if !canViewClaw(accessCfgMsg, ghLoginMsg, clawTagsMsg) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
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
			`SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at FROM messages
			 WHERE claw_id = ? AND tenant_id = ? AND created_at < ?
			 AND NOT (role = 'system' AND content IN (?, ?, ?))
			 ORDER BY created_at DESC LIMIT ?`,
			clawID, tenantID, before, wakeMessageMarker, defaultWakeContent, factoryWakeContent, limit,
		)
	} else if after != "" {
		rows, err = s.db.Query(
			`SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at FROM messages
			 WHERE claw_id = ? AND tenant_id = ? AND created_at > ?
			 AND NOT (role = 'system' AND content IN (?, ?, ?))
			 ORDER BY created_at ASC LIMIT ?`,
			clawID, tenantID, after, wakeMessageMarker, defaultWakeContent, factoryWakeContent, limit,
		)
	} else {
		// Default: last N messages
		rows, err = s.db.Query(
			`SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at FROM messages
			 WHERE claw_id = ? AND tenant_id = ?
			 AND NOT (role = 'system' AND content IN (?, ?, ?))
			 ORDER BY created_at DESC LIMIT ?`,
			clawID, tenantID, wakeMessageMarker, defaultWakeContent, factoryWakeContent, limit,
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
		if err := rows.Scan(&m.ID, &m.ClawID, &m.TenantID, &m.Role, &m.Content, &m.Format, &m.CreatedAt); err != nil {
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

	// Check if this is a status channel registration BEFORE any DB upsert.
	// Status channels must not mutate claw DB state (rp.GatewayReady is nil,
	// so initialStatus would incorrectly overwrite 'starting'/'bootstrap_needed').
	isStatusChannel := rp.Channel == "status"

	var bootstrapOK int
	var provider string
	var currentStatus string
	_ = s.db.QueryRow(`SELECT COALESCE(bootstrap_ok,0), COALESCE(provider,'') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&bootstrapOK, &provider)

	if !isStatusChannel {
		// Upsert claw and keep terminal/watching states sticky across reconnects.
		desiredStatus := initialStatus(rp.GatewayReady)
		if provider == "daytona" && bootstrapOK != 1 {
			desiredStatus = "starting"
		}
		currentStatus = desiredStatus
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
	} else {
		// For status channel, just read current status from DB
		_ = s.db.QueryRow(`SELECT status FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&currentStatus)
	}

	var registrationTagsJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&registrationTagsJSON)
	allowWake := bootstrapOK == 1 || provider != "daytona"
	var registrationTags []string
	_ = json.Unmarshal([]byte(registrationTagsJSON), &registrationTags)

	if isStatusChannel {
		// Status channel connects to existing claw
		s.mu.Lock()
		if existing, ok := s.claws[clawID]; ok {
			existing.mu.Lock()
			existing.statusConn = conn
			existing.mu.Unlock()
			s.mu.Unlock()
			log.Printf("[bridge] ✓ status channel connected: %s (%s)", rp.Name, clawID[:8])
			_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "registered", Payload: map[string]string{"claw_id": clawID, "channel": "status"}})
			// Simple read loop for status channel — just keepalive
			for {
				var msg types.WSMessage
				if err := wsjson.Read(ctx, conn, &msg); err != nil {
					s.mu.Lock()
					if existing2, ok2 := s.claws[clawID]; ok2 {
						existing2.mu.Lock()
						existing2.statusConn = nil
						existing2.mu.Unlock()
					}
					s.mu.Unlock()
					return
				}
				if msg.Type == "status_pong" {
					s.mu.Lock()
					if existing2, ok2 := s.claws[clawID]; ok2 {
						existing2.mu.Lock()
						existing2.lastStatusAt = time.Now()
						existing2.mu.Unlock()
					}
					s.mu.Unlock()
				}
			}
		}
		s.mu.Unlock()
		conn.Close(websocket.StatusPolicyViolation, "main channel not connected")
		return
	}

	cc := &clawConn{id: clawID, tenantID: tenantID, conn: conn, gatewayReady: gatewayReadyBool(rp.GatewayReady), tags: registrationTags, lastUserMessageAt: time.Now(), lastStatusAt: time.Now()}
	s.mu.Lock()
	if old, ok := s.claws[clawID]; ok {
		old.mu.RLock()
		cc.statusConn = old.statusConn
		cc.lastStatusAt = old.lastStatusAt
		// Copy message queue from old connection to preserve queued messages
		if len(old.messageQueue) > 0 {
			cc.messageQueue = make([]types.HubMessage, len(old.messageQueue))
			copy(cc.messageQueue, old.messageQueue)
		}
		old.mu.RUnlock()
	}
	// Capture whether we have queued messages before unlocking
	hasQueuedMessages := len(cc.messageQueue) > 0
	s.claws[clawID] = cc
	s.mu.Unlock()

	log.Printf("[bridge] ✓ connected: %s (%s) gateway_ready=%v", rp.Name, clawID[:8], cc.gatewayReady)

	// Ack
	_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "registered", Payload: map[string]string{"claw_id": clawID}})

	// Broadcast initial status to user sessions
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": currentStatus}})

	// Drain any queued messages that were copied from the old connection.
	// This must happen after the connection is live but before the read loop starts.
	// We call it synchronously (not in a goroutine) to avoid racing with new user messages.
	if hasQueuedMessages {
		s.sendNextQueuedMessage(cc)
	}

	// Initialize entry pipeline stage only after bridge connects so on_enter inject
	// can be delivered over WS.
	usedPipelineEntryInject := false
	if allowWake && cc.gatewayReady && currentStatus == "connected" {
		usedPipelineEntryInject = s.initializePipelineEntryIfNeeded(clawID)
	}
	// If no pipeline entry inject was sent, fire the default wake message.
	// But don't re-wake claws that already have a pipeline stage (hub restart reconnect).
	if allowWake && cc.gatewayReady && currentStatus == "connected" && !usedPipelineEntryInject {
		if s.getPipelineStage(clawID) == "" && !s.clawHasMessages(clawID) {
			go s.sendWakeMessage(cc, clawID)
		}
	}
	if allowWake && cc.gatewayReady && currentStatus == "connected" && !s.hasRecentCheckpoint(clawID, time.Hour) {
		go s.requestBootstrapCheckpoint(clawID)
	}

	// Read loop — claw sends messages back to users
	defer func() {
		s.mu.Lock()
		var partialContent string
		var partialMsgID string
		// Flush any partial streaming buffer as an interrupted message
		if partialCC, ok := s.claws[clawID]; ok && partialCC.streamingBuf.Len() > 0 {
			partialContent = partialCC.streamingBuf.String() + " [interrupted]"
			partialMsgID = partialCC.streamingMsgID
			if partialMsgID == "" {
				partialMsgID = uuid.New().String()
			}
			partialCC.streamingBuf.Reset()
			partialCC.streamingMsgID = ""
		}
		delete(s.claws, clawID)
		s.mu.Unlock()
		if partialContent != "" {
			interruptedAt := now()
			_, _ = s.db.Exec(
				`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)
				 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
				partialMsgID, clawID, tenantID, "claw", partialContent, interruptedAt,
			)
			s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: types.HubMessage{
				ID: partialMsgID, ClawID: clawID, TenantID: tenantID, Role: "claw",
				Content: partialContent, CreatedAt: interruptedAt,
			}})
		}
		// Clear typing indicator so the UI doesn't show a stuck "typing" state
		// if the claw disconnects mid-response.
		s.broadcastToUsers(tenantID, types.WSMessage{
			Type: "agent_typing",
			Payload: map[string]string{
				"claw_id": clawID,
				"status":  "idle",
			},
		})
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
					var shouldWarnContext bool
					var prevUsage int
					s.mu.Lock()
					if cc, ok := s.claws[clawID]; ok {
						cc.mu.Lock()
						// Log only on status changes, not every heartbeat
						prevUsage = cc.contextUsage
						cc.contextUsage = hb.ContextUsage
						// Promote from 'starting' to 'connected' once gateway is ready.
						// nil means field absent (old bridge) — treat as ready.
						if gatewayReadyBool(hb.GatewayReady) && !cc.gatewayReady {
							cc.gatewayReady = true
							res, execErr := s.db.Exec(`UPDATE claws SET status='connected', bootstrap_status='' WHERE id=? AND status='starting' AND bootstrap_ok=1`, clawID)
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
								go s.requestBootstrapCheckpoint(clawID)
							}
						} else if !hb.GatewayHealthy {
							cc.gatewayUnhealthyCount++
							if cc.gatewayUnhealthyCount == 1 {
								log.Printf("[heartbeat] %s (%s): gateway unhealthy", rp.Name, clawID[:8])
							} else if cc.gatewayUnhealthyCount%4 == 0 {
								log.Printf("[heartbeat] %s (%s): gateway unhealthy for %d consecutive checks", rp.Name, clawID[:8], cc.gatewayUnhealthyCount)
							}
							if cc.gatewayUnhealthyCount == 4 && !cc.streamingStartedAt.IsZero() {
								go s.injectHubMessageByID(clawID, "[hub] The gateway has been unresponsive for about a minute. If you're stuck in a long operation, consider sending [DONE] and starting fresh.")
							}
						}
						// Log context usage on every heartbeat when it crosses the 80% threshold,
						// regardless of gateway health — don't silence diagnostics during outages.
						if hb.ContextUsage != prevUsage && (hb.ContextUsage >= 80 || prevUsage >= 80) {
							log.Printf("[heartbeat] %s (%s): context_usage=%d%%", rp.Name, clawID[:8], hb.ContextUsage)
						}
						if hb.GatewayHealthy && cc.gatewayUnhealthyCount > 0 {
							log.Printf("[heartbeat] %s (%s): gateway recovered after %d unhealthy checks", rp.Name, clawID[:8], cc.gatewayUnhealthyCount)
							cc.gatewayUnhealthyCount = 0
						}
						// Inject context warning once per streaming turn when usage is >=95%
						if !cc.streamingStartedAt.IsZero() &&
							hb.ContextUsage >= 95 &&
							!cc.contextWarningSent {
							cc.contextWarningSent = true
							shouldWarnContext = true
						}
						cc.mu.Unlock()
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
						if !s.initializePipelineEntryIfNeeded(clawID) && s.getPipelineStage(clawID) == "" && !s.clawHasMessages(clawID) {
							go s.sendWakeMessage(wakeConn, clawID)
						}
					}
					// Check for streaming turn timeout (12 minutes)
					s.mu.RLock()
					cc, ok := s.claws[clawID]
					s.mu.RUnlock()
					if ok {
						cc.mu.Lock()
						if !cc.streamingStartedAt.IsZero() &&
							!cc.streamingTimeoutSent &&
							time.Since(cc.streamingStartedAt) > 12*time.Minute {
							cc.streamingTimeoutSent = true
							cc.mu.Unlock()
							go s.injectHubMessage(ctx, cc, "[hub] Your current response has been running for over 12 minutes. Please wrap up and send your response.")
						} else {
							cc.mu.Unlock()
						}
					}
				}
			} else if msg.Type == "agent_activity" {
				payload, _ := json.Marshal(msg.Payload)
				var activity map[string]interface{}
				if err := json.Unmarshal(payload, &activity); err == nil {
					createdAt := now()
					activity["claw_id"] = clawID
					activity["created_at"] = createdAt.Format(time.RFC3339Nano)
					content := activityContent(activity)
					if content != "" && !isUnhelpfulActivityContent(activity, content) {
						format := "activity:" + string(payload)
						_, _ = s.db.Exec(
							`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
							uuid.New().String(), clawID, tenantID, "activity", content, format, createdAt,
						)
					}
					s.broadcastToUsers(tenantID, types.WSMessage{
						Type:    "agent_activity",
						Payload: activity,
					})
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
					s.mu.RLock()
					cc, ok := s.claws[clawID]
					s.mu.RUnlock()
					if ok {
						cc.mu.Lock()
						if cc.streamingMsgID == "" {
							cc.streamingMsgID = uuid.New().String()
							cc.streamingStartedAt = time.Now()
							cc.streamingTimeoutSent = false
							cc.contextWarningSent = false
						}
						cc.streamingBuf.WriteString(chunk.Content)
						msgID := cc.streamingMsgID
						bufContent := cc.streamingBuf.String()
						cc.mu.Unlock()
						// Upsert — insert on first chunk, update content on subsequent
						_, _ = s.db.Exec(
							`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)
							 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
							msgID, clawID, tenantID, "claw", bufContent, now(),
						)
					}
				}
			} else if msg.Type == "message" {
				// Complete message — finalize the buffered stream or store fresh
				payload, _ := json.Marshal(msg.Payload)
				var hm types.HubMessage
				if err := json.Unmarshal(payload, &hm); err != nil {
					continue
				}
				hm.ClawID = clawID
				hm.TenantID = tenantID
				hm.Role = "claw"
				hm.CreatedAt = now()
				// Always clean up streaming state first, even for empty messages.
				// Use the outer cc (this goroutine's connection), not a fresh lookup.
				// If the claw reconnected, a new handleClawWS goroutine handles the new cc.
				cc.mu.Lock()
				if cc.streamingMsgID != "" {
					hm.ID = cc.streamingMsgID
					cc.streamingMsgID = ""
					cc.streamingBuf.Reset()
					cc.streamingStartedAt = time.Time{}
					cc.streamingTimeoutSent = false
					cc.contextWarningSent = false
				} else {
					hm.ID = uuid.New().String()
				}
				cc.mu.Unlock()
				// Drop empty messages — never store or broadcast
				if strings.TrimSpace(hm.Content) == "" {
					// Clear typing indicator first — always clear even if no queued messages
					s.broadcastToUsers(tenantID, types.WSMessage{
						Type: "agent_typing",
						Payload: map[string]string{
							"claw_id": clawID,
							"status":  "idle",
						},
					})
					// Drain queue using this goroutine's cc (the outer cc from line 1449).
					// If the claw reconnected, a new handleClawWS goroutine handles the new cc.
					s.sendNextQueuedMessage(cc)
					s.drainPendingCheckpoint(clawID)
					continue
				}
				_, _ = s.db.Exec(
					`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)
					 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
					hm.ID, hm.ClawID, hm.TenantID, hm.Role, hm.Content, hm.CreatedAt,
				)
				s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: hm})
				// Clear typing indicator now that response is complete
				s.broadcastToUsers(tenantID, types.WSMessage{
					Type: "agent_typing",
					Payload: map[string]string{
						"claw_id": clawID,
						"status":  "idle",
					},
				})
				// Check for [DONE] signal from a factory-created claw
				if strings.Contains(hm.Content, "[DONE]") {
					go func() {
						if _, err := s.requestCheckpoint(context.Background(), clawID, "done", "hub", false, checkpointRequestTimeout); err != nil {
							log.Printf("[checkpoint] done request for %s failed: %v", shortID(clawID), err)
						}
					}()
					go s.handleClawDoneSignal(clawID, hm.Content)
				}
				// Check for [TERMINATE] signal - allows claw to manage its own lifecycle
				if strings.Contains(hm.Content, "[TERMINATE]") {
					go s.handleClawTerminateSignal(clawID, hm.Content)
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
				// Check for queued messages and send the next one.
				// Use this goroutine's cc (the outer cc from line 1449).
				// If the claw reconnected, a new handleClawWS goroutine handles the new cc.
				s.sendNextQueuedMessage(cc)
				s.drainPendingCheckpoint(clawID)
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
						log.Printf("[hub-proxy] bad req payload: %v", err)
						return
					}
					log.Printf("[hub-proxy] req req_id=%s %s %s?%s", req.ReqID, req.Method, req.Path, req.Query)
					// Build an internal HTTP request
					urls := req.Path
					if req.Query != "" {
						urls += "?" + req.Query
					}
					httpReq, err := http.NewRequest(req.Method, "http://localhost"+urls, strings.NewReader(req.Body))
					if err != nil {
						log.Printf("[hub-proxy] build request failed req_id=%s err=%v", req.ReqID, err)
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
					if w.status == 0 {
						w.status = 200
					}
					log.Printf("[hub-proxy] res req_id=%s status=%d body_len=%d", req.ReqID, w.status, len(w.body))
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
		id, name, status, tagsJSON, bootstrapStatus string
	}
	var dbClaws []dbClaw
	rows, _ := s.db.QueryContext(ctx, `SELECT id, name, status, COALESCE(tags,'[]'), COALESCE(bootstrap_status,'') FROM claws WHERE tenant_id=? AND status NOT IN ('offline')`, tenantID)
	if rows != nil {
		for rows.Next() {
			var c dbClaw
			_ = rows.Scan(&c.id, &c.name, &c.status, &c.tagsJSON, &c.bootstrapStatus)
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
		// Apply tag-based view filter for GitHub OAuth users
		if ghLogin != "" && !canViewClaw(accessCfg, ghLogin, cc.tags) {
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
				"claw_id":          c.id,
				"name":             c.name,
				"status":           c.status, // provisioning / starting / error
				"bootstrap_status": c.bootstrapStatus,
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
	for _, uc := range s.broadcastRecipients(tenantID, msg) {
		_ = wsjson.Write(context.Background(), uc.conn, msg)
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
	if cc := s.claws[clawID]; cc != nil && cc.tenantID == tenantID {
		tags := append([]string(nil), cc.tags...)
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

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Provision creates or updates the default tenant (for alpha single-user setup).
// If a tenant named "default" already exists, its token and claw_token are updated
// so that hub.yaml token changes take effect on restart without manual DB surgery.
func (s *Server) Provision(token, clawToken string) (string, error) {
	var existingID string
	_ = s.db.QueryRow(`SELECT id FROM tenants WHERE name = 'default'`).Scan(&existingID)
	if existingID != "" {
		_, err := s.db.Exec(
			`UPDATE tenants SET token = ?, claw_token = ? WHERE id = ?`,
			token, clawToken, existingID,
		)
		if err != nil {
			return "", fmt.Errorf("provision update: %w", err)
		}
		return existingID, nil
	}
	id := uuid.New().String()
	_, err := s.db.Exec(
		`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		id, "default", token, clawToken, now(),
	)
	if err != nil {
		return "", fmt.Errorf("provision: %w", err)
	}
	return id, nil
}

// ─── Provisioning ─────────────────────────────────────────────────────────────

func (s *Server) provisionDaytona(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		return fmt.Errorf("daytona init: %w", err)
	}
	s.setBootstrapStatus(clawID, "Creating sandbox")
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
	recordE2EDaytonaSandboxID(instance.ID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='daytona', provider_id=? WHERE id=?`, instance.ID, clawID)

	// Bootstrap: install OpenClaw + claw-bridge via exec (retry up to 3x for transient Daytona API timeouts)
	clawName := req.Name
	go func() {
		// Each step inside bootstrapDaytona retries 3x internally.
		// Outer retries here handle the rare case of total step failure.
		const maxBootstrapAttempts = 3
		var lastErr error
		for attempt := 1; attempt <= maxBootstrapAttempts; attempt++ {
			if attempt > 1 {
				log.Printf("[daytona] full bootstrap retry for claw %s in 15s...", clawName)
				time.Sleep(15 * time.Second)
			}
			lastErr = s.bootstrapDaytona(context.Background(), clawID, clawName, instance.ID, p, env)
			if lastErr == nil {
				return
			}
			if s.daytonaBridgeRunning(context.Background(), instance.ID, p) {
				log.Printf("[daytona] bootstrap attempt %d/%d for claw %s returned error after claw-bridge started; treating bootstrap as complete: %v", attempt, maxBootstrapAttempts, clawName, lastErr)
				return
			}
			log.Printf("[daytona] bootstrap attempt %d/%d failed for claw %s: %v", attempt, maxBootstrapAttempts, clawName, lastErr)
		}
		log.Printf("[daytona] bootstrap failed for claw %s: %v", clawName, lastErr)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Daytona bootstrap failed: %v", lastErr), false)
		// stopAgentWithReason already terminates the VM; no need to destroy again
	}()
	return nil
}

func (s *Server) bootstrapDaytona(ctx context.Context, clawID, clawName, instanceID string, p *daytona.Provider, env map[string]string) error {
	log.Printf("[daytona] bootstrapping claw %s (instance %s)", clawID, instanceID)
	s.setBootstrapStatus(clawID, "Preparing runtime")

	exec := func(label string, timeout time.Duration, cmd string) error {
		s.setBootstrapStatus(clawID, daytonaBootstrapStatusForStep(label))
		const maxAttempts = 3
		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if attempt == 1 {
				log.Printf("[daytona] %s...", label)
			} else {
				log.Printf("[daytona] %s retry %d/%d...", label, attempt, maxAttempts)
				time.Sleep(5 * time.Second)
			}
			// Prefix HOME so commands run in the sandbox user's home, not the caller's.
			// Also source nvm and pin Node 24 LTS — Daytona snapshots may ship with
			// non-LTS Node (e.g. v25) and each exec is a fresh shell session.
			// If nvm use 24 fails (not installed yet), we install it on the fly.
			nvmSetup := `export HOME=/home/daytona; export NVM_DIR=/usr/local/share/nvm; [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh" && { nvm use 24 >/dev/null 2>&1 || nvm install 24 >/dev/null 2>&1; } ; `
			result, err := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", nvmSetup + cmd}, timeout)
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", label, err)
				continue
			}
			if result.ExitCode != 0 {
				lastErr = fmt.Errorf("%s failed (exit %d): %s", label, result.ExitCode, sanitizeBootstrapOutput(result.Stdout))
				continue
			}
			log.Printf("[daytona] %s done", label)
			return nil
		}
		return lastErr
	}

	// Step 1: Install pinned OpenClaw version.
	// Run install in background and poll — avoids the 60s HTTP client timeout
	// that kills synchronous long-running commands.
	// Uninstall old openclaw then reinstall pinned version (ensures nvm current symlink is updated)
	if err := exec("uninstall old openclaw", 20*time.Second,
		`NPM="/usr/local/share/nvm/current/bin/npm"; \
PREFIX="$("$NPM" config get prefix)"; \
echo "npm=$NPM prefix=$PREFIX"; \
sudo "$NPM" uninstall -g openclaw --prefix "$PREFIX" 2>&1 || true; \
hash -r; \
echo uninstalled`); err != nil {
		log.Printf("[daytona] warning: uninstall failed (ok if not installed): %v", err)
	}

	const daytonaOpenClawVersion = "2026.5.20"
	if err := exec("install openclaw", 3*time.Minute,
		fmt.Sprintf(`export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; \
PREFIX="$(/usr/local/share/nvm/current/bin/npm config get prefix)"; \
echo "npm=$NVM_DIR/current/bin/npm prefix=$PREFIX"; \
sudo env PATH="$NVM_DIR/current/bin:$PATH" npm install -g openclaw@%s --prefix "$PREFIX" --ignore-scripts 2>&1; \
hash -r; \
echo 'install done'`, daytonaOpenClawVersion)); err != nil {
		return err
	}

	if err := exec("verify openclaw", 20*time.Second,
		fmt.Sprintf(`export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; \
hash -r; \
OPENCLAW_PATH="$(command -v openclaw || true)"; \
OPENCLAW_VERSION="$(openclaw --version 2>&1 || true)"; \
PACKAGE_VERSION="$(node -e "try{console.log(require('$NVM_DIR/current/lib/node_modules/openclaw/package.json').version)}catch(e){process.exit(0)}" 2>/dev/null || true)"; \
echo "openclaw path=$OPENCLAW_PATH"; \
echo "openclaw version=$OPENCLAW_VERSION"; \
echo "openclaw package_version=$PACKAGE_VERSION"; \
case "$OPENCLAW_VERSION" in *%s*) ;; *) echo "expected openclaw %s"; exit 1 ;; esac`, daytonaOpenClawVersion, daytonaOpenClawVersion)); err != nil {
		return err
	}

	// Step 1b: Install Nix (Determinate Systems) if requested.
	var nixEnabled int
	_ = s.db.QueryRow(`SELECT nix FROM claws WHERE id=?`, clawID).Scan(&nixEnabled)
	if nixEnabled == 1 {
		if err := exec("install nix", 3*time.Minute,
			`export HOME=/home/daytona; \
curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix | \
  sh -s -- install linux --no-confirm --init none >> /tmp/nix-install.log 2>&1; \
. /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true; \
nix --version`); err != nil {
			log.Printf("[daytona] warning: nix install failed: %v", err)
		}
	}

	// Step 1c: Install Docker Engine if requested.
	var dockerEnabled int
	_ = s.db.QueryRow(`SELECT docker FROM claws WHERE id=?`, clawID).Scan(&dockerEnabled)
	if dockerEnabled == 1 {
		if err := exec("install docker", 3*time.Minute,
			`export HOME=/home/daytona; \
. /etc/os-release; \
if [ "$ID" = "debian" ] && [ -n "$VERSION_CODENAME" ]; then \
  DOCKER_REPO="deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian $VERSION_CODENAME stable"; \
  DOCKER_GPG="https://download.docker.com/linux/debian/gpg"; \
else \
  DOCKER_REPO="deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable"; \
  DOCKER_GPG="https://download.docker.com/linux/ubuntu/gpg"; \
fi; \
sudo apt-get update -qq && \
sudo apt-get install -y --fix-broken ca-certificates curl gnupg && \
sudo install -m 0755 -d /etc/apt/keyrings && \
curl -fsSL "$DOCKER_GPG" | sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/docker.gpg && \
sudo chmod a+r /etc/apt/keyrings/docker.gpg && \
echo "$DOCKER_REPO" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null && \
sudo apt-get update -qq && \
sudo apt-get install -y --fix-broken docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin && \
sudo usermod -aG docker daytona 2>/dev/null || true && \
docker --version`); err != nil {
			log.Printf("[daytona] warning: docker install failed: %v", err)
		}
	}

	// Step 2: Onboard (configure OpenClaw) with the correct auth provider
	s.setBootstrapStatus(clawID, "Configuring OpenClaw")
	var llmKeyNameDaytona string
	_ = s.db.QueryRow(`SELECT COALESCE(llm_key,'') FROM claws WHERE id=?`, clawID).Scan(&llmKeyNameDaytona)
	activeKeyNameDaytona := ""
	activeKeyProviderDaytona := ""
	s.mu.RLock()
	activeKeyDaytona := resolveActiveKey(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	defaultModelDaytona := resolveDefaultModelForKey(s.hubCfg, activeKeyDaytona)
	llmKeyEnvDaytona := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	onboardFlags := buildOnboardFlags(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	providerConfigScript := buildOpenClawProviderConfig(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	if activeKeyDaytona != nil {
		activeKeyNameDaytona = activeKeyDaytona.Name
		activeKeyProviderDaytona = activeKeyDaytona.Provider
	}
	s.mu.RUnlock()
	log.Printf("[daytona] OpenClaw model resolution claw=%s selected_llm_key=%q active_llm_key=%q provider=%q default_model=%q config_patch=%t",
		clawID, llmKeyNameDaytona, activeKeyNameDaytona, activeKeyProviderDaytona, defaultModelDaytona, providerConfigScript != "")
	gatewayPassword := randomHex(16)
	onboardCmd := fmt.Sprintf(
		"%sexport NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; openclaw onboard --non-interactive --accept-risk --skip-daemon --skip-health %s 2>&1",
		llmKeyEnvDaytona,
		onboardFlags,
	)
	log.Printf("[daytona] onboard openclaw...")
	onboardResult, onboardErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", "export HOME=/home/daytona; " + onboardCmd}, 2*time.Minute)
	if onboardErr != nil {
		result, diagErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", `export HOME=/home/daytona; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`}, 10*time.Second)
		if diagErr != nil || strings.TrimSpace(result.Stdout) != "exists" {
			return fmt.Errorf("onboard openclaw: %w", onboardErr)
		}
		log.Printf("[daytona] onboard returned error, but config file exists; continuing")
	} else if onboardResult.ExitCode != 0 {
		result, diagErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", `export HOME=/home/daytona; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`}, 10*time.Second)
		if diagErr != nil || strings.TrimSpace(result.Stdout) != "exists" {
			return fmt.Errorf("onboard openclaw failed (exit %d): %s", onboardResult.ExitCode, onboardResult.Stdout)
		}
		log.Printf("[daytona] onboard returned non-zero, but config file exists; continuing")
	} else {
		log.Printf("[daytona] onboard openclaw done")
	}

	configPatch := fmt.Sprintf("export HOME=/home/daytona; export OPENCLAW_DEFAULT_MODEL=%q; export ELASTICCLAW_GATEWAY_PASSWORD=%q; ", defaultModelDaytona, gatewayPassword) + llmKeyEnvDaytona + providerConfigScript
	if err := exec("configure openclaw model", 30*time.Second, configPatch); err != nil {
		return err
	}
	// Step 2a: Preflight required commands and environment.
	// Fail early if the sandbox is missing tools that OpenClaw or agents need.
	if err := exec("preflight required commands", 30*time.Second,
		`export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; \
for cmd in node npm git python3 curl; do command -v "$cmd" >/dev/null || { echo "missing: $cmd"; exit 1; }; done; \
echo "preflight ok"`); err != nil {
		return fmt.Errorf("daytona sandbox missing required commands: %w", err)
	}
	// Step 2b: Pre-stage plugin runtime dependencies before starting gateway.
	// This prevents the gateway from doing expensive npm installs while
	// clients are connected, which causes event-loop delays and connection drops.
	if err := exec("stage openclaw plugin deps", 3*time.Minute,
		`export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; \
export OPENCLAW_EAGER_BUNDLED_PLUGIN_DEPS=1; \
openclaw plugins deps --repair 2>&1 || echo "plugin deps staging completed with warnings"`); err != nil {
		log.Printf("[daytona] warning: plugin deps staging failed: %v", err)
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
export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; setsid nohup openclaw gateway run >> ~/.openclaw/gateway.log 2>&1 </dev/null &
# Phase 1: wait for HTTP server to be listening (quick)
for i in $(seq 1 30); do
  curl -sf http://localhost:18789/healthz >/dev/null && echo 'gateway listening' && break
  sleep 1
done
curl -sf http://localhost:18789/healthz >/dev/null || { echo 'gateway failed to listen'; tail -n 100 ~/.openclaw/gateway.log 2>/dev/null || true; exit 1; }
# Phase 2: wait for gateway startup to complete. Do not use openclaw health
# here: it pairs the CLI device with read-only scopes before claw-bridge can
# connect, then claw-bridge is rejected as a scope-upgrade.
for i in $(seq 1 30); do
  if grep -q 'gateway ready' ~/.openclaw/gateway.log 2>/dev/null; then
    echo "gateway ready"
    exit 0
  fi
  curl -sf http://localhost:18789/healthz >/dev/null || break
  sleep 1
done
# Fallback: if the readiness log line is unavailable but the gateway is still
# listening and healthy, don't fail the bootstrap.
if curl -sf http://localhost:18789/healthz >/dev/null; then
  echo "gateway ready (healthz)"
  exit 0
fi
echo 'gateway not ready'
tail -n 100 ~/.openclaw/gateway.log 2>/dev/null || true
exit 1`
	if err := exec("start openclaw gateway", 2*time.Minute, gatewaySetup); err != nil {
		return err
	}

	// Step 3: Download claw-bridge now, but do not start it until the workspace,
	// template files, and bootstrap gating are fully ready.
	s.setBootstrapStatus(clawID, "Preparing workspace")
	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		return fmt.Errorf("claw-bridge URL not configured: set bridge_image in hub.yaml (e.g. bridge_image: ttl.sh/your/claw-bridge:tag) or build a tagged release")
	}
	var downloadCmd string
	if strings.HasPrefix(bridgeURL, "http://") || strings.HasPrefix(bridgeURL, "https://") {
		downloadCmd = fmt.Sprintf(`rm -f /tmp/claw-bridge.download && curl -fsSL %q -o /tmp/claw-bridge.download && chmod +x /tmp/claw-bridge.download && mv -f /tmp/claw-bridge.download /tmp/claw-bridge && echo downloaded`, bridgeURL)
	} else {
		// OCI ref (ttl.sh or ghcr) — use oras
		downloadCmd = fmt.Sprintf(`
if ! command -v oras &>/dev/null; then
  curl -sL https://github.com/oras-project/oras/releases/download/v1.2.2/oras_1.2.2_linux_amd64.tar.gz | tar xz -C /tmp && sudo mv /tmp/oras /usr/local/bin/oras
fi
mkdir -p /tmp/bridge-dl && cd /tmp/bridge-dl && oras pull %q
BIN=$(find /tmp/bridge-dl -name 'claw-bridge*' -type f | head -1)
cp "$BIN" /tmp/claw-bridge.download && chmod +x /tmp/claw-bridge.download && mv -f /tmp/claw-bridge.download /tmp/claw-bridge && echo downloaded`, bridgeURL)
	}
	if err := s.downloadDaytonaConnector(ctx, clawID, instanceID, p, downloadCmd); err != nil {
		return err
	}

	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()

	// Write template files (SOUL.md, AGENTS.md, etc.) to the workspace before
	// the bridge starts so BOOTSTRAP.md and friends are present for the first turn.
	s.setBootstrapStatus(clawID, "Preparing workspace")
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
	var workspaceName string
	var repositories []types.GitHubRepoAccess
	var repositoriesJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(template,''), COALESCE(github_repos,'[]') FROM claws WHERE id=?`, clawID).Scan(&workspaceName, &repositoriesJSON)
	_ = json.Unmarshal([]byte(repositoriesJSON), &repositories)
	s.mu.RLock()
	hasHubGitHubApps := len(s.hubCfg.GitHubApps) > 0
	s.mu.RUnlock()
	hasWorkspaceGitHubApps := false
	if workspaceName != "" {
		if workspaceApps, err := loadWorkspaceGitHubAppConfigs(workspaceName); err == nil && len(workspaceApps) > 0 {
			hasWorkspaceGitHubApps = true
		}
	}
	hasGitHubApps := hasHubGitHubApps || hasWorkspaceGitHubApps
	if hasGitHubApps {
		s.setBootstrapStatus(clawID, "Preparing repository access")
		// Use the hub directly during bootstrap. The bridge is intentionally not
		// started yet so startup cannot race ahead of template file writes and
		// bootstrap_ok gating.
		tokenURL := fmt.Sprintf("%s/api/github/token/%s?claw_token=%s", s.clawHubURL(), clawID, clawToken)

		// Step 5a: write the credential helper binary
		credHelperScript := fmt.Sprintf(`export HOME=/home/daytona
sudo tee /usr/local/bin/elasticclaw-git-credentials > /dev/null << 'CREDEOF'
#!/bin/bash
# Retry up to 10 times — hub token endpoint may not be ready immediately
for i in $(seq 1 10); do
  response=$(curl -sf --max-time 35 %q)
  if [ $? -eq 0 ] && [ -n "$response" ]; then break; fi
  sleep 3
done
if [ -z "$response" ]; then exit 1; fi
token=$(echo "$response" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "protocol=https"
echo "host=github.com"
echo "username=x-access-token"
echo "password=$token"
CREDEOF
sudo chmod +x /usr/local/bin/elasticclaw-git-credentials
git config --global credential.helper /usr/local/bin/elasticclaw-git-credentials
echo 'credential helper installed'`, tokenURL)
		if err := exec("install git credential helper", 20*time.Second, credHelperScript); err != nil {
			return fmt.Errorf("install git credential helper: %w", err)
		} else {
			installGhScript := `export HOME=/home/daytona
if command -v gh >/dev/null 2>&1; then
  gh --version >/dev/null 2>&1
  exit 0
fi
if command -v apt-get >/dev/null 2>&1; then
  sudo apt-get update -qq && sudo apt-get install -y gh
elif command -v dnf >/dev/null 2>&1; then
  sudo dnf install -y gh
elif command -v yum >/dev/null 2>&1; then
  sudo yum install -y gh
else
  echo 'unsupported package manager for gh install'
  exit 1
fi
command -v gh >/dev/null 2>&1 && gh --version >/dev/null 2>&1`
			if err := exec("install gh cli", 2*time.Minute, installGhScript); err != nil {
				return fmt.Errorf("install gh cli: %w", err)
			}

			fetchGitHubTokenJSON := fmt.Sprintf(`export HOME=/home/daytona
rm -f /tmp/elasticclaw-github-token.json
curl -sf --max-time 35 %q -o /tmp/elasticclaw-github-token.json
status=$?
echo "curl_exit=$status"
ls -l /tmp/elasticclaw-github-token.json 2>&1 || true
[ $status -eq 0 ] || exit $status
[ -s /tmp/elasticclaw-github-token.json ] || exit 1
`, tokenURL)
			if err := exec("fetch github bootstrap token json", 45*time.Second, fetchGitHubTokenJSON); err != nil {
				return fmt.Errorf("fetch github bootstrap token json: %w", err)
			}

			parseGitHubToken := `export HOME=/home/daytona
python3 - <<'PYEOF' > /tmp/elasticclaw-github-token.txt
import json
with open('/tmp/elasticclaw-github-token.json') as f:
    data = json.load(f)
print(data.get('token', ''))
PYEOF
status=$?
echo "python_exit=$status"
ls -l /tmp/elasticclaw-github-token.txt 2>&1 || true
[ $status -eq 0 ] || exit $status
[ -s /tmp/elasticclaw-github-token.txt ] || exit 1`
			if err := exec("parse github bootstrap token", 20*time.Second, parseGitHubToken); err != nil {
				return fmt.Errorf("parse github bootstrap token: %w", err)
			}

			writeGitHubTokenEnv := `export HOME=/home/daytona
TOKEN=$(cat /tmp/elasticclaw-github-token.txt)
[ -n "$TOKEN" ] || exit 1
printf 'export GH_TOKEN=%s\n' "$TOKEN" | sudo tee /etc/profile.d/elasticclaw-github.sh > /dev/null
sudo chmod +x /etc/profile.d/elasticclaw-github.sh
[ -s /etc/profile.d/elasticclaw-github.sh ] || exit 1`
			if err := exec("write github token env", 20*time.Second, writeGitHubTokenEnv); err != nil {
				return fmt.Errorf("write github token env: %w", err)
			}

			ghAuthScript := `export HOME=/home/daytona
set +x
. /etc/profile.d/elasticclaw-github.sh
command -v gh
[ -n "$GH_TOKEN" ]
gh --version
TOKEN="$(cat /tmp/elasticclaw-github-token.txt)"
[ -n "$TOKEN" ]
unset GH_TOKEN
gh auth logout -h github.com || true
printf '%s\n' "$TOKEN" | gh auth login --hostname github.com --with-token
export GH_TOKEN="$TOKEN"`
			log.Printf("[daytona] auth gh cli (no retries)...")
			ghAuthResult, ghAuthErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", ghAuthScript}, 30*time.Second)
			if ghAuthErr != nil {
				return fmt.Errorf("auth gh cli: %w", ghAuthErr)
			}
			if ghAuthResult.ExitCode != 0 {
				return fmt.Errorf("auth gh cli failed (exit %d): %s", ghAuthResult.ExitCode, sanitizeBootstrapOutput(ghAuthResult.Stdout))
			}
			log.Printf("[daytona] auth gh cli done")

			ghStatusScript := `export HOME=/home/daytona
set +x
. /etc/profile.d/elasticclaw-github.sh
gh auth status`
			log.Printf("[daytona] verify gh auth (no retries)...")
			ghStatusResult, ghStatusErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", ghStatusScript}, 20*time.Second)
			if ghStatusErr != nil {
				return fmt.Errorf("verify gh auth: %w", ghStatusErr)
			}
			if ghStatusResult.ExitCode != 0 {
				return fmt.Errorf("verify gh auth failed (exit %d): %s", ghStatusResult.ExitCode, sanitizeBootstrapOutput(ghStatusResult.Stdout))
			}
			if len(repositories) > 0 {
				verifyReposScript := "export HOME=/home/daytona; set +x; . /etc/profile.d/elasticclaw-github.sh; "
				for _, repo := range repositories {
					verifyReposScript += fmt.Sprintf("gh repo view %s >/dev/null || exit 1; ", repo.Repo)
				}
				log.Printf("[daytona] verify configured repositories (no retries)...")
				verifyReposResult, verifyReposErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", verifyReposScript}, 30*time.Second)
				if verifyReposErr != nil {
					return fmt.Errorf("verify configured repositories: %w", verifyReposErr)
				}
				if verifyReposResult.ExitCode != 0 {
					return fmt.Errorf("verify configured repositories failed (exit %d): %s", verifyReposResult.ExitCode, sanitizeBootstrapOutput(verifyReposResult.Stdout))
				}
			}
			log.Printf("[daytona] verify gh auth done")

			log.Printf("[daytona] cloning %d repositories for claw %s", len(repositories), clawID)
			s.setBootstrapStatus(clawID, "Syncing repositories")
			for i, repo := range repositories {
				log.Printf("[daytona] repository[%d]: %s", i, repo.Repo)
			}

			cloneScript := "export HOME=/home/daytona; set +x; . /etc/profile.d/elasticclaw-github.sh; cd ~/.openclaw/workspace; git config --global --get credential.helper >/dev/null || exit 1; [ -n \"$GH_TOKEN\" ] || exit 1; set -o pipefail; "
			for _, repo := range repositories {
				repoParts := strings.SplitN(repo.Repo, "/", 2)
				repoName := repo.Repo
				if len(repoParts) == 2 {
					repoName = repoParts[1]
				}
				cloneScript += fmt.Sprintf("echo '[daytona] cloning %s into %s'; if [ ! -d %q ]; then git clone https://x-access-token:${GH_TOKEN}@github.com/%s %s 2>&1 | sed \"s/${GH_TOKEN}/***REDACTED***/g\" || { echo '[daytona] clone FAILED: %s'; exit 1; }; echo '[daytona] clone OK: %s'; else git -C %s pull --ff-only 2>&1 | sed \"s/${GH_TOKEN}/***REDACTED***/g\" || { echo '[daytona] pull FAILED: %s'; exit 1; }; echo '[daytona] pull OK: %s'; fi; ", repo.Repo, repoName, repoName, repo.Repo, repoName, repo.Repo, repo.Repo, repoName, repo.Repo, repo.Repo)
			}
			cloneResult, cloneErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", cloneScript}, 2*time.Minute)
			if cloneErr != nil {
				return fmt.Errorf("clone repos: %w", cloneErr)
			}
			if cloneResult.ExitCode != 0 {
				return fmt.Errorf("clone repos failed (exit %d): %s", cloneResult.ExitCode, sanitizeBootstrapOutput(cloneResult.Stdout))
			}
			log.Printf("[daytona] clone repos done")

			if len(repositories) > 0 {
				verifyCloneScript := "export HOME=/home/daytona; cd ~/.openclaw/workspace; "
				for _, repo := range repositories {
					repoParts := strings.SplitN(repo.Repo, "/", 2)
					repoName := repo.Repo
					if len(repoParts) == 2 {
						repoName = repoParts[1]
					}
					verifyCloneScript += fmt.Sprintf("echo '[daytona] verifying %s'; [ -d %q/.git ] || { echo '[daytona] verify FAILED: %s/.git missing'; exit 1; }; echo '[daytona] verify OK: %s'; ", repoName, repoName, repoName, repoName)
				}
				verifyResult, verifyErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", verifyCloneScript}, 20*time.Second)
				if verifyErr != nil {
					return fmt.Errorf("verify cloned repos: %w", verifyErr)
				}
				if verifyResult.ExitCode != 0 {
					return fmt.Errorf("verify cloned repos failed (exit %d): %s", verifyResult.ExitCode, sanitizeBootstrapOutput(verifyResult.Stdout))
				}
				log.Printf("[daytona] verify cloned repos done")
			}
		}
	}

	if err := s.restoreCheckpointToDaytona(ctx, clawID, instanceID, p); err != nil {
		return fmt.Errorf("restore checkpoint: %w", err)
	}

	_, _ = s.db.Exec(`UPDATE claws SET bootstrap_ok=1 WHERE id=?`, clawID)
	log.Printf("[daytona] bootstrap gated ready for claw %s", clawID)
	s.setBootstrapStatus(clawID, "Connecting to hub")

	// Start the bridge last so the first registration happens only after the
	// workspace, template files, GitHub setup, and bootstrap_ok gate are ready.
	// The bridge (and therefore the agent) must run inside the workspace
	// directory so that repo-relative paths resolve correctly.
	if err := s.startDaytonaBridge(ctx, instanceID, p, s.clawHubURL(), clawID, clawToken, clawName); err != nil {
		return err
	}

	log.Printf("[daytona] bootstrap complete for claw %s", clawID)
	return nil
}

func recordE2EDaytonaSandboxID(sandboxID string) {
	recordE2EProviderID("Daytona sandbox", "ELASTICCLAW_E2E_DAYTONA_SANDBOX_ID_FILE", sandboxID)
}

func recordE2EReplicatedVMID(vmID string) {
	recordE2EProviderID("Replicated VM", "ELASTICCLAW_E2E_REPLICATED_VM_ID_FILE", vmID)
}

func recordE2EProviderID(label, envName, id string) {
	path := strings.TrimSpace(os.Getenv(envName))
	if path == "" || strings.TrimSpace(id) == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			log.Printf("[e2e] record %s id: mkdir %s: %v", label, dir, err)
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.Printf("[e2e] record %s id: open %s: %v", label, path, err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, id); err != nil {
		log.Printf("[e2e] record %s id: write %s: %v", label, path, err)
	}
}

func (s *Server) startDaytonaBridge(ctx context.Context, instanceID string, p *daytona.Provider, hubURL, clawID, clawToken, clawName string) error {
	prepCmd := daytonaPrepareBridgeCommand()
	result, err := p.ExecWithTimeout(ctx, instanceID, []string{prepCmd}, 15*time.Second)
	if err != nil {
		return fmt.Errorf("start claw-bridge prep: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("start claw-bridge prep failed (exit %d): %s", result.ExitCode, sanitizeBootstrapOutput(result.Stdout))
	}
	if strings.Contains(result.Stdout, "claw-bridge already running") {
		log.Printf("[daytona] claw-bridge already running")
		return nil
	}

	const sessionID = "elasticclaw-bridge"
	if err := p.EnsureSession(ctx, instanceID, sessionID); err != nil {
		return fmt.Errorf("start claw-bridge session: %w", err)
	}
	cmdID, err := p.ExecSessionAsync(ctx, instanceID, sessionID, daytonaAsyncBridgeCommand(hubURL, clawID, clawToken, clawName))
	if err != nil {
		return fmt.Errorf("start claw-bridge async: %w", err)
	}
	log.Printf("[daytona] claw-bridge async command started session=%s command=%s", sessionID, cmdID)

	verifyCmd := daytonaBridgeRunningCommand()
	var lastVerify string
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			time.Sleep(1 * time.Second)
		}
		result, err := p.ExecWithTimeout(ctx, instanceID, []string{verifyCmd}, 5*time.Second)
		if err != nil {
			lastVerify = err.Error()
			continue
		}
		if result.ExitCode == 0 {
			log.Printf("[daytona] start claw-bridge done: %s", strings.TrimSpace(result.Stdout))
			return nil
		}
		lastVerify = result.Stdout
	}
	if result, err := p.ExecWithTimeout(ctx, instanceID, []string{`tail -n 80 /home/daytona/claw-bridge.log 2>/dev/null || true`}, 5*time.Second); err == nil && strings.TrimSpace(result.Stdout) != "" {
		lastVerify = strings.TrimSpace(lastVerify) + "\n" + result.Stdout
	}
	return fmt.Errorf("start claw-bridge verification failed: %s", sanitizeBootstrapOutput(lastVerify))
}

func (s *Server) daytonaBridgeRunning(ctx context.Context, instanceID string, p *daytona.Provider) bool {
	result, err := p.ExecWithTimeout(ctx, instanceID, []string{daytonaBridgeRunningCommand()}, 5*time.Second)
	if err != nil {
		return false
	}
	return result.ExitCode == 0
}

func daytonaBridgeRunningCommand() string {
	return `export HOME=/home/daytona
PIDFILE=/home/daytona/.openclaw/run/claw-bridge.pid
if [ -s "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
  echo "claw-bridge running pid=$(cat "$PIDFILE")"
  exit 0
fi
if pgrep -x claw-bridge >/dev/null 2>&1; then
  echo "claw-bridge running"
  exit 0
fi
echo "claw-bridge not running"
exit 1`
}

func daytonaPrepareBridgeCommand() string {
	return `set -e
export HOME=/home/daytona
mkdir -p /home/daytona/.openclaw/workspace /home/daytona/.openclaw/run
cd /home/daytona/.openclaw/workspace
PIDFILE=/home/daytona/.openclaw/run/claw-bridge.pid
if [ -s "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
  echo "claw-bridge already running pid=$(cat "$PIDFILE")"
  exit 0
fi
if pgrep -x claw-bridge >/dev/null 2>&1; then
  echo "claw-bridge already running"
  exit 0
fi
if [ ! -s /tmp/claw-bridge ]; then
  echo "claw-bridge download missing at /tmp/claw-bridge"
  exit 1
fi
sudo install -m 0755 /tmp/claw-bridge /usr/local/bin/claw-bridge
test -x /usr/local/bin/claw-bridge || { echo "claw-bridge installed at /usr/local/bin/claw-bridge is not executable"; exit 1; }
rm -f "$PIDFILE"`
}

func daytonaAsyncBridgeCommand(hubURL, clawID, clawToken, clawName string) string {
	return fmt.Sprintf(`export HOME=/home/daytona
mkdir -p /home/daytona/.openclaw/workspace /home/daytona/.openclaw/run
cd /home/daytona/.openclaw/workspace
PIDFILE=/home/daytona/.openclaw/run/claw-bridge.pid
LOG=/home/daytona/claw-bridge.log
if [ -s "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
  echo "claw-bridge already running pid=$(cat "$PIDFILE")"
  exit 0
fi
if pgrep -x claw-bridge >/dev/null 2>&1; then
  echo "claw-bridge already running"
  exit 0
fi
rm -f "$PIDFILE"
ELASTICCLAW_HUB_URL=%q ELASTICCLAW_CLAW_ID=%q ELASTICCLAW_CLAW_TOKEN=%q ELASTICCLAW_CLAW_NAME=%q \
sh -c 'echo $$ > "$1"; exec /usr/local/bin/claw-bridge' sh "$PIDFILE" >> "$LOG" 2>&1`, hubURL, clawID, clawToken, clawName)
}

func (s *Server) downloadDaytonaConnector(ctx context.Context, clawID, instanceID string, p *daytona.Provider, downloadCmd string) error {
	delays := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
	}
	const maxAttempts = 6
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt == 1 {
			s.setBootstrapStatus(clawID, "Downloading ElasticClaw connector")
			log.Printf("[daytona] download claw-bridge...")
		} else {
			delay := delays[attempt-2]
			s.setBootstrapStatus(clawID, fmt.Sprintf("Retrying connector download in %s", formatRetryDelay(delay)))
			log.Printf("[daytona] download claw-bridge retry %d/%d in %s...", attempt, maxAttempts, delay)
			select {
			case <-ctx.Done():
				return fmt.Errorf("could not download ElasticClaw connector after %d attempts: %w", attempt-1, ctx.Err())
			case <-time.After(delay):
			}
			s.setBootstrapStatus(clawID, "Downloading ElasticClaw connector")
		}

		nvmSetup := `export HOME=/home/daytona; export NVM_DIR=/usr/local/share/nvm; [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh" && { nvm use 24 >/dev/null 2>&1 || nvm install 24 >/dev/null 2>&1; } ; `
		result, err := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", nvmSetup + downloadCmd}, 3*time.Minute)
		if err != nil {
			lastErr = err
			log.Printf("[daytona] download claw-bridge attempt %d/%d failed: %v", attempt, maxAttempts, err)
			continue
		}
		if result.ExitCode != 0 {
			lastErr = fmt.Errorf("exit %d: %s", result.ExitCode, sanitizeBootstrapOutput(result.Stdout))
			log.Printf("[daytona] download claw-bridge attempt %d/%d failed: %v", attempt, maxAttempts, lastErr)
			continue
		}

		s.setBootstrapStatus(clawID, "Starting ElasticClaw connector")
		log.Printf("[daytona] download claw-bridge done")
		return nil
	}

	return fmt.Errorf("could not download ElasticClaw connector after %d attempts. Last error: %s", maxAttempts, sanitizeBootstrapError(lastErr))
}

func (s *Server) setBootstrapStatus(clawID, status string) {
	if clawID == "" {
		return
	}
	res, err := s.db.Exec(`UPDATE claws SET bootstrap_status=? WHERE id=? AND status != 'deleted'`, status, clawID)
	if err != nil {
		return
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return
	}

	var tenantID string
	_ = s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=? AND status != 'deleted'`, clawID).Scan(&tenantID)
	if tenantID == "" {
		return
	}
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type: "claw_status",
		Payload: map[string]string{
			"claw_id":          clawID,
			"status":           "starting",
			"bootstrap_status": status,
		},
	})
}

func activityContent(activity map[string]interface{}) string {
	for _, key := range []string{"error", "command", "path", "url", "detail"} {
		if value, ok := activity[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	if value, ok := activity["message"].(string); ok && strings.TrimSpace(value) != "" && !isPhaseActivityText(value) {
		return strings.TrimSpace(value)
	}
	if value, ok := activity["tool"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	for _, key := range []string{"phase", "stream"} {
		if value, ok := activity[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "Activity"
}

func isPhaseActivityText(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "running", "completed", "complete", "done", "failed", "error":
		return true
	default:
		return false
	}
}

func isUnhelpfulActivityContent(activity map[string]interface{}, content string) bool {
	if kind, _ := activity["kind"].(string); kind == "still_working" {
		return true
	}
	return strings.HasPrefix(content, "No streamed output")
}

func daytonaBootstrapStatusForStep(label string) string {
	switch label {
	case "uninstall old openclaw", "install openclaw", "verify openclaw":
		return "Preparing runtime"
	case "install nix", "install docker", "preflight required commands", "stage openclaw plugin deps":
		return "Preparing runtime"
	case "configure openclaw model", "start openclaw gateway":
		return "Configuring OpenClaw"
	case "install git credential helper", "install gh cli", "fetch github bootstrap token json", "parse github bootstrap token", "write github token env":
		return "Preparing repository access"
	case "write SOUL.md", "write AGENTS.md", "write BOOTSTRAP.md", "write CONTEXT.md":
		return "Preparing workspace"
	default:
		if strings.HasPrefix(label, "write ") {
			return "Preparing workspace"
		}
		return "Preparing sandbox"
	}
}

func formatRetryDelay(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

func sanitizeBootstrapOutput(out string) string {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	lines := strings.Split(out, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "declare -x ") {
			continue
		}
		cleaned = append(cleaned, trimmed)
	}
	result := strings.TrimSpace(strings.Join(cleaned, "\n"))
	if result == "" {
		return "no command output"
	}
	const maxLen = 1200
	if len(result) <= maxLen {
		return result
	}
	return result[len(result)-maxLen:]
}

func sanitizeBootstrapError(err error) string {
	if err == nil {
		return "unknown error"
	}
	return sanitizeBootstrapOutput(err.Error())
}

func (s *Server) provisionExedev(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	p, err := newExedevProvider(cfg)
	if err != nil {
		return fmt.Errorf("exedev init: %w", err)
	}

	createReq := types.CreateRequest{
		Name:          req.ProviderName,
		TemplateFiles: files,
		Env:           env,
	}
	instance, err := p.Create(ctx, createReq)
	if err != nil {
		return fmt.Errorf("exedev create: %w", err)
	}
	log.Printf("exedev VM created: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='exedev', provider_id=? WHERE id=?`, instance.ID, clawID)

	// Bootstrap asynchronously
	go func() {
		if err := s.bootstrapExedev(context.Background(), clawID, instance.ID, p, files); err != nil {
			log.Printf("exedev bootstrap failed for claw %s: %v", clawID, err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Exedev bootstrap failed: %s", sanitizeBootstrapError(err)), false)
		}
	}()

	return nil
}

func (s *Server) bootstrapExedev(ctx context.Context, clawID, vmName string, p *exedevProvider.Provider, files map[string][]byte) error {
	log.Printf("[exedev] bootstrapping claw %s (vm %s)", clawID, vmName)
	s.setBootstrapStatus(clawID, "Waiting for sandbox SSH")

	// Wait for VM to be reachable
	host := vmName + ".exe.xyz"
	reachable := false
	for i := 0; i < 30; i++ {
		sshArgs := []string{"-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=no"}
		if p.SSHKeyPath() != "" {
			sshArgs = append(sshArgs, "-i", p.SSHKeyPath())
		}
		sshArgs = append(sshArgs, host, "echo ready")
		cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
		if err := cmd.Run(); err == nil {
			reachable = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !reachable {
		return fmt.Errorf("exedev VM %s was not reachable via SSH after 150s", vmName)
	}
	s.setBootstrapStatus(clawID, "Preparing ElasticClaw connector")

	// Load claw configuration from DB in a single atomic query
	var clawName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName, templateFilesJSON string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(`SELECT COALESCE(name,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,''), COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(
		&clawName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName, &templateFilesJSON,
	); err != nil {
		return fmt.Errorf("load claw config: %w", err)
	}
	var githubRepos []types.GitHubRepoAccess
	_ = json.Unmarshal([]byte(githubReposJSON), &githubRepos)
	var templateFiles map[string]string
	_ = json.Unmarshal([]byte(templateFilesJSON), &templateFiles)

	s.mu.RLock()
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.mu.RUnlock()

	linearToken := resolveLinearToken(hubCfg, linearWorkspace)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = hubCfg.DefaultModel
	}
	log.Printf("[exedev bootstrap] claw %.8s nix=%d docker=%d llm_key=%q template_default_model=%q hub_default_model=%q resolved_default_model=%q",
		clawID, nixEnabled, dockerEnabled, llmKeyName, templateDefaultModel, hubCfg.DefaultModel, defaultModel)

	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		return fmt.Errorf("claw-bridge URL not configured: set bridge_image in hub.yaml or build a tagged release")
	}

	// Generate a random gateway password for this VM
	gatewayPassword := randomHex(16)

	// Build bootstrap script using same pattern as replicated
	script := GenerateReplicatedBootstrapScript(BootstrapParams{
		ClawID:          clawID,
		ClawName:        clawName,
		ClawToken:       clawToken,
		HubURL:          s.clawHubURL(),
		DefaultModel:    defaultModel,
		GatewayPassword: gatewayPassword,
		BridgeURL:       bridgeURL,
		Nix:             nixEnabled != 0,
		Docker:          dockerEnabled != 0,
		TemplateFiles:   templateFiles,
		HubCfg:          hubCfg,
		GitHubRepos:     githubRepos,
		LLMKeyEnv:       llmKeyEnv,
		LinearEnv:       buildLinearEnv(linearToken),
		ProviderConfig:  buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName),
		OnboardFlags:    buildOnboardFlags(hubCfg.LLMKeys, llmKeyName),
	})

	if flakeFiles := templateFlakeFiles(templateFiles); len(flakeFiles) > 0 {
		if _, err := p.Exec(ctx, vmName, []string{"mkdir", "-p", "~/workspace"}); err != nil {
			return fmt.Errorf("create flake staging dir: %w", err)
		}
		for path, content := range flakeFiles {
			if err := p.WriteFile(ctx, vmName, "~/workspace/"+path, []byte(content)); err != nil {
				return fmt.Errorf("stage %s before bootstrap: %w", path, err)
			}
		}
	}

	// Run bootstrap script — this installs Node.js, OpenClaw, and starts claw-bridge
	if err := p.SetupScript(ctx, vmName, script); err != nil {
		return fmt.Errorf("exedev bootstrap script failed: %s", sanitizeBootstrapError(err))
	}
	log.Printf("[exedev] bootstrap script completed on %s", vmName)
	s.setBootstrapStatus(clawID, "Writing workspace files")

	// Write template files after bootstrap so openclaw onboard doesn't overwrite them
	workdir := "~/workspace"
	if _, err := p.Exec(ctx, vmName, []string{"mkdir", "-p", workdir}); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}
	var writeErrs []string
	for path, content := range files {
		fullPath := workdir + "/" + path
		if err := p.WriteFile(ctx, vmName, fullPath, content); err != nil {
			writeErrs = append(writeErrs, fmt.Sprintf("%s: %v", path, err))
		}
	}
	if len(writeErrs) > 0 {
		return fmt.Errorf("template file staging failed: %s", strings.Join(writeErrs, "; "))
	}
	if err := s.restoreCheckpointToExedev(ctx, clawID, vmName, p); err != nil {
		return fmt.Errorf("restore checkpoint: %w", err)
	}

	log.Printf("[exedev] bootstrap complete for claw %.8s on %s", clawID, vmName)
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
	recordE2EReplicatedVMID(vmID)
	// Store vm_id in the claw record — keep status='provisioning' so the poller can detect
	// the provisioning→running transition and trigger bootstrap. Skip if already deleted.
	_, _ = s.db.Exec(
		`UPDATE claws SET provider='replicated', provider_id=? WHERE id=? AND status NOT IN ('deleted','starting','connected','idle')`, vmID, clawID,
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
	log.Printf("  Status:        provisioning (waiting for VM to start)")
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

func (s *Server) keepAliveDaytonaSandboxes() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.petDaytonaSandboxes()
	}
}

// pruneAnalytics runs a daily cleanup of factory_analytics rows older than 1 year.
func (s *Server) pruneAnalytics() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		pruneFactoryAnalytics(s.db)
	}
}

func (s *Server) petDaytonaSandboxes() {
	rows, err := s.db.Query(`
		SELECT id, name, provider_id
		FROM claws
		WHERE provider = 'daytona'
		  AND provider_id != ''
		  AND status NOT IN ('idle','deleted','error','offline')
	`)
	if err != nil {
		log.Printf("keepAliveDaytonaSandboxes: query error: %v", err)
		return
	}
	defer rows.Close()

	type clawRow struct{ id, name, providerID string }
	var claws []clawRow
	for rows.Next() {
		var c clawRow
		if err := rows.Scan(&c.id, &c.name, &c.providerID); err == nil {
			claws = append(claws, c)
		}
	}
	if len(claws) == 0 {
		return
	}

	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["daytona"]
	s.mu.RUnlock()
	if !ok {
		log.Printf("keepAliveDaytonaSandboxes: no daytona provider configured")
		return
	}
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		log.Printf("keepAliveDaytonaSandboxes: provider init error: %v", err)
		return
	}

	for _, c := range claws {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := p.ExecWithTimeout(ctx, c.providerID, []string{"bash", "-lc", "true"}, 20*time.Second)
		cancel()
		if err != nil {
			log.Printf("[daytona] keepalive failed for %s (%s): %v", c.name, c.id[:8], err)
			continue
		}
		log.Printf("[daytona] keepalive ok for %s (%s)", c.name, c.id[:8])
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

		cc.mu.RLock()
		lastUserMessageAt := cc.lastUserMessageAt
		lastStatusAt := cc.lastStatusAt
		lastStatusBroadcastAt := cc.lastStatusBroadcastAt
		statusConn := cc.statusConn
		gatewayReady := cc.gatewayReady
		contextUsage := cc.contextUsage
		contextWarningSent := cc.contextWarningSent
		tenantID := cc.tenantID
		cc.mu.RUnlock()

		// If user sent a message in the last 2 minutes, skip status broadcast
		if now.Sub(lastUserMessageAt) < 2*time.Minute {
			continue
		}

		// If we have a status channel, ping it (hold lock during write)
		if statusConn != nil {
			cc.mu.RLock()
			sc := cc.statusConn
			cc.mu.RUnlock()
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
			log.Printf("[watchdog] %s", msg)
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
			cc.mu.Lock()
			cc.lastStatusBroadcastAt = now
			cc.mu.Unlock()
		}

		// Context usage warning (>90%) — skip if a streaming turn is in progress
		// so the heartbeat's more-urgent 95% in-streaming warning isn't suppressed.
		cc.mu.RLock()
		streaming := !cc.streamingStartedAt.IsZero()
		cc.mu.RUnlock()
		if contextUsage > 90 && !contextWarningSent && !streaming {
			msg := fmt.Sprintf("⚠️ Agent %s is at %d%% context usage. It should wrap up soon or restart.", name, contextUsage)
			log.Printf("[watchdog] %s", msg)
			s.broadcastToUsers(tenantID, types.WSMessage{
				Type: "message",
				Payload: map[string]interface{}{
					"role":    "system",
					"content": msg,
					"claw_id": id,
				},
			})
			// Update contextWarningSent under per-claw lock
			cc.mu.Lock()
			cc.contextWarningSent = true
			cc.mu.Unlock()
		}
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
			// 404 means VM was deleted externally — clean up the claw
			if strings.Contains(err.Error(), "HTTP 404") {
				log.Printf("pollProviderStatus: VM %s not found (404) — marking claw %s offline", c.providerID, c.id[:8])
				res, execErr := s.db.Exec(
					`UPDATE claws SET status='offline' WHERE id=? AND status IN ('provisioning','starting')`,
					c.id)
				if execErr == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						s.mu.Lock()
						if cc, ok := s.claws[c.id]; ok {
							cc.conn.Close(websocket.StatusGoingAway, "VM not found")
							delete(s.claws, c.id)
						}
						s.mu.Unlock()
						s.broadcastToUsers(c.tenantID, types.WSMessage{
							Type:    "claw_status",
							Payload: map[string]string{"claw_id": c.id, "status": "offline"},
						})
					}
				}
			} else {
				log.Printf("pollProviderStatus: get VM %s error: %v", c.providerID, err)
			}
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
			log.Printf("Replicated VM %s for claw %s (%s) terminated", c.providerID, c.name, c.id)
			go s.stopAgentWithReason(c.id, "Sandbox terminated (TTL expired or external shutdown)", true)
			// Note: stopAgentWithReason handles disconnect, status, broadcast, VM cleanup
			// Spawned in goroutine so slow issue-tracker APIs don't stall the poll loop.
			// Skip the rest of the status update logic for this claw
			continue
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

const (
	wakeMessageMarker  = "__WAKE_MESSAGE__"
	defaultWakeContent = "Introduce yourself briefly and let the user know you're ready to help."
	factoryWakeContent = `You've been assigned an issue. Use your tools to read the full details, then:
1. Send a short intro message to the user: your name, the issue you're working on, and your plan.
2. Start working. As you go, narrate your progress — what you're exploring, what you're trying, why.
3. If you hit something interesting or unexpected, say so.
4. When you open a PR, summarize what you did and what the PR contains.
5. Do NOT ask for permission at any point. Just work and keep the user informed.`
)

// sendWakeMessage sends a silent system message to wake the agent.
// For factory claws, it sends a task-specific prompt.
// A marker is stored in DB so reconnects after hub restart don't re-introduce.
func (s *Server) sendWakeMessage(cc *clawConn, clawID string) {
	wakeContent := defaultWakeContent
	if factory, _ := s.findFactoryForClaw(clawID); factory != nil {
		wakeContent = factoryWakeContent
	}
	wakeMsg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    clawID,
		TenantID:  cc.tenantID,
		Role:      "system",
		Content:   wakeMessageMarker,
		CreatedAt: now(),
	}
	_, _ = s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		wakeMsg.ID, wakeMsg.ClawID, wakeMsg.TenantID, wakeMsg.Role, wakeMsg.Content, wakeMsg.CreatedAt,
	)
	wakeMsg.Content = wakeContent
	_ = wsjson.Write(context.Background(), cc.conn, types.WSMessage{Type: "message", Payload: wakeMsg})

	// Note: We don't call sendNextQueuedMessage here because sendWakeMessage is launched
	// with 'go' (asynchronously). The normal end-of-turn path in handleClawWS read loop
	// will drain the queue once the claw finishes the wake response. This prevents race
	// conditions where both goroutines try to dequeue messages concurrently.
}

// clawHasMessages returns true if the claw already has message history.
// Used to suppress the intro wake message on reconnect.
func (s *Server) clawHasMessages(clawID string) bool {
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id = ?`, clawID).Scan(&count)
	return count > 0
}

// bootstrapReplicated SSHes into a newly-running Replicated VM, pulls the
// claw-bridge binary from GitHub Releases, and starts it with hub connection env vars.
func (s *Server) bootstrapReplicated(clawID, clawName, vmID string, cfg types.ProviderConfig) {
	s.setBootstrapStatus(clawID, "Preparing ElasticClaw workspace")
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
	var dockerEnabled int
	if err := s.db.QueryRow(`SELECT docker FROM claws WHERE id=?`, clawID).Scan(&dockerEnabled); err != nil {
		log.Printf("[bootstrap] warning: could not read docker flag for claw %s: %v", clawID[:8], err)
	}
	log.Printf("[bootstrap] claw %s nix=%d docker=%d", clawID[:8], nixEnabled, dockerEnabled)
	// Read llm_key selection
	var llmKeyName string
	_ = s.db.QueryRow(`SELECT COALESCE(llm_key,'') FROM claws WHERE id=?`, clawID).Scan(&llmKeyName)
	log.Printf("[bootstrap] OpenClaw model resolution claw=%s llm_key=%q template_default_model=%q hub_default_model=%q resolved_default_model=%q",
		clawID[:8], llmKeyName, templateDefaultModel, s.hubCfg.DefaultModel, defaultModel)

	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		log.Printf("[bootstrap] ERROR: bridge_image not set and hub version is 'dev' — set bridge_image in hub.yaml")
		s.stopAgentWithReason(clawID, "Bootstrap failed: bridge_image not configured", false)
		return
	}
	s.setBootstrapStatus(clawID, "Waiting for sandbox SSH")

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
		Docker:          dockerEnabled != 0,
		TemplateFiles:   files,
		HubCfg:          hubCfg,
		GitHubRepos:     githubRepos,
		LLMKeyEnv:       llmKeyEnv,
		LinearEnv:       buildLinearEnv(linearToken),
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

This agent has authenticated access to the following repositories via a GitHub App installation token. The token is fetched automatically — you don't need to configure anything.

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

	if flakeFiles := templateFlakeFiles(files); len(flakeFiles) > 0 {
		s.setBootstrapStatus(clawID, "Staging Nix flake")
		if err := s.sshWriteFiles(sshUser, sshHost, "$HOME/workspace", flakeFiles); err != nil {
			log.Printf("[bootstrap] failed to stage flake before bootstrap for claw %s: %v", clawID[:8], err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not stage flake files: %s", sanitizeBootstrapError(err)), false)
			return
		}
	}

	// Run bootstrap script first — this installs OpenClaw and initializes the workspace.
	// Template files must be written AFTER the script completes so openclaw onboard
	// doesn't overwrite BOOTSTRAP.md and other workspace files.
	var sshErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			s.setBootstrapStatus(clawID, "Retrying sandbox bootstrap in 10s")
			log.Printf("Bootstrap retry %d/5 for claw %s in 10s...", attempt, clawName)
			time.Sleep(10 * time.Second)
		}
		s.setBootstrapStatus(clawID, "Preparing ElasticClaw connector")
		if sshErr = s.sshRun(sshUser, sshHost, script); sshErr == nil {
			break
		}
		log.Printf("Bootstrap attempt %d/5 failed: %v", attempt, sanitizeBootstrapError(sshErr))
	}
	if sshErr != nil {
		log.Printf("Bootstrap failed for claw %s after 5 attempts: %v", clawID, sanitizeBootstrapError(sshErr))
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed after 5 attempts: %s", sanitizeBootstrapError(sshErr)), false)
		return
	}
	s.setBootstrapStatus(clawID, "Writing workspace files")

	// Write template files AFTER bootstrap — openclaw onboard initializes the workspace
	// and would overwrite BOOTSTRAP.md if we wrote it before the script ran.
	if len(files) > 0 {
		fileNames := make([]string, 0, len(files))
		for k := range files {
			fileNames = append(fileNames, k)
		}
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

	if err := s.restoreCheckpointToSSH(clawID, sshUser, sshHost); err != nil {
		log.Printf("[bootstrap] restore checkpoint failed: %v", err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore checkpoint failed: %s", sanitizeBootstrapError(err)), false)
		return
	}

	// Run GitHub credential helper setup (needs bridge connected for hub proxy,
	// but the hub token URL is publicly accessible so it works directly).
	if credHelper := buildGitHubCredentialHelper(hubCfg, s.clawHubURL(), clawID, githubRepos); credHelper != "# GitHub App not configured — skipping credential helper" {
		if err := s.sshRun(sshUser, sshHost, credHelper); err != nil {
			log.Printf("[bootstrap] warning: cred helper setup failed: %v", err)
		} else {
			log.Printf("[bootstrap] GitHub credential helper installed for claw %s", clawName)
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

func templateFlakeFiles(files map[string]string) map[string]string {
	flakeFiles := make(map[string]string, 2)
	for _, name := range []string{"flake.nix", "flake.lock"} {
		if content, ok := files[name]; ok {
			flakeFiles[name] = content
		}
	}
	return flakeFiles
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
		return "openai/gpt-5.5"
	case "fireworks":
		return "fireworks/accounts/fireworks/models/kimi-k2p6"
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
set -euo pipefail
if [ -z "${HOME:-}" ]; then
  HOME="$(getent passwd "$(id -u)" | cut -d: -f6)"
  export HOME
fi
if [ -z "${HOME:-}" ] || [ ! -d "$HOME" ]; then
  echo "ERROR: HOME is not set to a valid directory; cannot configure git credential helper" >&2
  exit 1
fi
echo "Configuring GitHub credential helper for user=$(whoami) home=$HOME"

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
if ! command -v git &>/dev/null; then
  echo "Installing git..."
  sudo apt-get update -qq
  sudo apt-get install -y git
fi

# Configure git to use the credential helper
git config --global credential.helper /usr/local/bin/elasticclaw-git-credentials
git config --global --get-all credential.helper | grep -Fx /usr/local/bin/elasticclaw-git-credentials >/dev/null
git config --show-origin --global --get-all credential.helper

# Install gh CLI if possible. gh is useful, but git credential registration above is mandatory.
if ! command -v gh &>/dev/null; then
  (
    set +e
    echo "Installing gh CLI..."
    if curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg | sudo dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg 2>/dev/null; then
      echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" | sudo tee /etc/apt/sources.list.d/github-cli.list > /dev/null
      sudo apt-get update -qq && sudo apt-get install -y gh 2>/dev/null || echo "gh install failed, continuing"
    else
      echo "gh keyring install failed, continuing"
    fi
  ) || true
fi

# Configure gh to use the credential helper via GH_TOKEN env (set in a wrapper)
if command -v gh &>/dev/null; then
  # Write GH_TOKEN to /etc/profile.d so it's available in ALL shells
  if printf 'export GH_TOKEN=$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | grep ^password | cut -d= -f2)\n' | sudo tee /etc/profile.d/elasticclaw-github.sh > /dev/null; then
    sudo chmod +x /etc/profile.d/elasticclaw-github.sh || echo "GitHub profile.d chmod failed, continuing"
    echo "GitHub profile.d configured"
  else
    echo "GitHub profile.d setup failed, continuing"
  fi
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
	output, err := s.sshRunWithTimeout(user, host, script, 0)
	if err != nil {
		return err
	}
	log.Printf("bootstrap output:\n%s", output)
	return nil
}

// sshRunWithTimeout connects to host via the hub's SSH identity and runs a script.
// A zero timeout waits for the remote command to finish.
func (s *Server) sshRunWithTimeout(user, host, script string, timeout time.Duration) (string, error) {
	pubKeyType := s.identity.PrivateKey.PublicKey().Type()
	pubKeyFP := gossh.FingerprintSHA256(s.identity.PrivateKey.PublicKey())
	log.Printf("SSH attempting: user=%s host=%s key-type=%s fingerprint=%s", user, host, pubKeyType, pubKeyFP)
	log.Printf("SSH public key being used:\n%s", s.identity.PublicKey)

	sshCfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.identity.PrivateKey)},
		HostKeyCallback: s.sshHostKeyCallback(host),
		Timeout:         30 * time.Second,
	}

	client, err := gossh.Dial("tcp", host, sshCfg)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", host, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
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

	runDone := make(chan error, 1)
	go func() {
		runDone <- sess.Run("/bin/bash")
	}()
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err := <-runDone:
			if err != nil {
				mu.Lock()
				output := buf.String()
				mu.Unlock()
				return output, fmt.Errorf("ssh script failed: %w\noutput: %s", err, output)
			}
		case <-timer.C:
			_ = sess.Close()
			_ = client.Close()
			mu.Lock()
			output := buf.String()
			mu.Unlock()
			return output, fmt.Errorf("ssh script timed out after %s\noutput: %s", timeout, output)
		}
	} else if err := <-runDone; err != nil {
		mu.Lock()
		output := buf.String()
		mu.Unlock()
		return output, fmt.Errorf("ssh script failed: %w\noutput: %s", err, output)
	}
	mu.Lock()
	output := buf.String()
	mu.Unlock()
	return output, nil
}

// sshWriteFiles writes a map of filename->content to a remote directory via SSH.
func (s *Server) sshWriteFiles(user, host, dir string, files map[string]string) error {
	sshCfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.identity.PrivateKey)},
		HostKeyCallback: s.sshHostKeyCallback(host),
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
		HostKeyCallback: s.sshHostKeyCallback(fmt.Sprintf("%s:%d", sshHost, sshPort)),
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
	case "exedev":
		s.terminateExedevVM(vmID)
	default:
		log.Printf("terminateVM: unsupported provider %q for VM %s", provider, vmID)
	}
}

// terminateExedevVM destroys an exedev VM by ID.
func (s *Server) terminateExedevVM(vmID string) {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["exedev"]
	s.mu.RUnlock()
	if !ok {
		log.Printf("terminateExedevVM: no exedev provider configured")
		return
	}

	log.Printf("terminateExedevVM: destroying VM %s (ssh_key_path=%q)", vmID, cfg.SSHKeyPath)
	p, err := newExedevProvider(cfg)
	if err != nil {
		log.Printf("terminateExedevVM: provider init error: %v", err)
		return
	}
	if err := p.Destroy(context.Background(), vmID, false); err != nil {
		log.Printf("terminateExedevVM: failed to destroy VM %s: %v", vmID, err)
		return
	}
	log.Printf("Exedev VM %s terminated", vmID)
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

	var workspaceName string
	var reposJSON string
	err := s.db.QueryRow(
		`SELECT COALESCE(template,''), github_repos FROM claws WHERE id = ?`,
		clawID,
	).Scan(&workspaceName, &reposJSON)
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
	githubApps := append([]*types.GitHubAppConfig(nil), s.hubCfg.GitHubApps...)
	s.mu.RUnlock()
	if workspaceApps, err := loadWorkspaceGitHubAppConfigs(workspaceName); err == nil && len(workspaceApps) > 0 {
		githubApps = append(workspaceApps, githubApps...)
	}
	if len(githubApps) == 0 {
		http.Error(w, "no github apps configured", http.StatusNotImplemented)
		return
	}
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
		log.Printf("github token issued via app_id=%d for claw %s", appCfg.AppID, clawID[:8])
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
		Format:    "pre",
		CreatedAt: now(),
	}
	_, _ = s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at) VALUES(?,?,?,?,?,?,?)`,
		msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.Format, msg.CreatedAt,
	)
	_ = wsjson.Write(ctx, cc.conn, types.WSMessage{Type: "message", Payload: msg})
	s.broadcastToUsers(cc.tenantID, types.WSMessage{Type: "message", Payload: msg})
}

// sendNextQueuedMessage checks if there are queued messages for a claw and sends
// the next one if the claw is not currently busy. Must be called with s.mu unlocked.
func (s *Server) sendNextQueuedMessage(cc *clawConn) {
	cc.mu.Lock()

	// Check if there's anything to send
	if len(cc.messageQueue) == 0 {
		cc.mu.Unlock()
		return
	}

	// Check if claw is still busy
	if !cc.streamingStartedAt.IsZero() || cc.streamingMsgID != "" {
		cc.mu.Unlock()
		return
	}

	// Send the next queued message - copy fields needed for sending
	msg := cc.messageQueue[0]
	cc.messageQueue = cc.messageQueue[1:]
	remainingCount := len(cc.messageQueue)
	conn := cc.conn
	tenantID := cc.tenantID
	clawID := cc.id
	cc.lastUserMessageAt = time.Now()
	cc.mu.Unlock()

	// Send via WebSocket (outside of lock to avoid blocking other goroutines)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := wsjson.Write(ctx, conn, types.WSMessage{Type: "message", Payload: msg})

	if err != nil {
		// Write failed - re-enqueue the message at the front so it can be retried
		log.Printf("[hub] failed to send queued message to %s: %v, re-enqueueing", clawID[:8], err)
		s.mu.RLock()
		if currentCC, ok := s.claws[clawID]; ok {
			currentCC.mu.Lock()
			// Prepend the message back to the front of the queue
			currentCC.messageQueue = append([]types.HubMessage{msg}, currentCC.messageQueue...)
			currentCC.mu.Unlock()
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

	log.Printf("[hub] sent queued message to %s (%d remaining in queue)", clawID[:8], remainingCount)
}
