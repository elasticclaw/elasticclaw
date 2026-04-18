package hub

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// baseParams returns a minimal valid BootstrapParams for testing.
func baseParams() BootstrapParams {
	return BootstrapParams{
		ClawID:          "test-claw-id-1234",
		ClawName:        "test-claw",
		ClawToken:       "test-token",
		HubURL:          "https://hub.example.com",
		DefaultModel:    "anthropic/claude-sonnet-4-6",
		GatewayPassword: "test-gw-password",
		BridgeURL:       "https://github.com/elasticclaw/elasticclaw/releases/download/v0.0.3/claw-bridge-linux-amd64",
		Nix:             false,
		HubCfg:          &types.HubConfig{},
		GitHubRepos:     nil,
		LLMKeyEnv:       "export ANTHROPIC_API_KEY=\"test-key\"",
		LinearEnv:       "# Linear not configured",
		RelayEnv:        "# Relay not configured",
	}
}

// ── Script content tests ──────────────────────────────────────────────────────

func TestBootstrapScript_ContainsNodeInstall(t *testing.T) {
	script := GenerateReplicatedBootstrapScript(baseParams())
	assertContains(t, script, "nodesource.com/node_24.x", "Node 24 install")
	assertContains(t, script, "nodejs git", "git install alongside node")
}

func TestBootstrapScript_ContainsOpenClaw(t *testing.T) {
	script := GenerateReplicatedBootstrapScript(baseParams())
	assertContains(t, script, "npm install -g openclaw@latest", "openclaw npm install")
	assertContains(t, script, "openclaw gateway run", "openclaw gateway start")
	assertContains(t, script, "18789", "gateway port")
}

func TestBootstrapScript_ContainsBridgeURL(t *testing.T) {
	p := baseParams()
	p.BridgeURL = "https://github.com/elasticclaw/elasticclaw/releases/download/v1.2.3/claw-bridge-linux-amd64"
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, p.BridgeURL, "bridge URL in script")
}

func TestBootstrapScript_OCIBridgeSrc(t *testing.T) {
	p := baseParams()
	p.BridgeURL = "ttl.sh/marc/claw-bridge:1w"
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, "oras pull", "oras pull for OCI refs")
	assertContains(t, script, "ttl.sh/marc/claw-bridge:1w", "OCI bridge src")
	// Make sure it's not being treated as an HTTP URL
	if strings.Contains(script, "curl -fsSL \"ttl.sh") {
		t.Error("OCI ref should not be downloaded via curl")
	}
}

func TestBootstrapScript_BridgeEnvVars(t *testing.T) {
	p := baseParams()
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, `ELASTICCLAW_HUB_URL="https://hub.example.com"`, "hub URL env var")
	assertContains(t, script, `ELASTICCLAW_CLAW_ID="test-claw-id-1234"`, "claw ID env var")
	assertContains(t, script, `ELASTICCLAW_CLAW_TOKEN="test-token"`, "claw token env var")
	assertContains(t, script, `ELASTICCLAW_CLAW_NAME="test-claw"`, "claw name env var")
	assertContains(t, script, `ELASTICCLAW_GATEWAY_PASSWORD="test-gw-password"`, "gateway password env var")
}

func TestBootstrapScript_NixDisabledByDefault(t *testing.T) {
	p := baseParams()
	p.Nix = false
	script := GenerateReplicatedBootstrapScript(p)
	assertNotContains(t, script, "install.determinate.systems", "Nix not installed when disabled")
}

func TestBootstrapScript_NixEnabled(t *testing.T) {
	p := baseParams()
	p.Nix = true
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, "install.determinate.systems/nix", "Determinate Nix install URL")
	assertContains(t, script, "nix-daemon.sh", "nix daemon profile sourcing")
	assertContains(t, script, "/etc/profile.d/nix.sh", "nix persisted in profile.d")
}

