// Daytona provisioning: sandbox bootstrap, bridge install, and keep-alive.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	"github.com/elasticclaw/elasticclaw/pkg/hub/telemetry"
	daytona "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ─── Provisioning ─────────────────────────────────────────────────────────────

func (s *Server) provisionDaytona(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		return fmt.Errorf("daytona init: %w", err)
	}
	s.setBootstrapStatus(clawID, "Creating sandbox")
	// Resolve snapshot: template snapshot > hub default_snapshot
	snapshot := req.Snapshot
	if snapshot == "" {
		snapshot = cfg.DefaultSnapshot
	}
	createReq := types.CreateRequest{
		Name:          req.ProviderName, // stable ec-<shortid>, decoupled from display name
		FromImage:     snapshot,
		TemplateFiles: files,
		Env:           env,
	}
	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "daytona")
	instance, err := p.Create(createCtx, createReq)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("daytona create: %w", err)
	}
	logfCtx(ctx, "daytona workspace created: %s (claw %s)", instance.ID, clawID)
	recordE2EDaytonaSandboxID(instance.ID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='daytona', provider_id=? WHERE id=?`, instance.ID, clawID)

	// Bootstrap: install OpenClaw + claw-bridge via exec (retry up to 3x for transient Daytona API timeouts)
	clawName := req.Name
	go func() {
		// Each step inside bootstrapDaytona retries 3x internally.
		// Outer retries here handle the rare case of total step failure.
		const maxBootstrapAttempts = 3
		var lastErr error
		for attempt := 1; attempt <= maxBootstrapAttempts; attempt++ {
			if attempt > 1 {
				logfCtx(ctx, "[daytona] full bootstrap retry for claw %s in 15s...", clawName)
				time.Sleep(15 * time.Second)
			}
			// The async bootstrap must outlive the triggering request, so it
			// derives from the hub root context, not the enclosing ctx.
			lastErr = s.bootstrapDaytona(s.base(), clawID, clawName, instance.ID, p, env) //nolint:contextcheck
			if lastErr == nil {
				return
			}
			if s.daytonaBridgeRunning(s.base(), instance.ID, p) { //nolint:contextcheck
				logfCtx(ctx, "[daytona] bootstrap attempt %d/%d for claw %s returned error after claw-bridge started; treating bootstrap as complete: %v", attempt, maxBootstrapAttempts, clawName, lastErr)
				return
			}
			logfCtx(ctx, "[daytona] bootstrap attempt %d/%d failed for claw %s: %v", attempt, maxBootstrapAttempts, clawName, lastErr)
		}
		logfCtx(ctx, "[daytona] bootstrap failed for claw %s: %v", clawName, lastErr)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Daytona bootstrap failed: %v", lastErr), false)
		// stopAgentWithReason already terminates the VM; no need to destroy again
	}()
	return nil
}

func (s *Server) bootstrapDaytona(ctx context.Context, clawID, clawName, instanceID string, p *daytona.Provider, env map[string]string) error {
	logfCtx(ctx, "[daytona] bootstrapping claw %s (instance %s)", clawID, instanceID)
	s.setBootstrapStatus(clawID, "Preparing runtime")

	execResult := func(label string, timeout time.Duration, cmd string) (*types.ExecResult, error) {
		s.setBootstrapStatus(clawID, daytonaBootstrapStatusForStep(label))
		const maxAttempts = 3
		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if attempt == 1 {
				logfCtx(ctx, "[daytona] %s...", label)
			} else {
				logfCtx(ctx, "[daytona] %s retry %d/%d...", label, attempt, maxAttempts)
				time.Sleep(5 * time.Second)
			}
			// Prefix HOME so commands run in the sandbox user's home, not the caller's.
			// Also source nvm and pin Node 24 LTS — Daytona snapshots may ship with
			// non-LTS Node (e.g. v25) and each exec is a fresh shell session.
			// If nvm use 24 fails (not installed yet), we install it on the fly.
			nvmSetup := `export HOME=/home/daytona; export NVM_DIR=/usr/local/share/nvm; [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh" && { nvm use 24 >/dev/null 2>&1 || nvm install 24 >/dev/null 2>&1; } ; `
			result, err := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", nvmSetup + cmd}, timeout)
			if err != nil {
				lastErr = fmt.Errorf("%s: %w", label, err)
				continue
			}
			if result.ExitCode != 0 {
				lastErr = fmt.Errorf("%s failed (exit %d): %s", label, result.ExitCode, sanitizeBootstrapOutput(result.Stdout))
				continue
			}
			logfCtx(ctx, "[daytona] %s done", label)
			return result, nil
		}
		return nil, lastErr
	}

	exec := func(label string, timeout time.Duration, cmd string) error {
		_, err := execResult(label, timeout, cmd)
		return err
	}

	// Step 1: Install pinned OpenClaw version.
	// Run install in background and poll — avoids the 60s HTTP client timeout
	// that kills synchronous long-running commands.
	// Uninstall old openclaw then reinstall pinned version (ensures nvm current symlink is updated)
	if err := exec("uninstall old openclaw", 20*time.Second,
		`NPM="/usr/local/share/nvm/current/bin/npm"; \
PREFIX="$("$NPM" config get prefix)"; \
echo "npm=$NPM prefix=$PREFIX"; \
sudo "$NPM" uninstall -g openclaw --prefix "$PREFIX" 2>&1 || true; \
hash -r; \
echo uninstalled`); err != nil {
		logfCtx(ctx, "[daytona] warning: uninstall failed (ok if not installed): %v", err)
	}

	const daytonaOpenClawVersion = cliversion.OpenClawVersion
	if err := exec("start openclaw install", 20*time.Second, daytonaStartOpenClawInstallCommand(daytonaOpenClawVersion)); err != nil {
		return err
	}
	deadline := time.Now().Add(4 * time.Minute)
	var lastInstallStatus string
	installComplete := false
	for !installComplete {
		result, err := execResult("check openclaw install", 15*time.Second, daytonaOpenClawInstallStatusCommand(daytonaOpenClawVersion))
		if err != nil {
			lastInstallStatus = err.Error()
		} else {
			lastInstallStatus = strings.TrimSpace(result.Stdout)
			switch {
			case strings.Contains(result.Stdout, "openclaw-install-status=ok"):
				installComplete = true
			case strings.Contains(result.Stdout, "openclaw-install-status=failed"),
				strings.Contains(result.Stdout, "openclaw-install-status=missing"),
				strings.Contains(result.Stdout, "openclaw-install-status=unknown"):
				return fmt.Errorf("install openclaw failed: %s", sanitizeBootstrapOutput(result.Stdout))
			}
		}
		if installComplete {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("install openclaw timed out: %s", sanitizeBootstrapOutput(lastInstallStatus))
		}
		time.Sleep(10 * time.Second)
	}

	if err := exec("verify openclaw", 20*time.Second,
		fmt.Sprintf(`export NVM_DIR=/usr/local/share/nvm; \
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
case "$OPENCLAW_VERSION" in *%s*) ;; *) echo "expected openclaw %s"; exit 1 ;; esac`, daytonaOpenClawVersion, daytonaOpenClawVersion)); err != nil {
		return err
	}

	// Step 1b: Install Nix (Determinate Systems) if requested.
	var nixEnabled int
	_ = s.db.QueryRow(`SELECT nix FROM claws WHERE id=?`, clawID).Scan(&nixEnabled)
	if nixEnabled == 1 {
		if err := exec("install nix", 3*time.Minute,
			`export HOME=/home/daytona; \
curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix | \
  sh -s -- install linux --no-confirm --init none >> /tmp/nix-install.log 2>&1; \
. /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true; \
nix --version`); err != nil {
			logfCtx(ctx, "[daytona] warning: nix install failed: %v", err)
		}
	}

	// Step 1c: Install Docker Engine if requested.
	var dockerEnabled int
	_ = s.db.QueryRow(`SELECT docker FROM claws WHERE id=?`, clawID).Scan(&dockerEnabled)
	if dockerEnabled == 1 {
		if err := exec("install docker", 3*time.Minute,
			`export HOME=/home/daytona; \
. /etc/os-release; \
if [ "$ID" = "debian" ] && [ -n "$VERSION_CODENAME" ]; then \
  DOCKER_REPO="deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian $VERSION_CODENAME stable"; \
  DOCKER_GPG="https://download.docker.com/linux/debian/gpg"; \
else \
  DOCKER_REPO="deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable"; \
  DOCKER_GPG="https://download.docker.com/linux/ubuntu/gpg"; \
fi; \
sudo apt-get update -qq && \
sudo apt-get install -y --fix-broken ca-certificates curl gnupg && \
sudo install -m 0755 -d /etc/apt/keyrings && \
curl -fsSL "$DOCKER_GPG" | sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/docker.gpg && \
sudo chmod a+r /etc/apt/keyrings/docker.gpg && \
echo "$DOCKER_REPO" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null && \
sudo apt-get update -qq && \
sudo apt-get install -y --fix-broken docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin && \
sudo usermod -aG docker daytona 2>/dev/null || true && \
docker --version`); err != nil {
			logfCtx(ctx, "[daytona] warning: docker install failed: %v", err)
		}
	}

	// Step 2: Onboard (configure OpenClaw) with the correct auth provider
	s.setBootstrapStatus(clawID, "Configuring OpenClaw")
	var llmKeyNameDaytona string
	_ = s.db.QueryRow(`SELECT COALESCE(llm_key,'') FROM claws WHERE id=?`, clawID).Scan(&llmKeyNameDaytona)
	activeKeyNameDaytona := ""
	activeKeyProviderDaytona := ""
	s.cfgMu.RLock()
	activeKeyDaytona := resolveActiveKey(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	defaultModelDaytona := resolveDefaultModelForKey(s.hubCfg, activeKeyDaytona)
	llmKeyEnvDaytona := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	modelAuthEnvDaytona := buildModelAuthEnv(s.hubCfg, llmKeyNameDaytona)
	apiKeyAuthSyncDaytona := buildOpenClawAPIKeyAuthSyncShell(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	onboardFlags := buildOnboardFlags(s.hubCfg.LLMKeys, llmKeyNameDaytona, defaultModelDaytona)
	providerConfigScript := buildOpenClawProviderConfig(s.hubCfg.LLMKeys, llmKeyNameDaytona)
	if activeKeyDaytona != nil {
		activeKeyNameDaytona = activeKeyDaytona.Name
		activeKeyProviderDaytona = activeKeyDaytona.Provider
	}
	s.cfgMu.RUnlock()
	logfCtx(ctx, "[daytona] OpenClaw model resolution claw=%s selected_llm_key=%q active_llm_key=%q provider=%q default_model=%q config_patch=%t",
		clawID, llmKeyNameDaytona, activeKeyNameDaytona, activeKeyProviderDaytona, defaultModelDaytona, providerConfigScript != "")
	gatewayPassword := randomHex(16)
	if restoreShell := buildModelAuthRestoreShell(modelAuthEnvDaytona); restoreShell != "" {
		if err := exec("restore model auth", 30*time.Second, "export HOME=/home/daytona; "+restoreShell); err != nil {
			return fmt.Errorf("restore model auth: %w", err)
		}
	}
	if installCmd := daytonaInstallCodingModelCLICommand(defaultModelDaytona); installCmd != "" {
		if err := exec("install selected model cli", 2*time.Minute, installCmd); err != nil {
			return fmt.Errorf("install selected model cli: %w", err)
		}
	}
	onboardCmd := fmt.Sprintf(
		"%sexport NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; openclaw onboard --non-interactive --accept-risk --skip-daemon --skip-health %s 2>&1",
		llmKeyEnvDaytona,
		onboardFlags,
	)
	logfCtx(ctx, "[daytona] onboard openclaw...")
	onboardResult, onboardErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", "export HOME=/home/daytona; " + onboardCmd}, 2*time.Minute)
	if onboardErr != nil {
		result, diagErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", `export HOME=/home/daytona; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`}, 10*time.Second)
		if diagErr != nil || strings.TrimSpace(result.Stdout) != "exists" {
			return fmt.Errorf("onboard openclaw: %w", onboardErr)
		}
		logfCtx(ctx, "[daytona] onboard returned error, but config file exists; continuing")
	} else if onboardResult.ExitCode != 0 {
		result, diagErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", `export HOME=/home/daytona; [ -f "$HOME/.openclaw/openclaw.json" ] && echo exists || echo missing`}, 10*time.Second)
		if diagErr != nil || strings.TrimSpace(result.Stdout) != "exists" {
			return fmt.Errorf("onboard openclaw failed (exit %d): %s", onboardResult.ExitCode, onboardResult.Stdout)
		}
		logfCtx(ctx, "[daytona] onboard returned non-zero, but config file exists; continuing")
	} else {
		logfCtx(ctx, "[daytona] onboard openclaw done")
	}

	if apiKeyAuthSyncDaytona != "" {
		syncCmd := `export HOME=/home/daytona; export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; ` + llmKeyEnvDaytona + apiKeyAuthSyncDaytona
		if err := exec("sync openclaw api key auth", 30*time.Second, syncCmd); err != nil {
			return fmt.Errorf("sync openclaw api key auth: %w", err)
		}
	}

	configPatch := fmt.Sprintf("export HOME=/home/daytona; export OPENCLAW_DEFAULT_MODEL=%q; export ELASTICCLAW_GATEWAY_PASSWORD=%q; ", defaultModelDaytona, gatewayPassword) + llmKeyEnvDaytona + providerConfigScript
	if err := exec("configure openclaw model", 30*time.Second, configPatch); err != nil {
		return err
	}
	// Step 2a: Preflight required commands and environment.
	// Fail early if the sandbox is missing tools that OpenClaw or agents need.
	if err := exec("preflight required commands", 30*time.Second,
		`export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; \
for cmd in node npm git python3 curl; do command -v "$cmd" >/dev/null || { echo "missing: $cmd"; exit 1; }; done; \
echo "preflight ok"`); err != nil {
		return fmt.Errorf("daytona sandbox missing required commands: %w", err)
	}
	// Step 2b: Pre-stage plugin runtime dependencies before starting gateway.
	// This prevents the gateway from doing expensive npm installs while
	// clients are connected, which causes event-loop delays and connection drops.
	if err := exec("stage openclaw plugin deps", 3*time.Minute,
		`export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; \
export OPENCLAW_EAGER_BUNDLED_PLUGIN_DEPS=1; \
openclaw plugins deps --repair 2>&1 || echo "plugin deps staging completed with warnings"`); err != nil {
		logfCtx(ctx, "[daytona] warning: plugin deps staging failed: %v", err)
	}

	// Step 2c: Configure gateway bind/port and start it.
	// Use token auth (what onboard sets up) — don't override auth mode.
	gatewaySetup := `
python3 - <<'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(path) as f: cfg = json.load(f)
cfg.setdefault('gateway', {})['bind'] = 'loopback'
cfg['gateway']['port'] = 18789
# Keep token auth that onboard generated - don't change auth mode
with open(path, 'w') as f: json.dump(cfg, f, indent=2)
print('gateway config updated')
PYEOF
export NVM_DIR="/usr/local/share/nvm"; [ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"
export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH; setsid nohup openclaw gateway run >> ~/.openclaw/gateway.log 2>&1 </dev/null &
# Phase 1: wait for HTTP server to be listening (quick)
for i in $(seq 1 30); do
  curl -sf http://localhost:18789/healthz >/dev/null && echo 'gateway listening' && break
  sleep 1
done
curl -sf http://localhost:18789/healthz >/dev/null || { echo 'gateway failed to listen'; tail -n 100 ~/.openclaw/gateway.log 2>/dev/null || true; exit 1; }
# Phase 2: wait for gateway startup to complete. Do not use openclaw health
# here: it pairs the CLI device with read-only scopes before claw-bridge can
# connect, then claw-bridge is rejected as a scope-upgrade.
for i in $(seq 1 30); do
  if grep -q 'gateway ready' ~/.openclaw/gateway.log 2>/dev/null; then
    echo "gateway ready"
    exit 0
  fi
  curl -sf http://localhost:18789/healthz >/dev/null || break
  sleep 1
done
# Fallback: if the readiness log line is unavailable but the gateway is still
# listening and healthy, don't fail the bootstrap.
if curl -sf http://localhost:18789/healthz >/dev/null; then
  echo "gateway ready (healthz)"
  exit 0
fi
echo 'gateway not ready'
tail -n 100 ~/.openclaw/gateway.log 2>/dev/null || true
exit 1`
	if err := exec("start openclaw gateway", 2*time.Minute, gatewaySetup); err != nil {
		return err
	}

	// Step 3: Download claw-bridge now, but do not start it until the workspace,
	// template files, and bootstrap gating are fully ready.
	s.setBootstrapStatus(clawID, "Preparing workspace")
	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		return fmt.Errorf("claw-bridge URL not configured: set bridge_image in hub.yaml (e.g. bridge_image: ttl.sh/your/claw-bridge:tag) or build a tagged release")
	}
	var downloadCmd string
	if strings.HasPrefix(bridgeURL, "http://") || strings.HasPrefix(bridgeURL, "https://") {
		downloadCmd = fmt.Sprintf(`rm -f /tmp/claw-bridge.download && curl -fsSL %q -o /tmp/claw-bridge.download && chmod +x /tmp/claw-bridge.download && mv -f /tmp/claw-bridge.download /tmp/claw-bridge && echo downloaded`, bridgeURL)
	} else {
		// OCI ref (ttl.sh or ghcr) — use oras
		downloadCmd = fmt.Sprintf(`
if ! command -v oras &>/dev/null; then
  curl -sL https://github.com/oras-project/oras/releases/download/v1.2.2/oras_1.2.2_linux_amd64.tar.gz | tar xz -C /tmp && sudo mv /tmp/oras /usr/local/bin/oras
fi
mkdir -p /tmp/bridge-dl && cd /tmp/bridge-dl && oras pull %q
BIN=$(find /tmp/bridge-dl -name 'claw-bridge*' -type f | head -1)
cp "$BIN" /tmp/claw-bridge.download && chmod +x /tmp/claw-bridge.download && mv -f /tmp/claw-bridge.download /tmp/claw-bridge && echo downloaded`, bridgeURL)
	}
	if err := s.downloadDaytonaConnector(ctx, clawID, instanceID, p, downloadCmd); err != nil {
		return err
	}

	s.cfgMu.RLock()
	clawToken := s.hubCfg.ClawToken
	s.cfgMu.RUnlock()

	// Write template files (SOUL.md, AGENTS.md, etc.) to the workspace before
	// the bridge starts so BOOTSTRAP.md and friends are present for the first turn.
	s.setBootstrapStatus(clawID, "Preparing workspace")
	var filesJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&filesJSON)
	var templateFiles map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &templateFiles); err == nil && len(templateFiles) > 0 {
		templateFiles = workspaceTemplateFiles(templateFiles)
		for name, content := range templateFiles {
			name := name
			content := content
			safeName, err := cleanWorkspaceFilePath(name)
			if err != nil {
				logfCtx(ctx, "[daytona] warning: skipping invalid template file path %q: %v", name, err)
				continue
			}
			targetPath := "/home/daytona/.openclaw/workspace/" + safeName
			targetDir := path.Dir(targetPath)
			writeCmd := fmt.Sprintf(
				`export HOME=/home/daytona; mkdir -p %s && cat > %s << 'ELASTICCLAW_EOF'
%s
ELASTICCLAW_EOF`,
				shellQuote(targetDir), shellQuote(targetPath), content)
			if err := exec("write "+name, 15*time.Second, writeCmd); err != nil {
				logfCtx(ctx, "[daytona] warning: failed to write %s: %v", name, err)
			}
		}
		logfCtx(ctx, "[daytona] template files written for claw %s", clawID)
	}

	// Step 5: GitHub credential helper (if GitHub Apps configured)
	var workspaceName string
	var repositories []types.GitHubRepoAccess
	var repositoriesJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(template,''), COALESCE(github_repos,'[]') FROM claws WHERE id=?`, clawID).Scan(&workspaceName, &repositoriesJSON)
	_ = json.Unmarshal([]byte(repositoriesJSON), &repositories)
	s.cfgMu.RLock()
	hasHubGitHubApps := len(s.hubCfg.GitHubApps) > 0
	s.cfgMu.RUnlock()
	hasWorkspaceGitHubApps := false
	if workspaceName != "" {
		if workspaceApps, err := loadWorkspaceGitHubAppConfigs(workspaceName); err == nil && len(workspaceApps) > 0 {
			hasWorkspaceGitHubApps = true
		}
	}
	hasGitHubApps := hasHubGitHubApps || hasWorkspaceGitHubApps
	if hasGitHubApps {
		s.setBootstrapStatus(clawID, "Preparing repository access")
		// Use the hub directly during bootstrap. The bridge is intentionally not
		// started yet so startup cannot race ahead of template file writes and
		// bootstrap_ok gating.
		tokenURL := fmt.Sprintf("%s/api/github/token/%s?claw_token=%s", s.clawHubURL(), clawID, clawToken)

		// Step 5a: write the credential helper binary
		credHelperScript := fmt.Sprintf(`export HOME=/home/daytona
