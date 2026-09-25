package main

import (
	"regexp"
	"strings"
)

var digestIdentifier = regexp.MustCompile(`[a-zA-Z0-9_.-]+`)
var digestURLUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s?#@]+@`)
var digestCamelWord = regexp.MustCompile(`([a-z0-9])([A-Z])`)
var digestCamelAcronym = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
var digestSourceLine = regexp.MustCompile(`^:[0-9]+:`)

func digestSecretKey(key string) bool {
	key = digestCamelAcronym.ReplaceAllString(key, "${1}_${2}")
	key = digestCamelWord.ReplaceAllString(key, "${1}_${2}")
	parts := strings.FieldsFunc(strings.ToLower(key), func(r rune) bool {
		return r == '_' || r == '-' || r == '.'
	})
	for i, part := range parts {
		switch part {
		case "token", "secret", "password", "passwd", "apikey", "credential", "credentials", "auth", "authorization", "oauth", "cookie", "bearer":
			return true
		case "api", "access", "private":
			if i+1 < len(parts) && parts[i+1] == "key" {
				return true
			}
		}
	}
	return false
}

// Digests drop everything from the first line that holds a secret key or a
// PEM block. A value can continue past that line in too many ways (quotes,
// shell continuations, YAML blocks, JSON) to track reliably, and losing the
// tail of a digest entry is cheaper than copying a credential into a future
// agent's resume prompt. Keep this separate from the live activity sanitizer.
func sanitizeSessionDigestText(value string) string {
	value = digestURLUserinfo.ReplaceAllString(value, "${1}[redacted]@")
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		if digestLineHasSecret(line) {
			return strings.TrimSpace(strings.Join(append(lines[:i:i], "[redacted]"), "\n"))
		}
	}
	return strings.TrimSpace(value)
}

func digestLineHasSecret(line string) bool {
	if strings.Contains(line, "-----BEGIN") {
		return true
	}
	for _, match := range digestIdentifier.FindAllStringIndex(line, -1) {
		key := line[match[0]:match[1]]
		// A source location is useful context, not a credential assignment.
		if digestSecretKey(key) && !(strings.Contains(key, ".") && digestSourceLine.MatchString(line[match[1]:])) {
			return true
		}
	}
	return false
}
