// Docker and Lambda MicroVMs provisioning and teardown.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/telemetry"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func (s *Server) provisionDocker(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error {
	p, err := newDockerProvider(cfg)
	if err != nil {
		return fmt.Errorf("docker init: %w", err)
	}

	// Load claw configuration from DB
	var clawName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,'') FROM claws WHERE id=?`,
		clawID,
	).Scan(&clawName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName); err != nil {
		return fmt.Errorf("load claw config: %w", err)
	}

	s.cfgMu.RLock()
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.cfgMu.RUnlock()

	linearToken := resolveLinearToken(hubCfg, linearWorkspace)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = hubCfg.DefaultModel
	}

	gatewayPassword := randomHex(16)
	providerConfig := buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName)
	apiKeyAuthSync := buildOpenClawAPIKeyAuthSyncShell(hubCfg.LLMKeys, llmKeyName)
	onboardFlags := buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel)

	// Build env map for the container — passed directly as -e flags (no shell escaping needed)
	containerEnv := map[string]string{
		"ELASTICCLAW_HUB_URL":            dockerClawHubURL(hubCfg),
		"ELASTICCLAW_CLAW_ID":            clawID,
		"ELASTICCLAW_CLAW_TOKEN":         clawToken,
		"ELASTICCLAW_CLAW_NAME":          clawName,
		"ELASTICCLAW_GITHUB_REPOS":       githubReposJSON,
		"ELASTICCLAW_BOOTSTRAP":          "1",
		"ELASTICCLAW_WAIT_FOR_WORKSPACE": "1",
		"ELASTICCLAW_GATEWAY_PASSWORD":   gatewayPassword,
		"OPENCLAW_GATEWAY_PASSWORD":      gatewayPassword,
		"OPENCLAW_DEFAULT_MODEL":         defaultModel,
		"ELASTICCLAW_NIX":                boolEnv(nixEnabled != 0),
		"ELASTICCLAW_DOCKER":             boolEnv(dockerEnabled != 0),
		"ELASTICCLAW_PROVIDER_CONFIG":    providerConfig,
		"ELASTICCLAW_API_KEY_AUTH_SYNC":  apiKeyAuthSync,
		"ELASTICCLAW_ONBOARD_FLAGS":      onboardFlags,
	}

	// Inject LLM keys: buildLLMKeyEnv returns "export VAR=val\n" lines — parse into k/v
	for _, line := range strings.Split(llmKeyEnv+modelAuthEnv, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "export ") {
			continue
		}
		kv := strings.TrimPrefix(line, "export ")
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			k := kv[:idx]
			v := kv[idx+1:]
			if unquoted, err := strconv.Unquote(v); err == nil {
				v = unquoted
			} else if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
				v = v[1 : len(v)-1]
			}
			containerEnv[k] = v
		}
	}

	// Inject LINEAR_API_KEY if configured
	if linearToken != "" {
		containerEnv["LINEAR_API_KEY"] = linearToken
	}

	createReq := types.CreateRequest{
		Name: req.ProviderName,
		Env:  containerEnv,
	}

	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "docker")
	instance, err := p.Create(createCtx, createReq)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("docker create: %w", err)
	}
	logfCtx(ctx, "[docker] container started: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='docker', provider_id=? WHERE id=?`, instance.ID, clawID)
	homeDir, err := p.HomeDir(ctx, instance.ID)
	if err != nil {
		_ = p.Destroy(s.base(), instance.ID, false) //nolint:contextcheck // cleanup must run even if the caller ctx is canceled
		return fmt.Errorf("docker home dir: %w", err)
	}
	workspaceDir := path.Join(homeDir, "workspace")
	workspacePrefix := strings.TrimRight(workspaceDir, "/") + "/"

	s.setBootstrapStatus(clawID, "Copying workspace files")
	for relPath, content := range files {
		dest := path.Join(workspaceDir, relPath)
		if dest != workspaceDir && !strings.HasPrefix(dest, workspacePrefix) {
			_ = p.Destroy(s.base(), instance.ID, false) //nolint:contextcheck // cleanup must run even if the caller ctx is canceled
			return fmt.Errorf("docker workspace file path escapes workspace: %s", relPath)
		}
		if err := p.CopyIn(ctx, instance.ID, dest, content); err != nil {
			_ = p.Destroy(s.base(), instance.ID, false) //nolint:contextcheck // cleanup must run even if the caller ctx is canceled
			return fmt.Errorf("docker file copy failed: %s: %w", relPath, err)
		}
	}
	if err := p.CopyIn(ctx, instance.ID, path.Join(workspaceDir, ".elasticclaw-workspace-ready"), []byte("ready\n")); err != nil {
		_ = p.Destroy(s.base(), instance.ID, false) //nolint:contextcheck // cleanup must run even if the caller ctx is canceled
		return fmt.Errorf("docker workspace ready marker: %w", err)
	}
	logfCtx(ctx, "[docker] workspace files copied for claw %.8s to %s", clawID, workspaceDir)
	s.setBootstrapStatus(clawID, "Starting agent bridge")
	if err := s.ensureDockerBridge(ctx, p, instance.ID, homeDir); err != nil {
		_ = p.Destroy(s.base(), instance.ID, false) //nolint:contextcheck // cleanup must run even if the caller ctx is canceled
		return err
	}

	return nil
}

