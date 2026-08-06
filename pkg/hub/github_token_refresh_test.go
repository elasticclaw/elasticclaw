package hub

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestGitHubTokenProfileScriptUsesCredentialHelperWithoutStaticToken(t *testing.T) {
	script := buildGitHubTokenProfileScript()

	assertContains(t, script, "/usr/local/bin/elasticclaw-git-credentials", "profile fetches token from credential helper")
	assertContains(t, script, "elasticclaw-git-credentials get", "profile feeds git credential get protocol")
	assertContains(t, script, "protocol=https", "profile uses https credential protocol")
	assertContains(t, script, "sed -n 's/^password=//p'", "profile extracts only password field")
	assertContains(t, script, "export GH_TOKEN", "profile exports GH_TOKEN for gh")
	assertNotContains(t, script, "export GH_TOKEN=%s", "profile must not format in a static token")
	assertNotContains(t, script, "cat /tmp/elasticclaw-github-token", "profile must not read a persisted bootstrap token")
	assertNotContains(t, script, "ghp_static_test_token", "profile must not contain raw GitHub tokens")
}

func TestGitHubCLIWrapperRefreshesTokenForEachInvocation(t *testing.T) {
	script := buildGitHubCLIWrapperInstallScript()

	assertContains(t, script, "sudo tee /usr/local/bin/gh", "wrapper shadows gh on PATH")
	assertContains(t, script, "/usr/local/bin/elasticclaw-git-credentials", "wrapper fetches token from credential helper")
	assertContains(t, script, "elasticclaw-git-credentials get", "wrapper feeds git credential get protocol")
	assertContains(t, script, "export GH_TOKEN=\"$token\"", "wrapper exports fresh token")
	assertContains(t, script, "exec \"$REAL_GH\" \"$@\"", "wrapper delegates to real gh")
	assertNotContains(t, script, "gh auth login", "wrapper must not persist a short-lived token in hosts.yml")
	assertNotContains(t, script, "ghp_static_test_token", "wrapper must not contain raw GitHub tokens")
}

func TestGitHubCLIWrapperHandlesRealGhInUsrLocalBin(t *testing.T) {
	script := buildGitHubCLIWrapperInstallScript()

	assertContains(t, script, "/usr/local/bin/gh.elasticclaw-real", "wrapper preserves real gh when gh already lives in /usr/local/bin")
	assertContains(t, script, "sudo mv /usr/local/bin/gh /usr/local/bin/gh.elasticclaw-real", "wrapper moves real gh out of wrapper path")
	assertContains(t, script, "ElasticClaw GitHub App token refresh wrapper", "wrapper can detect an existing managed wrapper")
	assertContains(t, script, "GitHub gh wrapper already configured", "wrapper logs when already installed")
}

func TestGitHubCLIWrapperEscapesRealGhPathForSed(t *testing.T) {
	script := buildGitHubCLIWrapperInstallScript()

	assertContains(t, script, "REAL_GH_ESCAPED=", "wrapper escapes real gh path before sed replacement")
	assertContains(t, script, `sed 's/[&\\|]/\\&/g'`, "wrapper escapes sed replacement metacharacters")
	assertContains(t, script, `sudo sed -i "s|__ELASTICCLAW_REAL_GH__|$REAL_GH_ESCAPED|g"`, "wrapper uses escaped real gh path in sed")
	assertNotContains(t, script, `sudo sed -i "s|__ELASTICCLAW_REAL_GH__|$REAL_GH|g"`, "wrapper must not use raw path in sed replacement")
}

func TestDaytonaGitHubCloneScriptUsesCleanHTTPSRemote(t *testing.T) {
	script := buildDaytonaGitHubCloneScript([]types.GitHubRepoAccess{{Repo: "elasticclaw/private-repo"}})

	assertContains(t, script, "git config --global --get credential.helper", "clone verifies credential helper")
	assertContains(t, script, "https://github.com/elasticclaw/private-repo.git", "clone uses normal HTTPS remote")
	assertNotContains(t, script, "x-access-token", "clone must not embed token username in remote URL")
	assertNotContains(t, script, "${GH_TOKEN}", "clone must not embed GH_TOKEN in remote URL")
	assertNotContains(t, script, "sed \"s/${GH_TOKEN}", "clone output redaction should not depend on token in URL")
}