func TestBootstrapScript_NixInstalledBeforeOpenClaw(t *testing.T) {
	// Nix must come before OpenClaw so nix-installed tools are available
	p := baseParams()
	p.Nix = true
	script := GenerateReplicatedBootstrapScript(p)
	nixIdx := strings.Index(script, "install.determinate.systems")
	openclawIdx := strings.Index(script, "npm install -g openclaw")
	if nixIdx == -1 {
		t.Fatal("Nix install block not found")
	}
	if openclawIdx == -1 {
		t.Fatal("OpenClaw install block not found")
	}
	if nixIdx > openclawIdx {
		t.Error("Nix install must come BEFORE OpenClaw install in script")
	}
}

func TestBootstrapScript_BridgeStartsBeforeCredentialHelper(t *testing.T) {
	// claw-bridge must start before the credential helper because the
	// credential helper calls the hub API via the bridge's local proxy.
	p := baseParams()
	p.HubCfg = &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
		ClawToken:  "test-token",
	}
	p.GitHubRepos = []types.GitHubRepoAccess{{Repo: "owner/repo", Permissions: "write"}}
	script := GenerateReplicatedBootstrapScript(p)

	bridgeIdx := strings.Index(script, "claw-bridge started")
	credIdx := strings.Index(script, "elasticclaw-git-credentials")
	if bridgeIdx == -1 {
		t.Fatal("bridge start not found in script")
	}
	if credIdx == -1 {
		t.Fatal("credential helper not found in script")
	}
	if credIdx < bridgeIdx {
		t.Error("credential helper must come AFTER bridge starts")
	}
}

func TestBootstrapScript_GitHubCredentialHelper(t *testing.T) {
	p := baseParams()
	p.HubCfg = &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
		ClawToken:  "test-token",
	}
	p.GitHubRepos = []types.GitHubRepoAccess{
		{Repo: "owner/repo", Permissions: "write"},
	}
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, "elasticclaw-git-credentials", "credential helper script")
	assertContains(t, script, "/api/github/token/", "credential helper calls hub token endpoint")
	assertContains(t, script, "git config --global credential.helper", "git configured to use helper")
	assertContains(t, script, "owner/repo", "repo cloned")
}

func TestBootstrapScript_NoGitHubWhenNotConfigured(t *testing.T) {
	p := baseParams()
	p.HubCfg = &types.HubConfig{} // no GitHub apps
	p.GitHubRepos = nil
	script := GenerateReplicatedBootstrapScript(p)
	assertNotContains(t, script, "elasticclaw-git-credentials", "no cred helper when no GitHub app")
}

func TestBootstrapScript_RelayEnv(t *testing.T) {
	p := baseParams()
	p.RelayEnv = `export ELASTICCLAW_RELAY_URL="wss://relay.example.com"
export ELASTICCLAW_HUB_ID="hub123"
export ELASTICCLAW_RELAY_TOKEN="relaytoken"`
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, "ELASTICCLAW_RELAY_URL", "relay URL in bridge env")
	assertContains(t, script, "wss://relay.example.com", "relay URL value")
}

func TestBootstrapScript_LLMKeysInjected(t *testing.T) {
	p := baseParams()
	p.LLMKeyEnv = `export ANTHROPIC_API_KEY="sk-ant-real-key"
export OPENAI_API_KEY="sk-openai-key"`
	script := GenerateReplicatedBootstrapScript(p)
	// LLM keys must appear at the TOP of the script (before any installs)
	// so they're available throughout
	topSection := script[:strings.Index(script, "Install Node")]
	assertContains(t, topSection, "ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY in top of script")
	assertContains(t, topSection, "OPENAI_API_KEY", "OPENAI_API_KEY in top of script")
}

func TestBootstrapScript_GatewayPasswordInBridgeEnv(t *testing.T) {
	// Gateway password must appear in bridge env (not just at top) so bridge can auth
	p := baseParams()
	p.GatewayPassword = "super-secret-pw"
	script := GenerateReplicatedBootstrapScript(p)
	// Count occurrences — should appear at top AND in bridge env section
	count := strings.Count(script, "super-secret-pw")
	if count < 2 {
		t.Errorf("gateway password should appear at least twice (top + bridge env), got %d", count)
	}
}

func TestBootstrapScript_SetEEnabled(t *testing.T) {
	script := GenerateReplicatedBootstrapScript(baseParams())
	assertContains(t, script, "set -euo pipefail", "script uses set -euo pipefail")
}

