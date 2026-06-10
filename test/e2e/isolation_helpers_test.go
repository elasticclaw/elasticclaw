//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestE2EProviderPrefixIsRunScoped(t *testing.T) {
	got := e2eProviderPrefix(daytonaPrefix, "run-123")
	if got != "ec-e2e-run-123-" {
		t.Fatalf("provider prefix = %q", got)
	}
}

func TestDaytonaProviderConfigIncludesTarget(t *testing.T) {
	config := daytonaProviderConfig(e2eEnv{
		DaytonaAPIKey: "key",
		DaytonaTarget: "eu",
	})

	if !containsAll(config, `api_key: "key"`, `target: "eu"`) {
		t.Fatalf("daytona provider config missing target:\n%s", config)
	}
}

func TestDaytonaProviderConfigOmitsEmptyTarget(t *testing.T) {
	config := daytonaProviderConfig(e2eEnv{
		DaytonaAPIKey: "key",
	})

	if strings.Contains(config, "target:") {
		t.Fatalf("daytona provider config should omit empty target:\n%s", config)
	}
}

func TestDaytonaProviderConfigIncludesOptionalAPIURLAndSnapshot(t *testing.T) {
	config := daytonaProviderConfig(e2eEnv{
		DaytonaAPIKey:   "key",
		DaytonaAPIURL:   "https://daytona.example",
		DaytonaTarget:   "eu",
		DaytonaSnapshot: "daytona-medium",
	})

	if !containsAll(config, `api_url: "https://daytona.example"`, `target: "eu"`, `default_snapshot: "daytona-medium"`) {
		t.Fatalf("daytona provider config missing optional fields:\n%s", config)
	}
}

func TestDaytonaRegionUnavailableDiagnosticIsSkippable(t *testing.T) {
	diagnostic := `Provisioning failed: daytona create: failed to create sandbox: status 403: Region eu is not available to the organization for class linux-vm`
	if !isDaytonaRegionUnavailableDiagnostic(diagnostic) {
		t.Fatalf("diagnostic was not classified as Daytona region unavailable")
	}
}

func TestDaytonaRegionUnavailableDiagnosticDoesNotMaskOtherErrors(t *testing.T) {
	for _, diagnostic := range []string{
		"Daytona bootstrap failed: git clone failed",
		"Provisioning failed: replicated create: region eu is not available for class linux-vm",
		"Provisioning failed: daytona create: status 500 internal server error",
	} {
		if isDaytonaRegionUnavailableDiagnostic(diagnostic) {
			t.Fatalf("diagnostic was incorrectly classified as skippable: %s", diagnostic)
		}
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

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
