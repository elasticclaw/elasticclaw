package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

// linearWebhookPayload is the relevant subset of a Linear webhook event.
type linearWebhookPayload struct {
	Action string `json:"action"` // "create", "update", "remove"
	Type   string `json:"type"`   // "Issue", "IssueLabel", etc.
	Data   struct {
		ID          string `json:"id"`
		Identifier  string `json:"identifier"` // e.g. "ELA-123"
		Title       string `json:"title"`
		Description string `json:"description"`
		URL         string `json:"url"`
		State       struct {
			Name string `json:"name"`
		} `json:"state"`
		Team struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"team"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels,omitempty"`
		Assignee *struct {
			Name string `json:"name"`
		} `json:"assignee,omitempty"`
	} `json:"data"`
	UpdatedFrom *struct {
		State *struct {
			Name string `json:"name"`
		} `json:"state,omitempty"`
	} `json:"updatedFrom,omitempty"`
}

func (s *Server) handleLinearWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Validate signature if any Linear integration has a webhook_secret
	sig := r.Header.Get("Linear-Signature")
	if !s.validateLinearSignature(body, sig) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var payload linearWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// Only handle Issue updates
	if payload.Type != "Issue" || payload.Action != "update" {
		w.WriteHeader(http.StatusOK)
		return
	}

	go s.processLinearEvent(payload)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) validateLinearSignature(body []byte, sig string) bool {
	s.mu.RLock()
	integrations := s.hubCfg.Integrations
	factories := s.hubCfg.Factories
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	hasAnySecret := false

	// Check integration-level secrets
	if integrations != nil {
		for _, li := range integrations.Linear {
			if li.WebhookSecret == "" {
				continue
			}
			hasAnySecret = true
			mac := hmac.New(sha256.New, []byte(li.WebhookSecret))
			mac.Write(body)
			expected := hex.EncodeToString(mac.Sum(nil))
			if hmac.Equal([]byte(sig), []byte(expected)) {
				return true
			}
		}
	}

	// Check factory-level secrets
	if factories != nil {
		for _, factory := range factories {
			if factory.Integration != "linear" {
				continue
			}
			// Resolve secret: inline value or named ref
			secret := factory.WebhookSecret
			if secret == "" && factory.WebhookSecretRef != "" && secrets != nil {
				secret = secrets[factory.WebhookSecretRef]
			}
			if secret == "" {
				continue
			}
			hasAnySecret = true
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			expected := hex.EncodeToString(mac.Sum(nil))
			if hmac.Equal([]byte(sig), []byte(expected)) {
				return true
			}
		}
	}

	// If any secrets are configured but none matched, reject
	if hasAnySecret {
		return false
	}
	return true // no secrets configured
}