sudo tee /usr/local/bin/elasticclaw-git-credentials > /dev/null << 'CREDEOF'
#!/bin/bash
# Retry up to 10 times — hub token endpoint may not be ready immediately
for i in $(seq 1 10); do
  response=$(curl -sf --max-time 35 %q)
  if [ $? -eq 0 ] && [ -n "$response" ]; then break; fi
  sleep 3
done
if [ -z "$response" ]; then exit 1; fi
token=$(echo "$response" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])")
echo "protocol=https"
echo "host=github.com"
echo "username=x-access-token"
echo "password=$token"
CREDEOF
sudo chmod +x /usr/local/bin/elasticclaw-git-credentials
git config --global credential.helper /usr/local/bin/elasticclaw-git-credentials
echo 'credential helper installed'`, tokenURL)
		if err := exec("install git credential helper", 20*time.Second, credHelperScript); err != nil {
			return fmt.Errorf("install git credential helper: %w", err)
		} else {
			installGhScript := `export HOME=/home/daytona
if command -v gh >/dev/null 2>&1; then
  gh --version >/dev/null 2>&1
  exit 0
fi
if command -v apt-get >/dev/null 2>&1; then
  sudo apt-get update -qq && sudo apt-get install -y gh
elif command -v dnf >/dev/null 2>&1; then
  sudo dnf install -y gh
