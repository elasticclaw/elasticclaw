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
	"strings"

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
	hasAnySecret := false

	// Check integration-level secrets
	if s.hubCfg.Integrations != nil {
		for _, li := range s.hubCfg.Integrations.Linear {
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
	if s.hubCfg.Factories != nil {
		for _, factory := range s.hubCfg.Factories {
			if factory.Integration != "linear" || factory.WebhookSecret == "" {
				continue
			}
			hasAnySecret = true
			mac := hmac.New(sha256.New, []byte(factory.WebhookSecret))
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
		if factory.Team != "" && !strings.EqualFold(factory.Team, teamKey) {
			continue
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
			if factory.Integration == "linear" && (factory.Team == "" || strings.EqualFold(factory.Team, teamKey)) {
				s.logFactoryEvent(factory.Name, issueID, payload.Data.Title, previousStatus, currentStatus, "not_actionable",
					"", fmt.Sprintf("status '%s'→'%s' did not match trigger '%s'", previousStatus, currentStatus, factory.TriggerStatus))
			}
		}
	}
}

func (s *Server) createClawForIssue(factory *types.FactoryConfig, payload linearWebhookPayload) error {
	issueID := payload.Data.Identifier

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

	// Inject CONTEXT.md with issue details
	templateFiles["CONTEXT.md"] = buildLinearContext(payload)

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

	// Find provider from factory or hub config default
	provider := s.defaultProvider()
	if provider == "" {
		return fmt.Errorf("no provider configured")
	}

	// Find Linear token for this workspace
	linearToken := s.resolveLinearTokenForFactory(factory)

	// Build env vars
	env := map[string]string{
		"ELASTICCLAW_HUB_URL":    s.clawHubURL(),
		"ELASTICCLAW_CLAW_TOKEN": s.hubCfg.ClawToken,
	}
	if linearToken != "" {
		env["LINEAR_API_KEY"] = linearToken
	}

	// Resolve template config fields (from elasticclaw-config.yaml if present).
	// Factory-level overrides (color, tags) take precedence over template config.
	var (
		instanceType   string
		defaultModel   string
		llmKey         string
		nixEnabled     int
		githubRepos    []types.GitHubRepoAccess
		linearWorkspace string
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

	// Build tags — always include factory tag; merge with factory-configured tags
	tags := []string{"factory:" + factory.Name}
	for _, t := range factory.Tags {
		if t != "factory:"+factory.Name {
			tags = append(tags, t)
		}
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
		INSERT INTO claws(id, tenant_id, name, template, provider, default_model, template_files, github_repos, linear_workspace, nix, tags, color, llm_key, linear_issue_id, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,'provisioning',?)`,
		clawID, tenantID, clawName, factory.Template, provider, defaultModel, string(filesJSON),
		string(githubReposJSON), linearWorkspace, nixEnabled, string(tagsJSON), clawColor, llmKey, issueID, now,
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
		switch provider {
		case "replicated":
			if err := s.provisionReplicated(ctx, clawID, req, provCfg, env); err != nil {
				log.Printf("[factory] provision failed for %s: %v", clawID, err)
				// Only mark error if not already deleted
				_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=? AND status != 'deleted'`, clawID)
			}
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
		if p.Token != "" || p.APIKey != "" {
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

func escapeLikeWildcards(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func buildLinearContext(payload linearWebhookPayload) string {
	d := payload.Data
	var b strings.Builder
	b.WriteString("# CONTEXT.md - Issue Context\n\n")
	b.WriteString("This claw was automatically created by ElasticClaw to work on a Linear issue.\n\n")
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
	b.WriteString("Read this file first. Understand the issue. Then look at the codebase and implement it.\n")
	b.WriteString("When done: move the Linear issue to the configured done status using the Linear API, then signal done.\n")
	return b.String()
}

// Avoid collision with existing now() function

// handleClawDoneSignal is called when a claw sends a message containing [DONE].
// It parses PR URLs from the message, validates them via the GitHub API, stores
// them in claw_prs, then moves the Linear issue and terminates the claw.
// If no valid open PRs are found (and a GH App is configured), it injects an
// error message back so the claw can retry.
func (s *Server) handleClawDoneSignal(clawID, rawMessage string) {
	// Get the linear_issue_id for this claw
	var issueID string
	if err := s.db.QueryRow(`SELECT linear_issue_id FROM claws WHERE id = ?`, clawID).Scan(&issueID); err != nil || issueID == "" {
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

	// Move Linear issue to done_status if configured
	if factory.DoneStatus != "" {
		linearToken := s.resolveLinearTokenForFactory(factory)
		if linearToken != "" {
			if err := moveLinearIssue(linearToken, issueID, factory.DoneStatus); err != nil {
				log.Printf("[factory] failed to move issue %s to '%s': %v", issueID, factory.DoneStatus, err)
			} else {
				log.Printf("[factory] moved issue %s to '%s'", issueID, factory.DoneStatus)
			}
		}
	}

	// Mark claw as completed
	_, _ = s.db.Exec(`UPDATE claws SET status='completed' WHERE id=?`, clawID)
	s.mu.Lock()
	if cc, ok := s.claws[clawID]; ok {
		cc.conn.Close(1000, "factory: claw signaled done")
		delete(s.claws, clawID)
	}
	s.mu.Unlock()
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
		data, err := githubAPIWithBase(base, fmt.Sprintf("repos/%s/pulls/%d", pr.repo, pr.number), ghToken)
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
	// Extract team key from issue ID (e.g. "ELA" from "ELA-123")
	parts := strings.SplitN(issueID, "-", 2)
	if len(parts) != 2 {
		return nil
	}
	teamKey := parts[0]

	for _, factory := range s.hubCfg.Factories {
		if factory.Integration != "linear" {
			continue
		}
		if factory.Team == "" || strings.EqualFold(factory.Team, teamKey) {
			return factory
		}
	}
	return nil
}

// moveLinearIssue updates a Linear issue's state by name using the Linear GraphQL API.
func moveLinearIssue(token, issueIdentifier, targetStateName string) error {
	// First find the issue ID from identifier using GraphQL variables
	queryBody := map[string]interface{}{
		"query": "query($id: String!) { issue(id: $id) { id team { states { nodes { id name } } } } }",
		"variables": map[string]string{
			"id": issueIdentifier,
		},
	}
	queryJSON, _ := json.Marshal(queryBody)
	req, _ := http.NewRequest("POST", "https://api.linear.app/graphql", strings.NewReader(string(queryJSON)))
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
	req2, _ := http.NewRequest("POST", "https://api.linear.app/graphql", strings.NewReader(string(mutationJSON)))
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
