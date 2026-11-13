package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// appSlugTestServer fakes GET /app; status is read on every request.
func appSlugTestServer(t *testing.T, status *atomic.Int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"slug":"fw-agent-factory","permissions":{"pull_requests":"write"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestOwnAppBotResolvesSlugWithoutAppURL(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv, hits := appSlugTestServer(t, &status)
	s := &Server{githubBaseURL: srv.URL, hubCfg: &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{
		{AppID: 7, PrivateKeyPEM: testGitHubAppPEM(t)},
	}}}

	if !s.hasOwnAppIdentity() {
		t.Fatal("resolved slug did not give the hub an App identity")
	}
	if !s.isOwnAppBot("fw-agent-factory[bot]") || !s.isClawAppAuthor("FW-Agent-Factory[bot]", "Bot") {
		t.Fatal("resolved App bot not recognized")
	}
	if s.isOwnAppBot("dependabot[bot]") || s.isOwnAppBot("fw-agent-factory") {
		t.Fatal("other logins recognized as the App bot")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("GET /app hits=%d, want 1 (cached per app id)", got)
	}
}

func TestOwnAppBotPrefersConfiguredAppURL(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv, hits := appSlugTestServer(t, &status)
	pem := testGitHubAppPEM(t)
	s := &Server{githubBaseURL: srv.URL, hubCfg: &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{
		{AppID: 7, PrivateKeyPEM: pem},
		{AppID: 7, PrivateKeyPEM: pem, URL: "https://github.com/apps/configured-app"},
	}}}

	if !s.isOwnAppBot("configured-app[bot]") {
		t.Fatal("configured App URL slug not recognized")
	}
	if s.isOwnAppBot("fw-agent-factory[bot]") {
		t.Fatal("GET /app slug used although the App URL names the App")
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("GET /app hits=%d, want 0 when the App URL is configured", got)
	}
}

func TestOwnAppSlugLookupFailureIsRetriedLater(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	var status atomic.Int32
	status.Store(http.StatusInternalServerError)
	srv, hits := appSlugTestServer(t, &status)
	s := &Server{githubBaseURL: srv.URL, hubCfg: &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{
		{AppID: 7, PrivateKeyPEM: testGitHubAppPEM(t)},
	}}}

	if s.hasOwnAppIdentity() || s.isOwnAppBot("fw-agent-factory[bot]") {
		t.Fatal("failed lookup must fail closed")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("GET /app hits=%d, want 1 (failure backs off)", got)
	}

	status.Store(http.StatusOK)
	s.appSlugMu.Lock()
	s.appSlugs[7] = appSlugEntry{failedAt: time.Now().Add(-appSlugRetryAfter)}
	s.appSlugMu.Unlock()
	if !s.isOwnAppBot("fw-agent-factory[bot]") {
		t.Fatal("lookup not retried after the backoff")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("GET /app hits=%d, want 2", got)
	}
}
