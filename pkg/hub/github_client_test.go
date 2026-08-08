package hub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// resetGitHubClientForTest isolates the package-level client between tests.
func resetGitHubClientForTest(t *testing.T) {
	t.Helper()
	defaultGitHubClient.resetState()
	t.Cleanup(defaultGitHubClient.resetState)
}

func TestGitHubClientBacksOffWhenBudgetDepleted(t *testing.T) {
	resetGitHubClientForTest(t)

	var remaining int64 = 900
	var calls int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(atomic.LoadInt64(&remaining), 10))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10))
		_, _ = w.Write([]byte(`{"state":"open"}`))
	}))
	defer gh.Close()

	if _, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/1", "tok"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !defaultGitHubClient.allowLowPriority() {
		t.Fatalf("low-priority polling blocked while %d requests remain", remaining)
	}

	// Drop below the reserve: background polling must stand down while
	// interactive calls keep working.
	atomic.StoreInt64(&remaining, int64(githubRateLimitReserve-1))
	if _, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/2", "tok"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if defaultGitHubClient.allowLowPriority() {
		t.Fatalf("low-priority polling still allowed with remaining below reserve %d", githubRateLimitReserve)
	}
	if _, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/3", "tok"); err != nil {
		t.Fatalf("high-priority call must still be served: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Fatalf("requests=%d, want 3", got)
	}
}

func TestGitHubRateLimit403DoesNotRetryImmediately(t *testing.T) {
	resetGitHubClientForTest(t)

	var calls int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(20*time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for installation ID 1."}`))
	}))
	defer gh.Close()

	_, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/1", "tok")
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	if isPermanentGitHubAPIError(err) {
		t.Fatalf("rate limit must stay transient, got permanent: %v", err)
	}

	// Every later call short-circuits until the window resets, instead of
	// re-issuing the request on the next tick.
	for i := 0; i < 5; i++ {
		if _, err := githubAPIWithBase(gh.URL, fmt.Sprintf("repos/o/r/pulls/%d", i+2), "tok"); err == nil {
			t.Fatal("expected blocked error while rate limited")
		}
		if _, err := githubAPIListWithBase(gh.URL, "repos/o/r/issues/1/comments", "tok"); err == nil {
			t.Fatal("expected blocked error while rate limited")
		}
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("requests=%d, want 1 (no retries while rate limited)", got)
	}

	until, blocked := defaultGitHubClient.blockedUntilTime()
	if !blocked {
		t.Fatal("client not blocked after 403 rate limit")
	}
	if time.Until(until) < 15*time.Minute {
		t.Fatalf("blocked until %s, want the reported reset window", until)
	}
}

func TestGitHubRetryAfterBlocksSubsequentCalls(t *testing.T) {
	resetGitHubClientForTest(t)

	var calls int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
	}))
	defer gh.Close()

	if _, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/1", "tok"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/1", "tok"); err == nil {
		t.Fatal("expected blocked error")
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("requests=%d, want 1", got)
	}
	until, blocked := defaultGitHubClient.blockedUntilTime()
	if !blocked || time.Until(until) < 60*time.Second {
		t.Fatalf("Retry-After ignored: blocked=%v until=%s", blocked, until)
	}
}

func TestGitHubRateLimitBlockIsClamped(t *testing.T) {
	resetGitHubClientForTest(t)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile or malformed hint that would otherwise blind the hub for years.
		w.Header().Set("Retry-After", "99999999")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limit"}`))
	}))
	defer gh.Close()

	if _, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/1", "tok"); err == nil {
		t.Fatal("expected error")
	}
	until, blocked := defaultGitHubClient.blockedUntilTime()
	if !blocked {
		t.Fatal("client not blocked")
	}
	if wait := time.Until(until); wait > githubRateLimitMaxWait+time.Minute {
		t.Fatalf("blocked for %s, want clamped to %s", wait, githubRateLimitMaxWait)
	}
}

