package hub_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

func TestGitHubIssuesIntegrationWebhookCreatesFactoryClaw(t *testing.T) {
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
	if count != 1 {
		t.Fatalf("github issues integration webhook created %d claw(s), want 1", count)
	}
}

func TestGitHubIssuesIntegrationWebhookHonorsExcludeLabels(t *testing.T) {
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
			ExcludeLabels: []string{"Bug"},
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

	ghi.SetIssue("testorg/testrepo", 43, factorytest.IssueState{Title: "Bug Issue", Body: "Test body", State: "open", Labels: []string{"Bug"}})
	payload, sig := ghi.BuildWebhookPayload("testorg/testrepo", 43, "closed", "open")
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/integrations/github-issues/webhook", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Delivery", "delivery-testorg-testrepo-43")

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
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/43'`).Scan(&count); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if count != 0 {
		t.Fatalf("github issues integration webhook created %d claw(s), want 0", count)
	}
}

func TestGitHubIssuesIntegrationWebhookRejectsUnsignedWithoutConfiguredSecret(t *testing.T) {
	ghi := factorytest.NewMockGitHubIssues(t)
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
		}},
		Integrations: &types.IntegrationsConfig{
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{
				Workspace: "test-workspace",
				Token:     "test-github-issues-token",
			}},
		},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}

	s, _ := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	ghi.SetIssue("testorg/testrepo", 44, factorytest.IssueState{Title: "Unsigned Issue", Body: "Test body", State: "open"})
	payload, _ := ghi.BuildWebhookPayload("testorg/testrepo", 44, "closed", "open")
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/integrations/github-issues/webhook", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Delivery", "delivery-unsigned-no-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
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

func TestGitHubIssuesWorkflowPollUsesActualLabeler(t *testing.T) {
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
	saveGitHubIssueLabeledWorkflowFixtureWithLabelers(t, "workspace-a", []string{"alice"})

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{
		Title:  "Test Issue",
		Body:   "Test body",
		State:  "open",
		Labels: []string{"agent-ready"},
	})
	ghi.SetIssueEvents("testorg/testrepo", 42, []map[string]interface{}{{
		"id":         1,
		"event":      "labeled",
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"actor":      map[string]interface{}{"login": "alice"},
		"label":      map[string]interface{}{"name": "agent-ready"},
	}})

	s.PollIntegrationsForTest()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/42'`).Scan(&count); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if count != 1 {
		t.Fatalf("poll created %d claws for labeler-restricted GitHub issue, want 1", count)
	}
}

func TestGitHubIssuesWorkflowPollQueriesEachWorkspaceToken(t *testing.T) {
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
	s, _ := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")
	saveGitHubIssueLabeledWorkflowFixtureWithLabelersAndToken(t, "workspace-a", []string{"*"}, "token-a")
	saveGitHubIssueLabeledWorkflowFixtureWithLabelersAndToken(t, "workspace-b", []string{"*"}, "token-b")

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{
		Title:  "Test Issue",
		Body:   "Test body",
		State:  "open",
		Labels: []string{"agent-ready"},
	})

	s.PollIntegrationsForTest()

	gotA := ghi.AuthHeaderCount("Bearer token-a")
	gotB := ghi.AuthHeaderCount("Bearer token-b")
	if gotA < 1 {
		t.Fatalf("poll used token-a %d times, want at least 1", gotA)
	}
	if gotB < 1 {
		t.Fatalf("poll used token-b %d times, want at least 1", gotB)
	}
	if gotA+gotB != 3 {
		t.Fatalf("poll auth calls = token-a:%d token-b:%d, want two issue polls plus one comments fetch", gotA, gotB)
	}
}

func TestGitHubIssuesWorkspaceWebhookContextIncludesAllIssueComments(t *testing.T) {
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

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{Title: "Test Issue", Body: "Main issue body", State: "open"})
	ghi.SetIssueComments("testorg/testrepo", 42, []factorytest.IssueCommentState{{
		ID:        101,
		Body:      "First existing comment",
		User:      "alice",
		CreatedAt: "2026-06-05T20:37:00Z",
	}, {
		ID:        102,
		Body:      "Second existing comment",
		User:      "bob",
		CreatedAt: "2026-06-05T20:38:00Z",
	}, {
		ID:        103,
		Body:      "Third existing comment",
		User:      "carol",
		CreatedAt: "2026-06-05T20:39:00Z",
	}})
	payload, _ := ghi.BuildWebhookPayload("testorg/testrepo", 42, "closed", "open")
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/workspaces/workspace-a/webhooks/github-issues", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", hmacSHA256(payload, "secret"))
	req.Header.Set("X-GitHub-Delivery", "delivery-comments-context")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	context := waitForGitHubIssueContext(t, db, "testorg/testrepo/42")
	for _, want := range []string{"Main issue body", "First existing comment", "Second existing comment", "Third existing comment", "**Author:** @alice", "**Author:** @bob", "**Author:** @carol"} {
		if !strings.Contains(context, want) {
			t.Fatalf("CONTEXT.md missing %q:\n%s", want, context)
		}
	}
}