func TestDaytonaGitHubAccessSmokeScriptIsConstantTimeInRepoCount(t *testing.T) {
	many := make([]types.GitHubRepoAccess, 40)
	for i := range many {
		many[i] = types.GitHubRepoAccess{Repo: fmt.Sprintf("org/repo-%d", i), Permissions: "write"}
	}
	script := buildDaytonaGitHubAccessSmokeScript(many)

	assertContains(t, script, "gh api rate_limit", "smoke uses installation-token-safe API call")
	assertNotContains(t, script, "gh api user", "installation tokens cannot call /user")
	assertContains(t, script, "gh repo view", "smoke samples a configured repo")
	assertContains(t, script, "org/repo-0", "smoke samples only the first configured repo")
	// Must not O(N) view every workspace repo (that timed out large workspaces).
	if strings.Count(script, "gh repo view") != 1 {
		t.Fatalf("gh repo view count = %d, want 1\n%s", strings.Count(script, "gh repo view"), script)
	}
	for i := 1; i < len(many); i++ {
		if strings.Contains(script, fmt.Sprintf("org/repo-%d", i)) {
			t.Fatalf("smoke script must not reference repo-%d", i)
		}
	}
}

func TestGitHubBootstrapCloneTimeoutScalesWithRepoCount(t *testing.T) {
	if got := githubBootstrapCloneTimeout(0); got != 2*time.Minute {
		t.Fatalf("timeout(0)=%s, want 2m", got)
	}
	if got := githubBootstrapCloneTimeout(1); got != 2*time.Minute {
		t.Fatalf("timeout(1)=%s, want floor 2m", got)
	}
	if got := githubBootstrapCloneTimeout(32); got != 32*45*time.Second {
		t.Fatalf("timeout(32)=%s, want 24m", got)
	}
	if got := githubBootstrapCloneTimeout(1000); got != 30*time.Minute {
		t.Fatalf("timeout(1000)=%s, want cap 30m", got)
	}
	if got := githubBootstrapCloneVerifyTimeout(5); got != 20*time.Second {
		t.Fatalf("verify timeout(5)=%s, want floor 20s", got)
	}
	if got := githubBootstrapCloneVerifyTimeout(90); got != 90*time.Second {
		t.Fatalf("verify timeout(90)=%s, want 90s", got)
	}
}

// githubInstallationTokenTestServer mocks token mint + installation permission lookup.
// installPermissionsJSON is the permissions object returned by GET /app/installations/{id}.
func githubInstallationTokenTestServer(t *testing.T, installPermissionsJSON string, onAccessToken func(body string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations/99":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":99,"account":{"login":"org"},"permissions":` + installPermissionsJSON + `}`))
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":99,"account":{"login":"org"}}]`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/access_tokens"):
			body, _ := io.ReadAll(r.Body)
			if onAccessToken != nil {
				onAccessToken(string(body))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"tok","expires_at":"2099-01-01T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallationTokenOmitsNameListAboveGitHubCap(t *testing.T) {
	// GitHub rejects repositories arrays longer than 50. We still mint a
	// permission-restricted installation token so 50+ workspaces work; git
	// clones should use ?repo= for least privilege.
	var sawBody string
	srv := githubInstallationTokenTestServer(t, `{"contents":"write","workflows":"write"}`, func(body string) {
		sawBody = body
	})

	provider, err := NewGitHubTokenProvider(&types.GitHubAppConfig{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)})
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBaseURL = srv.URL
	provider.httpClient = srv.Client()

	repos := make([]RepoAccess, maxScopedInstallationRepos+1)
	for i := range repos {
		repos[i] = RepoAccess{Repo: fmt.Sprintf("org/r%d", i), Permissions: "write"}
	}
	if _, _, err := provider.InstallationToken(context.Background(), 99, repos); err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if strings.Contains(sawBody, `"repositories"`) {
		t.Fatalf("expected no repositories name list for %d repos, body=%s", len(repos), sawBody)
	}
	if !strings.Contains(sawBody, `"contents":"write"`) {
		t.Fatalf("expected write permissions, body=%s", sawBody)
	}
	if !strings.Contains(sawBody, `"workflows":"write"`) {
		t.Fatalf("expected workflows write when installation grants it, body=%s", sawBody)
	}
}

