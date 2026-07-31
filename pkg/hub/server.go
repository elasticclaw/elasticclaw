package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/elasticclaw/elasticclaw/internal/webui"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
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
	db        *sql.DB
	addr      string
	hubCfg    *types.HubConfig
	identity  *HubIdentity
	mux       *http.ServeMux
	artifacts artifact.Store

	mu    sync.RWMutex
	claws map[string]*clawConn // claw_id -> conn
	users map[string]*userConn // tenant_id -> []conn (broadcast)
	// gatewayRestartCounts retains the heartbeat counter across WebSocket reconnects.
	gatewayRestartCounts map[string]int
	// autoResumeRestartCounts records restart counts already handled per claw. Guarded by s.mu.
	autoResumeRestartCounts map[string]int
	lastTokenFailureLog     time.Time
	// trackedPRCount is the PR count observed by the last poll; it drives the
	// PR watcher's adaptive interval. Guarded by s.mu.
	trackedPRCount int
	// lastQuotaLog throttles the GitHub rate-limit budget log line. Guarded by s.mu.
	lastQuotaLog time.Time
	// lastRateLimitSkipLog throttles the "poll skipped, quota exhausted" log. Guarded by s.mu.
	lastRateLimitSkipLog time.Time

	// ghTokenCache holds GitHub App installation tokens until shortly before
	// they expire, keyed by requested repo access.
	ghTokenMu    sync.Mutex
	ghTokenCache map[string]cachedGitHubToken
	// one-time oauth_code -> signed GitHub session token

	dependencyStatus *dependencyStatusService

	fileAckMu           sync.Mutex
	fileAckWaiters      map[string]chan types.FileAck      // request_id -> waiter
	fileReadWaiters     map[string]chan types.FileReadResp // request_id -> waiter
	volumeAttachWaiters map[string]chan types.VolumeAttachAck
	volumeSyncWaiters   map[string]chan types.VolumeSyncAck

	checkpointMu      sync.Mutex
	checkpointWaiters map[string]chan error // checkpoint_id -> waiter

	replicatedBootstrapEnvMu sync.Mutex
	replicatedBootstrapEnv   map[string]map[string]string // temporary handoff while a Replicated VM becomes reachable

	// githubBaseURL overrides the GitHub API base for testing (default: https://api.github.com)
	githubBaseURL string
	// linearBaseURL overrides the Linear API base for testing (default: https://api.linear.app)
	linearBaseURL string
	// shortcutBaseURL overrides the Shortcut API base for testing (default: https://api.app.shortcut.com)
	shortcutBaseURL string
	// jiraBaseURL overrides the Jira API base for testing.
	jiraBaseURL        string
	trackerMoveBackoff func(int) time.Duration
	// fireworksBaseURL overrides the Fireworks API base for testing (default: https://api.fireworks.ai)
	fireworksBaseURL          string
	fireworksModelsMu         sync.Mutex
	fireworksModelsCacheKey   string
	fireworksModelsCache      []LLMModelOption
	fireworksModelsCacheUntil time.Time
	modelAuthJobsMu           sync.Mutex
	modelAuthJobs             map[string]*modelAuthLoginJob
	modelAuthRefreshMu        sync.Mutex
	modelAuthPending          map[string]string // rotated auth state awaiting durable config persistence
	grokTokenEndpoint         string            // test seam; defaults to the xAI OAuth token endpoint
	pollWarningMu             sync.Mutex
	pollWarnings              map[string]struct{}
	noProgressMu              sync.Mutex // serializes pause/resume state across the DB and active connection

	// webhookDedup prevents duplicate Linear webhook deliveries from creating
	// duplicate claws. Keyed by issue transition fingerprint; entries expire after 30s.
	webhookDedup   map[string]time.Time
	webhookDedupMu sync.Mutex

	// promoteMu serializes promotePendingClaws to prevent TOCTOU race where
	// multiple terminating claws each read active < max and promote, exceeding limit.
	promoteMu sync.Mutex

	// cronScheduler manages scheduled workflow runs
	cronScheduler *cronScheduler

	// Reaper state is deliberately in-memory: its conservative timers reset on
	// a hub restart rather than treating an uncertain outage as an agent failure.
	reaperMu            sync.Mutex
	reaperFirstSeen     map[string]time.Time
	nowFunc             func() time.Time
	terminateVMOverride func(provider, id string) error // test seam for terminal cleanup
}

