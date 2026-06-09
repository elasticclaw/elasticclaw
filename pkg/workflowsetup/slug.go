package workflowsetup

import (
	"fmt"
	"regexp"
	"strings"
)

const slugPatternText = `^[a-z0-9][a-z0-9_-]{0,62}$`

var slugPattern = regexp.MustCompile(slugPatternText)

// ValidateSlug validates workflow setup names intended for stable identifiers.
func ValidateSlug(name string) error {
	if slugPattern.MatchString(name) {
		return nil
	}
	return fmt.Errorf("invalid slug %q: must match %s", name, slugPatternText)
}

// SlugCandidate returns a lowercase best-effort slug candidate without
// mutating any existing external names.
func SlugCandidate(name string) string {
	var builder strings.Builder
	lastWasHyphen := false

	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			builder.WriteRune(r)
			lastWasHyphen = false
		case r == '-':
			if builder.Len() > 0 && !lastWasHyphen {
				builder.WriteByte('-')
				lastWasHyphen = true
			}
		default:
			if builder.Len() > 0 && !lastWasHyphen {
				builder.WriteByte('-')
				lastWasHyphen = true
			}
		}
	}

	candidate := strings.Trim(builder.String(), "-_")
	if len(candidate) > 63 {
		candidate = candidate[:63]
		candidate = strings.TrimRight(candidate, "-_")
	}
	return candidate
}
