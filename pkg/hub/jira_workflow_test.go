package hub_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestJiraWorkflowWebhookCreatesClaw(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	cfg := jiraWorkflowTestConfig()
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", "")
	saveJiraWorkflowFixture(t, "workspace-a")
	hub.SaveWorkspaceIssueTrackerWithBaseForTest(t, "workspace-a", "jira", "default", "https://jira.example.test", "", "jira-token", "jira-secret")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	payload := jiraWebhookPayload(t, "EC-123", "EC", "Backlog", "Ready for Agent", []string{"agent"})
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/workspaces/workspace-a/webhooks/jira", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ElasticClaw-Webhook-Secret", "jira-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	waitForJiraClawCount(t, db, "EC-123", 1)
}

func TestJiraGlobalWebhookCreatesClaw(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	cfg := jiraWorkflowTestConfig()
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", "")
	saveJiraWorkflowFixture(t, "workspace-a")
	hub.SaveWorkspaceIssueTrackerWithBaseForTest(t, "workspace-a", "jira", "default", "https://jira.example.test", "", "jira-token", "jira-secret")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	payload := jiraWebhookPayload(t, "EC-123", "EC", "Backlog", "Ready for Agent", []string{"agent"})
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/integrations/jira/webhook", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ElasticClaw-Webhook-Secret", "jira-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	waitForJiraClawCount(t, db, "EC-123", 1)
}

func TestJiraWorkflowPollCreatesOnceForMissedWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	jira := newMockJira(t)
	cfg := jiraWorkflowTestConfig()
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", "")
	saveJiraWorkflowFixture(t, "workspace-a")
	hub.SaveWorkspaceIssueTrackerWithBaseForTest(t, "workspace-a", "jira", "default", jira.URL, "", "jira-token", "")
	jira.setIssue("EC-123", "EC", "Ready for Agent", []string{"agent"})

	s.PollIntegrationsForTest()
	s.PollIntegrationsForTest()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE jira_issue_id='EC-123'`).Scan(&count); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if count != 1 {
		t.Fatalf("poll created %d claws for the same Jira issue, want 1", count)
	}
}

func waitForJiraClawCount(t *testing.T, db *sql.DB, issueID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE jira_issue_id=?`, issueID).Scan(&last); err != nil {
			t.Fatalf("count claws: %v", err)
		}
		if last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("created %d Jira claws, want %d", last, want)
}

func jiraWorkflowTestConfig() *types.HubConfig {
	return &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
}

func saveJiraWorkflowFixture(t *testing.T, workspace string) {
	t.Helper()
	hub.SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          workspace,
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: " + workspace + "\nprovider: noop\n",
				"CONTEXT.md":              "Test context\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion: "v1",
			Name:          "jira-workflow",
			Trigger: &types.WorkflowTrigger{
				Jira: &types.JiraWorkflowTrigger{
					Event:    "status_changed",
					Projects: []string{"EC"},
					States:   []string{"Ready for Agent"},
					Labels:   []string{"agent"},
				},
			},
			Stages: []types.WorkflowStage{{
				ID:    "working",
				Label: "Working",
				Entry: true,
				OnEnter: map[string]interface{}{
					"inject": "Read your CONTEXT.md and start working on the Jira issue.\n",
				},
			}},
		}},
	)
}

func jiraWebhookPayload(t *testing.T, key, project, previousStatus, status string, labels []string) []byte {
	t.Helper()
	body := map[string]interface{}{
		"webhookEvent": "jira:issue_updated",
		"timestamp":    int64(1710000000000),
		"user": map[string]interface{}{
			"accountId":   "user-123",
			"displayName": "Jira User",
		},
		"issue": map[string]interface{}{
			"id":  "10001",
			"key": key,
			"fields": map[string]interface{}{
				"summary":     "Test Jira issue",
				"description": "Do the Jira task",
				"labels":      labels,
				"status": map[string]interface{}{
					"name": status,
				},
				"project": map[string]interface{}{
					"key": project,
				},
			},
		},
		"changelog": map[string]interface{}{
			"id": "20001",
			"items": []map[string]interface{}{{
				"field":      "status",
				"fromString": previousStatus,
				"toString":   status,
			}},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Jira payload: %v", err)
	}
	return data
}

type mockJira struct {
	*httptest.Server
	issue map[string]interface{}
}

func newMockJira(t *testing.T) *mockJira {
	t.Helper()
	m := &mockJira{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/2/search":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"issues": []interface{}{m.issue}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/2/issue/"):
			_ = json.NewEncoder(w).Encode(m.issue)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mockJira) setIssue(key, project, status string, labels []string) {
	m.issue = map[string]interface{}{
		"id":   "10001",
		"key":  key,
		"self": m.URL + "/rest/api/2/issue/" + key,
		"fields": map[string]interface{}{
			"summary":     "Test Jira issue",
			"description": "Do the Jira task",
			"labels":      labels,
			"status": map[string]interface{}{
				"name": status,
			},
			"project": map[string]interface{}{
				"key": project,
			},
		},
	}
}
