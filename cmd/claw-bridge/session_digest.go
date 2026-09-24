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

// Digests deliberately discard the rest of any line containing a secret key.
// Keep this separate from the live activity sanitizer: losing nearby context is
// preferable to copying a credential into a future agent's resume prompt.
func sanitizeSessionDigestText(value string) string {
	value = digestURLUserinfo.ReplaceAllString(value, "${1}[redacted]@")
	lines := strings.Split(value, "\n")
	blockIndent := -1
	pendingValue, pem := false, false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if pem || pendingValue || (blockIndent >= 0 && (indent > blockIndent || strings.HasPrefix(trimmed, "-----BEGIN"))) {
			if strings.Contains(line, "-----BEGIN") {
				pem = true
			}
			if strings.Contains(line, "-----END") {
				pem = false
			}
			pendingValue = strings.Trim(line, " \t\r\\\"':=") == ""
			lines[i] = line[:indent] + "[redacted]"
			continue
		}
		blockIndent = -1
		for _, match := range digestIdentifier.FindAllStringIndex(line, -1) {
			key := line[match[0]:match[1]]
			if !digestSecretKey(key) {
				continue
			}
			// A source location is useful context, not a credential assignment.
			if strings.Contains(key, ".") && digestSourceLine.MatchString(line[match[1]:]) {
				continue
			}
			rest := strings.TrimLeft(line[match[1]:], " \t\r\\\"':=")
			pendingValue = rest == ""
			pem = strings.HasPrefix(rest, "-----BEGIN") && !strings.Contains(rest, "-----END")
			// Also cover indented values after a colon, YAML block scalars, and
			// multiline quoted values. Over-redaction here is intentional.
			blockIndent = indent
			lines[i] = line[:match[1]] + " [redacted]"
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
