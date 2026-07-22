package hub

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func hasRepositoryGlob(repositories []types.GitHubRepoAccess) bool {
	for _, repository := range repositories {
		if strings.ContainsAny(repository.Repo, "*?[") {
			return true
		}
	}
	return false
}

// expandRepositoryAccess resolves repository selectors against one GitHub App
// installation. Patterns without a slash match repository names; patterns with
// a slash match the canonical owner/repository name.
func expandRepositoryAccess(selectors []types.GitHubRepoAccess, available []githubRepository) ([]types.GitHubRepoAccess, error) {
	type selectedRepository struct {
		name       string
		permission string
	}
	selected := make(map[string]selectedRepository)

	for _, selector := range selectors {
		pattern := strings.ToLower(selector.Repo)
		isGlob := strings.ContainsAny(pattern, "*?[")
		matchFullName := strings.Contains(pattern, "/")
		matched := false

		for _, repository := range available {
			if repository.FullName == "" || repository.Name == "" {
				continue
			}
			target := strings.ToLower(repository.Name)
			if matchFullName || !isGlob {
				target = strings.ToLower(repository.FullName)
			}

			var matches bool
			if isGlob {
				var err error
				matches, err = path.Match(pattern, target)
				if err != nil {
					return nil, fmt.Errorf("invalid repository pattern %q: %w", selector.Repo, err)
				}
			} else {
				matches = pattern == target
			}
			if !matches {
				continue
			}

			matched = true
			key := strings.ToLower(repository.FullName)
			permission := selector.Permissions
			if permission == "" {
				permission = "read"
			}
			_, exists := selected[key]
			if !exists || permission == "write" {
				selected[key] = selectedRepository{name: repository.FullName, permission: permission}
			}
		}
		if !matched {
			kind := "repository"
			if isGlob {
				kind = "repository pattern"
			}
			return nil, fmt.Errorf("%s %q matched no repositories accessible to this GitHub App installation", kind, selector.Repo)
		}
	}

	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	expanded := make([]types.GitHubRepoAccess, 0, len(keys))
	for _, key := range keys {
		repository := selected[key]
		expanded = append(expanded, types.GitHubRepoAccess{
			Repo:        repository.name,
			Permissions: repository.permission,
		})
	}
	return expanded, nil
}

// expandWorkspaceRepositories selects the first configured GitHub App
// installation that can satisfy every selector. Keeping the expansion within
// one installation is required because each claw currently uses one GitHub
// installation token for all repository operations.
func (s *Server) expandWorkspaceRepositories(ctx context.Context, workspaceName string, selectors []types.GitHubRepoAccess) ([]types.GitHubRepoAccess, error) {
	if !hasRepositoryGlob(selectors) {
		return append([]types.GitHubRepoAccess(nil), selectors...), nil
	}

	workspaceApps, err := loadWorkspaceGitHubAppConfigs(workspaceName)
	if err != nil {
		return nil, fmt.Errorf("load workspace GitHub Apps: %w", err)
	}
	s.mu.RLock()
	hubApps := append([]*types.GitHubAppConfig(nil), s.hubCfg.GitHubApps...)
	s.mu.RUnlock()
	githubApps := append(workspaceApps, hubApps...)
	if len(githubApps) == 0 {
		return nil, fmt.Errorf("repository patterns require a GitHub App configured for workspace %q", workspaceName)
	}

	var failures []string
	for _, appConfig := range githubApps {
		provider, err := NewGitHubTokenProvider(appConfig)
		if err != nil {
			failures = append(failures, fmt.Sprintf("app %d: %v", appConfig.AppID, err))
			continue
		}
		if s.githubBaseURL != "" {
			provider.apiBaseURL = s.githubBaseURL
		}

		installations, err := provider.ListInstallations(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures = append(failures, fmt.Sprintf("app %d: %v", appConfig.AppID, err))
			continue
		}
		for _, installation := range installations {
			available, err := provider.ListInstallationRepositories(ctx, installation.ID)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				failures = append(failures, fmt.Sprintf("app %d installation %d: %v", appConfig.AppID, installation.ID, err))
				continue
			}
			expanded, err := expandRepositoryAccess(selectors, available)
			if err == nil {
				return expanded, nil
			}
			failures = append(failures, fmt.Sprintf("app %d installation %d: %v", appConfig.AppID, installation.ID, err))
		}
	}

	if len(failures) == 0 {
		return nil, fmt.Errorf("no installations found for the GitHub Apps configured for workspace %q", workspaceName)
	}
	return nil, fmt.Errorf("no single GitHub App installation can satisfy workspace repository selectors: %s", strings.Join(failures, "; "))
}
