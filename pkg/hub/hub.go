package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/internal/webui"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
	"github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"
	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
	"github.com/elasticclaw/elasticclaw/pkg/hub/telemetry"
	daytona "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	exedevProvider "github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	replicatedpkg "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	gossh "golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Server is the ElasticClaw hub.
type Server struct {
	db        *sql.DB
	addr      string
	logger    *slog.Logger
	hubCfg    *types.HubConfig
	identity  *HubIdentity
	mux       *http.ServeMux
	artifacts artifact.Store
	metrics   *serverMetrics

	mu    sync.RWMutex
	claws map[string]*clawConn // claw_id -> conn
	users map[string]*userConn // tenant_id -> []conn (broadcast)
	// one-time oauth_code -> signed GitHub session token

	dependencyStatus *dependencyStatusService

	fileAckMu           sync.Mutex
	fileAckWaiters      map[string]chan types.FileAck      // request_id -> waiter
	fileReadWaiters     map[string]chan types.FileReadResp // request_id -> waiter
	volumeAttachWaiters map[string]chan types.VolumeAttachAck
	volumeSyncWaiters   map[string]chan types.VolumeSyncAck

	checkpointMu      sync.Mutex
	checkpointWaiters map[string]chan error // checkpoint_id -> waiter

	// githubBaseURL overrides the GitHub API base for testing (default: https://api.github.com)
	githubBaseURL string
	// linearBaseURL overrides the Linear API base for testing (default: https://api.linear.app)
	linearBaseURL string
	// shortcutBaseURL overrides the Shortcut API base for testing (default: https://api.app.shortcut.com)
	shortcutBaseURL string
	// jiraBaseURL overrides the Jira API base for testing.
	jiraBaseURL string
	// fireworksBaseURL overrides the Fireworks API base for testing (default: https://api.fireworks.ai)
	fireworksBaseURL          string
	fireworksModelsMu         sync.Mutex
	fireworksModelsCacheKey   string
	fireworksModelsCache      []LLMModelOption
	fireworksModelsCacheUntil time.Time
	modelAuthJobsMu           sync.Mutex
	modelAuthJobs             map[string]*modelAuthLoginJob

	// webhookDedup prevents duplicate Linear webhook deliveries from creating
	// duplicate claws. Keyed by issue transition fingerprint; entries expire after 30s.
	webhookDedup   map[string]time.Time
	webhookDedupMu sync.Mutex

	// promoteMu serializes promotePendingClaws to prevent TOCTOU race where
	// multiple terminating claws each read active < max and promote, exceeding limit.
	promoteMu sync.Mutex

	// cronScheduler manages scheduled workflow runs
	cronScheduler *cronScheduler
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
	artifacts, err := artifact.NewStoreFromHubConfig(context.Background(), identityDir, hubCfg.ArtifactStorage)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("artifact storage: %w", err)
	}
	id, err := LoadOrCreateIdentity(identityDir)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("hub identity: %w", err)
	}
	logf("Hub SSH public key:\n%s", id.PublicKey)
	srv := &Server{
		db:                db,
		addr:              addr,
		logger:            slog.Default(),
		hubCfg:            hubCfg,
		identity:          id,
		artifacts:         artifacts,
		metrics:           newServerMetrics(db),
		claws:             make(map[string]*clawConn),
		users:             make(map[string]*userConn),
		dependencyStatus:  newDependencyStatusService(hubCfg),
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

	// Start cron scheduler for workflow triggers
	srv.cronScheduler = newCronScheduler(srv)
	if err := srv.cronScheduler.Start(); err != nil {
		logf("[cron] failed to start scheduler: %v", err)
	}
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
		logf("[hub] web UI disabled (--no-web-ui)")
	} else if webFS, err := webui.FS(); err == nil {
		if _, indexErr := webFS.Open("index.html"); indexErr != nil {
			logf("[hub] web UI not built — run: make build-web")
		} else {
			s.serveWebUI(mux, webFS)
			logf("[hub] serving embedded web UI")
		}
	}

	logf("ElasticClaw Hub listening on %s", s.addr)
	if s.hubCfg.UIPassword == "" {
		logf("⚠️  Web UI password not set — using default: 'admin'. Set ui_password in hub.yaml to secure the UI.")
	}
	// The metrics middleware wraps the mux directly so it sees the matched
	// route pattern; the otelhttp span (opt-in via ELASTICCLAW_OTLP_ENDPOINT)
	// sits just outside it and is skipped entirely when tracing is disabled.
	var handler http.Handler = mux
	if s.metrics != nil {
		handler = httpserver.WithMetrics(s.metrics, handler)
	}
	if telemetry.Enabled() {
		handler = otelhttp.NewHandler(handler, "hub")
	}
	return http.ListenAndServe(s.addr, httpserver.WithRecovery(httpserver.WithRequestID(s.logger, httpserver.CORS(handler))))
}

// registerRoutes wires the Server's handlers and auth middlewares into the
// httpserver route table. The route patterns themselves live in
// pkg/hub/httpserver; this method is only the composition site.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	var metricsHandler http.Handler
	if s.metrics != nil {
		metricsHandler = s.metrics.handler()
	}
	httpserver.RegisterRoutes(mux, serverAuth{s}, httpserver.Handlers{
		Metrics: metricsHandler,

		ClawWS: s.handleClawWS,
		UserWS: s.handleUserWS,

		Login:               s.handleLogin,
		WebLogin:            s.handleWebLogin,
		WebLogout:           s.handleWebLogout,
		WebMe:               s.handleWebMe,
		AuthConfig:          s.handleAuthConfig,
		GitHubClientID:      s.handleGitHubClientID,
		GitHubOAuthExchange: s.handleGitHubOAuthExchange,
		Branding:            s.handleBranding,

		HubConfig:            s.handleHubConfig,
		Settings:             s.handleSettings,
		SettingsStatus:       s.handleSettingsStatus,
		GitHubAppTest:        s.handleGitHubAppTest,
		ModelAuthLogin:       s.handleModelAuthLogin,
		ModelAuthLoginStatus: s.handleModelAuthLoginStatus,

		Templates:      s.handleTemplates,
		TemplateDetail: s.handleTemplateDetail,

		LinearWebhook:       s.handleLinearWebhook,
		GitHubWebhook:       s.handleGitHubWebhook,
		GitHubIssuesWebhook: s.handleGitHubIssuesWebhook,
		ShortcutWebhook:     s.handleShortcutWebhook,
		JiraWebhook:         s.handleJiraWebhook,
		ExternalWebhook:     s.handleExternalWebhook,

		FactoryEvents:                 s.handleFactoryEvents,
		FactoryTrigger:                s.handleFactoryTrigger,
		FactoryAnalytics:              s.handleFactoryAnalytics,
		FactoriesCRUD:                 s.handleFactoriesCRUD,
		AllFactoriesAnalytics:         s.handleAllFactoriesAnalytics,
		TaskRunAnalyticsSummary:       s.handleTaskRunAnalyticsSummary,
		TaskRunAnalyticsFilterOptions: s.handleTaskRunAnalyticsFilterOptions,
		TaskRunAnalyticsRuns:          s.handleTaskRunAnalyticsRuns,
		DependencyStatus:              s.handleDependencyStatus,

		WorkspacesCRUD:             s.handleWorkspacesCRUD,
		WorkspaceWorkflowsList:     s.handleWorkspaceWorkflowsList,
		WorkspaceWorkflowDetail:    s.handleWorkspaceWorkflowDetail,
		WorkspaceWorkflowTrigger:   s.handleWorkspaceWorkflowTrigger,
		CronWorkflowTrigger:        s.handleCronWorkflowTrigger,
		CronWorkflowRuns:           s.handleCronWorkflowRuns,
		CronWorkflowNextRun:        s.handleCronWorkflowNextRun,
		WorkspaceSecretsCRUD:       s.handleWorkspaceSecretsCRUD,
		WorkspaceGitHubAppsCRUD:    s.handleWorkspaceGitHubAppsCRUD,
		WorkspaceIssueTrackersCRUD: s.handleWorkspaceIssueTrackersCRUD,

		SecretsCRUD:          s.handleSecretsCRUD,
		MCPCrud:              s.handleMCPCrud,
		Claws:                s.handleClaws,
		ClawDetail:           s.handleClawDetail,
		CheckpointBlobUpload: s.handleCheckpointBlobUpload,
		CheckpointInternal:   s.handleCheckpointInternal,
		VolumeArchive:        s.handleVolumeArchive,
		Terminal:             s.handleTerminal,
		GitHubToken:          s.handleGitHubToken,
		Messages:             s.handleMessages,
		FileUpload:           s.handleFileUpload,
		FileView:             s.handleFileView,
		ClawSubresource:      s.handleClawSubresource,

		AIConfigApply:         s.handleAIConfigApply,
		AIConfigRevert:        s.handleAIConfigRevert,
		AIConfigBackup:        s.handleAIConfigBackup,
		AIConfigStream:        s.handleAIConfigStream,
		AIConfigCurrentConfig: s.handleAIConfigCurrentConfig,
		AIConfig:              s.handleAIConfig,

		Doctor:             s.handleDoctor,
		TroubleshootStream: s.handleTroubleshootStream,

		DebugClaws: s.handleDebugClaws,
	})
}

// serverAuth adapts the Server's unexported auth middlewares to the
// httpserver.Auth interface consumed by the route table.
type serverAuth struct{ s *Server }

func (a serverAuth) WithAuth(next http.HandlerFunc) http.HandlerFunc { return a.s.withAuth(next) }

func (a serverAuth) WithWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return a.s.withWebAuth(next)
}

func (a serverAuth) WithWebAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return a.s.withWebAdminAuth(next)
}

func (a serverAuth) WithAdminForMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	return a.s.withAdminForMethods(next, methods...)
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
		ctx = logger.NewContext(ctx, logger.FromContext(ctx).With("tenant_id", tenantID))
		if githubLogin != "" {
			ctx = context.WithValue(ctx, ctxGitHubLoginKey{}, githubLogin)
		}
		r = r.WithContext(ctx)
		next(w, r)
	}
}

func (s *Server) withAdminForMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	adminMethods := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		adminMethods[method] = struct{}{}
	}
	authHandler := s.withAuth(next)
	adminHandler := s.withConfigMutationAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminMethods[r.Method]; ok {
			adminHandler(w, r)
			return
		}
		authHandler(w, r)
	}
}

