//go:build e2e

package e2e

import "testing"

func TestE2EProviderPrefixIsRunScoped(t *testing.T) {
	got := e2eProviderPrefix(daytonaPrefix, "run-123")
	if got != "ec-e2e-run-123-" {
		t.Fatalf("provider prefix = %q", got)
	}
}

func TestSanitizeIDMatchesHostnameSafeRunID(t *testing.T) {
	if got := sanitizeID("MY_RUN.123"); got != "my-run-123" {
		t.Fatalf("sanitizeID = %q", got)
	}
}

func TestGitHubE2EHookURLMatchesOnlyWorkspace(t *testing.T) {
	current := "https://example.ngrok-free.app/api/workspaces/e2e-run-a/webhooks/github-issues"
	other := "https://example.ngrok-free.app/api/workspaces/e2e-run-b/webhooks/github-issues"

	if !githubE2EHookURLMatchesWorkspace(current, "e2e-run-a") {
		t.Fatalf("current workspace hook did not match")
	}
	if githubE2EHookURLMatchesWorkspace(other, "e2e-run-a") {
		t.Fatalf("other workspace hook matched current workspace")
	}
	if !isGitHubE2EHookURL(other) {
		t.Fatalf("broad sweep matcher should still recognize E2E hook")
	}
}

func TestLinearE2EWebhookURLMatchesOnlyWorkspace(t *testing.T) {
	current := "https://example.ngrok-free.app/api/workspaces/e2e-linear-run-a/webhooks/linear"
	other := "https://example.ngrok-free.app/api/workspaces/e2e-linear-run-b/webhooks/linear"

	if !linearE2EWebhookURLMatchesWorkspace(current, "e2e-linear-run-a") {
		t.Fatalf("current workspace webhook did not match")
	}
	if linearE2EWebhookURLMatchesWorkspace(other, "e2e-linear-run-a") {
		t.Fatalf("other workspace webhook matched current workspace")
	}
	if !isLinearE2EWebhookURL(other) {
		t.Fatalf("broad sweep matcher should still recognize E2E webhook")
	}
}

func TestGitHubE2EIssueMatchesOnlyRun(t *testing.T) {
	labels := []struct {
		Name string `json:"name"`
	}{{Name: "agent-ready-run-a"}}

	if !isE2EIssueForRun("Tell a dad joke. Do not make a PR.", "ElasticClaw E2E run: run-a", labels, "run-a", "agent-ready-run-a") {
		t.Fatalf("current run issue did not match")
	}
	if isE2EIssueForRun("Tell a dad joke. Do not make a PR.", "ElasticClaw E2E run: run-a-extra", nil, "run-a", "agent-ready-run-a") {
		t.Fatalf("run id prefix matched another run")
	}
	if isE2EIssueForRun("Tell a dad joke. Do not make a PR.", "ElasticClaw E2E run: run-b", labels, "run-c", "agent-ready-run-c") {
		t.Fatalf("other run issue matched current run")
	}
	if !isE2EIssue("Tell a dad joke. Do not make a PR.", "ElasticClaw E2E run: run-b", labels) {
		t.Fatalf("broad sweep matcher should still recognize E2E issue")
	}
}
