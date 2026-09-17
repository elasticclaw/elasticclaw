package cliversion

import "testing"

func TestPinnedOpenClawVersions(t *testing.T) {
	if got, want := OpenClawVersion, "2026.9.4"; got != want {
		t.Fatalf("OpenClawVersion = %q, want %q", got, want)
	}
	if got, want := OpenClawImageVersion, "2026.9.4"; got != want {
		t.Fatalf("OpenClawImageVersion = %q, want %q", got, want)
	}
	if got, want := OpenClawImage, "ghcr.io/openclaw/openclaw:2026.9.4"; got != want {
		t.Fatalf("OpenClawImage = %q, want %q", got, want)
	}
}

func TestPinnedCodexCLIVersion(t *testing.T) {
	if CodexCLIVersion != "0.153.4" {
		t.Fatalf("CodexCLIVersion = %q, want 0.153.4", CodexCLIVersion)
	}
}

func TestPinnedCodexPluginVersion(t *testing.T) {
	if CodexPluginVersion != "2026.9.4" {
		t.Fatalf("CodexPluginVersion = %q, want 2026.9.4", CodexPluginVersion)
	}
}

func TestPinnedGrokCLIVersion(t *testing.T) {
	if GrokCLIVersion != "0.2.103" {
		t.Fatalf("GrokCLIVersion = %q, want 0.2.103", GrokCLIVersion)
	}
}

func TestFromEnvAllowsPinnedOverride(t *testing.T) {
	t.Setenv("ELASTICCLAW_TEST_CLI_VERSION", "9.8.7")
	if got := FromEnv("ELASTICCLAW_TEST_CLI_VERSION", "1.2.3"); got != "9.8.7" {
		t.Fatalf("FromEnv override = %q", got)
	}
	if got := FromEnv("ELASTICCLAW_MISSING_VERSION", "1.2.3"); got != "1.2.3" {
		t.Fatalf("FromEnv fallback = %q", got)
	}
}