func (s *Server) withConfigMutationAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get(webSessionHeader)
		}
		queryToken := false
		if token == "" {
			token = r.URL.Query().Get("token")
			queryToken = token != ""
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

		if token == hubToken && hubToken != "" {
			next(w, r)
			return
		}
		if queryToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		sessionSecret := s.webSessionSecret()
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
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	s.mu.RLock()
	disablePassword := s.hubCfg.Auth != nil && s.hubCfg.Auth.DisablePasswordAuth
	s.mu.RUnlock()
	if disablePassword {
		writeErr(w, http.StatusForbidden, "forbidden", "password login disabled")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	if body.Password != s.resolveUIPassword() {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid password")
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

func (s *Server) handleBranding(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	var appName, logoURL string
	if s.hubCfg.Branding != nil {
		appName = s.hubCfg.Branding.AppName
		logoURL = s.hubCfg.Branding.LogoURL
	}
	s.mu.RUnlock()
	jsonOK(w, map[string]string{
		"appName": appName,
		"logoUrl": logoURL,
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
		logf("[webui] embedded files: %v", names)
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
		`SELECT id, name, template, COALESCE(provider,''), COALESCE(provider_id,''), status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,''), COALESCE(bootstrap_status,''), COALESCE(bootstrap_diagnostic,''), COALESCE(github_issue_id,'') FROM claws WHERE tenant_id = ? AND status != 'deleted' ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		logfCtx(r.Context(), "handleClaws query error: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", fmt.Sprintf("db error: %v", err))
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
		if err := rows.Scan(&c.ID, &c.Name, &c.Template, &c.Provider, &c.ProviderID, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color, &c.BootstrapStatus, &c.BootstrapDiagnostic, &c.GitHubIssueID); err != nil {
			continue
		}
		c.GitHubIssueURL = githubIssueURL(c.GitHubIssueID)
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
			if cc.GatewayReady {
				c.Status = "connected"
			} else {
				c.Status = "starting"
			}
			c.ContextUsage = cc.ContextUsage
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

func githubIssueURL(issueID string) string {
	parts := strings.Split(issueID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	for _, ch := range parts[2] {
		if ch < '0' || ch > '9' {
			return ""
		}
	}
	return fmt.Sprintf("https://github.com/%s/%s/issues/%s", parts[0], parts[1], parts[2])
}

func (s *Server) handleCreateClaw(w http.ResponseWriter, r *http.Request, tenantID string) {
	var req types.CreateClawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	if req.Name == "" || req.Provider == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name and provider are required")
		return
	}

	// Check provider is configured
	s.mu.RLock()
	provCfg, ok := s.hubCfg.Providers[req.Provider]
	s.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "unprocessable", fmt.Sprintf("provider %q is not configured on this hub", req.Provider))
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
			logfCtx(r.Context(), "[create] injected secret_ref %s as %s into claw env", secretRef, envName)
		} else {
			logfCtx(r.Context(), "[create] WARNING: secret_ref %q not found in hub secrets", secretRef)
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
	logfCtx(r.Context(), "[create] claw %s: nix=%d docker=%d", req.Name, nixEnabled, dockerEnabled)

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
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
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
				logfCtx(r.Context(), "[create] MCP server %q not found in hub config, skipping", mcpRef.Name)
				continue
			}
			if !hubMCP.Enabled {
				logfCtx(r.Context(), "[create] MCP server %q is disabled, skipping", mcpRef.Name)
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
					logfCtx(r.Context(), "[create] SSE MCP server %q not yet supported for local stdio", mcpRef.Name)
					continue
				}
			}
			if len(cmd) == 0 {
				logfCtx(r.Context(), "[create] MCP server %q has no command, skipping", mcpRef.Name)
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
		logfCtx(r.Context(), "Provisioning claw %s (%s) via %s (provider name: %s)...", req.Name, clawID, req.Provider, req.ProviderName)
		ctx := context.Background()
		var provErr error

		switch req.Provider {
		case "daytona":
			provErr = s.provisionDaytona(ctx, clawID, req, provCfg, templateFiles, env)
		case "replicated":
			provErr = s.provisionReplicated(ctx, clawID, req, provCfg, env)
		case "exedev":
			provErr = s.provisionExedev(ctx, clawID, req, provCfg, templateFiles, env)
		case "docker":
			provErr = s.provisionDocker(ctx, clawID, req, provCfg, templateFiles)
		case "lambda-microvms":
			provErr = s.provisionLambdaMicroVMs(ctx, clawID, req, provCfg, templateFiles)
		default:
			provErr = fmt.Errorf("unsupported provider: %s", req.Provider)
		}

		if provErr != nil {
			logfCtx(r.Context(), "provisioning failed for claw %s: %v", clawID, provErr)
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
					writeErr(w, http.StatusNotFound, "not_found", "not found")
				} else {
					writeErr(w, http.StatusInternalServerError, "internal", "db error")
				}
				return
			}
			var clawTags []string
			_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
			if !canModifyClaw(accessCfg, ghLogin, clawTags) {
				writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
				return
			}
		}
		var body struct {
			Name  *string   `json:"name"`
			Tags  *[]string `json:"tags"`
			Color *string   `json:"color"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid body")
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
				cc.Tags = normalized
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
					writeErr(w, http.StatusNotFound, "not_found", "not found")
				} else {
					writeErr(w, http.StatusInternalServerError, "internal", "db error")
				}
				return
			}
			var clawTags []string
			_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
			if !canModifyClaw(accessCfg, ghLogin, clawTags) {
				writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
				return
			}
		}

		// Look up provider info before marking deleted so we can terminate the VM.
		var provider, providerID, clawStatus string
		_ = s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,''), COALESCE(status,'') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&provider, &providerID, &clawStatus)

		// Post a comment on the linked issue/story when a factory-created claw is killed manually
		factory, issueID := s.findFactoryForClaw(clawID)
		if factory != nil && issueID != "" {
			switch factory.Integration {
			case "linear":
				token := s.resolveLinearTokenForFactory(factory)
				if token != "" {
					if err := s.commentLinearIssue(token, issueID, "Agent stopped: killed manually via dashboard"); err != nil {
						logfCtx(r.Context(), "[kill] failed to comment Linear issue %s: %v", issueID, err)
					} else {
						logfCtx(r.Context(), "[kill] commented Linear issue %s", issueID)
					}
				}
			case "shortcut":
				token := s.resolveShortcutToken(factory.Workspace)
				if token != "" {
					if err := commentShortcutIssue(s.resolveShortcutBaseURL(), token, issueID, "Agent stopped: killed manually via dashboard"); err != nil {
						logfCtx(r.Context(), "[kill] failed to comment Shortcut story %s: %v", issueID, err)
					} else {
						logfCtx(r.Context(), "[kill] commented Shortcut story %s", issueID)
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
								logfCtx(r.Context(), "[kill] failed to comment GitHub issue %s: %v", issueID, err)
							} else {
								logfCtx(r.Context(), "[kill] commented GitHub issue %s", issueID)
							}
						}
					}
				}
			}
		}

		res, err := s.db.Exec(`UPDATE claws SET status='deleted', bootstrap_status='' WHERE id = ? AND tenant_id = ? AND status != 'deleted'`, clawID, tenantID)
		if err != nil {
			logfCtx(r.Context(), "kill: db soft-delete error for claw %s: %v", clawID, err)
			writeErr(w, http.StatusInternalServerError, "internal", fmt.Sprintf("db error: %v", err))
			return
		}
		rowsAffected, err := res.RowsAffected()
		if err != nil || rowsAffected == 0 {
			writeErr(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		if clawStatus != "error" {
			s.recordTaskRunManualStopBeforeDelivery(clawID, ghLogin)
		}
		if s.cronScheduler != nil {
			s.cronScheduler.FinishRunByClawID(clawID, "canceled", "manually killed")
		}
		// Notify dashboards before provider cleanup so the card disappears immediately.
		s.broadcastToUsers(tenantID, types.WSMessage{
			Type:    "claw_status",
			Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
		})
		// Disconnect WebSocket if online
		s.mu.Lock()
		if cc, ok := s.claws[clawID]; ok {
			cc.WS.Close(websocket.StatusNormalClosure, "killed")
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
		`SELECT id, name, template, COALESCE(provider,''), COALESCE(provider_id,''), status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,''), COALESCE(bootstrap_status,''), COALESCE(bootstrap_diagnostic,''), COALESCE(github_issue_id,'') FROM claws WHERE id = ? AND tenant_id = ? AND status != 'deleted'`,
		clawID, tenantID,
	).Scan(&c.ID, &c.Name, &c.Template, &c.Provider, &c.ProviderID, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color, &c.BootstrapStatus, &c.BootstrapDiagnostic, &c.GitHubIssueID)
	_ = json.Unmarshal([]byte(tagsJSON), &c.Tags)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if ghLogin != "" && !canViewClaw(accessCfg, ghLogin, c.Tags) {
		writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
		return
	}
	c.TenantID = tenantID
	c.GitHubIssueURL = githubIssueURL(c.GitHubIssueID)
	if lastSeen.Valid {
		c.LastSeen = lastSeen.Time
	}
	s.mu.RLock()
	cc, online := s.claws[c.ID]
	s.mu.RUnlock()
	if online {
		if cc.GatewayReady {
			c.Status = "connected"
		} else {
			c.Status = "starting"
		}
		c.ContextUsage = cc.ContextUsage
	} else if c.Status != "provisioning" && c.Status != "error" {
		c.Status = "offline"
	}
	jsonOK(w, c)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/messages/"), "/")
	parts := strings.Split(path, "/")
	clawID := parts[0]
	if clawID == "" {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if len(parts) > 1 {
		switch parts[1] {
		case "timeline":
			s.handleMessageTimeline(w, r, tenantID, clawID)
		case "activity":
			s.handleMessageActivity(w, r, tenantID, clawID)
		default:
			writeErr(w, http.StatusNotFound, "not_found", "not found")
		}
		return
	}
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
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid request")
			return
		}

		// Apply tag-based interact filter for GitHub OAuth users
		if ghLoginMsg != "" {
			// Fetch claw tags to check interact permission
			var tagsJSONMsg string
			if err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSONMsg); err != nil {
				if err == sql.ErrNoRows {
					writeErr(w, http.StatusNotFound, "not_found", "not found")
				} else {
					writeErr(w, http.StatusInternalServerError, "internal", "db error")
				}
				return
			}
			var clawTagsMsg []string
			_ = json.Unmarshal([]byte(tagsJSONMsg), &clawTagsMsg)
			if !canInteractWithClaw(accessCfgMsg, ghLoginMsg, clawTagsMsg) {
				writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
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
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		s.recordTaskRunDashboardMessage(clawID, ghLoginMsg, msg.ID)
		// Forward to claw if connected (or queue if busy)
		s.mu.RLock()
		cc := s.claws[clawID]
		s.mu.RUnlock()
		if cc != nil {
			cc.Mu.Lock()
			cc.LastUserMessageAt = time.Now()
			// Check if claw is currently streaming/processing
			isBusy := !cc.StreamingStartedAt.IsZero() || cc.StreamingMsgID != ""
			if isBusy {
				// Queue the message for later delivery
				cc.MessageQueue = append(cc.MessageQueue, msg)
				queueLen := len(cc.MessageQueue)
				cc.Mu.Unlock()
				logfCtx(r.Context(), "[hub] message queued for %s (queue length: %d)", clawID[:8], queueLen)
			} else {
				cc.Mu.Unlock()
				// Send immediately
				_ = wsjson.Write(r.Context(), cc.WS, types.WSMessage{Type: "message", Payload: msg})
				s.metrics.wsMessage("out", "claw")
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
			writeErr(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		var clawTagsMsg []string
		_ = json.Unmarshal([]byte(tagsJSONMsg), &clawTagsMsg)
		if !canViewClaw(accessCfgMsg, ghLoginMsg, clawTagsMsg) {
			writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
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
			 AND NOT (role = 'system' AND content IN (?, ?, ?, ?, ?, ?))
			 ORDER BY created_at DESC LIMIT ?`,
			clawID, tenantID, before, wakeMessageMarker, defaultWakeContent, initialPlanWakeContent, initialPlanRequiredMarker, initialPlanAcceptedMarker, initialPlanCorrectionSentMarker, limit,
		)
	} else if after != "" {
		rows, err = s.db.Query(
			`SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at FROM messages
			 WHERE claw_id = ? AND tenant_id = ? AND created_at > ?
			 AND NOT (role = 'system' AND content IN (?, ?, ?, ?, ?, ?))
			 ORDER BY created_at ASC LIMIT ?`,
			clawID, tenantID, after, wakeMessageMarker, defaultWakeContent, initialPlanWakeContent, initialPlanRequiredMarker, initialPlanAcceptedMarker, initialPlanCorrectionSentMarker, limit,
		)
	} else {
		// Default: last N messages
		rows, err = s.db.Query(
			`SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at FROM messages
			 WHERE claw_id = ? AND tenant_id = ?
			 AND NOT (role = 'system' AND content IN (?, ?, ?, ?, ?, ?))
			 ORDER BY created_at DESC LIMIT ?`,
			clawID, tenantID, wakeMessageMarker, defaultWakeContent, initialPlanWakeContent, initialPlanRequiredMarker, initialPlanAcceptedMarker, initialPlanCorrectionSentMarker, limit,
		)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
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

type activitySummaryMeta struct {
	Count int    `json:"count"`
	From  string `json:"from"`
	To    string `json:"to,omitempty"`
}

func hiddenSystemMessagesArgs() []interface{} {
	return []interface{}{
		wakeMessageMarker,
		defaultWakeContent,
		initialPlanWakeContent,
		initialPlanRequiredMarker,
		initialPlanAcceptedMarker,
		initialPlanCorrectionSentMarker,
	}
}

func hiddenSystemMessagesSQL() string {
	return `AND NOT (role = 'system' AND content IN (?, ?, ?, ?, ?, ?))`
}

func (s *Server) handleMessageTimeline(w http.ResponseWriter, r *http.Request, tenantID, clawID string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !s.canViewMessages(w, r, tenantID, clawID) {
		return
	}

	limit := parsePositiveLimit(r, 50, 100)
	before := r.URL.Query().Get("before")
	rows, err := s.queryConversationMessages(clawID, tenantID, before, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	if len(rows) == 0 {
		summary, err := s.activitySummary(clawID, tenantID, nil, parseTimeCursor(before), "", before)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if summary == nil {
			jsonOK(w, []types.HubMessage{})
			return
		}
		jsonOK(w, []types.HubMessage{*summary})
		return
	}

	timeline := make([]types.HubMessage, 0, len(rows)*2)
	firstCreated := rows[0].CreatedAt
	hasOlderConversation, err := s.hasConversationBefore(clawID, tenantID, firstCreated)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if !hasOlderConversation {
		firstCursor := firstCreated.Format(time.RFC3339Nano)
		summary, err := s.activitySummary(clawID, tenantID, nil, &firstCreated, "", firstCursor)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if summary != nil {
			timeline = append(timeline, *summary)
		}
	}
	for i, msg := range rows {
		timeline = append(timeline, msg)
		lower := msg.CreatedAt
		lowerCursor := lower.Format(time.RFC3339Nano)
		var upper *time.Time
		upperCursor := ""
		if i+1 < len(rows) {
			nextCreated := rows[i+1].CreatedAt
			upper = &nextCreated
			upperCursor = nextCreated.Format(time.RFC3339Nano)
		} else if before != "" {
			upper = parseTimeCursor(before)
			upperCursor = before
		}
		summary, err := s.activitySummary(clawID, tenantID, &lower, upper, lowerCursor, upperCursor)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if summary != nil {
			timeline = append(timeline, *summary)
		}
	}
	jsonOK(w, timeline)
}

func (s *Server) handleMessageActivity(w http.ResponseWriter, r *http.Request, tenantID, clawID string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !s.canViewMessages(w, r, tenantID, clawID) {
		return
	}

	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	before := r.URL.Query().Get("before")
	limit := parsePositiveLimit(r, 200, 500)
	order := strings.ToLower(r.URL.Query().Get("order"))
	if order != "desc" {
		order = "asc"
	}

	query := `SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at
		FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role = 'activity'`
	args := []interface{}{clawID, tenantID}
	if from != "" {
		query += ` AND created_at > ?`
		if parsed := parseTimeCursor(from); parsed != nil {
			args = append(args, *parsed)
		} else {
			args = append(args, from)
		}
	}
	if to != "" {
		query += ` AND created_at < ?`
		if parsed := parseTimeCursor(to); parsed != nil {
			args = append(args, *parsed)
		} else {
			args = append(args, to)
		}
	}
	if before != "" {
		query += ` AND created_at < ?`
		if parsed := parseTimeCursor(before); parsed != nil {
			args = append(args, *parsed)
		} else {
			args = append(args, before)
		}
	}
	if order == "desc" {
		query += ` ORDER BY created_at DESC LIMIT ?`
	} else {
		query += ` ORDER BY created_at ASC LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer rows.Close()
	msgs, err := scanHubMessages(rows)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if msgs == nil {
		msgs = []types.HubMessage{}
	}
	jsonOK(w, msgs)
}

func parsePositiveLimit(r *http.Request, def, max int) int {
	limit := def
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > max {
		return max
	}
	return limit
}

func parseTimeCursor(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return &parsed
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return &parsed
	}
	return nil
}

func scanHubMessages(rows *sql.Rows) ([]types.HubMessage, error) {
	var msgs []types.HubMessage
	for rows.Next() {
		var m types.HubMessage
		if err := rows.Scan(&m.ID, &m.ClawID, &m.TenantID, &m.Role, &m.Content, &m.Format, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *Server) queryConversationMessages(clawID, tenantID, before string, limit int) ([]types.HubMessage, error) {
	query := `SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role != 'activity' ` + hiddenSystemMessagesSQL()
	args := []interface{}{clawID, tenantID}
	args = append(args, hiddenSystemMessagesArgs()...)
	if before != "" {
		query += ` AND created_at < ?`
		if parsed := parseTimeCursor(before); parsed != nil {
			args = append(args, *parsed)
		} else {
			args = append(args, before)
		}
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs, err := scanHubMessages(rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

func (s *Server) hasConversationBefore(clawID, tenantID string, before time.Time) (bool, error) {
	query := `SELECT COUNT(*) FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role != 'activity' AND created_at < ? ` + hiddenSystemMessagesSQL()
	args := []interface{}{clawID, tenantID, before}
	args = append(args, hiddenSystemMessagesArgs()...)
	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Server) activitySummary(clawID, tenantID string, from, to *time.Time, fromCursor, toCursor string) (*types.HubMessage, error) {
	query := `SELECT COUNT(*), COALESCE(MIN(created_at), ''), COALESCE(MAX(created_at), '')
		FROM messages WHERE claw_id = ? AND tenant_id = ? AND role = 'activity'`
	args := []interface{}{clawID, tenantID}
	if from != nil {
		query += ` AND created_at > ?`
		args = append(args, *from)
	}
	if to != nil {
		query += ` AND created_at < ?`
		args = append(args, *to)
	}

	var count int
	var minCreated, maxCreated string
	if err := s.db.QueryRow(query, args...).Scan(&count, &minCreated, &maxCreated); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	meta := activitySummaryMeta{Count: count, From: fromCursor, To: toCursor}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	createdAt := now()
	if maxCreated != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, maxCreated); err == nil {
			createdAt = parsed
		}
	}
	return &types.HubMessage{
		ID:        "activity-summary-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte(clawID+"|"+fromCursor+"|"+toCursor)).String(),
		ClawID:    clawID,
		TenantID:  tenantID,
		Role:      "activity_summary",
		Content:   fmt.Sprintf("%d tool calls", count),
		Format:    "activity_summary:" + string(data),
		CreatedAt: createdAt,
	}, nil
}

func (s *Server) canViewMessages(w http.ResponseWriter, r *http.Request, tenantID, clawID string) bool {
	ghLoginMsg := githubLoginFromContext(r.Context())
	if ghLoginMsg == "" {
		return true
	}
	var accessCfgMsg *types.AccessConfig
	s.mu.RLock()
	if s.hubCfg.Auth != nil {
		accessCfgMsg = s.hubCfg.Auth.Access
	}
	s.mu.RUnlock()

	var tagsJSONMsg string
	err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSONMsg)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return false
	}
	var clawTagsMsg []string
	_ = json.Unmarshal([]byte(tagsJSONMsg), &clawTagsMsg)
	if !canViewClaw(accessCfgMsg, ghLoginMsg, clawTagsMsg) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
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
	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "daytona")
	instance, err := p.Create(createCtx, createReq)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("daytona create: %w", err)
	}
	logfCtx(ctx, "daytona workspace created: %s (claw %s)", instance.ID, clawID)
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
				logfCtx(ctx, "[daytona] full bootstrap retry for claw %s in 15s...", clawName)
				time.Sleep(15 * time.Second)
			}
			lastErr = s.bootstrapDaytona(context.Background(), clawID, clawName, instance.ID, p, env)
			if lastErr == nil {
				return
			}
			if s.daytonaBridgeRunning(context.Background(), instance.ID, p) {
				logfCtx(ctx, "[daytona] bootstrap attempt %d/%d for claw %s returned error after claw-bridge started; treating bootstrap as complete: %v", attempt, maxBootstrapAttempts, clawName, lastErr)
				return
			}
			logfCtx(ctx, "[daytona] bootstrap attempt %d/%d failed for claw %s: %v", attempt, maxBootstrapAttempts, clawName, lastErr)
		}
		logfCtx(ctx, "[daytona] bootstrap failed for claw %s: %v", clawName, lastErr)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Daytona bootstrap failed: %v", lastErr), false)
		// stopAgentWithReason already terminates the VM; no need to destroy again
	}()
	return nil
}

func (s *Server) bootstrapDaytona(ctx context.Context, clawID, clawName, instanceID string, p *daytona.Provider, env map[string]string) error {
	logfCtx(ctx, "[daytona] bootstrapping claw %s (instance %s)", clawID, instanceID)
	s.setBootstrapStatus(clawID, "Preparing runtime")

	execResult := func(label string, timeout time.Duration, cmd string) (*types.ExecResult, error) {
		s.setBootstrapStatus(clawID, daytonaBootstrapStatusForStep(label))
		const maxAttempts = 3
		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if attempt == 1 {
				logfCtx(ctx, "[daytona] %s...", label)
			} else {
				logfCtx(ctx, "[daytona] %s retry %d/%d...", label, attempt, maxAttempts)
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
			logfCtx(ctx, "[daytona] %s done", label)
			return result, nil
		}
		return nil, lastErr
	}

	exec := func(label string, timeout time.Duration, cmd string) error {
		_, err := execResult(label, timeout, cmd)
		return err
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
		logfCtx(ctx, "[daytona] warning: uninstall failed (ok if not installed): %v", err)
	}

	const daytonaOpenClawVersion = cliversion.OpenClawVersion
	if err := exec("start openclaw install", 20*time.Second, daytonaStartOpenClawInstallCommand(daytonaOpenClawVersion)); err != nil {
		return err
	}
	deadline := time.Now().Add(4 * time.Minute)
	var lastInstallStatus string
	installComplete := false
	for !installComplete {
		result, err := execResult("check openclaw install", 15*time.Second, daytonaOpenClawInstallStatusCommand(daytonaOpenClawVersion))
		if err != nil {
			lastInstallStatus = err.Error()
		} else {
			lastInstallStatus = strings.TrimSpace(result.Stdout)
			switch {
			case strings.Contains(result.Stdout, "openclaw-install-status=ok"):
				installComplete = true
			case strings.Contains(result.Stdout, "openclaw-install-status=failed"),
				strings.Contains(result.Stdout, "openclaw-install-status=missing"),
				strings.Contains(result.Stdout, "openclaw-install-status=unknown"):
				return fmt.Errorf("install openclaw failed: %s", sanitizeBootstrapOutput(result.Stdout))
			}
		}
		if installComplete {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("install openclaw timed out: %s", sanitizeBootstrapOutput(lastInstallStatus))
		}
		time.Sleep(10 * time.Second)
	}

	if err := exec("verify openclaw", 20*time.Second,
		fmt.Sprintf(`export NVM_DIR=/usr/local/share/nvm; \
NPM="$NVM_DIR/current/bin/npm"; \
PREFIX="$("$NPM" config get prefix)"; \
export PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH"; \
hash -r; \
OPENCLAW_PATH="$(command -v openclaw || true)"; \
OPENCLAW_VERSION="$(openclaw --version 2>&1 || true)"; \
PACKAGE_VERSION="$(PREFIX="$PREFIX" node -e "try{console.log(require(process.env.PREFIX + '/lib/node_modules/openclaw/package.json').version)}catch(e){process.exit(0)}" 2>/dev/null || true)"; \
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
			logfCtx(ctx, "[daytona] warning: nix install failed: %v", err)
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
			logfCtx(ctx, "[daytona] warning: docker install failed: %v", err)
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
	modelAuthEnvDaytona := buildModelAuthEnv(s.hubCfg, llmKeyNameDaytona)
	apiKeyAuthSyncDaytona := buildOpenClawAPIKeyAuthSyncShell(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	onboardFlags := buildOnboardFlags(s.hubCfg.LLMKeys, llmKeyNameDaytona, defaultModelDaytona)
	providerConfigScript := buildOpenClawProviderConfig(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	if activeKeyDaytona != nil {
		activeKeyNameDaytona = activeKeyDaytona.Name
		activeKeyProviderDaytona = activeKeyDaytona.Provider
	}
	s.mu.RUnlock()
	logfCtx(ctx, "[daytona] OpenClaw model resolution claw=%s selected_llm_key=%q active_llm_key=%q provider=%q default_model=%q config_patch=%t",
		clawID, llmKeyNameDaytona, activeKeyNameDaytona, activeKeyProviderDaytona, defaultModelDaytona, providerConfigScript != "")
	gatewayPassword := randomHex(16)
	if restoreShell := buildModelAuthRestoreShell(modelAuthEnvDaytona); restoreShell != "" {
		if err := exec("restore model auth", 30*time.Second, "export HOME=/home/daytona; "+restoreShell); err != nil {
			return fmt.Errorf("restore model auth: %w", err)
		}
	}
	if installCmd := daytonaInstallCodingModelCLICommand(defaultModelDaytona); installCmd != "" {
		if err := exec("install selected model cli", 2*time.Minute, installCmd); err != nil {
			return fmt.Errorf("install selected model cli: %w", err)
		}
	}
	onboardCmd := fmt.Sprintf(
		"%sexport NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; openclaw onboard --non-interactive --accept-risk --skip-daemon --skip-health %s 2>&1",
		llmKeyEnvDaytona,
		onboardFlags,
	)
	logfCtx(ctx, "[daytona] onboard openclaw...")
	onboardResult, onboardErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", "export HOME=/home/daytona; " + onboardCmd}, 2*time.Minute)
	if onboardErr != nil {
		result, diagErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", `export HOME=/home/daytona; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`}, 10*time.Second)
		if diagErr != nil || strings.TrimSpace(result.Stdout) != "exists" {
			return fmt.Errorf("onboard openclaw: %w", onboardErr)
		}
		logfCtx(ctx, "[daytona] onboard returned error, but config file exists; continuing")
	} else if onboardResult.ExitCode != 0 {
		result, diagErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", `export HOME=/home/daytona; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`}, 10*time.Second)
		if diagErr != nil || strings.TrimSpace(result.Stdout) != "exists" {
			return fmt.Errorf("onboard openclaw failed (exit %d): %s", onboardResult.ExitCode, onboardResult.Stdout)
		}
		logfCtx(ctx, "[daytona] onboard returned non-zero, but config file exists; continuing")
	} else {
		logfCtx(ctx, "[daytona] onboard openclaw done")
	}

	if apiKeyAuthSyncDaytona != "" {
		syncCmd := `export HOME=/home/daytona; export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; ` + llmKeyEnvDaytona + apiKeyAuthSyncDaytona
		if err := exec("sync openclaw api key auth", 30*time.Second, syncCmd); err != nil {
			return fmt.Errorf("sync openclaw api key auth: %w", err)
		}
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
		logfCtx(ctx, "[daytona] warning: plugin deps staging failed: %v", err)
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
		templateFiles = workspaceTemplateFiles(templateFiles)
		for name, content := range templateFiles {
			name := name
			content := content
			safeName, err := cleanWorkspaceFilePath(name)
			if err != nil {
				logfCtx(ctx, "[daytona] warning: skipping invalid template file path %q: %v", name, err)
				continue
			}
			targetPath := "/home/daytona/.openclaw/workspace/" + safeName
			targetDir := path.Dir(targetPath)
			writeCmd := fmt.Sprintf(
				`export HOME=/home/daytona; mkdir -p %s && cat > %s << 'ELASTICCLAW_EOF'
%s
ELASTICCLAW_EOF`,
				shellQuote(targetDir), shellQuote(targetPath), content)
			if err := exec("write "+name, 15*time.Second, writeCmd); err != nil {
				logfCtx(ctx, "[daytona] warning: failed to write %s: %v", name, err)
			}
		}
		logfCtx(ctx, "[daytona] template files written for claw %s", clawID)
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

			configureGitHubTokenRefresh := `export HOME=/home/daytona
set +x
` + buildGitHubTokenProfileInstallScript() + `
` + buildGitHubCLIWrapperInstallScript() + `
. /etc/profile.d/elasticclaw-github.sh
command -v gh
[ -n "${GH_TOKEN:-}" ]
gh --version`
			logfCtx(ctx, "[daytona] configure gh token refresh (no retries)...")
			ghAuthResult, ghAuthErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", configureGitHubTokenRefresh}, 30*time.Second)
			if ghAuthErr != nil {
				return fmt.Errorf("configure gh token refresh: %w", ghAuthErr)
			}
			if ghAuthResult.ExitCode != 0 {
				return fmt.Errorf("configure gh token refresh failed (exit %d): %s", ghAuthResult.ExitCode, sanitizeBootstrapOutput(ghAuthResult.Stdout))
			}
			logfCtx(ctx, "[daytona] configure gh token refresh done")

			ghStatusScript := `export HOME=/home/daytona
set +x
. /etc/profile.d/elasticclaw-github.sh
gh auth status`
			logfCtx(ctx, "[daytona] verify gh auth (no retries)...")
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
					verifyReposScript += fmt.Sprintf("gh repo view %s >/dev/null || exit 1; ", shellQuote(repo.Repo))
				}
				logfCtx(ctx, "[daytona] verify configured repositories (no retries)...")
				verifyReposResult, verifyReposErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", verifyReposScript}, 30*time.Second)
				if verifyReposErr != nil {
					return fmt.Errorf("verify configured repositories: %w", verifyReposErr)
				}
				if verifyReposResult.ExitCode != 0 {
					return fmt.Errorf("verify configured repositories failed (exit %d): %s", verifyReposResult.ExitCode, sanitizeBootstrapOutput(verifyReposResult.Stdout))
				}
			}
			logfCtx(ctx, "[daytona] verify gh auth done")

			logfCtx(ctx, "[daytona] cloning %d repositories for claw %s", len(repositories), clawID)
			s.setBootstrapStatus(clawID, "Syncing repositories")
			for i, repo := range repositories {
				logfCtx(ctx, "[daytona] repository[%d]: %s", i, repo.Repo)
			}

			cloneScript := buildDaytonaGitHubCloneScript(repositories)
			cloneResult, cloneErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", cloneScript}, 2*time.Minute)
			if cloneErr != nil {
				return fmt.Errorf("clone repos: %w", cloneErr)
			}
			if cloneResult.ExitCode != 0 {
				return fmt.Errorf("clone repos failed (exit %d): %s", cloneResult.ExitCode, sanitizeBootstrapOutput(cloneResult.Stdout))
			}
			logfCtx(ctx, "[daytona] clone repos done")

			if len(repositories) > 0 {
				verifyCloneScript := "export HOME=/home/daytona; cd ~/.openclaw/workspace; "
				for _, repo := range repositories {
					verifyCloneScript += daytonaRepoReadinessSnippet(repo.Repo)
				}
				verifyResult, verifyErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", verifyCloneScript}, 20*time.Second)
				if verifyErr != nil {
					return fmt.Errorf("verify cloned repos: %w", verifyErr)
				}
				if verifyResult.ExitCode != 0 {
					return fmt.Errorf("verify cloned repos failed (exit %d): %s", verifyResult.ExitCode, sanitizeBootstrapOutput(verifyResult.Stdout))
				}
				logfCtx(ctx, "[daytona] verify cloned repos done")
			}
			if discoveryScript := buildRepoInstructionDiscoveryScript("$HOME/.openclaw/workspace", repositories); discoveryScript != "" {
				if err := exec("discover repo instructions", 20*time.Second, "export HOME=/home/daytona; "+discoveryScript); err != nil {
					logfCtx(ctx, "[daytona] warning: repo instruction discovery failed for claw %s: %v", clawID, err)
				} else {
					logfCtx(ctx, "[daytona] repo instruction discovery done")
				}
			}
		}
	}

	if err := s.restoreCheckpointToDaytona(ctx, clawID, instanceID, p); err != nil {
		return fmt.Errorf("restore checkpoint: %w", err)
	}

	// Final workspace readiness gate: verify every configured repository is
	// present at the expected path and has a .git directory. Fail fast with a
	// sanitized, actionable bootstrap error instead of starting the agent
	// against an incomplete workspace.
	if len(repositories) > 0 {
		s.setBootstrapStatus(clawID, "Verifying workspace readiness")
		verifyScript := "export HOME=/home/daytona; cd ~/.openclaw/workspace; "
		for _, repo := range repositories {
			verifyScript += daytonaRepoReadinessSnippet(repo.Repo)
		}
		verifyResult, verifyErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", verifyScript}, 20*time.Second)
		if verifyErr != nil {
			diag := fmt.Sprintf("Workspace readiness failed: %v", verifyErr)
			s.setBootstrapStatusWithDiagnostic(clawID, "Workspace incomplete", diag)
			return fmt.Errorf("workspace readiness: %w", verifyErr)
		}
		if verifyResult.ExitCode != 0 {
			diag := fmt.Sprintf("Workspace incomplete: required repositories are missing. %s", sanitizeBootstrapOutput(verifyResult.Stdout))
			s.setBootstrapStatusWithDiagnostic(clawID, "Workspace incomplete", diag)
			return fmt.Errorf("workspace readiness failed (exit %d): %s", verifyResult.ExitCode, sanitizeBootstrapOutput(verifyResult.Stdout))
		}
		logfCtx(ctx, "[daytona] workspace readiness verified for claw %s", clawID)
	}

	s.markBootstrapReady(clawID)
	logfCtx(ctx, "[daytona] bootstrap gated ready for claw %s", clawID)
	s.setBootstrapStatus(clawID, "Connecting to hub")

	// Start the bridge last so the first registration happens only after the
	// workspace, template files, GitHub setup, and bootstrap_ok gate are ready.
	// The bridge (and therefore the agent) must run inside the workspace
	// directory so that repo-relative paths resolve correctly.
	if err := s.startDaytonaBridge(ctx, instanceID, p, s.clawHubURL(), clawID, clawToken, clawName); err != nil {
		return err
	}

	logfCtx(ctx, "[daytona] bootstrap complete for claw %s", clawID)
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
			logf("[e2e] record %s id: mkdir %s: %v", label, dir, err)
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		logf("[e2e] record %s id: open %s: %v", label, path, err)
		return
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, id); err != nil {
		logf("[e2e] record %s id: write %s: %v", label, path, err)
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
		logfCtx(ctx, "[daytona] claw-bridge already running")
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
	logfCtx(ctx, "[daytona] claw-bridge async command started session=%s command=%s", sessionID, cmdID)

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
			logfCtx(ctx, "[daytona] start claw-bridge done: %s", strings.TrimSpace(result.Stdout))
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

func daytonaStartOpenClawInstallCommand(version string) string {
	installScript := fmt.Sprintf(`set -o pipefail
export HOME=/home/daytona
export NVM_DIR=/usr/local/share/nvm
NPM="$NVM_DIR/current/bin/npm"
PREFIX="$("$NPM" config get prefix)"
export PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH"
LOG=/tmp/openclaw-install.log
STATUS=/tmp/openclaw-install.status
echo "npm=$NPM prefix=$PREFIX"
if sudo env PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH" "$NPM" install -g openclaw@%s --prefix "$PREFIX" --ignore-scripts 2>&1; then
  hash -r
  echo ok > "$STATUS"
  echo "install done"
else
  rc=$?
  echo "failed:$rc" > "$STATUS"
  exit "$rc"
fi`, version)
	return fmt.Sprintf(`export HOME=/home/daytona
LOG=/tmp/openclaw-install.log
STATUS=/tmp/openclaw-install.status
rm -f "$LOG" "$STATUS"
setsid nohup bash -c %s > "$LOG" 2>&1 </dev/null &
echo "openclaw-install-status=started"`, shellQuote(installScript))
}

func daytonaInstallCodingModelCLICommand(model string) string {
	var packageSpec, binary string
	switch {
	case strings.HasPrefix(model, "codex/"):
		packageSpec = "@openai/codex@" + cliversion.FromEnv("ELASTICCLAW_CODEX_CLI_VERSION", "0.141.0")
		binary = "codex"
	case strings.HasPrefix(model, "grok/"):
		packageSpec = "@xai-official/grok@" + cliversion.FromEnv("ELASTICCLAW_GROK_CLI_VERSION", "0.1.0")
		binary = "grok"
	default:
		return ""
	}
	return fmt.Sprintf(`export HOME=/home/daytona
export NVM_DIR=/usr/local/share/nvm
NPM="$NVM_DIR/current/bin/npm"
PREFIX="$("$NPM" config get prefix)"
export PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH"
sudo env PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH" "$NPM" install -g %s --prefix "$PREFIX" --ignore-scripts
hash -r
%s --version 2>&1 || true`, shellQuote(packageSpec), binary)
}

func daytonaOpenClawInstallStatusCommand(version string) string {
	return fmt.Sprintf(`export HOME=/home/daytona
LOG=/tmp/openclaw-install.log
STATUS=/tmp/openclaw-install.status
if [ -s "$STATUS" ]; then
  status="$(cat "$STATUS")"
  case "$status" in
    ok)
      echo "openclaw-install-status=ok"
      exit 0
      ;;
    failed:*)
      echo "openclaw-install-status=failed"
      echo "$status"
      tail -n 120 "$LOG" 2>/dev/null || true
      exit 0
      ;;
    *)
      echo "openclaw-install-status=unknown:$status"
      tail -n 120 "$LOG" 2>/dev/null || true
      exit 0
      ;;
  esac
fi
if pgrep -af %s >/dev/null 2>&1; then
  echo "openclaw-install-status=pending"
  tail -n 20 "$LOG" 2>/dev/null || true
  exit 0
fi
echo "openclaw-install-status=missing"
tail -n 120 "$LOG" 2>/dev/null || true`, shellQuote("openclaw@"+version))
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
ELASTICCLAW_HUB_URL=%s ELASTICCLAW_CLAW_ID=%s ELASTICCLAW_CLAW_TOKEN=%s ELASTICCLAW_CLAW_NAME=%s \
sh -c 'echo $$ > "$1"; exec /usr/local/bin/claw-bridge' sh "$PIDFILE" >> "$LOG" 2>&1`,
		shellQuote(hubURL),
		shellQuote(clawID),
		shellQuote(clawToken),
		shellQuote(clawName),
	)
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
			logfCtx(ctx, "[daytona] download claw-bridge...")
		} else {
			delay := delays[attempt-2]
			s.setBootstrapStatus(clawID, fmt.Sprintf("Retrying connector download in %s", formatRetryDelay(delay)))
			logfCtx(ctx, "[daytona] download claw-bridge retry %d/%d in %s...", attempt, maxAttempts, delay)
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
			logfCtx(ctx, "[daytona] download claw-bridge attempt %d/%d failed: %v", attempt, maxAttempts, err)
			continue
		}
		if result.ExitCode != 0 {
			lastErr = fmt.Errorf("exit %d: %s", result.ExitCode, sanitizeBootstrapOutput(result.Stdout))
			logfCtx(ctx, "[daytona] download claw-bridge attempt %d/%d failed: %v", attempt, maxAttempts, lastErr)
			continue
		}

		s.setBootstrapStatus(clawID, "Starting ElasticClaw connector")
		logfCtx(ctx, "[daytona] download claw-bridge done")
		return nil
	}

	return fmt.Errorf("could not download ElasticClaw connector after %d attempts. Last error: %s", maxAttempts, sanitizeBootstrapError(lastErr))
}

