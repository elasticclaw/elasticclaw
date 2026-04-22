//go:build !production

package hub

import (
	"database/sql"
	"net/http"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// NewTestServerWithConfig creates a Server for integration testing with mock backends.
// Only call from tests. Uses the provided githubBaseURL and linearBaseURL to override
// external API calls so tests can use httptest.Server instances.
func NewTestServerWithConfig(t interface {
	Helper()
	Cleanup(func())
}, cfg *types.HubConfig, githubBaseURL, linearBaseURL string) (*Server, *sql.DB) {
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

	s := &Server{
		db:            db,
		hubCfg:        cfg,
		claws:         make(map[string]*clawConn),
		users:         make(map[string]*userConn),
		githubBaseURL: githubBaseURL,
		linearBaseURL: linearBaseURL,
	}
	// Register routes (same as Run but without serving web UI or starting relay)
	s.setupRoutes()
	return s, db
}

// setupRoutes registers all HTTP handlers on s.mux without starting the HTTP server.
// This mirrors the Run() logic but is separated so tests can wrap with httptest.Server.
func (s *Server) setupRoutes() {
	mux := s.newMux(false)
	s.mux = mux
}

// newMux builds and returns the handler mux. noWebUI=true skips embedding the web UI.
func (s *Server) newMux(noWebUI bool) *http.ServeMux {
	mux := http.NewServeMux()

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

	// Integration webhooks
	mux.HandleFunc("/api/integrations/linear/webhook", s.handleLinearWebhook)
	mux.HandleFunc("/api/integrations/shortcut/webhook", s.handleShortcutWebhook)
	mux.HandleFunc("/api/factories/", s.withAuth(s.handleFactoryEvents))
	mux.HandleFunc("/api/claws", s.withAuth(s.handleClaws))
	mux.HandleFunc("/api/claws/{id}", s.withAuth(s.handleClawDetail))
	mux.HandleFunc("/api/terminal/", s.handleTerminal)
	mux.HandleFunc("/api/github/token/", s.handleGitHubToken)
	mux.HandleFunc("/api/messages/", s.withAuth(s.handleMessages))
	mux.HandleFunc("/api/claws/", s.withAuth(s.handleClawSubresource))

	// Health
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
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