func (s *Server) processLinearEvent(payload linearWebhookPayload) {
	if s.hubCfg.Factories == nil {
		return
	}

	currentStatus := payload.Data.State.Name
	previousStatus := ""
	if payload.UpdatedFrom != nil && payload.UpdatedFrom.State != nil {
		previousStatus = payload.UpdatedFrom.State.Name
	}
	teamKey := payload.Data.Team.Key
	issueID := payload.Data.Identifier // e.g. "ELA-123"

	matched := false
	for _, factory := range s.hubCfg.Factories {
		if factory.Integration != "linear" {
			continue
		}
		// Skip disabled factories
		if factory.Enabled != nil && !*factory.Enabled {
			continue
		}
		if factory.Team != "" && !strings.EqualFold(factory.Team, teamKey) {
			continue
		}

		// Labels filter: all configured labels must be present on the issue (AND)
		if len(factory.Labels) > 0 {
			issueLabels := map[string]bool{}
			for _, l := range payload.Data.Labels {
				issueLabels[strings.ToLower(l.Name)] = true
			}
			allMatch := true
			for _, required := range factory.Labels {
				if !issueLabels[strings.ToLower(required)] {
					allMatch = false
					break
				}
			}
			if !allMatch {
				continue
			}
		}

		// AssignedTo filter
		if factory.AssignedTo != "" {
			assignee := ""
			if payload.Data.Assignee != nil {
				assignee = payload.Data.Assignee.Name
			}
			wanted := strings.ToLower(strings.TrimSpace(factory.AssignedTo))
			switch {
			case wanted == "any":
				if assignee == "" {
					continue
				}
			case wanted == "none":
				if assignee != "" {
					continue
				}
			case strings.HasPrefix(wanted, "!"):
				excluded := strings.TrimPrefix(strings.TrimPrefix(wanted, "!"), "@")
				if strings.EqualFold(assignee, excluded) {
					continue
				}
			default:
				target := strings.TrimPrefix(wanted, "@")
				if !strings.EqualFold(assignee, target) {
					continue
				}
			}
		}

		// Issue entering trigger status → create claw.
		// Only create when transitioning into the trigger status (not already in it).
		// When previousStatus is empty (Linear omits it), EqualFold returns false, so the guard passes.
		if strings.EqualFold(currentStatus, factory.TriggerStatus) && !strings.EqualFold(previousStatus, factory.TriggerStatus) {
			matched = true
			log.Printf("[factory:%s] issue %s entered '%s' — creating claw", factory.Name, issueID, factory.TriggerStatus)
			clawID := ""
			if err := s.createClawForIssue(factory, payload); err != nil {
				log.Printf("[factory:%s] failed to create claw for %s: %v", factory.Name, issueID, err)
				s.logFactoryEvent(factory.Name, issueID, payload.Data.Title, previousStatus, currentStatus, "error", "", err.Error())
			} else {
				// Look up the newly created claw ID
				_ = s.db.QueryRow(`SELECT id FROM claws WHERE linear_issue_id=? ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&clawID)
				s.logFactoryEvent(factory.Name, issueID, payload.Data.Title, previousStatus, currentStatus, "claw_created", clawID, "")
			}
		}

		// Issue leaving trigger status → terminate claw.
		// Check DB for active claw created by this factory by matching factory tag.
		if factory.TerminateOnLeave && !strings.EqualFold(currentStatus, factory.TriggerStatus) {
			var activeClaw string
			_ = s.db.QueryRow(
				`SELECT id FROM claws WHERE linear_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`,
				issueID,
			).Scan(&activeClaw)
			if activeClaw != "" {
				matched = true
				log.Printf("[factory:%s] issue %s moved to '%s' (not trigger) — terminating claw", factory.Name, issueID, currentStatus)
				s.terminateClawForIssue(issueID)
				s.logFactoryEvent(factory.Name, issueID, payload.Data.Title, previousStatus, currentStatus, "claw_terminated", activeClaw, "terminated: issue left trigger status")
			}
		}
	}

	if !matched {
		// Webhook received but no factory matched — log as not_actionable
		for _, factory := range s.hubCfg.Factories {
			if factory.Integration == "linear" && (factory.Enabled == nil || *factory.Enabled) && (factory.Team == "" || strings.EqualFold(factory.Team, teamKey)) {
				s.logFactoryEvent(factory.Name, issueID, payload.Data.Title, previousStatus, currentStatus, "not_actionable",
					"", fmt.Sprintf("status '%s'→'%s' did not match trigger '%s'", previousStatus, currentStatus, factory.TriggerStatus))
			}
		}
	}
}

func (s *Server) createClawForIssue(factory *types.FactoryConfig, payload linearWebhookPayload) error {
	issueID := payload.Data.Identifier

	// Verify we can read the issue before spending money on a sandbox.
	// Non-negotiable: if the issue is unreadable, we can't do any work.
	linearToken := s.resolveLinearTokenForFactory(factory)
	if linearToken != "" {
		tokenPrefix := "<empty>"
		if len(linearToken) > 12 {
			tokenPrefix = linearToken[:12] + "..."
		} else if len(linearToken) >= 4 {
			tokenPrefix = linearToken[:4] + "..."
		}
		log.Printf("[factory:%s] pre-flight check: issue=%s workspace=%q tokenPrefix=%s", factory.Name, issueID, factory.Workspace, tokenPrefix)
		if _, err := s.fetchLinearIssueDetails(linearToken, issueID); err != nil {
			log.Printf("[factory:%s] pre-flight FAILED for %s: %v", factory.Name, issueID, err)
			return fmt.Errorf("cannot read issue %s from Linear (check token/workspace access): %w", issueID, err)
		}
		log.Printf("[factory:%s] verified issue %s is readable", factory.Name, issueID)
	} else {
		log.Printf("[factory:%s] warning: no Linear token, skipping pre-flight issue read for %s", factory.Name, issueID)
	}

	// Enforce 1:1 — check if a claw already exists for this issue
	var existingID string
	_ = s.db.QueryRow(
		`SELECT id FROM claws WHERE linear_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`,
		issueID,
	).Scan(&existingID)
	if existingID != "" {
		return fmt.Errorf("claw %s already exists for issue %s", existingID[:8], issueID)
	}

	// Resolve template files
	templateFiles, err := s.resolveTemplateFiles(factory.Template)
	if err != nil {
		return fmt.Errorf("template %q not found: %w", factory.Template, err)
	}

	// Parse elasticclaw-config.yaml from the template files (if present) so we
	// honour provider settings like nix, github repos, default_model, instance_type, etc.
	// This mirrors what the CLI does via config.LoadTemplateConfig before calling CreateClaw.
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		var parseErr error
		tmplCfg, parseErr = config.ParseTemplateConfig([]byte(cfgContent))
		if parseErr != nil {
			log.Printf("[factory:%s] warning: could not parse elasticclaw-config.yaml from template %q: %v", factory.Name, factory.Template, parseErr)
			// Non-fatal — continue with defaults
			tmplCfg = nil
		}
	}

	// Inject issue context as CONTEXT.md for persistent reference.
	// BOOTSTRAP.md is intentionally omitted — the pipeline inject message tells
	// the agent to fetch issue details via Linear tools instead of reading a file
	// that races with first-run cleanup.
	issueContext := buildLinearContext(payload)
	templateFiles["CONTEXT.md"] = issueContext

	// Determine claw name
	clawName := issueID // e.g. "ELA-123"
	if factory.NamePattern != "" {
		clawName = strings.ReplaceAll(factory.NamePattern, "{issue_id}", issueID)
	}

	// Find tenant
	var tenantID string
	if err := s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return fmt.Errorf("no tenant: %w", err)
	}

	// Find provider: factory override > template config > hub default
	provider := factory.Provider
	if provider == "" {
		if tmplCfg != nil && tmplCfg.Provider != "" {
			provider = tmplCfg.Provider
		}
	}
	if provider == "" {
		provider = s.defaultProvider()
	}
	if provider == "" {
		return fmt.Errorf("no provider configured")
	}

	// linearToken was already resolved in the pre-flight check above.
	// Build env vars
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_TOKEN": s.hubCfg.ClawToken,
	}
	if linearToken != "" {
		env["LINEAR_API_KEY"] = linearToken
	}

	// Resolve and inject template-requested secrets (typed refs + legacy)
	resolvedSecrets := make(map[string]string) // envName → value (also used for SECRETS.md)
	if tmplCfg != nil && len(tmplCfg.Secrets) > 0 {
		for _, ref := range tmplCfg.Secrets {
			val, envName, ok := s.resolveSecretRef(ref, factory)
			if ok {
				env[envName] = val
				resolvedSecrets[envName] = val
				log.Printf("[factory:%s] injected secret %s as %s into claw env", factory.Name, ref.Type, envName)
			} else {
				log.Printf("[factory:%s] warning: requested secret (type=%s name=%s workspace=%s) not found", factory.Name, ref.Type, ref.Name, ref.Workspace)
			}
		}
	}

	// Resolve template config fields (from elasticclaw-config.yaml if present).
	// Factory-level overrides (color, tags) take precedence over template config.
	var (
		instanceType    string
		defaultModel    string
		llmKey          string
		nixEnabled      int
		githubRepos     []types.GitHubRepoAccess
		linearWorkspace string
		autoFixCI       int = 1
		autoFixBugbot   int = 1
	)
	if tmplCfg != nil {
		instanceType = tmplCfg.InstanceType
		defaultModel = tmplCfg.DefaultModel
		llmKey = tmplCfg.LLMKey
		if tmplCfg.Nix {
			nixEnabled = 1
		}
		if tmplCfg.GitHub != nil {
			githubRepos = tmplCfg.GitHub.Repos
		}
		if tmplCfg.Linear != nil {
			linearWorkspace = tmplCfg.Linear.Workspace
		}
		// Template can opt out of auto-watching; default is on (1)
		if tmplCfg.AutoWatchCI != nil && !*tmplCfg.AutoWatchCI {
			autoFixCI = 0
		}
		if tmplCfg.AutoWatchBugbot != nil && !*tmplCfg.AutoWatchBugbot {
			autoFixBugbot = 0
		}
	}
	// Build SECRETS.md so the agent knows which env vars are available.
	// Values are NOT written — they stay in env only.
	var secretList []string
	if linearToken != "" {
		secretList = append(secretList, "- `LINEAR_API_KEY` — Linear API token")
	}
	for envName, val := range resolvedSecrets {
		// Find the ref that produced this envName for type info
		var refType string
		for _, ref := range tmplCfg.Secrets {
			if ref.EnvVarName() == envName {
				refType = ref.Type
				break
			}
		}
		if refType == "" {
			refType = "custom"
		}
		secretList = append(secretList, fmt.Sprintf("- `%s` — %s secret", envName, refType))
		_ = val // value intentionally not logged/written
	}
	if len(secretList) > 0 {
		templateFiles["SECRETS.md"] = "## Available Secrets\n\nThe following API keys are available as environment variables:\n\n" + strings.Join(secretList, "\n") + "\n\nUse these with your tools as needed. Values are in the environment, not in files.\n"
	}

	// Resolve default model: template > llm_key lookup > hub default
	if defaultModel == "" && llmKey != "" {
		s.mu.RLock()
		for _, k := range s.hubCfg.LLMKeys {
			if k.Name == llmKey {
				defaultModel = resolveDefaultModelForKey(s.hubCfg, k)
				break
			}
		}
		s.mu.RUnlock()
	}
	if defaultModel == "" {
		s.mu.RLock()
		defaultModel = s.hubCfg.DefaultModel
		s.mu.RUnlock()
	}

	// Build tags — always include template:<name> and factory:<name>; merge with factory-configured tags
	tags := mergeTags(factory.Template, factory.Tags, nil)
	// Ensure factory:<name> is also present
	hasfactory := false
	for _, t := range tags {
		if t == "factory:"+factory.Name {
			hasfactory = true
			break
		}
	}
	if !hasfactory {
		tags = append(tags, "factory:"+factory.Name)
	}
	tagsJSON, _ := json.Marshal(tags)

	// Color: factory overrides template config
	clawColor := factory.Color
	if clawColor == "" && tmplCfg != nil {
		clawColor = tmplCfg.Color
	}

	githubReposJSON, _ := json.Marshal(githubRepos)

	// Insert claw record
	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(templateFiles)
	now := now()

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, tags, color, llm_key, auto_fix_ci, auto_fix_bugbot, linear_issue_id, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'provisioning',?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, string(tagsJSON), clawColor, llmKey, autoFixCI, autoFixBugbot, issueID, now,
	)
	if err != nil {
		return fmt.Errorf("db insert: %w", err)
	}

	// Provision asynchronously
	provCfg, _ := s.hubCfg.Providers[provider]
	go func() {
		// Guard: if the claw was deleted before provisioning started (e.g. issue
		// immediately moved back out of trigger status), abort silently.
		var currentStatus string
		_ = s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
		if currentStatus == "deleted" {
			log.Printf("[factory] claw %s already deleted before provisioning, aborting", clawID[:8])
			return
		}
		ctx := context.Background()
		req := types.CreateClawRequest{
			Name:         clawName,
			TemplateName: factory.Template,
			Provider:     provider,
			Files:        templateFiles,
			Env:          env,
			InstanceType: instanceType,
			ProviderName: "ec-" + clawID[:8],
		}
		// Convert string files to []byte for providers that need it
		fileBytes := make(map[string][]byte, len(templateFiles))
		for k, v := range templateFiles {
			fileBytes[k] = []byte(v)
		}

		var provErr error
		switch provider {
		case "replicated":
			provErr = s.provisionReplicated(ctx, clawID, req, provCfg, env)
		case "daytona":
			provErr = s.provisionDaytona(ctx, clawID, req, provCfg, fileBytes, env)
		case "vercel":
			provErr = s.provisionVercel(ctx, clawID, req, provCfg, fileBytes, env)
		case "noop":
			// Test provider — only allowed when explicitly enabled via env var.
			if os.Getenv("ELASTICCLAW_NOOP_PROVIDER") == "" {
				provErr = fmt.Errorf("noop provider requires ELASTICCLAW_NOOP_PROVIDER=1 (test use only)")
			} else {
				providerID := "noop-vm-" + clawID[:8]
				_, _ = s.db.Exec(`UPDATE claws SET status='connected', provider='noop', provider_id=? WHERE id=? AND status NOT IN ('idle','deleted','error')`, providerID, clawID)
			}
		default:
			provErr = fmt.Errorf("unsupported provider: %s", provider)
		}
		if provErr != nil {
			log.Printf("[factory] provision failed for %s: %v", clawID, provErr)
			_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=? AND status != 'deleted'`, clawID)
		}
	}()

	log.Printf("[factory] created claw %s (%s) for Linear issue %s", clawName, clawID[:8], issueID)
	// Notify connected dashboards immediately so the card appears without waiting for next poll
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "provisioning"},
	})

	return nil
}