func dockerClawHubURL(cfg *types.HubConfig) string {
	if cfg == nil {
		return ""
	}
	hubURL := cfg.PublicURL
	if cfg.URL != "" {
		hubURL = cfg.URL
	}
	parsed, err := url.Parse(hubURL)
	if err != nil || parsed.Hostname() == "" {
		return strings.TrimRight(hubURL, "/")
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "0.0.0.0", "::1":
		port := parsed.Port()
		parsed.Host = "host.docker.internal"
		if port != "" {
			parsed.Host += ":" + port
		}
		return strings.TrimRight(parsed.String(), "/")
	default:
		return strings.TrimRight(hubURL, "/")
	}
}

const maxDockerBridgeBinaryBytes = 200 << 20

func (s *Server) ensureDockerBridge(ctx context.Context, p interface {
	CopyIn(context.Context, string, string, []byte) error
	Exec(context.Context, string, []string) (*types.ExecResult, error)
}, containerID, homeDir string) error {
	if _, err := p.Exec(ctx, containerID, []string{"sh", "-lc", "command -v pgrep >/dev/null 2>&1 && pgrep -x claw-bridge >/dev/null"}); err == nil {
		logfCtx(ctx, "[docker] claw-bridge already running in container %s", containerID)
		return nil
	}

	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		return fmt.Errorf("claw-bridge URL not configured: set bridge_image in hub.yaml or build a tagged release")
	}
	if !strings.HasPrefix(bridgeURL, "http://") && !strings.HasPrefix(bridgeURL, "https://") {
		return fmt.Errorf("docker provider requires an HTTP(S) claw-bridge URL, got %q", bridgeURL)
	}
	bridgeBytes, err := downloadDockerBridgeBinary(ctx, bridgeURL)
	if err != nil {
		return err
	}
	bridgePath := path.Join(homeDir, ".elasticclaw", "bin", "claw-bridge")
	if err := p.CopyIn(ctx, containerID, bridgePath, bridgeBytes); err != nil {
		return fmt.Errorf("docker claw-bridge copy failed: %w", err)
	}
	logPath := path.Join(homeDir, "claw-bridge.log")
	startCmd := fmt.Sprintf(
		"set -e; chmod 0755 %s; nohup %s >> %s 2>&1 </dev/null & echo started",
		shellQuote(bridgePath),
		shellQuote(bridgePath),
		shellQuote(logPath),
	)
	if _, err := p.Exec(ctx, containerID, []string{"sh", "-lc", startCmd}); err != nil {
		return fmt.Errorf("docker claw-bridge start failed: %w", err)
	}
	logfCtx(ctx, "[docker] claw-bridge started in container %s", containerID)
	return nil
}