elif command -v yum >/dev/null 2>&1; then
  sudo yum install -y gh
else
  echo 'unsupported package manager for gh install'
  exit 1
fi
command -v gh >/dev/null 2>&1 && gh --version >/dev/null 2>&1`
			if err := exec("install gh cli", 2*time.Minute, installGhScript); err != nil {
				return fmt.Errorf("install gh cli: %w", err)
			}

			configureGitHubTokenRefresh := `export HOME=/home/daytona
set +x
` + buildGitHubTokenProfileInstallScript() + `
` + buildGitHubCLIWrapperInstallScript() + `
. /etc/profile.d/elasticclaw-github.sh
command -v gh
[ -n "${GH_TOKEN:-}" ]
gh --version`
			logfCtx(ctx, "[daytona] configure gh token refresh (no retries)...")
			ghAuthResult, ghAuthErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", configureGitHubTokenRefresh}, 30*time.Second)
			if ghAuthErr != nil {
				return fmt.Errorf("configure gh token refresh: %w", ghAuthErr)
			}
			if ghAuthResult.ExitCode != 0 {
				return fmt.Errorf("configure gh token refresh failed (exit %d): %s", ghAuthResult.ExitCode, sanitizeBootstrapOutput(ghAuthResult.Stdout))
			}
			logfCtx(ctx, "[daytona] configure gh token refresh done")

			ghStatusScript := `export HOME=/home/daytona
