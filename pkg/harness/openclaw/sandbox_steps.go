package openclaw

import (
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/harness"
)

var _ harness.SandboxSteps = bootstrap{}

func (bootstrap) UninstallCommand() string {
	return `NPM="/usr/local/share/nvm/current/bin/npm"; \
PREFIX="$("$NPM" config get prefix)"; \
echo "npm=$NPM prefix=$PREFIX"; \
sudo "$NPM" uninstall -g openclaw --prefix "$PREFIX" 2>&1 || true; \
hash -r; \
echo uninstalled`
}

func (bootstrap) VerifyVersionCommand(version string) string {
	return fmt.Sprintf(`export NVM_DIR=/usr/local/share/nvm; \
NPM="$NVM_DIR/current/bin/npm"; \
PREFIX="$("$NPM" config get prefix)"; \
export PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH"; \
hash -r; \
OPENCLAW_PATH="$(command -v openclaw || true)"; \
OPENCLAW_VERSION="$(openclaw --version 2>&1 || true)"; \
PACKAGE_VERSION="$(PREFIX="$PREFIX" node -e "try{console.log(require(process.env.PREFIX + '/lib/node_modules/openclaw/package.json').version)}catch(e){process.exit(0)}" 2>/dev/null || true)"; \
echo "openclaw path=$OPENCLAW_PATH"; \
echo "openclaw version=$OPENCLAW_VERSION"; \
echo "openclaw package_version=$PACKAGE_VERSION"; \
case "$OPENCLAW_VERSION" in *%s*) ;; *) echo "expected openclaw %s"; exit 1 ;; esac`, version, version)
}

func (bootstrap) OnboardCommand(llmKeyEnv, onboardFlags string) string {
	return fmt.Sprintf(
		"%sexport NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; openclaw onboard --non-interactive --accept-risk --skip-daemon --skip-health %s 2>&1",
		llmKeyEnv,
		onboardFlags,
	)
}

func (bootstrap) ConfigExistsCommand(homeDir string) string {
	return fmt.Sprintf(`export HOME=%s; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`, homeDir)
}

func (bootstrap) StagePluginDepsCommand() string {
	return `export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; \
export OPENCLAW_EAGER_BUNDLED_PLUGIN_DEPS=1; \
openclaw plugins deps --repair 2>&1 || echo "plugin deps staging completed with warnings"`
}

func (bootstrap) StartGatewayCommand(port int) string {
	return fmt.Sprintf(`
python3 - <<'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(path) as f: cfg = json.load(f)
cfg.setdefault('gateway', {})['bind'] = 'loopback'
cfg['gateway']['port'] = %[1]d
# Keep token auth that onboard generated - don't change auth mode
with open(path, 'w') as f: json.dump(cfg, f, indent=2)
print('gateway config updated')
PYEOF
export NVM_DIR="/usr/local/share/nvm"; [ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"
export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; setsid nohup openclaw gateway run >> ~/.openclaw/gateway.log 2>&1 </dev/null &
# Phase 1: wait for HTTP server to be listening (quick)
for i in $(seq 1 30); do
  curl -sf http://localhost:%[1]d/healthz >/dev/null && echo 'gateway listening' && break
  sleep 1
done
curl -sf http://localhost:%[1]d/healthz >/dev/null || { echo 'gateway failed to listen'; tail -n 100 ~/.openclaw/gateway.log 2>/dev/null || true; exit 1; }
# Phase 2: wait for gateway startup to complete. Do not use openclaw health
# here: it pairs the CLI device with read-only scopes before claw-bridge can
# connect, then claw-bridge is rejected as a scope-upgrade.
for i in $(seq 1 30); do
  if grep -q 'gateway ready' ~/.openclaw/gateway.log 2>/dev/null; then
    echo "gateway ready"
    exit 0
  fi
  curl -sf http://localhost:%[1]d/healthz >/dev/null || break
  sleep 1
done
# Fallback: if the readiness log line is unavailable but the gateway is still
# listening and healthy, don't fail the bootstrap.
if curl -sf http://localhost:%[1]d/healthz >/dev/null; then
  echo "gateway ready (healthz)"
  exit 0
fi
echo 'gateway not ready'
tail -n 100 ~/.openclaw/gateway.log 2>/dev/null || true
exit 1`, port)
}

func (bootstrap) InstallCommand(homeDir, version string) string {
	installScript := fmt.Sprintf(`set -o pipefail
export HOME=%s
export NVM_DIR=/usr/local/share/nvm
NPM="$NVM_DIR/current/bin/npm"
PREFIX="$("$NPM" config get prefix)"
export PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH"
LOG=/tmp/openclaw-install.log
STATUS=/tmp/openclaw-install.status
echo "npm=$NPM prefix=$PREFIX"
if sudo env PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH" "$NPM" install -g openclaw@%s --prefix "$PREFIX" 2>&1; then
  hash -r
  echo ok > "$STATUS"
  echo "install done"
else
  rc=$?
  echo "failed:$rc" > "$STATUS"
  exit "$rc"
fi`, homeDir, version)
	return fmt.Sprintf(`export HOME=%s
LOG=/tmp/openclaw-install.log
STATUS=/tmp/openclaw-install.status
rm -f "$LOG" "$STATUS"
setsid nohup bash -c %s > "$LOG" 2>&1 </dev/null &
echo "openclaw-install-status=started"`, homeDir, shellQuote(installScript))
}

func (bootstrap) InstallModelPluginCommand(homeDir, provider, version string) string {
	if provider != "codex" {
		return ""
	}
	spec := "npm:@openclaw/codex@" + version
	return fmt.Sprintf(`export HOME=%s
export NVM_DIR=/usr/local/share/nvm
export PATH="$NVM_DIR/current/bin:/usr/local/bin:$PATH"
if openclaw plugins info codex --json >/dev/null 2>&1; then
  echo "Codex plugin already installed"
else
  openclaw plugins install %s
fi
openclaw plugins info codex --json >/dev/null`, homeDir, shellQuote(spec))
}

func (bootstrap) InstallStatusCommand(homeDir, version string) string {
	return fmt.Sprintf(`export HOME=%s
LOG=/tmp/openclaw-install.log
STATUS=/tmp/openclaw-install.status
if [ -s "$STATUS" ]; then
  status="$(cat "$STATUS")"
  case "$status" in
    ok)
      echo "openclaw-install-status=ok"
      exit 0
      ;;
    failed:*)
      echo "openclaw-install-status=failed"
      echo "$status"
      tail -n 120 "$LOG" 2>/dev/null || true
      exit 0
      ;;
    *)
      echo "openclaw-install-status=unknown:$status"
      tail -n 120 "$LOG" 2>/dev/null || true
      exit 0
      ;;
  esac
fi
if pgrep -af %s >/dev/null 2>&1; then
  echo "openclaw-install-status=pending"
  tail -n 20 "$LOG" 2>/dev/null || true
  exit 0
fi
echo "openclaw-install-status=missing"
tail -n 120 "$LOG" 2>/dev/null || true`, homeDir, shellQuote("openclaw@"+version))
}
