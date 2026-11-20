package cliversion

import (
	"encoding/json"
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
	if got, want := OpenClawImageVersion, "2026.10.6"; got != want {
		t.Fatalf("OpenClawImageVersion = %q, want %q", got, want)
	}
	if got, want := OpenClawImage, "ghcr.io/openclaw/openclaw:2026.10.6"; got != want {
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

func TestOpenClawLock(t *testing.T) {
	testRuntimeLock(t, OpenClawPackageJSON, OpenClawPackageLock, "openclaw", OpenClawVersion)
}

func TestCodexPluginLock(t *testing.T) {
	testRuntimeLock(t, CodexPluginPackageJSON, CodexPluginPackageLock, "@openclaw/codex", CodexPluginVersion)
}

func testRuntimeLock(t *testing.T, manifestJSON, lockJSON []byte, packageName, version string) {
	t.Helper()
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Dependencies[packageName] != version {
		t.Fatal("package.json must pin requested version")
	}
	var lock struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Version      string            `json:"version"`
			Integrity    string            `json:"integrity"`
			Dependencies map[string]string `json:"dependencies"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lockJSON, &lock); err != nil {
		t.Fatal(err)
	}
	if lock.LockfileVersion != 3 {
		t.Fatalf("lockfileVersion = %d, want 3", lock.LockfileVersion)
	}
	if lock.Packages[""].Dependencies[packageName] != version {
		t.Fatal("lock root must pin requested version")
	}
	if lock.Packages["node_modules/"+packageName].Version != version {
		t.Fatal("locked package must match pinned version")
	}
	for name, pkg := range lock.Packages {
		if name != "" && !strings.HasPrefix(pkg.Integrity, "sha512-") {
			t.Errorf("%s has no sha512 integrity", name)
		}
	}
}