set +x
. /etc/profile.d/elasticclaw-github.sh
gh auth status`
			logfCtx(ctx, "[daytona] verify gh auth (no retries)...")
			ghStatusResult, ghStatusErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", ghStatusScript}, 20*time.Second)
			if ghStatusErr != nil {
				return fmt.Errorf("verify gh auth: %w", ghStatusErr)
			}
			if ghStatusResult.ExitCode != 0 {
				return fmt.Errorf("verify gh auth failed (exit %d): %s", ghStatusResult.ExitCode, sanitizeBootstrapOutput(ghStatusResult.Stdout))
			}
			if len(repositories) > 0 {
				verifyReposScript := "export HOME=/home/daytona; set +x; . /etc/profile.d/elasticclaw-github.sh; "
				for _, repo := range repositories {
					verifyReposScript += fmt.Sprintf("gh repo view %s >/dev/null || exit 1; ", shellQuote(repo.Repo))
				}
				logfCtx(ctx, "[daytona] verify configured repositories (no retries)...")
				verifyReposResult, verifyReposErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", verifyReposScript}, 30*time.Second)
				if verifyReposErr != nil {
					return fmt.Errorf("verify configured repositories: %w", verifyReposErr)
				}
				if verifyReposResult.ExitCode != 0 {
					return fmt.Errorf("verify configured repositories failed (exit %d): %s", verifyReposResult.ExitCode, sanitizeBootstrapOutput(verifyReposResult.Stdout))
				}
			}
			logfCtx(ctx, "[daytona] verify gh auth done")

			logfCtx(ctx, "[daytona] cloning %d repositories for claw %s", len(repositories), clawID)
			s.setBootstrapStatus(clawID, "Syncing repositories")
			for i, repo := range repositories {
				logfCtx(ctx, "[daytona] repository[%d]: %s", i, repo.Repo)
			}

			cloneScript := buildDaytonaGitHubCloneScript(repositories)
			cloneResult, cloneErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", cloneScript}, 2*time.Minute)
			if cloneErr != nil {
				return fmt.Errorf("clone repos: %w", cloneErr)
			}
			if cloneResult.ExitCode != 0 {
				return fmt.Errorf("clone repos failed (exit %d): %s", cloneResult.ExitCode, sanitizeBootstrapOutput(cloneResult.Stdout))
			}
			logfCtx(ctx, "[daytona] clone repos done")

			if len(repositories) > 0 {
				verifyCloneScript := "export HOME=/home/daytona; cd ~/.openclaw/workspace; "
				for _, repo := range repositories {
					verifyCloneScript += daytonaRepoReadinessSnippet(repo.Repo)
				}
				verifyResult, verifyErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", verifyCloneScript}, 20*time.Second)
				if verifyErr != nil {
					return fmt.Errorf("verify cloned repos: %w", verifyErr)
				}
				if verifyResult.ExitCode != 0 {
					return fmt.Errorf("verify cloned repos failed (exit %d): %s", verifyResult.ExitCode, sanitizeBootstrapOutput(verifyResult.Stdout))
				}
				logfCtx(ctx, "[daytona] verify cloned repos done")
			}
			if discoveryScript := buildRepoInstructionDiscoveryScript("$HOME/.openclaw/workspace", repositories); discoveryScript != "" {
				if err := exec("discover repo instructions", 20*time.Second, "export HOME=/home/daytona; "+discoveryScript); err != nil {
					logfCtx(ctx, "[daytona] warning: repo instruction discovery failed for claw %s: %v", clawID, err)
				} else {
					logfCtx(ctx, "[daytona] repo instruction discovery done")
				}
			}
		}
	}

	if err := s.restoreCheckpointToDaytona(ctx, clawID, instanceID, p); err != nil {
		return fmt.Errorf("restore checkpoint: %w", err)
	}

	// Final workspace readiness gate: verify every configured repository is
	// present at the expected path and has a .git directory. Fail fast with a
	// sanitized, actionable bootstrap error instead of starting the agent
	// against an incomplete workspace.
	if len(repositories) > 0 {
		s.setBootstrapStatus(clawID, "Verifying workspace readiness")
		verifyScript := "export HOME=/home/daytona; cd ~/.openclaw/workspace; "
		for _, repo := range repositories {
			verifyScript += daytonaRepoReadinessSnippet(repo.Repo)
		}
		verifyResult, verifyErr := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", verifyScript}, 20*time.Second)
		if verifyErr != nil {
			diag := fmt.Sprintf("Workspace readiness failed: %v", verifyErr)
			s.setBootstrapStatusWithDiagnostic(clawID, "Workspace incomplete", diag)
			return fmt.Errorf("workspace readiness: %w", verifyErr)
		}
		if verifyResult.ExitCode != 0 {
			diag := fmt.Sprintf("Workspace incomplete: required repositories are missing. %s", sanitizeBootstrapOutput(verifyResult.Stdout))
			s.setBootstrapStatusWithDiagnostic(clawID, "Workspace incomplete", diag)
			return fmt.Errorf("workspace readiness failed (exit %d): %s", verifyResult.ExitCode, sanitizeBootstrapOutput(verifyResult.Stdout))
		}
		logfCtx(ctx, "[daytona] workspace readiness verified for claw %s", clawID)
	}

	s.markBootstrapReady(clawID)
	logfCtx(ctx, "[daytona] bootstrap gated ready for claw %s", clawID)
	s.setBootstrapStatus(clawID, "Connecting to hub")

	// Start the bridge last so the first registration happens only after the
	// workspace, template files, GitHub setup, and bootstrap_ok gate are ready.
	// The bridge (and therefore the agent) must run inside the workspace
	// directory so that repo-relative paths resolve correctly.
	if err := s.startDaytonaBridge(ctx, instanceID, p, s.clawHubURL(), clawID, clawToken, clawName); err != nil {
		return err
	}

	logfCtx(ctx, "[daytona] bootstrap complete for claw %s", clawID)
	return nil
}

func (s *Server) startDaytonaBridge(ctx context.Context, instanceID string, p *daytona.Provider, hubURL, clawID, clawToken, clawName string) error {
	prepCmd := daytonaPrepareBridgeCommand()
	result, err := p.ExecWithTimeout(ctx, instanceID, []string{prepCmd}, 15*time.Second)
	if err != nil {
		return fmt.Errorf("start claw-bridge prep: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("start claw-bridge prep failed (exit %d): %s", result.ExitCode, sanitizeBootstrapOutput(result.Stdout))
	}
	if strings.Contains(result.Stdout, "claw-bridge already running") {
		logfCtx(ctx, "[daytona] claw-bridge already running")
		return nil
	}

	const sessionID = "elasticclaw-bridge"
	if err := p.EnsureSession(ctx, instanceID, sessionID); err != nil {
		return fmt.Errorf("start claw-bridge session: %w", err)
	}
	cmdID, err := p.ExecSessionAsync(ctx, instanceID, sessionID, daytonaAsyncBridgeCommand(hubURL, clawID, clawToken, clawName))
	if err != nil {
		return fmt.Errorf("start claw-bridge async: %w", err)
	}
	logfCtx(ctx, "[daytona] claw-bridge async command started session=%s command=%s", sessionID, cmdID)

	verifyCmd := daytonaBridgeRunningCommand()
	var lastVerify string
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			time.Sleep(1 * time.Second)
		}
		result, err := p.ExecWithTimeout(ctx, instanceID, []string{verifyCmd}, 5*time.Second)
		if err != nil {
			lastVerify = err.Error()
			continue
		}
		if result.ExitCode == 0 {
			logfCtx(ctx, "[daytona] start claw-bridge done: %s", strings.TrimSpace(result.Stdout))
			return nil
		}
		lastVerify = result.Stdout
	}
	if result, err := p.ExecWithTimeout(ctx, instanceID, []string{`tail -n 80 /home/daytona/claw-bridge.log 2>/dev/null || true`}, 5*time.Second); err == nil && strings.TrimSpace(result.Stdout) != "" {
		lastVerify = strings.TrimSpace(lastVerify) + "\n" + result.Stdout
	}
	return fmt.Errorf("start claw-bridge verification failed: %s", sanitizeBootstrapOutput(lastVerify))
}

func (s *Server) daytonaBridgeRunning(ctx context.Context, instanceID string, p *daytona.Provider) bool {
	result, err := p.ExecWithTimeout(ctx, instanceID, []string{daytonaBridgeRunningCommand()}, 5*time.Second)
	if err != nil {
		return false
	}
	return result.ExitCode == 0
}

func daytonaBridgeRunningCommand() string {
	return `export HOME=/home/daytona