type replicatedBootstrapRetryOptions struct {
	Label      string
	RetryLabel string
	Attempts   int
	Delays     []time.Duration
	Sleep      func(time.Duration)
	Run        func() error
}

func retryReplicatedBootstrapStep(s *Server, clawID string, opts replicatedBootstrapRetryOptions) error {
	if opts.Attempts < 1 {
		opts.Attempts = 1
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.RetryLabel == "" {
		opts.RetryLabel = "Retrying " + strings.ToLower(opts.Label)
	}

	var lastErr error
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if attempt > 1 {
			delay := replicatedBootstrapDelay(opts.Delays, attempt-2)
			if s != nil && clawID != "" {
				s.setBootstrapStatus(clawID, fmt.Sprintf("%s in %s", opts.RetryLabel, formatRetryDelay(delay)))
			}
			logf("[bootstrap] %s retry %d/%d in %s...", opts.Label, attempt, opts.Attempts, delay)
			opts.Sleep(delay)
		}
		if s != nil && clawID != "" {
			s.setBootstrapStatus(clawID, opts.Label)
		}
		if err := opts.Run(); err != nil {
			lastErr = err
			logf("[bootstrap] %s attempt %d/%d failed: %s", opts.Label, attempt, opts.Attempts, sanitizeBootstrapError(err))
			continue
		}
		return nil
	}

	return fmt.Errorf("%s failed after %d attempts: %s", opts.Label, opts.Attempts, sanitizeBootstrapError(lastErr))
}