func (s *Server) terminateClawForIssue(issueID string) {
	var clawID, tenantID string
	if err := s.db.QueryRow(
		`SELECT id, tenant_id FROM claws WHERE linear_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`,
		issueID,
	).Scan(&clawID, &tenantID); err != nil {
		return
	}
	log.Printf("[factory] terminating claw %s for issue %s", clawID[:8], issueID)
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "factory: issue left trigger status")
		delete(s.claws, clawID)
	}
	s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
	// Notify dashboards so the card disappears immediately
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
	})
}

func (s *Server) defaultProvider() string {
	for name, p := range s.hubCfg.Providers {
		// Only consider providers with real credentials, not Type-only stubs (e.g. noop)
		if p.Token != "" || p.APIKey != "" || p.AccessToken != "" {
			return name
		}
	}
	return ""
}

func (s *Server) resolveLinearTokenForFactory(factory *types.FactoryConfig) string {
	if s.hubCfg.Integrations == nil {
		return ""
	}
	for _, li := range s.hubCfg.Integrations.Linear {
		if factory.Workspace == "" || strings.EqualFold(li.Workspace, factory.Workspace) {
			return li.Token
		}
	}
	return ""
}

// resolveSecretRef resolves a typed SecretRef to its value and env var name.
// Returns (value, envName, ok). envName may be "" for misconfigured refs.
func (s *Server) resolveSecretRef(ref types.SecretRef, factory *types.FactoryConfig) (string, string, bool) {
	envName := ref.EnvVarName()
	if envName == "" {
		// Misconfigured ref (e.g. custom with no name, unknown type)
		return "", "", false
	}
	switch ref.Type {
	case "linear":
		if s.hubCfg.Integrations == nil {
			return "", envName, false
		}
		ws := ref.Workspace
		if ws == "" && factory != nil {
			ws = factory.Workspace
		}
		for _, li := range s.hubCfg.Integrations.Linear {
			if ws == "" || strings.EqualFold(li.Workspace, ws) {
				return li.Token, envName, true
			}
		}
		return "", envName, false
	case "shortcut":
		if s.hubCfg.Integrations == nil {
			return "", envName, false
		}
		ws := ref.Workspace
		if ws == "" && factory != nil {
			ws = factory.Workspace
		}
		for _, si := range s.hubCfg.Integrations.Shortcut {
			if ws == "" || strings.EqualFold(si.Workspace, ws) {
				return si.Token, envName, true
			}
		}
		return "", envName, false
	case "github":
		// GitHub tokens are minted per-installation; not stored in integrations.
		// For now, fall through to custom lookup in hub secrets.
		fallthrough
	case "custom":
		if val, ok := s.hubCfg.Secrets[ref.Name]; ok {
			return val, envName, true
		}
		return "", envName, false
	default:
		return "", envName, false
	}
}