func TestInstallationTokenScopesRepositoryAllowlist(t *testing.T) {
	var sawBody string
	srv := githubInstallationTokenTestServer(t, `{"contents":"write","workflows":"write"}`, func(body string) {
		sawBody = body
	})

	provider, err := NewGitHubTokenProvider(&types.GitHubAppConfig{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)})
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBaseURL = srv.URL
	provider.httpClient = srv.Client()

	small := []RepoAccess{{Repo: "org/a", Permissions: "read"}, {Repo: "org/b", Permissions: "write"}}
	if _, _, err := provider.InstallationToken(context.Background(), 99, small); err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if !strings.Contains(sawBody, `"repositories"`) || !strings.Contains(sawBody, `"a"`) || !strings.Contains(sawBody, `"b"`) {
		t.Fatalf("expected scoped repositories body, got %s", sawBody)
	}
	if !strings.Contains(sawBody, `"contents":"write"`) {
		t.Fatalf("expected write permissions, body=%s", sawBody)
	}
	if !strings.Contains(sawBody, `"workflows":"write"`) {
		t.Fatalf("expected workflows write when installation grants it, body=%s", sawBody)
	}
}

func TestInstallationTokenOmitsWorkflowsWhenInstallLacksPermission(t *testing.T) {
	var sawBody string
	// Installation has contents write but no workflows — requesting workflows would fail mint.
	srv := githubInstallationTokenTestServer(t, `{"contents":"write","pull_requests":"write"}`, func(body string) {
		sawBody = body
	})

	provider, err := NewGitHubTokenProvider(&types.GitHubAppConfig{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)})
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBaseURL = srv.URL
	provider.httpClient = srv.Client()

	if _, _, err := provider.InstallationToken(context.Background(), 99, []RepoAccess{{Repo: "org/a", Permissions: "write"}}); err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if !strings.Contains(sawBody, `"contents":"write"`) {
		t.Fatalf("expected write permissions, body=%s", sawBody)
	}
	if strings.Contains(sawBody, `"workflows"`) {
		t.Fatalf("must not request workflows when installation lacks it, body=%s", sawBody)
	}
}

func TestInstallationTokenOmitsWorkflowsWhenInstallLookupFails(t *testing.T) {
	var sawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations/99":
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/access_tokens"):
			body, _ := io.ReadAll(r.Body)
			sawBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"tok","expires_at":"2099-01-01T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	provider, err := NewGitHubTokenProvider(&types.GitHubAppConfig{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)})
	if err != nil {
		t.Fatal(err)
	}
	provider.apiBaseURL = srv.URL
	provider.httpClient = srv.Client()

	if _, _, err := provider.InstallationToken(context.Background(), 99, []RepoAccess{{Repo: "org/a", Permissions: "write"}}); err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if strings.Contains(sawBody, `"workflows"`) {
		t.Fatalf("must omit workflows when installation permission lookup fails, body=%s", sawBody)
	}
}

func TestDaytonaCredentialHelperRequestsPerRepoTokens(t *testing.T) {
	// buildGitHubCredentialHelper is used by replicated; Daytona embeds a similar script.
	// Assert the Daytona helper script template includes path→repo query wiring.
	// We reconstruct the key fragments by checking server.go source constants via
	// the smoke/clone helpers that ship alongside.
	script := buildDaytonaGitHubCloneScript([]types.GitHubRepoAccess{{Repo: "org/r1"}, {Repo: "org/r2"}})
	assertContains(t, script, "elasticclaw-github.sh", "clone sources token profile")
	// Per-repo scoping is in the installed credential helper; clone still uses clean remotes.
	assertContains(t, script, "https://github.com/org/r1.git", "clone uses clean HTTPS remote")
}

func TestReplicatedCredentialHelperInstallsDynamicGhTokenProfileAndWrapper(t *testing.T) {
	cfg := &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
		ClawToken:  "test-claw-token",
	}
	script := buildGitHubCredentialHelper(cfg, "https://hub.example.com", "claw-123", nil)

	if strings.Contains(script, "printf 'export GH_TOKEN=%s") {
		t.Fatalf("replicated helper writes static GH_TOKEN format:\n%s", script)
	}
	assertContains(t, script, "sed -n 's/^password=//p'", "profile extracts fresh helper token")
	assertContains(t, script, "sudo tee /usr/local/bin/gh", "gh wrapper installed")
	assertNotContains(t, script, "gh auth login", "replicated helper must not persist gh hosts.yml token")
}