func replicatedBootstrapDelay(delays []time.Duration, idx int) time.Duration {
	if len(delays) == 0 {
		return 5 * time.Second
	}
	if idx < len(delays) {
		return delays[idx]
	}
	return delays[len(delays)-1]
}

func replicatedWorkspaceReadinessCommand(dir string, files map[string]string) string {
	if len(files) == 0 {
		return "true"
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("set -e\n")
	for _, name := range names {
		remotePath := strings.TrimRight(dir, "/") + "/" + name
		b.WriteString("test -e ")
		b.WriteString(shellDoubleQuote(remotePath))
		b.WriteString(" || { echo ")
		b.WriteString(shellQuote("missing workspace file: " + name))
		b.WriteString("; exit 1; }\n")
	}
	b.WriteString("echo 'workspace files verified'\n")
	return b.String()
}

func shellDoubleQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if i == 0 && strings.HasPrefix(s, "$HOME") && (len(s) == len("$HOME") || s[len("$HOME")] == '/') {
			b.WriteString("$HOME")
			i += len("$HOME") - 1
			continue
		}
		switch s[i] {
		case '\\', '"', '`', '$':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

func (s *Server) setBootstrapStatus(clawID, status string) {
	s.setBootstrapStatusWithDiagnostic(clawID, status, "")
}

func repoDirectoryName(repoFullName string) string {
	repoParts := strings.SplitN(repoFullName, "/", 2)
	if len(repoParts) == 2 {
		return repoParts[1]
	}
	return repoFullName
}

func (s *Server) markBootstrapReady(clawID string) {
	if clawID == "" {
		return
	}
	_, _ = s.db.Exec(`UPDATE claws SET bootstrap_ok=1, bootstrap_diagnostic='' WHERE id=?`, clawID)
	s.promoteBootstrapReadyClaw(clawID)
}

func (s *Server) promoteBootstrapReadyClaw(clawID string) bool {
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil {
		return false
	}

	cc.Mu.RLock()
	gatewayReady := cc.GatewayReady
	tenantID := cc.TenantID
	cc.Mu.RUnlock()
	if !gatewayReady {
		return false
	}

	res, err := s.db.Exec(`UPDATE claws SET status='connected', bootstrap_status='' WHERE id=? AND status='starting' AND bootstrap_ok=1`, clawID)
	if err != nil {
		return false
	}
	rowsUpdated, _ := res.RowsAffected()
	if rowsUpdated == 0 {
		return false
	}

	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "connected"},
	})
	logf("[bridge] ✓ ready after bootstrap: %s", clawID[:8])
	go s.requestBootstrapCheckpoint(clawID)
	s.startWorkflowAfterVolumes(context.Background(), cc, clawID)
	return true
}

