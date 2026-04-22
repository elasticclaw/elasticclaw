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
	Server  *hub.Server
	HTTPSrv *httptest.Server
	GitHub  *MockGitHub
	Linear  *MockLinear
	DB      *sql.DB
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
