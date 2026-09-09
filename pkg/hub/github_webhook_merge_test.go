package hub

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type mergeFailureTransport struct{}

func (mergeFailureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "api.github.com" {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			return githubAppTokenResponse(http.StatusOK, `[{"id":1,"account":{"login":"owner"}}]`), nil
		}
		if r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens" {
			return githubAppTokenResponse(http.StatusCreated, `{"token":"repo-token","expires_at":"2030-01-01T00:00:00Z"}`), nil
		}
	}
	return &http.Response{
		StatusCode: http.StatusMethodNotAllowed,
		Body:       io.NopCloser(strings.NewReader(`{"message":"Pull Request is not mergeable"}`)),
		Header:     make(http.Header),
	}, nil
}

func TestMergeSinglePRForClawHTTPFailureMessage(t *testing.T) {
	oldTransport := http.DefaultTransport
	oldClient := defaultGitHubClient
	transport := mergeFailureTransport{}
	http.DefaultTransport = transport
	defaultGitHubClient = newGitHubClient()
	defaultGitHubClient.httpClient = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
		defaultGitHubClient = oldClient
	})

	s, db := NewTestServerWithConfig(t, &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}, "https://github.test", "", "")
	insertWatcherTestPR(t, db, "claw-merge-failure", "pr-merge-failure")

	if got := s.mergeSinglePRForClaw("claw-merge-failure", "owner/repo", 1); got != "PR #1 failed (HTTP 405)" {
		t.Fatalf("failure = %q, want HTTP failure", got)
	}
	assertMessageContains(t, db, "claw-merge-failure", "hub", []string{"[hub] merge_pr: failed to merge PR #1 (HTTP 405). Check CI status and review requirements."})
}