func (s *Server) startWorkflowAfterVolumes(ctx context.Context, cc *clawConn, clawID string) {
	if cc == nil {
		return
	}
	cc.Mu.Lock()
	if cc.WorkflowStartPending || cc.WorkflowStartDone {
		cc.Mu.Unlock()
		return
	}
	cc.WorkflowStartPending = true
	cc.Mu.Unlock()

	go func() {
		if err := s.attachWorkflowVolumes(ctx, cc, clawID); err != nil {
			cc.Mu.Lock()
			cc.WorkflowStartPending = false
			cc.Mu.Unlock()
			logfCtx(ctx, "[volume] attach workflow volumes for %s failed: %v", clawID[:8], err)
			s.releaseWorkflowVolumeLeases(clawID)
			go s.stopAgentWithReason(clawID, fmt.Sprintf("Workflow volume attach failed: %v", err), false)
			return
		}

		cc.Mu.Lock()
		cc.WorkflowStartPending = false
		cc.WorkflowStartDone = true
		cc.Mu.Unlock()

		if s.initializePipelineEntryIfNeeded(clawID) {
			s.sendInitialPlanInstruction(cc, clawID)
		} else if s.getPipelineStage(clawID) == "" && !s.clawHasMessages(clawID) {
			s.sendWakeMessage(cc, clawID)
		}
	}()
}

func daytonaRepoReadinessSnippet(repoFullName string) string {
	repoName := repoDirectoryName(repoFullName)
	return fmt.Sprintf("echo %s; [ -d %s/.git ] || { echo %s; exit 1; }; echo %s; ",
		shellQuote("[daytona] verifying "+repoName),
		shellQuote(repoName),
		shellQuote("[daytona] verify FAILED: "+repoName+"/.git missing"),
		shellQuote("[daytona] verify OK: "+repoName),
	)
}

func (s *Server) setBootstrapStatusWithDiagnostic(clawID, status, diagnostic string) {
	if clawID == "" {
		return
	}
	res, err := s.db.Exec(`UPDATE claws SET bootstrap_status=?, bootstrap_diagnostic=? WHERE id=? AND status != 'deleted'`, status, diagnostic, clawID)
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
			"claw_id":              clawID,
			"status":               "starting",
			"bootstrap_status":     status,
			"bootstrap_diagnostic": diagnostic,
		},
	})
}

func daytonaBootstrapStatusForStep(label string) string {
	switch label {
	case "uninstall old openclaw", "install openclaw", "verify openclaw":
		return "Preparing runtime"
	case "install nix", "install docker", "preflight required commands", "stage openclaw plugin deps":
		return "Preparing runtime"
	case "configure openclaw model", "start openclaw gateway":
		return "Configuring OpenClaw"
	case "install git credential helper", "install gh cli", "configure gh token refresh":
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
	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "exedev")
	instance, err := p.Create(createCtx, createReq)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("exedev create: %w", err)
	}
	logfCtx(ctx, "exedev VM created: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='exedev', provider_id=? WHERE id=?`, instance.ID, clawID)

	// Bootstrap asynchronously
	go func() {
		if err := s.bootstrapExedev(context.Background(), clawID, instance.ID, p, files); err != nil {
			logfCtx(ctx, "exedev bootstrap failed for claw %s: %v", clawID, err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Exedev bootstrap failed: %s", sanitizeBootstrapError(err)), false)
		}
	}()

	return nil
}

func (s *Server) bootstrapExedev(ctx context.Context, clawID, vmName string, p *exedevProvider.Provider, files map[string][]byte) error {
	logfCtx(ctx, "[exedev] bootstrapping claw %s (vm %s)", clawID, vmName)
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
	templateFiles = workspaceTemplateFiles(templateFiles)

	s.mu.RLock()
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.mu.RUnlock()

	linearToken := resolveLinearToken(hubCfg, linearWorkspace)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = hubCfg.DefaultModel
	}
	logfCtx(ctx, "[exedev bootstrap] claw %.8s nix=%d docker=%d llm_key=%q template_default_model=%q hub_default_model=%q resolved_default_model=%q",
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
		ModelAuthEnv:    modelAuthEnv,
		APIKeyAuthSync:  buildOpenClawAPIKeyAuthSyncShell(hubCfg.LLMKeys, llmKeyName),
		LinearEnv:       buildLinearEnv(linearToken),
		ProviderConfig:  buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName),
		OnboardFlags:    buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel),
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
	logfCtx(ctx, "[exedev] bootstrap script completed on %s", vmName)
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
	if credHelper := buildGitHubCredentialHelper(hubCfg, s.clawHubURL(), clawID, githubRepos); credHelper != "# GitHub App not configured — skipping credential helper" {
		if err := p.SetupScript(ctx, vmName, credHelper); err != nil {
			return fmt.Errorf("configure GitHub credentials and repo instructions: %w", err)
		}
		logfCtx(ctx, "[exedev] GitHub credential helper and repo instruction discovery completed for claw %.8s", clawID)
	}
	s.markBootstrapReady(clawID)

	logfCtx(ctx, "[exedev] bootstrap complete for claw %.8s on %s", clawID, vmName)
	return nil
}

func (s *Server) provisionDocker(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error {
	p, err := newDockerProvider(cfg)
	if err != nil {
		return fmt.Errorf("docker init: %w", err)
	}

	// Load claw configuration from DB
	var clawName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,'') FROM claws WHERE id=?`,
		clawID,
	).Scan(&clawName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName); err != nil {
		return fmt.Errorf("load claw config: %w", err)
	}

	s.mu.RLock()
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.mu.RUnlock()

	linearToken := resolveLinearToken(hubCfg, linearWorkspace)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = hubCfg.DefaultModel
	}

	gatewayPassword := randomHex(16)
	providerConfig := buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName)
	apiKeyAuthSync := buildOpenClawAPIKeyAuthSyncShell(hubCfg.LLMKeys, llmKeyName)
	onboardFlags := buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel)

	// Build env map for the container — passed directly as -e flags (no shell escaping needed)
	containerEnv := map[string]string{
		"ELASTICCLAW_HUB_URL":            dockerClawHubURL(hubCfg),
		"ELASTICCLAW_CLAW_ID":            clawID,
		"ELASTICCLAW_CLAW_TOKEN":         clawToken,
		"ELASTICCLAW_CLAW_NAME":          clawName,
		"ELASTICCLAW_GITHUB_REPOS":       githubReposJSON,
		"ELASTICCLAW_BOOTSTRAP":          "1",
		"ELASTICCLAW_WAIT_FOR_WORKSPACE": "1",
		"ELASTICCLAW_GATEWAY_PASSWORD":   gatewayPassword,
		"OPENCLAW_GATEWAY_PASSWORD":      gatewayPassword,
		"OPENCLAW_DEFAULT_MODEL":         defaultModel,
		"ELASTICCLAW_NIX":                boolEnv(nixEnabled != 0),
		"ELASTICCLAW_DOCKER":             boolEnv(dockerEnabled != 0),
		"ELASTICCLAW_PROVIDER_CONFIG":    providerConfig,
		"ELASTICCLAW_API_KEY_AUTH_SYNC":  apiKeyAuthSync,
		"ELASTICCLAW_ONBOARD_FLAGS":      onboardFlags,
	}

	// Inject LLM keys: buildLLMKeyEnv returns "export VAR=val\n" lines — parse into k/v
	for _, line := range strings.Split(llmKeyEnv+modelAuthEnv, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "export ") {
			continue
		}
		kv := strings.TrimPrefix(line, "export ")
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			k := kv[:idx]
			v := kv[idx+1:]
			if unquoted, err := strconv.Unquote(v); err == nil {
				v = unquoted
			} else if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
				v = v[1 : len(v)-1]
			}
			containerEnv[k] = v
		}
	}

	// Inject LINEAR_API_KEY if configured
	if linearToken != "" {
		containerEnv["LINEAR_API_KEY"] = linearToken
	}

	createReq := types.CreateRequest{
		Name: req.ProviderName,
		Env:  containerEnv,
	}

	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "docker")
	instance, err := p.Create(createCtx, createReq)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	logfCtx(ctx, "[docker] container started: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='docker', provider_id=? WHERE id=?`, instance.ID, clawID)
	homeDir, err := p.HomeDir(ctx, instance.ID)
	if err != nil {
		_ = p.Destroy(context.Background(), instance.ID, false)
		return fmt.Errorf("docker home dir: %w", err)
	}
	workspaceDir := path.Join(homeDir, "workspace")
	workspacePrefix := strings.TrimRight(workspaceDir, "/") + "/"

	s.setBootstrapStatus(clawID, "Copying workspace files")
	for relPath, content := range files {
		dest := path.Join(workspaceDir, relPath)
		if dest != workspaceDir && !strings.HasPrefix(dest, workspacePrefix) {
			_ = p.Destroy(context.Background(), instance.ID, false)
			return fmt.Errorf("docker workspace file path escapes workspace: %s", relPath)
		}
		if err := p.CopyIn(ctx, instance.ID, dest, content); err != nil {
			_ = p.Destroy(context.Background(), instance.ID, false)
			return fmt.Errorf("docker file copy failed: %s: %w", relPath, err)
		}
	}
	if err := p.CopyIn(ctx, instance.ID, path.Join(workspaceDir, ".elasticclaw-workspace-ready"), []byte("ready\n")); err != nil {
		_ = p.Destroy(context.Background(), instance.ID, false)
		return fmt.Errorf("docker workspace ready marker: %w", err)
	}
	logfCtx(ctx, "[docker] workspace files copied for claw %.8s to %s", clawID, workspaceDir)
	s.setBootstrapStatus(clawID, "Starting agent bridge")
	if err := s.ensureDockerBridge(ctx, p, instance.ID, homeDir); err != nil {
		_ = p.Destroy(context.Background(), instance.ID, false)
		return err
	}

	return nil
}

func dockerClawHubURL(cfg *types.HubConfig) string {
	if cfg == nil {
		return ""
	}
	hubURL := cfg.PublicURL
	if cfg.URL != "" {
		hubURL = cfg.URL
	}
	parsed, err := url.Parse(hubURL)
	if err != nil || parsed.Hostname() == "" {
		return strings.TrimRight(hubURL, "/")
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "0.0.0.0", "::1":
		port := parsed.Port()
		parsed.Host = "host.docker.internal"
		if port != "" {
			parsed.Host += ":" + port
		}
		return strings.TrimRight(parsed.String(), "/")
	default:
		return strings.TrimRight(hubURL, "/")
	}
}

const maxDockerBridgeBinaryBytes = 200 << 20

func (s *Server) ensureDockerBridge(ctx context.Context, p interface {
	CopyIn(context.Context, string, string, []byte) error
	Exec(context.Context, string, []string) (*types.ExecResult, error)
}, containerID, homeDir string) error {
	if _, err := p.Exec(ctx, containerID, []string{"sh", "-lc", "command -v pgrep >/dev/null 2>&1 && pgrep -x claw-bridge >/dev/null"}); err == nil {
		logfCtx(ctx, "[docker] claw-bridge already running in container %s", containerID)
		return nil
	}

	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		return fmt.Errorf("claw-bridge URL not configured: set bridge_image in hub.yaml or build a tagged release")
	}
	if !strings.HasPrefix(bridgeURL, "http://") && !strings.HasPrefix(bridgeURL, "https://") {
		return fmt.Errorf("docker provider requires an HTTP(S) claw-bridge URL, got %q", bridgeURL)
	}
	bridgeBytes, err := downloadDockerBridgeBinary(ctx, bridgeURL)
	if err != nil {
		return err
	}
	bridgePath := path.Join(homeDir, ".elasticclaw", "bin", "claw-bridge")
	if err := p.CopyIn(ctx, containerID, bridgePath, bridgeBytes); err != nil {
		return fmt.Errorf("docker claw-bridge copy failed: %w", err)
	}
	logPath := path.Join(homeDir, "claw-bridge.log")
	startCmd := fmt.Sprintf(
		"set -e; chmod 0755 %s; nohup %s >> %s 2>&1 </dev/null & echo started",
		shellQuote(bridgePath),
		shellQuote(bridgePath),
		shellQuote(logPath),
	)
	if _, err := p.Exec(ctx, containerID, []string{"sh", "-lc", startCmd}); err != nil {
		return fmt.Errorf("docker claw-bridge start failed: %w", err)
	}
	logfCtx(ctx, "[docker] claw-bridge started in container %s", containerID)
	return nil
}

