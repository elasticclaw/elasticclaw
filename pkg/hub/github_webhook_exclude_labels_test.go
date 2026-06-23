package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestGitHubPRFactoryExcludeLabelsUsesBackingIssueLabels(t *testing.T) {
	ghi := newGitHubIssueLabelMock(t)
	ghi.setIssueLabels("elastic/claw", 42, []string{"Bug"})

	s, db := newGitHubLabelFactoryTestServer(t, ghi.URL(), []*types.FactoryConfig{{
		Name:          "generic-pr",
		Integration:   "github",
		Template:      "elasticclaw",
		Provider:      "noop",
		Repos:         []string{"elastic/claw"},
		WebhookSecret: "test-webhook-secret",
		Labels:        []string{"agent-ready"},
		ExcludeLabels: []string{"Bug"},
		Trigger:       &types.GitHubTrigger{On: "pull_request"},
	}})

	postSignedGitHubWebhook(t, s, "pull_request", githubPRWebhookBody("opened", "elastic/claw", 42, "alice", "main"))
	assertNoGitHubPRClaw(t, db, "https://github.com/elastic/claw/pull/42")
}

func TestGitHubPRFactoryLabelsAndExcludeLabelsMatchBackingIssueLabels(t *testing.T) {
	ghi := newGitHubIssueLabelMock(t)
	ghi.setIssueLabels("elastic/claw", 43, []string{"agent-ready"})

	s, db := newGitHubLabelFactoryTestServer(t, ghi.URL(), []*types.FactoryConfig{{
		Name:          "generic-pr",
		Integration:   "github",
		Template:      "elasticclaw",
		Provider:      "noop",
		Repos:         []string{"elastic/claw"},
		WebhookSecret: "test-webhook-secret",
		Labels:        []string{"agent-ready"},
		ExcludeLabels: []string{"Bug"},
		Trigger:       &types.GitHubTrigger{On: "pull_request"},
	}})

	postSignedGitHubWebhook(t, s, "pull_request", githubPRWebhookBody("opened", "elastic/claw", 43, "alice", "main"))
	waitForGitHubPRClaw(t, db, "https://github.com/elastic/claw/pull/43")
}

func TestGitHubIssueFactoryExcludeLabelsAllowsIssueWithoutExcludedLabel(t *testing.T) {
	ghi := newGitHubIssueLabelMock(t)
	ghi.setIssueLabels("elastic/claw", 44, []string{"agent-ready"})

	s, db := newGitHubLabelFactoryTestServer(t, ghi.URL(), []*types.FactoryConfig{{
		Name:          "generic-issue",
		Integration:   "github",
		Template:      "elasticclaw",
		Provider:      "noop",
		Repos:         []string{"elastic/claw"},
		WebhookSecret: "test-webhook-secret",
		Labels:        []string{"agent-ready"},
		ExcludeLabels: []string{"Bug"},
		Trigger:       &types.GitHubTrigger{On: "issue", Action: "opened"},
	}})

	postSignedGitHubWebhook(t, s, "issues", githubIssueWebhookBody("opened", "elastic/claw", 44, []string{"agent-ready"}))
	waitForGitHubIssueClaw(t, db, "elastic/claw/44")
}

func TestGitHubIssueFactoryExcludeLabelsSkipsIssueWithExcludedLabel(t *testing.T) {
	ghi := newGitHubIssueLabelMock(t)
	ghi.setIssueLabels("elastic/claw", 45, []string{"agent-ready", "Bug"})

	s, db := newGitHubLabelFactoryTestServer(t, ghi.URL(), []*types.FactoryConfig{{
		Name:          "generic-issue",
		Integration:   "github",
		Template:      "elasticclaw",
		Provider:      "noop",
		Repos:         []string{"elastic/claw"},
		WebhookSecret: "test-webhook-secret",
		Labels:        []string{"agent-ready"},
		ExcludeLabels: []string{"Bug"},
		Trigger:       &types.GitHubTrigger{On: "issue", Action: "opened"},
	}})

	postSignedGitHubWebhook(t, s, "issues", githubIssueWebhookBody("opened", "elastic/claw", 45, []string{"agent-ready", "Bug"}))
	assertNoGitHubIssueClaw(t, db, "elastic/claw/45")
}

