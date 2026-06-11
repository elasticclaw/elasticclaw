package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestPollHumanInlineReviewCommentForwardsToClaw(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-inline-poll", "elastic/claw", 75, 20, 0)
	startTaskRunForTest(t, s, "claw-inline-poll", "inline-poll")

	pr := clawPR{
		id:                  "pr-claw-inline-poll",
		clawID:              "claw-inline-poll",
		repo:                "elastic/claw",
		prNumber:            75,
		prURL:               "https://github.com/elastic/claw/pull/75",
		lastReviewCommentID: 20,
	}

	s.checkGreptileReviewComments(pr, []interface{}{
		map[string]interface{}{
			"id":       float64(21),
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
			"body":     "inline concern",
			"html_url": "https://github.com/elastic/claw/pull/75#discussion_r21",
			"path":     "pkg/hub/server.go",
			"line":     float64(42),
		},
	})

	assertMessageContains(t, db, "claw-inline-poll", "hub", []string{
		"@reviewer",
		"inline review comment",
		"pkg/hub/server.go:42",
		"inline concern",
		"https://github.com/elastic/claw/pull/75#discussion_r21",
	})
}

func TestPollHumanRequestedChangesReviewForwardsToClaw(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-review-poll", "elastic/claw", 77, 0, 200)
	startTaskRunForTest(t, s, "claw-review-poll", "review-poll")

	pr := clawPR{
		id:           "pr-claw-review-poll",
		clawID:       "claw-review-poll",
		repo:         "elastic/claw",
		prNumber:     77,
		prURL:        "https://github.com/elastic/claw/pull/77",
		lastReviewID: 200,
	}

	s.checkPRReviews(pr, []interface{}{
		map[string]interface{}{
			"id":       float64(201),
			"state":    "CHANGES_REQUESTED",
			"body":     "Please address the inline comments.",
			"html_url": "https://github.com/elastic/claw/pull/77#pullrequestreview-201",
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
	})

	assertMessageContains(t, db, "claw-review-poll", "hub", []string{
		"@reviewer",
		"requested changes",
		"Please address the inline comments.",
		"https://github.com/elastic/claw/pull/77#pullrequestreview-201",
	})
}

func TestGitHubPullRequestReviewCommentWebhookInjectsHumanInlineComment(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-inline-webhook", "elastic/claw", 81, 20, 0)
	startTaskRunForTest(t, s, "claw-inline-webhook", "inline-webhook")

	body := map[string]interface{}{
		"action": "created",
		"comment": map[string]interface{}{
			"id":       21,
			"body":     "inline webhook concern",
			"html_url": "https://github.com/elastic/claw/pull/81#discussion_r21",
			"path":     "pkg/hub/github_webhook.go",
			"line":     88,
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{
			"number":   81,
			"html_url": "https://github.com/elastic/claw/pull/81",
		},
		"repository": map[string]interface{}{"full_name": "elastic/claw"},
	}

	postSignedGitHubWebhook(t, s, "pull_request_review_comment", body)

	waitForMessageContains(t, db, "claw-inline-webhook", "hub", []string{
		"@reviewer",
		"inline review comment",
		"pkg/hub/github_webhook.go:88",
		"inline webhook concern",
	})
}

func TestGitHubPullRequestReviewWebhookInjectsChangesRequested(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-review-webhook", "elastic/claw", 82, 0, 200)
	startTaskRunForTest(t, s, "claw-review-webhook", "review-webhook")

	body := map[string]interface{}{
		"action": "submitted",
		"review": map[string]interface{}{
			"id":       201,
			"state":    "changes_requested",
			"body":     "Please fix the review findings.",
			"html_url": "https://github.com/elastic/claw/pull/82#pullrequestreview-201",
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{
			"number":   82,
			"html_url": "https://github.com/elastic/claw/pull/82",
		},
		"repository": map[string]interface{}{"full_name": "elastic/claw"},
	}

	postSignedGitHubWebhook(t, s, "pull_request_review", body)

	waitForMessageContains(t, db, "claw-review-webhook", "hub", []string{
		"@reviewer",
		"requested changes",
		"Please fix the review findings.",
		"https://github.com/elastic/claw/pull/82#pullrequestreview-201",
	})
}

func TestReviewCommentWebhookAndStalePollDoNotDuplicateMessage(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-inline-race", "elastic/claw", 83, 20, 0)
	startTaskRunForTest(t, s, "claw-inline-race", "inline-race")

	body := map[string]interface{}{
		"action": "created",
		"comment": map[string]interface{}{
			"id":       21,
			"body":     "race inline concern",
			"html_url": "https://github.com/elastic/claw/pull/83#discussion_r21",
			"path":     "pkg/hub/pr_watcher.go",
			"line":     620,
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{
			"number":   83,
			"html_url": "https://github.com/elastic/claw/pull/83",
		},
		"repository": map[string]interface{}{"full_name": "elastic/claw"},
	}

	postSignedGitHubWebhook(t, s, "pull_request_review_comment", body)
	waitForMessageContains(t, db, "claw-inline-race", "hub", []string{"race inline concern"})

	stalePollSnapshot := clawPR{
		id:                  "pr-claw-inline-race",
		clawID:              "claw-inline-race",
		repo:                "elastic/claw",
		prNumber:            83,
		prURL:               "https://github.com/elastic/claw/pull/83",
		lastReviewCommentID: 20,
	}
	s.checkGreptileReviewComments(stalePollSnapshot, []interface{}{
		map[string]interface{}{
			"id":       float64(21),
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
			"body":     "race inline concern",
			"html_url": "https://github.com/elastic/claw/pull/83#discussion_r21",
			"path":     "pkg/hub/pr_watcher.go",
			"line":     float64(620),
		},
	})

	assertMessageCountContaining(t, db, "claw-inline-race", "hub", "race inline concern", 1)
}

func TestReviewWebhookAndStalePollDoNotDuplicateMessage(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-review-race", "elastic/claw", 84, 0, 200)
	startTaskRunForTest(t, s, "claw-review-race", "review-race")

	body := map[string]interface{}{
		"action": "submitted",
		"review": map[string]interface{}{
			"id":       201,
			"state":    "changes_requested",
			"body":     "race requested changes",
			"html_url": "https://github.com/elastic/claw/pull/84#pullrequestreview-201",
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{
			"number":   84,
			"html_url": "https://github.com/elastic/claw/pull/84",
		},
		"repository": map[string]interface{}{"full_name": "elastic/claw"},
	}

	postSignedGitHubWebhook(t, s, "pull_request_review", body)
	waitForMessageContains(t, db, "claw-review-race", "hub", []string{"race requested changes"})

	stalePollSnapshot := clawPR{
		id:           "pr-claw-review-race",
		clawID:       "claw-review-race",
		repo:         "elastic/claw",
		prNumber:     84,
		prURL:        "https://github.com/elastic/claw/pull/84",
		lastReviewID: 200,
	}
	s.checkPRReviews(stalePollSnapshot, []interface{}{
		map[string]interface{}{
			"id":       float64(201),
			"state":    "CHANGES_REQUESTED",
			"body":     "race requested changes",
			"html_url": "https://github.com/elastic/claw/pull/84#pullrequestreview-201",
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
	})

	assertMessageCountContaining(t, db, "claw-review-race", "hub", "race requested changes", 1)
}

func TestOutOfOrderReviewCommentWebhooksDeliverEachComment(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-inline-order", "elastic/claw", 85, 20, 0)
	startTaskRunForTest(t, s, "claw-inline-order", "inline-order")

	postSignedGitHubWebhook(t, s, "pull_request_review_comment", map[string]interface{}{
		"action": "created",
		"comment": map[string]interface{}{
			"id":       23,
			"body":     "newer inline concern",
			"html_url": "https://github.com/elastic/claw/pull/85#discussion_r23",
			"path":     "pkg/hub/newer.go",
			"line":     23,
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{"number": 85, "html_url": "https://github.com/elastic/claw/pull/85"},
		"repository":   map[string]interface{}{"full_name": "elastic/claw"},
	})
	waitForMessageContains(t, db, "claw-inline-order", "hub", []string{"newer inline concern"})

	postSignedGitHubWebhook(t, s, "pull_request_review_comment", map[string]interface{}{
		"action": "created",
		"comment": map[string]interface{}{
			"id":       21,
			"body":     "older inline concern",
			"html_url": "https://github.com/elastic/claw/pull/85#discussion_r21",
			"path":     "pkg/hub/older.go",
			"line":     21,
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{"number": 85, "html_url": "https://github.com/elastic/claw/pull/85"},
		"repository":   map[string]interface{}{"full_name": "elastic/claw"},
	})

	waitForMessageContains(t, db, "claw-inline-order", "hub", []string{"older inline concern"})
	assertMessageCountContaining(t, db, "claw-inline-order", "hub", "inline concern", 2)
}

func TestPollRecoversLowerReviewCommentAfterHigherWebhook(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-inline-recover", "elastic/claw", 86, 20, 0)
	startTaskRunForTest(t, s, "claw-inline-recover", "inline-recover")

	postSignedGitHubWebhook(t, s, "pull_request_review_comment", map[string]interface{}{
		"action": "created",
		"comment": map[string]interface{}{
			"id":       23,
			"body":     "webhook comment 23",
			"html_url": "https://github.com/elastic/claw/pull/86#discussion_r23",
			"path":     "pkg/hub/newer.go",
			"line":     23,
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{"number": 86, "html_url": "https://github.com/elastic/claw/pull/86"},
		"repository":   map[string]interface{}{"full_name": "elastic/claw"},
	})
	waitForMessageContains(t, db, "claw-inline-recover", "hub", []string{"webhook comment 23"})

	stalePollSnapshot := clawPR{
		id:                  "pr-claw-inline-recover",
		clawID:              "claw-inline-recover",
		repo:                "elastic/claw",
		prNumber:            86,
		prURL:               "https://github.com/elastic/claw/pull/86",
		lastReviewCommentID: 20,
	}
	s.checkGreptileReviewComments(stalePollSnapshot, []interface{}{
		map[string]interface{}{
			"id":       float64(21),
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
			"body":     "poll recovered comment 21",
			"html_url": "https://github.com/elastic/claw/pull/86#discussion_r21",
			"path":     "pkg/hub/older.go",
			"line":     float64(21),
		},
		map[string]interface{}{
			"id":       float64(23),
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
			"body":     "webhook comment 23",
			"html_url": "https://github.com/elastic/claw/pull/86#discussion_r23",
			"path":     "pkg/hub/newer.go",
			"line":     float64(23),
		},
	})

	assertMessageCountContaining(t, db, "claw-inline-recover", "hub", "poll recovered comment 21", 1)
	assertMessageCountContaining(t, db, "claw-inline-recover", "hub", "webhook comment 23", 1)
}

func TestOutOfOrderReviewWebhooksDeliverEachReview(t *testing.T) {
	s, db := newPRReviewFeedbackTestServer(t)
	seedTrackedPRForReviewFeedback(t, db, "claw-review-order", "elastic/claw", 87, 0, 200)
	startTaskRunForTest(t, s, "claw-review-order", "review-order")

	postSignedGitHubWebhook(t, s, "pull_request_review", map[string]interface{}{
		"action": "submitted",
		"review": map[string]interface{}{
			"id":       203,
			"state":    "changes_requested",
			"body":     "newer requested changes",
			"html_url": "https://github.com/elastic/claw/pull/87#pullrequestreview-203",
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{"number": 87, "html_url": "https://github.com/elastic/claw/pull/87"},
		"repository":   map[string]interface{}{"full_name": "elastic/claw"},
	})
	waitForMessageContains(t, db, "claw-review-order", "hub", []string{"newer requested changes"})

	postSignedGitHubWebhook(t, s, "pull_request_review", map[string]interface{}{
		"action": "submitted",
		"review": map[string]interface{}{
			"id":       201,
			"state":    "changes_requested",
			"body":     "older requested changes",
			"html_url": "https://github.com/elastic/claw/pull/87#pullrequestreview-201",
			"user":     map[string]interface{}{"login": "reviewer", "type": "User"},
		},
		"pull_request": map[string]interface{}{"number": 87, "html_url": "https://github.com/elastic/claw/pull/87"},
		"repository":   map[string]interface{}{"full_name": "elastic/claw"},
	})

	waitForMessageContains(t, db, "claw-review-order", "hub", []string{"older requested changes"})
	assertMessageCountContaining(t, db, "claw-review-order", "hub", "requested changes", 2)
}

func newPRReviewFeedbackTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{{
			Name:          "github-prs",
			Integration:   "github",
			Template:      "elasticclaw",
			Provider:      "noop",
			Repos:         []string{"elastic/claw"},
			WebhookSecret: "test-webhook-secret",
			Trigger:       &types.GitHubTrigger{On: "pull_request"},
		}},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}
	return NewTestServerWithConfig(t, cfg, "", "", "")
}

func seedTrackedPRForReviewFeedback(t *testing.T, db execer, clawID, repo string, prNumber int, lastReviewCommentID, lastReviewID int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, provider, status, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", clawID, "elasticclaw", "noop", "connected",
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, last_review_comment_id, last_review_id, created_at)
		 VALUES(?,?,?,?,?,?,?,datetime('now'))`,
		"pr-"+clawID, clawID, repo, prNumber, "https://github.com/"+repo+"/pull/"+strconv.Itoa(prNumber), lastReviewCommentID, lastReviewID,
	); err != nil {
		t.Fatalf("insert claw_prs: %v", err)
	}
}

type execer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func postSignedGitHubWebhook(t *testing.T, s *Server, event string, body map[string]interface{}) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal webhook payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/github/webhook", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signGitHubWebhookForReviewFeedback(payload, "test-webhook-secret"))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, want 200 body=%q", rr.Code, rr.Body.String())
	}
}

func signGitHubWebhookForReviewFeedback(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func waitForMessageContains(t *testing.T, db querier, clawID, role string, parts []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if messageContains(t, db, clawID, role, parts) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("message for claw %s role %s did not contain %v", clawID, role, parts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertMessageContains(t *testing.T, db querier, clawID, role string, parts []string) {
	t.Helper()
	if !messageContains(t, db, clawID, role, parts) {
		t.Fatalf("message for claw %s role %s did not contain %v", clawID, role, parts)
	}
}

func assertMessageCountContaining(t *testing.T, db querier, clawID, role, content string, want int) {
	t.Helper()
	rows, err := db.Query(`SELECT content FROM messages WHERE claw_id=? AND role=?`, clawID, role)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	got := 0
	for rows.Next() {
		var message string
		if err := rows.Scan(&message); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		if strings.Contains(message, content) {
			got++
		}
	}
	if got != want {
		t.Fatalf("messages containing %q = %d, want %d", content, got, want)
	}
}

type querier interface {
	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryRow(query string, args ...interface{}) *sql.Row
}

func messageContains(t *testing.T, db querier, clawID, role string, parts []string) bool {
	t.Helper()
	rows, err := db.Query(`SELECT content FROM messages WHERE claw_id=? AND role=?`, clawID, role)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		matches := true
		for _, part := range parts {
			if !strings.Contains(content, part) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func assertReviewWatermark(t *testing.T, db querier, clawID, column string, want int64) {
	t.Helper()
	if column != "last_review_comment_id" && column != "last_review_id" {
		t.Fatalf("unexpected watermark column %q", column)
	}
	var got int64
	if err := db.QueryRow(`SELECT `+column+` FROM claw_prs WHERE claw_id=?`, clawID).Scan(&got); err != nil {
		t.Fatalf("query watermark: %v", err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", column, got, want)
	}
}
