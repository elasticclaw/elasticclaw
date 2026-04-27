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
	Nix bool

	// GitHub credential helper
	HubCfg      *types.HubConfig
	GitHubRepos []types.GitHubRepoAccess

	// Env injection
	LLMKeyEnv      string // pre-built export lines
	LinearEnv      string // pre-built export line
	RelayEnv       string // pre-built export lines
	ProviderConfig string // python snippet to configure models.providers
	OnboardFlags   string // --auth-choice ... flags for openclaw onboard
}

// resolveActiveKey selects the active key by selected name, then default, then first.
func resolveActiveKey(keys []*types.LLMKeyConfig, selectedKeyName string) *types.LLMKeyConfig {
	for _, k := range keys {
		if k.Name == selectedKeyName {
			return k
		}
	}
	for _, k := range keys {
		if k.Default {
			return k
		}
	}
	if len(keys) > 0 {
		return keys[0]
	}
	return nil
}

// buildOpenClawProviderConfig returns a python snippet that writes the correct
// models.providers config to ~/.openclaw/openclaw.json based on configured LLM keys.
// selectedKeyName is used to pick the active key (falls back to default, then first).
func buildOpenClawProviderConfig(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	if len(keys) == 0 {
		// No keys configured — produce empty snippet
		return ""
	}

	// Determine active key
	activeKey := resolveActiveKey(keys, selectedKeyName)
	_ = activeKey // used implicitly via the provider list

	// Build per-provider config entries.
	// We emit an entry for every unique provider across all configured keys.
	// If a provider appears multiple times we use the active key's entry if it matches,
	// otherwise the first occurrence.
	seen := map[string]bool{}
	var providerLines []string

	// Helper: build a single provider dict as a python literal.
	buildEntry := func(k *types.LLMKeyConfig) string {
		envVar := k.EnvVarName()
		switch k.Provider {
		case "anthropic":
			return fmt.Sprintf(`'anthropic': {
            'apiKey': os.environ.get('%s', ''),
            'baseUrl': 'https://api.anthropic.com',
            'api': 'anthropic-messages',
            'models': [
                {'id': 'claude-sonnet-4-6', 'name': 'Claude Sonnet 4.6', 'api': 'anthropic-messages'},
                {'id': 'claude-opus-4-5',   'name': 'Claude Opus 4.5',   'api': 'anthropic-messages'},
                {'id': 'claude-sonnet-4-5', 'name': 'Claude Sonnet 4.5', 'api': 'anthropic-messages'}
            ]
        }`, envVar)
		case "fireworks":
			return fmt.Sprintf(`'fireworks': {
            'apiKey': os.environ.get('%s', ''),
            'baseUrl': 'https://api.fireworks.ai/inference/v1',
            'api': 'openai-completions',
            'models': [
                {'id': 'accounts/fireworks/models/kimi-k2p6',                  'name': 'Kimi K2'},
                {'id': 'accounts/fireworks/models/llama-v3p3-70b-instruct',    'name': 'Llama 3.3 70B'},
                {'id': 'accounts/fireworks/models/deepseek-v3',                'name': 'DeepSeek V3'}
            ]
        }`, envVar)
		case "openai":
			return fmt.Sprintf(`'openai': {
            'apiKey': os.environ.get('%s', ''),
            'baseUrl': 'https://api.openai.com/v1',
            'api': 'openai-completions',
            'models': [
                {'id': 'gpt-4o',      'name': 'GPT-4o'},
                {'id': 'gpt-4o-mini', 'name': 'GPT-4o Mini'}
            ]
        }`, envVar)
		case "groq":
			return fmt.Sprintf(`'groq': {
            'apiKey': os.environ.get('%s', ''),
            'baseUrl': 'https://api.groq.com/openai/v1',
            'api': 'openai-completions',
            'models': [
                {'id': 'llama-3.3-70b-versatile', 'name': 'Llama 3.3 70B'}
            ]
        }`, envVar)
		case "deepseek":
			return fmt.Sprintf(`'deepseek': {
            'apiKey': os.environ.get('%s', ''),
            'baseUrl': 'https://api.deepseek.com/v1',
            'api': 'openai-completions',
            'models': [
                {'id': 'deepseek-chat', 'name': 'DeepSeek Chat'}
            ]
        }`, envVar)
		default:
			return fmt.Sprintf(`'%s': {
            'apiKey': os.environ.get('%s', ''),
            'api': 'openai-completions'
        }`, k.Provider, envVar)
		}
	}

	// Prioritize the active key's provider first
	entry := buildEntry(activeKey)
	seen[activeKey.Provider] = true
	providerLines = append(providerLines, entry)

	// Then remaining keys
	for _, k := range keys {
		if seen[k.Provider] {
			continue
		}
		seen[k.Provider] = true
		providerLines = append(providerLines, buildEntry(k))
	}

	providersDict := strings.Join(providerLines, ",\n        ")

	return fmt.Sprintf(`python3 << 'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
try:
    with open(path) as f:
        config = json.load(f)
except:
    config = {}
model = os.environ.get('OPENCLAW_DEFAULT_MODEL', 'anthropic/claude-sonnet-4-6')
config.setdefault('agents', {}).setdefault('defaults', {})['model'] = model
config['models'] = {
    'providers': {
        %s
    }
}
config.setdefault('gateway', {})['bind'] = 'loopback'
config['gateway']['port'] = 18789
gw_password = os.environ.get('ELASTICCLAW_GATEWAY_PASSWORD', '')
if gw_password:
    config['gateway']['auth'] = {'mode': 'password', 'password': gw_password}
with open(path, 'w') as f:
    json.dump(config, f, indent=2)
print('OpenClaw config patched')
PYEOF`, providersDict)
}