func (s *Server) modelAuthTokenForClaw(clawID string) string {
	s.mu.RLock()
	secret := strings.TrimSpace(s.hubCfg.Token)
	s.mu.RUnlock()
	if secret == "" || clawID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("elasticclaw:model-auth:" + clawID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validModelAuthToken(clawID, token string) bool {
	want := s.modelAuthTokenForClaw(clawID)
	return want != "" && hmac.Equal([]byte(want), []byte(strings.TrimSpace(token)))
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
	gatewayRestartCount   int             // cumulative bridge restarts reported by heartbeats
	forcedFinishCount     int             // consecutive watchdog-forced streaming turn finishes
	workflowStartPending  bool            // true while initial volume attach / wake is in flight
	workflowStartDone     bool            // true once initial volume attach / wake has completed
	streamingBuf          strings.Builder // accumulates chunks for current in-flight response
	streamingMsgID        string          // pre-assigned message ID for the current stream
	streamingSplit        bool            // true once activity has split this turn into multiple persisted segments
	streamingStartedAt    time.Time       // when the current streaming turn started (zero if not streaming)
	streamingTimeoutSent  bool            // true once the 12-min timeout message has been injected this turn
	contextWarningSent    bool            // true once the context-nearly-full warning has been injected this turn
	awaitingResponse      bool            // true as soon as a prompt is delivered, before the first chunk/activity
	noProgressPaused      bool            // automatic delivery is paused after repeated turns with unchanged progress
	lastTurnFinishedAt    time.Time       // when the last streaming turn ended (for post-restart resume window)

	deliveryInFlight bool // serializes DB-backed delivery writes

	pendingCheckpointReason string // coalesced checkpoint request to run after current turn
	pendingCheckpointID     string
	pendingCheckpointBy     string
	checkpointInProgress    bool

	// Status channel for watchdog / progress reporting (second session on bridge)
	statusConn            *websocket.Conn // separate WS for lightweight status queries
	lastStatusAt          time.Time       // when we last got a status response
	lastUserMessageAt     time.Time       // when the user last sent a message (for idle detection)
	lastStatusBroadcastAt time.Time       // when we last broadcast status to user
	unresponsiveWarnedAt  time.Time       // when the silent-death warning was first broadcast
}

const (
	defaultGatewayUnhealthyMax = 12
	defaultBusyTurnMax         = 45 * time.Minute
	defaultSilentDeathMax      = 10 * time.Minute
)

const (
	streamingTimeoutNudge  = "[hub] Your current response has been running for over 12 minutes. Please wrap up and send your response."
	contextNearlyFullNudge = "[hub] Context window is nearly full. Summarize your progress briefly and send [DONE] with any PR URL, or ask the user what to do next."
	// autoResumeRecentTurnWindow includes races between a crash and turn completion.
	autoResumeRecentTurnWindow = 5 * time.Minute
)

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

func (cc *clawConn) isBusyLocked() bool {
	return cc.awaitingResponse || !cc.streamingStartedAt.IsZero() || cc.streamingMsgID != ""
}

func (cc *clawConn) finishTurnLocked() {
	cc.awaitingResponse = false
	cc.streamingMsgID = ""
	cc.streamingBuf.Reset()
	cc.streamingSplit = false
	cc.streamingStartedAt = time.Time{}
	cc.streamingTimeoutSent = false
	cc.contextWarningSent = false
	cc.lastTurnFinishedAt = time.Now()
}

func (s *Server) flushStreamingSegment(clawID, tenantID string, cc *clawConn) error {
	cc.mu.Lock()
	if cc.streamingBuf.Len() == 0 {
		cc.mu.Unlock()
		return nil
	}
	msgID := cc.streamingMsgID
	if msgID == "" {
		msgID = uuid.New().String()
	}
	content := cc.streamingBuf.String()
	cc.streamingMsgID = ""
	cc.streamingBuf.Reset()
	cc.streamingSplit = true
	cc.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET content=excluded.content, delivered_at=excluded.delivered_at`,
		msgID, clawID, tenantID, "claw", content, now(), now(),
	)
	return err
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
	log.Printf("Hub SSH public key:\n%s", id.PublicKey)
	srv := &Server{
		db:                      db,
		addr:                    addr,
		hubCfg:                  hubCfg,
		identity:                id,
		artifacts:               artifacts,
		claws:                   make(map[string]*clawConn),
		users:                   make(map[string]*userConn),
		gatewayRestartCounts:    make(map[string]int),
		autoResumeRestartCounts: make(map[string]int),
		dependencyStatus:        newDependencyStatusService(hubCfg),
		fileAckWaiters:          make(map[string]chan types.FileAck),
		fileReadWaiters:         make(map[string]chan types.FileReadResp),
		checkpointWaiters:       make(map[string]chan error),
		webhookDedup:            make(map[string]time.Time),
		reaperFirstSeen:         make(map[string]time.Time),
		nowFunc:                 now,
	}
	if srv.livenessEnabled() {
		srv.reconcileOnBoot()
	}

	// Start background poller to keep provider VM status fresh
	go srv.pollProviderStatus()
	go srv.keepAliveDaytonaSandboxes()
	go srv.pruneAnalytics()
	go srv.statusWatchdog()
	go srv.checkpointScheduler()
	if srv.livenessEnabled() {
		go srv.runReaper()
	}
	srv.startPRWatcher()

	// Start cron scheduler for workflow triggers
	srv.cronScheduler = newCronScheduler(srv)
	if err := srv.cronScheduler.start(); err != nil {
		log.Printf("[cron] failed to start scheduler: %v", err)
	}
	srv.startIntegrationPoller()

	return srv, nil
}

// Run starts the HTTP server (blocking).
// RunOptions configures runtime behavior of the hub.
type RunOptions struct {
	NoWebUI bool // skip serving embedded web UI (use in dev when Next.js runs separately)
}

// safeGo contains panics in goroutines that own agent or workflow progress.
func (s *Server) safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[hub] panic in %s: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

func (s *Server) Run(opts ...RunOptions) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return s.run(ctx, opts...)
}

func (s *Server) run(ctx context.Context, opts ...RunOptions) error {
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

	srv := &http.Server{Addr: s.addr, Handler: corsMiddleware(mux)}
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		log.Printf("[hub] shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		defer close(shutdownDone)
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("[hub] shutdown error: %v", err)
		}
	}()

	log.Printf("ElasticClaw Hub listening on %s", s.addr)
	if s.hubCfg.UIPassword == "" {
		log.Printf("⚠️  Web UI password not set — using default: 'admin'. Set ui_password in hub.yaml to secure the UI.")
	}
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		// ListenAndServe returns as soon as Shutdown starts. Wait for it to
		// finish draining active requests before closing their database.
		<-shutdownDone
		if closeErr := s.db.Close(); closeErr != nil {
			return fmt.Errorf("close database: %w", closeErr)
		}
		return nil
	}
	return err
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
	mux.HandleFunc("/api/branding", s.handleBranding) // public — no auth required
	mux.HandleFunc("/api/hub-config", s.withWebAdminAuth(s.handleHubConfig))
	mux.HandleFunc("/api/settings", s.withWebAdminAuth(s.handleSettings))
	mux.HandleFunc("/api/settings/status", s.withWebAdminAuth(s.handleSettingsStatus))
	mux.HandleFunc("/api/settings/github/test", s.withWebAdminAuth(s.handleGitHubAppTest))
	mux.HandleFunc("/api/settings/model-auth/login", s.withWebAdminAuth(s.handleModelAuthLogin))
	mux.HandleFunc("/api/settings/model-auth/login/{id}", s.withWebAdminAuth(s.handleModelAuthLoginStatus))

	// Template store
	mux.HandleFunc("/api/templates", s.withWebAuth(s.handleTemplates))
	mux.HandleFunc("/api/templates/{name}", s.withWebAuth(s.handleTemplateDetail))

	// Integration webhooks (signature-validated, no session auth)
	mux.HandleFunc("/api/integrations/linear/webhook", s.handleLinearWebhook)
	mux.HandleFunc("/api/integrations/github/webhook", s.handleGitHubWebhook)
	mux.HandleFunc("/api/integrations/github-issues/webhook", s.handleGitHubIssuesWebhook)
	mux.HandleFunc("/api/integrations/shortcut/webhook", s.handleShortcutWebhook)
	mux.HandleFunc("/api/integrations/jira/webhook", s.handleJiraWebhook)
	mux.HandleFunc("/api/integrations/external/webhook", s.handleExternalWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/linear", s.handleLinearWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/github", s.handleGitHubWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/github-issues", s.handleGitHubIssuesWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/shortcut", s.handleShortcutWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/jira", s.handleJiraWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/external", s.handleExternalWebhook)
	mux.HandleFunc("/api/factories/", s.withAuth(s.handleFactoryEvents))                                               // GET /api/factories/:name/events
	mux.HandleFunc("/api/factories/{name}/trigger", s.withAuth(s.handleFactoryTrigger))                                // POST manual trigger
	mux.HandleFunc("/api/factories/{name}/analytics", s.withAuth(s.handleFactoryAnalytics))                            // GET factory analytics
	mux.HandleFunc("/api/factories", s.withAdminForMethods(s.handleFactoriesCRUD, http.MethodPost, http.MethodDelete)) // factory CRUD (GET list, POST push)
	mux.HandleFunc("/api/analytics/factories", s.withAuth(s.handleAllFactoriesAnalytics))                              // GET all factories analytics
	mux.HandleFunc("/api/analytics/summary", s.withAuth(s.handleTaskRunAnalyticsSummary))
	mux.HandleFunc("/api/analytics/costs", s.withAuth(s.handleTaskRunAnalyticsCosts))
	mux.HandleFunc("/api/analytics/effectiveness", s.withAuth(s.handleTaskRunAnalyticsEffectiveness))
	mux.HandleFunc("/api/analytics/cost-drivers", s.withAuth(s.handleTaskRunAnalyticsCostDrivers))
	mux.HandleFunc("/api/analytics/general-stats", s.withAuth(s.handleTaskRunAnalyticsGeneralStats))
	mux.HandleFunc("/api/analytics/filter-options", s.withAuth(s.handleTaskRunAnalyticsFilterOptions))
	mux.HandleFunc("/api/analytics/runs", s.withAuth(s.handleTaskRunAnalyticsRuns))
	mux.HandleFunc("/api/analytics/runs/", s.withAuth(s.handleTaskRunAnalyticsRuns))
	mux.HandleFunc("/api/dependencies/status", s.withAuth(s.handleDependencyStatus))
	mux.HandleFunc("/api/workspaces", s.withAdminForMethods(s.handleWorkspacesCRUD, http.MethodPost, http.MethodDelete)) // workspace CRUD
	mux.HandleFunc("/api/workspaces/{name}/workflows", s.withAdminForMethods(s.handleWorkspaceWorkflowsList, http.MethodPost))
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}", s.withAdminForMethods(s.handleWorkspaceWorkflowDetail, http.MethodPatch))
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/trigger", s.withAuth(s.handleWorkspaceWorkflowTrigger))
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/cron/trigger", s.withAuth(s.handleCronWorkflowTrigger))  // POST manual trigger
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/cron/runs", s.withAuth(s.handleCronWorkflowRuns))        // GET run history
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/cron/runs/{runId}", s.withAuth(s.handleCronWorkflowRun)) // GET single run
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/cron/next", s.withAuth(s.handleCronWorkflowNextRun))     // GET next scheduled run
	mux.HandleFunc("/api/workspaces/{workspace}/secrets", s.withAdminForMethods(s.handleWorkspaceSecretsCRUD, http.MethodPut, http.MethodPost, http.MethodDelete))
	mux.HandleFunc("/api/workspaces/{workspace}/github-apps", s.withAdminForMethods(s.handleWorkspaceGitHubAppsCRUD, http.MethodPut, http.MethodPost, http.MethodDelete))
	mux.HandleFunc("/api/workspaces/{workspace}/issue-trackers", s.withAdminForMethods(s.handleWorkspaceIssueTrackersCRUD, http.MethodPut, http.MethodPost, http.MethodDelete))
	mux.HandleFunc("/api/secrets", s.withWebAdminAuth(s.handleSecretsCRUD)) // secrets CRUD (GET names, PUT upsert, DELETE)
	mux.HandleFunc("/api/mcp", s.withWebAdminAuth(s.handleMCPCrud))         // MCP server CRUD (GET list, PUT upsert, DELETE)
	mux.HandleFunc("/api/claws", s.withAuth(s.handleClaws))
	mux.HandleFunc("/api/claws/{id}", s.withAuth(s.handleClawDetail))
	mux.HandleFunc("/api/checkpoints/blob/", s.handleCheckpointBlobUpload)
	mux.HandleFunc("/api/checkpoints/", s.handleCheckpointInternal)
	mux.HandleFunc("/api/volumes/leases/{lease}/archive", s.handleVolumeArchive)
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
		`SELECT id, name, template, COALESCE(provider,''), COALESCE(provider_id,''), status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,''), COALESCE(bootstrap_status,''), COALESCE(bootstrap_diagnostic,''), COALESCE(github_issue_id,'') FROM claws WHERE tenant_id = ? AND status != 'deleted' ORDER BY created_at DESC`,
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
	env["ELASTICCLAW_MODEL_AUTH_TOKEN"] = s.modelAuthTokenForClaw(clawID)
	// Sensitive identity is hub-managed and must not be overridden by request env.
	env["ELASTICCLAW_TEMPLATE"] = req.TemplateName
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

	// Auto-enable Nix for workspaces that include a flake.nix.
	// This makes the workspace flake the contract for tools without requiring
	// an explicit "nix: true" in elasticclaw-config.yaml (while still
	// honoring explicit nix: true for non-flake Nix usage).
	if _, hasFlake := req.Files["flake.nix"]; hasFlake {
		req.Nix = true
		log.Printf("[create] claw %s: auto-enabled nix because flake.nix present", req.Name)
	}

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
		case "docker":
			provErr = s.provisionDocker(ctx, clawID, req, provCfg, templateFiles)
		case "lambda-microvms":
			provErr = s.provisionLambdaMicroVMs(ctx, clawID, req, provCfg, templateFiles)
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

		res, err := s.db.Exec(`UPDATE claws SET status='deleted', bootstrap_status='' WHERE id = ? AND tenant_id = ? AND status != 'deleted'`, clawID, tenantID)
		if err != nil {
			log.Printf("kill: db soft-delete error for claw %s: %v", clawID, err)
			http.Error(w, fmt.Sprintf("db error: %v", err), http.StatusInternalServerError)
			return
		}
		rowsAffected, err := res.RowsAffected()
		if err != nil || rowsAffected == 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if clawStatus != "error" {
			s.recordTaskRunManualStopBeforeDelivery(clawID, ghLogin)
		}
		if s.cronScheduler != nil {
			s.cronScheduler.finishRunByClawID(clawID, "canceled", "manually killed")
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
			// Keep messages (including agent activity logs) for post-mortem diagnosis.
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
	c.GitHubIssueURL = githubIssueURL(c.GitHubIssueID)
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
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/messages/"), "/")
	parts := strings.Split(path, "/")
	clawID := parts[0]
	if clawID == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if len(parts) > 1 {
		switch parts[1] {
		case "timeline":
			s.handleMessageTimeline(w, r, tenantID, clawID)
		case "activity":
			s.handleMessageActivity(w, r, tenantID, clawID)
		default:
			http.Error(w, "not found", http.StatusNotFound)
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
		s.resumeNoProgressAfterUserInput(clawID)
		if _, err := s.db.Exec(
			`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,NULL)`,
			msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.CreatedAt,
		); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		s.recordTaskRunDashboardMessage(clawID, ghLoginMsg, msg.ID)
		// Deliver oldest pending message if connected and idle.
		s.mu.RLock()
		cc := s.claws[clawID]
		s.mu.RUnlock()
		if cc != nil {
			cc.mu.Lock()
			cc.lastUserMessageAt = time.Now()
			cc.unresponsiveWarnedAt = time.Time{}
			busy := cc.isBusyLocked()
			cc.mu.Unlock()
			if !busy {
				s.sendNextQueuedMessage(cc)
			} else {
				log.Printf("[hub] message pending for busy claw %s", clawID[:8])
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.canViewMessages(w, r, tenantID, clawID) {
		return
	}

	limit := parsePositiveLimit(r, 50, 100)
	before := r.URL.Query().Get("before")
	rows, err := s.queryConversationMessages(clawID, tenantID, before, limit)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	if len(rows) == 0 {
		summary, err := s.activitySummary(clawID, tenantID, nil, parseTimeCursor(before), "", before)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
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
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if !hasOlderConversation {
		firstCursor := firstCreated.Format(time.RFC3339Nano)
		summary, err := s.activitySummary(clawID, tenantID, nil, &firstCreated, "", firstCursor)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
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
			http.Error(w, "db error", http.StatusInternalServerError)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	msgs, err := scanHubMessages(rows)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
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
		log.Printf("[claw ws] invalid token for claw %.8s: received_len=%d configured_len=%d err=%v", rp.ClawID, len(rp.Token), len(s.hubCfg.ClawToken), err)
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
		if !allowWakeBeforeBootstrap(provider, bootstrapOK) {
			desiredStatus = "starting"
		}
		currentStatus = desiredStatus
		_ = s.db.QueryRow(
			`INSERT INTO claws(id,tenant_id,name,template,status,last_seen,created_at) VALUES(?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET name=excluded.name,
			 template=COALESCE(NULLIF(excluded.template,''), claws.template),
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
	allowWake := allowWakeBeforeBootstrap(provider, bootstrapOK)
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
						existing2.unresponsiveWarnedAt = time.Time{}
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

	var noProgressPaused bool
	_ = s.db.QueryRow(`SELECT COALESCE(no_progress_paused, 0) != 0 FROM claws WHERE id=?`, clawID).Scan(&noProgressPaused)
	// A bridge-process restart tears down the main channel, so the old clawConn
	// (and its lastTurnFinishedAt) is usually gone by the time the new bridge
	// registers. Seed the post-restart resume window from the last claw
	// response — a turn interrupted by the crash was flushed as an
	// "[interrupted]" claw message at disconnect, so its created_at marks the
	// turn end closely enough for autoResumeRecentTurnWindow.
	var lastClawMsgAt time.Time
	_ = s.db.QueryRow(`SELECT created_at FROM messages WHERE claw_id=? AND role='claw' ORDER BY created_at DESC LIMIT 1`, clawID).Scan(&lastClawMsgAt)
	cc := &clawConn{id: clawID, tenantID: tenantID, conn: conn, gatewayReady: gatewayReadyBool(rp.GatewayReady), tags: registrationTags, lastUserMessageAt: time.Now(), lastStatusAt: time.Now(), noProgressPaused: noProgressPaused}
	s.mu.Lock()
	if s.gatewayRestartCounts != nil {
		cc.gatewayRestartCount = s.gatewayRestartCounts[clawID]
	}
	if old, ok := s.claws[clawID]; ok {
		old.mu.RLock()
		cc.statusConn = old.statusConn
		cc.lastStatusAt = old.lastStatusAt
		cc.lastTurnFinishedAt = old.lastTurnFinishedAt
		old.mu.RUnlock()
	}
	if cc.lastTurnFinishedAt.IsZero() {
		cc.lastTurnFinishedAt = lastClawMsgAt
	}
	s.claws[clawID] = cc
	s.mu.Unlock()

	log.Printf("[bridge] ✓ connected: %s (%s) gateway_ready=%v", rp.Name, clawID[:8], cc.gatewayReady)

	// Bind managed model credentials to this authenticated WebSocket instead of
	// accepting a caller-supplied claw ID on a tenant-token HTTP endpoint.
	ackPayload := map[string]any{"claw_id": clawID}
	modelAuthAuthorized := s.validModelAuthToken(clawID, rp.ModelAuthToken)
	if modelAuthAuthorized {
		credential, credentialErr := s.managedGrokCredential(context.WithValue(ctx, ctxTenantKey{}, tenantID), clawID)
		if credentialErr == nil {
			ackPayload["model_auth_credential"] = credential
		} else if !errors.Is(credentialErr, errManagedGrokNotConfigured) {
			logModelAuthRefreshError(clawID, credentialErr)
			ackPayload["model_auth_error"] = "managed model authentication is unavailable"
		}
	}
	_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "registered", Payload: ackPayload})

	// Broadcast initial status to user sessions
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": currentStatus}})

	// This is synchronous so reconnect delivery is ordered with new user messages.
	s.sendNextQueuedMessage(cc)

	// Initialize entry pipeline stage only after bridge connects so on_enter inject
	// can be delivered over WS.
	if allowWake && cc.gatewayReady && currentStatus == "connected" {
		s.startWorkflowAfterVolumes(ctx, cc, clawID)
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
				`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,?)
				 ON CONFLICT(id) DO UPDATE SET content=excluded.content, delivered_at=excluded.delivered_at`,
				partialMsgID, clawID, tenantID, "claw", partialContent, interruptedAt, interruptedAt,
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
		// Don't overwrite terminal/watching states or a replacement already being
		// provisioned. The disconnect may belong to the superseded instance.
		if currentStatus != "completed" && currentStatus != "deleted" && currentStatus != "idle" && currentStatus != "error" && currentStatus != "provisioning" {
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
					GatewayHealthy   bool     `json:"gateway_healthy"`
					GatewayReady     *bool    `json:"gateway_ready,omitempty"`
					ContextUsage     int      `json:"context_usage"`
					RestartCount     int      `json:"restart_count"`
					SessionKey       string   `json:"session_key"`
					InputTokens      *int     `json:"input_tokens"`
					OutputTokens     *int     `json:"output_tokens"`
					TotalTokens      *int     `json:"total_tokens"`
					EstimatedCostUSD *float64 `json:"estimated_cost_usd"`
					Model            string   `json:"model"`
					ModelProvider    string   `json:"model_provider"`
				}
				if err := json.Unmarshal(payload, &hb); err == nil {
					if err := s.recordTaskRunUsage(clawID, taskRunUsageSnapshot{SessionKey: hb.SessionKey, InputTokens: hb.InputTokens, OutputTokens: hb.OutputTokens, TotalTokens: hb.TotalTokens, EstimatedCostUSD: hb.EstimatedCostUSD, Model: hb.Model, ModelProvider: hb.ModelProvider}); err != nil {
						log.Printf("[usage] heartbeat for %s: %v", clawID, err)
					}
					gatewayUnhealthyMax := s.livenessSettings().gatewayUnhealthyMax
					var wakeConn *clawConn
					var shouldWake bool
					var shouldWarnContext bool
					var shouldEscalateGateway bool
					var shouldAutoResume bool
					var prevUsage int
					s.mu.Lock()
					if cc, ok := s.claws[clawID]; ok {
						cc.mu.Lock()
						// Log only on status changes, not every heartbeat
						prevUsage = cc.contextUsage
						cc.contextUsage = hb.ContextUsage
						if s.gatewayRestartCounts == nil {
							s.gatewayRestartCounts = make(map[string]int)
						}
						lastRestartCount, restartCountSeen := s.gatewayRestartCounts[clawID]
						// The map is in-memory only: the first heartbeat after a hub
						// restart carries a historical restart_count, not a fresh
						// restart. Record it as a baseline instead of treating it as
						// a restart, or every busy claw would get a spurious
						// "session was lost" resume after each hub deploy.
						if !restartCountSeen {
							s.gatewayRestartCounts[clawID] = hb.RestartCount
						} else if hb.RestartCount != lastRestartCount {
							// Any change signals a restart: an increase is an in-process
							// gateway restart; a decrease means the bridge process itself
							// was relaunched and its counter reset.
							log.Printf("[heartbeat] %s (%s): agent process restarted (restart_count=%d)", rp.Name, clawID[:8], hb.RestartCount)
							s.gatewayRestartCounts[clawID] = hb.RestartCount
							if s.autoResumeRestartCounts == nil {
								s.autoResumeRestartCounts = make(map[string]int)
							}
							turnOpenOrRecent := !cc.streamingStartedAt.IsZero() || cc.awaitingResponse ||
								(!cc.lastTurnFinishedAt.IsZero() && time.Since(cc.lastTurnFinishedAt) < autoResumeRecentTurnWindow)
							// Existence-aware lookup: a bridge relaunch resets its counter
							// to 0, which equals the zero value of an absent map entry — a
							// plain read would skip the resume for that first relaunch.
							lastResumedCount, resumeRecorded := s.autoResumeRestartCounts[clawID]
							if turnOpenOrRecent && (!resumeRecorded || lastResumedCount != hb.RestartCount) {
								s.autoResumeRestartCounts[clawID] = hb.RestartCount
								shouldAutoResume = true
							}
						}
						cc.gatewayRestartCount = s.gatewayRestartCounts[clawID]
						// Promote from 'starting' to 'connected' once gateway is ready.
						// nil means field absent (old bridge) — treat as ready.
						if gatewayReadyBool(hb.GatewayReady) {
							res, execErr := s.db.Exec(`UPDATE claws SET status='connected', bootstrap_status='' WHERE id=? AND status='starting' AND bootstrap_ok=1`, clawID)
							var rowsUpdated int64
							if execErr == nil {
								rowsUpdated, _ = res.RowsAffected()
							}
							cc.gatewayReady = true
							if rowsUpdated > 0 {
								s.broadcastToUsers(tenantID, types.WSMessage{
									Type:    "claw_status",
									Payload: map[string]string{"claw_id": clawID, "status": "connected"},
								})
								log.Printf("[bridge] ✓ ready: %s (%s)", rp.Name, clawID[:8])
								go s.recordClawAgentStarted(clawID)
								shouldWake = true
								wakeConn = cc
								go s.requestBootstrapCheckpoint(clawID)
							}
						}
						if !hb.GatewayHealthy {
							cc.gatewayUnhealthyCount++
							if cc.gatewayUnhealthyCount == 1 {
								log.Printf("[heartbeat] %s (%s): gateway unhealthy", rp.Name, clawID[:8])
							} else if cc.gatewayUnhealthyCount%4 == 0 {
								log.Printf("[heartbeat] %s (%s): gateway unhealthy for %d consecutive checks", rp.Name, clawID[:8], cc.gatewayUnhealthyCount)
							}
							if cc.gatewayUnhealthyCount == 4 && !cc.streamingStartedAt.IsZero() {
								go s.injectHubMessageByID(clawID, "[hub] The gateway has been unresponsive for about a minute. If you're stuck in a long operation, consider sending [DONE] and starting fresh.")
							}
							if cc.gatewayUnhealthyCount == gatewayUnhealthyMax {
								shouldEscalateGateway = true
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
					s.heartbeatWorkflowVolumeLeases(clawID)
					if shouldAutoResume {
						go s.enqueueRestartResume(clawID, hb.RestartCount)
					}
					if shouldEscalateGateway {
						// Re-read the claw state before escalating: idle/completed claws
						// remain connected intentionally, and bootstrapping claws are
						// handled by the bootstrap watchdog.
						go s.escalateClawHealthFailure(clawID, fmt.Sprintf("agent process unhealthy for %d consecutive heartbeats", gatewayUnhealthyMax))
					}
					if shouldWarnContext {
						s.mu.RLock()
						warnCC := s.claws[clawID]
						s.mu.RUnlock()
						if warnCC != nil {
							go s.sendStreamingNudge(warnCC, contextNearlyFullNudge)
						}
					}
					if shouldWake {
						s.startWorkflowAfterVolumes(ctx, wakeConn, clawID)
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
							go s.sendStreamingNudge(cc, streamingTimeoutNudge)
						} else {
							cc.mu.Unlock()
						}
					}
				}
			} else if msg.Type == "agent_activity" {
				if activity, payload, ok := normalizeAgentActivityPayload(msg.Payload); ok {
					if err := s.flushStreamingSegment(clawID, tenantID, cc); err != nil {
						log.Printf("[agent_activity] flush streaming segment for %s: %v", clawID[:8], err)
					}
					if isBusyAgentActivity(activity) {
						cc.mu.Lock()
						if cc.streamingStartedAt.IsZero() {
							cc.streamingStartedAt = time.Now()
							cc.streamingTimeoutSent = false
							cc.contextWarningSent = false
						}
						cc.mu.Unlock()
					}
					createdAt := now()
					activity["claw_id"] = clawID
					activity["created_at"] = createdAt.Format(time.RFC3339Nano)
					content := activityContent(activity)
					if content != "" && !isUnhelpfulActivityContent(activity, content) {
						format := "activity:" + string(payload)
						_, _ = s.db.Exec(
							`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,?)`,
							uuid.New().String(), clawID, tenantID, "activity", content, format, createdAt, createdAt,
						)
					}
					s.broadcastToUsers(tenantID, types.WSMessage{
						Type:    "agent_activity",
						Payload: activity,
					})
					s.handleInitialPlanActivity(clawID, tenantID, activity)
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
						}
						if cc.streamingStartedAt.IsZero() {
							cc.streamingStartedAt = time.Now()
						}
						cc.streamingBuf.WriteString(chunk.Content)
						msgID := cc.streamingMsgID
						bufContent := cc.streamingBuf.String()
						cc.mu.Unlock()
						// Upsert — insert on first chunk, update content on subsequent
						_, _ = s.db.Exec(
							`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,?)
							 ON CONFLICT(id) DO UPDATE SET content=excluded.content, delivered_at=excluded.delivered_at`,
							msgID, clawID, tenantID, "claw", bufContent, now(), now(),
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
				persistContent := hm.Content
				skipPersist := false
				if cc.streamingMsgID != "" {
					hm.ID = cc.streamingMsgID
					if cc.streamingBuf.Len() > 0 {
						persistContent = cc.streamingBuf.String()
					}
				} else {
					hm.ID = uuid.New().String()
					skipPersist = cc.streamingSplit
				}
				cc.finishTurnLocked()
				cc.forcedFinishCount = 0
				cc.mu.Unlock()
				s.deleteStaleWatchdogNags(clawID)
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
				if !skipPersist && strings.TrimSpace(persistContent) != "" {
					_, _ = s.db.Exec(
						`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,?)
						 ON CONFLICT(id) DO UPDATE SET content=excluded.content, delivered_at=excluded.delivered_at`,
						hm.ID, hm.ClawID, hm.TenantID, hm.Role, persistContent, hm.CreatedAt, hm.CreatedAt,
					)
					s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: hm})
				}
				automaticContinuationPaused := s.observeCompletedTurn(clawID, hm.ID, hm.Content)
				if !automaticContinuationPaused {
					s.handleInitialPlanResponse(clawID, tenantID, hm.Content)
				}
				// Evaluate pipeline triggers. If a pipeline explicitly owns a
				// [DONE] trigger, let it handle that signal instead of the
				// legacy factory PR-URL completion path below.
				pipelineHandledDone := false
				var pipelineDoneCtx pipelineContext
				var pipelineDoneStage *pipeline.Stage
				if strings.Contains(hm.Content, "[DONE]") {
					pipelineDoneCtx, pipelineDoneStage, pipelineHandledDone = s.pipelineStageForMessageContains(clawID, hm.Content)
				}
				if automaticContinuationPaused {
					pipelineHandledDone = false
				} else if pipelineHandledDone {
					prURLs := extractDonePRURLs(hm.Content)
					s.trackDoneSignal(pipelineDoneCtx.Name(), pipelineDoneCtx.IssueID, clawID, len(prURLs))
					s.safeGo("pipeline done transition", func() { s.transitionPipelineStageWithContext(clawID, *pipelineDoneStage, pipelineDoneCtx) })
				} else if !strings.Contains(hm.Content, "[DONE]") {
					s.safeGo("pipeline message triggers", func() { s.checkPipelineMessageTriggers(clawID, hm.Content) })
				}
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
					s.safeGo("done checkpoint", func() {
						if _, err := s.requestCheckpoint(context.Background(), clawID, "done", "hub", false, checkpointRequestTimeout); err != nil {
							log.Printf("[checkpoint] done request for %s failed: %v", shortID(clawID), err)
						}
					})
					if !pipelineHandledDone {
						s.safeGo("done signal", func() { s.handleClawDoneSignal(clawID, hm.Content) })
					}
				}
				// Check for [TERMINATE] signal - allows claw to manage its own lifecycle
				if strings.Contains(hm.Content, "[TERMINATE]") {
					go s.handleClawTerminateSignal(clawID, hm.Content)
				}
				// Detect and store any PR URLs mentioned by the agent
				go s.scanMessageForPRs(clawID, hm.Content)
				// Detect tool error loops and inject a corrective message
				if !automaticContinuationPaused && detectToolLoop(hm.Content) {
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
				if !automaticContinuationPaused {
					s.sendNextQueuedMessage(cc)
				}
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
			} else if msg.Type == "volume_attach_ack" {
				raw, _ := json.Marshal(msg.Payload)
				var ack types.VolumeAttachAck
				if err := json.Unmarshal(raw, &ack); err == nil && ack.RequestID != "" {
					s.fileAckMu.Lock()
					ch := s.volumeAttachWaiters[ack.RequestID]
					delete(s.volumeAttachWaiters, ack.RequestID)
					s.fileAckMu.Unlock()
					if ch != nil {
						select {
						case ch <- ack:
						default:
						}
					}
				}
			} else if msg.Type == "volume_sync_ack" {
				raw, _ := json.Marshal(msg.Payload)
				var ack types.VolumeSyncAck
				if err := json.Unmarshal(raw, &ack); err == nil && ack.RequestID != "" {
					s.fileAckMu.Lock()
					ch := s.volumeSyncWaiters[ack.RequestID]
					delete(s.volumeSyncWaiters, ack.RequestID)
					s.fileAckMu.Unlock()
					if ch != nil {
						select {
						case ch <- ack:
						default:
						}
					}
				}
			} else if msg.Type == "session_rotated" {
				go s.enqueueSessionRotatedResume(clawID)
			} else if msg.Type == "model_auth_sync" {
				if !modelAuthAuthorized {
					continue
				}
				go func() {
					credential, err := s.managedGrokCredential(context.WithValue(ctx, ctxTenantKey{}, tenantID), clawID)
					if err != nil {
						if !errors.Is(err, errManagedGrokNotConfigured) {
							logModelAuthRefreshError(clawID, err)
						}
						return
					}
					_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "model_auth_credential", Payload: credential})
				}()
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
					// Inject claw-token auth for internal endpoints used by the bridge.
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
			s.resumeNoProgressAfterUserInput(hm.ClawID)
			_, _ = s.db.Exec(
				`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,NULL)`,
				hm.ID, hm.ClawID, hm.TenantID, hm.Role, hm.Content, hm.CreatedAt,
			)
			s.recordTaskRunDashboardMessage(hm.ClawID, ghLogin, hm.ID)
			s.mu.RLock()
			cc := s.claws[hm.ClawID]
			s.mu.RUnlock()
			if cc != nil {
				cc.mu.Lock()
				cc.lastUserMessageAt = time.Now()
				cc.unresponsiveWarnedAt = time.Time{}
				busy := cc.isBusyLocked()
				cc.mu.Unlock()
				if !busy {
					s.sendNextQueuedMessage(cc)
				}
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

type daytonaSandboxProvisioner interface {
	Create(context.Context, types.CreateRequest) (*types.Instance, error)
	ConfigureOpenClaw(context.Context, string, map[string]string) error
	Destroy(context.Context, string, bool) error
}

var daytonaLongRetryDelays = []time.Duration{
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	40 * time.Second,
	60 * time.Second,
}

func createAndConfigureDaytonaSandbox(ctx context.Context, p daytonaSandboxProvisioner, req types.CreateRequest, env map[string]string, recordCreated func(*types.Instance) error) (*types.Instance, error) {
	return createAndConfigureDaytonaSandboxWithRetry(ctx, p, req, env, recordCreated, daytonaLongRetryDelays)
}

func createAndConfigureDaytonaSandboxWithRetry(ctx context.Context, p daytonaSandboxProvisioner, req types.CreateRequest, env map[string]string, recordCreated func(*types.Instance) error, retryDelays []time.Duration) (*types.Instance, error) {
	instance, err := p.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("daytona create: %w", err)
	}
	if recordCreated != nil {
		if err := recordCreated(instance); err != nil {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancelCleanup()
			if cleanupErr := p.Destroy(cleanupCtx, instance.ID, false); cleanupErr != nil {
				return nil, fmt.Errorf("daytona record sandbox %s: %w (sandbox cleanup failed: %v)", instance.ID, err, cleanupErr)
			}
			return nil, fmt.Errorf("daytona record sandbox %s: %w", instance.ID, err)
		}
	}

	var configureErr error
	attempts := 0
configureLoop:
	for {
		attempts++
		configureErr = p.ConfigureOpenClaw(ctx, instance.ID, env)
		if configureErr == nil {
			return instance, nil
		}
		if attempts > len(retryDelays) {
			break
		}
		timer := time.NewTimer(retryDelays[attempts-1])
		select {
		case <-ctx.Done():
			timer.Stop()
			break configureLoop
		case <-timer.C:
		}
	}

	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancelCleanup()
	if cleanupErr := p.Destroy(cleanupCtx, instance.ID, false); cleanupErr != nil {
		return nil, fmt.Errorf("daytona configure OpenClaw environment for sandbox %s after %d attempts: %w (sandbox cleanup failed: %v)", instance.ID, attempts, configureErr, cleanupErr)
	}
	return nil, fmt.Errorf("daytona configure OpenClaw environment for sandbox %s after %d attempts: %w", instance.ID, attempts, configureErr)
}

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
	instance, err := createAndConfigureDaytonaSandbox(ctx, p, createReq, env, func(created *types.Instance) error {
		if _, err := s.db.Exec(`UPDATE claws SET status='starting', provider='daytona', provider_id=? WHERE id=?`, created.ID, clawID); err != nil {
			return err
		}
		log.Printf("daytona workspace created: %s (claw %s)", created.ID, clawID)
		recordE2EDaytonaSandboxID(created.ID)
		return nil
	})
	if err != nil {
		return err
	}

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

	execResult := func(label string, timeout time.Duration, cmd string) (*types.ExecResult, error) {
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
			// Also source nvm and pin a compatible Node 24 LTS — Daytona snapshots may ship with
			// non-LTS Node (e.g. v25) and each exec is a fresh shell session.
			// OpenClaw requires Node 24.15.0 or newer on the Node 24 line. Reuse a
			// compatible installed patch; otherwise install the latest Node 24.
			nvmSetup := `export HOME=/home/daytona; export NVM_DIR=/usr/local/share/nvm; [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh" && { nvm use 24 >/dev/null 2>&1 || true; node -e 'const [major, minor] = process.versions.node.split(".").map(Number); process.exit(major === 24 && minor >= 15 ? 0 : 1)' >/dev/null 2>&1 || nvm install 24 >/dev/null 2>&1; } ; `
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
		log.Printf("[daytona] warning: uninstall failed (ok if not installed): %v", err)
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

	// Stage flake files *early* (before gateway, onboard, etc.) when present.
	// This ensures the workspace flake is on disk for:
	// - nix develop / flake-run wrapper creation in bridge bootstrap
	// - the final gateway to run inside the devShell
	// - deterministic workflow commands
	// Full template files (for agent context) are still (re)written later.
	// Flake presence already forced nixEnabled=1 at creation time.
	s.setBootstrapStatus(clawID, "Preparing workspace")
	// Ensure the staged workspace dir exists so early flake writes (and bridge detection)
	// are reliable even if the provider snapshot did not create ~/workspace.
	if err := exec("mkdir staged workspace", 10*time.Second, "export HOME=/home/daytona; mkdir -p /home/daytona/workspace"); err != nil {
		log.Printf("[daytona] warning: mkdir ~/workspace: %v", err)
	}
	var earlyFilesJSON string
	if err := s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&earlyFilesJSON); err != nil {
		return fmt.Errorf("load template_files for early flake staging %s: %w", clawID, err)
	}
	var earlyTemplateFiles map[string]string
	if err := json.Unmarshal([]byte(earlyFilesJSON), &earlyTemplateFiles); err != nil {
		return fmt.Errorf("parse template_files for early flake staging %s: %w", clawID, err)
	}
	if len(earlyTemplateFiles) > 0 {
		if flakeFiles := templateFlakeFiles(earlyTemplateFiles); len(flakeFiles) > 0 {
			for name, content := range flakeFiles {
				name := name
				content := content
				safeName, err := cleanWorkspaceFilePath(name)
				if err != nil {
					log.Printf("[daytona] warning: skipping invalid flake path %q: %v", name, err)
					continue
				}
				// Write to the *staged* workspace dir (~/workspace) where flake
				// tooling looks for the devShell definition.
				// NOTE: on Daytona the bridge runs WITHOUT ELASTICCLAW_BOOTSTRAP (see
				// daytonaAsyncBridgeCommand), so runBootstrap -- and therefore
				// hasWorkspaceFlake(), setupFlakeEnvironmentSync() and
				// syncStagedWorkspaceToOpenClawWorkspace() -- never runs here. Nothing
				// copies this staged file into ~/.openclaw/workspace; flake setup for
				// Daytona is hub-driven. Do not assume bridge-side consumption.
				targetPath := "/home/daytona/workspace/" + safeName
				targetDir := path.Dir(targetPath)
				// Base64-encode the user-controlled flake content, then use heredoc *only* for the
				// encoded payload with a random delimiter. This prevents the raw flake.nix/lock
				// (which is workspace-controlled) from being interpreted as shell if a delimiter
				// line appears inside it. Matches the safe pattern in remoteWriteFileCommand.
				encoded := base64.StdEncoding.EncodeToString([]byte(content))
				raw := make([]byte, 8)
				if _, err := rand.Read(raw); err != nil {
					// Extremely rare; timestamp fallback still gives unique delim for this write.
					raw = []byte(fmt.Sprintf("%x", time.Now().UnixNano()))
				}
				delim := "ELASTICCLAW_B64_" + hex.EncodeToString(raw)
				writeCmd := fmt.Sprintf(
					`export HOME=/home/daytona; mkdir -p %s && base64 -d > %s << '%s'
%s
%s`,
					shellQuote(targetDir), shellQuote(targetPath), delim, encoded, delim)
				// Fail-closed: required by the workspace flake contract (#526).
				// The bridge creates ~/.elasticclaw/flake-run from these files; workflow
				// run commands also route through it. Continuing on failure would leave
				// the claw without the declared devShell tools.
				if err := exec("write "+name+" (early flake)", 15*time.Second, writeCmd); err != nil {
					return fmt.Errorf("write %s (early flake): %w", name, err)
				}
			}
			log.Printf("[daytona] early flake files staged for claw %s", clawID)
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
	oauthAuthSyncDaytona := buildOpenClawOAuthAuthSyncShell(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	onboardFlags := buildOnboardFlags(s.hubCfg.LLMKeys, llmKeyNameDaytona, defaultModelDaytona)
	providerConfigScript := buildOpenClawProviderConfig(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	if activeKeyDaytona != nil {
		activeKeyNameDaytona = activeKeyDaytona.Name
		activeKeyProviderDaytona = activeKeyDaytona.Provider
	}
	s.mu.RUnlock()
	log.Printf("[daytona] OpenClaw model resolution claw=%s selected_llm_key=%q active_llm_key=%q provider=%q default_model=%q config_patch=%t",
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
	log.Printf("[daytona] onboard openclaw...")
	onboardResult, onboardErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", "export HOME=/home/daytona; " + onboardCmd}, 2*time.Minute)
	if onboardErr != nil {
		result, diagErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", `export HOME=/home/daytona; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`}, 10*time.Second)
		if diagErr != nil || strings.TrimSpace(result.Stdout) != "exists" {
			return fmt.Errorf("onboard openclaw: %s", sanitizeBootstrapError(onboardErr))
		}
		log.Printf("[daytona] onboard returned error, but config file exists; continuing")
	} else if onboardResult.ExitCode != 0 {
		result, diagErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", `export HOME=/home/daytona; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`}, 10*time.Second)
		if diagErr != nil || strings.TrimSpace(result.Stdout) != "exists" {
			return fmt.Errorf("onboard openclaw failed (exit %d): %s", onboardResult.ExitCode, sanitizeBootstrapOutput(onboardResult.Stdout))
		}
		log.Printf("[daytona] onboard returned non-zero, but config file exists; continuing")
	} else {
		log.Printf("[daytona] onboard openclaw done")
	}
	if installCmd := daytonaInstallModelPluginCommand(activeKeyProviderDaytona); installCmd != "" {
		if err := exec("install selected model plugin", 3*time.Minute, installCmd); err != nil {
			return fmt.Errorf("install selected model plugin: %w", err)
		}
	}
	if oauthAuthSyncDaytona != "" {
		syncCmd := `export HOME=/home/daytona; export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; ` + oauthAuthSyncDaytona
		if err := exec("sync openclaw OAuth auth", 30*time.Second, syncCmd); err != nil {
			return fmt.Errorf("sync openclaw OAuth auth: %w", err)
		}
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
	if err := s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&filesJSON); err != nil {
		return fmt.Errorf("load template_files for final write %s: %w", clawID, err)
	}
	var templateFiles map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &templateFiles); err != nil {
		return fmt.Errorf("parse template_files for final write %s: %w", clawID, err)
	}
	writtenWorkspaceFiles := map[string]string{}
	if len(templateFiles) > 0 {
		templateFiles = workspaceTemplateFiles(templateFiles)
		for _, name := range sortedWorkspaceFileNames(templateFiles) {
			content := templateFiles[name]
			// Skip flake files here: they were staged early (with collision-resistant
			// delimiter) specifically for the devShell contract. Rewriting them with
			// a fixed delimiter would re-introduce the heredoc injection risk.
			if name == "flake.nix" || name == "flake.lock" {
				continue
			}
			safeName, err := cleanWorkspaceFilePath(name)
			if err != nil {
				// Fail closed: a workspace missing files it declares (e.g. scripts/
				// referenced by workflow commands) fails every stage at runtime.
				return fmt.Errorf("invalid workspace file path %q: %w", name, err)
			}
			// Write straight into the LIVE workspace. The Daytona bridge is started
			// WITHOUT ELASTICCLAW_BOOTSTRAP (see daytonaAsyncBridgeCommand), so
			// runBootstrap -- and with it syncStagedWorkspaceToOpenClawWorkspace --
			// never runs on this provider: nothing would ever copy staged files over.
			// Mirrors Replicated, which also writes final files after bootstrap.
			targetPath := daytonaWorkspaceFilePath(safeName)
			targetDir := path.Dir(targetPath)
			// Use collision-resistant delimiter (same strategy as early flake staging)
			// to protect against content containing a fixed token.
			raw := make([]byte, 8)
			if _, err := rand.Read(raw); err != nil {
				return fmt.Errorf("random delimiter for workspace file %s: %w", name, err)
			}
			delim := "ELASTICCLAW_FILE_" + hex.EncodeToString(raw)
			writeCmd := fmt.Sprintf(
				`export HOME=/home/daytona; mkdir -p %s && cat > %s << '%s'
%s
%s`,
				shellQuote(targetDir), shellQuote(targetPath), delim, content, delim)
			if daytonaExecutableWorkspaceFile(safeName) {
				writeCmd += fmt.Sprintf("\nchmod +x %s", shellQuote(targetPath))
			}
			if err := exec("write "+name, 15*time.Second, writeCmd); err != nil {
				return fmt.Errorf("write workspace file %s: %w", name, err)
			}
			writtenWorkspaceFiles[safeName] = content
		}
		log.Printf("[daytona] %d workspace files written for claw %s", len(writtenWorkspaceFiles), clawID)
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
			log.Printf("[daytona] configure gh token refresh (no retries)...")
			ghAuthResult, ghAuthErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", configureGitHubTokenRefresh}, 30*time.Second)
			if ghAuthErr != nil {
				return fmt.Errorf("configure gh token refresh: %w", ghAuthErr)
			}
			if ghAuthResult.ExitCode != 0 {
				return fmt.Errorf("configure gh token refresh failed (exit %d): %s", ghAuthResult.ExitCode, sanitizeBootstrapOutput(ghAuthResult.Stdout))
			}
			log.Printf("[daytona] configure gh token refresh done")

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
					verifyReposScript += fmt.Sprintf("gh repo view %s >/dev/null || exit 1; ", shellQuote(repo.Repo))
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

			cloneScript := buildDaytonaGitHubCloneScript(repositories)
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
					verifyCloneScript += daytonaRepoReadinessSnippet(repo.Repo)
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
			if discoveryScript := buildRepoInstructionDiscoveryScript("$HOME/.openclaw/workspace", repositories); discoveryScript != "" {
				if err := exec("discover repo instructions", 20*time.Second, "export HOME=/home/daytona; "+discoveryScript); err != nil {
					log.Printf("[daytona] warning: repo instruction discovery failed for claw %s: %v", clawID, err)
				} else {
					log.Printf("[daytona] repo instruction discovery done")
				}
			}
		}
	}

	if err := s.restoreCheckpointToDaytona(ctx, clawID, instanceID, p); err != nil {
		return fmt.Errorf("restore checkpoint: %w", err)
	}

	// Workspace file gate: every file the workspace declares (including nested
	// scripts/**) must exist before the agent's first turn, otherwise workflow
	// commands fail with "can't open file" on every stage.
	if len(writtenWorkspaceFiles) > 0 {
		s.setBootstrapStatus(clawID, "Verifying workspace files")
		filesScript := daytonaWorkspaceFilesReadinessCommand(writtenWorkspaceFiles)
		filesResult, filesErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", filesScript}, 30*time.Second)
		if filesErr != nil {
			diag := fmt.Sprintf("Workspace files verification failed: %v", filesErr)
			s.setBootstrapStatusWithDiagnostic(clawID, "Workspace incomplete", diag)
			return fmt.Errorf("workspace files readiness: %w", filesErr)
		}
		if filesResult.ExitCode != 0 {
			diag := fmt.Sprintf("Workspace files incomplete. %s", sanitizeBootstrapOutput(filesResult.Stdout))
			s.setBootstrapStatusWithDiagnostic(clawID, "Workspace incomplete", diag)
			return fmt.Errorf("workspace files incomplete (exit %d): %s", filesResult.ExitCode, sanitizeBootstrapOutput(filesResult.Stdout))
		}
		log.Printf("[daytona] workspace files verified for claw %s", clawID)
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
		log.Printf("[daytona] workspace readiness verified for claw %s", clawID)
	}

	s.markBootstrapReady(clawID)
	log.Printf("[daytona] bootstrap gated ready for claw %s", clawID)
	s.setBootstrapStatus(clawID, "Connecting to hub")

	// Start the bridge last so the first registration happens only after the
	// workspace, template files, GitHub setup, and bootstrap_ok gate are ready.
	// The bridge (and therefore the agent) must run inside the workspace
	// directory so that repo-relative paths resolve correctly.
	var templateName string
	if err := s.db.QueryRow(`SELECT COALESCE(template,'') FROM claws WHERE id=?`, clawID).Scan(&templateName); err != nil {
		return fmt.Errorf("load claw template: %w", err)
	}
	if err := s.startDaytonaBridge(ctx, instanceID, p, s.clawHubURL(), clawID, clawToken, s.modelAuthTokenForClaw(clawID), clawName, templateName); err != nil {
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

func (s *Server) startDaytonaBridge(ctx context.Context, instanceID string, p *daytona.Provider, hubURL, clawID, clawToken, modelAuthToken, clawName, templateName string) error {
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
	cmdID, err := p.ExecSessionAsync(ctx, instanceID, sessionID, daytonaAsyncBridgeCommand(hubURL, clawID, clawToken, modelAuthToken, clawName, templateName))
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
  if pgrep -x claw-bridge >/dev/null 2>&1; then
    echo "claw-bridge running pid=$(cat "$PIDFILE")"
    exit 0
  fi
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
		packageSpec = "@openai/codex@" + cliversion.FromEnv("ELASTICCLAW_CODEX_CLI_VERSION", cliversion.CodexCLIVersion)
		binary = "codex"
	case strings.HasPrefix(model, "grok/"):
		packageSpec = "@xai-official/grok@" + cliversion.FromEnv("ELASTICCLAW_GROK_CLI_VERSION", cliversion.GrokCLIVersion)
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

func daytonaInstallModelPluginCommand(provider string) string {
	if provider != "codex" {
		return ""
	}
	version := cliversion.FromEnv("ELASTICCLAW_CODEX_PLUGIN_VERSION", cliversion.CodexPluginVersion)
	spec := "npm:@openclaw/codex@" + version
	return fmt.Sprintf(`export HOME=/home/daytona
export NVM_DIR=/usr/local/share/nvm
export PATH="$NVM_DIR/current/bin:/usr/local/bin:$PATH"
if openclaw plugins info codex --json >/dev/null 2>&1; then
  echo "Codex plugin already installed"
else
  openclaw plugins install %s
fi
openclaw plugins info codex --json >/dev/null`, shellQuote(spec))
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

// daytonaLiveWorkspaceDir is the workspace the OpenClaw agent actually runs in.
const daytonaLiveWorkspaceDir = "/home/daytona/.openclaw/workspace"

func sortedWorkspaceFileNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// daytonaExecutableWorkspaceFile reports whether a workspace file should be
// written with the executable bit (scripts are frequently invoked directly).
func daytonaExecutableWorkspaceFile(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".sh" || ext == ".py"
}

// daytonaWorkspaceFilePath returns the sandbox path a workspace file must be
// written to. The write loop and the readiness gate share it so they can never
// disagree about the target, and a regression back to the staged
// /home/daytona/workspace dir is caught by unit tests instead of at runtime.
func daytonaWorkspaceFilePath(name string) string {
	return daytonaLiveWorkspaceDir + "/" + strings.TrimPrefix(name, "/")
}

// daytonaWorkspaceFilesReadinessCommand verifies every workspace file landed in
// the live workspace, including nested paths such as scripts/review-loop/x.md.
func daytonaWorkspaceFilesReadinessCommand(files map[string]string) string {
	if len(files) == 0 {
		return "true"
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, name := range sortedWorkspaceFileNames(files) {
		b.WriteString("test -f ")
		b.WriteString(shellQuote(daytonaWorkspaceFilePath(name)))
		b.WriteString(" || { echo ")
		b.WriteString(shellQuote("missing workspace file: " + name))
		b.WriteString("; exit 1; }\n")
	}
	b.WriteString("echo 'workspace files verified'\n")
	return b.String()
}

func daytonaPrepareBridgeCommand() string {
	return `set -e
export HOME=/home/daytona
mkdir -p /home/daytona/.openclaw/workspace /home/daytona/.openclaw/run /home/daytona/workspace
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

func daytonaAsyncBridgeCommand(hubURL, clawID, clawToken, modelAuthToken, clawName, templateName string) string {
	return fmt.Sprintf(`export HOME=/home/daytona
mkdir -p /home/daytona/.openclaw/workspace /home/daytona/.openclaw/run /home/daytona/workspace
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
ELASTICCLAW_HUB_URL=%s ELASTICCLAW_CLAW_ID=%s ELASTICCLAW_CLAW_TOKEN=%s ELASTICCLAW_MODEL_AUTH_TOKEN=%s ELASTICCLAW_CLAW_NAME=%s ELASTICCLAW_TEMPLATE=%s \
sh -c '
PIDFILE="$1"
LOG="$2"
echo $$ > "$PIDFILE"
trap '\''rm -f "$PIDFILE"'\'' 0
child=""
trap '\''if [ -n "$child" ]; then kill "$child" 2>/dev/null; wait "$child" 2>/dev/null; fi; exit 0'\'' TERM INT
restarts=0
total_restarts=0
backoff=5
while :; do
  started_at=$(date +%%s)
  export ELASTICCLAW_BRIDGE_RESTARTS="$total_restarts"
  /usr/local/bin/claw-bridge >> "$LOG" 2>&1 &
  child=$!
  wait "$child"
  rc=$?
  child=""
  if [ "$rc" -eq 0 ]; then
    echo "[supervisor] claw-bridge exited cleanly" >> "$LOG"
    exit 0
  fi
  now=$(date +%%s)
  if [ $((now - started_at)) -ge 300 ]; then
    restarts=0
    backoff=5
  fi
  if [ "$restarts" -ge 3 ]; then
    echo "[supervisor] claw-bridge restart budget exhausted after 3 attempts" >> "$LOG"
    exit 1
  fi
  restarts=$((restarts + 1))
  total_restarts=$((total_restarts + 1))
  echo "[supervisor] claw-bridge exited (code=$rc); restarting (attempt $restarts/3) in ${backoff}s" >> "$LOG"
  sleep "$backoff"
  backoff=$((backoff * 2))
done
' sh "$PIDFILE" "$LOG"`,
		shellQuote(hubURL),
		shellQuote(clawID),
		shellQuote(clawToken),
		shellQuote(modelAuthToken),
		shellQuote(clawName),
		shellQuote(templateName),
	)
}

func (s *Server) downloadDaytonaConnector(ctx context.Context, clawID, instanceID string, p *daytona.Provider, downloadCmd string) error {
	delays := daytonaLongRetryDelays
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
			log.Printf("[bootstrap] %s retry %d/%d in %s...", opts.Label, attempt, opts.Attempts, delay)
			opts.Sleep(delay)
		}
		if s != nil && clawID != "" {
			s.setBootstrapStatus(clawID, opts.Label)
		}
		if err := opts.Run(); err != nil {
			lastErr = err
			log.Printf("[bootstrap] %s attempt %d/%d failed: %s", opts.Label, attempt, opts.Attempts, sanitizeBootstrapError(err))
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

func replicatedFinalWorkspaceDir(sshHome string) string {
	return path.Join(sshHome, ".openclaw", "workspace")
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

func allowWakeBeforeBootstrap(provider string, bootstrapOK int) bool {
	switch provider {
	case "daytona", "replicated", "exedev":
		return bootstrapOK == 1
	default:
		return true
	}
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

	cc.mu.RLock()
	gatewayReady := cc.gatewayReady
	tenantID := cc.tenantID
	cc.mu.RUnlock()
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
	go s.recordClawAgentStarted(clawID)
	log.Printf("[bridge] ✓ ready after bootstrap: %s", clawID[:8])
	go s.requestBootstrapCheckpoint(clawID)
	s.startWorkflowAfterVolumes(context.Background(), cc, clawID)
	return true
}

func (s *Server) startWorkflowAfterVolumes(ctx context.Context, cc *clawConn, clawID string) {
	if cc == nil {
		return
	}
	cc.mu.Lock()
	if cc.workflowStartPending || cc.workflowStartDone {
		cc.mu.Unlock()
		return
	}
	cc.workflowStartPending = true
	cc.mu.Unlock()

	go func() {
		if err := s.attachWorkflowVolumes(ctx, cc, clawID); err != nil {
			cc.mu.Lock()
			cc.workflowStartPending = false
			cc.mu.Unlock()
			log.Printf("[volume] attach workflow volumes for %s failed: %v", clawID[:8], err)
			s.releaseWorkflowVolumeLeases(clawID)
			go s.stopAgentWithReason(clawID, fmt.Sprintf("Workflow volume attach failed: %v", err), false)
			return
		}

		cc.mu.Lock()
		cc.workflowStartPending = false
		cc.workflowStartDone = true
		cc.mu.Unlock()

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

func normalizeAgentActivityPayload(payload interface{}) (map[string]interface{}, []byte, bool) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, false
	}
	var activity map[string]interface{}
	if err := json.Unmarshal(raw, &activity); err != nil || activity == nil {
		return nil, nil, false
	}
	return activity, raw, true
}

func isBusyAgentActivity(activity map[string]interface{}) bool {
	kind, _ := activity["kind"].(string)
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "model_started":
		return true
	case "tool":
		phase, _ := activity["phase"].(string)
		switch strings.ToLower(strings.TrimSpace(phase)) {
		case "completed", "complete", "done", "failed", "error", "cancelled", "canceled":
			return false
		default:
			return true
		}
	default:
		return false
	}
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
	instance, err := p.Create(ctx, createReq)
	if err != nil {
		return fmt.Errorf("exedev create: %w", err)
	}
	log.Printf("exedev VM created: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='exedev', provider_id=? WHERE id=?`, instance.ID, clawID)

	// Bootstrap asynchronously
	go func() {
		if err := s.bootstrapExedev(context.Background(), clawID, instance.ID, p, files, env); err != nil {
			log.Printf("exedev bootstrap failed for claw %s: %v", clawID, err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Exedev bootstrap failed: %s", sanitizeBootstrapError(err)), false)
		}
	}()

	return nil
}

func (s *Server) bootstrapExedev(ctx context.Context, clawID, vmName string, p *exedevProvider.Provider, files map[string][]byte, env map[string]string) error {
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
	var clawName, templateName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName, templateFilesJSON string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(`SELECT COALESCE(name,''), COALESCE(template,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,''), COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(
		&clawName, &templateName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName, &templateFilesJSON,
	); err != nil {
		return fmt.Errorf("load claw config: %w", err)
	}
	var githubRepos []types.GitHubRepoAccess
	if err := json.Unmarshal([]byte(githubReposJSON), &githubRepos); err != nil {
		return fmt.Errorf("parse github_repos for exedev bootstrap: %w", err)
	}
	var templateFiles map[string]string
	if err := json.Unmarshal([]byte(templateFilesJSON), &templateFiles); err != nil {
		return fmt.Errorf("parse template_files for exedev bootstrap: %w", err)
	}
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
		ModelAuthToken:  s.modelAuthTokenForClaw(clawID),
		TemplateName:    templateName,
		HubURL:          s.clawHubURL(),
		DefaultModel:    defaultModel,
		LLMProvider:     resolveActiveProvider(hubCfg.LLMKeys, llmKeyName),
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
		OAuthAuthSync:   buildOpenClawOAuthAuthSyncShell(hubCfg.LLMKeys, llmKeyName),
		LinearEnv:       buildLinearEnv(linearToken),
		ProviderConfig:  buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName),
		OnboardFlags:    buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel),
		Env:             env,
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
	if credHelper := buildGitHubCredentialHelper(hubCfg, s.clawHubURL(), clawID, githubRepos); credHelper != "# GitHub App not configured — skipping credential helper" {
		if err := p.SetupScript(ctx, vmName, credHelper); err != nil {
			return fmt.Errorf("configure GitHub credentials and repo instructions: %w", err)
		}
		log.Printf("[exedev] GitHub credential helper and repo instruction discovery completed for claw %.8s", clawID)
	}
	s.markBootstrapReady(clawID)

	log.Printf("[exedev] bootstrap complete for claw %.8s on %s", clawID, vmName)
	return nil
}

func (s *Server) provisionDocker(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error {
	p, err := newDockerProvider(cfg)
	if err != nil {
		return fmt.Errorf("docker init: %w", err)
	}

	// Load claw configuration from DB
	var clawName, templateName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(template,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,'') FROM claws WHERE id=?`,
		clawID,
	).Scan(&clawName, &templateName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName); err != nil {
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
	oauthAuthSync := buildOpenClawOAuthAuthSyncShell(hubCfg.LLMKeys, llmKeyName)
	onboardFlags := buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel)

	// Build env map for the container — passed directly as -e flags (no shell escaping needed).
	// Start with the request environment so workflow and workspace secret refs reach
	// Docker claws, then overlay hub-managed values to prevent callers from
	// overriding the claw's identity or connection details.
	containerEnv := mergeDockerContainerEnv(req.Env, map[string]string{
		"ELASTICCLAW_HUB_URL":            dockerClawHubURL(hubCfg),
		"ELASTICCLAW_CLAW_ID":            clawID,
		"ELASTICCLAW_CLAW_TOKEN":         clawToken,
		"ELASTICCLAW_MODEL_AUTH_TOKEN":   s.modelAuthTokenForClaw(clawID),
		"ELASTICCLAW_CLAW_NAME":          clawName,
		"ELASTICCLAW_TEMPLATE":           templateName,
		"ELASTICCLAW_GITHUB_REPOS":       githubReposJSON,
		"ELASTICCLAW_BOOTSTRAP":          "1",
		"ELASTICCLAW_WAIT_FOR_WORKSPACE": "1",
		"ELASTICCLAW_GATEWAY_PASSWORD":   gatewayPassword,
		"OPENCLAW_GATEWAY_PASSWORD":      gatewayPassword,
		"OPENCLAW_DEFAULT_MODEL":         defaultModel,
		"ELASTICCLAW_LLM_PROVIDER":       resolveActiveProvider(hubCfg.LLMKeys, llmKeyName),
		"ELASTICCLAW_NIX":                boolEnv(nixEnabled != 0),
		"ELASTICCLAW_DOCKER":             boolEnv(dockerEnabled != 0),
		"ELASTICCLAW_PROVIDER_CONFIG":    providerConfig,
		"ELASTICCLAW_API_KEY_AUTH_SYNC":  apiKeyAuthSync,
		"ELASTICCLAW_OAUTH_AUTH_SYNC":    oauthAuthSync,
		"ELASTICCLAW_ONBOARD_FLAGS":      onboardFlags,
	})

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

	instance, err := p.Create(ctx, createReq)
	if err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	log.Printf("[docker] container started: %s (claw %s)", instance.ID, clawID)
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
	log.Printf("[docker] workspace files copied for claw %.8s to %s", clawID, workspaceDir)
	s.setBootstrapStatus(clawID, "Starting agent bridge")
	if err := s.ensureDockerBridge(ctx, p, instance.ID, homeDir); err != nil {
		_ = p.Destroy(context.Background(), instance.ID, false)
		return err
	}

	return nil
}

func mergeDockerContainerEnv(requestEnv, managedEnv map[string]string) map[string]string {
	merged := make(map[string]string, len(requestEnv)+len(managedEnv))
	for key, value := range requestEnv {
		merged[key] = value
	}
	for key, value := range managedEnv {
		merged[key] = value
	}
	return merged
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
		log.Printf("[docker] claw-bridge already running in container %s", containerID)
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
	log.Printf("[docker] claw-bridge started in container %s", containerID)
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

	var clawName, templateName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(template,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,'') FROM claws WHERE id=?`,
		clawID,
	).Scan(&clawName, &templateName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName); err != nil {
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
	oauthAuthSync := buildOpenClawOAuthAuthSyncShell(hubCfg.LLMKeys, llmKeyName)
	onboardFlags := buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel)
	gatewayPassword := randomHex(16)

	env := map[string]string{
		"ELASTICCLAW_HUB_URL":            s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":            clawID,
		"ELASTICCLAW_CLAW_TOKEN":         clawToken,
		"ELASTICCLAW_MODEL_AUTH_TOKEN":   s.modelAuthTokenForClaw(clawID),
		"ELASTICCLAW_CLAW_NAME":          clawName,
		"ELASTICCLAW_TEMPLATE":           templateName,
		"ELASTICCLAW_GITHUB_REPOS":       githubReposJSON,
		"ELASTICCLAW_BOOTSTRAP":          "1",
		"ELASTICCLAW_WAIT_FOR_WORKSPACE": "1",
		"ELASTICCLAW_GATEWAY_PASSWORD":   gatewayPassword,
		"OPENCLAW_GATEWAY_PASSWORD":      gatewayPassword,
		"OPENCLAW_DEFAULT_MODEL":         defaultModel,
		"ELASTICCLAW_LLM_PROVIDER":       resolveActiveProvider(hubCfg.LLMKeys, llmKeyName),
		"ELASTICCLAW_NIX":                boolEnv(nixEnabled != 0),
		"ELASTICCLAW_DOCKER":             boolEnv(dockerEnabled != 0),
		"ELASTICCLAW_PROVIDER_CONFIG":    providerConfig,
		"ELASTICCLAW_API_KEY_AUTH_SYNC":  apiKeyAuthSync,
		"ELASTICCLAW_OAUTH_AUTH_SYNC":    oauthAuthSync,
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
	instance, err := p.Create(ctx, createReq)
	if err != nil {
		return fmt.Errorf("lambda microvms create: %w", err)
	}
	log.Printf("[lambda-microvms] microvm started: %s (claw %s)", instance.ID, clawID)
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

	vmID, err := p.ProvisionClaw(ctx, replicatedpkg.VMCreateRequest{
		Name:         req.ProviderName, // stable ec-<shortid>
		InstanceType: req.InstanceType,
		TTL:          req.TTL,
	}, nil, nil)
	if err != nil {
		return fmt.Errorf("replicated provision: %w", err)
	}
	recordE2EReplicatedVMID(vmID)
	s.rememberReplicatedBootstrapEnv(clawID, env)
	// Store vm_id in the claw record — keep status='provisioning' so the poller can detect
	// the provisioning→running transition and trigger bootstrap. Skip if already deleted.
	_, _ = s.db.Exec(
		`UPDATE claws SET provider='replicated', provider_id=? WHERE id=? AND status NOT IN ('deleted','starting','connected','idle')`, vmID, clawID,
	)
	// If deleted, clean up the VM and bail
	var currentStatus string
	_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
	if currentStatus == "deleted" {
		s.forgetReplicatedBootstrapEnv(clawID)
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

func (s *Server) rememberReplicatedBootstrapEnv(clawID string, env map[string]string) {
	s.replicatedBootstrapEnvMu.Lock()
	defer s.replicatedBootstrapEnvMu.Unlock()
	if s.replicatedBootstrapEnv == nil {
		s.replicatedBootstrapEnv = make(map[string]map[string]string)
	}
	s.replicatedBootstrapEnv[clawID] = cloneStringMap(env)
}

func (s *Server) loadReplicatedBootstrapEnv(clawID string) (map[string]string, bool) {
	s.replicatedBootstrapEnvMu.Lock()
	defer s.replicatedBootstrapEnvMu.Unlock()
	env, ok := s.replicatedBootstrapEnv[clawID]
	return cloneStringMap(env), ok
}

func (s *Server) forgetReplicatedBootstrapEnv(clawID string) {
	s.replicatedBootstrapEnvMu.Lock()
	defer s.replicatedBootstrapEnvMu.Unlock()
	delete(s.replicatedBootstrapEnv, clawID)
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
		err := petDaytonaSandboxWithRetry(context.Background(), p, c.providerID, daytonaKeepaliveRetryDelays)
		if err != nil {
			log.Printf("[daytona] keepalive failed for %s (%s): %v", c.name, c.id[:8], err)
			continue
		}
		log.Printf("[daytona] keepalive ok for %s (%s)", c.name, c.id[:8])
	}
}

type daytonaKeepaliveExecutor interface {
	ExecWithTimeout(context.Context, string, []string, time.Duration) (*types.ExecResult, error)
}

var daytonaKeepaliveRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second}

func petDaytonaSandboxWithRetry(ctx context.Context, executor daytonaKeepaliveExecutor, providerID string, retryDelays []time.Duration) error {
	var lastErr error
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, lastErr = executor.ExecWithTimeout(attemptCtx, providerID, []string{"bash", "-lc", "true"}, 5*time.Second)
		cancel()
		if lastErr == nil {
			return nil
		}
		if !daytona.IsTransientExecError(lastErr) || attempt == len(retryDelays) {
			return lastErr
		}

		timer := time.NewTimer(retryDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
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
	now, cfg := time.Now(), s.livenessSettings()

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
		unresponsiveWarnedAt := cc.unresponsiveWarnedAt
		streamingStartedAt := cc.streamingStartedAt
		statusConn := cc.statusConn
		gatewayReady := cc.gatewayReady
		contextUsage := cc.contextUsage
		contextWarningSent := cc.contextWarningSent
		tenantID := cc.tenantID
		cc.mu.RUnlock()

		// The bridge normally closes a turn with a message.  If that terminal
		// message is lost, preserve the partial response and unblock the queue.
		// A second consecutive recovery means the bridge is repeatedly wedged.
		if !streamingStartedAt.IsZero() && now.Sub(streamingStartedAt) > cfg.busyTurnMax {
			var content, messageID string
			var escalate bool
			finished := false
			cc.mu.Lock()
			if !cc.streamingStartedAt.IsZero() && now.Sub(cc.streamingStartedAt) > cfg.busyTurnMax {
				if cc.streamingBuf.Len() > 0 {
					content = cc.streamingBuf.String() + " [interrupted]"
					messageID = cc.streamingMsgID
					if messageID == "" {
						messageID = uuid.New().String()
					}
				}
				cc.finishTurnLocked()
				finished = true
				cc.forcedFinishCount++
				escalate = cc.forcedFinishCount >= 2
			}
			cc.mu.Unlock()
			if finished {
				s.deleteStaleWatchdogNags(id)
			}
			if content != "" {
				_, _ = s.db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET content=excluded.content, delivered_at=excluded.delivered_at`, messageID, id, tenantID, "claw", content, now, now)
				s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: types.HubMessage{ID: messageID, ClawID: id, TenantID: tenantID, Role: "claw", Content: content, CreatedAt: now}})
			}
			s.broadcastToUsers(tenantID, types.WSMessage{Type: "agent_typing", Payload: map[string]string{"claw_id": id, "status": "idle"}})
			// The idle drain below delivers the next pending message; calling
			// sendNextQueuedMessage here as well would send two back-to-back.
			if escalate {
				go s.stopAgentWithReason(id, "agent repeatedly stuck mid-turn", false)
			}
		}
		cc.mu.RLock()
		idle := !cc.isBusyLocked()
		cc.mu.RUnlock()
		if idle {
			s.sendNextQueuedMessage(cc)
		}

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

		var name, status string
		var bootstrapOK int
		_ = s.db.QueryRow(`SELECT name, status, bootstrap_ok FROM claws WHERE id=?`, id).Scan(&name, &status, &bootstrapOK)

		// Detect silent death while the claw is fully bootstrapped. Warn after five
		// minutes, then escalate through the common failure funnel after ten.
		healthAction := watchdogAction(now, status, bootstrapOK != 0, gatewayReady, lastStatusAt, lastUserMessageAt, unresponsiveWarnedAt, cfg.silentDeathMax)
		if healthAction == watchdogHealthWarn && now.Sub(lastStatusBroadcastAt) > 5*time.Minute {
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
			cc.unresponsiveWarnedAt = now
			cc.mu.Unlock()
		} else if healthAction == watchdogHealthEscalate {
			cc.mu.Lock()
			cc.unresponsiveWarnedAt = now
			cc.mu.Unlock()
			minutes := int(now.Sub(lastStatusAt).Minutes())
			go s.stopAgentWithReason(id, fmt.Sprintf("no status updates for %d minutes, agent presumed dead", minutes), false)
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
			// A 404 means the provider lost the VM. Route it through the same
			// retry/terminal funnel as an explicit terminated status.
			if strings.Contains(err.Error(), "HTTP 404") {
				log.Printf("pollProviderStatus: VM %s not found (404) for claw %s", c.providerID, c.id[:8])
				go s.stopAgentWithReason(c.id, "Provider VM lost: replicated VM no longer exists", true)
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
				env, ok := s.loadReplicatedBootstrapEnv(c.id)
				if !ok {
					// Resolved environment values intentionally are not persisted. If the
					// hub restarted during provisioning, fail closed instead of silently
					// starting an agent without its configured environment.
					const reason = "Bootstrap failed: transient environment unavailable after hub restart; recreate the claw"
					log.Printf("Claw %s (%s): %s", c.name, c.id[:8], reason)
					go s.stopAgentWithReason(c.id, reason, false)
					continue
				}
				go s.bootstrapReplicated(c.id, c.name, c.providerID, replicatedCfg, env)
			}
		case "terminated", "error":
			log.Printf("Replicated VM %s for claw %s (%s) terminated", c.providerID, c.name, c.id)
			go s.stopAgentWithReason(c.id, "Provider VM lost: sandbox terminated (TTL expired or external shutdown)", true)
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
	cc.mu.Lock()
	if cc.isBusyLocked() || cc.noProgressPaused {
		cc.mu.Unlock()
		return
	}
	cc.awaitingResponse = true
	cc.streamingStartedAt = time.Now()
	cc.streamingTimeoutSent = false
	cc.contextWarningSent = false
	cc.mu.Unlock()

	wakeContent := defaultWakeContent
	if s.clawNeedsInitialPlan(clawID) {
		wakeContent = initialPlanWakeContent
		_ = s.insertSystemMarker(clawID, cc.tenantID, initialPlanRequiredMarker)
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
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,?)`,
		wakeMsg.ID, wakeMsg.ClawID, wakeMsg.TenantID, wakeMsg.Role, wakeMsg.Content, wakeMsg.CreatedAt, wakeMsg.CreatedAt,
	)
	wakeMsg.Content = wakeContent
	if err := wsjson.Write(context.Background(), cc.conn, types.WSMessage{Type: "message", Payload: wakeMsg}); err != nil {
		cc.mu.Lock()
		cc.finishTurnLocked()
		cc.mu.Unlock()
	}

	// Note: We don't call sendNextQueuedMessage here because sendWakeMessage is launched
	// with 'go' (asynchronously). The normal end-of-turn path in handleClawWS read loop
	// will drain the queue once the claw finishes the wake response. This prevents race
	// conditions where both goroutines try to dequeue messages concurrently.
}

func (s *Server) sendInitialPlanInstruction(cc *clawConn, clawID string) {
	if cc == nil || !s.clawNeedsInitialPlan(clawID) || s.hasSystemMarker(clawID, initialPlanAcceptedMarker) {
		return
	}
	cc.mu.Lock()
	if cc.isBusyLocked() || cc.noProgressPaused {
		cc.mu.Unlock()
		return
	}
	cc.awaitingResponse = true
	cc.streamingStartedAt = time.Now()
	cc.streamingTimeoutSent = false
	cc.contextWarningSent = false
	cc.mu.Unlock()
	if !s.insertSystemMarker(clawID, cc.tenantID, initialPlanRequiredMarker) {
		cc.mu.Lock()
		cc.finishTurnLocked()
		cc.mu.Unlock()
		return
	}
	msg := types.HubMessage{
		ID:        uuid.New().String(),
		ClawID:    clawID,
		TenantID:  cc.tenantID,
		Role:      "system",
		Content:   initialPlanWakeContent,
		CreatedAt: now(),
	}
	if err := wsjson.Write(context.Background(), cc.conn, types.WSMessage{Type: "message", Payload: msg}); err != nil {
		cc.mu.Lock()
		cc.finishTurnLocked()
		cc.mu.Unlock()
	}
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
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at,delivered_at) VALUES(?,?,?,?,?,?,?)`,
		uuid.New().String(), clawID, tenantID, "system", marker, now(), now(),
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
func (s *Server) bootstrapReplicated(clawID, clawName, vmID string, cfg types.ProviderConfig, env map[string]string) {
	defer s.forgetReplicatedBootstrapEnv(clawID)
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

	var filesJSON, templateName string
	if err := s.db.QueryRow(
		`SELECT COALESCE(template_files,'{}'), COALESCE(template,'') FROM claws WHERE id=?`,
		clawID,
	).Scan(&filesJSON, &templateName); err != nil {
		log.Printf("[bootstrap] failed to load template_files for claw %s: %v", clawID[:8], err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not read template metadata: %s", err), false)
		return
	}
	var files map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		log.Printf("[bootstrap] failed to parse template_files for claw %s: %v", clawID[:8], err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: invalid template metadata: %s", err), false)
		return
	}
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
	defaultModel, llmKeyName = resolveModelAndLLMKey(s.hubCfg, llmKeyName, defaultModel)
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
	sshHome, err := sshHomeDir(sshUser)
	if err != nil {
		log.Printf("bootstrap: invalid SSH user %q: %v", sshUser, err)
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
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.mu.RUnlock()

	script := GenerateReplicatedBootstrapScript(BootstrapParams{
		ClawID:          clawID,
		ClawName:        clawName,
		ClawToken:       clawToken,
		ModelAuthToken:  s.modelAuthTokenForClaw(clawID),
		TemplateName:    templateName,
		HubURL:          s.clawHubURL(),
		DefaultModel:    defaultModel,
		LLMProvider:     resolveActiveProvider(hubCfg.LLMKeys, llmKeyName),
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
		OAuthAuthSync:   buildOpenClawOAuthAuthSyncShell(hubCfg.LLMKeys, llmKeyName),
		LinearEnv:       buildLinearEnv(linearToken),
		ProviderConfig:  buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName),
		OnboardFlags:    buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel),
		Env:             env,
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
			log.Printf("[bootstrap] failed to stage flake before bootstrap for claw %s: %v", clawID[:8], err)
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
		log.Printf("Bootstrap failed for claw %s: %v", clawID, err)
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
		log.Printf("[bootstrap] writing %d template files for claw %s: %v", len(files), clawName, fileNames)
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Writing workspace files",
			RetryLabel: "Retrying workspace file write",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				// The Replicated bootstrap script has already finished, including the
				// bridge's one-time staged-workspace sync. Write final context files
				// directly to the live workspace so they are immediately available.
				return s.sshWriteFiles(sshUser, sshHost, replicatedFinalWorkspaceDir(sshHome), files)
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
				return s.sshRun(sshUser, sshHost, replicatedWorkspaceReadinessCommand(replicatedFinalWorkspaceDir(sshHome), files))
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: workspace files incomplete: %s", err), false)
			return
		}
		log.Printf("Template files written for claw %s", clawName)
	}

	if err := s.restoreCheckpointToSSH(clawID, sshUser, sshHost); err != nil {
		log.Printf("[bootstrap] restore checkpoint failed: %v", err)
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
		log.Printf("[bootstrap] GitHub credential helper installed for claw %s", clawName)
	}
	s.markBootstrapReady(clawID)

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
		return normalizeModelForProvider(key.Provider, key.DefaultModel)
	}

	// Check if hub's DefaultModel matches this key's provider
	if hubCfg.DefaultModel != "" && modelMatchesProvider(key.Provider, hubCfg.DefaultModel) {
		return normalizeModelForProvider(key.Provider, hubCfg.DefaultModel)
	}

	// Construct a provider-specific default model
	switch key.Provider {
	case "anthropic":
		return "anthropic/claude-sonnet-4-6"
	case "openai":
		return "openai/gpt-5.5"
	case "codex":
		return defaultCodexModel
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

	// If the target directory is a git repo, stage the written files so Nix can
	// evaluate a workspace flake (nix requires flake.nix/flake.lock to be tracked).
	if gitCmd, names := workspaceFlakeStageCommand(dir, files); len(names) > 0 {
		sess, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("create git staging session: %w", err)
		}
		out, err := sess.CombinedOutput(gitCmd)
		sess.Close()
		if err != nil {
			return fmt.Errorf("stage flake files %v: %w\n%s", names, err, string(out))
		}
	}
	return nil
}

// workspaceFlakeStageCommand builds a git add command that stages only
// flake.nix and flake.lock from the written workspace files map.
// It returns the command and the list of file names that will be staged.
// Returns an empty command and nil names when there are no flake files to stage.
func workspaceFlakeStageCommand(dir string, files map[string]string) (string, []string) {
	names := make([]string, 0, 2)
	for name := range files {
		safeName, err := cleanWorkspaceFilePath(name)
		if err != nil {
			continue
		}
		if safeName != "flake.nix" && safeName != "flake.lock" {
			continue
		}
		names = append(names, safeName)
	}
	if len(names) == 0 {
		return "", nil
	}
	sort.Strings(names)
	quotedNames := make([]string, len(names))
	for i, n := range names {
		quotedNames[i] = shellDoubleQuote(n)
	}
	gitCmd := fmt.Sprintf("cd %s && if [ -d .git ]; then git add -- %s; fi",
		shellDoubleQuote(dir), strings.Join(quotedNames, " "))
	return gitCmd, names
}

func remoteWriteFileCommand(dir, name, content string) string {
	remotePath := strings.TrimRight(dir, "/") + "/" + name
	remoteDir := path.Dir(remotePath)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))

	// Use collision-resistant random delimiter (same strategy as Daytona early/later writes)
	// to prevent heredoc injection if the (base64) payload happens to contain the marker.
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		// Extremely rare; use a timestamp-based unique fallback (still collision resistant for practical purposes).
		raw = []byte(fmt.Sprintf("%x", time.Now().UnixNano()))
	}
	delim := "ELASTICCLAW_B64_" + hex.EncodeToString(raw)

	return fmt.Sprintf("mkdir -p -- %s && base64 -d > %s << '%s'\n%s\n%s",
		shellDoubleQuote(remoteDir),
		shellDoubleQuote(remotePath),
		delim, encoded, delim,
	)
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
	if err := s.terminateVMErr(provider, vmID); err != nil {
		log.Printf("terminateVM: %v", err)
	}
}

func (s *Server) terminateVMErr(provider, vmID string) error {
	if vmID == "" {
		return nil
	}
	if s.terminateVMOverride != nil {
		return s.terminateVMOverride(provider, vmID)
	}
	switch provider {
	case "replicated":
		return s.terminateReplicatedVM(vmID)
	case "daytona":
		return s.terminateDaytonaVM(vmID)
	case "exedev":
		return s.terminateExedevVM(vmID)
	case "docker":
		return s.terminateDockerVM(vmID)
	case "lambda-microvms":
		return s.terminateLambdaMicroVM(vmID)
	default:
		return fmt.Errorf("unsupported provider %q for VM %s", provider, vmID)
	}
}

// terminateDockerVM destroys a Docker agent container by name/ID.
func (s *Server) terminateDockerVM(vmID string) error {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["docker"]
	s.mu.RUnlock()
	if !ok {
		log.Printf("terminateDockerVM: no docker provider configured")
		return fmt.Errorf("no docker provider configured")
	}
	p, err := newDockerProvider(cfg)
	if err != nil {
		log.Printf("terminateDockerVM: provider init error: %v", err)
		return err
	}
	if err := p.Destroy(context.Background(), vmID, false); err != nil {
		log.Printf("terminateDockerVM: failed to destroy container %s: %v", vmID, err)
		return err
	}
	log.Printf("Docker container %s terminated", vmID)
	return nil
}

// terminateLambdaMicroVM destroys an AWS Lambda MicroVM by ID.
func (s *Server) terminateLambdaMicroVM(vmID string) error {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["lambda-microvms"]
	s.mu.RUnlock()
	if !ok {
		log.Printf("terminateLambdaMicroVM: no lambda-microvms provider configured")
		return fmt.Errorf("no lambda-microvms provider configured")
	}
	p, err := newLambdaMicroVMsProvider(cfg)
	if err != nil {
		log.Printf("terminateLambdaMicroVM: provider init error: %v", err)
		return err
	}
	if err := p.Destroy(context.Background(), vmID, false); err != nil {
		log.Printf("terminateLambdaMicroVM: failed to destroy MicroVM %s: %v", vmID, err)
		return err
	}
	log.Printf("Lambda MicroVM %s terminated", vmID)
	return nil
}

// terminateExedevVM destroys an exedev VM by ID.
func (s *Server) terminateExedevVM(vmID string) error {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["exedev"]
	s.mu.RUnlock()
	if !ok {
		log.Printf("terminateExedevVM: no exedev provider configured")
		return fmt.Errorf("no exedev provider configured")
	}

	log.Printf("terminateExedevVM: destroying VM %s (ssh_key_path=%q)", vmID, cfg.SSHKeyPath)
	p, err := newExedevProvider(cfg)
	if err != nil {
		log.Printf("terminateExedevVM: provider init error: %v", err)
		return err
	}
	if err := p.Destroy(context.Background(), vmID, false); err != nil {
		log.Printf("terminateExedevVM: failed to destroy VM %s: %v", vmID, err)
		return err
	}
	log.Printf("Exedev VM %s terminated", vmID)
	return nil
}

// terminateDaytonaVM destroys a Daytona workspace by ID.
func (s *Server) terminateDaytonaVM(workspaceID string) error {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["daytona"]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no daytona provider configured")
	}
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		log.Printf("terminateDaytonaVM: provider init error: %v", err)
		return err
	}
	if err := p.Destroy(context.Background(), workspaceID, false); err != nil {
		log.Printf("terminateDaytonaVM: failed to destroy workspace %s: %v", workspaceID, err)
		return err
	}
	log.Printf("Daytona workspace %s terminated", workspaceID)
	return nil
}

