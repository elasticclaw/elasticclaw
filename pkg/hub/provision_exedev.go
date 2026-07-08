// Exedev provisioning and teardown.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/telemetry"
	exedevProvider "github.com/elasticclaw/elasticclaw/pkg/provider/exedev"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func (s *Server) provisionExedev(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, files map[string][]byte, env map[string]string) error {
	p, err := newExedevProvider(cfg)
	if err != nil {
		return fmt.Errorf("exedev init: %w", err)
	}

	createReq := types.CreateRequest{
		Name:          req.ProviderName,
		TemplateFiles: files,
		Env:           env,
	}
	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "exedev")
	instance, err := p.Create(createCtx, createReq)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("exedev create: %w", err)
	}
	logfCtx(ctx, "exedev VM created: %s (claw %s)", instance.ID, clawID)
	_, _ = s.db.Exec(`UPDATE claws SET status='starting', provider='exedev', provider_id=? WHERE id=?`, instance.ID, clawID)

	// Bootstrap asynchronously
	go func() {
		if err := s.bootstrapExedev(context.Background(), clawID, instance.ID, p, files); err != nil {
			logfCtx(ctx, "exedev bootstrap failed for claw %s: %v", clawID, err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Exedev bootstrap failed: %s", sanitizeBootstrapError(err)), false)
		}
	}()

	return nil
}

