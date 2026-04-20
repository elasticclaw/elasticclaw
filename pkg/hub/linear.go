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

		// Issue entering trigger status → create claw
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
				`SELECT id FROM claws WHERE linear_issue_id = ? AND status NOT IN ('error','deleted') AND tags LIKE ? LIMIT 1`,
				issueID,
				"%factory:"+factory.Name+"%",
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

	// Build tags — always include factory tag; merge with factory-configured tags
	tags := []string{"factory:" + factory.Name}
	for _, t := range factory.Tags {
		if t != "factory:"+factory.Name {
			tags = append(tags, t)
		}
	}
	tagsJSON, _ := json.Marshal(tags)

	// Color
	clawColor := factory.Color

	// Insert claw record
	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(templateFiles)
	now := now()

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, template_files, linear_issue_id, tags, color, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,'provisioning',?)`,
		clawID, tenantID, clawName, factory.Template, provider, string(filesJSON), issueID, string(tagsJSON), clawColor, now,
	)
	if err != nil {
		return fmt.Errorf("db insert: %w", err)
	}

	// Provision asynchronously
	provCfg, _ := s.hubCfg.Providers[provider]
	go func() {
		ctx := context.Background()
		req := types.CreateClawRequest{
			Name:         clawName,
			TemplateName: factory.Template,
			Provider:     provider,
			Files:        templateFiles,
			Env:          env,
		}
		switch provider {
		case "replicated":
			if err := s.provisionReplicated(ctx, clawID, req, provCfg, env); err != nil {
				log.Printf("[factory] provision failed for %s: %v", clawID, err)
				_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
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
// Finds the matching factory config and moves the Linear issue to done_status.
func (s *Server) handleClawDoneSignal(clawID string) {
	// Get the linear_issue_id for this claw
	var issueID string
	if err := s.db.QueryRow(`SELECT linear_issue_id FROM claws WHERE id = ?`, clawID).Scan(&issueID); err != nil || issueID == "" {
		return // not a factory claw
	}

	log.Printf("[factory] claw %s sent [DONE] for issue %s", clawID[:8], issueID)

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
