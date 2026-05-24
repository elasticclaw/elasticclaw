package factorytest

import (
	"database/sql"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type TestServer struct {
	Server       *hub.Server
	HTTPSrv      *httptest.Server
	GitHub       *MockGitHub
	GitHubIssues *MockGitHubIssues
	Linear       *MockLinear
	Shortcut     *MockShortcut
	DB           *sql.DB
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

	s, db := hub.NewTestServerWithConfig(t, cfg, gh.URL, li.URL, "")
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
	gh := NewMockGitHub(t)
	li := NewMockLinear(t)
	sc := NewMockShortcut(t)

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

	s, db := hub.NewTestServerWithConfig(t, cfg, gh.URL, li.URL, sc.URL)
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

// NewTestServerWithGitHubIssues creates a TestServer that includes GitHub Issues integration.
func NewTestServerWithGitHubIssues(t *testing.T) *TestServer {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	gh := NewMockGitHub(t)
	ghi := NewMockGitHubIssues(t)
	ghi.WebhookSecret = "test-webhook-secret"
	li := NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	hub.SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "test-workspace",
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: test-workspace\nprovider: noop\n",
				"CONTEXT.md":              "Test context\n",
			},
		},
		[]*types.WorkflowConfig{
			{
				SchemaVersion: "v1",
				Name:          "test-workflow",
				Trigger: &types.WorkflowTrigger{
					Type:         "github_issues",
					Event:        "issue_edited",
					Repositories: []string{"testorg/testrepo"},
					States:       []string{"open"},
				},
				Stages: []types.WorkflowStage{
					{
						ID:    "working",
						Label: "Working",
						Entry: true,
						OnEnter: map[string]interface{}{
							"inject": "Read your CONTEXT.md and start working on the issue.\n",
						},
					},
				},
			},
		},
	)
	hub.SaveWorkspaceIssueTrackerForTest(t, "test-workspace", "github-issues", "default", "test-github-issues-token", "test-webhook-secret")

	s, db := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")
	s.StartPRWatcherForTest()

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	return &TestServer{
		Server:       s,
		HTTPSrv:      httpSrv,
		GitHub:       gh,
		GitHubIssues: ghi,
		Linear:       li,
		DB:           db,
	}
}
