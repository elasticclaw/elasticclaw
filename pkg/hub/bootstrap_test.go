package hub

import (
	"encoding/json"
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

func TestBootstrapScript_ContainsBootstrapMode(t *testing.T) {
	// New flow: script downloads bridge and starts it with bootstrap enabled;
	// Node/OpenClaw/gateway steps live inside claw-bridge Go code.
	script := GenerateReplicatedBootstrapScript(baseParams())
	assertContains(t, script, "ELASTICCLAW_BOOTSTRAP=1", "bootstrap env var set")
	assertContains(t, script, "/usr/local/bin/claw-bridge >> \"$HOME/claw-bridge.log\" 2>&1 </dev/null &", "bridge backgrounded in bootstrap mode")
	assertContains(t, script, "ELASTICCLAW_BOOTSTRAP_NOTIFY_FILE", "bootstrap completion notify file set")
	assertNotContains(t, script, "exec /usr/local/bin/claw-bridge", "bridge must not block SSH session")
	// Node/OpenClaw installs are NOT in the bash script anymore
	assertNotContains(t, script, "nodesource.com", "Node install not in bash script")
	assertNotContains(t, script, "npm install -g openclaw", "openclaw install not in bash script")
}

func TestBootstrapScript_ContainsBridgeURL(t *testing.T) {
	p := baseParams()
	p.BridgeURL = "https://github.com/elasticclaw/elasticclaw/releases/download/v1.2.3/claw-bridge-linux-amd64"
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, p.BridgeURL, "bridge URL in script")
}

func TestDaytonaBridgeCommands_AreAsyncAndIdempotent(t *testing.T) {
	prep := daytonaPrepareBridgeCommand()
	cmd := daytonaAsyncBridgeCommand("https://hub.example.com", "claw-123", "token-123", "NEXT-156")
	running := daytonaBridgeRunningCommand()

	assertContains(t, prep, "pgrep -x claw-bridge", "detects already running bridge from previous start behavior")
	assertContains(t, prep, "set -e", "prep fails fast instead of masking install errors")
	assertContains(t, prep, "[ ! -s /tmp/claw-bridge ]", "reports missing downloaded bridge before install")
	assertContains(t, prep, "sudo install -m 0755 /tmp/claw-bridge /usr/local/bin/claw-bridge", "installs bridge outside tmp before execution")
	assertContains(t, prep, "claw-bridge installed at /usr/local/bin/claw-bridge is not executable", "reports non-executable install")
	assertContains(t, cmd, "exec /usr/local/bin/claw-bridge", "runs installed bridge from async session command")
	assertContains(t, cmd, "claw-bridge.pid", "writes pid file for idempotent retries")
	assertContains(t, cmd, "kill -0", "validates existing and newly started process")
	assertContains(t, cmd, `ELASTICCLAW_CLAW_ID="claw-123"`, "passes claw id to bridge")
	assertNotContains(t, cmd, "nohup /tmp/claw-bridge", "does not execute bridge directly from tmp")
	assertNotContains(t, cmd, "setsid", "does not rely on shell detach when Daytona async sessions are available")

	assertContains(t, running, "claw-bridge.pid", "retry guard checks bridge pid file")
	assertContains(t, running, "pgrep -x claw-bridge", "retry guard detects already running bridge")
}

func TestBootstrapScript_ConnectorDownloadRetriesWithUserFacingLabel(t *testing.T) {
	script := GenerateReplicatedBootstrapScript(baseParams())
	assertContains(t, script, "CONNECTOR_ATTEMPTS=6", "connector retry count")
	assertContains(t, script, "CONNECTOR_DELAYS=(5 10 20 40 60)", "connector retry backoff")
	assertContains(t, script, "Downloading ElasticClaw connector", "user-facing connector label")
	assertContains(t, script, "Retrying connector download in", "retry status")
	assertContains(t, script, "could not download ElasticClaw connector", "user-facing failure")
	assertNotContains(t, script, "Downloading claw-bridge from", "implementation name hidden from remote output")
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
	assertContains(t, script, `ELASTICCLAW_HUB_URL='https://hub.example.com'`, "hub URL env var")
	assertContains(t, script, `ELASTICCLAW_CLAW_ID='test-claw-id-1234'`, "claw ID env var")
	assertContains(t, script, `ELASTICCLAW_CLAW_TOKEN='test-token'`, "claw token env var")
	assertContains(t, script, `ELASTICCLAW_CLAW_NAME='test-claw'`, "claw name env var")
	assertContains(t, script, `ELASTICCLAW_GATEWAY_PASSWORD='test-gw-password'`, "gateway password env var")
}