func downloadDockerBridgeBinary(ctx context.Context, bridgeURL string) ([]byte, error) {
	if bridgePath := os.Getenv("ELASTICCLAW_E2E_BRIDGE_BINARY"); bridgePath != "" && strings.Contains(bridgeURL, "/__elasticclaw_e2e/claw-bridge-linux-amd64") {
		data, err := os.ReadFile(bridgePath)
		if err != nil {
			return nil, fmt.Errorf("docker claw-bridge read local E2E binary: %w", err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("docker claw-bridge local E2E binary is empty")
		}
		if len(data) > maxDockerBridgeBinaryBytes {
			return nil, fmt.Errorf("docker claw-bridge local E2E binary exceeds %d bytes", maxDockerBridgeBinaryBytes)
		}
		return data, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bridgeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("docker claw-bridge download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker claw-bridge download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("docker claw-bridge download failed: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDockerBridgeBinaryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("docker claw-bridge read: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("docker claw-bridge download returned an empty body")
	}
	if len(data) > maxDockerBridgeBinaryBytes {
		return nil, fmt.Errorf("docker claw-bridge download exceeds %d bytes", maxDockerBridgeBinaryBytes)
	}
	return data, nil
}

func (s *Server) provisionLambdaMicroVMs(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error {
	p, err := newLambdaMicroVMsProvider(cfg)
	if err != nil {
		return fmt.Errorf("lambda microvms init: %w", err)
	}

	var clawName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,'') FROM claws WHERE id=?`,
		clawID,
	).Scan(&clawName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName); err != nil {
		return fmt.Errorf("load claw config: %w", err)
	}

	s.mu.RLock()
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.mu.RUnlock()

	linearToken := resolveLinearToken(hubCfg, linearWorkspace)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = hubCfg.DefaultModel
	}
	providerConfig := buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName)
	apiKeyAuthSync := buildOpenClawAPIKeyAuthSyncShell(hubCfg.LLMKeys, llmKeyName)
	onboardFlags := buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel)
	gatewayPassword := randomHex(16)

	env := map[string]string{
		"ELASTICCLAW_HUB_URL":            s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":            clawID,
		"ELASTICCLAW_CLAW_TOKEN":         clawToken,
		"ELASTICCLAW_CLAW_NAME":          clawName,
		"ELASTICCLAW_GITHUB_REPOS":       githubReposJSON,
		"ELASTICCLAW_BOOTSTRAP":          "1",
		"ELASTICCLAW_WAIT_FOR_WORKSPACE": "1",
		"ELASTICCLAW_GATEWAY_PASSWORD":   gatewayPassword,
		"OPENCLAW_GATEWAY_PASSWORD":      gatewayPassword,
		"OPENCLAW_DEFAULT_MODEL":         defaultModel,
		"ELASTICCLAW_NIX":                boolEnv(nixEnabled != 0),
		"ELASTICCLAW_DOCKER":             boolEnv(dockerEnabled != 0),
		"ELASTICCLAW_PROVIDER_CONFIG":    providerConfig,
		"ELASTICCLAW_API_KEY_AUTH_SYNC":  apiKeyAuthSync,
		"ELASTICCLAW_ONBOARD_FLAGS":      onboardFlags,
	}
	for _, line := range strings.Split(llmKeyEnv+modelAuthEnv, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "export ") {
			continue
		}
		kv := strings.TrimPrefix(line, "export ")
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			k := kv[:idx]
			v := kv[idx+1:]
			if unquoted, err := strconv.Unquote(v); err == nil {
				v = unquoted
			} else if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
				v = v[1 : len(v)-1]
			}
			env[k] = v
		}
	}
	if linearToken != "" {
		env["LINEAR_API_KEY"] = linearToken
	}
	for k, v := range req.Env {
		if _, exists := env[k]; !exists {
			env[k] = v
		}
	}

	createReq := types.CreateRequest{
		Name:          req.ProviderName,
		Env:           env,
		TemplateFiles: files,
	}
	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "lambda-microvms")
	instance, err := p.Create(createCtx, createReq)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("lambda microvms create: %w", err)
	}
	logfCtx(ctx, "[lambda-microvms] microvm started: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='lambda-microvms', provider_id=? WHERE id=?`, instance.ID, clawID)
	return nil
}

// boolEnv converts a bool to "true"/"false" for environment variable injection.
func boolEnv(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func (s *Server) provisionReplicated(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, env map[string]string) error {
	// Hub's generated key is always included; append any extra debug keys from hub config.
	cfg.SSHPublicKey = s.identity.PublicKey
	cfg.ExtraSSHPublicKeys = s.hubCfg.SSHPublicKeys
	p, err := newReplicatedProvider(cfg)
	if err != nil {
		return fmt.Errorf("replicated init: %w", err)
	}

	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "replicated")
	vmID, err := p.ProvisionClaw(createCtx, replicatedpkg.VMCreateRequest{
		Name:         req.ProviderName, // stable ec-<shortid>
		InstanceType: req.InstanceType,
		TTL:          req.TTL,
	}, nil, nil)
	endSpan(err)
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
		logfCtx(ctx, "[provision] claw %s deleted mid-provision, destroying VM %s", clawID[:8], vmID)
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

	logfCtx(ctx, "Replicated VM provisioned")
	logfCtx(ctx, "  Claw:          %s (%s)", req.Name, clawID)
	logfCtx(ctx, "  VM ID:         %s", vmID)
	logfCtx(ctx, "  Instance type: %s", instanceType)
	logfCtx(ctx, "  TTL:           %s", ttl)
	logfCtx(ctx, "  SSH:           ssh %s", replicatedpkg.VMHostname(vmID))
	logfCtx(ctx, "  Status:        provisioning (waiting for VM to start)")
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
		logf("keepAliveDaytonaSandboxes: query error: %v", err)
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
		logf("keepAliveDaytonaSandboxes: no daytona provider configured")
		return
	}
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		logf("keepAliveDaytonaSandboxes: provider init error: %v", err)
		return
	}

	for _, c := range claws {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := p.ExecWithTimeout(ctx, c.providerID, []string{"bash", "-lc", "true"}, 20*time.Second)
		cancel()
		if err != nil {
			logf("[daytona] keepalive failed for %s (%s): %v", c.name, c.id[:8], err)
			continue
		}
		logf("[daytona] keepalive ok for %s (%s)", c.name, c.id[:8])
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
		logf("pollProviderStatus: query error: %v", err)
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
		logf("pollProviderStatus: provider init error: %v", err)
		return
	}

	for _, c := range pending {
		vm, err := p.GetVM(context.Background(), c.providerID)
		if err != nil {
			// 404 means VM was deleted externally — clean up the claw
			if strings.Contains(err.Error(), "HTTP 404") {
				logf("pollProviderStatus: VM %s not found (404) — marking claw %s offline", c.providerID, c.id[:8])
				res, execErr := s.db.Exec(
					`UPDATE claws SET status='offline' WHERE id=? AND status IN ('provisioning','starting')`,
					c.id)
				if execErr == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						s.mu.Lock()
						if cc, ok := s.claws[c.id]; ok {
							cc.WS.Close(websocket.StatusGoingAway, "VM not found")
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
				logf("pollProviderStatus: get VM %s error: %v", c.providerID, err)
			}
			continue
		}
		// Only log if status changed or there's a problem
		if vm.Status != c.status && vm.Status != "running" {
			logf("Claw %s (%s): VM %s %s → %s", c.name, c.id[:8], c.providerID, c.status, vm.Status)
		}

		// Map Replicated VM status to claw status
		var newStatus string
		switch vm.Status {
		case "running":
			newStatus = "starting"
			// First time we see running — trigger bootstrap
			if c.status == "provisioning" {
				logf("Claw %s (%s): VM running, bootstrapping...", c.name, c.id[:8])
				go s.bootstrapReplicated(c.id, c.name, c.providerID, replicatedCfg)
			}
		case "terminated", "error":
			logf("Replicated VM %s for claw %s (%s) terminated", c.providerID, c.name, c.id)
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
					logf("Claw %s (%s): VM %s %s → hub status %s",
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
	wakeMessageMarker               = "__WAKE_MESSAGE__"
	initialPlanRequiredMarker       = "__INITIAL_PLAN_REQUIRED__"
	initialPlanAcceptedMarker       = "__INITIAL_PLAN_ACCEPTED__"
	initialPlanCorrectionSentMarker = "__INITIAL_PLAN_CORRECTION_SENT__"
	defaultWakeContent              = "Introduce yourself briefly and let the user know you're ready to help."
	initialPlanWakeContent          = `Initial plan required before implementation.

Before editing files, running builds, or doing broad tool exploration, send one visible assistant message that contains:
1. Your understanding of the issue or task.
2. The likely area of the codebase or behavior involved.
3. A rough implementation plan.
4. What you will verify or test.

This first message must be a normal assistant message visible to the user. Tool calls, activity rows, and update_plan do not count. After that visible plan, wait for the hub's proceed message, then start implementation and continue sending visible progress updates.`
	initialPlanProceedContent    = `[hub] Initial plan received. Proceed with implementation. Keep sending visible progress updates before and after substantial work; tool calls and activity rows do not count as user communication.`
	initialPlanCorrectionContent = `[hub] Initial plan is required before implementation. Pause tool work and send a visible assistant message with your understanding of the issue, likely code area, rough plan, and verification approach.`
)

// sendWakeMessage sends a silent system message to wake the agent.
// For factory claws, it sends a task-specific prompt.
// A marker is stored in DB so reconnects after hub restart don't re-introduce.
func (s *Server) sendWakeMessage(cc *clawConn, clawID string) {
	wakeContent := defaultWakeContent
	if s.clawNeedsInitialPlan(clawID) {
		wakeContent = initialPlanWakeContent
		_ = s.insertSystemMarker(clawID, cc.TenantID, initialPlanRequiredMarker)
	}
	wakeMsg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    clawID,
		TenantID:  cc.TenantID,
		Role:      "system",
		Content:   wakeMessageMarker,
		CreatedAt: now(),
	}
	_, _ = s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		wakeMsg.ID, wakeMsg.ClawID, wakeMsg.TenantID, wakeMsg.Role, wakeMsg.Content, wakeMsg.CreatedAt,
	)
	wakeMsg.Content = wakeContent
	_ = wsjson.Write(context.Background(), cc.WS, types.WSMessage{Type: "message", Payload: wakeMsg})

	// Note: We don't call sendNextQueuedMessage here because sendWakeMessage is launched
	// with 'go' (asynchronously). The normal end-of-turn path in handleClawWS read loop
	// will drain the queue once the claw finishes the wake response. This prevents race
	// conditions where both goroutines try to dequeue messages concurrently.
}

func (s *Server) sendInitialPlanInstruction(cc *clawConn, clawID string) {
	if cc == nil || !s.clawNeedsInitialPlan(clawID) || s.hasSystemMarker(clawID, initialPlanAcceptedMarker) {
		return
	}
	if !s.insertSystemMarker(clawID, cc.TenantID, initialPlanRequiredMarker) {
		return
	}
	msg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    clawID,
		TenantID:  cc.TenantID,
		Role:      "system",
		Content:   initialPlanWakeContent,
		CreatedAt: now(),
	}
	_ = wsjson.Write(context.Background(), cc.WS, types.WSMessage{Type: "message", Payload: msg})
}

func (s *Server) clawNeedsInitialPlan(clawID string) bool {
	issueID, tags := s.clawIssueAndTags(clawID)
	if issueID != "" {
		return true
	}
	for _, tag := range tags {
		if strings.HasPrefix(tag, "factory:") || strings.HasPrefix(tag, "workflow:") {
			return true
		}
	}
	return false
}

func (s *Server) tenantIDForClaw(clawID string) string {
	var tenantID string
	_ = s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&tenantID)
	return tenantID
}

