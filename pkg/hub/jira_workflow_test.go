package hub_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
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

func TestJiraWorkflowAutomationIssuePayloadCreatesClaw(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	cfg := jiraWorkflowTestConfig()
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", "")
	saveJiraWorkflowFixture(t, "workspace-a")
	hub.SaveWorkspaceIssueTrackerWithBaseForTest(t, "workspace-a", "jira", "default", "https://jira.example.test", "", "jira-token", "jira-secret")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	payload := jiraAutomationIssuePayload(t, "EC-123", "EC", "Backlog", "Ready for Agent", []string{"agent"})
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

func TestJiraGlobalWebhookAcceptsSecretQueryParam(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	cfg := jiraWorkflowTestConfig()
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", "")
	saveJiraWorkflowFixture(t, "workspace-a")
	hub.SaveWorkspaceIssueTrackerWithBaseForTest(t, "workspace-a", "jira", "default", "https://jira.example.test", "", "jira-token", "jira-secret")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	payload := jiraWebhookPayload(t, "EC-123", "EC", "Backlog", "Ready for Agent", []string{"agent"})
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/integrations/jira/webhook?secret=jira-secret", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
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

func TestJiraWorkflowExcludeLabelsBlockWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	cfg := jiraWorkflowTestConfig()
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", "")
	saveJiraWorkflowFixtureWithExclude(t, "workspace-a", []string{"blocked"})
	hub.SaveWorkspaceIssueTrackerWithBaseForTest(t, "workspace-a", "jira", "default", "https://jira.example.test", "", "jira-token", "jira-secret")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	payload := jiraWebhookPayload(t, "EC-123", "EC", "Backlog", "Ready for Agent", []string{"agent", "blocked"})
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

	assertJiraClawCountStable(t, db, "EC-123", 0)
}

func TestJiraFactoryExcludeLabelsBlockWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	jira := newMockJira(t)
	cfg := jiraFactoryTestConfig(jira.URL)
	cfg.Factories[0].ExcludeLabels = []string{"blocked"}
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", "")
	jira.setIssue("EC-123", "EC", "Ready for Agent", []string{"agent", "blocked"})

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	payload := jiraWebhookPayload(t, "EC-123", "EC", "Backlog", "Ready for Agent", []string{"agent", "blocked"})
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/integrations/jira/webhook", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	assertJiraClawCountStable(t, db, "EC-123", 0)
}

func TestJiraTerminateSignalCommentsIssue(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	jira := newMockJira(t)
	cfg := jiraFactoryTestConfig(jira.URL)
	s, db := hub.NewTestServerWithConfig(t, cfg, "", "", "")
	jira.setIssue("EC-123", "EC", "Ready for Agent", []string{"agent"})

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	payload := jiraWebhookPayload(t, "EC-123", "EC", "Backlog", "Ready for Agent", []string{"agent"})
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/integrations/jira/webhook", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	waitForJiraClawCount(t, db, "EC-123", 1)
	var clawID string
	if err := db.QueryRow(`SELECT id FROM claws WHERE jira_issue_id='EC-123'`).Scan(&clawID); err != nil {
		t.Fatalf("find claw: %v", err)
	}
	bridge := factorytest.ConnectFakeBridge(t, httpSrv.URL, clawID, "EC-123", "test-claw-token")
	bridge.SendMessage("Stopping now. [TERMINATE]")

	comment := jira.waitForComment(t, "EC-123")
	if !strings.Contains(comment, "Agent stopped: claw requested self-termination") {
		t.Fatalf("comment = %q, want self-termination message", comment)
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

func assertJiraClawCountStable(t *testing.T, db *sql.DB, issueID string, want int) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	var last int
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE jira_issue_id=?`, issueID).Scan(&last); err != nil {
			t.Fatalf("count claws: %v", err)
		}
		if last != want {
			t.Fatalf("created %d Jira claws, want %d", last, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func jiraWorkflowTestConfig() *types.HubConfig {
	return &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
}

func jiraFactoryTestConfig(jiraURL string) *types.HubConfig {
	return &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
		Integrations: &types.IntegrationsConfig{
			Jira: []*types.JiraIntegrationConfig{{
				Workspace: "workspace-a",
				BaseURL:   jiraURL,
				Token:     "jira-token",
			}},
		},
		Factories: []*types.FactoryConfig{{
			Name:          "jira-factory",
			Integration:   "jira",
			Workspace:     "workspace-a",
			Projects:      []string{"EC"},
			Labels:        []string{"agent"},
			TriggerStatus: "Ready for Agent",
			Template:      "jira-template",
			Provider:      "noop",
		}},
	}
}

func saveJiraWorkflowFixture(t *testing.T, workspace string) {
	saveJiraWorkflowFixtureWithExclude(t, workspace, nil)
}

func saveJiraWorkflowFixtureWithExclude(t *testing.T, workspace string, excludeLabels []string) {
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
					Event:         "status_changed",
					Projects:      []string{"EC"},
					States:        []string{"Ready for Agent"},
					Labels:        []string{"agent"},
					ExcludeLabels: append([]string(nil), excludeLabels...),
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

func jiraAutomationIssuePayload(t *testing.T, key, project, previousStatus, status string, labels []string) []byte {
	t.Helper()
	body := map[string]interface{}{
		"self": "https://jira.example.test/rest/api/2/issue/10001",
		"id":   10001,
		"key":  key,
		"changelog": map[string]interface{}{
			"startAt":    0,
			"maxResults": 1,
			"total":      1,
			"histories": []map[string]interface{}{{
				"id": "20001",
				"items": []map[string]interface{}{{
					"field":      "status",
					"fromString": previousStatus,
					"toString":   status,
				}},
			}},
		},
		"fields": map[string]interface{}{
			"summary":     "Test Jira issue",
			"description": "Do the Jira task",
			"labels":      labels,
			"updated":     int64(1710000000000),
			"status": map[string]interface{}{
				"name": status,
			},
			"project": map[string]interface{}{
				"key":  project,
				"name": "Test Project",
			},
			"assignee": map[string]interface{}{
				"accountId":   "user-123",
				"displayName": "Jira User",
			},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Jira automation payload: %v", err)
	}
	return data
}

type mockJira struct {
	*httptest.Server
	mu       sync.Mutex
	issue    map[string]interface{}
	comments map[string][]string
}

func newMockJira(t *testing.T) *mockJira {
	t.Helper()
	m := &mockJira{comments: map[string][]string{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/rest/api/2/search":
			m.mu.Lock()
			defer m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"issues": []interface{}{m.issue}})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/rest/api/2/issue/") && strings.HasSuffix(r.URL.Path, "/comment"):
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			key := strings.TrimPrefix(r.URL.Path, "/rest/api/2/issue/")
			key = strings.TrimSuffix(key, "/comment")
			m.mu.Lock()
			m.comments[key] = append(m.comments[key], payload.Body)
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "comment-1"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/rest/api/2/issue/"):
			m.mu.Lock()
			defer m.mu.Unlock()
			_ = json.NewEncoder(w).Encode(m.issue)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mockJira) setIssue(key, project, status string, labels []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
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

func (m *mockJira) waitForComment(t *testing.T, key string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		comments := append([]string(nil), m.comments[key]...)
		m.mu.Unlock()
		if len(comments) > 0 {
			return comments[len(comments)-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Jira comment on %s", key)
	return ""
}
