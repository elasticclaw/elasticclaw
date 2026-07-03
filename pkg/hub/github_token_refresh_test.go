package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestGitHubTokenProfileScriptUsesCredentialHelperWithoutStaticToken(t *testing.T) {
	script := buildGitHubTokenProfileScript()

	assertContains(t, script, "/usr/local/bin/elasticclaw-git-credentials", "profile fetches token from credential helper")
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
	assertContains(t, script, "export GH_TOKEN=\"$token\"", "wrapper exports fresh token")
	assertContains(t, script, "exec \"$REAL_GH\" \"$@\"", "wrapper delegates to real gh")
	assertNotContains(t, script, "gh auth login", "wrapper must not persist a short-lived token in hosts.yml")
	assertNotContains(t, script, "ghp_static_test_token", "wrapper must not contain raw GitHub tokens")
}

func TestDaytonaGitHubCloneScriptUsesCleanHTTPSRemote(t *testing.T) {
	script := buildDaytonaGitHubCloneScript([]types.GitHubRepoAccess{{Repo: "elasticclaw/private-repo"}})

	assertContains(t, script, "git config --global --get credential.helper", "clone verifies credential helper")
	assertContains(t, script, "https://github.com/elasticclaw/private-repo.git", "clone uses normal HTTPS remote")
	assertNotContains(t, script, "x-access-token", "clone must not embed token username in remote URL")
	assertNotContains(t, script, "${GH_TOKEN}", "clone must not embed GH_TOKEN in remote URL")
	assertNotContains(t, script, "sed \"s/${GH_TOKEN}", "clone output redaction should not depend on token in URL")
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