func escapeLikeWildcards(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func buildLinearContext(payload linearWebhookPayload) string {
	d := payload.Data
	var b strings.Builder
	b.WriteString("# Issue Context\n\n")
	b.WriteString("This claw was automatically created by a factory to work on a Linear issue. Read this, understand the task, then get to work.\n\n")
	b.WriteString(fmt.Sprintf("## Issue: %s\n\n", d.Identifier))
	b.WriteString(fmt.Sprintf("**Title:** %s\n\n", d.Title))
	if d.URL != "" {
		b.WriteString(fmt.Sprintf("**URL:** %s\n\n", d.URL))
	}
	if d.Team.Name != "" {
		b.WriteString(fmt.Sprintf("**Team:** %s (%s)\n\n", d.Team.Name, d.Team.Key))
	}
	if d.Description != "" {
		b.WriteString("## Description\n\n")
		b.WriteString(d.Description)
		b.WriteString("\n")
	}
	b.WriteString("\n---\n\n")
	b.WriteString("## Instructions\n\n")
	b.WriteString("1. Read this file fully\n")
	b.WriteString("2. Explore the codebase\n")
	b.WriteString("3. Implement the feature/fix described above\n")
	b.WriteString("4. When complete, send exactly: `[DONE] https://github.com/org/repo/pull/N` (with your PR URL)\n")
	return b.String()
}

// Avoid collision with existing now() function

// handleClawDoneSignal is called when a claw sends a message containing [DONE].
// It parses PR URLs from the message, validates them via the GitHub API, stores
// them in claw_prs, then moves the Linear issue and terminates the claw.
// If no valid open PRs are found (and a GH App is configured), it injects an
// error message back so the claw can retry.
func (s *Server) handleClawDoneSignal(clawID, rawMessage string) {
	// Get the linear_issue_id and tenant for this claw
	var issueID, tenantID string
	if err := s.db.QueryRow(`SELECT linear_issue_id, tenant_id FROM claws WHERE id = ?`, clawID).Scan(&issueID, &tenantID); err != nil || issueID == "" {
		return // not a factory claw
	}

	log.Printf("[factory] claw %s sent [DONE] for issue %s", clawID[:8], issueID)

	// Extract PR URLs from the [DONE] line.
	// Expected format: [DONE] https://github.com/org/repo/pull/1 https://...
	prURLs := extractDonePRURLs(rawMessage)

	// Validate PRs via GitHub API if we have a token.
	ghToken := s.resolveGitHubToken()
	if ghToken != "" {
		if rejected, reason := s.validateDonePRs(clawID, prURLs, ghToken); rejected {
			// Nudge the claw to fix and retry — do not terminate.
			s.injectUserMessage(clawID, reason)
			return
		}
	} else if len(prURLs) == 0 {
		// No GH App configured, but still require at least one PR URL in the signal.
		s.injectUserMessage(clawID, "[factory] `[DONE]` received with no PR URLs. Please open a PR and resend: `[DONE] https://github.com/org/repo/pull/N`")
		return
	}

	// Store all validated PRs (idempotent).
	for _, pr := range extractPRs(strings.Join(prURLs, " ")) {
		s.storePRMention(clawID, pr.repo, pr.number, pr.url)
	}

	// Find the factory config for this issue
	factory := s.findFactoryForIssue(issueID)
	if factory == nil {
		return
	}

	// Check if the pipeline handles the [DONE] signal
	pipelineHandledDone := false
	if pl := parsePipelineForFactory(factory); pl != nil {
		if stage := pl.StageForMessageContains(rawMessage); stage != nil {
			s.transitionPipelineStage(clawID, *stage, factory, issueID)
			pipelineHandledDone = true
		}
	}

	// Move the issue to done_status if configured (skip if pipeline already handled it)
	if !pipelineHandledDone && factory.DoneStatus != "" {
		if strings.HasPrefix(issueID, "sc-") {
			// Shortcut story
			scToken := s.resolveShortcutToken(factory.Workspace)
			if scToken != "" {
				if err := moveShortcutStory(scToken, issueID, factory.DoneStatus); err != nil {
					log.Printf("[factory] failed to move story %s to '%s': %v", issueID, factory.DoneStatus, err)
				} else {
					log.Printf("[factory] moved story %s to '%s'", issueID, factory.DoneStatus)
				}
			}
		} else {
			// Linear issue
			linearToken := s.resolveLinearTokenForFactory(factory)
			if linearToken != "" {
				if err := s.moveLinearIssueOnServer(linearToken, issueID, factory.DoneStatus); err != nil {
					log.Printf("[factory] failed to move issue %s to '%s': %v", issueID, factory.DoneStatus, err)
				} else {
					log.Printf("[factory] moved issue %s to '%s'", issueID, factory.DoneStatus)
				}
			}
		}
	}

	// Keep the claw running — it stays connected to watch for CI failures,
	// bugbot comments, and PR events. The claw is terminated when the PR is
	// merged; if it is closed without merge, the claw is notified and decides
	// what to do (polled by checkPRMerged).
	// Just mark it as 'watching' so the UI shows it differently.
	res, err := s.db.Exec(`UPDATE claws SET status='idle' WHERE id=? AND status NOT IN ('deleted','error')`, clawID)
	if err != nil {
		return
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil || rowsAffected == 0 {
		return
	}
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "idle"},
	})
	// Notify the claw it's in watch mode — skip if the pipeline already injected a message
	if !pipelineHandledDone {
		s.injectUserMessage(clawID, "PR created and Linear issue updated. Staying connected to watch for CI failures and review comments. Will terminate when PR is merged; if it is closed without merge, I'll notify you and decide next steps.")
	}
}

