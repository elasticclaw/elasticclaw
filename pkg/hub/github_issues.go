package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

// githubIssuesWebhookPayload holds the relevant fields from a GitHub issues webhook event.
type githubIssuesWebhookPayload struct {
	Action string `json:"action"` // "opened", "edited", "closed", "reopened", "labeled", "unlabeled", "assigned", "unassigned"
	Issue  struct {
		ID          int64  `json:"id"`
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
		State       string `json:"state"`                  // "open", "closed"
		StateReason string `json:"state_reason,omitempty"` // "completed", "not_planned", "reopened"
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignee *struct {
			Login string `json:"login"`
		} `json:"assignee"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"issue"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "Bot", "User", "Organization"
	} `json:"sender"`
	Label *struct {
		Name string `json:"name"`
	} `json:"label,omitempty"`
	Changes *struct {
		Title *struct {
			From string `json:"from"`
		} `json:"title,omitempty"`
		Body *struct {
			From string `json:"from"`
		} `json:"body,omitempty"`
	} `json:"changes,omitempty"`
	// GitHub sends X-GitHub-Delivery header as unique delivery ID
}

func (s *Server) handleGitHubIssuesWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		log.Printf("[github-issues-webhook] %s %s → 405 method not allowed", r.Method, r.URL.Path)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	if workspaceName == "" {
		log.Printf("[github-issues-webhook] %s ignored; use /api/workspaces/{workspace}/webhooks/github-issues", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		log.Printf("[github-issues-webhook] %s %s → 400 read error: %v", r.Method, r.URL.Path, err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Validate signature using X-Hub-Signature-256 header
	sig := r.Header.Get("X-Hub-Signature-256")
	reason := s.validateGitHubIssuesSignatureReason(workspaceName, body, sig)
	if reason != "" {
		log.Printf("[github-issues-webhook] %s %s → 401 %s (sig present=%v)", r.Method, r.URL.Path, reason, sig != "")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var payload githubIssuesWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	// Only handle issues events (not pull_request, etc.)
	// The webhook should be configured to only send issues events, but guard anyway
	if payload.Issue.Number == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Dedup: ignore duplicate webhook deliveries using GitHub's delivery ID
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID != "" && s.isDuplicateWebhook("gh-issues:"+deliveryID) {
		log.Printf("[github-issues-webhook] dedup: skipping duplicate delivery %s for issue #%d in %s", deliveryID, payload.Issue.Number, payload.Repository.FullName)
		w.WriteHeader(http.StatusOK)
		return
	}

	go s.processGitHubIssuesEvent(workspaceName, payload)
	w.WriteHeader(http.StatusOK)
}

// validateGitHubIssuesSignatureReason validates the webhook signature and returns
// an empty string on success, or a human-readable reason on failure.
func (s *Server) validateGitHubIssuesSignatureReason(workspaceName string, body []byte, sig string) string {
	workspaceSecretCount := 0
	for _, secret := range workspaceIssueTrackerWebhookSecrets(workspaceName, "github-issues") {
		workspaceSecretCount++
		if verifyGitHubHMAC(body, sig, secret) {
			return ""
		}
	}
	if workspaceSecretCount > 0 {
		return fmt.Sprintf("signature did not match any configured workspace webhook secret (%d checked)", workspaceSecretCount)
	}
	return ""
}

func verifyGitHubHMAC(body []byte, sig, secret string) bool {
	if sig == "" || secret == "" {
		return false
	}
	// Strip "sha256=" prefix if present
	sig = strings.TrimPrefix(sig, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}

func (s *Server) processGitHubIssuesEvent(workspaceName string, payload githubIssuesWebhookPayload) {
	issueID := fmt.Sprintf("%s/%d", payload.Repository.FullName, payload.Issue.Number)
	log.Printf("[github-issues-webhook] processing event: action=%q issue=%s title=%q sender=%q",
		payload.Action, issueID, payload.Issue.Title, payload.Sender.Login)

	workflowWorkspaces, _ := loadExternalWorkflowsByIntegration("github-issues")
	workflowWorkspaces = filterWorkflowWorkspacesByName(workflowWorkspaces, workspaceName)
	if len(workflowWorkspaces) == 0 {
		log.Printf("[github-issues-webhook] no workflows configured — nothing to do")
		return
	}
	log.Printf("[github-issues-webhook] checking %d workflow workspace(s)", len(workflowWorkspaces))

	currentStatus := payload.Issue.State
	previousStatus := ""

	// For state changes, track the transition
	if payload.Action == "closed" {
		previousStatus = "open"
	} else if payload.Action == "reopened" {
		previousStatus = "closed"
	}

	issueTitle := payload.Issue.Title
	assignee := ""
	if payload.Issue.Assignee != nil {
		assignee = payload.Issue.Assignee.Login
	}

	// Build label set
	issueLabels := map[string]bool{}
	for _, l := range payload.Issue.Labels {
		issueLabels[strings.ToLower(l.Name)] = true
	}
	log.Printf("[github-issues-webhook] issue %s labels=%v assignee=%q currentStatus=%q previousStatus=%q",
		issueID, func() []string {
			out := make([]string, 0, len(issueLabels))
			for k := range issueLabels {
				out = append(out, k)
			}
			return out
		}(), assignee, currentStatus, previousStatus)

	// Handle label/unlabel actions: the label that was added/removed is in payload.Label
	// For "labeled", build the pre-event label set so previousMatched works correctly
	// when the trigger is a label name (e.g., "bug") and the issue already had it.
	previousLabels := make(map[string]bool, len(issueLabels))
	for k, v := range issueLabels {
		previousLabels[k] = v
	}
	if payload.Action == "labeled" && payload.Label != nil {
		// Remove the newly-added label from the previous set
		delete(previousLabels, strings.ToLower(payload.Label.Name))
		issueLabels[strings.ToLower(payload.Label.Name)] = true
		// Treat labeling as a status change for factory matching
		if previousStatus == "" {
			previousStatus = currentStatus
		}
		log.Printf("[github-issues-webhook] label '%s' added by %q — previousLabels=%v",
			payload.Label.Name, payload.Sender.Login, func() []string {
				out := make([]string, 0, len(previousLabels))
				for k := range previousLabels {
					out = append(out, k)
				}
				return out
			}())
	}
	if payload.Action == "unlabeled" && payload.Label != nil {
		delete(issueLabels, strings.ToLower(payload.Label.Name))
		if previousStatus == "" {
			previousStatus = currentStatus
		}
	}

	if !s.processGitHubIssuesWorkflowEvent(workflowWorkspaces, payload, issueID, issueTitle, currentStatus, previousStatus, previousLabels, issueLabels, assignee) {
		log.Printf("[github-issues-webhook] no workflow matched for issue %s", issueID)
	}
}

func githubIssuesTriggerRepos(factory *types.FactoryConfig) []string {
	if len(factory.TriggerRepos) > 0 {
		return factory.TriggerRepos
	}
	// Backward compatibility for older GitHub Issues factory configs.
	return factory.Repos
}

func githubIssuesWorkflowTriggerRepos(workflow *types.WorkflowConfig) []string {
	if workflow.Trigger != nil && len(workflow.Trigger.Repositories) > 0 {
		return workflow.Trigger.Repositories
	}
	if len(workflow.TriggerRepos) > 0 {
		return workflow.TriggerRepos
	}
	return workflow.Repos
}

func (s *Server) processGitHubIssuesWorkflowEvent(workspaces []*types.WorkspaceConfig, payload githubIssuesWebhookPayload, issueID, issueTitle, currentStatus, previousStatus string, previousLabels, issueLabels map[string]bool, assignee string) bool {
	matched := false
	for _, workspace := range workspaces {
		for _, workflow := range workspace.Workflows {
			if workflow == nil {
				continue
			}
			if workflow.Enabled != nil && !*workflow.Enabled {
				continue
			}
			if !githubRepoMatches(payload.Repository.FullName, githubIssuesWorkflowTriggerRepos(workflow)) {
				continue
			}
			if workflow.AssignedTo != "" && !assignedToMatches(workflow.AssignedTo, assignee) {
				continue
			}

			if workflow.Trigger != nil {
				if !githubIssuesWorkflowTriggerMatches(workflow.Trigger, payload, currentStatus, issueLabels) {
					continue
				}
			} else {
				if len(workflow.Labels) > 0 {
					allMatch := true
					for _, required := range workflow.Labels {
						if !issueLabels[strings.ToLower(required)] {
							allMatch = false
							break
						}
					}
					if !allMatch {
						continue
					}
				}
				if len(workflow.AllowedLabelers) > 0 && (payload.Action == "labeled" || payload.Action == "unlabeled") {
					allowed := false
					for _, labeler := range workflow.AllowedLabelers {
						if strings.EqualFold(labeler, payload.Sender.Login) {
							allowed = true
							break
						}
					}
					if !allowed {
						continue
					}
				}
				triggerStatus := workflow.TriggerStatus
				if triggerStatus == "" {
					triggerStatus = "open"
				}
				triggerMatched := strings.EqualFold(currentStatus, triggerStatus) || issueLabels[strings.ToLower(triggerStatus)]
				previousMatched := previousStatus != "" && strings.EqualFold(previousStatus, triggerStatus)
				if !previousMatched && (payload.Action == "labeled" || payload.Action == "unlabeled") {
					previousMatched = previousLabels[strings.ToLower(triggerStatus)]
				}
				if !triggerMatched || previousMatched {
					continue
				}
			}
			matched = true
			log.Printf("[workflow:%s/%s] GitHub issue %s matched workflow trigger — creating claw", workspace.Name, workflow.Name, issueID)
			if err := s.createClawForGitHubIssueWorkflow(workspace, workflow, payload, "github-issues webhook"); err != nil {
				log.Printf("[workflow:%s/%s] failed to create claw for %s: %v", workspace.Name, workflow.Name, issueID, err)
			}
		}
	}
	return matched
}

func githubIssuesWorkflowTriggerMatches(trigger *types.WorkflowTrigger, payload githubIssuesWebhookPayload, currentStatus string, issueLabels map[string]bool) bool {
	if trigger == nil || trigger.Type != "github_issues" {
		return false
	}
	switch trigger.Event {
	case "issue_labeled":
		if payload.Action != "labeled" {
			return false
		}
	case "issue_opened":
		if payload.Action != "opened" {
			return false
		}
	case "issue_reopened":
		if payload.Action != "reopened" {
			return false
		}
	case "issue_edited":
		if payload.Action != "edited" {
			return false
		}
	case "issue_assigned":
		if payload.Action != "assigned" {
			return false
		}
	case "issue_unassigned":
		if payload.Action != "unassigned" {
			return false
		}
	default:
		return false
	}
	if len(trigger.States) > 0 {
		stateMatched := false
		for _, state := range trigger.States {
			if strings.EqualFold(currentStatus, state) {
				stateMatched = true
				break
			}
		}
		if !stateMatched {
			return false
		}
	}
	for _, required := range trigger.Labels {
		if !issueLabels[strings.ToLower(required)] {
			return false
		}
	}
	if len(trigger.Labelers) > 0 && (payload.Action == "labeled" || payload.Action == "unlabeled") {
		for _, labeler := range trigger.Labelers {
			if labeler == "*" || strings.EqualFold(labeler, payload.Sender.Login) {
				return true
			}
		}
		return false
	}
	return true
}

func assignedToMatches(filter, assignee string) bool {
	wanted := strings.ToLower(strings.TrimSpace(filter))
	switch {
	case wanted == "":
		return true
	case wanted == "any":
		return assignee != ""
	case wanted == "none":
		return assignee == ""
	case strings.HasPrefix(wanted, "!"):
		excluded := strings.TrimPrefix(strings.TrimPrefix(wanted, "!"), "@")
		return !strings.EqualFold(assignee, excluded)
	default:
		target := strings.TrimPrefix(wanted, "@")
		return strings.EqualFold(assignee, target)
	}
}

func (s *Server) createClawForGitHubIssueWorkflow(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, payload githubIssuesWebhookPayload, reason string) error {
	issueID := fmt.Sprintf("%s/%d", payload.Repository.FullName, payload.Issue.Number)
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claws WHERE github_issue_id=? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&existing)
	if existing != "" {
		log.Printf("[workflow:%s/%s] GitHub issue %s already has active claw %s", workspace.Name, workflow.Name, issueID, existing[:8])
		return nil
	}

	triggerKey := factoryTriggerKey("github-issues", issueID)
	triggerOwner := fmt.Sprintf("workflow:%s/%s", workspace.Name, workflow.Name)
	claimed, err := s.claimFactoryTrigger(triggerOwner, "github-issues", triggerKey, reason, map[string]string{
		"issue_id":  issueID,
		"source":    reason,
		"workspace": workspace.Name,
		"workflow":  workflow.Name,
	})
	if err != nil {
		return fmt.Errorf("claim workflow trigger: %w", err)
	}
	if !claimed {
		log.Printf("[workflow:%s/%s] GitHub issue %s already has an active trigger claim", workspace.Name, workflow.Name, issueID)
		return nil
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(triggerOwner, "github-issues", triggerKey)
		}
	}()

	templateFiles := cloneStringMap(workspace.Files)
	templateFiles["CONTEXT.md"] = buildGitHubIssuesContext(payload)

	clawName := issueID
	if workflow.NamePattern != "" {
		clawName = strings.ReplaceAll(workflow.NamePattern, "{issue_id}", issueID)
		clawName = strings.ReplaceAll(clawName, "{issue_number}", fmt.Sprintf("%d", payload.Issue.Number))
		clawName = strings.ReplaceAll(clawName, "{repo}", payload.Repository.FullName)
	}

	clawID, _, err := s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{
		workspaceFiles: templateFiles,
		clawName:       clawName,
		githubIssueID:  issueID,
		reason:         reason,
	})
	if err != nil {
		return err
	}
	if err := s.completeFactoryTrigger(triggerOwner, "github-issues", triggerKey, clawID); err != nil {
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		return fmt.Errorf("complete workflow trigger: %w", err)
	}
	claimOpen = false
	return nil
}

func (s *Server) createClawForGitHubIssue(factory *types.FactoryConfig, payload githubIssuesWebhookPayload, reason string) error {
	issueID := fmt.Sprintf("%s/%d", payload.Repository.FullName, payload.Issue.Number)
	triggerKey := factoryTriggerKey("github-issues", issueID)

	claimed, err := s.claimFactoryTrigger(factory.Name, "github-issues", triggerKey, reason, map[string]string{
		"issue_id": issueID,
		"source":   reason,
	})
	if err != nil {
		return fmt.Errorf("claim factory trigger: %w", err)
	}
	if !claimed {
		if reason != "poll" {
			log.Printf("[factory:%s] GitHub issue %s already has an active trigger claim — treating as idempotent success", factory.Name, issueID)
		}
		return errFactoryTriggerAlreadyClaimed
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(factory.Name, "github-issues", triggerKey)
		}
	}()

	// Verify we can read the issue before spending money on a sandbox.
	ghToken := s.resolveGitHubIssuesTokenForFactory(factory)
	if ghToken != "" {
		// Quick pre-flight: can we fetch the issue?
		base := s.githubBaseURL
		if base == "" {
			base = "https://api.github.com"
		}
		_, err := githubAPIWithBase(base, fmt.Sprintf("repos/%s/issues/%d", payload.Repository.FullName, payload.Issue.Number), ghToken)
		if err != nil {
			return fmt.Errorf("cannot read issue: %w", err)
		}
	}

	// Load template
	templateFiles, err := s.resolveTemplateFiles(factory.Template)
	if err != nil {
		return err
	}
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		var parseErr error
		tmplCfg, parseErr = config.ParseTemplateConfig([]byte(cfgContent))
		if parseErr != nil {
			log.Printf("[factory:%s] warning: elasticclaw-config.yaml parse error: %v", factory.Name, parseErr)
			tmplCfg = nil
		}
	}

	// Build context file
	ctxFile := buildGitHubIssuesContext(payload)
	templateFiles["CONTEXT.md"] = ctxFile

	// Build claw name
	clawName := issueID
	if factory.NamePattern != "" {
		clawName = strings.ReplaceAll(factory.NamePattern, "{issue_id}", issueID)
		clawName = strings.ReplaceAll(clawName, "{issue_number}", fmt.Sprintf("%d", payload.Issue.Number))
		clawName = strings.ReplaceAll(clawName, "{repo}", payload.Repository.FullName)
	}

	// Find tenant
	var tenantID string
	if err := s.db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		return fmt.Errorf("no tenant: %w", err)
	}

	// Find provider
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

	// Build env vars
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_TOKEN": s.hubCfg.ClawToken,
	}
	if ghToken != "" {
		env["GITHUB_ISSUES_API_KEY"] = ghToken
		// Note: We intentionally do NOT set GITHUB_TOKEN here. The credential
		// helper provides a proper GitHub App token for git operations. Setting
		// GITHUB_TOKEN would override the credential helper and cause git/gh to
		// use the issue-reading PAT which lacks repo permissions.
	}

	// Resolve and inject template-requested secrets
	resolvedSecrets := make(map[string]string)
	if tmplCfg != nil && len(tmplCfg.Secrets) > 0 {
		log.Printf("[factory:%s] DEPRECATED: template %q uses 'secrets:' list — migrate to 'secret_refs:' map", factory.Name, factory.Template)
		for _, ref := range tmplCfg.Secrets {
			secretVal, envName, ok := s.resolveSecretRef(ref, factory)
			if ok {
				env[envName] = secretVal
				resolvedSecrets[envName] = secretVal
				log.Printf("[factory:%s] injected template secret %s as %s into claw env", factory.Name, ref.Type, envName)
			} else {
				log.Printf("[factory:%s] warning: requested secret (type=%s name=%s workspace=%s) not found", factory.Name, ref.Type, ref.Name, ref.Workspace)
			}
		}
	}

	// Resolve and inject template-level secret_refs
	if tmplCfg != nil && len(tmplCfg.SecretRefs) > 0 {
		for envName, secretRef := range tmplCfg.SecretRefs {
			if val, ok := s.hubCfg.Secrets[secretRef]; ok {
				env[envName] = val
				resolvedSecrets[envName] = val
				log.Printf("[factory:%s] injected template secret_ref %s as %s into claw env", factory.Name, secretRef, envName)
			} else {
				log.Printf("[factory:%s] WARNING: template secret_ref %q not found in hub secrets", factory.Name, secretRef)
			}
		}
	}

	// Resolve and inject factory-level secret_refs (factory overrides template)
	if len(factory.SecretRefs) > 0 {
		for envName, secretRef := range factory.SecretRefs {
			if val, ok := s.hubCfg.Secrets[secretRef]; ok {
				env[envName] = val
				resolvedSecrets[envName] = val
				log.Printf("[factory:%s] injected factory secret_ref %s as %s into claw env", factory.Name, secretRef, envName)
			} else {
				log.Printf("[factory:%s] WARNING: secret_ref %q not found in hub secrets", factory.Name, secretRef)
			}
		}
	}

	// Resolve template config fields
	var (
		instanceType    string
		defaultModel    string
		llmKey          string
		nixEnabled      int
		dockerEnabled   int
		githubRepos     []types.GitHubRepoAccess
		linearWorkspace string
	)
	if tmplCfg != nil {
		instanceType = tmplCfg.InstanceType
		defaultModel = tmplCfg.DefaultModel
		llmKey = tmplCfg.LLMKey
		if tmplCfg.Nix {
			nixEnabled = 1
		}
		if tmplCfg.Docker {
			dockerEnabled = 1
		}
		if tmplCfg.GitHub != nil {
			githubRepos = tmplCfg.GitHub.Repos
		}
		if tmplCfg.Linear != nil {
			linearWorkspace = tmplCfg.Linear.Workspace
		}
	}

	// Build SECRETS.md
	var secretList []string
	if ghToken != "" {
		secretList = append(secretList, "- `GITHUB_ISSUES_API_KEY` — GitHub Issues API token")
		// Note: GITHUB_TOKEN is intentionally NOT set — the credential helper
		// provides the proper token for git operations. See issue #181.
	}
	for envName := range resolvedSecrets {
		var refType string
		if tmplCfg != nil {
			for _, ref := range tmplCfg.Secrets {
				if ref.EnvVarName() == envName {
					refType = ref.Type
					break
				}
			}
			if refType == "" {
				if _, ok := tmplCfg.SecretRefs[envName]; ok {
					refType = "template secret_ref"
				}
			}
		}
		if refType == "" {
			if _, ok := factory.SecretRefs[envName]; ok {
				refType = "factory secret_ref"
			}
		}
		if refType == "" {
			refType = "custom"
		}
		secretList = append(secretList, fmt.Sprintf("- `%s` — %s secret", envName, refType))
	}
	if len(secretList) > 0 {
		templateFiles["SECRETS.md"] = "# Available Secrets\n\n" + strings.Join(secretList, "\n") + "\n\n> Do not write these values to files. They are injected as environment variables.\n"
	}
	templateFiles = injectFigmaAPIDocs(templateFiles, env)

	// Build tags
	tags := mergeTags(factory.Template, factory.Tags, nil)
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

	// Color
	clawColor := factory.Color
	if clawColor == "" && tmplCfg != nil {
		clawColor = tmplCfg.Color
	}

	githubReposJSON, _ := json.Marshal(githubRepos)

	// Check concurrency limit — serialize with promoteMu to prevent TOCTOU
	// race where concurrent factory webhooks both read active < max and both
	// insert as provisioning, exceeding the limit.
	// Check concurrency limit (group-aware)
	s.promoteMu.Lock()

	s.mu.RLock()
	groupName, groupLimit := s.resolveGroupLimit(factory)
	s.mu.RUnlock()

	activeCount := s.countActiveClawsInGroup(groupName)
	isPending := false
	if groupLimit > 0 && activeCount >= groupLimit {
		isPending = true
		log.Printf("[factory] concurrency limit reached for group %q (active=%d, limit=%d) — queueing claw for GitHub issue %s as pending", groupName, activeCount, groupLimit, issueID)
	}

	// Insert claw record
	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(templateFiles)
	now := now()

	initialStatus := "provisioning"
	if isPending {
		initialStatus = "pending"
	}

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, linear_issue_id, github_issue_id, shortcut_story_id, status, created_at, factory_name, concurrency_group)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), clawColor, llmKey, "", issueID, "", initialStatus, now, factory.Name, groupName,
	)

	// Release promoteMu immediately after INSERT so we don't hold it across
	// the potentially slow async provisioning below.
	s.promoteMu.Unlock()

	if err != nil {
		return fmt.Errorf("db insert: %w", err)
	}
	if err := s.completeFactoryTrigger(factory.Name, "github-issues", triggerKey, clawID); err != nil {
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		return fmt.Errorf("complete factory trigger: %w", err)
	}
	claimOpen = false

	log.Printf("[factory] created claw %s (%s) for GitHub issue %s (status=%s, reason=%s)", clawName, clawID[:8], issueID, initialStatus, reason)
	// Notify connected dashboards immediately so the card appears
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": initialStatus},
	})

	// Move the issue to WorkingStatus if configured (only if not pending —
	// a queued claw hasn't actually started working yet)
	if !isPending && factory.WorkingStatus != "" {
		if ghToken != "" {
			rest := strings.TrimPrefix(issueID, "gh-")
			lastSlash := strings.LastIndex(rest, "/")
			if lastSlash > 0 {
				repo := rest[:lastSlash]
				issueNumStr := rest[lastSlash+1:]
				var issueNum int
				if _, err := fmt.Sscanf(issueNumStr, "%d", &issueNum); err == nil {
					base := s.githubBaseURL
					if base == "" {
						base = "https://api.github.com"
					}
					if err := moveGitHubIssue(ghToken, repo, issueNum, factory.WorkingStatus, base); err != nil {
						log.Printf("[factory] failed to move GitHub issue %s to working status '%s': %v", issueID, factory.WorkingStatus, err)
					} else {
						log.Printf("[factory] moved GitHub issue %s to working status '%s'", issueID, factory.WorkingStatus)
					}
				}
			}
		}
	}

	if isPending {
		return nil
	}

	// Provision asynchronously
	provCfg, _ := s.hubCfg.Providers[provider]
	go func() {
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
		case "exedev":
			provErr = s.provisionExedev(ctx, clawID, req, provCfg, fileBytes, env)
		case "noop":
			// Noop provider is not implemented; skip
		default:
			provErr = fmt.Errorf("unknown provider %s", provider)
		}

		if provErr != nil {
			log.Printf("[factory] claw %s provision failed: %v", clawID[:8], provErr)
			s.stopAgentWithReason(clawID, fmt.Sprintf("Factory provision failed: %v", provErr), false)
			return
		}
		log.Printf("[factory] claw %s provisioned successfully", clawID[:8])
	}()

	return nil
}

func (s *Server) terminateClawForGitHubIssue(issueID string) {
	var clawID, tenantID string
	if err := s.db.QueryRow(`SELECT id, tenant_id FROM claws WHERE github_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&clawID, &tenantID); err != nil {
		return
	}
	log.Printf("[factory] terminating claw %s for GitHub issue %s", clawID[:8], issueID)

	// Post a comment on the linked GitHub issue before terminating
	factory := s.findFactoryForIssue(issueID)
	if factory != nil && issueID != "" && factory.Integration == "github-issues" {
		parts := strings.Split(issueID, "/")
		if len(parts) == 3 {
			token := s.resolveGitHubIssuesTokenForFactory(factory)
			if token != "" {
				repo := parts[0] + "/" + parts[1]
				var issueNum int
				if _, err := fmt.Sscanf(parts[2], "%d", &issueNum); err == nil {
					if err := commentGitHubIssue(token, repo, issueNum, "Agent stopped: issue left trigger status"); err != nil {
						log.Printf("[factory] failed to comment GitHub issue %s: %v", issueID, err)
					} else {
						log.Printf("[factory] commented GitHub issue %s", issueID)
					}
				}
			}
		}
	}

	s.checkpointBeforeTermination(clawID, "issue-left-trigger")

	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "factory: issue left trigger status")
		delete(s.claws, clawID)
	}
	s.mu.Unlock()
	_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
	})
	// Promote any pending claws now that a slot is free
	go s.promotePendingClaws()
}

