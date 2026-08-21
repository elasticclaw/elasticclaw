package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestResolveGitHubIssuesTokenForRepoUsesTriggerRepos(t *testing.T) {
	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{
			{
				Name:         "org-a",
				Integration:  "github-issues",
				Workspace:    "workspace-a",
				Template:     "elasticclaw",
				TriggerRepos: []string{"org-a/*"},
			},
			{
				Name:         "org-b",
				Integration:  "github-issues",
				Workspace:    "workspace-b",
				Template:     "elasticclaw",
				TriggerRepos: []string{"org-b/repo"},
			},
		},
		Integrations: &types.IntegrationsConfig{
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{
				{Workspace: "workspace-a", Token: "token-a"},
				{Workspace: "workspace-b", Token: "token-b"},
			},
		},
	}
	s, _ := NewTestServerWithConfig(t, cfg, "", "", "")

	if got := s.resolveGitHubIssuesTokenForRepo("org-b/repo"); got != "token-b" {
		t.Fatalf("token = %q, want token-b", got)
	}
	if got := s.resolveGitHubIssuesTokenForRepo("org-a/another-repo"); got != "token-a" {
		t.Fatalf("wildcard token = %q, want token-a", got)
	}
}

func TestGitHubRepoMatchesWithExclusions(t *testing.T) {
	cases := []struct {
		fullName string
		include  []string
		exclude  []string
		want     bool
	}{
		{"org/a", []string{"org/*"}, []string{"org/b"}, true},
		{"org/b", []string{"org/*"}, []string{"org/b"}, false},
		{"org/c", []string{"org/*"}, []string{"org/b"}, true},
		{"org/a", []string{"org/a", "org/b"}, []string{"org/a"}, false},
		{"org/a", nil, []string{"org/a"}, false},
		{"org/a", []string{"org/*"}, []string{"other/*"}, true},
		{"org/sub/a", []string{"org/*"}, []string{"org/b"}, true}, // nested path still matches org/* include
	}
	for _, c := range cases {
		got := githubRepoMatchesWithExclusions(c.fullName, c.include, c.exclude)
		if got != c.want {
			t.Errorf("githubRepoMatchesWithExclusions(%q, %v, %v) = %v, want %v", c.fullName, c.include, c.exclude, got, c.want)
		}
	}
}

func TestResolveGitHubIssuesTokenForRepoFallsBackToLegacyRepos(t *testing.T) {
	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{{
			Name:        "legacy",
			Integration: "github-issues",
			Workspace:   "workspace-legacy",
			Template:    "elasticclaw",
			Repos:       []string{"legacy-org/*"},
		}},
		Integrations: &types.IntegrationsConfig{
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{Workspace: "workspace-legacy", Token: "legacy-token"}},
		},
	}
	s, _ := NewTestServerWithConfig(t, cfg, "", "", "")

	if got := s.resolveGitHubIssuesTokenForRepo("legacy-org/repo"); got != "legacy-token" {
		t.Fatalf("token = %q, want legacy-token", got)
	}
}
