package cliversion

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

// TestNodeCompatibleExpr exercises the exact JS expression the bootstrap
// scripts embed, with injected Node versions, against the accepted range
// ">=24.22.1 <25 || >=26.12.1" (OpenClaw engines plus the CVE-2026-111497 fix).
func TestNodeCompatibleExpr(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not in PATH")
	}
	cases := []struct {
		version string
		want    bool
	}{
		{"24.16.0", false}, // OpenClaw floor, still vulnerable
		{"24.21.0", false}, // Daytona images, still vulnerable
		{"24.22.0", false},
		{"24.22.1", true}, // patched floor
		{"24.23.0", true},
		{"25.0.0", false}, // 25.x is excluded outright
		{"25.9.1", false},
		{"26.1.0", false}, // OpenClaw floor, still vulnerable
		{"26.12.0", false},
		{"26.12.1", true}, // patched floor
		{"26.13.0", true},
		{"27.0.0", true}, // range is unbounded above 26.12.1
		{"28.3.1", true},
	}
	for _, tc := range cases {
		t.Run(tc.version, func(t *testing.T) {
			script := "const [major, minor, patch] = process.argv[1].split(\".\").map(Number);" +
				"process.exit(" + NodeCompatibleExpr + " ? 0 : 1)"
			err := exec.Command("node", "-e", script, tc.version).Run()
			if tc.want && err != nil {
				t.Fatalf("node %s rejected, want accepted (exit=%v)", tc.version, err)
			}
			if !tc.want && err == nil {
				t.Fatalf("node %s accepted, want rejected", tc.version)
			}
		})
	}
}

func TestAgentDockerfileChecksNodeCompatible(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docker", "agent.Dockerfile"))
	if err != nil {
		t.Fatalf("read agent.Dockerfile: %v", err)
	}
	if !strings.Contains(string(data), "\nRUN "+NodeCompatibleCheck+"\n") {
		t.Fatal("agent.Dockerfile: missing `RUN <cliversion.NodeCompatibleCheck>` after the Node install")
	}
}
