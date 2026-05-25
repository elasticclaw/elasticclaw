package hub_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

func TestGitHubIssuesIntegrationWebhookIsIgnored(t *testing.T) {
	ghi := factorytest.NewMockGitHubIssues(t)
	ghi.WebhookSecret = "test-webhook-secret"
	li := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{{
			Name:          "legacy",
			Integration:   "github-issues",
			Workspace:     "test-workspace",
			TriggerStatus: "open",
			Template:      "elasticclaw",
			Provider:      "noop",
			TriggerRepos:  []string{"testorg/testrepo"},
			WebhookSecret: "test-webhook-secret",
		}},
		Integrations: &types.IntegrationsConfig{
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{
				Workspace:     "test-workspace",
				Token:         "test-github-issues-token",
				WebhookSecret: "test-webhook-secret",
			}},
		},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}

	s, db := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{Title: "Test Issue", Body: "Test body", State: "open"})
	payload, sig := ghi.BuildWebhookPayload("testorg/testrepo", 42, "closed", "open")
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/integrations/github-issues/webhook", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Delivery", "delivery-testorg-testrepo-42")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/42'`).Scan(&count); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if count != 0 {
		t.Fatalf("legacy integration webhook created %d claw(s), want 0", count)
	}
}

func TestGitHubIssuesWorkspaceWebhookOnlyDispatchesThatWorkspace(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	ghi := factorytest.NewMockGitHubIssues(t)
	ghi.WebhookSecret = "secret-a"
	li := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")

	saveGitHubIssueWorkflowFixture(t, "workspace-a", "secret-a")
	saveGitHubIssueWorkflowFixture(t, "workspace-b", "secret-b")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{Title: "Test Issue", Body: "Test body", State: "open"})
	payload, _ := ghi.BuildWebhookPayload("testorg/testrepo", 42, "closed", "open")
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/workspaces/workspace-a/webhooks/github-issues", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", hmacSHA256(payload, "secret-a"))
	req.Header.Set("X-GitHub-Delivery", "delivery-workspace-a")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	time.Sleep(100 * time.Millisecond)
	var total, workspaceA, workspaceB int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/42'`).Scan(&total); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/42' AND template='workspace-a'`).Scan(&workspaceA); err != nil {
		t.Fatalf("count workspace-a claws: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/42' AND template='workspace-b'`).Scan(&workspaceB); err != nil {
		t.Fatalf("count workspace-b claws: %v", err)
	}
	if total != 1 || workspaceA != 1 || workspaceB != 0 {
		t.Fatalf("counts total=%d workspace-a=%d workspace-b=%d, want 1/1/0", total, workspaceA, workspaceB)
	}
}

func TestGitHubIssuesWorkspaceWebhookIsIdempotentForSameIssue(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	ghi := factorytest.NewMockGitHubIssues(t)
	ghi.WebhookSecret = "secret"
	li := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")
	saveGitHubIssueWorkflowFixture(t, "workspace-a", "secret")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{Title: "Test Issue", Body: "Test body", State: "open"})
	payload, _ := ghi.BuildWebhookPayload("testorg/testrepo", 42, "closed", "open")

	var wg sync.WaitGroup
	for _, deliveryID := range []string{"delivery-a", "delivery-b"} {
		wg.Add(1)
		go func(deliveryID string) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/workspaces/workspace-a/webhooks/github-issues", strings.NewReader(string(payload)))
			if err != nil {
				t.Errorf("request: %v", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", hmacSHA256(payload, "secret"))
			req.Header.Set("X-GitHub-Delivery", deliveryID)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("post: %v", err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		}(deliveryID)
	}
	wg.Wait()

	var count int
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/42'`).Scan(&count); err != nil {
			t.Fatalf("count claws: %v", err)
		}
		if count > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count != 1 {
		t.Fatalf("created %d claws for the same GitHub issue, want 1", count)
	}
}

func TestGitHubIssuesWorkflowPollCreatesOnceForMissedWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	ghi := factorytest.NewMockGitHubIssues(t)
	li := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")
	saveGitHubIssueLabeledWorkflowFixture(t, "workspace-a")

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{
		Title:  "Test Issue",
		Body:   "Test body",
		State:  "open",
		Labels: []string{"agent-ready"},
	})

	s.PollIntegrationsForTest()
	s.PollIntegrationsForTest()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/42'`).Scan(&count); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if count != 1 {
		t.Fatalf("poll created %d claws for the same GitHub issue, want 1", count)
	}
}

func saveGitHubIssueWorkflowFixture(t *testing.T, workspace, secret string) {
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
			Name:          "test-workflow",
			Trigger: &types.WorkflowTrigger{
				Type:         "github_issues",
				Event:        "issue_reopened",
				Repositories: []string{"testorg/testrepo"},
				States:       []string{"open"},
			},
			Stages: []types.WorkflowStage{{
				ID:    "working",
				Label: "Working",
				Entry: true,
				OnEnter: map[string]interface{}{
					"inject": "Read your CONTEXT.md and start working on the issue.\n",
				},
			}},
		}},
	)
	hub.SaveWorkspaceIssueTrackerForTest(t, workspace, "github-issues", "default", "test-github-issues-token", secret)
}

func saveGitHubIssueLabeledWorkflowFixture(t *testing.T, workspace string) {
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
			Name:          "test-workflow",
			Trigger: &types.WorkflowTrigger{
				GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
					Event:        "issue_labeled",
					Repositories: []string{"testorg/testrepo"},
					States:       []string{"open"},
					Labels:       []string{"agent-ready"},
					Labelers:     []string{"*"},
				},
			},
			Stages: []types.WorkflowStage{{
				ID:    "working",
				Label: "Working",
				Entry: true,
				OnEnter: map[string]interface{}{
					"inject": "Read your CONTEXT.md and start working on the issue.\n",
				},
			}},
		}},
	)
	hub.SaveWorkspaceIssueTrackerForTest(t, workspace, "github-issues", "default", "test-github-issues-token", "")
}

func hmacSHA256(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
