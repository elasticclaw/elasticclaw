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
	"nhooyr.io/websocket"
)

// linearWebhookPayload is the relevant subset of a Linear webhook event.
type linearProjectRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type linearWebhookPayload struct {
	Action    string `json:"action"`    // "create", "update", "remove"
	Type      string `json:"type"`      // "Issue", "IssueLabel", etc.
	CreatedAt string `json:"createdAt"` // ISO 8601 timestamp of the webhook event
	Data      struct {
		ID          string `json:"id"`
		CreatedAt   string `json:"createdAt"`
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
		Project   *linearProjectRef `json:"project,omitempty"`
		ProjectID string            `json:"projectId,omitempty"`
		Labels    []struct {
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
	Actor struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Name  string `json:"name"`
		Email string `json:"email"`
		URL   string `json:"url"`
	} `json:"actor,omitempty"`
	WebhookID string `json:"webhookId,omitempty"`
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
	if !s.validateLinearSignature(r.PathValue("workspace"), body, sig) {
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

	// Dedup: ignore duplicate webhook deliveries.
	// Key includes webhookId + issue identifier + createdAt timestamp.
	// Linear retries carry the same webhookId and same createdAt; rapid
	// transitions (A→B→A→B) have different createdAt values and pass through.
	// If createdAt is missing, fall back to a nanosecond timestamp so we never
	// silently collapse the key — missing createdAt means we can't dedup
	// reliably, so we err on the side of processing (claw creation is idempotent).
	createdAt := payload.CreatedAt
	if createdAt == "" {
		createdAt = fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	dedupKey := payload.WebhookID + ":" + payload.Data.Identifier + ":" + createdAt
	if payload.WebhookID != "" && s.isDuplicateWebhook(dedupKey) {
		log.Printf("[linear-webhook] dedup: skipping duplicate delivery %s for %s (createdAt=%s)", payload.WebhookID, payload.Data.Identifier, createdAt)
		w.WriteHeader(http.StatusOK)
		return
	}

	workspace := strings.TrimSpace(r.PathValue("workspace"))
	s.safeGo("linear webhook", func() { s.processLinearEvent(workspace, payload) })
	w.WriteHeader(http.StatusOK)
}

// isDuplicateWebhook checks if we've recently seen this webhook fingerprint.
// It also cleans expired entries while holding the lock.
func (s *Server) isDuplicateWebhook(key string) bool {
	s.webhookDedupMu.Lock()
	defer s.webhookDedupMu.Unlock()

	now := time.Now()
	// Clean expired entries (older than 5s). The short window only catches
	// near-simultaneous retries. We rely on claw creation idempotency (1:1
	// issue→claw enforcement in createClawForLinearIssue) for any retry that
	// arrives after this window expires. The createdAt in the dedup key ensures
	// rapid A→B→A→B transitions on the same issue are never suppressed.
	for k, t := range s.webhookDedup {
		if now.Sub(t) > 5*time.Second {
			delete(s.webhookDedup, k)
		}
	}

	if lastSeen, ok := s.webhookDedup[key]; ok && now.Sub(lastSeen) < 5*time.Second {
		return true
	}
	s.webhookDedup[key] = now
	return false
}

func (s *Server) validateLinearSignature(workspaceName string, body []byte, sig string) bool {
	s.mu.RLock()
	integrations := s.hubCfg.Integrations
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()

	factories := s.resolveFactories()
	workflowWorkspaces, _ := loadExternalWorkflowsByIntegration("linear")

	hasAnySecret := false

	for _, secret := range workspaceIssueTrackerWebhookSecrets(workspaceName, "linear") {
		hasAnySecret = true
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}
	if workspaceName != "" {
		return !hasAnySecret
	}

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

	for _, workspace := range workflowWorkspaces {
		for _, secretRef := range workspace.WebhookSecrets {
			if secretRef == "" || secrets == nil {
				continue
			}
			secret := secrets[secretRef]
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

func (s *Server) processLinearEvent(workspaceName string, payload linearWebhookPayload) {
	workflowWorkspaces, _ := loadExternalWorkflowsByIntegration("linear")
	workflowWorkspaces = filterWorkflowWorkspacesByName(workflowWorkspaces, workspaceName)
	_ = s.processLinearWorkflowEvent(workflowWorkspaces, payload)

	factories := s.resolveFactories()
	if len(factories) == 0 {
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
	for _, factory := range factories {
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

		currentLabels := make([]string, 0, len(payload.Data.Labels))
		for _, label := range payload.Data.Labels {
			currentLabels = append(currentLabels, label.Name)
		}
		if !labelsAllowed(currentLabels, factory.Labels, factory.ExcludeLabels) {
			continue
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
			if err := s.createClawForIssue(factory, payload, "linear webhook"); err != nil {
				if isFactoryTriggerAlreadyClaimed(err) {
					continue
				}
				log.Printf("[factory:%s] failed to create claw for %s: %v", factory.Name, issueID, err)
				s.logFactoryEvent(factory.Name, issueID, payload.Data.Title, previousStatus, currentStatus, "error", "", err.Error())
				s.trackFactoryCreationFailure(factory.Name, issueID, err.Error())
			} else {
				// Look up the newly created claw ID
				_ = s.db.QueryRow(`SELECT id FROM claws WHERE linear_issue_id=? AND factory_name=? ORDER BY created_at DESC LIMIT 1`, issueID, factory.Name).Scan(&clawID)
				s.logFactoryEvent(factory.Name, issueID, payload.Data.Title, previousStatus, currentStatus, "claw_created", clawID, "")
				if clawID != "" {
					s.trackFactoryCreationSuccess(factory.Name, issueID, clawID)
				}
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
		for _, factory := range factories {
			if factory.Integration == "linear" && (factory.Enabled == nil || *factory.Enabled) && (factory.Team == "" || strings.EqualFold(factory.Team, teamKey)) {
				s.logFactoryEvent(factory.Name, issueID, payload.Data.Title, previousStatus, currentStatus, "not_actionable",
					"", fmt.Sprintf("status '%s'→'%s' did not match trigger '%s'", previousStatus, currentStatus, factory.TriggerStatus))
			}
		}
	}
}

func (s *Server) processLinearWorkflowEvent(workspaces []*types.WorkspaceConfig, payload linearWebhookPayload) bool {
	if len(workspaces) == 0 {
		return false
	}

	currentStatus := payload.Data.State.Name
	previousStatus := ""
	if payload.UpdatedFrom != nil && payload.UpdatedFrom.State != nil {
		previousStatus = payload.UpdatedFrom.State.Name
	}
	teamKey := payload.Data.Team.Key
	issueID := payload.Data.Identifier
	issueLabels := map[string]bool{}
	for _, label := range payload.Data.Labels {
		issueLabels[strings.ToLower(label.Name)] = true
	}
	assignee := ""
	if payload.Data.Assignee != nil {
		assignee = payload.Data.Assignee.Name
	}
	projectID, projectName := linearWebhookProject(payload)

	matched := false
	for _, workspace := range workspaces {
		for _, workflow := range workspace.Workflows {
			if workflow == nil || workflow.Integration != "linear" {
				continue
			}
			if workflow.Enabled != nil && !*workflow.Enabled {
				continue
			}
			if !linearWorkflowFiltersMatch(workflow, teamKey, projectID, projectName, issueLabels, assignee) {
				continue
			}
			if workflow.TriggerStatus == "" {
				continue
			}
			if strings.EqualFold(currentStatus, workflow.TriggerStatus) && !strings.EqualFold(previousStatus, workflow.TriggerStatus) {
				matched = true
				log.Printf("[workflow:%s/%s] Linear issue %s entered %q — creating claw", workspace.Name, workflow.Name, issueID, workflow.TriggerStatus)
				if err := s.createClawForLinearWorkflow(workspace, workflow, payload, "linear webhook"); err != nil {
					log.Printf("[workflow:%s/%s] failed to create claw for %s: %v", workspace.Name, workflow.Name, issueID, err)
					s.notifyLinearWorkflowCreateFailure(workspace, workflow, payload, err)
				}
			}
			if workflow.TerminateOnLeave && !strings.EqualFold(currentStatus, workflow.TriggerStatus) {
				var activeClaw string
				_ = s.db.QueryRow(`SELECT id FROM claws WHERE linear_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&activeClaw)
				if activeClaw != "" {
					matched = true
					log.Printf("[workflow:%s/%s] Linear issue %s left trigger %q — terminating claw %s", workspace.Name, workflow.Name, issueID, workflow.TriggerStatus, activeClaw[:8])
					s.terminateClawForIssue(issueID)
				}
			}
		}
	}
	return matched
}

func linearWebhookProject(payload linearWebhookPayload) (string, string) {
	projectID := strings.TrimSpace(payload.Data.ProjectID)
	if payload.Data.Project == nil {
		return projectID, ""
	}
	if nestedID := strings.TrimSpace(payload.Data.Project.ID); nestedID != "" {
		projectID = nestedID
	}
	return projectID, strings.TrimSpace(payload.Data.Project.Name)
}

func linearWorkflowProjects(workflow *types.WorkflowConfig) []string {
	if workflow == nil {
		return nil
	}
	if workflow.Trigger != nil && workflow.Trigger.Linear != nil {
		return workflow.Trigger.Linear.Projects
	}
	// Compatibility for already-normalized workflow values.
	return workflow.TriggerRepos
}

func linearProjectMatches(projectID, projectName string, projects []string) bool {
	if len(projects) == 0 {
		return true
	}
	projectID = strings.TrimSpace(projectID)
	projectName = strings.TrimSpace(projectName)
	if projectID == "" && projectName == "" {
		return false
	}
	for _, configured := range projects {
		configured = strings.TrimSpace(configured)
		if projectID != "" && configured == projectID {
			return true
		}
		if projectName != "" && strings.EqualFold(configured, projectName) {
			return true
		}
	}
	return false
}

func linearWorkflowFiltersMatch(workflow *types.WorkflowConfig, teamKey, projectID, projectName string, issueLabels map[string]bool, assignee string) bool {
	if workflow == nil {
		return false
	}
	if workflow.Team != "" && !strings.EqualFold(workflow.Team, teamKey) {
		return false
	}
	if !linearProjectMatches(projectID, projectName, linearWorkflowProjects(workflow)) {
		return false
	}
	if !labelSetAllowed(issueLabels, workflow.Labels, workflow.ExcludeLabels) {
		return false
	}
	return workflow.AssignedTo == "" || assignedToMatches(workflow.AssignedTo, assignee)
}

func linearWorkflowMatchesIssue(workflow *types.WorkflowConfig, teamKey, state, projectID, projectName string, issueLabels map[string]bool, assignee string) bool {
	if workflow == nil || workflow.TriggerStatus == "" || !strings.EqualFold(workflow.TriggerStatus, state) {
		return false
	}
	return linearWorkflowFiltersMatch(workflow, teamKey, projectID, projectName, issueLabels, assignee)
}

func (s *Server) notifyLinearWorkflowCreateFailure(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, payload linearWebhookPayload, createErr error) {
	s.notifyWorkflowCreateFailure(workspace, workflow, workflowIssueRef{Integration: "linear", IssueID: payload.Data.Identifier, LinearIdentifier: payload.Data.Identifier}, triggerActor{
		ID:    payload.Actor.ID,
		Type:  payload.Actor.Type,
		Name:  payload.Actor.Name,
		Email: payload.Actor.Email,
		URL:   payload.Actor.URL,
	}, createErr)
}

func (s *Server) createClawForLinearWorkflow(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig, payload linearWebhookPayload, reason string) error {
	issueID := payload.Data.Identifier
	issueCreatedAt := parseRFC3339Timestamp(payload.Data.CreatedAt)
	var existing string
	_ = s.db.QueryRow(`SELECT id FROM claws WHERE linear_issue_id=? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&existing)
	if existing != "" {
		log.Printf("[workflow:%s/%s] Linear issue %s already has active claw %s", workspace.Name, workflow.Name, issueID, existing[:8])
		return nil
	}

	triggerKey := factoryTriggerKey("linear", issueID)
	triggerOwner := fmt.Sprintf("workflow:%s/%s", workspace.Name, workflow.Name)
	claimed, err := s.claimFactoryTrigger(triggerOwner, "linear", triggerKey, reason, map[string]string{
		"issue_id":  issueID,
		"source":    reason,
		"workspace": workspace.Name,
		"workflow":  workflow.Name,
	})
	if err != nil {
		return fmt.Errorf("claim workflow trigger: %w", err)
	}
	if !claimed {
		log.Printf("[workflow:%s/%s] Linear issue %s already has an active trigger claim", workspace.Name, workflow.Name, issueID)
		return nil
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(triggerOwner, "linear", triggerKey)
		}
	}()

	templateFiles := cloneStringMap(workspace.Files)
	_, requiresPR, _ := taskRunAnalyticsContractForWorkflow(workflow)
	templateFiles["CONTEXT.md"] = buildLinearContext(payload, requiresPR)

	clawName := issueID
	if workflow.NamePattern != "" {
		clawName = strings.ReplaceAll(workflow.NamePattern, "{issue_id}", issueID)
		clawName = strings.ReplaceAll(clawName, "{team}", payload.Data.Team.Key)
	}

	clawID, isPending, err := s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{
		workspaceFiles:       templateFiles,
		clawName:             clawName,
		linearIssueID:        issueID,
		issueCreatedAt:       issueCreatedAt,
		issueLabels:          linearPayloadLabels(payload),
		issueLabelsAvailable: true,
		reason:               reason,
		triggerActor: &triggerActor{
			ID:    payload.Actor.ID,
			Type:  payload.Actor.Type,
			Name:  payload.Actor.Name,
			Email: payload.Actor.Email,
			URL:   payload.Actor.URL,
		},
	})
	if err != nil {
		return err
	}
	s.setClawIssueTitle(clawID, payload.Data.Title)
	if err := s.completeFactoryTrigger(triggerOwner, "linear", triggerKey, clawID); err != nil {
		_, _ = s.finishClawTerminalTx(clawID, "deleted", "", "failed", err.Error(), terminalTxOpts{})
		if s.cronScheduler != nil {
			s.cronScheduler.releaseClawWorkflowSlot(clawID)
		}
		return fmt.Errorf("complete workflow trigger: %w", err)
	}
	claimOpen = false
	if !isPending && workflow.WorkingStatus != "" {
		if token := s.resolveLinearTokenForWorkflow(workspace.Name, workflow); token != "" {
			if err := s.moveLinearIssueOnServer(token, issueID, workflow.WorkingStatus); err != nil {
				log.Printf("[workflow:%s/%s] failed to move issue %s to working status %q: %v", workspace.Name, workflow.Name, issueID, workflow.WorkingStatus, err)
			}
		}
	}
	log.Printf("[workflow:%s/%s] created claw %s for Linear issue %s", workspace.Name, workflow.Name, clawID[:8], issueID)
	return nil
}

func linearPayloadLabels(payload linearWebhookPayload) []string {
	labels := make([]string, 0, len(payload.Data.Labels))
	for _, label := range payload.Data.Labels {
		labels = append(labels, label.Name)
	}
	return labels
}

func (s *Server) createClawForIssue(factory *types.FactoryConfig, payload linearWebhookPayload, reason string) error {
	issueID := payload.Data.Identifier
	triggerKey := factoryTriggerKey("linear", issueID)

	claimed, err := s.claimFactoryTrigger(factory.Name, "linear", triggerKey, reason, map[string]string{
		"issue_id": issueID,
		"source":   reason,
	})
	if err != nil {
		return fmt.Errorf("claim factory trigger: %w", err)
	}
	if !claimed {
		if reason != "poll" {
			log.Printf("[factory:%s] Linear issue %s already has an active trigger claim — treating as idempotent success", factory.Name, issueID)
		}
		return errFactoryTriggerAlreadyClaimed
	}
	claimOpen := true
	defer func() {
		if claimOpen {
			s.failFactoryTrigger(factory.Name, "linear", triggerKey)
		}
	}()

	// Verify we can read the issue before spending money on a sandbox.
	// Non-negotiable: if the issue is unreadable, we can't do any work.
	linearToken := s.resolveLinearTokenForFactory(factory)
	var issueCreatedAt time.Time
	if linearToken != "" {
		details, err := s.fetchLinearIssueDetails(linearToken, issueID)
		if err != nil {
			log.Printf("[factory:%s] pre-flight FAILED for %s: %v", factory.Name, issueID, err)
			return fmt.Errorf("cannot read issue %s from Linear (check token/workspace access): %w", issueID, err)
		}
		issueCreatedAt = parseRFC3339Timestamp(details.CreatedAt)
	} else {
		log.Printf("[factory:%s] warning: no Linear token, skipping pre-flight issue read for %s", factory.Name, issueID)
	}

	// Build Linear-specific context and inject into template files
	templateFiles, err := s.resolveTemplateFiles(factory.Template)
	if err != nil {
		return fmt.Errorf("template %q not found: %w", factory.Template, err)
	}
	_, requiresPR, _ := taskRunAnalyticsContractForFactory(factory)
	issueContext := buildLinearContext(payload, requiresPR)
	templateFiles["CONTEXT.md"] = issueContext

	// Create claw using shared factory creator, passing pre-built template files
	clawID, isPending, err := s.createClawFromFactory(factory, issueID, nil, templateFiles, reason)
	if err != nil {
		return err
	}
	// Keep the tracker title with the run; the dashboard can then display it
	// without another tracker request.
	s.setClawIssueTitle(clawID, payload.Data.Title)
	s.recordTaskRunIssueCreatedAt(clawID, issueCreatedAt)
	if err := s.completeFactoryTrigger(factory.Name, "linear", triggerKey, clawID); err != nil {
		_, _ = s.finishClawTerminalTx(clawID, "deleted", "", "failed", err.Error(), terminalTxOpts{})
		if s.cronScheduler != nil {
			s.cronScheduler.releaseClawWorkflowSlot(clawID)
		}
		return fmt.Errorf("complete factory trigger: %w", err)
	}
	claimOpen = false
	log.Printf("[factory] created claw %s for Linear issue %s", clawID[:8], issueID)

	// Move the issue to WorkingStatus if configured (only if not pending —
	// a queued claw hasn't actually started working yet)
	if !isPending && factory.WorkingStatus != "" {
		linearToken := s.resolveLinearTokenForFactory(factory)
		if linearToken != "" {
			if err := s.moveLinearIssueOnServer(linearToken, issueID, factory.WorkingStatus); err != nil {
				log.Printf("[factory] failed to move issue %s to working status '%s': %v", issueID, factory.WorkingStatus, err)
			} else {
				log.Printf("[factory] moved issue %s to working status '%s'", issueID, factory.WorkingStatus)
			}
		}
	}

	return nil
}

func (s *Server) terminateClawForIssue(issueID string) {
	var clawID, tenantID, factoryName string
	if err := s.db.QueryRow(
		`SELECT id, tenant_id, factory_name FROM claws WHERE (linear_issue_id = ? OR shortcut_story_id = ? OR jira_issue_id = ?) AND status NOT IN ('error','deleted') LIMIT 1`,
		issueID, issueID, issueID,
	).Scan(&clawID, &tenantID, &factoryName); err != nil {
		return
	}
	log.Printf("[factory] terminating claw %s for issue %s", clawID[:8], issueID)

	// Post a comment on the linked issue/story before terminating
	factory := s.findFactoryForIssue(issueID)
	if factory != nil && issueID != "" {
		switch factory.Integration {
		case "linear":
			token := s.resolveLinearTokenForFactory(factory)
			if token != "" {
				if err := s.commentLinearIssue(token, issueID, "Agent stopped: issue left trigger status"); err != nil {
					log.Printf("[factory] failed to comment Linear issue %s: %v", issueID, err)
				} else {
					log.Printf("[factory] commented Linear issue %s", issueID)
				}
			}
		case "shortcut":
			token := s.resolveShortcutToken(factory.Workspace)
			if token != "" {
				if err := commentShortcutIssue(s.resolveShortcutBaseURL(), token, issueID, "Agent stopped: issue left trigger status"); err != nil {
					log.Printf("[factory] failed to comment Shortcut story %s: %v", issueID, err)
				} else {
					log.Printf("[factory] commented Shortcut story %s", issueID)
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
						if err := commentGitHubIssue(token, repo, issueNum, "Agent stopped: issue left trigger status"); err != nil {
							log.Printf("[factory] failed to comment GitHub issue %s: %v", issueID, err)
						} else {
							log.Printf("[factory] commented GitHub issue %s", issueID)
						}
					}
				}
			}
		case "jira":
			if tracker, ok := s.resolveJiraTrackerForFactory(factory); ok {
				if err := s.commentJiraIssue(tracker, issueID, "Agent stopped: issue left trigger status"); err != nil {
					log.Printf("[factory] failed to comment Jira issue %s: %v", issueID, err)
				} else {
					log.Printf("[factory] commented Jira issue %s", issueID)
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
	applied, err := s.finishClawTerminalTx(clawID, "deleted", "", "canceled", "issue left trigger status", terminalTxOpts{})
	if err != nil || !applied {
		return
	}
	if s.cronScheduler != nil {
		s.cronScheduler.releaseClawWorkflowSlot(clawID)
	}
	// Notify dashboards so the card disappears immediately
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
	})
	// Track analytics
	if factoryName != "" {
		s.trackFactoryTermination(factoryName, issueID, clawID, "issue left trigger status")
	}
	// Promote any pending claws now that a slot is free
	go s.promotePendingClaws()
}

func (s *Server) defaultProvider() string {
	var singleName string
	singleCount := 0
	for name, p := range s.hubCfg.Providers {
		// Only consider providers with real credentials, not Type-only stubs (e.g. noop)
		if p.Token != "" || p.APIKey != "" || p.AccessToken != "" {
			return name
		}
		if name != "noop" && p.Type != "noop" {
			singleName = name
			singleCount++
		}
	}
	if singleCount == 1 {
		return singleName
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

func (s *Server) resolveLinearTokenForWorkflow(workspaceName string, workflow *types.WorkflowConfig) string {
	if tracker, ok := findWorkspaceIssueTracker(workspaceName, "linear", workflow.Workspace); ok {
		return tracker.Token
	}
	return ""
}

func (s *Server) resolveShortcutTokenForWorkflow(workspaceName string, workflow *types.WorkflowConfig) string {
	if tracker, ok := findWorkspaceIssueTracker(workspaceName, "shortcut", workflow.Workspace); ok {
		return tracker.Token
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
	case "github-issues":
		if s.hubCfg.Integrations == nil {
			return "", envName, false
		}
		ws := ref.Workspace
		if ws == "" && factory != nil {
			ws = factory.Workspace
		}
		for _, gi := range s.hubCfg.Integrations.GitHubIssues {
			if ws == "" || strings.EqualFold(gi.Workspace, ws) {
				return gi.Token, envName, true
			}
		}
		return "", envName, false
	case "jira":
		if s.hubCfg.Integrations == nil {
			return "", envName, false
		}
		ws := ref.Workspace
		if ws == "" && factory != nil {
			ws = factory.Workspace
		}
		for _, ji := range s.hubCfg.Integrations.Jira {
			if ws == "" || strings.EqualFold(ji.Workspace, ws) {
				return ji.Token, envName, true
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

// countActiveClaws returns the number of claws that count toward the concurrency limit.
// Active claws are: provisioning, starting, connected, running, or idle.
// Pending, deleted, and error claws do NOT count.
func (s *Server) countActiveClaws() int {
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM claws WHERE status IN ('provisioning','starting','connected','running','idle')`).Scan(&count)
	return count
}

// countActiveClawsInGroup returns the number of active claws belonging to the
// given concurrency group. This is the scoped count used for per-group limit
// enforcement. The concurrency_group column is populated at insert time.
// Legacy rows (created before the migration) have concurrency_group = ” and
// are treated as belonging to the "global" group.
func (s *Server) countActiveClawsInGroup(groupName string) int {
	var count int
	if groupName == "global" {
		_ = s.db.QueryRow(`
			SELECT COUNT(*) FROM claws
			WHERE status IN ('provisioning','starting','connected','running','idle')
			  AND (concurrency_group = 'global' OR concurrency_group = '')`).Scan(&count)
	} else {
		_ = s.db.QueryRow(`
			SELECT COUNT(*) FROM claws
			WHERE status IN ('provisioning','starting','connected','running','idle')
			  AND concurrency_group = ?`, groupName).Scan(&count)
	}
	return count
}

// resolveGroupLimit returns the concurrency group name and limit for a factory.
// It checks the factory's ConcurrencyGroup, falls back to "global", and looks up
// the limit from hub config. A limit of 0 means unlimited.
// The deprecated MaxConcurrentClaws field is only used as a fallback when the
// "global" group is not explicitly configured in ConcurrencyGroups.
// Must be called with s.mu held (at least RLock).
func (s *Server) resolveGroupLimit(factory *types.FactoryConfig) (groupName string, limit int) {
	groupName = factory.ConcurrencyGroup
	if groupName == "" {
		groupName = "global"
	}
	found := false
	for _, g := range s.hubCfg.ConcurrencyGroups {
		if g.Name == groupName {
			limit = g.Limit
			found = true
			break
		}
	}
	// Only fall back to the deprecated MaxConcurrentClaws if the "global" group
	// was not explicitly configured. This preserves Limit=0 (unlimited) when the
	// user has explicitly set it in ConcurrencyGroups.
	if !found && groupName == "global" && s.hubCfg.MaxConcurrentClaws > 0 {
		limit = s.hubCfg.MaxConcurrentClaws
	}
	return groupName, limit
}

// promotePendingClaws checks if any pending claws can be promoted to provisioning
// when a running claw terminates. Called after any claw deletion.
func (s *Server) promotePendingClaws() {
	// Serialize promotion to prevent TOCTOU race: multiple goroutines
	// terminating simultaneously could each read active < max and promote,
	// exceeding the limit.
	s.promoteMu.Lock()
	defer s.promoteMu.Unlock()

	// Fetch all pending claws ordered by age so we can scan for the first one
	// whose group still has capacity. A single-row LIMIT 1 would trap us in an
	// infinite loop when the oldest claw belongs to a full group.
	// We select concurrency_group directly — it's stored at insert time, so
	// no factory config lookup is needed.
	rows, err := s.db.Query(`SELECT id, tenant_id, concurrency_group FROM claws WHERE status = 'pending' ORDER BY created_at ASC`)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
		var clawID, tenantID, groupName string
		if err := rows.Scan(&clawID, &tenantID, &groupName); err != nil {
			continue
		}
		if groupName == "" {
			groupName = "global"
		}

		s.mu.RLock()
		groupLimit := 0
		found := false
		for _, g := range s.hubCfg.ConcurrencyGroups {
			if g.Name == groupName {
				groupLimit = g.Limit
				found = true
				break
			}
		}
		if !found && groupName == "global" && s.hubCfg.MaxConcurrentClaws > 0 {
			groupLimit = s.hubCfg.MaxConcurrentClaws
		}
		s.mu.RUnlock()

		active := s.countActiveClawsInGroup(groupName)

		// 0 means unlimited — promote all pending claws
		if groupLimit > 0 && active >= groupLimit {
			// This group is at capacity — skip this claw and check the next
			// oldest pending claw (which may belong to a different group that
			// still has capacity). Do NOT return; starvation of other groups
			// would occur if we did.
			continue
		}

		// Promote to provisioning — scope UPDATE to pending status so a
		// concurrent hard-delete doesn't match zero rows silently.
		log.Printf("[factory] promoting pending claw %s to provisioning (active=%d, group=%s, limit=%d)", clawID[:8], active, groupName, groupLimit)
		res, _ := s.db.Exec(`UPDATE claws SET status='provisioning' WHERE id=? AND status='pending'`, clawID)
		n, _ := res.RowsAffected()
		if n == 0 {
			// Claw was deleted between SELECT and UPDATE; skip to next pending
			continue
		}

		// Broadcast so UI updates
		s.broadcastToUsers(tenantID, types.WSMessage{
			Type:    "claw_status",
			Payload: map[string]string{"claw_id": clawID, "status": "provisioning"},
		})

		// Start provisioning asynchronously
		go s.provisionPendingClaw(clawID)
	}
}

// provisionPendingClaw provisions a claw that was previously pending.
// This is similar to the async provisioning in createClawForIssue but fetches
// the claw record from the DB since it was already inserted.
func (s *Server) provisionPendingClaw(clawID string) {
	// Re-fetch the claw record
	var (
		name, template, provider, defaultModel, templateFilesJSON  string
		githubReposJSON, linearWorkspace, llmKey                   string
		nixEnabled, dockerEnabled                                  int
		tagsJSON, color                                            string
		linearIssueID, githubIssueID, shortcutStoryID, jiraIssueID string
		tenantID                                                   string
		previewPort                                                int
	)
	err := s.db.QueryRow(
		`SELECT tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, docker, tags, color, llm_key, linear_issue_id, github_issue_id, shortcut_story_id, jira_issue_id, preview_port FROM claws WHERE id=?`,
		clawID,
	).Scan(&tenantID, &name, &template, &provider, &defaultModel, &templateFilesJSON, &githubReposJSON, &linearWorkspace, &nixEnabled, &dockerEnabled, &tagsJSON, &color, &llmKey, &linearIssueID, &githubIssueID, &shortcutStoryID, &jiraIssueID, &previewPort)
	if err != nil {
		log.Printf("[factory] failed to fetch pending claw %s: %v", clawID[:8], err)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Factory provision failed: failed to fetch pending claw: %v", err), false)
		return
	}

	// Guard: if the claw was deleted before provisioning started
	var currentStatus string
	err2 := s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&currentStatus)
	if err2 != nil || currentStatus == "deleted" {
		log.Printf("[factory] claw %s already deleted before provisioning, aborting", clawID[:8])
		return
	}

	var templateFiles map[string]string
	_ = json.Unmarshal([]byte(templateFilesJSON), &templateFiles)

	// Resolve factory for this issue so we can look up integration tokens and
	// template-declared secrets.
	var factory *types.FactoryConfig
	issueID := linearIssueID
	if issueID == "" {
		issueID = githubIssueID
	}
	if issueID == "" {
		issueID = shortcutStoryID
	}
	if issueID == "" {
		issueID = jiraIssueID
	}
	if issueID != "" {
		factory = s.findFactoryForIssue(issueID)
	}

	// Resolve tokens for env (Linear, GitHub Issues, Shortcut, Jira)
	var linearToken, githubToken, shortcutToken string
	var jiraTracker workspaceIssueTracker
	var hasJiraTracker bool
	if factory != nil {
		linearToken = s.resolveLinearTokenForFactory(factory)
		githubToken = s.resolveGitHubIssuesTokenForFactory(factory)
		shortcutToken = s.resolveShortcutToken(factory.Workspace)
		jiraTracker, hasJiraTracker = s.resolveJiraTrackerForFactory(factory)
	}

	s.mu.RLock()
	clawToken := s.hubCfg.ClawToken
	s.mu.RUnlock()
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_TOKEN": clawToken,
	}
	if linearToken != "" {
		env["LINEAR_API_KEY"] = linearToken
	}
	if githubToken != "" {
		env["GITHUB_ISSUES_API_KEY"] = githubToken
		// Note: We intentionally do NOT set GITHUB_TOKEN here. The credential
		// helper provides a proper GitHub App token for git operations. Setting
		// GITHUB_TOKEN would override the credential helper and cause git/gh to
		// use the issue-reading PAT which lacks repo permissions.
	}
	if shortcutToken != "" {
		env["SHORTCUT_API_KEY"] = shortcutToken
	}
	if hasJiraTracker && jiraTracker.Token != "" {
		env["JIRA_API_KEY"] = jiraTracker.Token
		if jiraTracker.BaseURL != "" {
			env["JIRA_BASE_URL"] = jiraTracker.BaseURL
		}
		if jiraTracker.Username != "" {
			env["JIRA_USERNAME"] = jiraTracker.Username
		}
	}

	// Parse elasticclaw-config.yaml from recovered template files to honour
	// template settings like InstanceType that were set at creation time.
	var tmplCfg *types.TemplateConfig
	if cfgContent, ok := templateFiles["elasticclaw-config.yaml"]; ok {
		var parseErr error
		tmplCfg, parseErr = config.ParseTemplateConfig([]byte(cfgContent))
		if parseErr != nil {
			log.Printf("[factory] warning: could not parse elasticclaw-config.yaml for pending claw %s: %v", clawID[:8], parseErr)
			tmplCfg = nil
		}
	}

	// Resolve and inject template-requested secrets (same as factory create paths).
	if tmplCfg != nil && len(tmplCfg.Secrets) > 0 && factory != nil {
		log.Printf("[factory] DEPRECATED: template %q uses 'secrets:' list — migrate to 'secret_refs:' map", template)
		for _, ref := range tmplCfg.Secrets {
			secretVal, envName, ok := s.resolveSecretRef(ref, factory)
			if ok {
				env[envName] = secretVal
				log.Printf("[factory] injected template secret %s as %s into pending claw %s env", ref.Type, envName, clawID[:8])
			} else {
				log.Printf("[factory] warning: requested secret (type=%s name=%s workspace=%s) not found for pending claw %s", ref.Type, ref.Name, ref.Workspace, clawID[:8])
			}
		}
	}

	// Resolve and inject template-level secret_refs
	if tmplCfg != nil && len(tmplCfg.SecretRefs) > 0 {
		for envName, secretRef := range tmplCfg.SecretRefs {
			if val, ok := s.hubCfg.Secrets[secretRef]; ok {
				env[envName] = val
				log.Printf("[factory] injected template secret_ref %s as %s into pending claw %s env", secretRef, envName, clawID[:8])
			} else {
				log.Printf("[factory] WARNING: template secret_ref %q not found in hub secrets for pending claw %s", secretRef, clawID[:8])
			}
		}
	}

	// Resolve and inject factory-level secret_refs (factory overrides template)
	if factory != nil && len(factory.SecretRefs) > 0 {
		for envName, secretRef := range factory.SecretRefs {
			if val, ok := s.hubCfg.Secrets[secretRef]; ok {
				env[envName] = val
				log.Printf("[factory] injected factory secret_ref %s as %s into pending claw %s env", secretRef, envName, clawID[:8])
			} else {
				log.Printf("[factory] WARNING: factory secret_ref %q not found in hub secrets for pending claw %s", secretRef, clawID[:8])
			}
		}
	}
	templateFiles = injectFigmaAPIDocs(templateFiles, env)

	ctx := context.Background()
	req := types.CreateClawRequest{
		Name:         name,
		TemplateName: template,
		Provider:     provider,
		Files:        templateFiles,
		Env:          env,
		ProviderName: "ec-" + clawID[:8],
		PreviewPort:  previewPort,
	}
	if tmplCfg != nil && req.InstanceType == "" {
		req.InstanceType = tmplCfg.InstanceType
	}

	// Find provider config
	s.mu.RLock()
	provCfg, _ := s.hubCfg.Providers[provider]
	s.mu.RUnlock()

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
	case "exedev":
		provErr = s.provisionExedev(ctx, clawID, req, provCfg, fileBytes, env)
	case "docker":
		provErr = s.provisionDocker(ctx, clawID, req, provCfg, fileBytes)
	case "lambda-microvms":
		provErr = s.provisionLambdaMicroVMs(ctx, clawID, req, provCfg, fileBytes)
	case "noop":
		if os.Getenv("ELASTICCLAW_NOOP_PROVIDER") == "" {
			provErr = fmt.Errorf("noop provider requires ELASTICCLAW_NOOP_PROVIDER=1")
		} else {
			providerID := "noop-vm-" + clawID[:8]
			_, _ = s.execStatusLogged("connect claw "+clawID, `UPDATE claws SET status='connected', provider='noop', provider_id=? WHERE id=? AND status NOT IN ('idle','deleted','error')`, providerID, clawID)
		}
	default:
		provErr = fmt.Errorf("unsupported provider: %s", provider)
	}
	if provErr != nil {
		log.Printf("[factory] provision failed for pending claw %s: %v", clawID, provErr)
		s.stopAgentWithReason(clawID, fmt.Sprintf("Factory provision failed: %v", provErr), false)
		// Slot is now free (error claws don't count toward limit); try to
		// promote the next pending claw.
		go s.promotePendingClaws()
		return
	}

	s.moveIssueToWorkingStatusForPendingClaw(factory, linearIssueID, githubIssueID, shortcutStoryID, jiraIssueID, linearToken, githubToken, shortcutToken, jiraTracker, hasJiraTracker)
}

// moveIssueToWorkingStatusForPendingClaw moves a promoted claw's linked issue
// to its factory working status. Failures are logged but do not stop provisioning.
func (s *Server) moveIssueToWorkingStatusForPendingClaw(factory *types.FactoryConfig, linearIssueID, githubIssueID, shortcutStoryID, jiraIssueID, linearToken, githubToken, shortcutToken string, jiraTracker workspaceIssueTracker, hasJiraTracker bool) {
	if factory == nil || factory.WorkingStatus == "" {
		return
	}

	if linearIssueID != "" && linearToken != "" {
		if err := s.moveLinearIssueOnServer(linearToken, linearIssueID, factory.WorkingStatus); err != nil {
			log.Printf("[factory] failed to move issue %s to working status '%s': %v", linearIssueID, factory.WorkingStatus, err)
		} else {
			log.Printf("[factory] moved issue %s to working status '%s'", linearIssueID, factory.WorkingStatus)
		}
		return
	}

	if githubIssueID != "" && githubToken != "" {
		rest := strings.TrimPrefix(githubIssueID, "gh-")
		lastSlash := strings.LastIndex(rest, "/")
		if lastSlash <= 0 {
			log.Printf("[factory] cannot move GitHub issue %s to working status '%s': malformed issue ID (want owner/repo/number)", githubIssueID, factory.WorkingStatus)
			return
		}
		repo := rest[:lastSlash]
		var issueNum int
		if _, err := fmt.Sscanf(rest[lastSlash+1:], "%d", &issueNum); err != nil {
			log.Printf("[factory] cannot move GitHub issue %s to working status '%s': malformed issue number: %v", githubIssueID, factory.WorkingStatus, err)
			return
		}
		base := s.githubBaseURL
		if base == "" {
			base = "https://api.github.com"
		}
		if err := moveGitHubIssue(githubToken, repo, issueNum, factory.WorkingStatus, base); err != nil {
			log.Printf("[factory] failed to move GitHub issue %s to working status '%s': %v", githubIssueID, factory.WorkingStatus, err)
		} else {
			log.Printf("[factory] moved GitHub issue %s to working status '%s'", githubIssueID, factory.WorkingStatus)
		}
		return
	}

	if shortcutStoryID != "" && shortcutToken != "" {
		if err := moveShortcutStory(s.resolveShortcutBaseURL(), shortcutToken, shortcutStoryID, factory.WorkingStatus); err != nil {
			log.Printf("[factory] failed to move story %s to working status '%s': %v", shortcutStoryID, factory.WorkingStatus, err)
		} else {
			log.Printf("[factory] moved story %s to working status '%s'", shortcutStoryID, factory.WorkingStatus)
		}
		return
	}

	if jiraIssueID != "" && hasJiraTracker {
		if err := s.moveJiraIssue(jiraTracker, jiraIssueID, factory.WorkingStatus); err != nil {
			log.Printf("[factory] failed to move Jira issue %s to working status '%s': %v", jiraIssueID, factory.WorkingStatus, err)
		} else {
			log.Printf("[factory] moved Jira issue %s to working status '%s'", jiraIssueID, factory.WorkingStatus)
		}
	}
}

func buildLinearContext(payload linearWebhookPayload, requiresPR bool) string {
	d := payload.Data
	var b strings.Builder
	b.WriteString("# Issue Context\n\n")
	b.WriteString("This agent was automatically created by a factory to work on a Linear issue. Read this, understand the task, then get to work.\n\n")
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
	if requiresPR {
		b.WriteString("4. Follow the PR Completion Policy below\n")
		appendDefaultFactoryPRPolicy(&b)
	} else {
		b.WriteString("4. When complete, send exactly: `[DONE]`\n")
	}
	return b.String()
}

// Avoid collision with existing now() function

// handleClawDoneSignal is called when a claw sends a message containing [DONE].
// It parses PR URLs from the message, validates them via the GitHub API, stores
// them in claw_prs, then moves the Linear issue and terminates the claw.
// If no valid open PRs are found (and a GH App is configured), it injects an
// error message back so the claw can retry.
func (s *Server) handleClawDoneSignal(clawID, rawMessage string) {
	// Get the issue ID and tenant for this claw. Workflow claws may not have an
	// issue, but [DONE] must still give those runs a terminal path.
	var issueID, tenantID string
	if err := s.db.QueryRow(`SELECT COALESCE(NULLIF(linear_issue_id,''),NULLIF(github_issue_id,''),NULLIF(shortcut_story_id,''),NULLIF(jira_issue_id,''),''), tenant_id FROM claws WHERE id = ?`, clawID).Scan(&issueID, &tenantID); err != nil {
		return
	}

	// Extract PR URLs from the [DONE] line.
	// Expected format: [DONE] https://github.com/org/repo/pull/1 https://...
	prURLs := extractDonePRURLs(rawMessage)

	if pipelineCtx, stage, ok := s.pipelineStageForMessageContains(clawID, rawMessage); ok {
		if issueID != "" {
			s.trackDoneSignal(pipelineCtx.Name(), issueID, clawID, len(prURLs))
		}
		s.transitionPipelineStageWithContext(clawID, *stage, pipelineCtx)
		return
	}
	if issueID == "" {
		var runningWorkflowRun bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_runs WHERE claw_id=? AND status='running')`, clawID).Scan(&runningWorkflowRun); err != nil || !runningWorkflowRun {
			// Interactive claws have no tracker issue or workflow run; [DONE] is
			// ordinary conversation for them, not a teardown signal.
			return
		}
		s.completeIssueLessDoneClaw(clawID, tenantID, prURLs)
		return
	}

	log.Printf("[factory] claw %s sent [DONE] for issue %s", clawID[:8], issueID)

	noPRDoneAllowed := false
	if len(prURLs) == 0 {
		if !s.taskRunRequiresPRForClaw(clawID) {
			noPRDoneAllowed = true
		} else {
			if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
				EventKey:        "done_without_pr:" + clawID,
				Source:          taskRunSourceHub,
				EventType:       taskRunEventDoneWithoutPR,
				ActorType:       taskRunActorAgent,
				InteractionRole: taskRunInteractionTerminal,
				FailureType:     taskRunFailureDoneWithoutPR,
				Detail:          map[string]any{"issueID": issueID},
				OccurredAt:      now(),
			}); err != nil {
				log.Printf("[task-run-analytics] failed to record done_without_pr for claw %s: %v", clawID, err)
			}
			s.injectUserMessage(clawID, "[factory] `[DONE]` received with no PR URLs. Please open a PR and resend: `[DONE] https://github.com/org/repo/pull/N`")
			return
		}
	}

	// Validate PRs via GitHub API if we have a token.
	ghToken := s.resolveGitHubToken()
	if ghToken != "" && !noPRDoneAllowed {
		if rejected, reason := s.validateDonePRs(clawID, prURLs, ghToken); rejected {
			// Nudge the claw to fix and retry — do not terminate.
			s.injectUserMessage(clawID, reason)
			return
		}
	}

	// Block PR creation if any required gate has failed.
	if s.hasFailedRequiredGate(clawID) {
		msg := "[factory] `[DONE]` blocked: a required tool gate has failed. Please fix the issues and retry."
		log.Printf("[factory] claw %s [DONE] blocked by failed required gate", clawID[:8])
		s.injectUserMessage(clawID, msg)
		return
	}

	// Store all validated PRs (idempotent).
	for _, pr := range extractPRs(strings.Join(prURLs, " ")) {
		if err := s.storePRMention(clawID, pr.repo, pr.number, pr.url); err != nil {
			log.Printf("[factory] failed to register PR %s: %v", pr.url, err)
			// Resend with ALL original URLs: earlier PRs in the list are already
			// stored (idempotent), and the retried [DONE] must carry the full set
			// so pipeline transitions and analytics see the complete PR list.
			s.injectUserMessage(clawID, fmt.Sprintf("[factory] Failed to register PR %s: internal error. Please resend: [DONE] %s", pr.url, strings.Join(prURLs, " ")))
			return
		}
	}

	pipelineHandledDone := false
	if pipelineCtx, ok := s.findPipelineContextForIssue(issueID); ok {
		s.trackDoneSignal(pipelineCtx.Name(), issueID, clawID, len(prURLs))
		if pl := parsePipelineForContext(pipelineCtx); pl != nil {
			if stage := pl.StageForMessageContains(rawMessage); stage != nil {
				s.transitionPipelineStageWithContext(clawID, *stage, pipelineCtx)
				pipelineHandledDone = true
			}
		}
	}

	// Legacy factory status handling remains for factory-created claws.
	factory := s.findFactoryForIssue(issueID)
	if factory == nil {
		if noPRDoneAllowed {
			s.completeNoPRDoneClaw(clawID, tenantID, issueID)
		}
		return
	}
	if !pipelineHandledDone {
		s.trackDoneSignal(factory.Name, issueID, clawID, len(prURLs))
	}
	if !pipelineHandledDone {
		if pl := parsePipelineForFactory(factory); pl != nil {
			if stage := pl.StageForMessageContains(rawMessage); stage != nil {
				s.transitionPipelineStage(clawID, *stage, factory, issueID)
				pipelineHandledDone = true
			}
		}
	}
	// Persist completion before touching the tracker. This prevents a tracker
	// move from surviving a process crash while the claw still looks active.
	if !noPRDoneAllowed {
		applied, err := s.finishClawTerminalTx(clawID, "idle", "", "completed", "success", terminalTxOpts{})
		if err != nil || !applied {
			return
		}
		if s.cronScheduler != nil {
			s.cronScheduler.releaseClawWorkflowSlot(clawID)
		}
	}

	// Move the issue to finished_status if configured (agent finished working)
	// If no finished_status, fall back to done_status for backward compatibility
	targetStatus := factory.FinishedStatus
	if targetStatus == "" {
		targetStatus = factory.DoneStatus
	}
	if !pipelineHandledDone && targetStatus != "" {
		// The move is retried with backoff; run it off the done-signal path so
		// a slow or failing tracker cannot delay marking the claw idle.
		factoryCopy := *factory
		go s.moveFactoryIssueToStatus(&factoryCopy, issueID, targetStatus)
	}

	// Keep the claw running — it stays connected to watch for CI failures,
	// bugbot comments, and PR events. The claw is terminated when the PR is
	// merged; if it is closed without merge, the claw is notified and decides
	// what to do (polled by checkPRMerged).
	// Just mark it as 'watching' so the UI shows it differently.
	if noPRDoneAllowed {
		s.completeNoPRDoneClaw(clawID, tenantID, issueID)
		return
	}
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "idle"},
	})
	// Notify the claw it's in watch mode — skip if the pipeline already injected a message
	// Also skip for no-issue manual trigger claws (issueID == "")
	if !pipelineHandledDone && issueID != "" && !noPRDoneAllowed {
		var issueTracker string
		switch {
		case strings.HasPrefix(issueID, "sc-"):
			issueTracker = "Shortcut story"
		case strings.HasPrefix(issueID, "gh-"):
			issueTracker = "GitHub issue"
		default:
			issueTracker = "Linear issue"
		}
		s.injectUserMessage(clawID, fmt.Sprintf("PR created and %s updated. Staying connected to watch for CI failures and review comments. Will terminate when PR is merged; if it is closed without merge, I'll notify you and decide next steps.", issueTracker))
	}
}

// moveFactoryIssueToStatus moves a factory-tracked issue to the given
// tracker status, retrying with backoff on failure.
func (s *Server) moveFactoryIssueToStatus(factory *types.FactoryConfig, issueID, targetStatus string) {
	switch factory.Integration {
	case "jira":
		if tracker, ok := s.resolveJiraTrackerForFactory(factory); ok {
			if err := s.retryTrackerMove("move Jira issue", func() error { return s.moveJiraIssue(tracker, issueID, targetStatus) }); err != nil {
				log.Printf("[factory] failed to move Jira issue %s to '%s': %v", issueID, targetStatus, err)
			} else {
				log.Printf("[factory] moved Jira issue %s to '%s'", issueID, targetStatus)
			}
		}
	case "shortcut":
		// Shortcut story
		scToken := s.resolveShortcutToken(factory.Workspace)
		if scToken != "" {
			if err := s.retryTrackerMove("move Shortcut story", func() error { return moveShortcutStory(s.resolveShortcutBaseURL(), scToken, issueID, targetStatus) }); err != nil {
				log.Printf("[factory] failed to move story %s to '%s': %v", issueID, targetStatus, err)
			} else {
				log.Printf("[factory] moved story %s to '%s'", issueID, targetStatus)
			}
		}
	case "github-issues":
		// GitHub issue
		ghToken := s.resolveGitHubIssuesTokenForFactory(factory)
		if ghToken != "" {
			// Parse gh-owner/repo/number from issueID
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
					if err := s.retryTrackerMove("move GitHub issue", func() error { return moveGitHubIssue(ghToken, repo, issueNum, targetStatus, base) }); err != nil {
						log.Printf("[factory] failed to move GitHub issue %s to '%s': %v", issueID, targetStatus, err)
					} else {
						log.Printf("[factory] moved GitHub issue %s to '%s'", issueID, targetStatus)
					}
				}
			}
		}
	default:
		// Linear issue
		linearToken := s.resolveLinearTokenForFactory(factory)
		if linearToken != "" {
			if err := s.retryTrackerMove("move Linear issue", func() error { return s.moveLinearIssueOnServer(linearToken, issueID, targetStatus) }); err != nil {
				log.Printf("[factory] failed to move issue %s to '%s': %v", issueID, targetStatus, err)
			} else {
				log.Printf("[factory] moved issue %s to '%s'", issueID, targetStatus)
			}
		}
	}
}

// completeIssueLessDoneClaw completes a workflow/manual claw that has no
// tracker issue. PR-bearing runs stay idle to watch their PRs; all other runs
// use the normal non-PR teardown path.
func (s *Server) completeIssueLessDoneClaw(clawID, tenantID string, prURLs []string) {
	if len(prURLs) > 0 {
		if ghToken := s.resolveGitHubToken(); ghToken != "" {
			if rejected, reason := s.validateDonePRs(clawID, prURLs, ghToken); rejected {
				s.injectUserMessage(clawID, reason)
				return
			}
		}
	}
	if s.hasFailedRequiredGate(clawID) {
		s.injectUserMessage(clawID, "[factory] `[DONE]` blocked: a required tool gate has failed. Please fix the issues and retry.")
		return
	}
	for _, pr := range extractPRs(strings.Join(prURLs, " ")) {
		if err := s.storePRMention(clawID, pr.repo, pr.number, pr.url); err != nil {
			// A failed INSERT leaves the PR untracked, so the watcher would never
			// detect its merge/close and the claw would stall in 'idle' forever.
			// Nudge the claw to resend [DONE] instead of idling it.
			log.Printf("[factory] failed to register PR %s: %v", pr.url, err)
			// Resend with ALL original URLs: earlier PRs in the list are already
			// stored (idempotent), and the retried [DONE] must carry the full set
			// so pipeline transitions and analytics see the complete PR list.
			s.injectUserMessage(clawID, fmt.Sprintf("[factory] Failed to register PR %s: internal error. Please resend: [DONE] %s", pr.url, strings.Join(prURLs, " ")))
			return
		}
	}
	if len(prURLs) == 0 {
		s.completeNoPRDoneClaw(clawID, tenantID, "")
		return
	}
	applied, err := s.finishClawTerminalTx(clawID, "idle", "", "completed", "success", terminalTxOpts{})
	if err != nil || !applied {
		return
	}
	if s.cronScheduler != nil {
		s.cronScheduler.releaseClawWorkflowSlot(clawID)
	}
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "idle"},
	})
}

func (s *Server) completeNoPRDoneClaw(clawID, tenantID, issueID string) {
	var provider, providerID string
	if err := s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id=? AND tenant_id=?`, clawID, tenantID).Scan(&provider, &providerID); err != nil {
		return
	}
	applied, err := s.finishClawTerminalTx(clawID, "deleted", "", "completed", "success", terminalTxOpts{})
	if err != nil || !applied {
		return
	}
	s.syncWorkflowVolumes(clawID)
	if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
		EventKey:        "task_completed:" + clawID,
		Source:          taskRunSourceHub,
		EventType:       taskRunEventTaskCompleted,
		ActorType:       taskRunActorAgent,
		InteractionRole: taskRunInteractionTerminal,
		Detail:          map[string]any{"issueID": issueID, "requires_pr": false},
		OccurredAt:      now(),
	}); err != nil {
		log.Printf("[task-run-analytics] failed to record non-pr completion for claw %s: %v", clawID, err)
	}
	if s.cronScheduler != nil {
		s.cronScheduler.releaseClawWorkflowSlot(clawID)
	}
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
	})
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(websocket.StatusNormalClosure, "completed")
		delete(s.claws, clawID)
	}
	s.mu.Unlock()
	go func() {
		s.checkpointBeforeTermination(clawID, "task-completed")
		if providerID != "" {
			s.terminateVMForClaw(clawID, provider, providerID)
		}
		_, _ = s.db.Exec(`DELETE FROM messages WHERE claw_id=?`, clawID)
		_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id=?`, clawID)
		s.promotePendingClaws()
	}()
}

// handleClawTerminateSignal is called when a claw sends a message containing [TERMINATE].
// It immediately terminates the claw, allowing the claw to manage its own lifecycle.
// Unlike [DONE] which moves issues and keeps the claw in idle mode, [TERMINATE] immediately
// deletes the claw and its associated VM.
func (s *Server) handleClawTerminateSignal(clawID, rawMessage string) {
	// Get the tenant ID and issue ID for this claw
	var issueID, tenantID, factoryName string
	if err := s.db.QueryRow(`SELECT COALESCE(NULLIF(linear_issue_id,''),NULLIF(github_issue_id,''),NULLIF(shortcut_story_id,''),NULLIF(jira_issue_id,'')), tenant_id, factory_name FROM claws WHERE id = ?`, clawID).Scan(&issueID, &tenantID, &factoryName); err != nil {
		log.Printf("[terminate] claw %s not found in DB: %v", clawID[:8], err)
		return
	}

	log.Printf("[terminate] claw %s sent [TERMINATE] for issue %s", clawID[:8], issueID)

	// Find the factory config for this issue (for commenting)
	factory := s.findFactoryForIssue(issueID)
	if factory != nil && issueID != "" {
		// Post a comment that the claw terminated itself
		switch factory.Integration {
		case "linear":
			token := s.resolveLinearTokenForFactory(factory)
			if token != "" {
				if err := s.commentLinearIssue(token, issueID, "Agent stopped: claw requested self-termination"); err != nil {
					log.Printf("[terminate] failed to comment Linear issue %s: %v", issueID, err)
				} else {
					log.Printf("[terminate] commented Linear issue %s", issueID)
				}
			}
		case "shortcut":
			token := s.resolveShortcutToken(factory.Workspace)
			if token != "" {
				if err := commentShortcutIssue(s.resolveShortcutBaseURL(), token, issueID, "Agent stopped: claw requested self-termination"); err != nil {
					log.Printf("[terminate] failed to comment Shortcut story %s: %v", issueID, err)
				} else {
					log.Printf("[terminate] commented Shortcut story %s", issueID)
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
						if err := commentGitHubIssue(token, repo, issueNum, "Agent stopped: claw requested self-termination"); err != nil {
							log.Printf("[terminate] failed to comment GitHub issue %s: %v", issueID, err)
						} else {
							log.Printf("[terminate] commented GitHub issue %s", issueID)
						}
					}
				}
			}
		case "jira":
			if tracker, ok := s.resolveJiraTrackerForFactory(factory); ok {
				if err := s.commentJiraIssue(tracker, issueID, "Agent stopped: claw requested self-termination"); err != nil {
					log.Printf("[terminate] failed to comment Jira issue %s: %v", issueID, err)
				} else {
					log.Printf("[terminate] commented Jira issue %s", issueID)
				}
			}
		}
	}

	// Look up provider info before deleting so we can terminate the VM
	var provider, providerID string
	_ = s.db.QueryRow(`SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id = ?`, clawID).Scan(&provider, &providerID)

	// Close WebSocket connection if online
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "claw requested self-termination")
		delete(s.claws, clawID)
	}
	s.mu.Unlock()

	// Delete messages first (FK constraint)
	_, _ = s.db.Exec(`DELETE FROM messages WHERE claw_id = ?`, clawID)
	_, _ = s.db.Exec(`DELETE FROM claw_prs WHERE claw_id = ?`, clawID)
	_, _ = s.db.Exec(`DELETE FROM claws WHERE id = ?`, clawID)

	// Notify dashboards so the card disappears immediately
	s.broadcastToUsers(tenantID, types.WSMessage{
		Type:    "claw_status",
		Payload: map[string]string{"claw_id": clawID, "status": "deleted"},
	})

	// Terminate the provider VM asynchronously
	if providerID != "" {
		go s.terminateVM(provider, providerID)
	}

	// Track analytics
	if factoryName != "" {
		s.trackFactoryTermination(factoryName, issueID, clawID, "claw self-termination")
	}

	// Promote any pending claws now that a slot is free
	go s.promotePendingClaws()

	log.Printf("[terminate] claw %s terminated successfully", clawID[:8])
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
	factories := s.resolveFactories()
	if err := s.db.QueryRow(`SELECT tags FROM claws WHERE linear_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&tagsJSON); err == nil {
		var tags []string
		if json.Unmarshal([]byte(tagsJSON), &tags) == nil {
			if factory := s.factoryFromTags(tags, factories); factory != nil {
				return factory
			}
		}
	}
	// Also check github_issue_id column
	if err := s.db.QueryRow(`SELECT tags FROM claws WHERE github_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&tagsJSON); err == nil {
		var tags []string
		if json.Unmarshal([]byte(tagsJSON), &tags) == nil {
			if factory := s.factoryFromTags(tags, factories); factory != nil {
				return factory
			}
		}
	}
	if err := s.db.QueryRow(`SELECT tags FROM claws WHERE shortcut_story_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&tagsJSON); err == nil {
		var tags []string
		if json.Unmarshal([]byte(tagsJSON), &tags) == nil {
			if factory := s.factoryFromTags(tags, factories); factory != nil {
				return factory
			}
		}
	}
	if err := s.db.QueryRow(`SELECT tags FROM claws WHERE jira_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`, issueID).Scan(&tagsJSON); err == nil {
		var tags []string
		if json.Unmarshal([]byte(tagsJSON), &tags) == nil {
			if factory := s.factoryFromTags(tags, factories); factory != nil {
				return factory
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
	if strings.HasPrefix(issueID, "gh-") {
		expectedIntegration = "github-issues"
	}

	for _, factory := range factories {
		if factory.Integration != expectedIntegration {
			continue
		}
		if factory.Team == "" || strings.EqualFold(factory.Team, teamKey) {
			return factory
		}
	}
	return nil
}

func (s *Server) factoryFromTags(tags []string, factories []*types.FactoryConfig) *types.FactoryConfig {
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "factory:") {
			continue
		}
		factoryName := strings.TrimPrefix(tag, "factory:")
		for _, factory := range factories {
			if factory.Name == factoryName {
				return factory
			}
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

// commentLinearIssue adds a comment to a Linear issue using the commentCreate GraphQL mutation.
func (s *Server) commentLinearIssue(token, issueIdentifier, body string) error {
	base := s.linearBaseURL
	if base == "" {
		base = "https://api.linear.app"
	}
	return commentLinearIssueWithBase(base, token, issueIdentifier, body)
}

// commentLinearIssueWithBase adds a comment to a Linear issue against a custom base URL.
func commentLinearIssueWithBase(baseURL, token, issueIdentifier, body string) error {
	// Resolve issue ID from identifier
	queryBody := map[string]interface{}{
		"query":     "query($id: String!) { issue(id: $id) { id } }",
		"variables": map[string]string{"id": issueIdentifier},
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
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Linear API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data struct {
			Issue struct {
				ID string `json:"id"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	issueID := result.Data.Issue.ID
	if issueID == "" {
		return fmt.Errorf("issue %s not found", issueIdentifier)
	}

	// Create comment
	mut := `mutation($issueId: String!, $body: String!) {
		commentCreate(input: { issueId: $issueId, body: $body }) {
			success
			comment { id }
		}
	}`
	mutBody := map[string]interface{}{
		"query":     mut,
		"variables": map[string]string{"issueId": issueID, "body": body},
	}
	mutJSON, _ := json.Marshal(mutBody)
	req2, _ := http.NewRequest("POST", baseURL+"/graphql", strings.NewReader(string(mutJSON)))
	req2.Header.Set("Authorization", token)
	req2.Header.Set("Content-Type", "application/json")

	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode >= 400 {
		body, _ := io.ReadAll(resp2.Body)
		return fmt.Errorf("Linear API error %d: %s", resp2.StatusCode, string(body))
	}

	var commentResult struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&commentResult); err != nil {
		return err
	}
	if !commentResult.Data.CommentCreate.Success {
		return fmt.Errorf("commentCreate returned success=false")
	}
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
	CreatedAt   string `json:"createdAt"`
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

	// Linear's issue(id:) actually accepts the display identifier like "CAN-61"
	// directly (not just UUID). This is documented in Linear's GraphQL examples.
	queryBody := map[string]interface{}{
		"query": "query($id: String!) { issue(id: $id) { identifier title url description createdAt } }",
		"variables": map[string]string{
			"id": issueIdentifier,
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

	var result struct {
		Data struct {
			Issue linearIssueDetails `json:"issue"`
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
		combined := strings.Join(errMsgs, "; ")
		log.Printf("[linear] fetchLinearIssueDetails GraphQL errors for %s: %s", issueIdentifier, combined)
		log.Printf("[linear] fetchLinearIssueDetails response body for %s: %s", issueIdentifier, string(bodyBytes))
		return nil, fmt.Errorf("GraphQL error: %s", combined)
	}
	if result.Data.Issue.Identifier == "" {
		log.Printf("[linear] fetchLinearIssueDetails: empty issue returned for %s", issueIdentifier)
		return nil, fmt.Errorf("issue %s not found", issueIdentifier)
	}
	log.Printf("[linear] fetchLinearIssueDetails success for %s: id=%s title=%s", issueIdentifier, result.Data.Issue.Identifier, result.Data.Issue.Title)
	return &result.Data.Issue, nil
}
