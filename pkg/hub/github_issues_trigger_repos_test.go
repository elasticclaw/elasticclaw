package hub_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestGitHubIssuesWebhookFiltersByTriggerRepos(t *testing.T) {
	ghi := factorytest.NewMockGitHubIssues(t)
	ghi.WebhookSecret = "test-webhook-secret"
	li := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{
			{
				Name:          "org-a",
				Integration:   "github-issues",
				Workspace:     "test-workspace",
				TriggerStatus: "open",
				Template:      "elasticclaw",
				Provider:      "noop",
				TriggerRepos:  []string{"org-a/*"},
				WebhookSecret: "test-webhook-secret",
			},
			{
				Name:          "org-b",
				Integration:   "github-issues",
				Workspace:     "test-workspace",
				TriggerStatus: "open",
				Template:      "elasticclaw",
				Provider:      "noop",
				TriggerRepos:  []string{"org-b/repo"},
				WebhookSecret: "test-webhook-secret",
			},
		},
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

	ghi.SetIssue("org-b/repo", 42, factorytest.IssueState{Title: "Test Issue", Body: "Test body", State: "open"})
	postGitHubIssuesWebhook(t, httpSrv.URL, ghi, "org-b/repo", 42)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='org-b/repo/42'`).Scan(&count); err != nil {
			t.Fatalf("count claws: %v", err)
		}
		if count == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var factoryName string
	if err := db.QueryRow(`SELECT factory_name FROM claws WHERE github_issue_id='org-b/repo/42'`).Scan(&factoryName); err != nil {
		t.Fatalf("load claw factory: %v", err)
	}
	if factoryName != "org-b" {
		t.Fatalf("factory_name = %q, want org-b", factoryName)
	}
}

func TestGitHubIssuesWebhookWithoutTriggerReposMatchesAllRepos(t *testing.T) {
	ghi := factorytest.NewMockGitHubIssues(t)
	ghi.WebhookSecret = "test-webhook-secret"
	li := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{{
			Name:          "catch-all",
			Integration:   "github-issues",
			Workspace:     "test-workspace",
			TriggerStatus: "open",
			Template:      "elasticclaw",
			Provider:      "noop",
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

	ghi.SetIssue("any-org/any-repo", 42, factorytest.IssueState{Title: "Test Issue", Body: "Test body", State: "open"})
	postGitHubIssuesWebhook(t, httpSrv.URL, ghi, "any-org/any-repo", 42)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='any-org/any-repo/42'`).Scan(&count); err != nil {
			t.Fatalf("count claws: %v", err)
		}
		if count == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("expected catch-all github-issues factory to create one claw")
}

func postGitHubIssuesWebhook(t *testing.T, hubURL string, ghi *factorytest.MockGitHubIssues, repo string, issueNumber int) {
	t.Helper()
	payload, sig := ghi.BuildWebhookPayload(repo, issueNumber, "closed", "open")
	req, err := http.NewRequest("POST", hubURL+"/api/integrations/github-issues/webhook", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("github issues webhook request build failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Delivery", "delivery-"+strings.ReplaceAll(repo, "/", "-"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("github issues webhook post failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook returned status %d", resp.StatusCode)
	}
}
