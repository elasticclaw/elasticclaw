package hub

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// BootstrapParams holds all inputs needed to generate a bootstrap script.
// It is intentionally a pure value type — no DB, no server, no side effects.
type BootstrapParams struct {
	// Claw identity
	ClawID         string
	ClawName       string
	ClawToken      string
	ModelAuthToken string
	TemplateName   string

	// Hub connectivity
	HubURL string

	// OpenClaw config
	DefaultModel    string
	GatewayPassword string
	LLMProvider     string

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
	LLMKeyEnv      string            // pre-built export lines
	ModelAuthEnv   string            // pre-built export lines for CLI-backed model auth state
	APIKeyAuthSync string            // shell script that persists API-key auth into OpenClaw's auth store
	OAuthAuthSync  string            // shell script that persists restored OAuth auth into OpenClaw's auth store
	LinearEnv      string            // pre-built export line
	ProviderConfig string            // python snippet to patch OpenClaw config
	OnboardFlags   string            // --auth-choice ... flags for openclaw onboard
	Env            map[string]string // custom env vars from workflow/factory secret_refs and template env
}

// bootstrapManagedEnvKeys lists values that the bootstrap script derives from
// dedicated parameters. Custom env must not override or duplicate them.
var bootstrapManagedEnvKeys = map[string]bool{
	"ELASTICCLAW_HUB_URL":            true,
	"ELASTICCLAW_CLAW_ID":            true,
	"ELASTICCLAW_CLAW_TOKEN":         true,
	"ELASTICCLAW_MODEL_AUTH_TOKEN":   true,
	"ELASTICCLAW_CLAW_NAME":          true,
	"ELASTICCLAW_TEMPLATE":           true,
	"ELASTICCLAW_GITHUB_REPOS":       true,
	"ELASTICCLAW_BOOTSTRAP":          true,
	"ELASTICCLAW_WAIT_FOR_WORKSPACE": true,
	"ELASTICCLAW_GATEWAY_PASSWORD":   true,
	"OPENCLAW_GATEWAY_PASSWORD":      true,
	"OPENCLAW_DEFAULT_MODEL":         true,
	"ELASTICCLAW_LLM_PROVIDER":       true,
	"ELASTICCLAW_NIX":                true,
	"ELASTICCLAW_DOCKER":             true,
	"ELASTICCLAW_PROVIDER_CONFIG":    true,
	"ELASTICCLAW_API_KEY_AUTH_SYNC":  true,
	"ELASTICCLAW_OAUTH_AUTH_SYNC":    true,
	"ELASTICCLAW_ONBOARD_FLAGS":      true,
	"LINEAR_API_KEY":                 true,
}

// shellVarNameRegex matches valid POSIX shell variable names.
var shellVarNameRegex = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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

func resolveActiveProvider(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	if key := resolveActiveKey(keys, selectedKeyName); key != nil {
		return key.Provider
	}
	return ""
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
	oauthAuthSyncLine := "# No OAuth auth sync"
	if p.OAuthAuthSync != "" {
		oauthAuthSyncLine = fmt.Sprintf("export ELASTICCLAW_OAUTH_AUTH_SYNC=%s",
			shellQuote(p.OAuthAuthSync))
	}

	linearEnvLine := p.LinearEnv
	if linearEnvLine == "" {
		linearEnvLine = "# Linear not configured"
	}

	customEnvExports := "# No custom env vars"
	customEnvPersist := ""
	if len(p.Env) > 0 {
		var lines []string
		var persistLines []string
		keys := make([]string, 0, len(p.Env))
		for k := range p.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := p.Env[k]
			if bootstrapManagedEnvKeys[k] {
				continue
			}
			if !shellVarNameRegex.MatchString(k) {
				log.Printf("[bootstrap] WARNING: skipping invalid env var name %q", k)
				continue
			}
			lines = append(lines, fmt.Sprintf("export %s=%s", k, shellQuote(v)))
			persistLines = append(persistLines, fmt.Sprintf("  printf 'export %s=%%q\\n' \"$%s\"", k, k))
		}
		if len(lines) > 0 {
			customEnvExports = strings.Join(lines, "\n")
			customEnvPersist = strings.Join(persistLines, "\n")
		}
	}

	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

# ── Custom env vars (workflow/factory secret_refs, template env) ───────────────
%s
# ── Identity + credentials ────────────────────────────────────────────────────
export ELASTICCLAW_HUB_URL=%s
export ELASTICCLAW_CLAW_ID=%s
export ELASTICCLAW_CLAW_TOKEN=%s
export ELASTICCLAW_MODEL_AUTH_TOKEN=%s
export ELASTICCLAW_CLAW_NAME=%s
export ELASTICCLAW_TEMPLATE=%s
export ELASTICCLAW_GATEWAY_PASSWORD=%s
export OPENCLAW_GATEWAY_PASSWORD="$ELASTICCLAW_GATEWAY_PASSWORD"
export OPENCLAW_DEFAULT_MODEL=%s
export ELASTICCLAW_LLM_PROVIDER=%s
export ELASTICCLAW_NIX=%s
export ELASTICCLAW_DOCKER=%s
%s
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
%s
  printf 'export ELASTICCLAW_HUB_URL=%%q\n' "$ELASTICCLAW_HUB_URL"
  printf 'export ELASTICCLAW_CLAW_ID=%%q\n' "$ELASTICCLAW_CLAW_ID"
  printf 'export ELASTICCLAW_CLAW_TOKEN=%%q\n' "$ELASTICCLAW_CLAW_TOKEN"
  printf 'export ELASTICCLAW_MODEL_AUTH_TOKEN=%%q\n' "$ELASTICCLAW_MODEL_AUTH_TOKEN"
  printf 'export ELASTICCLAW_CLAW_NAME=%%q\n' "$ELASTICCLAW_CLAW_NAME"
  printf 'export ELASTICCLAW_TEMPLATE=%%q\n' "$ELASTICCLAW_TEMPLATE"
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
		customEnvExports,
		shellQuote(p.HubURL), shellQuote(p.ClawID), shellQuote(p.ClawToken), shellQuote(p.ModelAuthToken), shellQuote(p.ClawName), shellQuote(p.TemplateName), shellQuote(p.GatewayPassword),
		shellQuote(p.DefaultModel), shellQuote(p.LLMProvider), shellQuote(nixFlag), shellQuote(dockerFlag),
		p.LLMKeyEnv, p.ModelAuthEnv, linearEnvLine, apiKeyAuthSyncLine, oauthAuthSyncLine, shellQuote(p.OnboardFlags), providerConfigLine,
		shellQuote(p.BridgeURL),
		customEnvPersist,
	)
}

// shellQuote returns a single-quoted shell string safe for embedding in scripts.
// Single quotes cannot appear inside single-quoted strings in bash, so we
// replace them with '"'"'.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
