package cliversion

import "testing"

func TestPinnedOpenClawVersions(t *testing.T) {
	if got, want := OpenClawVersion, "2026.7.1-2"; got != want {
		t.Fatalf("OpenClawVersion = %q, want %q", got, want)
	}
	if got, want := OpenClawImageVersion, "2026.7.1"; got != want {
		t.Fatalf("OpenClawImageVersion = %q, want %q", got, want)
	}
	if got, want := OpenClawImage, "ghcr.io/openclaw/openclaw:2026.7.1"; got != want {
		t.Fatalf("OpenClawImage = %q, want %q", got, want)
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