func TestBootstrapScript_BridgeEnvFileQuotesValues(t *testing.T) {
	script := GenerateReplicatedBootstrapScript(baseParams())
	assertContains(t, script, `printf 'export ELASTICCLAW_CLAW_NAME=%q\n' "$ELASTICCLAW_CLAW_NAME"`, "claw name quoted in persisted env")
	assertContains(t, script, `printf 'export ELASTICCLAW_GATEWAY_PASSWORD=%q\n' "$ELASTICCLAW_GATEWAY_PASSWORD"`, "gateway password quoted in persisted env")
}

func TestBootstrapScript_NixDisabledByDefault(t *testing.T) {
	p := baseParams()
	p.Nix = false
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, `ELASTICCLAW_NIX='false'`, "Nix disabled flag passed to bridge")
}

func TestBootstrapScript_NixEnabled(t *testing.T) {
	p := baseParams()
	p.Nix = true
	script := GenerateReplicatedBootstrapScript(p)
	assertContains(t, script, `ELASTICCLAW_NIX='true'`, "Nix enabled flag passed to bridge")
	// Nix install itself is now in claw-bridge Go code, not bash
	assertNotContains(t, script, "install.determinate.systems", "Nix URL not in bash script")
}

func TestBootstrapScript_BridgeStartsBeforeCredentialHelper(t *testing.T) {
	// claw-bridge is started before the credential helper section (which is a
	// separate SSH step now, not in the generated script).
	p := baseParams()
	p.HubCfg = &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
		ClawToken:  "test-token",
	}
	p.GitHubRepos = []types.GitHubRepoAccess{{Repo: "owner/repo", Permissions: "write"}}
	script := GenerateReplicatedBootstrapScript(p)

	assertContains(t, script, "/usr/local/bin/claw-bridge >> \"$HOME/claw-bridge.log\" 2>&1 </dev/null &", "bridge started")
	// Credential helper is NOT in the generated script — it runs as a separate SSH step
	assertNotContains(t, script, "elasticclaw-git-credentials", "cred helper not in bootstrap script")
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
	// LLM keys must appear before starting claw-bridge so they're in the environment
	bridgeIdx := strings.Index(script, "/usr/local/bin/claw-bridge >>")
	if bridgeIdx == -1 {
		t.Fatal("claw-bridge start not found")
	}
	topSection := script[:bridgeIdx]
	assertContains(t, topSection, "ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY before bridge start")
	assertContains(t, topSection, "OPENAI_API_KEY", "OPENAI_API_KEY before bridge start")
}

func TestBootstrapScript_OnboardFlagsShellQuoted(t *testing.T) {
	p := baseParams()
	p.OnboardFlags = buildOnboardFlags(nil, "", p.DefaultModel)
	script := GenerateReplicatedBootstrapScript(p)

	assertContains(t, script, `export ELASTICCLAW_ONBOARD_FLAGS='--auth-choice anthropic-api-key --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}"'`, "onboard flags shell quoted")
	assertNotContains(t, script, `export ELASTICCLAW_ONBOARD_FLAGS="--auth-choice anthropic-api-key --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}""`, "onboard flags must not use nested double quotes")
}

