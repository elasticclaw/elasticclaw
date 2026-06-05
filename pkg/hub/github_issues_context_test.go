package hub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildGitHubIssuesContextIncludesAllComments(t *testing.T) {
	payload := githubIssuesWebhookPayload{}
	payload.Issue.Number = 358
	payload.Issue.Title = "Agents dont read all the content of an issue"
	payload.Issue.Body = "Main issue body"
	payload.Issue.HTMLURL = "https://github.com/elasticclaw/elasticclaw/issues/358"
	payload.Issue.User.Login = "AnaBerg"
	payload.Repository.FullName = "elasticclaw/elasticclaw"

	comments := []githubIssueComment{{
		ID:        101,
		Body:      "First comment with required detail",
		HTMLURL:   "https://github.com/elasticclaw/elasticclaw/issues/358#issuecomment-101",
		CreatedAt: "2026-06-05T20:37:00Z",
		User: githubIssueCommentUser{
			Login: "alice",
		},
	}, {
		ID:        102,
		Body:      "Second comment with extra acceptance criteria",
		HTMLURL:   "https://github.com/elasticclaw/elasticclaw/issues/358#issuecomment-102",
		CreatedAt: "2026-06-05T20:38:00Z",
		User: githubIssueCommentUser{
			Login: "bob",
		},
	}, {
		ID:        103,
		Body:      "Third comment with final instruction",
		HTMLURL:   "https://github.com/elasticclaw/elasticclaw/issues/358#issuecomment-103",
		CreatedAt: "2026-06-05T20:39:00Z",
		User: githubIssueCommentUser{
			Login: "carol",
		},
	}}

	got := buildGitHubIssuesContext(payload, comments, "")
	for _, want := range []string{
		"Main issue body",
		"## Comments",
		"First comment with required detail",
		"Second comment with extra acceptance criteria",
		"Third comment with final instruction",
		"**Author:** @alice",
		"**Author:** @bob",
		"**Author:** @carol",
		"**Created:** 2026-06-05T20:37:00Z",
		"**Created:** 2026-06-05T20:38:00Z",
		"**Created:** 2026-06-05T20:39:00Z",
		"https://github.com/elasticclaw/elasticclaw/issues/358#issuecomment-101",
		"https://github.com/elasticclaw/elasticclaw/issues/358#issuecomment-102",
		"https://github.com/elasticclaw/elasticclaw/issues/358#issuecomment-103",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("context missing %q:\n%s", want, got)
		}
	}
	firstPos := strings.Index(got, "First comment with required detail")
	secondPos := strings.Index(got, "Second comment with extra acceptance criteria")
	thirdPos := strings.Index(got, "Third comment with final instruction")
	if firstPos > secondPos {
		t.Fatalf("comments rendered out of order: first at %d, second at %d:\n%s", firstPos, secondPos, got)
	}
	if secondPos > thirdPos {
		t.Fatalf("comments rendered out of order: second at %d, third at %d:\n%s", secondPos, thirdPos, got)
	}
}

func TestBuildGitHubIssuesContextIncludesCommentFetchWarning(t *testing.T) {
	payload := githubIssuesWebhookPayload{}
	payload.Issue.Number = 358
	payload.Issue.Title = "Agents dont read all the content of an issue"
	payload.Repository.FullName = "elasticclaw/elasticclaw"

	got := buildGitHubIssuesContext(payload, nil, "Issue comments could not be loaded automatically. Review the issue URL for additional context.")
	if !strings.Contains(got, "Issue comments could not be loaded automatically. Review the issue URL for additional context.") {
		t.Fatalf("context missing fetch warning:\n%s", got)
	}
}

func TestFetchGitHubIssueCommentsFollowsPagination(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/testorg/testrepo/issues/42/comments" {
			t.Fatalf("unexpected comments request path: %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization header = %q, want bearer token", got)
		}
		switch r.URL.RawQuery {
		case "per_page=100":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/testorg/testrepo/issues/42/comments?per_page=100&page=2>; rel="next"`, srvURL))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":2,"body":"second by date","created_at":"2026-06-05T20:38:00Z","user":{"login":"bob"}}]`))
		case "per_page=100&page=2":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":1,"body":"first by date","created_at":"2026-06-05T20:37:00Z","user":{"login":"alice"}}]`))
		default:
			t.Fatalf("unexpected comments request query: %q", r.URL.RawQuery)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	comments, err := fetchGitHubIssueComments(srv.URL, "testorg/testrepo", 42, "test-token")
	if err != nil {
		t.Fatalf("fetch comments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments len = %d, want 2", len(comments))
	}
	if comments[0].Body != "first by date" || comments[1].Body != "second by date" {
		t.Fatalf("comments were not fetched and sorted oldest-first: %#v", comments)
	}
	if comments[0].ID != 1 || comments[0].CreatedAt != "2026-06-05T20:37:00Z" || comments[0].User.Login != "alice" {
		t.Fatalf("first comment fields incorrect: %#v", comments[0])
	}
	if comments[1].ID != 2 || comments[1].CreatedAt != "2026-06-05T20:38:00Z" || comments[1].User.Login != "bob" {
		t.Fatalf("second comment fields incorrect: %#v", comments[1])
	}
}
