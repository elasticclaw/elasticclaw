package main

import (
	"strings"
	"testing"
)

func TestActivityAndDigestRedaction(t *testing.T) {
	// Activity expectations intentionally retain origin/main's behavior plus
	// quoted-value handling; only the digest uses aggressive redaction.
	for _, tt := range []struct{ input, activity, digest string }{
		{"Run: GITHUB_TOKEN=SECRET gh pr list", "Run: GITHUB_TOKEN=[redacted] gh pr list", "Run: GITHUB_TOKEN [redacted]"},
		{"- run: GITHUB_TOKEN=SECRET", "- run: GITHUB_TOKEN=[redacted]", "- run: GITHUB_TOKEN [redacted]"},
		{"cookie: token=SECRET; other=1", "cookie: token=[redacted] other=1", "cookie [redacted]"},
		{"env: API_KEY=SECRET", "env: api_key=[redacted]", "env: API_KEY [redacted]"},
		{"note: access_token=SECRET", "note: access_token=[redacted]", "note: access_token [redacted]"},
		{"user:token=SECRET", "user:token=[redacted]", "user:token [redacted]"},
		{"step:\n  GITHUB_TOKEN=SECRET", "step:\n  GITHUB_TOKEN=[redacted]", "step:\n  GITHUB_TOKEN [redacted]"},
		{"Authorization: token ghp_SECRET", "Authorization: token ghp_SECRET", "Authorization [redacted]"},
		{"Authorization: Token X", "Authorization: Token X", "Authorization [redacted]"},
		{"Authorization: ApiKey X", "Authorization: ApiKey X", "Authorization [redacted]"},
		{"Token X", "Token X", "Token [redacted]"},
		{"ApiKey X", "ApiKey X", "ApiKey [redacted]"},
		{"private_key: |\n  -----BEGIN PRIVATE KEY-----\n  SECRET\n  -----END PRIVATE KEY-----\nnext: safe", "private_key: |\n  -----BEGIN PRIVATE KEY-----\n  SECRET\n  -----END PRIVATE KEY-----\nnext: safe", "private_key [redacted]\n  [redacted]\n  [redacted]\n  [redacted]\nnext: safe"},
		{"private_key: >\n  SECRET\nnext: safe", "private_key: >\n  SECRET\nnext: safe", "private_key [redacted]\n  [redacted]\nnext: safe"},
		{"private_key: -----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----\nnext: safe", "private_key: -----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----\nnext: safe", "[redacted]\n[redacted]\n[redacted]\nnext: safe"},
		{"api_key:\nSECRET\nnext: safe", "api_key:\nSECRET\nnext: safe", "api_key [redacted]\n[redacted]\nnext: safe"},
		{"{\"api_key\":\n \"SECRET\"}", "{\"api_key\":\n \"SECRET\"}", "{\"api_key [redacted]\n [redacted]"},
		{"{\"api_key\" \r\n\t: \r\n\t\"SECRET\"}", "{\"api_key\" \r\n\t: \r\n\t\"SECRET\"}", "{\"api_key [redacted]\n\t[redacted]\n\t[redacted]"},
		{`GH_TOKEN="SECRET"`, `GH_TOKEN=[redacted]`, `GH_TOKEN [redacted]`},
		{`GH_TOKEN='SECRET'`, `GH_TOKEN=[redacted]`, `GH_TOKEN [redacted]`},
		{`GH_TOKEN="SECRET`, `GH_TOKEN=[redacted]`, `GH_TOKEN [redacted]`},
		{`api_key="first\"SECRET" path=main.go`, `api_key=[redacted]SECRET" path=main.go`, `api_key [redacted]`},
		{`api_key='first\'SECRET' path=main.go`, `api_key=[redacted]SECRET' path=main.go`, `api_key [redacted]`},
		{`curl -d "{\"api_key\":\"first\\\"SECRET\",\"path\":\"main.go\"}"`, `curl -d "{\"api_key\":\"first\\\"SECRET\",\"path\":\"main.go\"}"`, `curl -d "{\"api_key [redacted]`},
		{`https://user:SECRET@host/path`, `https://user:SECRET@host/path`, `https://[redacted]@host/path`},
		{`https://x-access-token:SECRET@github.com/org/repo.git`, `https://x-access-token:SECRET@github.com/org/repo.git`, `https://[redacted]@github.com/org/repo.git`},
		{`curl https://user:SECRET@host/a https://user:SECRET@host/b`, `curl https://user:SECRET@host/a https://user:SECRET@host/b`, `curl https://[redacted]@host/a https://[redacted]@host/b`},
		{"git log --author=ana", "git log --author=ana", "git log --author=ana"},
		{"Co-Authored-By: X", "Co-Authored-By: X", "Co-Authored-By: X"},
		{"auth.go:12: msg", "auth.go:12: msg", "auth.go:12: msg"},
		{"max_tokens=4096 tokens=4096 maxTokens=4096", "max_tokens=4096 tokens=4096 maxTokens=4096", "max_tokens=4096 tokens=4096 maxTokens=4096"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			if got := sanitizeActivityText(tt.input); got != tt.activity {
				t.Errorf("activity = %q, want %q", got, tt.activity)
			}
			if got := sanitizeSessionDigestText(tt.input); got != tt.digest {
				t.Errorf("digest = %q, want %q", got, tt.digest)
			}
		})
	}
}

func TestSessionDigestRedactsMultilineSecrets(t *testing.T) {
	for _, input := range []string{
		"-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----",
		"prefix: -----BEGIN RSA PRIVATE KEY-----\nSECRET\n-----END RSA PRIVATE KEY-----",
		"note -----BEGIN CERTIFICATE-----\nSECRET\n-----END CERTIFICATE-----",
		"-----BEGIN PRIVATE KEY-----\n-----END CERTIFICATE-----\nSECRET\n-----END PRIVATE KEY-----",
		"private_key: |\n-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----",
		"private_key:\n-----BEGIN PRIVATE KEY-----\nSECRET\n-----END PRIVATE KEY-----",
		"password: \"first\n  SECRET\"",
		"password:\n\nSECRET",
		"token: |\n  SECRET\n\n  STILL_SECRET",
		"PASSWORD=\"first\nSECRET\"",
		"api_key: 'first\nSECRET\nSECRET'",
	} {
		t.Run(input, func(t *testing.T) {
			if got := sanitizeSessionDigestText(input + "\nnext: safe"); strings.Contains(got, "SECRET") || !strings.HasSuffix(got, "next: safe") {
				t.Fatalf("multiline secret leaked: %q", got)
			}
		})
	}
}