func TestBootstrapScript_GatewayPasswordInBridgeEnv(t *testing.T) {
	// Gateway password must appear at top (so it's in env for claw-bridge) and in
	// the persist block (so bridge can restart with it).
	p := baseParams()
	p.GatewayPassword = "super-secret-pw"
	script := GenerateReplicatedBootstrapScript(p)
	// Must appear at top as literal value
	assertContains(t, script, `ELASTICCLAW_GATEWAY_PASSWORD='super-secret-pw'`, "gateway password set at top")
	assertContains(t, script, `export OPENCLAW_GATEWAY_PASSWORD="$ELASTICCLAW_GATEWAY_PASSWORD"`, "openclaw gateway password env set")
	// Must appear in persist block (as variable reference, not literal)
	assertContains(t, script, `printf 'export ELASTICCLAW_GATEWAY_PASSWORD=%q\n' "$ELASTICCLAW_GATEWAY_PASSWORD"`, "gateway password in persist block")
	assertContains(t, script, `printf 'export OPENCLAW_GATEWAY_PASSWORD=%q\n' "$OPENCLAW_GATEWAY_PASSWORD"`, "openclaw gateway password in persist block")
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
bash -c 'printf "%s\n" "$ELASTICCLAW_CLAW_NAME" "$ELASTICCLAW_GATEWAY_PASSWORD"'
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
	want := []string{name, password}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sourced values mismatch: got %q, want %q", got, want)
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
		{
			name:       "codex",
			provider:   "codex",
			envVar:     "CODEX_API_KEY",
			authChoice: "codex-api-key",
			flagName:   "--codex-api-key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := []*types.LLMKeyConfig{
				{Name: tc.name + "-key", Provider: tc.provider, Default: true},
			}
			flags := buildOnboardFlags(keys, "", "")
			assertContains(t, flags, "--auth-choice "+tc.authChoice, "provider auth choice")
			assertContains(t, flags, tc.flagName+` "${`+tc.envVar+`:-}"`, "provider api key flag")
			assertNotContains(t, flags, "anthropic-api-key", "should not fallback to anthropic")
		})
	}
}

func TestBuildOnboardFlags_OllamaUsesNativeBaseURLAndModelID(t *testing.T) {
	keys := []*types.LLMKeyConfig{
		{Name: "ollama-main", Provider: "ollama", Default: true},
	}

	flags := buildOnboardFlags(keys, "", "ollama/qwen2.5-coder:7b")

	assertContains(t, flags, "--auth-choice ollama", "ollama auth choice")
	assertContains(t, flags, `--custom-base-url "http://ollama:11434"`, "ollama native base URL")
	assertContains(t, flags, `--custom-model-id 'qwen2.5-coder:7b'`, "ollama model id without provider prefix")
	assertNotContains(t, flags, "/v1", "OpenClaw Ollama base URL must use native API, not OpenAI-compatible path")
}

func TestBuildOnboardFlags_OllamaPrefersKeyDefaultModel(t *testing.T) {
	keys := []*types.LLMKeyConfig{
		{Name: "ollama-main", Provider: "ollama", Default: true, DefaultModel: "ollama/llama3.2:3b"},
	}

	flags := buildOnboardFlags(keys, "", "ollama/qwen2.5-coder:7b")

	assertContains(t, flags, `--custom-model-id 'llama3.2:3b'`, "ollama key default model")
}

func TestBuildOpenClawProviderConfig_DoesNotWriteLegacyProviderCatalog(t *testing.T) {
	keys := []*types.LLMKeyConfig{
		{Name: "openai-main", Provider: "openai", Default: true},
		{Name: "fireworks-main", Provider: "fireworks"},
		{Name: "groq-main", Provider: "groq"},
		{Name: "deepseek-main", Provider: "deepseek"},
		{Name: "codex-main", Provider: "codex"},
	}

	snippet := buildOpenClawProviderConfig(keys, "openai-main")

	assertContains(t, snippet, "agent_defaults['model'] = model", "still sets default model")
	assertContains(t, snippet, "config.pop('models', None)", "removes legacy top-level models catalog")
	assertNotContains(t, snippet, "providers.update", "does not write legacy provider catalog")
	assertNotContains(t, snippet, "'fireworks': {", "does not write fireworks provider entry")
	assertNotContains(t, snippet, "'openai': {", "does not write openai provider entry")
}

