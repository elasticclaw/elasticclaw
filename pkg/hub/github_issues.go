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
		ID        int64  `json:"id"`
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		HTMLURL   string `json:"html_url"`
		State     string `json:"state"` // "open", "closed"
		StateReason string `json:"state_reason,omitempty"` // "completed", "not_planned", "reopened"
		Labels    []struct {
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Validate signature using X-Hub-Signature-256 header
	sig := r.Header.Get("X-Hub-Signature-256")
	if !s.validateGitHubIssuesSignature(body, sig) {
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

	go s.processGitHubIssuesEvent(payload)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) validateGitHubIssuesSignature(body []byte, sig string) bool {
	s.mu.RLock()
	integrations := s.hubCfg.Integrations
	factories := s.hubCfg.Factories
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	hasAnySecret := false

	// Check integration-level secrets
	if integrations != nil {
		for _, gi := range integrations.GitHubIssues {
			if gi.WebhookSecret == "" {
				continue
			}
			hasAnySecret = true
			if verifyGitHubHMAC(body, sig, gi.WebhookSecret) {
				return true
			}
		}
	}

	// Check factory-level secrets
	if factories != nil {
		for _, factory := range factories {
			if factory.Integration != "github-issues" {
				continue
			}
			secret := factory.WebhookSecret
			if secret == "" && factory.WebhookSecretRef != "" && secrets != nil {
				secret = secrets[factory.WebhookSecretRef]
			}
			if secret == "" {
				continue
			}
			hasAnySecret = true
			if verifyGitHubHMAC(body, sig, secret) {
				return true
			}
		}
	}

	// If any secrets are configured but none matched, reject.
	// Also reject if there are GitHub Issues integrations configured but
	// none have secrets set — unauthenticated webhooks are a security risk.
	if hasAnySecret {
		return false
	}
	if integrations != nil && len(integrations.GitHubIssues) > 0 {
		return false
	}
	// No integrations and no factory secrets — allow (dev/testing)
	return true
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

func (s *Server) processGitHubIssuesEvent(payload githubIssuesWebhookPayload) {
	issueID := fmt.Sprintf("%s/%d", payload.Repository.FullName, payload.Issue.Number)
	log.Printf("[github-issues-webhook] processing event: action=%q issue=%s title=%q sender=%q",
		payload.Action, issueID, payload.Issue.Title, payload.Sender.Login)

	if s.hubCfg.Factories == nil {
		log.Printf("[github-issues-webhook] no factories configured — nothing to do")
		return
	}
	log.Printf("[github-issues-webhook] checking %d factories", len(s.hubCfg.Factories))

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

	matched := false
	for _, factory := range s.hubCfg.Factories {
		log.Printf("[github-issues-webhook] checking factory %q: integration=%q enabled=%v trigger_status=%q labels=%v assigned_to=%q allowed_labelers=%v",
			factory.Name, factory.Integration,
			func() bool {
				if factory.Enabled == nil {
					return true
				}
				return *factory.Enabled
			}(),
			factory.TriggerStatus, factory.Labels, factory.AssignedTo, factory.AllowedLabelers)

		if factory.Integration != "github-issues" {
			log.Printf("[github-issues-webhook] factory %q: SKIP — integration=%q, want github-issues", factory.Name, factory.Integration)
			continue
		}
		if factory.Enabled != nil && !*factory.Enabled {
			log.Printf("[github-issues-webhook] factory %q: SKIP — disabled", factory.Name)
			continue
		}

		// Workspace filter: for Linear/Shortcut, workspace selects the integration token.
		// For GitHub Issues, workspace is just a human label — no token resolution needed.
		if factory.Workspace != "" && factory.Integration != "github-issues" {
			ghToken := s.resolveGitHubIssuesTokenForFactory(factory)
			if ghToken == "" {
				log.Printf("[github-issues-webhook] factory %q: SKIP — workspace=%q but no token resolved", factory.Name, factory.Workspace)
				continue
			}
			log.Printf("[github-issues-webhook] factory %q: workspace=%q, token resolved", factory.Name, factory.Workspace)
		}

		// Labels filter: all configured labels must be present on the issue (AND)
		if len(factory.Labels) > 0 {
			allMatch := true
			missing := []string{}
			for _, required := range factory.Labels {
				if !issueLabels[strings.ToLower(required)] {
					allMatch = false
					missing = append(missing, required)
				}
			}
			if !allMatch {
				log.Printf("[github-issues-webhook] factory %q: SKIP — missing required labels: %v (have: %v)", factory.Name, missing, func() []string {
					out := make([]string, 0, len(issueLabels))
					for k := range issueLabels {
						out = append(out, k)
					}
					return out
				}())
				continue
			}
			log.Printf("[github-issues-webhook] factory %q: labels match (all %v present)", factory.Name, factory.Labels)
		}

		// AllowedLabelers filter: restrict who can trigger by labeling
		if len(factory.AllowedLabelers) > 0 && (payload.Action == "labeled" || payload.Action == "unlabeled") {
			labelerAllowed := false
			for _, allowed := range factory.AllowedLabelers {
				if strings.EqualFold(allowed, payload.Sender.Login) {
					labelerAllowed = true
					break
				}
			}
			if !labelerAllowed {
				log.Printf("[github-issues-webhook] factory %q: SKIP — labeler %q not in allowed_labelers %v",
					factory.Name, payload.Sender.Login, factory.AllowedLabelers)
				continue
			}
			log.Printf("[github-issues-webhook] factory %q: labeler %q allowed", factory.Name, payload.Sender.Login)
		}

		// AssignedTo filter
		if factory.AssignedTo != "" {
			wanted := strings.ToLower(strings.TrimSpace(factory.AssignedTo))
			assigneeMatch := false
			skipReason := ""
			switch {
			case wanted == "any":
				if assignee != "" {
					assigneeMatch = true
				} else {
					skipReason = "assignee=any but issue unassigned"
				}
			case wanted == "none":
				if assignee == "" {
					assigneeMatch = true
				} else {
					skipReason = fmt.Sprintf("assignee=none but issue assigned to %q", assignee)
				}
			case strings.HasPrefix(wanted, "!"):
				excluded := strings.TrimPrefix(strings.TrimPrefix(wanted, "!"), "@")
				if !strings.EqualFold(assignee, excluded) {
					assigneeMatch = true
				} else {
					skipReason = fmt.Sprintf("assignee %q is excluded (%q)", assignee, wanted)
				}
			default:
				target := strings.TrimPrefix(wanted, "@")
				if strings.EqualFold(assignee, target) {
					assigneeMatch = true
				} else {
					skipReason = fmt.Sprintf("assignee=%q, want %q", assignee, target)
				}
			}
			if !assigneeMatch {
				log.Printf("[github-issues-webhook] factory %q: SKIP — %s", factory.Name, skipReason)
				continue
			}
			log.Printf("[github-issues-webhook] factory %q: assignee match (%q)", factory.Name, factory.AssignedTo)
		}

		// Issue entering trigger status → create claw.
		// For GitHub Issues, trigger_status maps to state ("open") or a label.
		triggerMatched := false
		if strings.EqualFold(currentStatus, factory.TriggerStatus) {
			triggerMatched = true
			log.Printf("[github-issues-webhook] factory %q: trigger matched by current status %q == trigger_status %q",
				factory.Name, currentStatus, factory.TriggerStatus)
		} else if issueLabels[strings.ToLower(factory.TriggerStatus)] {
			triggerMatched = true
			log.Printf("[github-issues-webhook] factory %q: trigger matched by label %q present",
				factory.Name, factory.TriggerStatus)
		} else {
			log.Printf("[github-issues-webhook] factory %q: trigger NOT matched — currentStatus=%q, labels=%v, trigger_status=%q",
				factory.Name, currentStatus, func() []string {
					out := make([]string, 0, len(issueLabels))
					for k := range issueLabels {
						out = append(out, k)
					}
					return out
				}(), factory.TriggerStatus)
		}

		// Only create when transitioning into the trigger condition
		previousMatched := false
		if previousStatus != "" {
			if strings.EqualFold(previousStatus, factory.TriggerStatus) {
				previousMatched = true
				log.Printf("[github-issues-webhook] factory %q: previous status %q matched trigger (transition detection)",
					factory.Name, previousStatus)
			}
		}
		// For label-based triggers, also check if the trigger label was already
		// present before this event (using the pre-event label set).
		if !previousMatched && (payload.Action == "labeled" || payload.Action == "unlabeled") {
			if previousLabels[strings.ToLower(factory.TriggerStatus)] {
				previousMatched = true
				log.Printf("[github-issues-webhook] factory %q: trigger label %q was already present before this event",
					factory.Name, factory.TriggerStatus)
			}
		}

		if triggerMatched && !previousMatched {
			matched = true
			log.Printf("[github-issues-webhook] factory %q: ✓ CREATING CLAW — issue %s entered trigger '%s' (was not in trigger before)",
				factory.Name, issueID, factory.TriggerStatus)
			clawID := ""
			if err := s.createClawForGitHubIssue(factory, payload); err != nil {
				log.Printf("[github-issues-webhook] factory %q: ✗ FAILED to create claw for %s: %v", factory.Name, issueID, err)
				s.logFactoryEvent(factory.Name, issueID, issueTitle, previousStatus, currentStatus, "error", "", err.Error())
			} else {
				_ = s.db.QueryRow(`SELECT id FROM claws WHERE github_issue_id=? ORDER BY created_at DESC LIMIT 1`, issueID).Scan(&clawID)
				log.Printf("[github-issues-webhook] factory %q: ✓ CREATED claw %s for issue %s", factory.Name, clawID[:8], issueID)
				s.logFactoryEvent(factory.Name, issueID, issueTitle, previousStatus, currentStatus, "claw_created", clawID, "")
			}
		} else if triggerMatched && previousMatched {
			log.Printf("[github-issues-webhook] factory %q: SKIP — issue %s already in trigger '%s', no transition",
				factory.Name, issueID, factory.TriggerStatus)
		} else {
			log.Printf("[github-issues-webhook] factory %q: SKIP — trigger not matched for issue %s", factory.Name, issueID)
		}

		// Issue leaving trigger status → terminate claw.
		if factory.TerminateOnLeave && !triggerMatched {
			var activeClaw string
			_ = s.db.QueryRow(
				`SELECT id FROM claws WHERE github_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`,
				issueID,
			).Scan(&activeClaw)
			if activeClaw != "" {
				matched = true
				log.Printf("[github-issues-webhook] factory %q: issue %s left trigger '%s' — terminating claw %s",
					factory.Name, issueID, factory.TriggerStatus, activeClaw[:8])
				s.terminateClawForGitHubIssue(issueID)
				s.logFactoryEvent(factory.Name, issueID, issueTitle, previousStatus, currentStatus, "claw_terminated", activeClaw, "terminated: issue left trigger status")
			}
		}
	}

	if !matched {
		log.Printf("[github-issues-webhook] no factory matched for issue %s — checking why:", issueID)
		for _, factory := range s.hubCfg.Factories {
			if factory.Integration == "github-issues" && (factory.Enabled == nil || *factory.Enabled) {
				log.Printf("[github-issues-webhook] factory %q: not_actionable — status '%s'→'%s' did not match trigger '%s' (labels=%v)",
					factory.Name, previousStatus, currentStatus, factory.TriggerStatus, factory.Labels)
				s.logFactoryEvent(factory.Name, issueID, issueTitle, previousStatus, currentStatus, "not_actionable",
					"", fmt.Sprintf("status '%s'→'%s' did not match trigger '%s'", previousStatus, currentStatus, factory.TriggerStatus))
			}
		}
	}
}

func (s *Server) createClawForGitHubIssue(factory *types.FactoryConfig, payload githubIssuesWebhookPayload) error {
	issueID := fmt.Sprintf("%s/%d", payload.Repository.FullName, payload.Issue.Number)

	// Verify we can read the issue before spending money on a sandbox
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

	// Enforce 1:1 — check if a claw already exists for this issue.
	// If the claw is offline, error, or stopped, delete and recreate it since
	// the underlying sandbox is gone. Only skip if it's actively starting,
	// running, or connected.
	var existingID, existingStatus string
	_ = s.db.QueryRow(
		`SELECT id, status FROM claws WHERE github_issue_id = ? AND status NOT IN ('deleted') LIMIT 1`,
		issueID,
	).Scan(&existingID, &existingStatus)
	if existingID != "" {
		if existingStatus == "starting" || existingStatus == "connected" || existingStatus == "running" {
			log.Printf("[factory:%s] claw %s already exists for issue %s (status=%s) — treating as idempotent success", factory.Name, existingID[:8], issueID, existingStatus)
			return nil
		}
		log.Printf("[factory:%s] claw %s exists for issue %s but status=%s, deleting and recreating", factory.Name, existingID[:8], issueID, existingStatus)
		_, _ = s.db.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, existingID)
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
		env["GITHUB_TOKEN"] = ghToken // also set generic GITHUB_TOKEN for GitHub API access
	}

	// Resolve and inject template-requested secrets
	resolvedSecrets := make(map[string]string)
	if tmplCfg != nil && len(tmplCfg.Secrets) > 0 {
		for _, ref := range tmplCfg.Secrets {
			secretVal, envName, ok := s.resolveSecretRef(ref, factory)
			if ok {
				env[envName] = secretVal
				resolvedSecrets[envName] = secretVal
				log.Printf("[factory:%s] injected secret %s as %s into claw env", factory.Name, ref.Type, envName)
			} else {
				log.Printf("[factory:%s] warning: requested secret (type=%s name=%s workspace=%s) not found", factory.Name, ref.Type, ref.Name, ref.Workspace)
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
		if tmplCfg.Docker {
			dockerEnabled = 1
		}
		if tmplCfg.GitHub != nil {
			githubRepos = tmplCfg.GitHub.Repos
		}
		if tmplCfg.Linear != nil {
			linearWorkspace = tmplCfg.Linear.Workspace
		}
		if tmplCfg.AutoWatchCI != nil && !*tmplCfg.AutoWatchCI {
			autoFixCI = 0
		}
		if tmplCfg.AutoWatchBugbot != nil && !*tmplCfg.AutoWatchBugbot {
			autoFixBugbot = 0
		}
	}

	// Build SECRETS.md
	var secretList []string
	if ghToken != "" {
		secretList = append(secretList, "- `GITHUB_ISSUES_API_KEY` — GitHub Issues API token")
		secretList = append(secretList, "- `GITHUB_TOKEN` — GitHub API token")
	}
	for envName := range resolvedSecrets {
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
	}
	if len(secretList) > 0 {
		templateFiles["SECRETS.md"] = "# Available Secrets\n\n" + strings.Join(secretList, "\n") + "\n\n> Do not write these values to files. They are injected as environment variables.\n"
	}

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

	// Check concurrency limit
	s.mu.RLock()
	maxConcurrent := s.hubCfg.MaxConcurrentClaws
	s.mu.RUnlock()

	activeCount := s.countActiveClaws()
	isPending := false
	if maxConcurrent > 0 && activeCount >= maxConcurrent {
		isPending = true
		log.Printf("[factory] concurrency limit reached (active=%d, max=%d) — queueing claw for GitHub issue %s as pending", activeCount, maxConcurrent, issueID)
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
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, auto_fix_ci, auto_fix_bugbot, github_issue_id, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, dockerEnabled, string(tagsJSON), clawColor, llmKey, autoFixCI, autoFixBugbot, issueID, initialStatus, now,
	)
	if err != nil {
		return fmt.Errorf("db insert: %w", err)
	}

	log.Printf("[factory] created claw %s (%s) for GitHub issue %s (status=%s)", clawName, clawID[:8], issueID, initialStatus)
	// Notify connected dashboards immediately so the card appears
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": initialStatus},
	})

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
		case "vercel":
			provErr = s.provisionVercel(ctx, clawID, req, provCfg, fileBytes, env)
		case "noop":
			// Noop provider is not implemented; skip
		default:
			provErr = fmt.Errorf("unknown provider %s", provider)
		}

		if provErr != nil {
			log.Printf("[factory] claw %s provision failed: %v", clawID[:8], provErr)
			_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
			s.broadcastToUsers(tenantID, types.WSMessage{
				Type:    "claw_status",
				Payload: map[string]string{"claw_id": clawID, "status": "error"},
			})
			return
		}

		_, _ = s.db.Exec(`UPDATE claws SET status='online' WHERE id=?`, clawID)
		s.broadcastToUsers(tenantID, types.WSMessage{
			Type:    "claw_status",
			Payload: map[string]string{"claw_id": clawID, "status": "online"},
		})
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
// all GitHub Issues integrations and factories.
func (s *Server) resolveGitHubIssuesTokenForRepo(repoFullName string) string {
	s.mu.RLock()
	integrations := s.hubCfg.Integrations
	factories := s.hubCfg.Factories
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	if integrations != nil {
		for _, gi := range integrations.GitHubIssues {
			if gi.Token != "" {
				return gi.Token
			}
		}
	}

	if factories != nil {
		for _, factory := range factories {
			if factory.Integration != "github-issues" {
				continue
			}
			if factory.WebhookSecret != "" {
				return factory.WebhookSecret
			}
			if factory.WebhookSecretRef != "" && secrets != nil {
				if secret := secrets[factory.WebhookSecretRef]; secret != "" {
					return secret
				}
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