func TestGitHubReserveScalesWithSmallLimits(t *testing.T) {
	resetGitHubClientForTest(t)

	h := http.Header{}
	h.Set("X-RateLimit-Limit", "60")
	h.Set("X-RateLimit-Remaining", "40")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10))
	defaultGitHubClient.observe(http.StatusOK, h, nil)

	// 40 remaining is below the 500 default reserve but well above half of a
	// 60/hour limit, so background polling must stay enabled.
	if !defaultGitHubClient.allowLowPriority() {
		t.Fatal("low-priority polling disabled by a reserve larger than the limit")
	}
}

func TestGitHubClientUsesETagSoUnchangedPollsAreFree(t *testing.T) {
	resetGitHubClientForTest(t)

	var conditional int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"abc123"`)
		if r.Header.Get("If-None-Match") == `W/"abc123"` {
			atomic.AddInt64(&conditional, 1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte(`{"state":"open","number":7}`))
	}))
	defer gh.Close()

	first, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/7", "tok")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := githubAPIWithBase(gh.URL, "repos/o/r/pulls/7", "tok")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if atomic.LoadInt64(&conditional) != 1 {
		t.Fatalf("second call did not send If-None-Match")
	}
	if second["state"] != first["state"] || second["number"] != first["number"] {
		t.Fatalf("304 body = %v, want cached %v", second, first)
	}
}

// githubAppAnyTokenTransport mints installation tokens for scoped and unscoped
// requests alike, and counts how many tokens were minted.
type githubAppAnyTokenTransport struct {
	base  http.RoundTripper
	mints *int64
}

func (t githubAppAnyTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return t.base.RoundTrip(r)
	}
	if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
		return githubAppTokenResponse(http.StatusOK, `[{"id":1,"account":{"login":"owner"}}]`), nil
	}
	if r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens" {
		atomic.AddInt64(t.mints, 1)
		return githubAppTokenResponse(http.StatusCreated, `{"token":"tok","expires_at":"2030-01-01T00:00:00Z"}`), nil
	}
	return githubAppTokenResponse(http.StatusNotFound, `{"message":"not found"}`), nil
}

func TestPollAllPRsStopsCallingGitHubWhileRateLimited(t *testing.T) {
	resetGitHubClientForTest(t)

	var mints int64
	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppAnyTokenTransport{base: oldTransport, mints: &mints}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	var calls int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded for installation ID 1."}`))
	}))
	defer gh.Close()

	cfg := &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}
	s, db := NewTestServerWithConfig(t, cfg, gh.URL, "", "")
	insertWatcherTestPR(t, db, "claw-ratelimited", "pr-ratelimited")

	s.pollAllPRs()
	after := atomic.LoadInt64(&calls)
	if after == 0 {
		t.Fatal("first poll made no GitHub calls")
	}
	for i := 0; i < 3; i++ {
		s.pollAllPRs()
	}
	if got := atomic.LoadInt64(&calls); got != after {
		t.Fatalf("requests=%d after further polls, want %d (no calls while rate limited)", got, after)
	}
	// The next poll is deferred to the reported reset, not the base interval.
	if delay := s.nextPollDelay(); delay <= prWatcherBaseInterval {
		t.Fatalf("nextPollDelay=%s, want a backoff longer than %s", delay, prWatcherBaseInterval)
	}

	// Repo-scoped only (pollAllPRs uses tokenForRepo so multi-org workspace apps
	// are not collapsed onto a single unscoped installation token).
	if got := atomic.LoadInt64(&mints); got != 1 {
		t.Fatalf("installation tokens minted=%d, want 1 (repo-scoped token cached across polls)", got)
	}
}


