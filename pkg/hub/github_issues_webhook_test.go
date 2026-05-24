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