func (s *Server) hasSystemMarker(clawID, marker string) bool {
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='system' AND content=?`, clawID, marker).Scan(&count)
	return count > 0
}

func (s *Server) insertSystemMarker(clawID, tenantID, marker string) bool {
	if clawID == "" || tenantID == "" || marker == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasSystemMarker(clawID, marker) {
		return false
	}
	res, err := s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		uuid.New().String(), clawID, tenantID, "system", marker, now(),
	)
	if err != nil {
		return false
	}
	rows, _ := res.RowsAffected()
	return rows > 0
}

func (s *Server) handleInitialPlanResponse(clawID, tenantID, content string) {
	if !s.hasSystemMarker(clawID, initialPlanRequiredMarker) || s.hasSystemMarker(clawID, initialPlanAcceptedMarker) {
		return
	}
	if isValidInitialPlan(content) {
		_ = s.insertSystemMarker(clawID, tenantID, initialPlanAcceptedMarker)
		s.injectHubMessageByID(clawID, initialPlanProceedContent)
		return
	}
	if !s.hasSystemMarker(clawID, initialPlanCorrectionSentMarker) {
		_ = s.insertSystemMarker(clawID, tenantID, initialPlanCorrectionSentMarker)
		s.injectHubMessageByID(clawID, initialPlanCorrectionContent)
	}
}

func (s *Server) handleInitialPlanActivity(clawID, tenantID string, activity map[string]interface{}) {
	if !s.hasSystemMarker(clawID, initialPlanRequiredMarker) ||
		s.hasSystemMarker(clawID, initialPlanAcceptedMarker) ||
		s.hasSystemMarker(clawID, initialPlanCorrectionSentMarker) {
		return
	}
	kind, _ := activity["kind"].(string)
	if kind != "tool" {
		return
	}
	_ = s.insertSystemMarker(clawID, tenantID, initialPlanCorrectionSentMarker)
	s.injectHubMessageByID(clawID, initialPlanCorrectionContent)
}

func isValidInitialPlan(content string) bool {
	content = strings.TrimSpace(content)
	if len(content) < 120 || len(strings.Fields(content)) < 35 {
		return false
	}
	lower := strings.ToLower(content)
	hasUnderstanding := strings.Contains(lower, "understand") ||
		strings.Contains(lower, "issue") ||
		strings.Contains(lower, "task") ||
		strings.Contains(lower, "problem")
	hasPlan := strings.Contains(lower, "plan") ||
		strings.Contains(lower, "step") ||
		strings.Contains(lower, "approach")
	hasVerification := strings.Contains(lower, "test") ||
		strings.Contains(lower, "verify") ||
		strings.Contains(lower, "check") ||
		strings.Contains(lower, "build")
	hasCodeArea := strings.Contains(lower, "file") ||
		strings.Contains(lower, "code") ||
		strings.Contains(lower, "package") ||
		strings.Contains(lower, "component") ||
		strings.Contains(lower, "backend") ||
		strings.Contains(lower, "frontend")
	return hasUnderstanding && hasPlan && hasVerification && hasCodeArea
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
		logf("[bootstrap] claw %s deleted before bootstrap, destroying VM %s", clawID[:8], vmID)
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
		logf("[bootstrap] warning: could not read nix flag for claw %s: %v", clawID[:8], err)
	}
	var dockerEnabled int
	if err := s.db.QueryRow(`SELECT docker FROM claws WHERE id=?`, clawID).Scan(&dockerEnabled); err != nil {
		logf("[bootstrap] warning: could not read docker flag for claw %s: %v", clawID[:8], err)
	}
	logf("[bootstrap] claw %s nix=%d docker=%d", clawID[:8], nixEnabled, dockerEnabled)
	// Read llm_key selection
	var llmKeyName string
	_ = s.db.QueryRow(`SELECT COALESCE(llm_key,'') FROM claws WHERE id=?`, clawID).Scan(&llmKeyName)
	defaultModel, llmKeyName = resolveModelAndLLMKey(s.hubCfg, llmKeyName, defaultModel)
	logf("[bootstrap] OpenClaw model resolution claw=%s llm_key=%q template_default_model=%q hub_default_model=%q resolved_default_model=%q",
		clawID[:8], llmKeyName, templateDefaultModel, s.hubCfg.DefaultModel, defaultModel)

	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		logf("[bootstrap] ERROR: bridge_image not set and hub version is 'dev' — set bridge_image in hub.yaml")
		s.stopAgentWithReason(clawID, "Bootstrap failed: bridge_image not configured", false)
		return
	}
	s.setBootstrapStatus(clawID, "Waiting for sandbox SSH")

	// Get the direct SSH endpoint from Replicated (IP:port, user is always root)
	cp, err := newReplicatedProvider(cfg)
	if err != nil {
		logf("bootstrap: provider init error: %v", err)
		return
	}
	vm, err := cp.GetVM(context.Background(), vmID)
	if err != nil || vm.DirectSSHEndpoint == "" || vm.DirectSSHPort == 0 {
		logf("bootstrap: could not get direct SSH endpoint for VM %s: %v", vmID, err)
		return
	}
	// Replicated uses the comment from the SSH public key as the Linux username.
	// Our key comment is "elasticclaw@hub", so the username is "elasticclaw".
	sshUser := replicatedpkg.SSHUserFromPublicKey(s.identity.PublicKey)
	sshHome, err := sshHomeDir(sshUser)
	if err != nil {
		logf("bootstrap: invalid SSH user %q: %v", sshUser, err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: invalid SSH user: %s", sanitizeBootstrapError(err)), false)
		return
	}
	sshHost := fmt.Sprintf("%s:%d", vm.DirectSSHEndpoint, vm.DirectSSHPort)
	replicatedSSHDelays := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
	}
	logf("Bootstrap SSH: %s@%s", sshUser, sshHost)
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
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
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
		ModelAuthEnv:    modelAuthEnv,
		APIKeyAuthSync:  buildOpenClawAPIKeyAuthSyncShell(hubCfg.LLMKeys, llmKeyName),
		LinearEnv:       buildLinearEnv(linearToken),
		ProviderConfig:  buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName),
		OnboardFlags:    buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel),
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
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Staging Nix flake",
			RetryLabel: "Retrying Nix flake staging",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshWriteFiles(sshUser, sshHost, path.Join(sshHome, "workspace"), flakeFiles)
			},
		}); err != nil {
			logf("[bootstrap] failed to stage flake before bootstrap for claw %s: %v", clawID[:8], err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not stage flake files: %s", err), false)
			return
		}
	}

	// Run bootstrap script first — this installs OpenClaw and initializes the workspace.
	// Template files must be written AFTER the script completes so openclaw onboard
	// doesn't overwrite BOOTSTRAP.md and other workspace files.
	if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
		Label:      "Preparing ElasticClaw connector",
		RetryLabel: "Retrying sandbox bootstrap",
		Attempts:   5,
		Delays:     []time.Duration{10 * time.Second},
		Run: func() error {
			return s.sshRun(sshUser, sshHost, script)
		},
	}); err != nil {
		logf("Bootstrap failed for claw %s: %v", clawID, err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: %s", err), false)
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
		sort.Strings(fileNames)
		logf("[bootstrap] writing %d template files for claw %s: %v", len(files), clawName, fileNames)
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Writing workspace files",
			RetryLabel: "Retrying workspace file write",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshWriteFiles(sshUser, sshHost, path.Join(sshHome, ".openclaw", "workspace"), files)
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not write workspace files: %s", err), false)
			return
		}
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Verifying workspace files",
			RetryLabel: "Retrying workspace file verification",
			Attempts:   3,
			Delays:     []time.Duration{2 * time.Second, 5 * time.Second},
			Run: func() error {
				return s.sshRun(sshUser, sshHost, replicatedWorkspaceReadinessCommand(path.Join(sshHome, ".openclaw", "workspace"), files))
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: workspace files incomplete: %s", err), false)
			return
		}
		logf("Template files written for claw %s", clawName)
	}

	if err := s.restoreCheckpointToSSH(clawID, sshUser, sshHost); err != nil {
		logf("[bootstrap] restore checkpoint failed: %v", err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore checkpoint failed: %s", sanitizeBootstrapError(err)), false)
		return
	}

	// Run GitHub credential helper setup (needs bridge connected for hub proxy,
	// but the hub token URL is publicly accessible so it works directly).
	if credHelper := buildGitHubCredentialHelper(hubCfg, s.clawHubURL(), clawID, githubRepos); credHelper != "# GitHub App not configured — skipping credential helper" {
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Configuring GitHub credentials",
			RetryLabel: "Retrying GitHub credential setup",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshRun(sshUser, sshHost, credHelper)
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not configure GitHub credentials: %s", err), false)
			return
		}
		logf("[bootstrap] GitHub credential helper installed for claw %s", clawName)
	}
	s.markBootstrapReady(clawID)

	logf("Bootstrap complete for claw %s (%s)", clawName, clawID[:8])
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
			if k.Name == selectedKeyName && llmKeyHasRequiredAPIKey(k) {
				envVar := k.EnvVarName()
				seen[envVar] = true
				fmt.Fprintf(&b, "export %s=%q\n", envVar, k.APIKey)
				break
			}
		}
	}

	// Second pass: export default keys for providers not yet seen
	for _, k := range keys {
		if !k.Default || !llmKeyHasRequiredAPIKey(k) {
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
		if !llmKeyHasRequiredAPIKey(k) {
			continue
		}
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
	case "codex":
		return "codex/gpt-5.5"
	case "grok":
		return "grok/grok-build-0.1"
	case "fireworks":
		return defaultFireworksModel
	case "groq":
		return "groq/llama-3.3-70b-versatile"
	case "deepseek":
		return "deepseek/deepseek-chat"
	case "ollama":
		return "ollama/qwen2.5-coder:1.5b"
	case "moonshot":
		return "moonshot/moonshot-v1-8k"
	default:
		// Fall back to hub default even if provider doesn't match
		return hubCfg.DefaultModel
	}
}

func resolveModelAndLLMKey(hubCfg *types.HubConfig, selectedKeyName, defaultModel string) (string, string) {
	if hubCfg == nil {
		return defaultModel, selectedKeyName
	}
	resolvedModel := defaultModel
	resolvedKeyName := selectedKeyName
	if resolvedModel == "" {
		activeKey := resolveActiveKey(hubCfg.LLMKeys, selectedKeyName)
		if activeKey != nil {
			if resolvedKeyName == "" || resolvedKeyName != activeKey.Name {
				resolvedKeyName = activeKey.Name
			}
			resolvedModel = resolveDefaultModelForKey(hubCfg, activeKey)
		}
	}
	if resolvedModel == "" {
		resolvedModel = hubCfg.DefaultModel
	}
	return resolvedModel, resolvedKeyName
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

func buildGitHubTokenProfileScript() string {
	return `# ElasticClaw GitHub App token refresh for gh.
# This intentionally resolves through the credential helper instead of storing
# the short-lived installation token generated during bootstrap.
if [ -x /usr/local/bin/elasticclaw-git-credentials ]; then
  token="$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | sed -n 's/^password=//p' | head -n1)"
  if [ -n "$token" ]; then
    export GH_TOKEN="$token"
  else
    unset GH_TOKEN
  fi
  unset token
fi
`
}

func buildGitHubTokenProfileInstallScript() string {
	return `sudo tee /etc/profile.d/elasticclaw-github.sh > /dev/null << 'PROFILEEOF'
` + buildGitHubTokenProfileScript() + `PROFILEEOF
sudo chmod +x /etc/profile.d/elasticclaw-github.sh
[ -s /etc/profile.d/elasticclaw-github.sh ] || exit 1`
}

func buildGitHubCLIWrapperInstallScript() string {
	return `if command -v gh >/dev/null 2>&1; then
  REAL_GH="$(command -v gh)"
  if [ "$REAL_GH" = "/usr/local/bin/gh" ]; then
    if grep -q "ElasticClaw GitHub App token refresh wrapper" /usr/local/bin/gh 2>/dev/null; then
      echo "GitHub gh wrapper already configured"
      REAL_GH=""
    elif [ -x /usr/local/bin/gh.elasticclaw-real ]; then
      REAL_GH="/usr/local/bin/gh.elasticclaw-real"
    else
      sudo mv /usr/local/bin/gh /usr/local/bin/gh.elasticclaw-real
      REAL_GH="/usr/local/bin/gh.elasticclaw-real"
    fi
  fi
  if [ -n "$REAL_GH" ] && [ -x "$REAL_GH" ]; then
    sudo tee /usr/local/bin/gh > /dev/null << 'GHEOF'
#!/bin/bash
# ElasticClaw GitHub App token refresh wrapper.
set +x
REAL_GH="__ELASTICCLAW_REAL_GH__"
if [ -x /usr/local/bin/elasticclaw-git-credentials ]; then
  token="$(/usr/local/bin/elasticclaw-git-credentials 2>/dev/null | sed -n 's/^password=//p' | head -n1)"
  if [ -n "$token" ]; then
    export GH_TOKEN="$token"
  fi
  unset token
fi
exec "$REAL_GH" "$@"
GHEOF
    REAL_GH_ESCAPED="$(printf '%s' "$REAL_GH" | sed 's/[&\\|]/\\&/g')"
    sudo sed -i "s|__ELASTICCLAW_REAL_GH__|$REAL_GH_ESCAPED|g" /usr/local/bin/gh
    sudo chmod +x /usr/local/bin/gh
    echo "GitHub gh wrapper configured"
  fi
fi`
}

func buildDaytonaGitHubCloneScript(repos []types.GitHubRepoAccess) string {
	var b strings.Builder
	b.WriteString("export HOME=/home/daytona; export GIT_TERMINAL_PROMPT=0; set +x; cd ~/.openclaw/workspace; git config --global --get credential.helper >/dev/null || exit 1; set -o pipefail; ")
	for _, repo := range repos {
		repoName := repoDirectoryName(repo.Repo)
		cloneURL := "https://github.com/" + repo.Repo + ".git"
		fmt.Fprintf(&b, "echo %s; if [ ! -d %s ]; then git clone %s %s || { echo %s; exit 1; }; echo %s; else git -C %s remote set-url origin %s || true; git -C %s pull --ff-only || { echo %s; exit 1; }; echo %s; fi; ",
			shellQuote(fmt.Sprintf("[daytona] cloning %s into %s", repo.Repo, repoName)),
			shellQuote(repoName),
			shellQuote(cloneURL),
			shellQuote(repoName),
			shellQuote("[daytona] clone FAILED: "+repo.Repo),
			shellQuote("[daytona] clone OK: "+repo.Repo),
			shellQuote(repoName),
			shellQuote(cloneURL),
			shellQuote(repoName),
			shellQuote("[daytona] pull FAILED: "+repo.Repo),
			shellQuote("[daytona] pull OK: "+repo.Repo),
		)
	}
	return b.String()
}

var repoInstructionFileNames = []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md"}

const repoInstructionsIndexName = "REPO_INSTRUCTIONS.md"

const repoEnvironmentIndexName = "REPO_ENVIRONMENT.md"

const repoInstructionsAgentsSection = `## Repository Instructions

If ` + "`REPO_INSTRUCTIONS.md`" + ` exists, read it before working inside any cloned repository. It lists repository-owned instruction files such as ` + "`AGENTS.md`" + `, ` + "`CLAUDE.md`" + `, and ` + "`GEMINI.md`" + `.`

const repoEnvironmentAgentsSection = `## Repository Environments

If ` + "`REPO_ENVIRONMENT.md`" + ` exists, read it before running commands inside cloned repositories. Repositories with ` + "`flake.nix`" + ` should run repo-local commands with that repository's own Nix development shell, for example ` + "`cd <repo> && nix develop --accept-flake-config -c <command>`" + `.`

func buildRepoInstructionDiscoveryScript(workspaceDir string, repos []types.GitHubRepoAccess) string {
	if len(repos) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, `set -euo pipefail
WORKSPACE_DIR=%s
mkdir -p "$WORKSPACE_DIR"
cd "$WORKSPACE_DIR"

TMP="$(mktemp "$WORKSPACE_DIR/.repo-instructions.XXXXXX")"
FOUND=0
{
  printf '%%s\n\n' '# Repository Instructions'
  printf '%%s\n\n' 'ElasticClaw detected repository-owned agent instruction files. Read the relevant files before making changes in that repository.'
`, shellDoubleQuote(workspaceDir))
	for _, repo := range repos {
		repoName := repoDirectoryName(repo.Repo)
		fmt.Fprintf(&b, `  REPO_DIR=%s
  REPO_FOUND=0
  if [ -d "$REPO_DIR" ]; then
`, shellQuote(repoName))
		for _, fileName := range repoInstructionFileNames {
			repoPath := repoName + "/" + fileName
			fmt.Fprintf(&b, "    if [ -f \"$REPO_DIR/%s\" ]; then\n", fileName)
			fmt.Fprintf(&b, "      if [ \"$REPO_FOUND\" -eq 0 ]; then\n")
			fmt.Fprintf(&b, "        printf '\\n## %%s\\n\\n' %s\n", shellQuote(repoName))
			fmt.Fprintf(&b, "        REPO_FOUND=1\n")
			fmt.Fprintf(&b, "        FOUND=1\n")
			fmt.Fprintf(&b, "      fi\n")
			fmt.Fprintf(&b, "      printf -- '- `%%s`\\n' %s\n", shellQuote(repoPath))
			fmt.Fprintf(&b, "    fi\n")
		}
		b.WriteString("  fi\n")
	}
	fmt.Fprintf(&b, `} > "$TMP"
if [ "$FOUND" -eq 1 ]; then
  mv "$TMP" "$WORKSPACE_DIR/%s"
else
  rm -f "$TMP" "$WORKSPACE_DIR/%s"
fi

ENV_TMP="$(mktemp "$WORKSPACE_DIR/.repo-environment.XXXXXX")"
ENV_FOUND=0
{
  printf '%%s\n\n' '# Repository Environments'
  printf '%%s\n\n' 'ElasticClaw detected repository-local Nix flakes. Run commands for each repository with that repository flake instead of assuming one global project environment.'
  printf '%%s\n\n' 'For one command, use: cd <repo> && nix develop --accept-flake-config -c <command>'
  printf '%%s\n\n' 'For a sequence of commands in one repository, use: cd <repo> && nix develop --accept-flake-config'
`, repoInstructionsIndexName, repoInstructionsIndexName)
	for _, repo := range repos {
		repoName := repoDirectoryName(repo.Repo)
		fmt.Fprintf(&b, `  REPO_DIR=%s
  if [ -f "$REPO_DIR/flake.nix" ]; then
    ENV_FOUND=1
    printf -- '- %s: cd %s && nix develop --accept-flake-config -c <command>\n'
  fi
`, shellQuote(repoName), repoName, repoName)
	}
	fmt.Fprintf(&b, `} > "$ENV_TMP"
if [ "$ENV_FOUND" -eq 1 ]; then
  mv "$ENV_TMP" "$WORKSPACE_DIR/%s"
else
  rm -f "$ENV_TMP" "$WORKSPACE_DIR/%s"
fi

AGENTS_FILE="$WORKSPACE_DIR/AGENTS.md"
SECTION='## Repository Instructions'
if [ ! -f "$AGENTS_FILE" ]; then
  cat > "$AGENTS_FILE" << 'ELASTICCLAW_REPO_AGENTS'
%s
ELASTICCLAW_REPO_AGENTS
elif ! grep -Fqx "$SECTION" "$AGENTS_FILE"; then
  cat >> "$AGENTS_FILE" << 'ELASTICCLAW_REPO_AGENTS'

%s
ELASTICCLAW_REPO_AGENTS
fi

ENV_SECTION='## Repository Environments'
if ! grep -Fqx "$ENV_SECTION" "$AGENTS_FILE"; then
  cat >> "$AGENTS_FILE" << 'ELASTICCLAW_REPO_ENV'

%s
ELASTICCLAW_REPO_ENV
fi
`, repoEnvironmentIndexName, repoEnvironmentIndexName, repoInstructionsAgentsSection, repoInstructionsAgentsSection, repoEnvironmentAgentsSection)
	return b.String()
}