// GenerateReplicatedBootstrapScript returns the bash script that bootstraps
// a Replicated VM into a functioning elasticclaw claw.
//
// This is a pure function — same inputs always produce the same output.
// All I/O (DB reads, SSH, etc.) happens in bootstrapReplicated before calling this.
func GenerateReplicatedBootstrapScript(p BootstrapParams) string {
	credHelper := buildGitHubCredentialHelper(p.HubCfg, p.HubURL, p.ClawID, p.GitHubRepos)

	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

# ── LLM API keys + service tokens ────────────────────────────────────────────
export OPENCLAW_DEFAULT_MODEL="%s"
export ELASTICCLAW_GATEWAY_PASSWORD="%s"
%s
%s
# ── Install Node.js 24 via nodesource ────────────────────────────────────────
echo "Installing Node.js 24..."
sudo apt-get update -qq
sudo apt-get install -y curl ca-certificates
sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | \
  sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/nodesource.gpg
echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main" | sudo tee /etc/apt/sources.list.d/nodesource.list > /dev/null
sudo apt-get update -qq
sudo apt-get install -y nodejs git
echo "Node: $(node --version)"

%s
# ── Install OpenClaw ──────────────────────────────────────────────────────────
echo "Installing OpenClaw..."
sudo npm install -g openclaw@latest --ignore-scripts
echo "OpenClaw: $(openclaw --version)"

# ── Configure OpenClaw ────────────────────────────────────────────────────────
mkdir -p "$HOME/.openclaw/workspace"
if [ ! -f "$HOME/.openclaw/openclaw.json" ]; then
  echo "Configuring OpenClaw..."
  openclaw onboard \
    --non-interactive --accept-risk \
    --gateway-bind loopback --gateway-port 18789 \
    --skip-daemon %s 2>/dev/null || true
  %s
fi
# ── Start OpenClaw gateway ────────────────────────────────────────────────────
# Disable Bonjour/mDNS — not supported on Replicated VMs (multicast blocked),
# causes gateway crash. OPENCLAW_DISABLE_BONJOUR=1 is the official env var.
export OPENCLAW_NO_RESPAWN=1
export OPENCLAW_DISABLE_BONJOUR=1
echo "Starting OpenClaw gateway..."
nohup openclaw gateway run >> "$HOME/openclaw-gateway.log" 2>&1 &
for i in $(seq 1 30); do
  sleep 1
  if curl -sf http://localhost:18789/healthz &>/dev/null; then
    echo "OpenClaw gateway ready after ${i}s"
    break
  fi
  if [ "$i" = "30" ]; then
    echo "WARNING: gateway did not respond in 30s"
    tail -10 "$HOME/openclaw-gateway.log" 2>/dev/null || true
  fi
done

# ── Install claw-bridge ───────────────────────────────────────────────────────
BRIDGE_SRC="%s"
echo "Installing claw-bridge from $BRIDGE_SRC..."
if echo "$BRIDGE_SRC" | grep -qE '^https?://'; then
  curl -fsSL "$BRIDGE_SRC" -o /tmp/claw-bridge
else
  # OCI ref — use oras
  if ! command -v oras &>/dev/null; then
    echo "Installing oras..."
    curl -sL https://github.com/oras-project/oras/releases/download/v1.2.2/oras_1.2.2_linux_amd64.tar.gz | tar xz -C /tmp
    sudo mv /tmp/oras /usr/local/bin/oras
  fi
  mkdir -p /tmp/claw-bridge-dl && cd /tmp/claw-bridge-dl
  oras pull "$BRIDGE_SRC"
  BINARY=$(find /tmp/claw-bridge-dl -name 'claw-bridge*' -type f | head -1)
  if [ -z "$BINARY" ]; then
    echo "ERROR: claw-bridge binary not found after oras pull"
    ls -la /tmp/claw-bridge-dl/
    exit 1
  fi
  cp "$BINARY" /tmp/claw-bridge
  cd -
fi
chmod +x /tmp/claw-bridge
sudo mv /tmp/claw-bridge /usr/local/bin/claw-bridge
echo "claw-bridge installed"

# ── Start claw-bridge ─────────────────────────────────────────────────────────
export ELASTICCLAW_HUB_URL="%s"
export ELASTICCLAW_CLAW_ID="%s"
export ELASTICCLAW_CLAW_TOKEN="%s"
export ELASTICCLAW_CLAW_NAME="%s"
export ELASTICCLAW_GATEWAY_PASSWORD="%s"
%s
export OPENCLAW_DEFAULT_MODEL="%s"
%s
echo "Starting claw-bridge (HUB_URL=$ELASTICCLAW_HUB_URL)..."
nohup /usr/local/bin/claw-bridge >> "$HOME/claw-bridge.log" 2>&1 &
BRIDGE_PID=$!
echo "claw-bridge started (PID $BRIDGE_PID)"
for i in $(seq 1 10); do
  sleep 1
  if ! kill -0 $BRIDGE_PID 2>/dev/null; then
    echo "ERROR: claw-bridge died after ${i}s"
    echo "=== claw-bridge.log ==="
    cat "$HOME/claw-bridge.log" 2>/dev/null || echo "(no log)"
    exit 1
  fi
  if grep -q 'connected\|registered\|ready' "$HOME/claw-bridge.log" 2>/dev/null; then
    echo "claw-bridge connected after ${i}s"
    break
  fi
done
if kill -0 $BRIDGE_PID 2>/dev/null; then
  echo "claw-bridge is running (PID $BRIDGE_PID)"
  tail -10 "$HOME/claw-bridge.log" 2>/dev/null || echo "(no log yet)"
else
  echo "ERROR: claw-bridge died"
  cat "$HOME/claw-bridge.log" 2>/dev/null
  exit 1
fi

# ── GitHub credential helper + repo clone ────────────────────────────────────
# Bridge is running — hub API is reachable via proxy now
%s
`,
		p.DefaultModel, p.GatewayPassword, p.LLMKeyEnv, p.LinearEnv,
		buildNixInstall(p.Nix),
		p.OnboardFlags,
		p.ProviderConfig,
		p.BridgeURL,
		p.HubURL, p.ClawID, p.ClawToken, p.ClawName, p.GatewayPassword,
		p.RelayEnv, p.DefaultModel, p.LLMKeyEnv,
		credHelper,
	)
}

// buildOnboardFlags returns the --auth-choice flags for openclaw onboard
// based on the active LLM key (selected > default > first).
func buildOnboardFlags(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	active := resolveActiveKey(keys, selectedKeyName)
	if active == nil {
		return `--auth-choice anthropic-api-key --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}"`
	}
	envVar := active.EnvVarName()
	switch active.Provider {
	case "anthropic":
		return fmt.Sprintf(`--auth-choice anthropic-api-key --anthropic-api-key "${%s:-placeholder}"`, envVar)
	case "fireworks":
		return fmt.Sprintf(`--auth-choice fireworks-api-key --fireworks-api-key "${%s}"`, envVar)
	case "openai":
		return fmt.Sprintf(`--auth-choice openai-api-key --openai-api-key "${%s}"`, envVar)
	case "groq":
		return fmt.Sprintf(`--auth-choice groq-api-key --groq-api-key "${%s}"`, envVar)
	case "deepseek":
		return fmt.Sprintf(`--auth-choice deepseek-api-key --deepseek-api-key "${%s}"`, envVar)
	default:
		return `--auth-choice anthropic-api-key --anthropic-api-key "${ANTHROPIC_API_KEY:-placeholder}"`
	}
}
