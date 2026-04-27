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

func TestBootstrapScript_BridgeEnvFileQuotesValues(t *testing.T) {
	script := GenerateReplicatedBootstrapScript(baseParams())
	assertContains(t, script, `printf 'ELASTICCLAW_CLAW_NAME=%q\n' "$ELASTICCLAW_CLAW_NAME"`, "claw name quoted in persisted env")
	assertContains(t, script, `printf 'ELASTICCLAW_GATEWAY_PASSWORD=%q\n' "$ELASTICCLAW_GATEWAY_PASSWORD"`, "gateway password quoted in persisted env")
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

func TestBootstrapScript_BridgeEnvFileEscapesValues(t *testing.T) {
	script := GenerateReplicatedBootstrapScript(baseParams())
	start := strings.Index(script, "# Persist env vars")
	if start == -1 {
		t.Fatal("persist env block not found")
	}
	end := strings.Index(script[start:], "chmod 600")
	if end == -1 {
		t.Fatal("persist env block end not found")
	}
	snippet := script[start : start+end]

	home := t.TempDir()
	name := "My Test Claw"
	password := "p@ss $word `cmd` \" quote"
	cmd := exec.Command("bash", "-c", snippet+`
source "$HOME/.claw-bridge.env"
printf '%s\n' "$ELASTICCLAW_CLAW_NAME" "$ELASTICCLAW_GATEWAY_PASSWORD"
`)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"ELASTICCLAW_HUB_URL=https://hub.example.com",
		"ELASTICCLAW_CLAW_ID=test-claw-id-1234",
		"ELASTICCLAW_CLAW_TOKEN=test-token",
		"ELASTICCLAW_CLAW_NAME="+name,
		"ELASTICCLAW_GATEWAY_PASSWORD="+password,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("source bridge env file: %v\n%s", err, string(out))
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != 2 || got[0] != name || got[1] != password {
		t.Fatalf("sourced values mismatch: got %q, want %q", got, []string{name, password})
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

func TestBuildOnboardFlags_OpenAICompatibleProviders(t *testing.T) {
	cases := []struct {
		name       string
		provider   string
		envVar     string
		authChoice string
		flagName   string
	}{
		{
			name:       "openai",
			provider:   "openai",
			envVar:     "OPENAI_API_KEY",
			authChoice: "openai-api-key",
			flagName:   "--openai-api-key",
		},
		{
			name:       "groq",
			provider:   "groq",
			envVar:     "GROQ_API_KEY",
			authChoice: "groq-api-key",
			flagName:   "--groq-api-key",
		},
		{
			name:       "deepseek",
			provider:   "deepseek",
			envVar:     "DEEPSEEK_API_KEY",
			authChoice: "deepseek-api-key",
			flagName:   "--deepseek-api-key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := []*types.LLMKeyConfig{
				{Name: tc.name + "-key", Provider: tc.provider, Default: true},
			}
			flags := buildOnboardFlags(keys, "")
			assertContains(t, flags, "--auth-choice "+tc.authChoice, "provider auth choice")
			assertContains(t, flags, tc.flagName+` "${`+tc.envVar+`}"`, "provider api key flag")
			assertNotContains(t, flags, "anthropic-api-key", "should not fallback to anthropic")
		})
	}
}

func TestBuildOpenClawProviderConfig_OpenAICompatibleProviders(t *testing.T) {
	keys := []*types.LLMKeyConfig{
		{Name: "openai-main", Provider: "openai", Default: true},
		{Name: "groq-main", Provider: "groq"},
		{Name: "deepseek-main", Provider: "deepseek"},
	}

	snippet := buildOpenClawProviderConfig(keys, "openai-main")

	assertContains(t, snippet, `'openai': {`, "openai provider entry")
	assertContains(t, snippet, "'baseUrl': 'https://api.openai.com/v1'", "openai baseUrl")
	assertContains(t, snippet, "{'id': 'gpt-4o',      'name': 'GPT-4o'}", "openai models")

	assertContains(t, snippet, `'groq': {`, "groq provider entry")
	assertContains(t, snippet, "'baseUrl': 'https://api.groq.com/openai/v1'", "groq baseUrl")
	assertContains(t, snippet, "{'id': 'llama-3.3-70b-versatile', 'name': 'Llama 3.3 70B'}", "groq models")

	assertContains(t, snippet, `'deepseek': {`, "deepseek provider entry")
	assertContains(t, snippet, "'baseUrl': 'https://api.deepseek.com/v1'", "deepseek baseUrl")
	assertContains(t, snippet, "{'id': 'deepseek-chat', 'name': 'DeepSeek Chat'}", "deepseek models")
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
				"-e", "SC2016", // $() in single quotes is intentional (expands on remote host)
				f.Name(),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("shellcheck failed for %s:\n%s", tc.name, string(out))
			}
		})
	}
}

// ── Stdin-pipe parse test ──────────────────────────────────────────────

// TestBootstrapScript_StdinExec executes the bootstrap script via stdin to bash with all
// network/system commands stubbed out. This is the same code path as sshRun() which uses
// sess.Stdin = strings.NewReader(script) piped to /bin/bash.
// Catches heredoc-in-stdin parse bugs that shellcheck misses (shellcheck reads from a file,
// bash -n also misses this since it doesn't actually consume heredoc bodies from stdin).
func TestBootstrapScript_StdinExec(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}

	cases := []struct {
		name   string
		params BootstrapParams
	}{
		{"base", baseParams()},
		{"with_github", func() BootstrapParams {
			p := baseParams()
			p.HubCfg = &types.HubConfig{
				GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
				ClawToken:  "tok",
			}
			p.GitHubRepos = []types.GitHubRepoAccess{{Repo: "org/repo", Permissions: "write"}}
			return p
		}()},
		{"nix_enabled", func() BootstrapParams { p := baseParams(); p.Nix = true; return p }()},
		// Regression: GitHub App configured but no repos — was producing empty { } group command
		{"github_app_no_repos", func() BootstrapParams {
			p := baseParams()
			p.HubCfg = &types.HubConfig{
				GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
				ClawToken:  "tok",
			}
			p.GitHubRepos = nil // no repos
			return p
		}()},
	}

	// We need to verify heredoc-in-stdin parsing works.
	// Strategy: prepend function stubs for every command and use a fake PATH
	// so the script runs to completion without actually doing anything.
	stubScript := `
set +e
# Stub everything
for cmd in curl apt-get npm sudo systemctl git gh oras python3 gpg tee chmod mv nohup; do
  eval "${cmd}() { return 0; }"
done
curl() { echo stub_curl_output; return 0; }
openclaw() {
  if [ "$1" = "--version" ]; then echo "OpenClaw 2026.1.0"; fi
  return 0
}
`

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := GenerateReplicatedBootstrapScript(tc.params)
			// Replace set -euo pipefail with set -uo pipefail so stubs can fail gracefully
			script = strings.Replace(script, "set -euo pipefail", "set -uo pipefail", 1)
			full := stubScript + script
			// Execute via stdin — same as sshRun()
			cmd := exec.Command("bash")
			cmd.Stdin = strings.NewReader(full)
			out, err := cmd.CombinedOutput()
			// We allow non-zero exit (stubs may fail mid-script)
			// What we're checking is no *parse* error from the heredoc-in-stdin pattern
			if err != nil && strings.Contains(string(out), "syntax error") {
				t.Errorf("bash syntax error (stdin exec) for %s:\n%s", tc.name, string(out))
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