func (s *Server) resolveGitHubIssuesTokenForFactory(factory *types.FactoryConfig) string {
	if s.hubCfg.Integrations == nil {
		return ""
	}
	for _, gi := range s.hubCfg.Integrations.GitHubIssues {
		if factory.Workspace == "" || strings.EqualFold(gi.Workspace, factory.Workspace) {
			return gi.Token
		}
	}
	return ""
}

func buildGitHubIssuesContext(payload githubIssuesWebhookPayload) string {
	i := payload.Issue
	var b strings.Builder
	b.WriteString("# Issue Context\n\n")
	b.WriteString("This claw was automatically created by a factory to work on a GitHub issue. Read this, understand the task, then get to work.\n\n")
	b.WriteString(fmt.Sprintf("## Issue: #%d\n\n", i.Number))
	b.WriteString(fmt.Sprintf("**Title:** %s\n\n", i.Title))
	b.WriteString(fmt.Sprintf("**Repository:** %s\n\n", payload.Repository.FullName))
	if i.HTMLURL != "" {
		b.WriteString(fmt.Sprintf("**URL:** %s\n\n", i.HTMLURL))
	}
	if i.User.Login != "" {
		b.WriteString(fmt.Sprintf("**Author:** @%s\n\n", i.User.Login))
	}
	if len(i.Labels) > 0 {
		b.WriteString("**Labels:** ")
		for i, l := range i.Labels {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(l.Name)
		}
		b.WriteString("\n\n")
	}
	if i.Body != "" {
		b.WriteString("## Description\n\n")
		b.WriteString(i.Body)
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

// commentGitHubIssue adds a comment to a GitHub issue.
func commentGitHubIssue(token, repo string, issueNumber int, body string) error {
	base := "https://api.github.com"
	commentBody := map[string]interface{}{"body": body}
	b, _ := json.Marshal(commentBody)
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("%s/repos/%s/issues/%d/comments", base, repo, issueNumber),
		bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github API error %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// githubAPIPostWithBase makes a POST/PATCH/PUT request to the GitHub API.
func githubAPIPostWithBase(baseURL, path, token, method string, body interface{}) (map[string]interface{}, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+"/"+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github API %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("github API parse error: %w", err)
	}
	return result, nil
}

// findClawForGitHubIssue finds an active claw tracking the given GitHub issue URL.
func (s *Server) findClawForGitHubIssue(issueURL string) string {
	var clawID string
	_ = s.db.QueryRow(
		`SELECT id FROM claws WHERE github_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`,
		issueURL,
	).Scan(&clawID)
	return clawID
}

// closeGitHubIssueForClaw finds the tracked GitHub issue for a claw and closes it.
func (s *Server) closeGitHubIssueForClaw(clawID string) {
	var issueURL string
	err := s.db.QueryRow(
		`SELECT github_issue_id FROM claws WHERE id=?`,
		clawID,
	).Scan(&issueURL)
	if err != nil || issueURL == "" {
		log.Printf("[pipeline] close_issue: no tracked GitHub issue for claw %s", clawID[:8])
		return
	}

	// Parse owner/repo#number from the issue URL
	// URL format: https://github.com/owner/repo/issues/123
	parts := strings.Split(issueURL, "/")
	if len(parts) < 2 {
		log.Printf("[pipeline] close_issue: invalid issue URL %q", issueURL)
		return
	}
	// Extract owner/repo from URL
	var owner, repo string
	for i, p := range parts {
		if p == "github.com" && i+2 < len(parts) {
			owner = parts[i+1]
			repo = parts[i+2]
			break
		}
	}
	if owner == "" || repo == "" {
		log.Printf("[pipeline] close_issue: could not parse owner/repo from %q", issueURL)
		return
	}
	repoFullName := owner + "/" + repo

	// Extract issue number from URL
	var issueNumber int
	fmt.Sscanf(parts[len(parts)-1], "%d", &issueNumber)
	if issueNumber == 0 {
		log.Printf("[pipeline] close_issue: could not parse issue number from %q", issueURL)
		return
	}

	// Find token for this repo
	token := s.resolveGitHubIssuesTokenForRepo(repoFullName)
	if token == "" {
		log.Printf("[pipeline] close_issue: no GitHub token for repo %s", repoFullName)
		s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] close_issue: no GitHub token available for %s — cannot close issue.", repoFullName))
		return
	}

	baseURL := s.ghBaseURL()

	body, _ := json.Marshal(map[string]string{
		"state":        "closed",
		"state_reason": "completed",
	})
	_, err = githubAPIPostWithBase(baseURL, fmt.Sprintf("repos/%s/issues/%d", repoFullName, issueNumber), token, "PATCH", body)
	if err != nil {
		log.Printf("[pipeline] close_issue: failed to close issue %s#%d: %v", repoFullName, issueNumber, err)
		s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] close_issue: failed to close issue #%d: %v", issueNumber, err))
		return
	}

	log.Printf("[pipeline] close_issue: closed issue %s#%d", repoFullName, issueNumber)
	s.injectHubMessageByID(clawID, fmt.Sprintf("[hub] Closed GitHub issue #%d in %s.", issueNumber, repoFullName))
}