func newGitHubLabelFactoryTestServer(t *testing.T, githubBaseURL string, factories []*types.FactoryConfig) (*Server, *sql.DB) {
	t.Helper()
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: factories,
		Integrations: &types.IntegrationsConfig{
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{
				Workspace: "default",
				Token:     "test-github-token",
			}},
		},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}
	return NewTestServerWithConfig(t, cfg, githubBaseURL, "", "")
}

type githubIssueLabelMock struct {
	server *httptest.Server
	labels map[string][]string
}

func newGitHubIssueLabelMock(t *testing.T) *githubIssueLabelMock {
	t.Helper()
	mock := &githubIssueLabelMock{labels: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(path, "/")
		if r.Method != http.MethodGet || len(parts) != 4 || parts[2] != "issues" {
			if r.Method == http.MethodGet && len(parts) == 5 && parts[2] == "issues" && parts[4] == "comments" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("[]"))
				return
			}
			http.NotFound(w, r)
			return
		}
		var number int
		fmt.Sscanf(parts[3], "%d", &number)
		key := fmt.Sprintf("%s/%s#%d", parts[0], parts[1], number)
		labels, ok := mock.labels[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		respLabels := make([]map[string]string, 0, len(labels))
		for _, label := range labels {
			respLabels = append(respLabels, map[string]string{"name": label})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"number": number,
			"labels": respLabels,
		})
	})
	mock.server = httptest.NewServer(mux)
	t.Cleanup(mock.server.Close)
	return mock
}

func githubIssueWebhookBody(action, repo string, number int, labels []string) map[string]interface{} {
	rawLabels := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		rawLabels = append(rawLabels, map[string]string{"name": label})
	}
	return map[string]interface{}{
		"action": action,
		"issue": map[string]interface{}{
			"id":       number,
			"number":   number,
			"title":    "Test Issue",
			"body":     "Test body",
			"html_url": fmt.Sprintf("https://github.com/%s/issues/%d", repo, number),
			"state":    "open",
			"labels":   rawLabels,
			"user":     map[string]interface{}{"login": "alice", "type": "User"},
		},
		"repository": map[string]interface{}{
			"full_name": repo,
		},
		"sender": map[string]interface{}{"login": "alice", "type": "User"},
	}
}

func (m *githubIssueLabelMock) URL() string {
	return m.server.URL
}

func (m *githubIssueLabelMock) setIssueLabels(repo string, number int, labels []string) {
	m.labels[fmt.Sprintf("%s#%d", repo, number)] = append([]string(nil), labels...)
}

func githubPRWebhookBody(action, repo string, number int, author, baseBranch string) map[string]interface{} {
	return map[string]interface{}{
		"action": action,
		"number": number,
		"sender": map[string]interface{}{"login": author, "type": "User"},
		"repository": map[string]interface{}{
			"full_name": repo,
		},
		"pull_request": map[string]interface{}{
			"html_url": fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
			"title":    "Test PR",
			"user":     map[string]interface{}{"login": author},
			"head":     map[string]interface{}{"ref": "feature", "sha": "abc123"},
			"base":     map[string]interface{}{"ref": baseBranch},
		},
	}
}

func waitForGitHubPRClaw(t *testing.T, db *sql.DB, prURL string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE pr_url=?`, prURL).Scan(&count); err != nil {
			t.Fatalf("count PR claws: %v", err)
		}
		if count > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for PR claw for %s", prURL)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertNoGitHubPRClaw(t *testing.T, db *sql.DB, prURL string) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE pr_url=?`, prURL).Scan(&count); err != nil {
			t.Fatalf("count PR claws: %v", err)
		}
		if count != 0 {
			t.Fatalf("created %d PR claw(s) for %s, want 0", count, prURL)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForGitHubIssueClaw(t *testing.T, db *sql.DB, issueID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id=?`, issueID).Scan(&count); err != nil {
			t.Fatalf("count issue claws: %v", err)
		}
		if count > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for issue claw for %s", issueID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertNoGitHubIssueClaw(t *testing.T, db *sql.DB, issueID string) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id=?`, issueID).Scan(&count); err != nil {
			t.Fatalf("count issue claws: %v", err)
		}
		if count != 0 {
			t.Fatalf("created %d issue claw(s) for %s, want 0", count, issueID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
