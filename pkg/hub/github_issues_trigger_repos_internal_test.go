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