// terminateReplicatedVM terminates a Replicated CMX VM by ID.
func (s *Server) terminateReplicatedVM(vmID string) error {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["replicated"]
	s.mu.RUnlock()
	if !ok {
		log.Printf("terminateReplicatedVM: no replicated provider configured")
		return fmt.Errorf("no replicated provider configured")
	}
	p, err := newReplicatedProvider(cfg)
	if err != nil {
		log.Printf("terminateReplicatedVM: provider init error: %v", err)
		return err
	}
	if err := p.DeleteVM(context.Background(), vmID); err != nil {
		log.Printf("terminateReplicatedVM: failed to delete VM %s: %v", vmID, err)
		return err
	}
	log.Printf("Replicated VM %s terminated", vmID)
	return nil
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
	var tagsJSON string
	err := s.db.QueryRow(
		`SELECT COALESCE(template,''), github_repos, COALESCE(tags,'[]') FROM claws WHERE id = ?`,
		clawID,
	).Scan(&workspaceName, &reposJSON, &tagsJSON)
	if err != nil {
		http.Error(w, "claw not found", http.StatusNotFound)
		return
	}
	workspaceName = clawWorkspaceName(workspaceName, tagsJSON)

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
	providers := make([]githubTokenProviderCandidate, 0, len(githubApps))
	for i, appCfg := range githubApps {
		provider, err := NewGitHubTokenProvider(appCfg)
		if err != nil {
			log.Printf("github app[%d] (app_id=%d url=%s) config error: %v", i, appCfg.AppID, appCfg.URL, err)
			continue
		}
		providers = append(providers, githubTokenProviderCandidate{
			index:    i,
			appID:    appCfg.AppID,
			url:      appCfg.URL,
			provider: provider,
		})
	}
	for _, candidate := range providers {
		provider := candidate.provider
		token, expiresAt, err := provider.InstallationToken(r.Context(), 0, repos)
		if err != nil {
			// Debug-level only — expected when multiple apps configured and only one matches
			log.Printf("[github] app[%d] app_id=%d: no match for repos (trying next): %v", candidate.index, candidate.appID, err)
			continue
		}
		log.Printf("github token issued via app_id=%d for claw %s", candidate.appID, clawID[:8])
		jsonOK(w, map[string]interface{}{
			"token":      token,
			"expires_at": expiresAt,
		})
		return
	}

	inaccessible := diagnoseGitHubRepoAccess(r.Context(), providers, repos)
	if len(inaccessible) > 0 {
		msg := inaccessibleGitHubReposMessage(inaccessible)
		log.Printf("no github app found with installation for repos %v (claw %s): %s", repos, clawID[:8], msg)
		http.Error(w, msg, http.StatusNotFound)
		return
	}

	log.Printf("no github app found with installation for repos %v (claw %s)", repos, clawID[:8])
	http.Error(w, "no github installation found for the requested repos", http.StatusNotFound)
}