func TestInstallationTokensAreCachedAcrossCalls(t *testing.T) {
	var mints int64
	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppAnyTokenTransport{base: oldTransport, mints: &mints}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	cfg := &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}
	s, _ := NewTestServerWithConfig(t, cfg, "", "", "")

	for i := 0; i < 10; i++ {
		if got := s.resolveGitHubToken(); got != "tok" {
			t.Fatalf("unscoped token = %q", got)
		}
		if got := s.resolveGitHubTokenForRepo("owner/repo"); got != "tok" {
			t.Fatalf("repo token = %q", got)
		}
	}
	if got := atomic.LoadInt64(&mints); got != 2 {
		t.Fatalf("installation tokens minted=%d, want 2 (one per distinct scope)", got)
	}
}

// Factories often configure GitHub Apps only on the workspace (no hub-global
// github_apps). PR watcher / poller / reconciler must still mint tokens.
func TestResolveGitHubTokenUsesWorkspaceAppsWhenHubHasNone(t *testing.T) {
	var mints int64
	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppAnyTokenTransport{base: oldTransport, mints: &mints}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	// Point hub config dir at a temp tree with one workspace app only.
	tmp := t.TempDir()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", tmp+"/hub.yaml")
	wsDir := tmp + "/workspaces/adversaries/.elasticclaw-managed"
	if err := os.MkdirAll(wsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Minimal workspace marker so the dir is a real workspace.
	if err := os.WriteFile(tmp+"/workspaces/adversaries/elasticclaw-config.yaml", []byte("schema_version: v1\nname: adversaries\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	pem := testGitHubAppPEM(t)
	appsYAML := "github_apps:\n  AdversaryLabs:\n    app_id: 1\n    url: https://github.com/apps/factory-marecampbell-com\n    private_key_pem: |\n"
	for _, line := range strings.Split(pem, "\n") {
		appsYAML += "      " + line + "\n"
	}
	if err := os.WriteFile(wsDir+"/github_apps.yaml", []byte(appsYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// No hub-global apps.
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	if got := s.resolveGitHubToken(); got != "tok" {
		t.Fatalf("unscoped token from workspace app = %q, want tok", got)
	}
	if got := s.resolveGitHubTokenForRepo("adversarylabs/engineering-review-adversary"); got != "tok" {
		t.Fatalf("repo token from workspace app = %q, want tok", got)
	}
	if got := atomic.LoadInt64(&mints); got != 2 {
		t.Fatalf("installation tokens minted=%d, want 2", got)
	}
	if !s.isOwnAppBot("factory-marecampbell-com[bot]") {
		t.Fatal("expected workspace app bot login to be recognized as own app bot")
	}
}

func TestPRWatcherIntervalScalesWithTrackedPRs(t *testing.T) {
	if got := prWatcherInterval(0); got != prWatcherBaseInterval {
		t.Fatalf("interval(0)=%s, want %s", got, prWatcherBaseInterval)
	}
	if got := prWatcherInterval(1); got != prWatcherBaseInterval {
		t.Fatalf("interval(1)=%s, want the base interval floor %s", got, prWatcherBaseInterval)
	}

	prev := prWatcherInterval(4)
	if prev <= prWatcherBaseInterval {
		t.Fatalf("interval(4)=%s, want more than the base interval", prev)
	}
	for _, n := range []int{8, 16, 32} {
		got := prWatcherInterval(n)
		if got <= prev {
			t.Fatalf("interval(%d)=%s, want more than %s", n, got, prev)
		}
		prev = got
	}
	if got := prWatcherInterval(10000); got != prWatcherMaxInterval {
		t.Fatalf("interval(10000)=%s, want the cap %s", got, prWatcherMaxInterval)
	}

	// The whole point: hourly cost stays inside the watcher's budget until the
	// interval hits its cap, past which the rate-limit reserve takes over.
	for _, n := range []int{1, 2, 4, 8} {
		interval := prWatcherInterval(n)
		if interval >= prWatcherMaxInterval {
			continue
		}
		perHour := float64(n*prWatcherCallsPerPR) * float64(time.Hour) / float64(interval)
		if perHour > prWatcherHourlyBudget+1 {
			t.Fatalf("%d PR(s) cost %.0f calls/hour, over budget %d", n, perHour, prWatcherHourlyBudget)
		}
	}
}
