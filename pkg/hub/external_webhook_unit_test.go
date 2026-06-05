package hub

import "testing"

func TestExternalFactoryTriggerKeyUsesBodyFingerprintWhenEventTypeMissing(t *testing.T) {
	payload := externalWebhookPayload{}

	first := externalFactoryTriggerKey("factory", "owner/repo", "", payload, []byte(`{"build":1}`))
	second := externalFactoryTriggerKey("factory", "owner/repo", "", payload, []byte(`{"build":2}`))

	if first == second {
		t.Fatalf("expected distinct keys for distinct typeless payload bodies, got %q", first)
	}
	if first == "factory:owner/repo@" || second == "factory:owner/repo@" {
		t.Fatalf("expected non-empty key suffixes, got %q and %q", first, second)
	}
}

func TestExternalFactoryTriggerKeyPrefersReleaseTag(t *testing.T) {
	payload := externalWebhookPayload{}
	payload.Release = &struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
		Prerelease  bool   `json:"prerelease"`
		Draft       bool   `json:"draft"`
		CreatedAt   string `json:"created_at"`
		PublishedAt string `json:"published_at"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
	}{TagName: "v1.2.3"}

	got := externalFactoryTriggerKey("factory", "owner/repo", "released", payload, []byte(`{}`))
	if got != "factory:owner/repo@v1.2.3" {
		t.Fatalf("key = %q, want release tag key", got)
	}
}
