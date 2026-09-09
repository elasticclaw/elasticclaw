//go:build !production

package hub

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// NewTestServerWithConfig creates a Server for integration testing with mock backends.
// Only call from tests. Uses the provided githubBaseURL, linearBaseURL, and
// shortcutBaseURL to override external API calls so tests can use httptest.Server instances.
func NewTestServerWithConfig(t interface {
	Helper()
	Cleanup(func())
}, cfg *types.HubConfig, githubBaseURL, linearBaseURL, shortcutBaseURL string) (*Server, *sql.DB) {
	db, err := openDB(":memory:")
	if err != nil {
		panic("NewTestServerWithConfig: openDB: " + err.Error())
	}
	// Keep SQLite in-memory on a single connection so all goroutines see the same DB.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	if cfg == nil {
		cfg = &types.HubConfig{}
	}

	// Provision a default tenant for the test server
	_, _ = db.Exec(
		`INSERT OR IGNORE INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,datetime('now'))`,
		"test-tenant-id", "test", "test-token", cfg.ClawToken,
	)

	// Push an empty "elasticclaw" template so resolveTemplateFiles doesn't fail
	for _, factory := range cfg.Factories {
		if factory.Template != "" {
			_, _ = db.Exec(
				`INSERT OR IGNORE INTO hub_templates(name,files,created_at,updated_at) VALUES(?,?,datetime('now'),datetime('now'))`,
				factory.Template, `{}`,
			)
		}
	}
	artifacts, err := artifact.NewLocalStore(tTempDir(t))
	if err != nil {
		panic("NewTestServerWithConfig: artifact store: " + err.Error())
	}

	s := &Server{
		db:                       db,
		hubCfg:                   cfg,
		artifacts:                artifacts,
		claws:                    make(map[string]*clawConn),
		users:                    make(map[string]*userConn),
		gatewayUnhealthyCounts:   make(map[string]int),
		gatewayEscalatedAt:       make(map[string]time.Time),
		dependencyStatus:         newDependencyStatusService(cfg),
		webhookDedup:             make(map[string]time.Time),
		githubBaseURL:            githubBaseURL,
		linearBaseURL:            linearBaseURL,
		shortcutBaseURL:          shortcutBaseURL,
		ticketMetadataEnrichment: make(chan struct{}, 32),
		ticketCursorKey:          randomTicketCursorKey(),
	}
	s.attachLLMUsageLimitsToDependencyStatus(s.dependencyStatus)
	// Register routes (same as Run but without serving web UI or starting relay)
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	s.mux = mux
	return s, db
}

func tTempDir(t interface {
	Helper()
	Cleanup(func())
}) string {
	type tempDir interface {
		TempDir() string
	}
	if td, ok := t.(tempDir); ok {
		return td.TempDir()
	}
	return ""
}

// SaveWorkspaceForTest writes a workspace and its workflows through the same
// external storage path used by the hub.
func SaveWorkspaceForTest(t interface {
	Helper()
	Fatalf(string, ...interface{})
}, workspace *types.WorkspaceConfig, workflows []*types.WorkflowConfig) {
	t.Helper()
	if err := saveExternalWorkspace(workspace); err != nil {
		t.Fatalf("save workspace: %v", err)
	}
	if len(workflows) > 0 {
		if err := saveExternalWorkflows(workspace.Name, workflows); err != nil {
			t.Fatalf("save workflows: %v", err)
		}
	}
}

// SaveWorkspaceIssueTrackerForTest writes a workspace-managed issue tracker.
func SaveWorkspaceIssueTrackerForTest(t interface {
	Helper()
	Fatalf(string, ...interface{})
}, workspace, trackerType, name, token, webhookSecret string) {
	t.Helper()
	if err := saveWorkspaceIssueTracker(workspace, trackerType, name, workspaceIssueTracker{
		Token:         token,
		WebhookSecret: webhookSecret,
	}); err != nil {
		t.Fatalf("save workspace issue tracker: %v", err)
	}
}

// SaveWorkspaceIssueTrackerWithBaseForTest writes a workspace-managed issue tracker
// with tracker-specific connection details.
func SaveWorkspaceIssueTrackerWithBaseForTest(t interface {
	Helper()
	Fatalf(string, ...interface{})
}, workspace, trackerType, name, baseURL, username, token, webhookSecret string) {
	t.Helper()
	if err := saveWorkspaceIssueTracker(workspace, trackerType, name, workspaceIssueTracker{
		BaseURL:       baseURL,
		Username:      username,
		Token:         token,
		WebhookSecret: webhookSecret,
	}); err != nil {
		t.Fatalf("save workspace issue tracker: %v", err)
	}
}

// Handler returns the server's HTTP handler (mux). Must be called after setupRoutes.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// PollPRsForTest triggers an immediate PR poll (for testing without waiting for the ticker).
func (s *Server) PollPRsForTest() {
	s.pollAllPRs()
}

// StartPRWatcherForTest starts the PR watcher background goroutine.
func (s *Server) StartPRWatcherForTest() {
	s.startPRWatcher()
}

// PollIntegrationsForTest triggers an immediate integration poll tick (for testing without waiting for the ticker).
func (s *Server) PollIntegrationsForTest() {
	s.pollTick()
}

// ClearWebhookDedupForTest empties the in-memory webhook dedup cache so tests can
// exercise the durable factory_triggers claim rather than the short-lived 5s window.
func (s *Server) ClearWebhookDedupForTest() {
	s.webhookDedupMu.Lock()
	defer s.webhookDedupMu.Unlock()
	for k := range s.webhookDedup {
		delete(s.webhookDedup, k)
	}
}