PIDFILE=/home/daytona/.openclaw/run/claw-bridge.pid
if [ -s "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
  echo "claw-bridge running pid=$(cat "$PIDFILE")"
  exit 0
fi
if pgrep -x claw-bridge >/dev/null 2>&1; then
  echo "claw-bridge running"
  exit 0
fi
echo "claw-bridge not running"
exit 1`
}

func daytonaStartOpenClawInstallCommand(version string) string {
	installScript := fmt.Sprintf(`set -o pipefail
export HOME=/home/daytona
export NVM_DIR=/usr/local/share/nvm
NPM="$NVM_DIR/current/bin/npm"
PREFIX="$("$NPM" config get prefix)"
export PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH"
LOG=/tmp/openclaw-install.log
STATUS=/tmp/openclaw-install.status
echo "npm=$NPM prefix=$PREFIX"
if sudo env PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH" "$NPM" install -g openclaw@%s --prefix "$PREFIX" --ignore-scripts 2>&1; then
  hash -r
  echo ok > "$STATUS"
  echo "install done"
else
  rc=$?
  echo "failed:$rc" > "$STATUS"
  exit "$rc"
fi`, version)
	return fmt.Sprintf(`export HOME=/home/daytona
LOG=/tmp/openclaw-install.log
STATUS=/tmp/openclaw-install.status
rm -f "$LOG" "$STATUS"
setsid nohup bash -c %s > "$LOG" 2>&1 </dev/null &
echo "openclaw-install-status=started"`, shellQuote(installScript))
}

func daytonaInstallCodingModelCLICommand(model string) string {
	var packageSpec, binary string
	switch {
	case strings.HasPrefix(model, "codex/"):
		packageSpec = "@openai/codex@" + cliversion.FromEnv("ELASTICCLAW_CODEX_CLI_VERSION", "0.141.0")
		binary = "codex"
	case strings.HasPrefix(model, "grok/"):
		packageSpec = "@xai-official/grok@" + cliversion.FromEnv("ELASTICCLAW_GROK_CLI_VERSION", "0.1.0")
		binary = "grok"
	default:
		return ""
	}
	return fmt.Sprintf(`export HOME=/home/daytona
export NVM_DIR=/usr/local/share/nvm
NPM="$NVM_DIR/current/bin/npm"
PREFIX="$("$NPM" config get prefix)"
export PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH"
sudo env PATH="$PREFIX/bin:$NVM_DIR/current/bin:/usr/local/bin:$PATH" "$NPM" install -g %s --prefix "$PREFIX" --ignore-scripts
hash -r
%s --version 2>&1 || true`, shellQuote(packageSpec), binary)
}

func daytonaOpenClawInstallStatusCommand(version string) string {
	return fmt.Sprintf(`export HOME=/home/daytona
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
tail -n 120 "$LOG" 2>/dev/null || true`, shellQuote("openclaw@"+version))
}

func daytonaPrepareBridgeCommand() string {
	return `set -e
export HOME=/home/daytona
mkdir -p /home/daytona/.openclaw/workspace /home/daytona/.openclaw/run
cd /home/daytona/.openclaw/workspace
PIDFILE=/home/daytona/.openclaw/run/claw-bridge.pid
if [ -s "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
  echo "claw-bridge already running pid=$(cat "$PIDFILE")"
  exit 0
fi
if pgrep -x claw-bridge >/dev/null 2>&1; then
  echo "claw-bridge already running"
  exit 0
fi
if [ ! -s /tmp/claw-bridge ]; then
  echo "claw-bridge download missing at /tmp/claw-bridge"
  exit 1
fi
sudo install -m 0755 /tmp/claw-bridge /usr/local/bin/claw-bridge
test -x /usr/local/bin/claw-bridge || { echo "claw-bridge installed at /usr/local/bin/claw-bridge is not executable"; exit 1; }
rm -f "$PIDFILE"`
}

func daytonaAsyncBridgeCommand(hubURL, clawID, clawToken, clawName string) string {
	return fmt.Sprintf(`export HOME=/home/daytona
mkdir -p /home/daytona/.openclaw/workspace /home/daytona/.openclaw/run
cd /home/daytona/.openclaw/workspace
PIDFILE=/home/daytona/.openclaw/run/claw-bridge.pid
LOG=/home/daytona/claw-bridge.log
if [ -s "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
  echo "claw-bridge already running pid=$(cat "$PIDFILE")"
  exit 0
fi
if pgrep -x claw-bridge >/dev/null 2>&1; then
  echo "claw-bridge already running"
  exit 0
fi
rm -f "$PIDFILE"
ELASTICCLAW_HUB_URL=%s ELASTICCLAW_CLAW_ID=%s ELASTICCLAW_CLAW_TOKEN=%s ELASTICCLAW_CLAW_NAME=%s \
sh -c 'echo $$ > "$1"; exec /usr/local/bin/claw-bridge' sh "$PIDFILE" >> "$LOG" 2>&1`,
		shellQuote(hubURL),
		shellQuote(clawID),
		shellQuote(clawToken),
		shellQuote(clawName),
	)
}

func (s *Server) downloadDaytonaConnector(ctx context.Context, clawID, instanceID string, p *daytona.Provider, downloadCmd string) error {
	delays := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
	}
	const maxAttempts = 6
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt == 1 {
			s.setBootstrapStatus(clawID, "Downloading ElasticClaw connector")
			logfCtx(ctx, "[daytona] download claw-bridge...")
		} else {
			delay := delays[attempt-2]
			s.setBootstrapStatus(clawID, fmt.Sprintf("Retrying connector download in %s", formatRetryDelay(delay)))
			logfCtx(ctx, "[daytona] download claw-bridge retry %d/%d in %s...", attempt, maxAttempts, delay)
			select {
			case <-ctx.Done():
				return fmt.Errorf("could not download ElasticClaw connector after %d attempts: %w", attempt-1, ctx.Err())
			case <-time.After(delay):
			}
			s.setBootstrapStatus(clawID, "Downloading ElasticClaw connector")
		}

		nvmSetup := `export HOME=/home/daytona; export NVM_DIR=/usr/local/share/nvm; [ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh" && { nvm use 24 >/dev/null 2>&1 || nvm install 24 >/dev/null 2>&1; } ; `
		result, err := p.ExecWithTimeout(ctx, instanceID, []string{"bash", "-c", nvmSetup + downloadCmd}, 3*time.Minute)
		if err != nil {
			lastErr = err
			logfCtx(ctx, "[daytona] download claw-bridge attempt %d/%d failed: %v", attempt, maxAttempts, err)
			continue
		}
		if result.ExitCode != 0 {
			lastErr = fmt.Errorf("exit %d: %s", result.ExitCode, sanitizeBootstrapOutput(result.Stdout))
			logfCtx(ctx, "[daytona] download claw-bridge attempt %d/%d failed: %v", attempt, maxAttempts, lastErr)
			continue
		}

		s.setBootstrapStatus(clawID, "Starting ElasticClaw connector")
		logfCtx(ctx, "[daytona] download claw-bridge done")
		return nil
	}

	return fmt.Errorf("could not download ElasticClaw connector after %d attempts. Last error: %s", maxAttempts, sanitizeBootstrapError(lastErr))
}

func daytonaRepoReadinessSnippet(repoFullName string) string {
	repoName := repoDirectoryName(repoFullName)
	return fmt.Sprintf("echo %s; [ -d %s/.git ] || { echo %s; exit 1; }; echo %s; ",
		shellQuote("[daytona] verifying "+repoName),
		shellQuote(repoName),
		shellQuote("[daytona] verify FAILED: "+repoName+"/.git missing"),
		shellQuote("[daytona] verify OK: "+repoName),
	)
}

func daytonaBootstrapStatusForStep(label string) string {
	switch label {
	case "uninstall old openclaw", "install openclaw", "verify openclaw":
		return "Preparing runtime"
	case "install nix", "install docker", "preflight required commands", "stage openclaw plugin deps":
		return "Preparing runtime"
	case "configure openclaw model", "start openclaw gateway":
		return "Configuring OpenClaw"
	case "install git credential helper", "install gh cli", "configure gh token refresh":
		return "Preparing repository access"
	case "write SOUL.md", "write AGENTS.md", "write BOOTSTRAP.md", "write CONTEXT.md":
		return "Preparing workspace"
	default:
		if strings.HasPrefix(label, "write ") {
			return "Preparing workspace"
		}
		return "Preparing sandbox"
	}
}

func (s *Server) keepAliveDaytonaSandboxes() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.petDaytonaSandboxes()
	}
}

