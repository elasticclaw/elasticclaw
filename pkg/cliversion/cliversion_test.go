package cliversion

import "testing"

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