// resolveGitHubIssuesTokenForRepo resolves a GitHub token for a specific repo by checking
// factory configs and their matching integration workspaces.
func (s *Server) resolveGitHubIssuesTokenForRepo(repoFullName string) string {
	// Find a factory that matches this repo, then resolve its token.
	factories := s.resolveFactories()
	for _, factory := range factories {
		if factory.Integration != "github-issues" {
			continue
		}
		triggerRepos := githubIssuesTriggerRepos(factory)
		if len(triggerRepos) > 0 && !githubRepoMatches(repoFullName, triggerRepos) {
			continue
		}
		token := s.resolveGitHubIssuesTokenForFactory(factory)
		if token != "" {
			return token
		}
	}

	// Fallback: return any global token.
	s.mu.RLock()
	integrations := s.hubCfg.Integrations
	s.mu.RUnlock()
	if integrations != nil {
		for _, gi := range integrations.GitHubIssues {
			if gi.Token != "" {
				return gi.Token
			}
		}
	}

	return ""
}

// moveGitHubIssue updates a GitHub issue's state or labels.
func moveGitHubIssue(token, repo string, issueNumber int, targetState string, baseURL string) error {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	// targetState can be "open", "closed", or a label name
	if strings.EqualFold(targetState, "open") || strings.EqualFold(targetState, "closed") {
		state := strings.ToLower(targetState)
		body := map[string]string{"state": state}
		if state == "closed" {
			body["state_reason"] = "completed"
		}
		_, err := githubAPIPostWithBase(baseURL, fmt.Sprintf("repos/%s/issues/%d", repo, issueNumber), token, "PATCH", body)
		return err
	}
	// Otherwise treat as label addition
	_, err := githubAPIPostWithBase(baseURL, fmt.Sprintf("repos/%s/issues/%d/labels", repo, issueNumber), token, "POST", map[string][]string{"labels": {targetState}})
	return err
}