// extractDonePRURLs parses PR URLs from a [DONE] message.
// It finds the [DONE] token and returns all github.com PR URLs that follow it on the same line.
func extractDonePRURLs(message string) []string {
	for _, line := range strings.Split(message, "\n") {
		if idx := strings.Index(line, "[DONE]"); idx >= 0 {
			rest := line[idx+len("[DONE]"):]
			var urls []string
			for _, pr := range extractPRs(rest) {
				urls = append(urls, pr.url)
			}
			return urls
		}
	}
	return nil
}

// validateDonePRs checks that every PR URL in the [DONE] signal refers to an open PR.
// Returns (true, reason) if validation fails and the claw should be nudged to retry.
func (s *Server) validateDonePRs(clawID string, prURLs []string, ghToken string) (rejected bool, reason string) {
	if len(prURLs) == 0 {
		return true, "[factory] `[DONE]` received with no PR URLs. Please open a PR on a feature branch and resend:\n```\n[DONE] https://github.com/org/repo/pull/N\n```"
	}

	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}

	var problems []string
	for _, pr := range extractPRs(strings.Join(prURLs, " ")) {
		// Use a repo-scoped token so private repos are accessible
		tokenForPR := s.resolveGitHubTokenForRepo(pr.repo)
		if tokenForPR == "" {
			tokenForPR = ghToken // fallback to unscoped
		}
		data, err := githubAPIWithBase(base, fmt.Sprintf("repos/%s/pulls/%d", pr.repo, pr.number), tokenForPR)
		if err != nil {
			problems = append(problems, fmt.Sprintf("- could not fetch %s: %v", pr.url, err))
			continue
		}
		state, _ := data["state"].(string)
		if state != "open" {
			if state == "" {
				problems = append(problems, fmt.Sprintf("- PR not found: %s", pr.url))
			} else {
				problems = append(problems, fmt.Sprintf("- PR is `%s` (expected `open`): %s", state, pr.url))
			}
		}
	}

	if len(problems) == 0 {
		return false, ""
	}

	return true, fmt.Sprintf(
		"[factory] `[DONE]` rejected — PR validation failed:\n%s\n\nFix the issue and resend `[DONE] <pr-url>`.",
		strings.Join(problems, "\n"),
	)
}