func (s *Server) bootstrapExedev(ctx context.Context, clawID, vmName string, p *exedevProvider.Provider, files map[string][]byte) error {
	logfCtx(ctx, "[exedev] bootstrapping claw %s (vm %s)", clawID, vmName)
	s.setBootstrapStatus(clawID, "Waiting for sandbox SSH")

	// Wait for VM to be reachable
	host := vmName + ".exe.xyz"
	reachable := false
	for i := 0; i < 30; i++ {
		sshArgs := []string{"-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=no"}
		if p.SSHKeyPath() != "" {
			sshArgs = append(sshArgs, "-i", p.SSHKeyPath())
		}
		sshArgs = append(sshArgs, host, "echo ready")
		cmd := exec.CommandContext(ctx, "ssh", sshArgs...)
		if err := cmd.Run(); err == nil {
			reachable = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !reachable {
		return fmt.Errorf("exedev VM %s was not reachable via SSH after 150s", vmName)
	}
	s.setBootstrapStatus(clawID, "Preparing ElasticClaw connector")

	// Load claw configuration from DB in a single atomic query
	var clawName, githubReposJSON, linearWorkspace, templateDefaultModel, llmKeyName, templateFilesJSON string
	var nixEnabled, dockerEnabled int
	if err := s.db.QueryRow(`SELECT COALESCE(name,''), COALESCE(github_repos,'[]'), COALESCE(linear_workspace,''), COALESCE(default_model,''), nix, docker, COALESCE(llm_key,''), COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(
		&clawName, &githubReposJSON, &linearWorkspace, &templateDefaultModel, &nixEnabled, &dockerEnabled, &llmKeyName, &templateFilesJSON,
	); err != nil {
		return fmt.Errorf("load claw config: %w", err)
	}
	var githubRepos []types.GitHubRepoAccess
	_ = json.Unmarshal([]byte(githubReposJSON), &githubRepos)
	var templateFiles map[string]string
	_ = json.Unmarshal([]byte(templateFilesJSON), &templateFiles)
	templateFiles = workspaceTemplateFiles(templateFiles)

	s.mu.RLock()
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.mu.RUnlock()

	linearToken := resolveLinearToken(hubCfg, linearWorkspace)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = hubCfg.DefaultModel
	}
	logfCtx(ctx, "[exedev bootstrap] claw %.8s nix=%d docker=%d llm_key=%q template_default_model=%q hub_default_model=%q resolved_default_model=%q",
		clawID, nixEnabled, dockerEnabled, llmKeyName, templateDefaultModel, hubCfg.DefaultModel, defaultModel)

	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		return fmt.Errorf("claw-bridge URL not configured: set bridge_image in hub.yaml or build a tagged release")
	}

	// Generate a random gateway password for this VM
	gatewayPassword := randomHex(16)

	// Build bootstrap script using same pattern as replicated
	script := GenerateReplicatedBootstrapScript(BootstrapParams{
		ClawID:          clawID,
		ClawName:        clawName,
		ClawToken:       clawToken,
		HubURL:          s.clawHubURL(),
		DefaultModel:    defaultModel,
		GatewayPassword: gatewayPassword,
		BridgeURL:       bridgeURL,
		Nix:             nixEnabled != 0,
		Docker:          dockerEnabled != 0,
		TemplateFiles:   templateFiles,
		HubCfg:          hubCfg,
		GitHubRepos:     githubRepos,
		LLMKeyEnv:       llmKeyEnv,
		ModelAuthEnv:    modelAuthEnv,
		APIKeyAuthSync:  buildOpenClawAPIKeyAuthSyncShell(hubCfg.LLMKeys, llmKeyName),
		LinearEnv:       buildLinearEnv(linearToken),
		ProviderConfig:  buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName),
		OnboardFlags:    buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel),
	})

	if flakeFiles := templateFlakeFiles(templateFiles); len(flakeFiles) > 0 {
		if _, err := p.Exec(ctx, vmName, []string{"mkdir", "-p", "~/workspace"}); err != nil {
			return fmt.Errorf("create flake staging dir: %w", err)
		}
		for path, content := range flakeFiles {
			if err := p.WriteFile(ctx, vmName, "~/workspace/"+path, []byte(content)); err != nil {
				return fmt.Errorf("stage %s before bootstrap: %w", path, err)
			}
		}
	}

	// Run bootstrap script — this installs Node.js, OpenClaw, and starts claw-bridge
	if err := p.SetupScript(ctx, vmName, script); err != nil {
		return fmt.Errorf("exedev bootstrap script failed: %s", sanitizeBootstrapError(err))
	}
	logfCtx(ctx, "[exedev] bootstrap script completed on %s", vmName)
	s.setBootstrapStatus(clawID, "Writing workspace files")

	// Write template files after bootstrap so openclaw onboard doesn't overwrite them
	workdir := "~/workspace"
	if _, err := p.Exec(ctx, vmName, []string{"mkdir", "-p", workdir}); err != nil {
		return fmt.Errorf("create workdir: %w", err)
	}
	var writeErrs []string
	for path, content := range files {
		fullPath := workdir + "/" + path
		if err := p.WriteFile(ctx, vmName, fullPath, content); err != nil {
			writeErrs = append(writeErrs, fmt.Sprintf("%s: %v", path, err))
		}
	}
	if len(writeErrs) > 0 {
		return fmt.Errorf("template file staging failed: %s", strings.Join(writeErrs, "; "))
	}
	if err := s.restoreCheckpointToExedev(ctx, clawID, vmName, p); err != nil {
		return fmt.Errorf("restore checkpoint: %w", err)
	}
	if credHelper := buildGitHubCredentialHelper(hubCfg, s.clawHubURL(), clawID, githubRepos); credHelper != "# GitHub App not configured — skipping credential helper" {
		if err := p.SetupScript(ctx, vmName, credHelper); err != nil {
			return fmt.Errorf("configure GitHub credentials and repo instructions: %w", err)
		}
		logfCtx(ctx, "[exedev] GitHub credential helper and repo instruction discovery completed for claw %.8s", clawID)
	}
	s.markBootstrapReady(clawID)

	logfCtx(ctx, "[exedev] bootstrap complete for claw %.8s on %s", clawID, vmName)
	return nil
}

// terminateExedevVM destroys an exedev VM by ID.
func (s *Server) terminateExedevVM(vmID string) {
	s.mu.RLock()
	cfg, ok := s.hubCfg.Providers["exedev"]
	s.mu.RUnlock()
	if !ok {
		logf("terminateExedevVM: no exedev provider configured")
		return
	}

	logf("terminateExedevVM: destroying VM %s (ssh_key_path=%q)", vmID, cfg.SSHKeyPath)
	p, err := newExedevProvider(cfg)
	if err != nil {
		logf("terminateExedevVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(context.Background(), "destroy", "exedev")
	err = p.Destroy(destroyCtx, vmID, false)
	endSpan(err)
	if err != nil {
		logf("terminateExedevVM: failed to destroy VM %s: %v", vmID, err)
		return
	}
	logf("Exedev VM %s terminated", vmID)
}
