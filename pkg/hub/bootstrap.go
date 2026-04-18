package hub

import (
	"fmt"

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
	LLMKeyEnv   string // pre-built export lines
	LinearEnv   string // pre-built export line
	RelayEnv    string // pre-built export lines
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
  export ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-placeholder}"
  openclaw onboard \
    --non-interactive --accept-risk \
    --auth-choice anthropic-api-key \
    --anthropic-api-key "$ANTHROPIC_API_KEY" \
    --gateway-bind loopback --gateway-port 18789 \
    --skip-daemon 2>/dev/null || true
  python3 << 'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
try:
    with open(path) as f:
        config = json.load(f)
except:
    config = {}
model = os.environ.get('OPENCLAW_DEFAULT_MODEL', 'anthropic/claude-sonnet-4-6')
apiKey = os.environ.get('ANTHROPIC_API_KEY', 'placeholder')
config.setdefault('agents', {}).setdefault('defaults', {})['model'] = model
config['models'] = {
    'providers': {
        'anthropic': {
            'apiKey': apiKey,
            'baseUrl': 'https://api.anthropic.com',
            'api': 'anthropic-messages',
            'models': [
                {'id': 'claude-sonnet-4-6', 'name': 'Claude Sonnet 4.6', 'api': 'anthropic-messages'},
                {'id': 'claude-opus-4-5', 'name': 'Claude Opus 4.5', 'api': 'anthropic-messages'},
                {'id': 'claude-sonnet-4-5', 'name': 'Claude Sonnet 4.5', 'api': 'anthropic-messages'}
            ]
        }
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
PYEOF
fi

# ── Start OpenClaw gateway ────────────────────────────────────────────────────
echo "Starting OpenClaw gateway..."
export OPENCLAW_NO_RESPAWN=1
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
		p.BridgeURL,
		p.HubURL, p.ClawID, p.ClawToken, p.ClawName, p.GatewayPassword,
		p.RelayEnv, p.DefaultModel, p.LLMKeyEnv,
		credHelper,
	)
}
