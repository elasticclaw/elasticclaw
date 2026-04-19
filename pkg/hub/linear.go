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
		Identifier  string `json:"identifier"`  // e.g. "ELA-123"
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
		PreviousState *struct {
			Name string `json:"name"`
		} `json:"previousState,omitempty"`
	} `json:"data"`
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
	if s.hubCfg.Integrations == nil {
		return true // no integrations configured, accept all
	}
	for _, li := range s.hubCfg.Integrations.Linear {
		if li.WebhookSecret == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(li.WebhookSecret))
		mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return true
		}
	}
	// If secrets are configured but none matched, reject
	for _, li := range s.hubCfg.Integrations.Linear {
		if li.WebhookSecret != "" {
			return false
		}
	}
	return true // no secrets configured
}

func (s *Server) processLinearEvent(payload linearWebhookPayload) {
	if s.hubCfg.Factories == nil {
		return
	}

	currentStatus := payload.Data.State.Name
	previousStatus := ""
	if payload.Data.PreviousState != nil {
		previousStatus = payload.Data.PreviousState.Name
	}
	teamKey := payload.Data.Team.Key
	issueID := payload.Data.Identifier // e.g. "ELA-123"

	for _, factory := range s.hubCfg.Factories {
		if factory.Integration != "linear" {
			continue
		}
		if factory.Team != "" && !strings.EqualFold(factory.Team, teamKey) {
			continue
		}

		// Issue entering trigger status → create claw
		if currentStatus == factory.TriggerStatus && previousStatus != factory.TriggerStatus {
			log.Printf("[factory:%s] issue %s entered '%s' — creating claw", factory.Name, issueID, factory.TriggerStatus)
			if err := s.createClawForIssue(factory, payload); err != nil {
				log.Printf("[factory:%s] failed to create claw for %s: %v", factory.Name, issueID, err)
			}
		}

		// Issue leaving trigger status → terminate claw
		if factory.TerminateOnLeave && previousStatus == factory.TriggerStatus && currentStatus != factory.TriggerStatus {
			log.Printf("[factory:%s] issue %s left '%s' — terminating claw", factory.Name, issueID, factory.TriggerStatus)
			s.terminateClawForIssue(issueID)
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

	// Insert claw record
	clawID := uuid.New().String()
	filesJSON, _ := json.Marshal(filesMapToBytes(templateFiles))
	now := now()

	_, err = s.db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, template_files, linear_issue_id, status, created_at)
		VALUES(?,?,?,?,?,?,?,'provisioning',?)`,
		clawID, tenantID, clawName, factory.Template, provider, string(filesJSON), issueID, now,
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
		_ = req // provisioning called below
		switch provider {
		case "replicated":
			if err := s.provisionReplicated(ctx, clawID, req, provCfg, env); err != nil {
				log.Printf("[factory] provision failed for %s: %v", clawID, err)
				_, _ = s.db.Exec(`UPDATE claws SET status='error' WHERE id=?`, clawID)
			}
		}
	}()

	log.Printf("[factory] created claw %s (%s) for Linear issue %s", clawName, clawID[:8], issueID)
	return nil
}

func (s *Server) terminateClawForIssue(issueID string) {
	var clawID string
	if err := s.db.QueryRow(
		`SELECT id FROM claws WHERE linear_issue_id = ? AND status NOT IN ('error','deleted') LIMIT 1`,
		issueID,
	).Scan(&clawID); err != nil {
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

func filesMapToBytes(files map[string]string) map[string][]byte {
	result := make(map[string][]byte, len(files))
	for k, v := range files {
		result[k] = []byte(v)
	}
	return result
}

// Avoid collision with existing now() function

