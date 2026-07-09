package hub

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/internal/webui"

	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
	"github.com/elasticclaw/elasticclaw/pkg/hub/claws"
	"github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"
	"github.com/elasticclaw/elasticclaw/pkg/hub/telemetry"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Server is the ElasticClaw hub.
//
// Locking (phase-2 item 2.3 — one mutex per subsystem, no global lock):
//
//   - cfgMu guards the hubCfg pointer (config hot-reload swaps the whole
//     struct; readers snapshot the fields they need under RLock).
//   - clawReg (claws.Registry) owns the claw-connection map with its own
//     RWMutex; per-connection mutable state is behind each Conn's Mu.
//   - userReg owns the user-session map with its own RWMutex.
//   - The remaining named mutexes (fileAckMu, checkpointMu, markerMu, …)
//     each guard exactly the fields declared next to them.
//
// Lock order: never hold two subsystem locks at once — snapshot under one
// lock, then act. Within the claw subsystem the order is registry lock
// first, then Conn.Mu (only inside Registry.Do/DoRead callbacks).
type Server struct {
	db        *sql.DB
	addr      string
	logger    *slog.Logger
	hubCfg    *types.HubConfig
	identity  *HubIdentity
	mux       *http.ServeMux
	artifacts artifact.Store
	metrics   *serverMetrics

	// cfgMu guards hubCfg (the settings-subsystem cache lock).
	cfgMu sync.RWMutex

	// clawReg is the claw-connection registry (own lock, zero value ready).
	clawReg claws.Registry
	// wsPool bounds concurrent WS message handlers (zero value ready,
	// default limit; NewServer applies hubCfg.WSWorkerLimit).
	wsPool claws.WorkerPool
	// userReg is the user-session registry (own lock, zero value ready).
	userReg userRegistry
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

	// markerMu makes the system-marker check+insert atomic (wake.go);
	// formerly a job of the global mutex.
	markerMu sync.Mutex

	// cronScheduler manages scheduled workflow runs
	cronScheduler *cronScheduler
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
		dependencyStatus:  newDependencyStatusService(hubCfg),
		fileAckWaiters:    make(map[string]chan types.FileAck),
		fileReadWaiters:   make(map[string]chan types.FileReadResp),
		checkpointWaiters: make(map[string]chan error),
		webhookDedup:      make(map[string]time.Time),
	}
	srv.wsPool.Limit = int64(hubCfg.WSWorkerLimit)

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
