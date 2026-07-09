package httpserver

import (
	"net/http"
	"os"
)

// Auth wraps handlers with the hub's authentication policies. It is
// implemented by the hub server; defining it here keeps the route table in
// this package without httpserver depending on hub internals.
type Auth interface {
	// WithAuth requires a valid API token (tenant or web session).
	WithAuth(next http.HandlerFunc) http.HandlerFunc
	// WithWebAuth requires a valid web UI session.
	WithWebAuth(next http.HandlerFunc) http.HandlerFunc
	// WithWebAdminAuth requires a web UI session with admin rights.
	WithWebAdminAuth(next http.HandlerFunc) http.HandlerFunc
	// WithAdminForMethods requires admin auth for the listed methods and
	// regular auth for everything else.
	WithAdminForMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc
}

// Handlers lists every endpoint the router mounts. Fields are plain
// http.HandlerFunc so the handlers can stay as unexported methods of
// hub.Server while the pkg/hub split proceeds one subpackage at a time; as
// handlers move into their own packages only the composition site changes.
type Handlers struct {
	// Metrics serves the Prometheus exposition format; nil disables the
	// /metrics route (hand-built test servers have no metrics registry).
	Metrics http.Handler

	// WebSockets
	ClawWS http.HandlerFunc
	UserWS http.HandlerFunc

	// Auth + branding
	Login               http.HandlerFunc
	WebLogin            http.HandlerFunc
	WebLogout           http.HandlerFunc
	WebMe               http.HandlerFunc
	AuthConfig          http.HandlerFunc
	GitHubClientID      http.HandlerFunc
	GitHubOAuthExchange http.HandlerFunc
	Branding            http.HandlerFunc

	// Settings
	HubConfig            http.HandlerFunc
	Settings             http.HandlerFunc
	SettingsStatus       http.HandlerFunc
	GitHubAppTest        http.HandlerFunc
	ModelAuthLogin       http.HandlerFunc
	ModelAuthLoginStatus http.HandlerFunc

	// Template store
	Templates      http.HandlerFunc
	TemplateDetail http.HandlerFunc

	// Integration webhooks (signature-validated, no session auth)
	LinearWebhook       http.HandlerFunc
	GitHubWebhook       http.HandlerFunc
	GitHubIssuesWebhook http.HandlerFunc
	ShortcutWebhook     http.HandlerFunc
	JiraWebhook         http.HandlerFunc
	ExternalWebhook     http.HandlerFunc

	// Factories + analytics
	FactoryEvents                 http.HandlerFunc
	FactoryTrigger                http.HandlerFunc
	FactoryAnalytics              http.HandlerFunc
	FactoriesCRUD                 http.HandlerFunc
	AllFactoriesAnalytics         http.HandlerFunc
	TaskRunAnalyticsSummary       http.HandlerFunc
	TaskRunAnalyticsFilterOptions http.HandlerFunc
	TaskRunAnalyticsRuns          http.HandlerFunc
	DependencyStatus              http.HandlerFunc

	// Workspaces + workflows
	WorkspacesCRUD             http.HandlerFunc
	WorkspaceWorkflowsList     http.HandlerFunc
	WorkspaceWorkflowDetail    http.HandlerFunc
	WorkspaceWorkflowTrigger   http.HandlerFunc
	CronWorkflowTrigger        http.HandlerFunc
	CronWorkflowRuns           http.HandlerFunc
	CronWorkflowNextRun        http.HandlerFunc
	WorkspaceSecretsCRUD       http.HandlerFunc
	WorkspaceGitHubAppsCRUD    http.HandlerFunc
	WorkspaceIssueTrackersCRUD http.HandlerFunc

	// Claws + resources
	SecretsCRUD          http.HandlerFunc
	MCPCrud              http.HandlerFunc
	Claws                http.HandlerFunc
	ClawDetail           http.HandlerFunc
	CheckpointBlobUpload http.HandlerFunc
	CheckpointInternal   http.HandlerFunc
	VolumeArchive        http.HandlerFunc
	Terminal             http.HandlerFunc
	GitHubToken          http.HandlerFunc
	Messages             http.HandlerFunc
	FileUpload           http.HandlerFunc
	FileView             http.HandlerFunc
	ClawSubresource      http.HandlerFunc

	// AI Config
	AIConfigApply         http.HandlerFunc
	AIConfigRevert        http.HandlerFunc
	AIConfigBackup        http.HandlerFunc
	AIConfigStream        http.HandlerFunc
	AIConfigCurrentConfig http.HandlerFunc
	AIConfig              http.HandlerFunc

	// Doctor / Troubleshoot
	Doctor             http.HandlerFunc
	TroubleshootStream http.HandlerFunc

	// Debug
	DebugClaws http.HandlerFunc
}