func buildBestEffortRepoInstructionDiscoveryScript(workspaceDir string, repos []types.GitHubRepoAccess) string {
	discoveryScript := buildRepoInstructionDiscoveryScript(workspaceDir, repos)
	if discoveryScript == "" {
		return ""
	}
	return fmt.Sprintf(`(
%s
) || echo "Warning: repo instruction discovery failed; continuing"
`, discoveryScript)
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

# Configure gh to use the credential helper via GH_TOKEN env and wrapper.
if command -v gh &>/dev/null; then
  (
    set +e
%s
%s
  ) || echo "GitHub gh token refresh setup failed, continuing"
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
) || echo "Warning: repo clone failed — agent can retry after bridge connects"
%s`, tokenURL, buildGitHubTokenProfileInstallScript(), buildGitHubCLIWrapperInstallScript(), buildGitHubCloneScript(repos), buildBestEffortRepoInstructionDiscoveryScript("$HOME/.openclaw/workspace", repos))
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
	logf("bootstrap output:\n%s", output)
	return nil
}

// sshRunWithTimeout connects to host via the hub's SSH identity and runs a script.
// A zero timeout waits for the remote command to finish.
func (s *Server) sshRunWithTimeout(user, host, script string, timeout time.Duration) (string, error) {
	pubKeyType := s.identity.PrivateKey.PublicKey().Type()
	pubKeyFP := gossh.FingerprintSHA256(s.identity.PrivateKey.PublicKey())
	logf("SSH attempting: user=%s host=%s key-type=%s fingerprint=%s", user, host, pubKeyType, pubKeyFP)
	logf("SSH public key being used:\n%s", s.identity.PublicKey)

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

func cleanWorkspaceFilePath(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.Contains(trimmed, "\x00") {
		return "", fmt.Errorf("path contains NUL byte")
	}
	cleaned := path.Clean(filepath.ToSlash(trimmed))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("path must stay inside workspace")
	}
	return cleaned, nil
}

func sshHomeDir(user string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return "", fmt.Errorf("empty SSH user")
	}
	if strings.ContainsAny(user, "/\x00") {
		return "", fmt.Errorf("SSH user contains invalid characters")
	}
	if user == "root" {
		return "/root", nil
	}
	return "/home/" + user, nil
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
		safeName, err := cleanWorkspaceFilePath(name)
		if err != nil {
			sess.Close()
			return fmt.Errorf("invalid template file path %q: %w", name, err)
		}
		cmd := remoteWriteFileCommand(dir, safeName, content)
		out, err := sess.CombinedOutput(cmd)
		sess.Close()
		if err != nil {
			return fmt.Errorf("write %s: %w\n%s", name, err, string(out))
		}
	}
	return nil
}

func remoteWriteFileCommand(dir, name, content string) string {
	remotePath := strings.TrimRight(dir, "/") + "/" + name
	remoteDir := path.Dir(remotePath)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	return fmt.Sprintf("mkdir -p -- %s && base64 -d > %s << 'ELASTICCLAW_B64'\n%s\nELASTICCLAW_B64",
		shellDoubleQuote(remoteDir),
		shellDoubleQuote(remotePath),
		encoded,
	)
}

// ─── Terminal WebSocket ───────────────────────────────────────────────────────

// handleTerminal proxies a WebSocket connection to an SSH PTY on the claw's VM.
// Route: GET /api/terminal/{clawID}?token=...
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
	case "docker":
		s.terminateDockerVM(vmID)
	case "lambda-microvms":
		s.terminateLambdaMicroVM(vmID)
	default:
		logf("terminateVM: unsupported provider %q for VM %s", provider, vmID)
	}
}

// terminateDockerVM destroys a Docker agent container by name/ID.
func (s *Server) terminateDockerVM(vmID string) {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["docker"]
	s.mu.RUnlock()
	if !ok {
		logf("terminateDockerVM: no docker provider configured")
		return
	}
	p, err := newDockerProvider(cfg)
	if err != nil {
		logf("terminateDockerVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(context.Background(), "destroy", "docker")
	err = p.Destroy(destroyCtx, vmID, false)
	endSpan(err)
	if err != nil {
		logf("terminateDockerVM: failed to destroy container %s: %v", vmID, err)
		return
	}
	logf("Docker container %s terminated", vmID)
}

// terminateLambdaMicroVM destroys an AWS Lambda MicroVM by ID.
func (s *Server) terminateLambdaMicroVM(vmID string) {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["lambda-microvms"]
	s.mu.RUnlock()
	if !ok {
		logf("terminateLambdaMicroVM: no lambda-microvms provider configured")
		return
	}
	p, err := newLambdaMicroVMsProvider(cfg)
	if err != nil {
		logf("terminateLambdaMicroVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(context.Background(), "destroy", "lambda-microvms")
	err = p.Destroy(destroyCtx, vmID, false)
	endSpan(err)
	if err != nil {
		logf("terminateLambdaMicroVM: failed to destroy MicroVM %s: %v", vmID, err)
		return
	}
	logf("Lambda MicroVM %s terminated", vmID)
}

// terminateExedevVM destroys an exedev VM by ID.
func (s *Server) terminateExedevVM(vmID string) {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["exedev"]
	s.mu.RUnlock()
	if !ok {
		logf("terminateExedevVM: no exedev provider configured")
		return
	}

	logf("terminateExedevVM: destroying VM %s (ssh_key_path=%q)", vmID, cfg.SSHKeyPath)
	p, err := newExedevProvider(cfg)
	if err != nil {
		logf("terminateExedevVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(context.Background(), "destroy", "exedev")
	err = p.Destroy(destroyCtx, vmID, false)
	endSpan(err)
	if err != nil {
		logf("terminateExedevVM: failed to destroy VM %s: %v", vmID, err)
		return
	}
	logf("Exedev VM %s terminated", vmID)
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
		logf("terminateDaytonaVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(context.Background(), "destroy", "daytona")
	err = p.Destroy(destroyCtx, workspaceID, false)
	endSpan(err)
	if err != nil {
		logf("terminateDaytonaVM: failed to destroy workspace %s: %v", workspaceID, err)
		return
	}
	logf("Daytona workspace %s terminated", workspaceID)
}

// terminateReplicatedVM terminates a Replicated CMX VM by ID.
func (s *Server) terminateReplicatedVM(vmID string) {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["replicated"]
	s.mu.RUnlock()
	if !ok {
		logf("terminateReplicatedVM: no replicated provider configured")
		return
	}
	p, err := newReplicatedProvider(cfg)
	if err != nil {
		logf("terminateReplicatedVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(context.Background(), "destroy", "replicated")
	err = p.DeleteVM(destroyCtx, vmID)
	endSpan(err)
	if err != nil {
		logf("terminateReplicatedVM: failed to delete VM %s: %v", vmID, err)
		return
	}
	logf("Replicated VM %s terminated", vmID)
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
			logfCtx(r.Context(), "github app[%d] (app_id=%d url=%s) config error: %v", i, appCfg.AppID, appCfg.URL, err)
			continue
		}
		token, expiresAt, err := provider.InstallationToken(r.Context(), 0, repos)
		if err != nil {
			// Debug-level only — expected when multiple apps configured and only one matches
			logfCtx(r.Context(), "[github] app[%d] app_id=%d: no match for repos (trying next): %v", i, appCfg.AppID, err)
			continue
		}
		logfCtx(r.Context(), "github token issued via app_id=%d for claw %s", appCfg.AppID, clawID[:8])
		jsonOK(w, map[string]interface{}{
			"token":      token,
			"expires_at": expiresAt,
		})
		return
	}

	logfCtx(r.Context(), "no github app found with installation for repos %v (claw %s)", repos, clawID[:8])
	http.Error(w, "no github installation found for the requested repos", http.StatusNotFound)
}
