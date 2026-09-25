package cliversion

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

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

func TestAgentDockerfilePinMatchesOpenClawVersion(t *testing.T) {
	// docker/agent.Dockerfile pre-installs OpenClaw with a hardcoded npm pin
	// ("Keep in sync with pkg/cliversion OpenClawVersion"). Enforce that sync
	// so the image cannot silently drift from the version bootstrap verifies.
	data, err := os.ReadFile(filepath.Join("..", "..", "docker", "agent.Dockerfile"))
	if err != nil {
		t.Fatalf("read agent.Dockerfile: %v", err)
	}
	m := regexp.MustCompile(`(?m)^RUN npm install -g openclaw@(\S+)`).FindSubmatch(data)
	if m == nil {
		t.Fatal("agent.Dockerfile: no `npm install -g openclaw@<version>` pin found")
	}
	if got := string(m[1]); got != OpenClawVersion {
		t.Fatalf("agent.Dockerfile pins openclaw@%s, want OpenClawVersion %s", got, OpenClawVersion)
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