func (s *Server) findFactoryForIssue(issueID string) *types.FactoryConfig {
	// First, try to find the factory that created this claw by looking up the factory tag
	var tagsJSON string
	if err := s.db.QueryRow(`SELECT tags FROM claws WHERE linear_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&tagsJSON); err == nil {
		var tags []string
		if json.Unmarshal([]byte(tagsJSON), &tags) == nil {
			for _, tag := range tags {
				if strings.HasPrefix(tag, "factory:") {
					factoryName := strings.TrimPrefix(tag, "factory:")
					for _, factory := range s.hubCfg.Factories {
						if factory.Name == factoryName {
							return factory
						}
					}
				}
			}
		}
	}

	// Fallback: Extract team key from issue ID (e.g. "ELA" from "ELA-123", "sc" from "sc-123")
	parts := strings.SplitN(issueID, "-", 2)
	if len(parts) != 2 {
		return nil
	}
	teamKey := parts[0]

	// Determine expected integration type based on issue ID format
	expectedIntegration := "linear"
	if teamKey == "sc" {
		expectedIntegration = "shortcut"
	}

	for _, factory := range s.hubCfg.Factories {
		if factory.Integration != expectedIntegration {
			continue
		}
		if factory.Team == "" || strings.EqualFold(factory.Team, teamKey) {
			return factory
		}
	}
	return nil
}

// moveLinearIssueOnServer updates a Linear issue's state, using the server's configured
// linearBaseURL override if set (for testing).
func (s *Server) moveLinearIssueOnServer(token, issueIdentifier, targetStateName string) error {
	base := s.linearBaseURL
	if base == "" {
		base = "https://api.linear.app"
	}
	return moveLinearIssueWithBase(base, token, issueIdentifier, targetStateName)
}

// moveLinearIssueWithBase updates a Linear issue's state against a custom base URL (for testing).
func moveLinearIssueWithBase(baseURL, token, issueIdentifier, targetStateName string) error {
	// First find the issue ID from identifier using GraphQL variables
	queryBody := map[string]interface{}{
		"query": "query($id: String!) { issue(id: $id) { id team { states { nodes { id name } } } } }",
		"variables": map[string]string{
			"id": issueIdentifier,
		},
	}
	queryJSON, _ := json.Marshal(queryBody)
	req, _ := http.NewRequest("POST", baseURL+"/graphql", strings.NewReader(string(queryJSON)))
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			Issue struct {
				ID   string `json:"id"`
				Team struct {
					States struct {
						Nodes []struct {
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"states"`
				} `json:"team"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}

	issueID := result.Data.Issue.ID
	var stateID string
	for _, s := range result.Data.Issue.Team.States.Nodes {
		if strings.EqualFold(s.Name, targetStateName) {
			stateID = s.ID
			break
		}
	}
	if issueID == "" || stateID == "" {
		return fmt.Errorf("issue or state not found")
	}

	// Update the issue state using GraphQL variables
	mutationBody := map[string]interface{}{
		"query": "mutation($id: String!, $stateId: String!) { issueUpdate(id: $id, input: { stateId: $stateId }) { success } }",
		"variables": map[string]string{
			"id":      issueID,
			"stateId": stateID,
		},
	}
	mutationJSON, _ := json.Marshal(mutationBody)
	req2, _ := http.NewRequest("POST", baseURL+"/graphql", strings.NewReader(string(mutationJSON)))
	req2.Header.Set("Authorization", token)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return err
	}
	resp2.Body.Close()
	return nil
}

// logFactoryEvent records a factory webhook event and prunes entries older than 4 hours.
func (s *Server) logFactoryEvent(factoryName, issueID, issueTitle, prevStatus, newStatus, action, clawID, detail string) {
	id := uuid.New().String()
	_, _ = s.db.Exec(
		`INSERT INTO factory_events(id,factory_name,issue_id,issue_title,prev_status,new_status,action,claw_id,detail,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, factoryName, issueID, issueTitle, prevStatus, newStatus, action, clawID, detail, now(),
	)
	// Prune events older than 4 hours
	_, _ = s.db.Exec(`DELETE FROM factory_events WHERE created_at < datetime('now', '-4 hours')`)
}

