package hub

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// BootstrapParams holds all inputs needed to generate a bootstrap script.
// It is intentionally a pure value type — no DB, no server, no side effects.
type BootstrapParams struct {
	// Claw identity
	ClawID    string
	ClawName  string
	ClawToken string

	// Hub connectivity
	HubURL string

	// OpenClaw config
	DefaultModel    string
	GatewayPassword string

	// claw-bridge binary source (HTTPS URL or OCI ref)
	BridgeURL string

	// Features
	Nix    bool
	Docker bool

	// TemplateFiles includes all template files (may contain flake.nix)
	TemplateFiles map[string]string

	// GitHub credential helper
	HubCfg      *types.HubConfig
	GitHubRepos []types.GitHubRepoAccess

	// Env injection
	LLMKeyEnv      string // pre-built export lines
	ModelAuthEnv   string // pre-built export lines for CLI-backed model auth state
	APIKeyAuthSync string // shell script that persists API-key auth into OpenClaw's auth store
	LinearEnv      string // pre-built export line
	ProviderConfig string // python snippet to patch OpenClaw config
	OnboardFlags   string // --auth-choice ... flags for openclaw onboard
}

// resolveActiveKey selects the active key by selected name, then default, then first.
func resolveActiveKey(keys []*types.LLMKeyConfig, selectedKeyName string) *types.LLMKeyConfig {
	for _, k := range keys {
		if k.Name == selectedKeyName && llmKeyHasRequiredAPIKey(k) {
			return k
		}
	}
	for _, k := range keys {
		if k.Default && llmKeyHasRequiredAPIKey(k) {
			return k
		}
	}
	if len(keys) > 0 {
		for _, k := range keys {
			if llmKeyHasRequiredAPIKey(k) {
				return k
			}
		}
	}
	return nil
}

// buildOpenClawProviderConfig returns a python snippet that patches
// ~/.openclaw/openclaw.json with the agent default, gateway settings, and any
// auth-profile compatibility writes needed after openclaw onboard.
// selectedKeyName is used to pick the active key (falls back to default, then first).
func buildOpenClawProviderConfig(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	// Determine active key
	activeKey := resolveActiveKey(keys, selectedKeyName)

	anthropicEnvVar := ""
	if activeKey != nil && activeKey.Provider == "anthropic" {
		anthropicEnvVar = activeKey.EnvVarName()
	} else {
		for _, k := range keys {
			if k.Provider == "anthropic" {
				anthropicEnvVar = k.EnvVarName()
				break
			}
		}
	}

	anthropicPatch := ""
	if anthropicEnvVar != "" {
		anthropicPatch = fmt.Sprintf(`anthropic_key = os.environ.get('%s', '')
if anthropic_key:
    auth_path = os.path.expanduser('~/.openclaw/agents/main/agent/auth-profiles.json')
    os.makedirs(os.path.dirname(auth_path), exist_ok=True)
    try:
        with open(auth_path) as f:
            auth = json.load(f)
    except (FileNotFoundError, json.JSONDecodeError):
        auth = {}
    profiles = auth.setdefault('profiles', {})
    order = auth.setdefault('order', {})
    profiles['anthropic:default'] = {
        'provider': 'anthropic',
        'mode': 'api_key',
        'type': 'api_key',
        'key': anthropic_key
    }
    anthropic_order = [p for p in order.get('anthropic', []) if p != 'anthropic:default']
    order['anthropic'] = ['anthropic:default'] + anthropic_order
    with open(auth_path, 'w') as f:
        json.dump(auth, f, indent=2)
`, anthropicEnvVar)
	}

	return fmt.Sprintf(`python3 << 'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
os.makedirs(os.path.dirname(path), exist_ok=True)
try:
    with open(path) as f:
        config = json.load(f)
except FileNotFoundError:
    config = {}
except Exception:
    config = {}
model = os.environ.get('OPENCLAW_DEFAULT_MODEL', 'anthropic/claude-sonnet-4-6')
agent_defaults = config.setdefault('agents', {}).setdefault('defaults', {})
agent_defaults['model'] = model
agent_models = agent_defaults.get('models')
if isinstance(agent_models, dict):
    for key in list(agent_models.keys()):
        if 'kimi-k2p5' in key:
            agent_models.pop(key, None)
# OpenClaw rejects the legacy top-level models provider catalog shape we used
# to write. Onboard handles provider auth; keep only agent default.
models = config.get('models')
if isinstance(models, dict) and any(k in models for k in ('providers', 'routers', 'mode')):
    config.pop('models', None)
if model.startswith('ollama/'):
    model_id = model.split('/', 1)[1]
    agent_defaults.setdefault('experimental', {})['localModelLean'] = True
    config.setdefault('models', {})['mode'] = 'merge'
    providers = config['models'].setdefault('providers', {})
    providers['ollama'] = {
        'baseUrl': 'http://ollama:11434',
        'api': 'ollama',
        'apiKey': 'OLLAMA_API_KEY',
        'models': [{
            'id': model_id,
            'name': model_id,
            'reasoning': False,
            'input': ['text'],
            'cost': {'input': 0, 'output': 0, 'cacheRead': 0, 'cacheWrite': 0},
            'contextWindow': 32768,
            'maxTokens': 1024,
            'params': {'num_ctx': 32768, 'thinking': False, 'keep_alive': '15m'},
            'compat': {'supportsTools': True, 'supportsUsageInStreaming': True},
        }],
    }
if model.startswith('grok/'):
    model_id = model.split('/', 1)[1]
    config.setdefault('models', {})['mode'] = 'merge'
    providers = config['models'].setdefault('providers', {})
    providers['grok'] = {
        'baseUrl': 'https://api.x.ai/v1',
        'api': 'openai',
        'apiKey': 'XAI_API_KEY',
        'models': [{
            'id': model_id,
            'name': model_id,
            'reasoning': True,
            'input': ['text', 'image'],
            'cost': {'input': 0, 'output': 0, 'cacheRead': 0, 'cacheWrite': 0},
            'contextWindow': 256000,
            'maxTokens': 8192,
            'compat': {'supportsTools': True, 'supportsUsageInStreaming': True},
        }],
    }
%sconfig.setdefault('gateway', {})['bind'] = 'loopback'
config['gateway']['port'] = 18789
gw_password = os.environ.get('ELASTICCLAW_GATEWAY_PASSWORD', '')
if gw_password:
    config['gateway']['auth'] = {'mode': 'password', 'password': gw_password}
    config['gateway']['remote'] = {'password': gw_password}
with open(path, 'w') as f:
    json.dump(config, f, indent=2)
print('OpenClaw config patched')
PYEOF`, anthropicPatch)
}

