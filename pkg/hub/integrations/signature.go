package integrations

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// This file hosts the one shared webhook signature helper used by every
// integration (spec item 2.2 step 4: extract signature validation into a
// shared helper and audit that every integration validates signatures).
//
// Audit result (behavior unchanged by the extraction):
//
//   - Linear    — HMAC-SHA256 hex in the Linear-Signature header, checked
//     against workspace, integration, factory and workflow secrets;
//     accepts when no secret is configured (open webhook).
//   - GitHub PR — HMAC-SHA256 in X-Hub-Signature-256 ("sha256=" prefix),
//     checked against factory secrets; REJECTS when no secret is
//     configured (strict).
//   - GitHub Issues — HMAC-SHA256 in X-Hub-Signature-256, checked against
//     workspace and factory secrets; global endpoint rejects when no
//     secret is configured, workspace-scoped endpoint accepts.
//   - Shortcut  — HMAC-SHA256 in Payload-Signature ("sha256=" prefix),
//     checked against workspace and factory secrets; accepts when no
//     secret is configured.
//   - Jira      — Jira cannot sign payloads, so a shared secret is compared
//     (constant-time) from X-ElasticClaw-Webhook-Secret or ?secret=;
//     accepts when no secret is configured.
//   - External  — HMAC-SHA256 in X-Webhook-Signature per factory; a factory
//     without a secret only matches unsigned deliveries.

// verifyHMACSHA256 reports whether sig is the hex HMAC-SHA256 of body under
// secret. An optional "sha256=" prefix (GitHub/Shortcut convention) is
// stripped. Empty secrets and empty signatures never match, and the
// comparison is constant-time.
func verifyHMACSHA256(body []byte, secret, sig string) bool {
	sig = strings.TrimPrefix(sig, "sha256=")
	if secret == "" || sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

// constantTimeStringEqual compares two shared secrets in constant time
// (used by the Jira webhook, which cannot sign payloads).
func constantTimeStringEqual(a, b string) bool {
	if a == "" || b == "" || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