// handleFactoryEvents serves GET /api/factories/:name/events
func (s *Server) handleFactoryEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Path: /api/factories/:name/events
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/factories/"), "/")
	if len(parts) < 2 || parts[1] != "events" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	factoryName := parts[0]

	rows, err := s.db.Query(
		`SELECT id, factory_name, issue_id, issue_title, prev_status, new_status, action, claw_id, detail, created_at
		 FROM factory_events WHERE factory_name = ? ORDER BY created_at DESC LIMIT 200`,
		factoryName,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type FactoryEvent struct {
		ID          string `json:"id"`
		FactoryName string `json:"factoryName"`
		IssueID     string `json:"issueId"`
		IssueTitle  string `json:"issueTitle"`
		PrevStatus  string `json:"prevStatus"`
		NewStatus   string `json:"newStatus"`
		Action      string `json:"action"`
		ClawID      string `json:"clawId"`
		Detail      string `json:"detail"`
		CreatedAt   string `json:"createdAt"`
	}

	var events []FactoryEvent
	for rows.Next() {
		var e FactoryEvent
		if err := rows.Scan(&e.ID, &e.FactoryName, &e.IssueID, &e.IssueTitle, &e.PrevStatus, &e.NewStatus, &e.Action, &e.ClawID, &e.Detail, &e.CreatedAt); err != nil {
			continue
		}
		events = append(events, e)
	}
	if events == nil {
		events = []FactoryEvent{}
	}
	jsonOK(w, events)
}

