package v2

import (
	"fmt"
	"strings"
)

// gitHubRepositoryPermissions is the set of repository-scoped GitHub App
// permission names accepted in the granular form of
// repositories.*.permissions (issue #697). Names match the GitHub REST API
// "App Permissions" keys used by POST /app/installations/{id}/access_tokens.
var gitHubRepositoryPermissions = map[string]bool{
	"actions":                      true,
	"administration":               true,
	"artifact_metadata":            true,
	"attestations":                 true,
	"checks":                       true,
	"code_quality":                 true,
	"codespaces":                   true,
	"contents":                     true,
	"dependabot_secrets":           true,
	"deployments":                  true,
	"discussions":                  true,
	"environments":                 true,
	"issues":                       true,
	"merge_queues":                 true,
	"metadata":                     true,
	"packages":                     true,
	"pages":                        true,
	"pull_requests":                true,
	"repository_custom_properties": true,
	"repository_hooks":             true,
	"repository_projects":          true,
	"secret_scanning_alerts":       true,
	"secrets":                      true,
	"security_events":              true,
	"single_file":                  true,
	"statuses":                     true,
	"vulnerability_alerts":         true,
	"workflows":                    true,
}

// gitHubPermissionAliases maps friendlier names to the canonical GitHub App
// permission keys. The GitHub UI calls vulnerability_alerts "Dependabot
// alerts" and security_events "Code scanning alerts", so authors naturally
// reach for those names.
var gitHubPermissionAliases = map[string]string{
	"dependabot_alerts":    "vulnerability_alerts",
	"code_scanning_alerts": "security_events",
}

// CanonicalGitHubPermissionName normalizes a declared permission name to the
// canonical GitHub App permission key. Unknown names are returned lowercased
// and trimmed so validation can reject them with the authored spelling.
func CanonicalGitHubPermissionName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if canonical, ok := gitHubPermissionAliases[name]; ok {
		return canonical
	}
	return name
}

// IsKnownGitHubPermissionName reports whether name (already canonicalized)
// is a repository-scoped GitHub App permission.
func IsKnownGitHubPermissionName(name string) bool {
	return gitHubRepositoryPermissions[CanonicalGitHubPermissionName(name)]
}

// CanonicalGitHubPermissionLevel normalizes a declared permission level to
// "read", "write", or "" when invalid or empty.
func CanonicalGitHubPermissionLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "read", "write":
		return strings.ToLower(strings.TrimSpace(level))
	default:
		return ""
	}
}

// validateRepositoryPermissions checks the granular permission form of a v2
// repository. The scalar form stays lenient (values other than "write"
// normalize to "read" at projection) so existing configs never start failing
// validation. The granular form is new surface, so unknown permission names
// and invalid levels are rejected with the authored spelling.
func validateRepositoryPermissions(repoName string, perms RepositoryPermissions) error {
	seen := make(map[string]bool, len(perms.granular))
	for name, level := range perms.granular {
		canonical := CanonicalGitHubPermissionName(name)
		if !gitHubRepositoryPermissions[canonical] {
			return fmt.Errorf("repositories.%s.permissions.%s: unknown GitHub App permission (canonical name: %s)", repoName, name, canonical)
		}
		if seen[canonical] {
			return fmt.Errorf("repositories.%s.permissions.%s: duplicate declaration of %s", repoName, name, canonical)
		}
		seen[canonical] = true
		// Normalize the level once (case/whitespace variants) so the checks
		// below cannot be bypassed by e.g. "WRITE" or " write ".
		normalized := CanonicalGitHubPermissionLevel(level)
		if normalized == "" {
			return fmt.Errorf("repositories.%s.permissions.%s: invalid level %q (must be read or write)", repoName, name, level)
		}
		if canonical == "metadata" && normalized == "write" {
			// metadata is always read and cannot be widened; reject instead of
			// silently dropping the requested level at mint time.
			return fmt.Errorf("repositories.%s.permissions.metadata: metadata is always read and cannot be set to write", repoName)
		}
	}
	return nil
}
