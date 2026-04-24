package hub

import (
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// canViewClaw returns true if the user can see this claw.
// Logic:
//  1. If cfg == nil or ViewRequiresTags is empty → allow all (backward compat)
//  2. If userLogin is in cfg.Admins → allow
//  3. Resolve {user} in each tag pattern to userLogin
//  4. If any resolved pattern matches any tag in clawTags → allow
//  5. Otherwise → deny
func canViewClaw(cfg *types.AccessConfig, userLogin string, clawTags []string) bool {
	if cfg == nil || len(cfg.ViewRequiresTags) == 0 {
		return true
	}
	for _, admin := range cfg.Admins {
		if strings.EqualFold(admin, userLogin) {
			return true
		}
	}
	return matchesTags(cfg.ViewRequiresTags, userLogin, clawTags)
}

// canInteractWithClaw returns true if the user can send messages/kill this claw.
// Same logic as canViewClaw but uses InteractRequiresTags.
func canInteractWithClaw(cfg *types.AccessConfig, userLogin string, clawTags []string) bool {
	if cfg == nil || len(cfg.InteractRequiresTags) == 0 {
		return true
	}
	for _, admin := range cfg.Admins {
		if strings.EqualFold(admin, userLogin) {
			return true
		}
	}
	return matchesTags(cfg.InteractRequiresTags, userLogin, clawTags)
}

// matchesTags checks if any of the required tag patterns (after {user} substitution)
// matches any tag in clawTags. Exact string match after substitution.
func matchesTags(requiredPatterns []string, userLogin string, clawTags []string) bool {
	for _, pattern := range requiredPatterns {
		resolved := strings.ReplaceAll(pattern, "{user}", userLogin)
		for _, tag := range clawTags {
			if resolved == tag {
				return true
			}
		}
	}
	return false
}
