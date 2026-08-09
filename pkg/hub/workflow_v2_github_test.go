package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestWorkflowV2GitHubVerifierResolvesClaimThroughAPI(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/org/api/pulls/42" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": 1234, "html_url": "https://github.com/org/api/pull/42", "state": "open", "merged": false,
			"head": map[string]interface{}{"sha": "abc123", "ref": "feature"},
			"base": map[string]interface{}{"ref": "main"},
		})
	}))
	defer api.Close()
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, api.URL, "", "")
	workspace := &typesv2.Workspace{Name: "engineering", Repositories: map[string]typesv2.Repository{
		"api": {Provider: "github", Repository: "org/api", SourceControl: "github-prod"},
	}}
	verified, err := s.verifyWorkflowV2PullRequest(context.Background(), workflowv2.Run{ID: "run-1"}, workspace,
		typesv2.PullRequestClaim{URL: "https://github.com/org/api/pull/42"})
	if err != nil {
		t.Fatal(err)
	}
	if verified.RepositoryName != "api" || verified.Repository != "org/api" || verified.Number != 42 ||
		verified.HeadSHA != "abc123" || verified.SourceBranch != "feature" || verified.BaseBranch != "main" ||
		verified.Provenance.Connection != "github-prod" || !verified.Provenance.Reconciled {
		t.Fatalf("verified = %#v", verified)
	}
}

func TestWorkflowV2GitHubVerifierRejectsClaimOutsideWorkspaceWithoutAPICall(t *testing.T) {
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer api.Close()
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, api.URL, "", "")
	workspace := &typesv2.Workspace{Name: "engineering", Repositories: map[string]typesv2.Repository{
		"api": {Provider: "github", Repository: "org/api"},
	}}
	_, err := s.verifyWorkflowV2PullRequest(context.Background(), workflowv2.Run{}, workspace,
		typesv2.PullRequestClaim{URL: "https://github.com/other/repo/pull/1"})
	if err == nil {
		t.Fatal("outside repository was accepted")
	}
	if calls != 0 {
		t.Fatalf("made %d API calls for unauthorized repository", calls)
	}
}

func TestParseWorkflowV2GitHubPRURLRejectsAmbiguousURLs(t *testing.T) {
	for _, raw := range []string{
		"http://github.com/org/repo/pull/1",
		"https://github.com/org/repo/pull/1/files",
		"https://github.com/org/repo/pull/1?diff=split",
		"https://github.com/org/repo/issues/1",
	} {
		if _, _, err := parseWorkflowV2GitHubPRURL(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestWorkflowV2GitHubRequestHonorsContextCancellation(t *testing.T) {
	requestCancelled := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(requestCancelled)
	}))
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := githubAPIWithBaseContext(ctx, api.URL, "repos/org/cancel/pulls/98765", "")
	if err == nil {
		t.Fatal("cancelled verification returned no error")
	}
	select {
	case <-requestCancelled:
	case <-time.After(time.Second):
		t.Fatal("GitHub request did not observe context cancellation")
	}
}