func TestBuildOpenClawProviderConfig_ConfiguresOllamaProviderBaseURL(t *testing.T) {
	keys := []*types.LLMKeyConfig{
		{Name: "ollama-main", Provider: "ollama", Default: true},
	}

	snippet := buildOpenClawProviderConfig(keys, "ollama-main")

	assertContains(t, snippet, "if model.startswith('ollama/'):", "only configures Ollama when selected model is Ollama")
	assertContains(t, snippet, "agent_defaults.setdefault('experimental', {})['localModelLean'] = True", "uses lean mode for weak local dev models")
	assertContains(t, snippet, "'baseUrl': 'http://ollama:11434'", "uses Compose Ollama service URL")
	assertContains(t, snippet, "'apiKey': 'OLLAMA_API_KEY'", "keeps Ollama API key env reference")
	assertContains(t, snippet, "'timeoutSeconds': 86400", "removes interactive timeout pressure for slow local Ollama")
	assertContains(t, snippet, "'contextWindow': 32768", "keeps OpenClaw local model prompt budget within Docker Ollama limits")
	assertContains(t, snippet, "'maxTokens': 1024", "keeps local dev generation bounded")
	assertContains(t, snippet, "'params': {'num_ctx': 32768, 'thinking': False, 'keep_alive': '15m'}", "sets native Ollama runtime context explicitly")
	assertContains(t, snippet, "'compat': {'supportsTools': True, 'supportsUsageInStreaming': True}", "keeps tool support while lean mode reduces local model prompt pressure")
	assertContains(t, snippet, "providers['ollama']", "writes only the built-in Ollama provider config")
	assertNotContains(t, snippet, "'ollama-cloud'", "does not rewrite Ollama Cloud")
}

func TestBuildOpenClawProviderConfig_DoesNotOverrideAnthropicModels(t *testing.T) {
	keys := []*types.LLMKeyConfig{
		{Name: "anthropic-main", Provider: "anthropic", Default: true},
	}

	snippet := buildOpenClawProviderConfig(keys, "anthropic-main")

	assertContains(t, snippet, "agent_defaults['model'] = model", "still sets default model")
	assertContains(t, snippet, "anthropic_key = os.environ.get('ANTHROPIC_API_KEY', '')", "reads Anthropic key env var")
	assertContains(t, snippet, "auth_path = os.path.expanduser('~/.openclaw/agents/main/agent/auth-profiles.json')", "writes Anthropic agent auth profile")
	assertContains(t, snippet, "profiles['anthropic:default']", "adds Anthropic default auth profile")
	assertContains(t, snippet, "config['gateway']['remote'] = {'password': gw_password}", "sets gateway remote password for local clients")
	assertNotContains(t, snippet, "'anthropic': {", "does not replace OpenClaw's bundled Anthropic provider config")
	assertNotContains(t, snippet, "config['models'] =", "does not replace the models section")
	assertNotContains(t, snippet, "providers.update", "does not add an empty providers patch for Anthropic-only config")
}

func TestBuildOpenClawProviderConfig_NoKeysStillPatchesDefaultModel(t *testing.T) {
	snippet := buildOpenClawProviderConfig(nil, "")

	assertContains(t, snippet, "agent_defaults['model'] = model", "still sets default model")
	assertContains(t, snippet, "config['gateway']['remote'] = {'password': gw_password}", "sets gateway remote password")
	assertNotContains(t, snippet, "providers.update", "does not add provider config without keys")
}

func TestBuildOpenClawProviderConfig_ConfiguresAnthropicWithoutProviderCatalog(t *testing.T) {
	keys := []*types.LLMKeyConfig{
		{Name: "anthropic-main", Provider: "anthropic", Default: true},
		{Name: "groq-main", Provider: "groq"},
	}

	snippet := buildOpenClawProviderConfig(keys, "anthropic-main")

	assertContains(t, snippet, "profiles['anthropic:default']", "configures Anthropic auth")
	assertNotContains(t, snippet, "providers.update({", "does not write legacy provider config")
	assertNotContains(t, snippet, "'groq': {", "does not add custom provider")
	assertNotContains(t, snippet, "'anthropic': {", "does not add Anthropic custom provider")
}