func TestBootstrapScript_ShebangPresent(t *testing.T) {
	script := GenerateReplicatedBootstrapScript(baseParams())
	if !strings.HasPrefix(script, "#!/bin/bash") {
		t.Error("script must start with #!/bin/bash")
	}
}

// ── Shellcheck test ───────────────────────────────────────────────────────────

func TestBootstrapScript_Shellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck not in PATH — install it to enable this test")
	}

	cases := []struct {
		name   string
		params BootstrapParams
	}{
		{"base", baseParams()},
		{"nix_enabled", func() BootstrapParams { p := baseParams(); p.Nix = true; return p }()},
		{"with_relay", func() BootstrapParams {
			p := baseParams()
			p.RelayEnv = `export ELASTICCLAW_RELAY_URL="wss://relay.example.com"`
			return p
		}()},
		{"with_github", func() BootstrapParams {
			p := baseParams()
			p.HubCfg = &types.HubConfig{
				GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
				ClawToken:  "tok",
			}
			p.GitHubRepos = []types.GitHubRepoAccess{{Repo: "org/repo", Permissions: "write"}}
			return p
		}()},
		{"oci_bridge", func() BootstrapParams {
			p := baseParams()
			p.BridgeURL = "ttl.sh/marc/claw-bridge:1w"
			return p
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := GenerateReplicatedBootstrapScript(tc.params)
			f, err := os.CreateTemp("", "bootstrap-*.sh")
			if err != nil {
				t.Fatalf("create temp file: %v", err)
			}
			defer os.Remove(f.Name())

			if _, err := f.WriteString(script); err != nil {
				t.Fatalf("write script: %v", err)
			}
			f.Close()

			cmd := exec.Command("shellcheck", "-s", "bash",
				"-e", "SC1091", // don't check sourced files
				"-e", "SC2086", // we're ok with unquoted vars in some places
				f.Name(),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("shellcheck failed for %s:\n%s", tc.name, string(out))
			}
		})
	}
}

// ── Container integration test ────────────────────────────────────────────────

func TestBootstrapScript_ContainerRun(t *testing.T) {
	if os.Getenv("ELASTICCLAW_CONTAINER_TESTS") == "" {
		t.Skip("set ELASTICCLAW_CONTAINER_TESTS=1 to run container integration tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not in PATH")
	}

	// Start a mock hub that the container bootstrap will connect to
	mockHub := newMockHub(t)
	defer mockHub.Close()

	p := baseParams()
	p.HubURL = mockHub.URL()
	p.BridgeURL = os.Getenv("ELASTICCLAW_TEST_BRIDGE_URL")
	if p.BridgeURL == "" {
		t.Skip("set ELASTICCLAW_TEST_BRIDGE_URL to a real claw-bridge binary URL")
	}

	script := GenerateReplicatedBootstrapScript(p)

	// Write script to a temp file to mount into container
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "bootstrap.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// Run in Ubuntu 24.04 container — same base as Replicated VMs
	cmd := exec.Command("docker", "run", "--rm",
		"--network=host", // so container can reach mock hub on localhost
		"-v", scriptPath+":/bootstrap.sh:ro",
		"-e", "HOME=/root",
		"ubuntu:24.04",
		"bash", "-c",
		// Stub out slow/network steps for container test
		`
apt-get() { echo "STUB apt-get $*"; }; export -f apt-get
# Run bootstrap but intercept the long-running parts
bash /bootstrap.sh 2>&1
`,
	)

	out, err := cmd.CombinedOutput()
	t.Logf("Container output:\n%s", string(out))

	if err != nil {
		t.Errorf("container bootstrap failed: %v", err)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func assertContains(t *testing.T, s, substr, desc string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected script to contain %s\nwanted: %q", desc, substr)
	}
}

func assertNotContains(t *testing.T, s, substr, desc string) {
	t.Helper()
	if strings.Contains(s, substr) {
		t.Errorf("expected script NOT to contain %s\nfound: %q", desc, substr)
	}
}
