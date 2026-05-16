package factorytest

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type TestServer struct {
	Server   *hub.Server
	HTTPSrv  *httptest.Server
	GitHub   *MockGitHub
	Linear   *MockLinear
	Shortcut *MockShortcut
	DB       *sql.DB
}

func (ts *TestServer) URL() string { return ts.HTTPSrv.URL }

func (ts *TestServer) ClawToken() string { return "test-claw-token" }

func (ts *TestServer) WaitForClawWithIssue(t *testing.T, issueID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var clawID string
		ts.DB.QueryRow(`SELECT id FROM claws WHERE linear_issue_id=?`, issueID).Scan(&clawID)
		if clawID != "" {
			return clawID
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("WaitForClawWithIssue: no claw for issue %s after %v", issueID, timeout)
	return ""
}

func (ts *TestServer) WaitForClawWithStory(t *testing.T, storyID string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var clawID string
		ts.DB.QueryRow(`SELECT id FROM claws WHERE shortcut_story_id=?`, storyID).Scan(&clawID)
		if clawID != "" {
			return clawID
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("WaitForClawWithStory: no claw for story %s after %v", storyID, timeout)
	return ""
}

func (ts *TestServer) WaitForClawStatus(t *testing.T, clawID, status string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var current string
		ts.DB.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&current)
		if current == status {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var current string
	ts.DB.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&current)
	t.Fatalf("WaitForClawStatus: claw %s: want %q got %q after %v", clawID[:8], status, current, timeout)
}

func NewTestServer(t *testing.T) *TestServer {
	t.Helper()
	// Enable noop provider for tests
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	gh := NewMockGitHub(t)
	li := NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{
			{
				Name:          "test-factory",
				Integration:   "linear",
				Workspace:     "test-workspace",
				TriggerStatus: "In Progress",
				DoneStatus:    "done-state-id",
				Template:      "elasticclaw",
				Provider:      "noop",
				PipelineYAML: `stages:
  - id: working
    label: "Working"
    entry: true
    on_enter:
      inject: |
        Read your CONTEXT.md and start working on the issue.
`,
			},
		},
		Integrations: &types.IntegrationsConfig{
			Linear: []*types.LinearIntegrationConfig{
				{
					Workspace: "test-workspace",
					Token:     "test-linear-token",
				},
			},
		},
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}

	s, db := hub.NewTestServerWithConfig(t, cfg, gh.URL, li.URL)
	s.StartPRWatcherForTest()

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	return &TestServer{
		Server:  s,
		HTTPSrv: httpSrv,
		GitHub:  gh,
		Linear:  li,
		DB:      db,
	}
}

// NewTestServerWithShortcut creates a TestServer that includes Shortcut integration.
func NewTestServerWithShortcut(t *testing.T) *TestServer {
	t.Helper()
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	gh := NewMockGitHub(t)
	li := NewMockLinear(t)
	sc := NewMockShortcut(t)

	// Override HTTP client for Shortcut API calls to route to the mock server.
	// NOTE: This replaces the global http.DefaultClient for the duration of the test.
	// Tests using this helper must not run in parallel. A future refactor should
	// inject an *http.Client into the Server struct (like githubBaseURL/linearBaseURL)
	// instead of relying on global state.
	origClient := http.DefaultClient
	http.DefaultClient = &http.Client{
		Transport: &shortcutTestTransport{
			mockURL:  sc.URL,
			fallback: http.DefaultTransport,
		},
	}
	t.Cleanup(func() { http.DefaultClient = origClient })

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{
			{
				Name:          "test-factory",
				Integration:   "shortcut",
				Workspace:     "test-workspace",
				TriggerStatus: "In Progress",
				DoneStatus:    "Done",
				Template:      "elasticclaw",
				Provider:      "noop",
				WebhookSecret: "test-webhook-secret",
				PipelineYAML: `stages:
  - id: working
    label: "Working"
    entry: true
    on_enter:
      inject: |
        Read your CONTEXT.md and start working on the issue.
`,
			},
		},
		Integrations: &types.IntegrationsConfig{
			Shortcut: []*types.ShortcutIntegrationConfig{
				{
					Workspace: "test-workspace",
					Token:     "test-shortcut-token",
				},
			},
		},
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}

	s, db := hub.NewTestServerWithConfig(t, cfg, gh.URL, li.URL)
	s.StartPRWatcherForTest()

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	return &TestServer{
		Server:   s,
		HTTPSrv:  httpSrv,
		GitHub:   gh,
		Linear:   li,
		Shortcut: sc,
		DB:       db,
	}
}

// shortcutTestTransport intercepts Shortcut API calls and routes them to the mock server.
type shortcutTestTransport struct {
	mockURL  string
	fallback http.RoundTripper
}

func (t *shortcutTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "shortcut.com") {
		// Clone the request before mutating its URL to satisfy the RoundTripper contract.
		newURL := *req.URL
		newURL.Scheme = "http"
		newURL.Host = strings.TrimPrefix(t.mockURL, "http://")
		req = req.Clone(req.Context())
		req.URL = &newURL
	}
	return t.fallback.RoundTrip(req)
}