func TestBuildOpenClawProviderConfig_RemovesInvalidModelsCatalogAndStaleK2P5Alias(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not in PATH")
	}

	keys := []*types.LLMKeyConfig{
		{Name: "fireworks-main", Provider: "fireworks", Default: true},
	}

	snippet := buildOpenClawProviderConfig(keys, "custom-main")

	home := t.TempDir()
	configDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	configPath := filepath.Join(configDir, "openclaw.json")
	initialConfig := map[string]interface{}{
		"agents": map[string]interface{}{
			"defaults": map[string]interface{}{
				"models": map[string]interface{}{
					"fireworks/accounts/fireworks/routers/kimi-k2p5-turbo": map[string]interface{}{"alias": "Kimi K2.5 Turbo"},
					"fireworks/accounts/fireworks/models/kimi-k2p6":        map[string]interface{}{"alias": "Kimi K2.6"},
				},
			},
		},
		"models": map[string]interface{}{
			"mode": "merge",
			"providers": map[string]interface{}{
				"fireworks": map[string]interface{}{"apiKey": "fw-test"},
			},
			"routers": map[string]interface{}{
				"fireworks": map[string]interface{}{"stale": true},
			},
		},
	}
	data, _ := json.Marshal(initialConfig)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cmd := exec.Command("bash", "-c", snippet)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"OPENCLAW_DEFAULT_MODEL=fireworks/accounts/fireworks/models/kimi-k2p6",
		keys[0].EnvVarName()+"=fw-test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run config snippet: %v\n%s", err, string(out))
	}

	var patched map[string]interface{}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read patched config: %v", err)
	}
	if err := json.Unmarshal(configData, &patched); err != nil {
		t.Fatalf("parse patched config: %v", err)
	}
	if _, ok := patched["models"]; ok {
		t.Fatalf("legacy top-level models catalog was not removed: %#v", patched["models"])
	}
	agents, ok := patched["agents"].(map[string]interface{})
	if !ok {
		t.Fatalf("agents missing or wrong type: %#v", patched["agents"])
	}
	defaults, ok := agents["defaults"].(map[string]interface{})
	if !ok {
		t.Fatalf("agents.defaults missing or wrong type: %#v", agents["defaults"])
	}
	if defaults["model"] != "fireworks/accounts/fireworks/models/kimi-k2p6" {
		t.Fatalf("default model not patched: %#v", defaults["model"])
	}
	agentModels, ok := defaults["models"].(map[string]interface{})
	if !ok {
		t.Fatalf("agents.defaults.models missing or wrong type: %#v", defaults["models"])
	}
	if _, ok := agentModels["fireworks/accounts/fireworks/routers/kimi-k2p5-turbo"]; ok {
		t.Fatalf("stale k2p5 alias was not removed: %#v", agentModels)
	}
	if _, ok := agentModels["fireworks/accounts/fireworks/models/kimi-k2p6"]; !ok {
		t.Fatalf("non-stale k2p6 alias was removed: %#v", agentModels)
	}
}