func clawWorkspaceName(templateName, tagsJSON string) string {
	if templateName = strings.TrimSpace(templateName); templateName != "" {
		return templateName
	}
	var tags []string
	if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
		return ""
	}
	if workspaceName, workflowName := workflowTags(tags); workspaceName != "" && workflowName != "" {
		return workspaceName
	}
	return ""
}

func inaccessibleGitHubReposMessage(repos []string) string {
	return fmt.Sprintf("GitHub App cannot access: %s", strings.Join(repos, ", "))
}

type githubTokenProviderCandidate struct {
	index    int
	appID    int64
	url      string
	provider *GitHubTokenProvider
}

func diagnoseGitHubRepoAccess(ctx context.Context, providers []githubTokenProviderCandidate, repos []RepoAccess) []string {
	if len(providers) == 0 || len(repos) == 0 {
		return nil
	}
	inaccessible := make(map[string]bool, len(repos))
	for _, repo := range repos {
		if strings.TrimSpace(repo.Repo) != "" {
			inaccessible[repo.Repo] = true
		}
	}
	for _, candidate := range providers {
		provider := candidate.provider
		for _, repo := range repos {
			if !inaccessible[repo.Repo] {
				continue
			}
			if _, _, err := provider.InstallationToken(ctx, 0, []RepoAccess{repo}); err == nil {
				delete(inaccessible, repo.Repo)
			}
		}
	}
	return sortedStringKeys(inaccessible)
}