func (s *Server) petDaytonaSandboxes() {
	rows, err := s.db.Query(`
		SELECT id, name, provider_id
		FROM claws
		WHERE provider = 'daytona'
		  AND provider_id != ''
		  AND status NOT IN ('idle','deleted','error','offline')
	`)
	if err != nil {
		logf("keepAliveDaytonaSandboxes: query error: %v", err)
		return
	}
	defer rows.Close()

	type clawRow struct{ id, name, providerID string }
	var claws []clawRow
	for rows.Next() {
		var c clawRow
		if err := rows.Scan(&c.id, &c.name, &c.providerID); err == nil {
			claws = append(claws, c)
		}
	}
	if len(claws) == 0 {
		return
	}

	s.cfgMu.RLock()
	cfg, ok := s.hubCfg.Providers["daytona"]
	s.cfgMu.RUnlock()
	if !ok {
		logf("keepAliveDaytonaSandboxes: no daytona provider configured")
		return
	}
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		logf("keepAliveDaytonaSandboxes: provider init error: %v", err)
		return
	}

	for _, c := range claws {
		ctx, cancel := context.WithTimeout(s.base(), 30*time.Second)
		_, err := p.ExecWithTimeout(ctx, c.providerID, []string{"bash", "-lc", "true"}, 20*time.Second)
		cancel()
		if err != nil {
			logf("[daytona] keepalive failed for %s (%s): %v", c.name, c.id[:8], err)
			continue
		}
		logf("[daytona] keepalive ok for %s (%s)", c.name, c.id[:8])
	}
}

// terminateDaytonaVM destroys a Daytona workspace by ID.
func (s *Server) terminateDaytonaVM(workspaceID string) {
	s.cfgMu.RLock()
	cfg, ok := s.hubCfg.Providers["daytona"]
	s.cfgMu.RUnlock()
	if !ok {
		return
	}
	p, err := newDaytonaProvider(cfg)
	if err != nil {
		logf("terminateDaytonaVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(s.base(), "destroy", "daytona")
	err = p.Destroy(destroyCtx, workspaceID, false)
	endSpan(err)
	if err != nil {
		logf("terminateDaytonaVM: failed to destroy workspace %s: %v", workspaceID, err)
		return
	}
	logf("Daytona workspace %s terminated", workspaceID)
}
