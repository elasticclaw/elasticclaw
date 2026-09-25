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
	pendingValue := false
	pemEnd := ""
	openQuote := ""
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		// A quoted secret value that spans lines stays redacted until it closes.
		if openQuote != "" {
			// Scan the whole line: it may close this quote and open another.
			openQuote = quoteStateAfter(openQuote, line)
			lines[i] = line[:indent] + "[redacted]"
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "-----BEGIN") || pemEnd != "" || pendingValue || (blockIndent >= 0 && indent > blockIndent) {
			if start := strings.Index(line, "-----BEGIN"); start >= 0 && pemEnd == "" {
				label := line[start+len("-----BEGIN"):]
				pemEnd = "-----END"
				if end := strings.Index(label, "-----"); end >= 0 {
					pemEnd += label[:end] + "-----"
				}
			}
			if pemEnd != "" && strings.Contains(line, pemEnd) {
				pemEnd = ""
			}
			pendingValue = strings.Trim(line, " \t\r\\\"':=") == "" || endsWithContinuation(line)
			// A value continued onto this line may open a quote that closes later.
			openQuote = quoteStateAfter("", line)
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
			pendingValue = rest == "" || endsWithContinuation(line)
			// Any quote still open at the end of the line (from this or a later
			// assignment) keeps the following lines redacted until it closes.
			openQuote = quoteStateAfter("", line)
			// Also cover indented values after a colon, YAML block scalars, and
			// multiline quoted values. Over-redaction here is intentional.
			blockIndent = indent
			// Redact the whole line: text before the key can hold the secret too
			// (echo ghp_x | gh auth login), and so can an identifier that matched.
			lines[i] = line[:indent] + "[redacted]"
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// quoteStateAfter returns the quote still open after scanning s, starting
// with open already open ("" when none).
func quoteStateAfter(open, s string) string {
	var state byte
	if open != "" {
		state = open[0]
	}
	escaped := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case state == 0 && (c == '"' || c == '\''):
			state = c
		case c == state:
			state = 0
		}
	}
	if state == 0 {
		return ""
	}
	return string(state)
}

// endsWithContinuation reports a shell-style trailing backslash continuation.
func endsWithContinuation(line string) bool {
	return strings.HasSuffix(strings.TrimRight(line, " \t\r"), "\\")
}