func TestGitHubIssuesWorkflowPollContextIncludesAllIssueComments(t *testing.T) {
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
		Body:   "Main issue body",
		State:  "open",
		Labels: []string{"agent-ready"},
	})
	ghi.SetIssueComments("testorg/testrepo", 42, []factorytest.IssueCommentState{{
		ID:        201,
		Body:      "First poll comment",
		User:      "alice",
		CreatedAt: "2026-06-05T20:37:00Z",
	}, {
		ID:        202,
		Body:      "Second poll comment",
		User:      "bob",
		CreatedAt: "2026-06-05T20:38:00Z",
	}})

	s.PollIntegrationsForTest()

	context := waitForGitHubIssueContext(t, db, "testorg/testrepo/42")
	for _, want := range []string{"Main issue body", "First poll comment", "Second poll comment", "**Author:** @alice", "**Author:** @bob"} {
		if !strings.Contains(context, want) {
			t.Fatalf("CONTEXT.md missing %q:\n%s", want, context)
		}
	}
}

func TestGitHubIssuesWorkflowCreateFailureCommentsLabelsAndAssignsTriggerActor(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	ghi := factorytest.NewMockGitHubIssues(t)
	ghi.WebhookSecret = "secret"
	li := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{ClawToken: "test-claw-token"}
	s, db := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")
	saveGitHubIssueWorkflowFixtureWithProviderAndErrorLabel(t, "workspace-a", "secret", "missing-provider", "agent-error")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{Title: "Test Issue", Body: "Main issue body", State: "open"})
	payload, _ := ghi.BuildWebhookPayload("testorg/testrepo", 42, "closed", "open")
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/workspaces/workspace-a/webhooks/github-issues", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", hmacSHA256(payload, "secret"))
	req.Header.Set("X-GitHub-Delivery", "delivery-create-failure")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	comment := waitForGitHubIssuePostedComment(t, ghi, "testorg/testrepo", 42)
	for _, want := range []string{
		"@testuser ElasticClaw could not finish this implementation.",
		"Status code: 500",
		"ElasticClaw could not find a valid execution provider.",
		"Check the ElasticClaw workspace/workflow configuration, then re-trigger the workflow.",
	} {
		if !strings.Contains(comment, want) {
			t.Fatalf("comment missing %q:\n%s", want, comment)
		}
	}
	if strings.Contains(comment, "provider \"missing-provider\" is not configured") {
		t.Fatalf("comment exposed raw provider error:\n%s", comment)
	}
	waitForGitHubIssueAssignee(t, ghi, "testorg/testrepo", 42, "testuser")
	waitForGitHubIssueLabel(t, ghi, "testorg/testrepo", 42, "agent-error")
	var claws int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id='testorg/testrepo/42'`).Scan(&claws); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if claws != 0 {
		t.Fatalf("created %d claws, want 0 for creation failure", claws)
	}
}

func TestGitHubIssuesWorkflowCreateFailureIgnoresErrorLabelWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	ghi := factorytest.NewMockGitHubIssues(t)
	ghi.WebhookSecret = "secret"
	li := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{ClawToken: "test-claw-token"}
	s, _ := hub.NewTestServerWithConfig(t, cfg, ghi.URL, li.URL, "")
	saveGitHubIssueLabeledWorkflowFixtureWithProviderAndErrorLabel(t, "workspace-a", "secret", "missing-provider", "agent-error")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	ghi.SetIssue("testorg/testrepo", 42, factorytest.IssueState{
		Title:  "Test Issue",
		Body:   "Main issue body",
		State:  "open",
		Labels: []string{"agent-ready"},
	})
	postGitHubIssuesWebhook(t, httpSrv.URL, "workspace-a", "delivery-ready", labeledGitHubIssuePayload(t, ghi, "testorg/testrepo", 42, "agent-ready"))

	waitForGitHubIssueLabel(t, ghi, "testorg/testrepo", 42, "agent-error")
	postGitHubIssuesWebhook(t, httpSrv.URL, "workspace-a", "delivery-error-label", labeledGitHubIssuePayload(t, ghi, "testorg/testrepo", 42, "agent-error"))

	assertGitHubIssueCommentCountStable(t, ghi, "testorg/testrepo", 42, 1)
}

func waitForGitHubIssueContext(t *testing.T, db *sql.DB, issueID string) string {
	t.Helper()
	var filesJSON string
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := db.QueryRow(`SELECT template_files FROM claws WHERE github_issue_id=? LIMIT 1`, issueID).Scan(&filesJSON)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("query claw template files for %s: %v", issueID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	var files map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		t.Fatalf("parse template files: %v", err)
	}
	return files["CONTEXT.md"]
}

func waitForGitHubIssuePostedComment(t *testing.T, ghi *factorytest.MockGitHubIssues, repo string, number int) string {
	t.Helper()
	comments := waitForGitHubIssuePostedComments(t, ghi, repo, number, 1)
	return comments[len(comments)-1]
}

func waitForGitHubIssuePostedComments(t *testing.T, ghi *factorytest.MockGitHubIssues, repo string, number, wantAtLeast int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		comments := ghi.PostedComments(repo, number)
		if len(comments) >= wantAtLeast {
			return comments
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for GitHub issue comment")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertGitHubIssueCommentCountStable(t *testing.T, ghi *factorytest.MockGitHubIssues, repo string, number, want int) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		comments := ghi.PostedComments(repo, number)
		if len(comments) != want {
			t.Fatalf("posted %d failure comments, want %d: %#v", len(comments), want, comments)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForGitHubIssueAssignee(t *testing.T, ghi *factorytest.MockGitHubIssues, repo string, number int, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := ghi.Issue(repo, number).Assignee; got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("assignee = %q, want %q", ghi.Issue(repo, number).Assignee, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForGitHubIssueLabel(t *testing.T, ghi *factorytest.MockGitHubIssues, repo string, number int, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if containsString(ghi.Issue(repo, number).Labels, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("labels = %v, want %q", ghi.Issue(repo, number).Labels, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func saveGitHubIssueWorkflowFixture(t *testing.T, workspace, secret string) {
	t.Helper()
	saveGitHubIssueWorkflowFixtureWithProviderAndErrorLabel(t, workspace, secret, "noop", "")
}

func saveGitHubIssueWorkflowFixtureWithProviderAndErrorLabel(t *testing.T, workspace, secret, provider, agentStatusError string) {
	t.Helper()
	hub.SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          workspace,
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: " + workspace + "\nprovider: " + provider + "\n",
				"CONTEXT.md":              "Test context\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion: "v1",
			Name:          "test-workflow",
			Trigger: &types.WorkflowTrigger{
				GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
					Event:            "issue_reopened",
					Repositories:     []string{"testorg/testrepo"},
					States:           []string{"open"},
					AgentStatusError: agentStatusError,
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
	hub.SaveWorkspaceIssueTrackerForTest(t, workspace, "github-issues", "default", "test-github-issues-token", secret)
}

func saveGitHubIssueLabeledWorkflowFixtureWithProviderAndErrorLabel(t *testing.T, workspace, secret, provider, agentStatusError string) {
	t.Helper()
	hub.SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          workspace,
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: " + workspace + "\nprovider: " + provider + "\n",
				"CONTEXT.md":              "Test context\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion: "v1",
			Name:          "test-workflow",
			Trigger: &types.WorkflowTrigger{
				GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
					Event:            "issue_labeled",
					Repositories:     []string{"testorg/testrepo"},
					States:           []string{"open"},
					Labels:           []string{"agent-ready"},
					Labelers:         []string{"*"},
					AgentStatusError: agentStatusError,
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
	hub.SaveWorkspaceIssueTrackerForTest(t, workspace, "github-issues", "default", "test-github-issues-token", secret)
}

func postGitHubIssuesWebhook(t *testing.T, serverURL, workspace, deliveryID string, payload []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/workspaces/"+workspace+"/webhooks/github-issues", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", hmacSHA256(payload, "secret"))
	req.Header.Set("X-GitHub-Delivery", deliveryID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func labeledGitHubIssuePayload(t *testing.T, ghi *factorytest.MockGitHubIssues, repo string, number int, label string) []byte {
	t.Helper()
	issue := ghi.Issue(repo, number)
	if !containsString(issue.Labels, label) {
		issue.Labels = append(issue.Labels, label)
		ghi.SetIssue(repo, number, issue)
	}
	payload := map[string]interface{}{
		"action": "labeled",
		"issue": map[string]interface{}{
			"id":           number,
			"number":       number,
			"title":        issue.Title,
			"body":         issue.Body,
			"html_url":     fmt.Sprintf("https://github.com/%s/issues/%d", repo, number),
			"state":        issue.State,
			"state_reason": "",
			"labels":       labelsToNameMapsForTest(issue.Labels),
			"assignee":     nil,
			"user":         map[string]interface{}{"login": "testuser", "type": "User"},
		},
		"repository": map[string]interface{}{"full_name": repo},
		"sender":     map[string]interface{}{"login": "testuser", "type": "User"},
		"label":      map[string]interface{}{"name": label},
	}
	out, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return out
}

func labelsToNameMapsForTest(labels []string) []map[string]string {
	out := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, map[string]string{"name": label})
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func saveGitHubIssueLabeledWorkflowFixture(t *testing.T, workspace string) {
	t.Helper()
	saveGitHubIssueLabeledWorkflowFixtureWithLabelers(t, workspace, []string{"*"})
}

func saveGitHubIssueLabeledWorkflowFixtureWithLabelers(t *testing.T, workspace string, labelers []string) {
	t.Helper()
	saveGitHubIssueLabeledWorkflowFixtureWithLabelersAndToken(t, workspace, labelers, "test-github-issues-token")
}

func saveGitHubIssueLabeledWorkflowFixtureWithLabelersAndToken(t *testing.T, workspace string, labelers []string, token string) {
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
					Labelers:     labelers,
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
	hub.SaveWorkspaceIssueTrackerForTest(t, workspace, "github-issues", "default", token, "")
}

func hmacSHA256(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