// buildOpenClawAPIKeyAuthSyncShell returns a shell snippet that persists
// direct API-key auth into OpenClaw's current auth store. OpenClaw 2026.7.1-2
// resolves agent auth from openclaw-agent.sqlite, so writing only the legacy
// auth-profiles.json file is not enough for embedded agents.
func buildOpenClawAPIKeyAuthSyncShell(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	activeKey := resolveActiveKey(keys, selectedKeyName)
	if activeKey == nil || activeKey.Provider != "anthropic" || !llmKeyHasRequiredAPIKey(activeKey) {
		return ""
	}
	envVar := activeKey.EnvVarName()
	return fmt.Sprintf(`if [ -n "${%s:-}" ]; then
  printf '%%s\n' "${%s}" | openclaw models auth paste-api-key --provider anthropic --profile-id anthropic:default
fi`, envVar, envVar)
}

// GenerateReplicatedBootstrapScript returns a minimal bash script that downloads
// claw-bridge and execs it with --bootstrap. All VM setup logic now lives inside
// claw-bridge itself (runBootstrap in cmd/claw-bridge/main.go).
//
// This is a pure function — same inputs always produce the same output.
// All I/O (DB reads, SSH, etc.) happens in bootstrapReplicated before calling this.
func GenerateReplicatedBootstrapScript(p BootstrapParams) string {
	nixFlag := "false"
	if p.Nix {
		nixFlag = "true"
	}
	dockerFlag := "false"
	if p.Docker {
		dockerFlag = "true"
	}
	// Encode the provider config python snippet as a single env var value so
	// claw-bridge can receive it without heredoc escaping issues.
	// We use a simple approach: if it's non-empty, pass it as ELASTICCLAW_PROVIDER_CONFIG.
	// The value may contain newlines; bash's export handles that fine.
	providerConfigLine := "# No provider config"
	if p.ProviderConfig != "" {
		// Escape for shell: use printf %q approach via parameter expansion in the
		// script. Simpler: write it as a heredoc into a temp file the claw-bridge
		// reads. But easiest: base64-encode it so there are no quoting issues.
		providerConfigLine = fmt.Sprintf("export ELASTICCLAW_PROVIDER_CONFIG=%s",
			shellQuote(p.ProviderConfig))
	}
	apiKeyAuthSyncLine := "# No API key auth sync"
	if p.APIKeyAuthSync != "" {
		apiKeyAuthSyncLine = fmt.Sprintf("export ELASTICCLAW_API_KEY_AUTH_SYNC=%s",
			shellQuote(p.APIKeyAuthSync))
	}

	linearEnvLine := p.LinearEnv
	if linearEnvLine == "" {
		linearEnvLine = "# Linear not configured"
	}

	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

# ── Identity + credentials ────────────────────────────────────────────────────
export ELASTICCLAW_HUB_URL=%s
export ELASTICCLAW_CLAW_ID=%s
export ELASTICCLAW_CLAW_TOKEN=%s
export ELASTICCLAW_CLAW_NAME=%s
export ELASTICCLAW_GATEWAY_PASSWORD=%s
export OPENCLAW_GATEWAY_PASSWORD="$ELASTICCLAW_GATEWAY_PASSWORD"
export OPENCLAW_DEFAULT_MODEL=%s
export ELASTICCLAW_NIX=%s
export ELASTICCLAW_DOCKER=%s
%s
%s
%s
%s
export ELASTICCLAW_ONBOARD_FLAGS=%s
%s
# ── Install claw-bridge ───────────────────────────────────────────────────────
BRIDGE_SRC=%s
download_connector_once() {
  rm -f /tmp/claw-bridge
  if echo "$BRIDGE_SRC" | grep -qE '^https?://'; then
    curl -fsSL "$BRIDGE_SRC" -o /tmp/claw-bridge
  else
    # OCI ref — use oras
    if ! command -v oras &>/dev/null; then
      echo "Installing oras..."
      curl -sL https://github.com/oras-project/oras/releases/download/v1.2.2/oras_1.2.2_linux_amd64.tar.gz | tar xz -C /tmp
      sudo mv /tmp/oras /usr/local/bin/oras
    fi
    sudo apt-get install -y curl ca-certificates 2>/dev/null || true
    rm -rf /tmp/claw-bridge-dl
    mkdir -p /tmp/claw-bridge-dl && cd /tmp/claw-bridge-dl
    oras pull "$BRIDGE_SRC"
    BINARY=$(find /tmp/claw-bridge-dl -name 'claw-bridge*' -type f | head -1)
    if [ -z "$BINARY" ]; then
      echo "ERROR: ElasticClaw connector binary not found after oras pull"
      ls -la /tmp/claw-bridge-dl/
      return 1
    fi
    cp "$BINARY" /tmp/claw-bridge
    cd -
  fi
}

CONNECTOR_DELAYS=(5 10 20 40 60)
CONNECTOR_ATTEMPTS=6
for attempt in $(seq 1 "$CONNECTOR_ATTEMPTS"); do
  echo "Downloading ElasticClaw connector (attempt $attempt/$CONNECTOR_ATTEMPTS)..."
  if download_connector_once; then
    break
  fi
  if [ "$attempt" -eq "$CONNECTOR_ATTEMPTS" ]; then
    echo "ERROR: could not download ElasticClaw connector after $CONNECTOR_ATTEMPTS attempts"
    exit 1
  fi
  delay="${CONNECTOR_DELAYS[$((attempt-1))]}"
  echo "Retrying connector download in ${delay}s..."
  sleep "$delay"
done
chmod +x /tmp/claw-bridge
sudo mv /tmp/claw-bridge /usr/local/bin/claw-bridge
echo "ElasticClaw connector installed"

# ── Bootstrap + run ───────────────────────────────────────────────────────────
# claw-bridge --bootstrap installs Node.js, OpenClaw, configures the gateway,
# then transitions directly into the normal bridge connect loop.
# Persist env vars so bridge can be restarted later.
{
  printf 'export ELASTICCLAW_HUB_URL=%%q\n' "$ELASTICCLAW_HUB_URL"
  printf 'export ELASTICCLAW_CLAW_ID=%%q\n' "$ELASTICCLAW_CLAW_ID"
  printf 'export ELASTICCLAW_CLAW_TOKEN=%%q\n' "$ELASTICCLAW_CLAW_TOKEN"
  printf 'export ELASTICCLAW_CLAW_NAME=%%q\n' "$ELASTICCLAW_CLAW_NAME"
  printf 'export ELASTICCLAW_GATEWAY_PASSWORD=%%q\n' "$ELASTICCLAW_GATEWAY_PASSWORD"
  printf 'export OPENCLAW_GATEWAY_PASSWORD=%%q\n' "$OPENCLAW_GATEWAY_PASSWORD"
} > "$HOME/.claw-bridge.env"
chmod 600 "$HOME/.claw-bridge.env"

# Run claw-bridge in bootstrap mode in the background, then wait until the
# bootstrap phase completes so the SSH session can exit and the hub can write
# template files.
export ELASTICCLAW_BOOTSTRAP=1
export ELASTICCLAW_BOOTSTRAP_NOTIFY_FILE="$HOME/.claw-bridge.bootstrap.ready"
rm -f "$ELASTICCLAW_BOOTSTRAP_NOTIFY_FILE"
cat > "$HOME/.claw-bridge-supervisor.sh" <<'EOF'
#!/bin/bash
. "$HOME/.claw-bridge.env"
export ELASTICCLAW_BOOTSTRAP ELASTICCLAW_BOOTSTRAP_NOTIFY_FILE
restarts=0
total_restarts=0
backoff=5
child=""
trap 'if [ -n "$child" ]; then kill "$child" 2>/dev/null; wait "$child" 2>/dev/null; fi; exit 0' TERM INT
while :; do
  started_at=$(date +%%s)
  export ELASTICCLAW_BRIDGE_RESTARTS="$total_restarts"
  /usr/local/bin/claw-bridge >> "$HOME/claw-bridge.log" 2>&1 &
  child=$!
  wait "$child"
  rc=$?
  child=""
  if [ "$rc" -eq 0 ]; then
    echo "[supervisor] claw-bridge exited cleanly"
    exit 0
  fi
  now=$(date +%%s)
  if [ $((now - started_at)) -ge 300 ]; then
    restarts=0
    backoff=5
  fi
  if [ "$restarts" -ge 3 ]; then
    echo "[supervisor] claw-bridge restart budget exhausted after 3 attempts"
    exit 1
  fi
  restarts=$((restarts + 1))
  total_restarts=$((total_restarts + 1))
  echo "[supervisor] claw-bridge exited (code=$rc); restarting (attempt $restarts/3) in ${backoff}s"
  unset ELASTICCLAW_BOOTSTRAP ELASTICCLAW_BOOTSTRAP_NOTIFY_FILE
  sleep "$backoff"
  backoff=$((backoff * 2))
done
EOF
chmod 700 "$HOME/.claw-bridge-supervisor.sh"
nohup "$HOME/.claw-bridge-supervisor.sh" >> "$HOME/claw-bridge.log" 2>&1 </dev/null &
BRIDGE_PID=$!
for _ in {1..1800}; do
  if [ -f "$ELASTICCLAW_BOOTSTRAP_NOTIFY_FILE" ]; then
    echo "claw-bridge bootstrap complete; bridge running in background"
    exit 0
  fi
  if ! kill -0 "$BRIDGE_PID" 2>/dev/null; then
    wait "$BRIDGE_PID"
    exit $?
  fi
  sleep 1
done
echo "ERROR: timed out waiting for claw-bridge bootstrap to complete"
exit 1
`,
		shellQuote(p.HubURL), shellQuote(p.ClawID), shellQuote(p.ClawToken), shellQuote(p.ClawName), shellQuote(p.GatewayPassword),
		shellQuote(p.DefaultModel), shellQuote(nixFlag), shellQuote(dockerFlag),
		p.LLMKeyEnv, p.ModelAuthEnv, linearEnvLine, apiKeyAuthSyncLine, shellQuote(p.OnboardFlags), providerConfigLine,
		shellQuote(p.BridgeURL),
	)
}

// shellQuote returns a single-quoted shell string safe for embedding in scripts.
// Single quotes cannot appear inside single-quoted strings in bash, so we
// replace them with '"'"'.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// buildOnboardFlags returns the --auth-choice flags for openclaw onboard based
// on the active LLM key (selected > default > first).
func buildOnboardFlags(keys []*types.LLMKeyConfig, selectedKeyName, defaultModel string) string {
	active := resolveActiveKey(keys, selectedKeyName)
	if active == nil {
		return `--auth-choice anthropic-api-key --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}"`
	}
	envVar := active.EnvVarName()
	switch active.Provider {
	case "anthropic":
		return fmt.Sprintf(`--auth-choice anthropic-api-key --anthropic-api-key "${%s:-placeholder}"`, envVar)
	case "fireworks":
		return fmt.Sprintf(`--auth-choice fireworks-api-key --fireworks-api-key "${%s:-}"`, envVar)
	case "openai":
		return fmt.Sprintf(`--auth-choice openai-api-key --openai-api-key "${%s:-}"`, envVar)
	case "groq":
		return fmt.Sprintf(`--auth-choice groq-api-key --groq-api-key "${%s:-}"`, envVar)
	case "deepseek":
		return fmt.Sprintf(`--auth-choice deepseek-api-key --deepseek-api-key "${%s:-}"`, envVar)
	case "codex":
		return fmt.Sprintf(`--auth-choice codex-api-key --codex-api-key "${%s:-}"`, envVar)
	case "grok":
		return fmt.Sprintf(`--auth-choice openai-api-key --openai-api-key "${%s:-}"`, envVar)
	case "ollama":
		model := active.DefaultModel
		if model == "" || !strings.HasPrefix(model, active.Provider+"/") {
			model = defaultModel
		}
		if model == "" || !strings.HasPrefix(model, active.Provider+"/") {
			model = "ollama/qwen2.5-coder:1.5b"
		}
		return fmt.Sprintf(`--auth-choice ollama --custom-base-url "http://ollama:11434" --custom-model-id %s`, shellQuote(stripProviderPrefix(model)))
	default:
		return `--auth-choice anthropic-api-key --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}"`
	}
}