func sortedStringKeys(values map[string]bool) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// detectToolLoop returns true if the same class of tool error appears 3+ times
// in the content of a completed assistant turn.
func detectToolLoop(content string) bool {
	patterns := []string{
		"edit failed:", "write failed:", "read failed:",
		"exec failed:", "elevated is not available", "tool-policy",
	}
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
func (s *Server) injectHubMessage(_ context.Context, cc *clawConn, text string) {
	// Route watchdog prompts through the same durable queue as every other
	// input. Writing directly to the socket can start a second bridge turn in
	// the gap before the first response emits a chunk or activity.
	s.injectHubMessageByID(cc.id, text)
}

// deleteStaleWatchdogNags removes nudges that targeted the just-ended turn.
func (s *Server) deleteStaleWatchdogNags(clawID string) {
	res, err := s.db.Exec(`DELETE FROM messages WHERE claw_id=? AND role='hub' AND delivered_at IS NULL AND content IN (?,?)`, clawID, streamingTimeoutNudge, contextNearlyFullNudge)
	if err != nil {
		log.Printf("[watchdog] delete stale nags for %s: %v", shortID(clawID), err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[watchdog] dropped %d stale watchdog nag(s) for %s at turn end", n, shortID(clawID))
	}
}

const (
	restartResumePrefix        = "[hub] Agent process restart detected."
	sessionRotatedResumePrefix = "[hub] OpenClaw session reset detected."
)

func (s *Server) enqueueRestartResume(clawID string, restartCount int) {
	s.enqueueSessionLostResume(clawID, restartResumePrefix, fmt.Sprintf("restart:%d", restartCount))
}

// sessionRotatedResumeThrottle bounds the rotation → resume → rotation loop:
// if lock conflicts persist across fresh sessions (e.g. another process keeps
// touching the session files), each rotation would otherwise enqueue another
// resume prompt indefinitely — rotation turns complete with an empty reply,
// so the no-progress watchdog never observes them.
const sessionRotatedResumeThrottle = 10 * time.Minute

func (s *Server) enqueueSessionRotatedResume(clawID string) {
	var recent int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='hub' AND content LIKE ? AND created_at > ?`,
		clawID, sessionRotatedResumePrefix+"%", now().Add(-sessionRotatedResumeThrottle)).Scan(&recent)
	if err == nil && recent > 0 {
		log.Printf("[watchdog] skipping session-rotated resume for %s: already resumed within %s", shortID(clawID), sessionRotatedResumeThrottle)
		return
	}
	s.enqueueSessionLostResume(clawID, sessionRotatedResumePrefix, fmt.Sprintf("session_rotated:%s", uuid.NewString()))
}

func (s *Server) enqueueSessionLostResume(clawID, prefix, marker string) {
	var status, issueTitle, linearID, githubID, shortcutID, jiraID string
	var bootstrapOK int
	err := s.db.QueryRow(`SELECT status, COALESCE(bootstrap_ok,0), COALESCE(issue_title,''), COALESCE(linear_issue_id,''), COALESCE(github_issue_id,''), COALESCE(shortcut_story_id,''), COALESCE(jira_issue_id,'') FROM claws WHERE id=?`, clawID).Scan(&status, &bootstrapOK, &issueTitle, &linearID, &githubID, &shortcutID, &jiraID)
	if err != nil {
		return
	}
	if status != "connected" || bootstrapOK == 0 {
		log.Printf("[watchdog] skipping auto-resume for %s: status=%s bootstrap_ok=%d", shortID(clawID), status, bootstrapOK)
		return
	}
	var issueRef string
	switch {
	case linearID != "":
		issueRef = "Linear issue " + linearID
	case githubID != "":
		issueRef = "GitHub issue " + githubID
	case shortcutID != "":
		issueRef = "Shortcut story " + shortcutID
	case jiraID != "":
		issueRef = "Jira issue " + jiraID
	}
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString(" Your previous session was lost, so you have no memory of the earlier conversation. The workspace on disk is intact, including your repositories under ~/workspace.")
	if issueTitle != "" || issueRef != "" {
		b.WriteString("\n\nAssigned task: ")
		if issueTitle != "" {
			b.WriteString(issueTitle)
		}
		if issueRef != "" {
			if issueTitle != "" {
				b.WriteString(" (")
				b.WriteString(issueRef)
				b.WriteString(")")
			} else {
				b.WriteString(issueRef)
			}
		}
		b.WriteString(".")
	}
	b.WriteString("\n\nBefore anything else, recover your state from the workspace: run git status and git log --oneline -15, check which branch you are on and whether there are uncommitted changes or an open PR for it. Then resume the task from where the workspace shows it stopped. Do not start over and do not discard existing work.")
	// Append a zero-width marker so that two resume prompts for two different
	// incidents are never treated as the identical pending message by
	// injectMessage's dedup — each restart/session rotation is a distinct
	// incident even when the rest of the wording is unchanged.
	b.WriteString(fmt.Sprintf("\n\n<!-- %s -->", marker))
	log.Printf("[watchdog] enqueueing %s resume for %s", marker, shortID(clawID))
	s.injectHubMessageByID(clawID, b.String())
}

func (s *Server) sendStreamingNudge(cc *clawConn, text string) {
	cc.mu.RLock()
	sc, clawID, tenantID := cc.statusConn, cc.id, cc.tenantID
	turnOpen := cc.isBusyLocked()
	cc.mu.RUnlock()
	// The nudge targets the in-flight turn. This runs on a goroutine, so the
	// turn may have ended (and deleteStaleWatchdogNags already run) before we
	// get here — queueing now would deliver the stale nudge as the next
	// turn's prompt. Drop it instead.
	if !turnOpen {
		log.Printf("[watchdog] dropping nudge for %s: turn already ended", shortID(clawID))
		return
	}
	if sc == nil {
		s.injectHubMessageByID(clawID, text)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, sc, types.WSMessage{Type: "nudge", Payload: mustJSONRaw(map[string]string{"claw_id": clawID, "content": text})}); err != nil {
		// Up to 5s may have elapsed; re-check before falling back to the queue.
		cc.mu.RLock()
		turnOpen = cc.isBusyLocked()
		cc.mu.RUnlock()
		if !turnOpen {
			log.Printf("[watchdog] dropping nudge for %s: turn ended during status-channel send", shortID(clawID))
			return
		}
		log.Printf("[watchdog] nudge over status channel for %s failed, queueing: %v", shortID(clawID), err)
		s.injectHubMessageByID(clawID, text)
		return
	}
	msg := types.HubMessage{ID: uuid.New().String(), ClawID: clawID, TenantID: tenantID, Role: "hub", Content: text, Format: "pre", CreatedAt: now()}
	_, _ = s.db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,?)`, msg.ID, msg.ClawID, msg.TenantID, msg.Role, msg.Content, msg.Format, msg.CreatedAt, msg.CreatedAt)
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: msg})
}

