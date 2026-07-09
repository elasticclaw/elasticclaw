// Replicated provisioning: VM bootstrap, sync, and retry helpers.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/telemetry"
	replicatedpkg "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
)

type replicatedBootstrapRetryOptions struct {
	Label      string
	RetryLabel string
	Attempts   int
	Delays     []time.Duration
	Sleep      func(time.Duration)
	Run        func() error
}

func retryReplicatedBootstrapStep(s *Server, clawID string, opts replicatedBootstrapRetryOptions) error {
	if opts.Attempts < 1 {
		opts.Attempts = 1
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	if opts.RetryLabel == "" {
		opts.RetryLabel = "Retrying " + strings.ToLower(opts.Label)
	}

	var lastErr error
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if attempt > 1 {
			delay := replicatedBootstrapDelay(opts.Delays, attempt-2)
			if s != nil && clawID != "" {
				s.setBootstrapStatus(clawID, fmt.Sprintf("%s in %s", opts.RetryLabel, formatRetryDelay(delay)))
			}
			logf("[bootstrap] %s retry %d/%d in %s...", opts.Label, attempt, opts.Attempts, delay)
			opts.Sleep(delay)
		}
		if s != nil && clawID != "" {
			s.setBootstrapStatus(clawID, opts.Label)
		}
		if err := opts.Run(); err != nil {
			lastErr = err
			logf("[bootstrap] %s attempt %d/%d failed: %s", opts.Label, attempt, opts.Attempts, sanitizeBootstrapError(err))
			continue
		}
		return nil
	}

	return fmt.Errorf("%s failed after %d attempts: %s", opts.Label, opts.Attempts, sanitizeBootstrapError(lastErr))
}

func replicatedBootstrapDelay(delays []time.Duration, idx int) time.Duration {
	if len(delays) == 0 {
		return 5 * time.Second
	}
	if idx < len(delays) {
		return delays[idx]
	}
	return delays[len(delays)-1]
}