// RegisterRoutes mounts the hub's route table on mux, applying the auth
// policies per route. Route patterns, registration order and per-route auth
// wrapping are preserved verbatim from the previous hub.Server.registerRoutes.
func RegisterRoutes(mux *http.ServeMux, auth Auth, h Handlers) {
	// Prometheus metrics (exposition format; no auth, like most exporters —
	// keep the port private or firewall the path if that is a concern).
	if h.Metrics != nil {
		mux.Handle("/metrics", h.Metrics)
	}

	// Claw WebSocket
	mux.HandleFunc("/claw/ws", h.ClawWS)

	// Browser WebSocket
	mux.HandleFunc("/api/ws", auth.WithAuth(h.UserWS))

	// REST API
	mux.HandleFunc("/api/login", h.Login)
	mux.HandleFunc("/api/auth/login", h.WebLogin)
	mux.HandleFunc("/api/auth/logout", h.WebLogout)
	mux.HandleFunc("/api/auth/me", auth.WithWebAuth(h.WebMe))
	mux.HandleFunc("/api/auth/config", h.AuthConfig)               // public — no auth required
	mux.HandleFunc("/api/auth/github/client-id", h.GitHubClientID) // public
	mux.HandleFunc("/api/auth/github/exchange", h.GitHubOAuthExchange)
	mux.HandleFunc("/api/branding", h.Branding) // public — no auth required
	mux.HandleFunc("/api/hub-config", auth.WithWebAdminAuth(h.HubConfig))
	mux.HandleFunc("/api/settings", auth.WithWebAdminAuth(h.Settings))
	mux.HandleFunc("/api/settings/status", auth.WithWebAdminAuth(h.SettingsStatus))
	mux.HandleFunc("/api/settings/github/test", auth.WithWebAdminAuth(h.GitHubAppTest))
	mux.HandleFunc("/api/settings/model-auth/login", auth.WithWebAdminAuth(h.ModelAuthLogin))
	mux.HandleFunc("/api/settings/model-auth/login/{id}", auth.WithWebAdminAuth(h.ModelAuthLoginStatus))

	// Template store
	mux.HandleFunc("/api/templates", auth.WithWebAuth(h.Templates))
	mux.HandleFunc("/api/templates/{name}", auth.WithWebAuth(h.TemplateDetail))

	// Integration webhooks (signature-validated, no session auth)
	mux.HandleFunc("/api/integrations/linear/webhook", h.LinearWebhook)
	mux.HandleFunc("/api/integrations/github/webhook", h.GitHubWebhook)
	mux.HandleFunc("/api/integrations/github-issues/webhook", h.GitHubIssuesWebhook)
	mux.HandleFunc("/api/integrations/shortcut/webhook", h.ShortcutWebhook)
	mux.HandleFunc("/api/integrations/jira/webhook", h.JiraWebhook)
	mux.HandleFunc("/api/integrations/external/webhook", h.ExternalWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/linear", h.LinearWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/github", h.GitHubWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/github-issues", h.GitHubIssuesWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/shortcut", h.ShortcutWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/jira", h.JiraWebhook)
	mux.HandleFunc("/api/workspaces/{workspace}/webhooks/external", h.ExternalWebhook)
	mux.HandleFunc("/api/factories/", auth.WithAuth(h.FactoryEvents))                                               // GET /api/factories/:name/events
	mux.HandleFunc("/api/factories/{name}/trigger", auth.WithAuth(h.FactoryTrigger))                                // POST manual trigger
	mux.HandleFunc("/api/factories/{name}/analytics", auth.WithAuth(h.FactoryAnalytics))                            // GET factory analytics
	mux.HandleFunc("/api/factories", auth.WithAdminForMethods(h.FactoriesCRUD, http.MethodPost, http.MethodDelete)) // factory CRUD (GET list, POST push)
	mux.HandleFunc("/api/analytics/factories", auth.WithAuth(h.AllFactoriesAnalytics))                              // GET all factories analytics
	mux.HandleFunc("/api/analytics/summary", auth.WithAuth(h.TaskRunAnalyticsSummary))
	mux.HandleFunc("/api/analytics/filter-options", auth.WithAuth(h.TaskRunAnalyticsFilterOptions))
	mux.HandleFunc("/api/analytics/runs", auth.WithAuth(h.TaskRunAnalyticsRuns))
	mux.HandleFunc("/api/analytics/runs/", auth.WithAuth(h.TaskRunAnalyticsRuns))
	mux.HandleFunc("/api/dependencies/status", auth.WithAuth(h.DependencyStatus))
	mux.HandleFunc("/api/workspaces", auth.WithAdminForMethods(h.WorkspacesCRUD, http.MethodPost, http.MethodDelete)) // workspace CRUD
	mux.HandleFunc("/api/workspaces/{name}/workflows", auth.WithAdminForMethods(h.WorkspaceWorkflowsList, http.MethodPost))
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}", auth.WithAdminForMethods(h.WorkspaceWorkflowDetail, http.MethodPatch))
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/trigger", auth.WithAuth(h.WorkspaceWorkflowTrigger))
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/cron/trigger", auth.WithAuth(h.CronWorkflowTrigger)) // POST manual trigger
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/cron/runs", auth.WithAuth(h.CronWorkflowRuns))       // GET run history
	mux.HandleFunc("/api/workspaces/{workspace}/workflows/{workflow}/cron/next", auth.WithAuth(h.CronWorkflowNextRun))    // GET next scheduled run
	mux.HandleFunc("/api/workspaces/{workspace}/secrets", auth.WithAdminForMethods(h.WorkspaceSecretsCRUD, http.MethodPut, http.MethodPost, http.MethodDelete))
	mux.HandleFunc("/api/workspaces/{workspace}/github-apps", auth.WithAdminForMethods(h.WorkspaceGitHubAppsCRUD, http.MethodPut, http.MethodPost, http.MethodDelete))
	mux.HandleFunc("/api/workspaces/{workspace}/issue-trackers", auth.WithAdminForMethods(h.WorkspaceIssueTrackersCRUD, http.MethodPut, http.MethodPost, http.MethodDelete))
	mux.HandleFunc("/api/secrets", auth.WithWebAdminAuth(h.SecretsCRUD)) // secrets CRUD (GET names, PUT upsert, DELETE)
	mux.HandleFunc("/api/mcp", auth.WithWebAdminAuth(h.MCPCrud))         // MCP server CRUD (GET list, PUT upsert, DELETE)
	mux.HandleFunc("/api/claws", auth.WithAuth(h.Claws))
	mux.HandleFunc("/api/claws/{id}", auth.WithAuth(h.ClawDetail))
	mux.HandleFunc("/api/checkpoints/blob/", h.CheckpointBlobUpload)
	mux.HandleFunc("/api/checkpoints/", h.CheckpointInternal)
	mux.HandleFunc("/api/volumes/leases/{lease}/archive", h.VolumeArchive)
	mux.HandleFunc("/api/terminal/", h.Terminal)
	mux.HandleFunc("/api/github/token/", h.GitHubToken) // credential helper endpoint (claw-token auth)
	mux.HandleFunc("/api/messages/", auth.WithAuth(h.Messages))
	mux.HandleFunc("/api/files/", auth.WithAuth(h.FileUpload))
	mux.HandleFunc("/api/files/view/", auth.WithAuth(h.FileView))
	mux.HandleFunc("/api/claws/", auth.WithAuth(h.ClawSubresource)) // /api/claws/:id/prs, /api/claws/:id/settings

	// AI Config
	// AI Config — register sub-paths before the bare path so Go's exact-match
	// ServeMux routes them correctly (avoids 404 on specific sub-paths).
	mux.HandleFunc("/api/settings/ai-config/apply", auth.WithWebAdminAuth(h.AIConfigApply))
	mux.HandleFunc("/api/settings/ai-config/revert", auth.WithWebAdminAuth(h.AIConfigRevert))
	mux.HandleFunc("/api/settings/ai-config/backup", auth.WithWebAdminAuth(h.AIConfigBackup))
	mux.HandleFunc("/api/settings/ai-config/stream", auth.WithWebAdminAuth(h.AIConfigStream))
	mux.HandleFunc("/api/settings/ai-config/current-config", auth.WithWebAdminAuth(h.AIConfigCurrentConfig))
	mux.HandleFunc("/api/settings/ai-config", auth.WithWebAdminAuth(h.AIConfig))

	// Doctor / Troubleshoot
	mux.HandleFunc("/api/doctor", auth.WithWebAdminAuth(h.Doctor))
	mux.HandleFunc("/api/troubleshoot/stream", auth.WithWebAdminAuth(h.TroubleshootStream))

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
				WriteErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
				return
			}
			http.ServeFile(w, r, bridgePath)
		})
	}

	// Debug: dump in-memory claw state (auth required)
	mux.HandleFunc("/api/debug/claws", auth.WithAuth(h.DebugClaws))
}