// sendNextQueuedMessage delivers the oldest pending message if the claw is idle.
func (s *Server) sendNextQueuedMessage(cc *clawConn) {
	cc.mu.Lock()
	if cc.isBusyLocked() || cc.deliveryInFlight || cc.noProgressPaused {
		cc.mu.Unlock()
		return
	}
	// No inter-turn cooldown here: if the next message reaches OpenClaw while
	// the previous turn is still releasing its prompt lock, the bridge retries
	// the rejected sessions.send on the same session with backoff (see
	// sessionLockConflictRetryDelays in cmd/claw-bridge), which preserves the
	// transcript instead of forcing a session rotation.
	// Claim the in-flight guard before querying so a concurrent caller cannot
	// read the same pending row and re-deliver it after we mark it delivered.
	cc.deliveryInFlight = true
	conn := cc.conn
	tenantID := cc.tenantID
	clawID := cc.id
	cc.mu.Unlock()
	defer func() {
		cc.mu.Lock()
		cc.deliveryInFlight = false
		cc.mu.Unlock()
	}()

	var msg types.HubMessage
	err := s.db.QueryRow(`SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at
		FROM messages WHERE claw_id=? AND tenant_id=? AND delivered_at IS NULL
		ORDER BY created_at, rowid LIMIT 1`, clawID, tenantID).Scan(
		&msg.ID, &msg.ClawID, &msg.TenantID, &msg.Role, &msg.Content, &msg.Format, &msg.CreatedAt)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		log.Printf("[hub] find pending message for %s: %v", shortID(clawID), err)
		return
	}

	// Re-check the claw is still idle before writing. A reconnect always
	// allocates a fresh clawConn, so cc.conn cannot change under us; a write
	// to a dead socket fails and leaves the row pending for the new connection.
	cc.mu.Lock()
	if cc.isBusyLocked() || cc.noProgressPaused {
		cc.mu.Unlock()
		return
	}
	// Reserve the turn before the WebSocket write. The bridge starts work as
	// soon as it receives this frame, while chunks and activity can arrive
	// later. Without this reservation, concurrent workflow injections can both
	// observe an idle claw and start back-to-back model turns.
	cc.awaitingResponse = true
	cc.streamingStartedAt = time.Now()
	cc.streamingTimeoutSent = false
	cc.contextWarningSent = false
	cc.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = wsjson.Write(ctx, conn, types.WSMessage{Type: "message", Payload: msg})
	if err != nil {
		cc.mu.Lock()
		cc.finishTurnLocked()
		cc.mu.Unlock()
		log.Printf("[hub] failed to deliver pending message to %s: %v", shortID(clawID), err)
		return
	}
	if _, err := s.db.Exec(`UPDATE messages SET delivered_at=? WHERE id=? AND delivered_at IS NULL`, now(), msg.ID); err != nil {
		log.Printf("[hub] mark delivered message %s: %v", msg.ID, err)
		return
	}
	cc.mu.Lock()
	cc.lastUserMessageAt = time.Now()
	cc.unresponsiveWarnedAt = time.Time{}
	cc.mu.Unlock()

	// Signal to UI that agent is working
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type: "agent_typing",
		Payload: map[string]string{
			"claw_id": clawID,
			"status":  "typing",
		},
	})

	log.Printf("[hub] delivered pending message to %s", shortID(clawID))
}