func replicatedWorkspaceReadinessCommand(dir string, files map[string]string) string {
	if len(files) == 0 {
		return "true"
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("set -e\n")
	for _, name := range names {
		remotePath := strings.TrimRight(dir, "/") + "/" + name
		b.WriteString("test -e ")
		b.WriteString(shellDoubleQuote(remotePath))
		b.WriteString(" || { echo ")
		b.WriteString(shellQuote("missing workspace file: " + name))
		b.WriteString("; exit 1; }\n")
	}
	b.WriteString("echo 'workspace files verified'\n")
	return b.String()
}

func (s *Server) provisionReplicated(ctx context.Context, clawID string, req types.CreateClawRequest, cfg types.ProviderConfig, env map[string]string) error {
	// Hub's generated key is always included; append any extra debug keys from hub config.
	cfg.SSHPublicKey = s.identity.PublicKey
	cfg.ExtraSSHPublicKeys = s.hubCfg.SSHPublicKeys
	p, err := newReplicatedProvider(cfg)
	if err != nil {
		return fmt.Errorf("replicated init: %w", err)
	}

	createCtx, endSpan := telemetry.StartProviderSpan(ctx, "create", "replicated")
	vmID, err := p.ProvisionClaw(createCtx, replicatedpkg.VMCreateRequest{
		Name:         req.ProviderName, // stable ec-<shortid>
		InstanceType: req.InstanceType,
		TTL:          req.TTL,
	}, nil, nil)
	endSpan(err)
	if err != nil {
		return fmt.Errorf("replicated provision: %w", err)
	}
	recordE2EReplicatedVMID(vmID)
	// Store vm_id in the claw record — keep status='provisioning' so the poller can detect
	// the provisioning→running transition and trigger bootstrap. Skip if already deleted.
	_, _ = s.db.Exec(
		`UPDATE claws SET provider='replicated', provider_id=? WHERE id=? AND status NOT IN ('deleted','starting','connected','idle')`, vmID, clawID,
	)
	// If deleted, clean up the VM and bail
	var currentStatus string
	_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
	if currentStatus == "deleted" {
		logfCtx(ctx, "[provision] claw %s deleted mid-provision, destroying VM %s", clawID[:8], vmID)
		_ = p.DeleteVM(ctx, vmID)
		return fmt.Errorf("claw deleted mid-provision")
	}

	instanceType := req.InstanceType
	if instanceType == "" {
		instanceType = cfg.DefaultInstanceType
		if instanceType == "" {
			instanceType = replicatedpkg.DefaultInstanceType
		}
	}
	ttl := req.TTL
	if ttl == "" {
		ttl = cfg.DefaultTTL
		if ttl == "" {
			ttl = replicatedpkg.DefaultTTL
		}
	}

	logfCtx(ctx, "Replicated VM provisioned")
	logfCtx(ctx, "  Claw:          %s (%s)", req.Name, clawID)
	logfCtx(ctx, "  VM ID:         %s", vmID)
	logfCtx(ctx, "  Instance type: %s", instanceType)
	logfCtx(ctx, "  TTL:           %s", ttl)
	logfCtx(ctx, "  SSH:           ssh %s", replicatedpkg.VMHostname(vmID))
	logfCtx(ctx, "  Status:        provisioning (waiting for VM to start)")
	return nil
}

func (s *Server) syncReplicatedVMs() {
	s.cfgMu.RLock()
	replicatedCfg, ok := s.hubCfg.Providers["replicated"]
	s.cfgMu.RUnlock()
	if !ok || replicatedCfg.Token == "" {
		return
	}

	// Find claws provisioned on Replicated that are still in a VM-managed state.
	// Exclude hub-managed statuses (idle, connected) — those claws don't need VM polling.
	rows, err := s.db.Query(`
		SELECT id, tenant_id, name, provider_id, status
		FROM claws
		WHERE provider = 'replicated'
		  AND provider_id != ''
		  AND status IN ('provisioning', 'starting')
	`)
	if err != nil {
		logf("pollProviderStatus: query error: %v", err)
		return
	}
	defer rows.Close()

	type clawRow struct {
		id, tenantID, name, providerID, status string
	}
	var pending []clawRow
	for rows.Next() {
		var c clawRow
		if err := rows.Scan(&c.id, &c.tenantID, &c.name, &c.providerID, &c.status); err != nil {
			continue
		}
		pending = append(pending, c)
	}
	rows.Close()

	if len(pending) == 0 {
		return
	}

	p, err := newReplicatedProvider(replicatedCfg)
	if err != nil {
		logf("pollProviderStatus: provider init error: %v", err)
		return
	}

	for _, c := range pending {
		vm, err := p.GetVM(context.Background(), c.providerID)
		if err != nil {
			// 404 means VM was deleted externally — clean up the claw
			if strings.Contains(err.Error(), "HTTP 404") {
				logf("pollProviderStatus: VM %s not found (404) — marking claw %s offline", c.providerID, c.id[:8])
				res, execErr := s.db.Exec(
					`UPDATE claws SET status='offline' WHERE id=? AND status IN ('provisioning','starting')`,
					c.id)
				if execErr == nil {
					if n, _ := res.RowsAffected(); n > 0 {
						s.clawReg.Do(func(conns map[string]*clawConn) {
							if cc, ok := conns[c.id]; ok {
								cc.WS.Close(websocket.StatusGoingAway, "VM not found")
								delete(conns, c.id)
							}
						})
						s.broadcastToUsers(c.tenantID, types.WSMessage{
							Type:    "claw_status",
							Payload: map[string]string{"claw_id": c.id, "status": "offline"},
						})
					}
				}
			} else {
				logf("pollProviderStatus: get VM %s error: %v", c.providerID, err)
			}
			continue
		}
		// Only log if status changed or there's a problem
		if vm.Status != c.status && vm.Status != "running" {
			logf("Claw %s (%s): VM %s %s → %s", c.name, c.id[:8], c.providerID, c.status, vm.Status)
		}

		// Map Replicated VM status to claw status
		var newStatus string
		switch vm.Status {
		case "running":
			newStatus = "starting"
			// First time we see running — trigger bootstrap
			if c.status == "provisioning" {
				logf("Claw %s (%s): VM running, bootstrapping...", c.name, c.id[:8])
				go s.bootstrapReplicated(c.id, c.name, c.providerID, replicatedCfg)
			}
		case "terminated", "error":
			logf("Replicated VM %s for claw %s (%s) terminated", c.providerID, c.name, c.id)
			go s.stopAgentWithReason(c.id, "Sandbox terminated (TTL expired or external shutdown)", true)
			// Note: stopAgentWithReason handles disconnect, status, broadcast, VM cleanup
			// Spawned in goroutine so slow issue-tracker APIs don't stall the poll loop.
			// Skip the rest of the status update logic for this claw
			continue
		default:
			// assigned, pending, etc — still coming up
			newStatus = "provisioning"
		}

		// Only overwrite provisioning/starting statuses — never clobber hub-managed
		// statuses (idle, connected, deleted, error) which have higher semantic meaning.
		// Use a conditional UPDATE so we race-safely check the current DB value.
		if newStatus != c.status {
			res, execErr := s.db.Exec(
				`UPDATE claws SET status=? WHERE id=? AND status IN ('provisioning','starting')`,
				newStatus, c.id)
			if execErr == nil {
				if n, _ := res.RowsAffected(); n > 0 {
					logf("Claw %s (%s): VM %s %s → hub status %s",
						c.name, c.id[:8], c.providerID, vm.Status, newStatus)
					s.broadcastToUsers(c.tenantID, types.WSMessage{
						Type:    "claw_status",
						Payload: map[string]string{"claw_id": c.id, "status": newStatus},
					})
				}
			}
		}
	}
}

// bootstrapReplicated SSHes into a newly-running Replicated VM, pulls the
// claw-bridge binary from GitHub Releases, and starts it with hub connection env vars.
func (s *Server) bootstrapReplicated(clawID, clawName, vmID string, cfg types.ProviderConfig) {
	s.setBootstrapStatus(clawID, "Preparing ElasticClaw workspace")
	// Bail immediately if claw was deleted while VM was spinning up
	var checkStatus string
	_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&checkStatus)
	if checkStatus == "deleted" {
		logf("[bootstrap] claw %s deleted before bootstrap, destroying VM %s", clawID[:8], vmID)
		p, _ := newReplicatedProvider(cfg)
		if p != nil {
			_ = p.DeleteVM(context.Background(), vmID)
		}
		return
	}

	var filesJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(template_files,'{}') FROM claws WHERE id=?`, clawID).Scan(&filesJSON)
	var files map[string]string
	_ = json.Unmarshal([]byte(filesJSON), &files)

	// Load github repos config for this claw
	var githubReposJSON string
	_ = s.db.QueryRow(`SELECT COALESCE(github_repos,'[]') FROM claws WHERE id=?`, clawID).Scan(&githubReposJSON)
	var githubRepos []types.GitHubRepoAccess
	_ = json.Unmarshal([]byte(githubReposJSON), &githubRepos)

	// Resolve Linear token for this claw
	var linearWorkspace string
	_ = s.db.QueryRow(`SELECT COALESCE(linear_workspace,'') FROM claws WHERE id=?`, clawID).Scan(&linearWorkspace)
	linearToken := resolveLinearToken(s.hubCfg, linearWorkspace)
	// Resolve model: template override wins over hub default
	var templateDefaultModel string
	_ = s.db.QueryRow(`SELECT COALESCE(default_model,'') FROM claws WHERE id=?`, clawID).Scan(&templateDefaultModel)
	defaultModel := templateDefaultModel
	if defaultModel == "" {
		defaultModel = s.hubCfg.DefaultModel
	}
	// Read nix flag
	var nixEnabled int
	if err := s.db.QueryRow(`SELECT nix FROM claws WHERE id=?`, clawID).Scan(&nixEnabled); err != nil {
		logf("[bootstrap] warning: could not read nix flag for claw %s: %v", clawID[:8], err)
	}
	var dockerEnabled int
	if err := s.db.QueryRow(`SELECT docker FROM claws WHERE id=?`, clawID).Scan(&dockerEnabled); err != nil {
		logf("[bootstrap] warning: could not read docker flag for claw %s: %v", clawID[:8], err)
	}
	logf("[bootstrap] claw %s nix=%d docker=%d", clawID[:8], nixEnabled, dockerEnabled)
	// Read llm_key selection
	var llmKeyName string
	_ = s.db.QueryRow(`SELECT COALESCE(llm_key,'') FROM claws WHERE id=?`, clawID).Scan(&llmKeyName)
	defaultModel, llmKeyName = resolveModelAndLLMKey(s.hubCfg, llmKeyName, defaultModel)
	logf("[bootstrap] OpenClaw model resolution claw=%s llm_key=%q template_default_model=%q hub_default_model=%q resolved_default_model=%q",
		clawID[:8], llmKeyName, templateDefaultModel, s.hubCfg.DefaultModel, defaultModel)

	bridgeURL := s.bridgeDownloadURL()
	if bridgeURL == "" {
		logf("[bootstrap] ERROR: bridge_image not set and hub version is 'dev' — set bridge_image in hub.yaml")
		s.stopAgentWithReason(clawID, "Bootstrap failed: bridge_image not configured", false)
		return
	}
	s.setBootstrapStatus(clawID, "Waiting for sandbox SSH")

	// Get the direct SSH endpoint from Replicated (IP:port, user is always root)
	cp, err := newReplicatedProvider(cfg)
	if err != nil {
		logf("bootstrap: provider init error: %v", err)
		return
	}
	vm, err := cp.GetVM(context.Background(), vmID)
	if err != nil || vm.DirectSSHEndpoint == "" || vm.DirectSSHPort == 0 {
		logf("bootstrap: could not get direct SSH endpoint for VM %s: %v", vmID, err)
		return
	}
	// Replicated uses the comment from the SSH public key as the Linux username.
	// Our key comment is "elasticclaw@hub", so the username is "elasticclaw".
	sshUser := replicatedpkg.SSHUserFromPublicKey(s.identity.PublicKey)
	sshHome, err := sshHomeDir(sshUser)
	if err != nil {
		logf("bootstrap: invalid SSH user %q: %v", sshUser, err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: invalid SSH user: %s", sanitizeBootstrapError(err)), false)
		return
	}
	sshHost := fmt.Sprintf("%s:%d", vm.DirectSSHEndpoint, vm.DirectSSHPort)
	replicatedSSHDelays := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
	}
	logf("Bootstrap SSH: %s@%s", sshUser, sshHost)
	// Store SSH connection details in the DB for terminal access
	_, _ = s.db.Exec(
		`UPDATE claws SET ssh_host=?, ssh_port=?, ssh_user=? WHERE id=?`,
		vm.DirectSSHEndpoint, vm.DirectSSHPort, sshUser, clawID,
	)

	// Generate a random gateway password for this VM so claw-bridge can connect with full scopes
	gatewayPassword := randomHex(16)

	s.cfgMu.RLock()
	// Inject all configured LLM keys, prioritizing the selected key if specified
	llmKeyEnv := buildLLMKeyEnv(s.hubCfg.LLMKeys, llmKeyName)
	modelAuthEnv := buildModelAuthEnv(s.hubCfg, llmKeyName)
	clawToken := s.hubCfg.ClawToken
	hubCfg := s.hubCfg
	s.cfgMu.RUnlock()

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
		TemplateFiles:   files,
		HubCfg:          hubCfg,
		GitHubRepos:     githubRepos,
		LLMKeyEnv:       llmKeyEnv,
		ModelAuthEnv:    modelAuthEnv,
		APIKeyAuthSync:  buildOpenClawAPIKeyAuthSyncShell(hubCfg.LLMKeys, llmKeyName),
		LinearEnv:       buildLinearEnv(linearToken),
		ProviderConfig:  buildOpenClawProviderConfig(hubCfg.LLMKeys, llmKeyName),
		OnboardFlags:    buildOnboardFlags(hubCfg.LLMKeys, llmKeyName, defaultModel),
	})
	// Inject GitHub tools context into TOOLS.md if GitHub is configured
	s.cfgMu.RLock()
	hasGitHubApps2 := len(s.hubCfg.GitHubApps) > 0
	s.cfgMu.RUnlock()
	if hasGitHubApps2 && len(githubRepos) > 0 {
		repoLines := ""
		for _, r := range githubRepos {
			repoLines += fmt.Sprintf("- `%s` (%s)\n", r.Repo, r.Permissions)
		}
		githubSection := fmt.Sprintf(`
## GitHub Access

This agent has authenticated access to the following repositories via a GitHub App installation token. The token is fetched automatically — you don't need to configure anything.

%s
**git** and **gh CLI** are pre-configured and will work without any additional auth setup:

`+"```bash\n"+`# These just work:
git clone https://github.com/owner/repo
gh pr create
gh issue list
`+"```\n"+`
Tokens are short-lived and refreshed automatically on each git/gh operation.
`, repoLines)
		if existing, ok := files["TOOLS.md"]; ok {
			files["TOOLS.md"] = existing + "\n" + githubSection
		} else {
			files["TOOLS.md"] = githubSection
		}
	}

	if flakeFiles := templateFlakeFiles(files); len(flakeFiles) > 0 {
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Staging Nix flake",
			RetryLabel: "Retrying Nix flake staging",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshWriteFiles(sshUser, sshHost, path.Join(sshHome, "workspace"), flakeFiles)
			},
		}); err != nil {
			logf("[bootstrap] failed to stage flake before bootstrap for claw %s: %v", clawID[:8], err)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not stage flake files: %s", err), false)
			return
		}
	}

	// Run bootstrap script first — this installs OpenClaw and initializes the workspace.
	// Template files must be written AFTER the script completes so openclaw onboard
	// doesn't overwrite BOOTSTRAP.md and other workspace files.
	if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
		Label:      "Preparing ElasticClaw connector",
		RetryLabel: "Retrying sandbox bootstrap",
		Attempts:   5,
		Delays:     []time.Duration{10 * time.Second},
		Run: func() error {
			return s.sshRun(sshUser, sshHost, script)
		},
	}); err != nil {
		logf("Bootstrap failed for claw %s: %v", clawID, err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: %s", err), false)
		return
	}
	s.setBootstrapStatus(clawID, "Writing workspace files")

	// Write template files AFTER bootstrap — openclaw onboard initializes the workspace
	// and would overwrite BOOTSTRAP.md if we wrote it before the script ran.
	if len(files) > 0 {
		fileNames := make([]string, 0, len(files))
		for k := range files {
			fileNames = append(fileNames, k)
		}
		sort.Strings(fileNames)
		logf("[bootstrap] writing %d template files for claw %s: %v", len(files), clawName, fileNames)
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Writing workspace files",
			RetryLabel: "Retrying workspace file write",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshWriteFiles(sshUser, sshHost, path.Join(sshHome, ".openclaw", "workspace"), files)
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not write workspace files: %s", err), false)
			return
		}
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Verifying workspace files",
			RetryLabel: "Retrying workspace file verification",
			Attempts:   3,
			Delays:     []time.Duration{2 * time.Second, 5 * time.Second},
			Run: func() error {
				return s.sshRun(sshUser, sshHost, replicatedWorkspaceReadinessCommand(path.Join(sshHome, ".openclaw", "workspace"), files))
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: workspace files incomplete: %s", err), false)
			return
		}
		logf("Template files written for claw %s", clawName)
	}

	if err := s.restoreCheckpointToSSH(clawID, sshUser, sshHost); err != nil {
		logf("[bootstrap] restore checkpoint failed: %v", err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Restore checkpoint failed: %s", sanitizeBootstrapError(err)), false)
		return
	}

	// Run GitHub credential helper setup (needs bridge connected for hub proxy,
	// but the hub token URL is publicly accessible so it works directly).
	if credHelper := buildGitHubCredentialHelper(hubCfg, s.clawHubURL(), clawID, githubRepos); credHelper != "# GitHub App not configured — skipping credential helper" {
		if err := retryReplicatedBootstrapStep(s, clawID, replicatedBootstrapRetryOptions{
			Label:      "Configuring GitHub credentials",
			RetryLabel: "Retrying GitHub credential setup",
			Attempts:   6,
			Delays:     replicatedSSHDelays,
			Run: func() error {
				return s.sshRun(sshUser, sshHost, credHelper)
			},
		}); err != nil {
			s.stopAgentWithReason(clawID, fmt.Sprintf("Bootstrap failed: could not configure GitHub credentials: %s", err), false)
			return
		}
		logf("[bootstrap] GitHub credential helper installed for claw %s", clawName)
	}
	s.markBootstrapReady(clawID)

	logf("Bootstrap complete for claw %s (%s)", clawName, clawID[:8])
}

// terminateReplicatedVM terminates a Replicated CMX VM by ID.
func (s *Server) terminateReplicatedVM(vmID string) {
	s.cfgMu.RLock()
	cfg, ok := s.hubCfg.Providers["replicated"]
	s.cfgMu.RUnlock()
	if !ok {
		logf("terminateReplicatedVM: no replicated provider configured")
		return
	}
	p, err := newReplicatedProvider(cfg)
	if err != nil {
		logf("terminateReplicatedVM: provider init error: %v", err)
		return
	}
	destroyCtx, endSpan := telemetry.StartProviderSpan(context.Background(), "destroy", "replicated")
	err = p.DeleteVM(destroyCtx, vmID)
	endSpan(err)
	if err != nil {
		logf("terminateReplicatedVM: failed to delete VM %s: %v", vmID, err)
		return
	}
	logf("Replicated VM %s terminated", vmID)
}