func downloadDockerBridgeBinary(ctx context.Context, bridgeURL string) ([]byte, error) {
	if bridgePath := os.Getenv("ELASTICCLAW_E2E_BRIDGE_BINARY"); bridgePath != "" && strings.Contains(bridgeURL, "/__elasticclaw_e2e/claw-bridge-linux-amd64") {
		data, err := os.ReadFile(bridgePath)
		if err != nil {
			return nil, fmt.Errorf("docker claw-bridge read local E2E binary: %w", err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("docker claw-bridge local E2E binary is empty")
		}
		if len(data) > maxDockerBridgeBinaryBytes {
			return nil, fmt.Errorf("docker claw-bridge local E2E binary exceeds %d bytes", maxDockerBridgeBinaryBytes)
		}
		return data, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bridgeURL, nil)
	if err != nil {
		return nil, fmt.Errorf("docker claw-bridge download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker claw-bridge download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("docker claw-bridge download failed: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDockerBridgeBinaryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("docker claw-bridge read: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("docker claw-bridge download returned an empty body")
	}
	if len(data) > maxDockerBridgeBinaryBytes {
		return nil, fmt.Errorf("docker claw-bridge download exceeds %d bytes", maxDockerBridgeBinaryBytes)
	}
	return data, nil
}

func (s *Server) provisionLambdaMicroVMs(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte) error {
	p, err := newLambdaMicroVMsProvider(cfg)
	if err != nil {
		return fmt.Errorf("lambda microvms init: %w", err)
	}

	var clawName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(
		`SELECT COALESCE(name,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,'') FROM claws WHERE id=?`,
		clawID,
	).Scan(&clawName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName); err != nil {
		return fmt.Errorf("load claw config: %w", err)
	}

	s.cfgMu.RLock()
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.cfgMu.RUnlock()

	linearToken := resolveLinearToken(hubCfg, linearWorkspace)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = hubCfg.DefaultModel
	}
	providerConfig := buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName)
	apiKeyAuthSync := buildOpenClawAPIKeyAuthSyncShell(hubCfg.LLMKeys, llmKeyName)
	onboardFlags := buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel)
	gatewayPassword := randomHex(16)

	env := map[string]string{
		"ELASTICCLAW_HUB_URL":            s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":            clawID,
		"ELASTICCLAW_CLAW_TOKEN":         clawToken,
		"ELASTICCLAW_CLAW_NAME":          clawName,
		"ELASTICCLAW_GITHUB_REPOS":       githubReposJSON,
		"ELASTICCLAW_BOOTSTRAP":          "1",
		"ELASTICCLAW_WAIT_FOR_WORKSPACE": "1",
		"ELASTICCLAW_GATEWAY_PASSWORD":   gatewayPassword,
		"OPENCLAW_GATEWAY_PASSWORD":      gatewayPassword,
		"OPENCLAW_DEFAULT_MODEL":         defaultModel,
		"ELASTICCLAW_NIX":                boolEnv(nixEnabled != 0),
		"ELASTICCLAW_DOCKER":             boolEnv(dockerEnabled != 0),
		"ELASTICCLAW_PROVIDER_CONFIG":    providerConfig,
		"ELASTICCLAW_API_KEY_AUTH_SYNC":  apiKeyAuthSync,
		"ELASTICCLAW_ONBOARD_FLAGS":      onboardFlags,
	}
	for _, line := range strings.Split(llmKeyEnv+modelAuthEnv, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "export ") {
			continue
		}
		kv := strings.TrimPrefix(line, "export ")
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			k := kv[:idx]
			v := kv[idx+1:]
			if unquoted, err := strconv.Unquote(v); err == nil {
				v = unquoted
			} else if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
				v = v[1 : len(v)-1]
			}
			env[k] = v
		}
	}
	if linearToken != "" {
		env["LINEAR_API_KEY"] = linearToken
	}
	for k, v := range req.Env {
		if _, exists := env[k]; !exists {
			env[k] = v
		}
	}

	createReq := types.CreateRequest{
		Name:          req.ProviderName,
		Env:           env,
		TemplateFiles: files,
	}
	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "lambda-microvms")
	instance, err := p.Create(createCtx, createReq)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("lambda microvms create: %w", err)
	}
	logfCtx(ctx, "[lambda-microvms] microvm started: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='lambda-microvms', provider_id=? WHERE id=?`, instance.ID, clawID)
	return nil
}

// boolEnv converts a bool to "true"/"false" for environment variable injection.
func boolEnv(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// terminateDockerVM destroys a Docker agent container by name/ID.
func (s *Server) terminateDockerVM(vmID string) {
	s.cfgMu.RLock()
	cfg, ok := s.hubCfg.Providers["docker"]
	s.cfgMu.RUnlock()
	if !ok {
		logf("terminateDockerVM: no docker provider configured")
		return
	}
	p, err := newDockerProvider(cfg)
	if err != nil {
		logf("terminateDockerVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(s.base(), "destroy", "docker")
	err = p.Destroy(destroyCtx, vmID, false)
	endSpan(err)
	if err != nil {
		logf("terminateDockerVM: failed to destroy container %s: %v", vmID, err)
		return
	}
	logf("Docker container %s terminated", vmID)
}

// terminateLambdaMicroVM destroys an AWS Lambda MicroVM by ID.
func (s *Server) terminateLambdaMicroVM(vmID string) {
	s.cfgMu.RLock()
	cfg, ok := s.hubCfg.Providers["lambda-microvms"]
	s.cfgMu.RUnlock()
	if !ok {
		logf("terminateLambdaMicroVM: no lambda-microvms provider configured")
		return
	}
	p, err := newLambdaMicroVMsProvider(cfg)
	if err != nil {
		logf("terminateLambdaMicroVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(s.base(), "destroy", "lambda-microvms")
	err = p.Destroy(destroyCtx, vmID, false)
	endSpan(err)
	if err != nil {
		logf("terminateLambdaMicroVM: failed to destroy MicroVM %s: %v", vmID, err)
		return
	}
	logf("Lambda MicroVM %s terminated", vmID)
}