func TestBuildOpenClawProviderConfig_WritesAnthropicAuthProfileAndRemovesLegacyProviderCatalog(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not in PATH")
	}

	home := t.TempDir()
	configDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(configDir, 0700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	configPath := filepath.Join(configDir, "openclaw.json")
	initialConfig := map[string]interface{}{
		"models": map[string]interface{}{
			"providers": map[string]interface{}{
				"anthropic": map[string]interface{}{
					"baseUrl": "https://api.anthropic.com",
					"models": []interface{}{
						map[string]interface{}{"id": "claude-sonnet-4-6", "maxTokens": float64(64000)},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(initialConfig)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	keys := []*types.LLMKeyConfig{{Name: "anthropic-main", Provider: "anthropic", Default: true}}
	snippet := buildOpenClawProviderConfig(keys, "anthropic-main")
	cmd := exec.Command("bash", "-c", snippet)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"ANTHROPIC_API_KEY=sk-ant-test",
		"ELASTICCLAW_GATEWAY_PASSWORD=test-gw-password",
		"OPENCLAW_DEFAULT_MODEL=anthropic/claude-sonnet-4-6",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run config snippet: %v\n%s", err, string(out))
	}

	var patched map[string]interface{}
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read patched config: %v", err)
	}
	if err := json.Unmarshal(configData, &patched); err != nil {
		t.Fatalf("parse patched config: %v", err)
	}
	if _, ok := patched["models"]; ok {
		t.Fatalf("legacy top-level models catalog was not removed: %#v", patched["models"])
	}
	gateway, ok := patched["gateway"].(map[string]interface{})
	if !ok {
		t.Fatalf("gateway missing or wrong type: %#v", patched["gateway"])
	}
	remote, ok := gateway["remote"].(map[string]interface{})
	if !ok {
		t.Fatalf("gateway.remote missing or wrong type: %#v", gateway["remote"])
	}
	if remote["password"] != "test-gw-password" {
		t.Fatalf("gateway remote password not set: %#v", remote)
	}

	authPath := filepath.Join(home, ".openclaw", "agents", "main", "agent", "auth-profiles.json")
	authData, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth profiles: %v", err)
	}
	var auth map[string]interface{}
	if err := json.Unmarshal(authData, &auth); err != nil {
		t.Fatalf("parse auth profiles: %v", err)
	}
	profiles, ok := auth["profiles"].(map[string]interface{})
	if !ok {
		t.Fatalf("auth profiles missing or wrong type: %#v", auth["profiles"])
	}
	profile, ok := profiles["anthropic:default"].(map[string]interface{})
	if !ok {
		t.Fatalf("anthropic default profile missing or wrong type: %#v", profiles["anthropic:default"])
	}
	if profile["key"] != "sk-ant-test" || profile["provider"] != "anthropic" {
		t.Fatalf("bad anthropic profile: %#v", profile)
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
		{"docker_enabled", func() BootstrapParams { p := baseParams(); p.Docker = true; return p }()},
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
		{"docker_enabled", func() BootstrapParams { p := baseParams(); p.Docker = true; return p }()},
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

func TestGitHubCredentialHelper_RequiresAndVerifiesGitRegistration(t *testing.T) {
	cfg := &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
		ClawToken:  "test-claw-token",
	}
	script := buildGitHubCredentialHelper(cfg, "https://hub.example.com", "claw-123", nil)

	assertContains(t, script, "set -euo pipefail", "strict shell mode")
	assertContains(t, script, `if [ -z "${HOME:-}" ]; then`, "HOME fallback")
	assertContains(t, script, "cannot configure git credential helper", "HOME validation error")
	assertContains(t, script, "Configuring GitHub credential helper for user=$(whoami) home=$HOME", "user and HOME log")
	assertContains(t, script, "sudo apt-get install -y git", "mandatory git install")
	assertContains(t, script, "git config --global credential.helper /usr/local/bin/elasticclaw-git-credentials", "git helper registration")
	assertContains(t, script, "git config --global --get-all credential.helper | grep -Fx /usr/local/bin/elasticclaw-git-credentials >/dev/null", "git helper verification")
	assertContains(t, script, "git config --show-origin --global --get-all credential.helper", "git helper origin log")
	assertNotContains(t, script, "sudo apt-get install -y git 2>/dev/null || true", "git install must not be silently optional")

	gitConfigIdx := strings.Index(script, "git config --global credential.helper /usr/local/bin/elasticclaw-git-credentials")
	ghInstallIdx := strings.Index(script, "Installing gh CLI")
	if gitConfigIdx == -1 || ghInstallIdx == -1 {
		t.Fatalf("expected git config and gh install markers in helper script")
	}
	if gitConfigIdx > ghInstallIdx {
		t.Fatalf("mandatory git credential helper registration must happen before optional gh install")
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