// linearIssueDetails holds the fields we fetch for pipeline template rendering.
type linearIssueDetails struct {
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// fetchLinearIssueDetails looks up an issue by its Linear identifier (e.g. "CAN-61")
// and returns the fields needed for pipeline template rendering.
func (s *Server) fetchLinearIssueDetails(token, issueIdentifier string) (*linearIssueDetails, error) {
	base := s.linearBaseURL
	if base == "" {
		base = "https://api.linear.app"
	}

	// Log key prefix for debugging (never log full key)
	keyPrefix := "<empty>"
	if len(token) > 12 {
		keyPrefix = token[:12] + "..."
	} else if len(token) >= 4 {
		keyPrefix = token[:4] + "..."
	} else if token != "" {
		keyPrefix = token + "..."
	}
	log.Printf("[linear] fetchLinearIssueDetails: issue=%s base=%s keyPrefix=%s", issueIdentifier, base, keyPrefix)

	// Linear's issue(id:) expects a UUID, not a display identifier like "CAN-61".
	// Use issues() with the identifier argument directly (not inside a filter).
	log.Printf("[linear] fetchLinearIssueDetails: building GraphQL query for identifier=%q", issueIdentifier)
	queryBody := map[string]interface{}{
		"query": "query($identifier: String!) { issues(identifier: $identifier) { nodes { identifier title url description } } }",
		"variables": map[string]string{
			"identifier": issueIdentifier,
		},
	}
	queryJSON, _ := json.Marshal(queryBody)
	req, err := http.NewRequest("POST", base+"/graphql", strings.NewReader(string(queryJSON)))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[linear] fetchLinearIssueDetails HTTP error for %s: %v", issueIdentifier, err)
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)
	if len(bodyStr) > 500 {
		bodyStr = bodyStr[:500] + "... [truncated]"
	}
	log.Printf("[linear] fetchLinearIssueDetails response for %s: status=%d len=%d body=%q", issueIdentifier, resp.StatusCode, len(bodyBytes), bodyStr)
	if resp.StatusCode != 200 {
		log.Printf("[linear] fetchLinearIssueDetails NON-200 for %s: status=%d fullBody=%s", issueIdentifier, resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		Data struct {
			Issues struct {
				Nodes []linearIssueDetails `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		log.Printf("[linear] fetchLinearIssueDetails JSON decode error for %s: %v", issueIdentifier, err)
		return nil, err
	}
	if len(result.Errors) > 0 {
		var errMsgs []string
		for _, e := range result.Errors {
			errMsgs = append(errMsgs, e.Message)
		}
		log.Printf("[linear] fetchLinearIssueDetails GraphQL errors for %s: %s", issueIdentifier, strings.Join(errMsgs, "; "))
	}
	if len(result.Data.Issues.Nodes) == 0 {
		log.Printf("[linear] fetchLinearIssueDetails: no issue found for %s", issueIdentifier)
		return nil, fmt.Errorf("issue %s not found", issueIdentifier)
	}
	issue := result.Data.Issues.Nodes[0]
	log.Printf("[linear] fetchLinearIssueDetails success for %s: id=%s title=%s", issueIdentifier, issue.Identifier, issue.Title)
	return &issue, nil
}
