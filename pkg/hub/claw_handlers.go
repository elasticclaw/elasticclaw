// HTTP handlers for the claw collection and claw detail resources.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket"
)

func (s *Server) handleClaws(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)

	if r.Method == http.MethodPost {
		s.handleCreateClaw(w, r, tenantID)
		return
	}

	rows, err := s.db.Query(
		`SELECT id, name, template, COALESCE(provider,''), COALESCE(provider_id,''), status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,''), COALESCE(bootstrap_status,''), COALESCE(bootstrap_diagnostic,''), COALESCE(github_issue_id,'') FROM claws WHERE tenant_id = ? AND status != 'deleted' ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		logfCtx(r.Context(), "handleClaws query error: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", fmt.Sprintf("db error: %v", err))
		return
	}
	defer rows.Close()

	// Resolve access config and GitHub login for tag-based filtering
	s.mu.RLock()
	var accessCfg *types.AccessConfig
	if s.hubCfg.Auth != nil {
		accessCfg = s.hubCfg.Auth.Access
	}
	s.mu.RUnlock()
	ghLogin := githubLoginFromContext(r.Context())

	var out []types.Claw
	for rows.Next() {
		var c types.Claw
		var lastSeen sql.NullTime
		var tagsJSON string
		if err := rows.Scan(&c.ID, &c.Name, &c.Template, &c.Provider, &c.ProviderID, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color, &c.BootstrapStatus, &c.BootstrapDiagnostic, &c.GitHubIssueID); err != nil {
			continue
		}
		c.GitHubIssueURL = githubIssueURL(c.GitHubIssueID)
		_ = json.Unmarshal([]byte(tagsJSON), &c.Tags)
		c.TenantID = tenantID
		if lastSeen.Valid {
			c.LastSeen = lastSeen.Time
		}
		s.mu.RLock()
		cc, online := s.claws[c.ID]
		s.mu.RUnlock()
		if online {
			// Claw is currently connected — show live status
			if cc.GatewayReady {
				c.Status = "connected"
			} else {
				c.Status = "starting"
			}
			c.ContextUsage = cc.ContextUsage
		} else if c.Status != "provisioning" && c.Status != "starting" && c.Status != "error" && c.Status != "pending" {
			// Not currently connected and not in an active provisioning state —
			// DB status is stale (e.g. 'connected' from before hub restart)
			c.Status = "offline"
		}
		// Apply tag-based view filter (only applies to GitHub OAuth users)
		if ghLogin != "" && !canViewClaw(accessCfg, ghLogin, c.Tags) {
			continue
		}
		out = append(out, c)
	}
	if out == nil {
		out = []types.Claw{}
	}
	jsonOK(w, out)
}

func githubIssueURL(issueID string) string {
	parts := strings.Split(issueID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ""
	}
	for _, ch := range parts[2] {
		if ch < '0' || ch > '9' {
			return ""
		}
	}
	return fmt.Sprintf("https://github.com/%s/%s/issues/%s", parts[0], parts[1], parts[2])
}

func (s *Server) handleCreateClaw(w http.ResponseWriter, r *http.Request, tenantID string) {
	var req types.CreateClawRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	if req.Name == "" || req.Provider == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name and provider are required")
		return
	}

	// Check provider is configured
	s.mu.RLock()
	provCfg, ok := s.hubCfg.Providers[req.Provider]
	s.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "unprocessable", fmt.Sprintf("provider %q is not configured on this hub", req.Provider))
		return
	}

	// Pre-register claw row so it exists before the workspace boots
	clawID := uuid.New().String()

	// Build env to inject: hub connection info so the claw can register back
	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	hubSecrets := s.hubCfg.Secrets
	s.mu.RUnlock()
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_ID":    clawID,
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
	}
	for k, v := range req.Env {
		env[k] = v
	}
	for envName, secretRef := range req.SecretRefs {
		if val, ok := hubSecrets[secretRef]; ok {
			env[envName] = val
			logfCtx(r.Context(), "[create] injected secret_ref %s as %s into claw env", secretRef, envName)
		} else {
			logfCtx(r.Context(), "[create] WARNING: secret_ref %q not found in hub secrets", secretRef)
		}
	}
	req.Env = env
	req.Files = injectFigmaAPIDocs(req.Files, env)
	filesJSON, _ := json.Marshal(req.Files)

	// Store GitHub repos config from template if present
	var githubReposJSON string = "[]"
	if req.GitHub != nil && len(req.GitHub.Repos) > 0 {
		b, _ := json.Marshal(req.GitHub.Repos)
		githubReposJSON = string(b)
	}

	// Store Linear workspace label from template if present
	var linearWorkspace string
	if req.Linear != nil {
		linearWorkspace = req.Linear.Workspace
	}

	nixEnabled := 0
	if req.Nix {
		nixEnabled = 1
	}
	dockerEnabled := 0
	if req.Docker {
		dockerEnabled = 1
	}
	logfCtx(r.Context(), "[create] claw %s: nix=%d docker=%d", req.Name, nixEnabled, dockerEnabled)

	// Resolve default model: explicit > llm_key lookup > default key > hub default
	defaultModel := req.DefaultModel
	if defaultModel == "" {
		s.mu.RLock()
		var activeKey *types.LLMKeyConfig
		for _, k := range s.hubCfg.LLMKeys {
			if k.Name == req.LLMKey {
				activeKey = k
				break
			}
		}
		// If no explicit key selected, fall back to the default key
		if activeKey == nil {
			for _, k := range s.hubCfg.LLMKeys {
				if k.Default {
					activeKey = k
					break
				}
			}
		}
		if activeKey != nil {
			defaultModel = resolveDefaultModelForKey(s.hubCfg, activeKey)
		} else {
			defaultModel = s.hubCfg.DefaultModel
		}
		s.mu.RUnlock()
	}
	req.DefaultModel = defaultModel

	tags := mergeTags(req.TemplateName, req.Tags, nil) // CLI tags already merged client-side
	tagsJSON, _ := json.Marshal(tags)
	color := resolveColor(req.Color, req.Name)

	_, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, status, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, req.Name, req.TemplateName, req.Provider, req.DefaultModel, string(filesJSON),
		githubReposJSON, linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), color, req.LLMKey, "provisioning", now(),
	)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	// Convert string files to bytes for the provider
	templateFiles := make(map[string][]byte, len(req.Files))
	for k, v := range req.Files {
		templateFiles[k] = []byte(v)
	}

	// Resolve MCP server configs from template + hub config
	var mcpConfigs []*types.MCPConfig
	if len(req.MCPs) > 0 {
		s.mu.RLock()
		hubMCPServers := s.hubCfg.MCPServers
		hubSecrets := s.hubCfg.Secrets
		s.mu.RUnlock()
		for _, mcpRef := range req.MCPs {
			var hubMCP *types.MCPServerHubConfig
			for _, hm := range hubMCPServers {
				if hm.Name == mcpRef.Name {
					hubMCP = hm
					break
				}
			}
			if hubMCP == nil {
				logfCtx(r.Context(), "[create] MCP server %q not found in hub config, skipping", mcpRef.Name)
				continue
			}
			if !hubMCP.Enabled {
				logfCtx(r.Context(), "[create] MCP server %q is disabled, skipping", mcpRef.Name)
				continue
			}
			// Build command
			cmd := hubMCP.Command
			if len(cmd) == 0 {
				switch hubMCP.Source {
				case types.MCPSourceNpx:
					cmd = []string{"npx", "-y", hubMCP.Package}
				case types.MCPSourceUvx:
					cmd = []string{"uvx", hubMCP.Package}
				case types.MCPSourceSmithery:
					cmd = []string{"npx", "-y", "@smithery/cli@latest", "run", hubMCP.Package}
				case types.MCPSourceDocker:
					cmd = []string{"docker", "run", "-i", "--rm", hubMCP.Image}
				case types.MCPSourceSSE:
					// SSE is remote — no local command, skip for now
					logfCtx(r.Context(), "[create] SSE MCP server %q not yet supported for local stdio", mcpRef.Name)
					continue
				}
			}
			if len(cmd) == 0 {
				logfCtx(r.Context(), "[create] MCP server %q has no command, skipping", mcpRef.Name)
				continue
			}
			// Build env: merge hub config + template overrides + resolved secrets
			mcpEnv := make(map[string]string)
			for k, v := range hubMCP.Config {
				mcpEnv[k] = v
			}
			// Template-level overrides (from MCPRef.Config) take precedence over hub-level config
			for k, v := range mcpRef.Env {
				mcpEnv[k] = v
			}
			for envVar, secretRef := range hubMCP.Secrets {
				if val, ok := hubSecrets[secretRef]; ok {
					mcpEnv[envVar] = val
				}
			}
			mcpConfigs = append(mcpConfigs, &types.MCPConfig{
				Name:    mcpRef.Name,
				Command: cmd,
				Env:     mcpEnv,
			})
		}
	}
	req.MCPs = mcpConfigs

	// Provision asynchronously so the HTTP request returns quickly
	// Use a stable short ID as the provider-side name so renaming the claw
	// doesn't require a provider API call.
	providerNamePrefix := strings.TrimSpace(os.Getenv("ELASTICCLAW_PROVIDER_NAME_PREFIX"))
	if providerNamePrefix == "" {
		providerNamePrefix = "ec-"
	}
	req.ProviderName = providerNamePrefix + clawID[:8]
	go func() {
		logfCtx(r.Context(), "Provisioning claw %s (%s) via %s (provider name: %s)...", req.Name, clawID, req.Provider, req.ProviderName)
		ctx := context.Background()
		var provErr error

		switch req.Provider {
		case "daytona":
			provErr = s.provisionDaytona(ctx, clawID, req, provCfg, templateFiles, env)
		case "replicated":
			provErr = s.provisionReplicated(ctx, clawID, req, provCfg, env)
		case "exedev":
			provErr = s.provisionExedev(ctx, clawID, req, provCfg, templateFiles, env)
		case "docker":
			provErr = s.provisionDocker(ctx, clawID, req, provCfg, templateFiles)
		case "lambda-microvms":
			provErr = s.provisionLambdaMicroVMs(ctx, clawID, req, provCfg, templateFiles)
		default:
			provErr = fmt.Errorf("unsupported provider: %s", req.Provider)
		}

		if provErr != nil {
			logfCtx(r.Context(), "provisioning failed for claw %s: %v", clawID, provErr)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Provisioning failed: %v", provErr), false)
		}
	}()

	claw := types.Claw{
		ID: clawID, TenantID: tenantID, Name: req.Name,
		Template: req.TemplateName, Status: "provisioning", CreatedAt: now(),
	}
	w.WriteHeader(http.StatusAccepted)
	jsonOK(w, claw)
}

func (s *Server) handleClawDetail(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r)
	clawID := r.PathValue("id")
	if clawID == "" {
		clawID = strings.TrimPrefix(r.URL.Path, "/api/claws/")
	}
	ghLogin := githubLoginFromContext(r.Context())
	var accessCfg *types.AccessConfig
	if ghLogin != "" {
		s.mu.RLock()
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.mu.RUnlock()
	}

	if r.Method == http.MethodPatch {
		if ghLogin != "" {
			var tagsJSON string
			if err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSON); err != nil {
				if err == sql.ErrNoRows {
					writeErr(w, http.StatusNotFound, "not_found", "not found")
				} else {
					writeErr(w, http.StatusInternalServerError, "internal", "db error")
				}
				return
			}
			var clawTags []string
			_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
			if !canModifyClaw(accessCfg, ghLogin, clawTags) {
				writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
				return
			}
		}
		var body struct {
			Name  *string   `json:"name"`
			Tags  *[]string `json:"tags"`
			Color *string   `json:"color"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid body")
			return
		}
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			_, _ = s.db.Exec(`UPDATE claws SET name = ? WHERE id = ? AND tenant_id = ?`, strings.TrimSpace(*body.Name), clawID, tenantID)
		}
		if body.Tags != nil {
			// Normalize tags to k=v format
			normalized := make([]string, 0, len(*body.Tags))
			seen := make(map[string]bool)
			for _, t := range *body.Tags {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if !seen[t] {
					seen[t] = true
					normalized = append(normalized, t)
				}
			}
			tagsJSON, _ := json.Marshal(normalized)
			_, _ = s.db.Exec(`UPDATE claws SET tags = ?, updated_at = datetime('now') WHERE id = ? AND tenant_id = ?`, string(tagsJSON), clawID, tenantID)
			// Update in-memory cache so WS broadcast filtering stays current
			s.mu.Lock()
			if cc, ok := s.claws[clawID]; ok {
				cc.Tags = normalized
			}
			s.mu.Unlock()
		}
		if body.Color != nil {
			color := resolveColor(*body.Color, clawID)
			_, _ = s.db.Exec(`UPDATE claws SET color = ? WHERE id = ? AND tenant_id = ?`, color, clawID, tenantID)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method == http.MethodDelete {
		// Resolve short ID prefix to full UUID
		var fullID string
		_ = s.db.QueryRow(`SELECT id FROM claws WHERE tenant_id = ? AND (id = ? OR id LIKE ?)`, tenantID, clawID, clawID+"%").Scan(&fullID)
		if fullID != "" {
			clawID = fullID
		}
		if ghLogin != "" {
			var tagsJSON string
			if err := s.db.QueryRow(`SELECT COALESCE(tags,'[]') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&tagsJSON); err != nil {
				if err == sql.ErrNoRows {
					writeErr(w, http.StatusNotFound, "not_found", "not found")
				} else {
					writeErr(w, http.StatusInternalServerError, "internal", "db error")
				}
				return
			}
			var clawTags []string
			_ = json.Unmarshal([]byte(tagsJSON), &clawTags)
			if !canModifyClaw(accessCfg, ghLogin, clawTags) {
				writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
				return
			}
		}

		// Look up provider info before marking deleted so we can terminate the VM.
		var provider, providerID, clawStatus string
		_ = s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,''), COALESCE(status,'') FROM claws WHERE id = ? AND tenant_id = ?`, clawID, tenantID).Scan(&provider, &providerID, &clawStatus)

		// Post a comment on the linked issue/story when a factory-created claw is killed manually
		factory, issueID := s.findFactoryForClaw(clawID)
		if factory != nil && issueID != "" {
			switch factory.Integration {
			case "linear":
				token := s.resolveLinearTokenForFactory(factory)
				if token != "" {
					if err := s.commentLinearIssue(token, issueID, "Agent stopped: killed manually via dashboard"); err != nil {
						logfCtx(r.Context(), "[kill] failed to comment Linear issue %s: %v", issueID, err)
					} else {
						logfCtx(r.Context(), "[kill] commented Linear issue %s", issueID)
					}
				}
			case "shortcut":
				token := s.resolveShortcutToken(factory.Workspace)
				if token != "" {
					if err := commentShortcutIssue(s.resolveShortcutBaseURL(), token, issueID, "Agent stopped: killed manually via dashboard"); err != nil {
						logfCtx(r.Context(), "[kill] failed to comment Shortcut story %s: %v", issueID, err)
					} else {
						logfCtx(r.Context(), "[kill] commented Shortcut story %s", issueID)
					}
				}
			case "github-issues":
				parts := strings.Split(issueID, "/")
				if len(parts) == 3 {
					token := s.resolveGitHubIssuesTokenForFactory(factory)
					if token != "" {
						repo := parts[0] + "/" + parts[1]
						var issueNum int
						if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
							if err := commentGitHubIssue(token, repo, issueNum, "Agent stopped: killed manually via dashboard"); err != nil {
								logfCtx(r.Context(), "[kill] failed to comment GitHub issue %s: %v", issueID, err)
							} else {
								logfCtx(r.Context(), "[kill] commented GitHub issue %s", issueID)
							}
						}
					}
				}
			}
		}

		res, err := s.db.Exec(`UPDATE claws SET status='deleted', bootstrap_status='' WHERE id = ? AND tenant_id = ? AND status != 'deleted'`, clawID, tenantID)
		if err != nil {
			logfCtx(r.Context(), "kill: db soft-delete error for claw %s: %v", clawID, err)
			writeErr(w, http.StatusInternalServerError, "internal", fmt.Sprintf("db error: %v", err))
			return
		}
		rowsAffected, err := res.RowsAffected()
		if err != nil || rowsAffected == 0 {
			writeErr(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		if clawStatus != "error" {
			s.recordTaskRunManualStopBeforeDelivery(clawID, ghLogin)
		}
		if s.cronScheduler != nil {
			s.cronScheduler.FinishRunByClawID(clawID, "canceled", "manually killed")
		}
		// Notify dashboards before provider cleanup so the card disappears immediately.
		s.broadcastToUsers(tenantID, types.WSMessage{
			Type:    "claw_status",
			Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
		})
		// Disconnect WebSocket if online
		s.mu.Lock()
		if cc, ok := s.claws[clawID]; ok {
			cc.WS.Close(websocket.StatusNormalClosure, "killed")
			delete(s.claws, clawID)
		}
		s.mu.Unlock()
		go func() {
			s.checkpointBeforeTermination(clawID, "manual-kill")
			if providerID != "" {
				s.terminateVM(provider, providerID)
			}
			_, _ = s.db.Exec(`DELETE FROM messages WHERE claw_id = ?`, clawID)
			_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id = ?`, clawID)
		}()
		// Promote any pending claws now that a slot is free
		go s.promotePendingClaws()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var c types.Claw
	var lastSeen sql.NullTime
	var tagsJSON string
	err := s.db.QueryRow(
		`SELECT id, name, template, COALESCE(provider,''), COALESCE(provider_id,''), status, last_seen, created_at, ssh_host, ssh_port, ssh_user, COALESCE(tags,'[]'), COALESCE(color,''), COALESCE(bootstrap_status,''), COALESCE(bootstrap_diagnostic,''), COALESCE(github_issue_id,'') FROM claws WHERE id = ? AND tenant_id = ? AND status != 'deleted'`,
		clawID, tenantID,
	).Scan(&c.ID, &c.Name, &c.Template, &c.Provider, &c.ProviderID, &c.Status, &lastSeen, &c.CreatedAt, &c.SSHHost, &c.SSHPort, &c.SSHUser, &tagsJSON, &c.Color, &c.BootstrapStatus, &c.BootstrapDiagnostic, &c.GitHubIssueID)
	_ = json.Unmarshal([]byte(tagsJSON), &c.Tags)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if ghLogin != "" && !canViewClaw(accessCfg, ghLogin, c.Tags) {
		writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
		return
	}
	c.TenantID = tenantID
	c.GitHubIssueURL = githubIssueURL(c.GitHubIssueID)
	if lastSeen.Valid {
		c.LastSeen = lastSeen.Time
	}
	s.mu.RLock()
	cc, online := s.claws[c.ID]
	s.mu.RUnlock()
	if online {
		if cc.GatewayReady {
			c.Status = "connected"
		} else {
			c.Status = "starting"
		}
		c.ContextUsage = cc.ContextUsage
	} else if c.Status != "provisioning" && c.Status != "error" {
		c.Status = "offline"
	}
	jsonOK(w, c)
}
