package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	agentErrorReassignPreviousOwner = "previous_owner"
	agentErrorReassignNone          = "none"
	defaultAgentErrorComment        = `Agent stopped because of an unrecoverable error.

Reason:
{{ .Reason }}

{{ .OriginalOwnerLine }}

{{ .AssignmentResult }}`
)

type trackerOwner struct {
	ID      string `json:"id,omitempty"`
	Login   string `json:"login,omitempty"`
	Name    string `json:"name,omitempty"`
	Mention string `json:"mention,omitempty"`
}

type agentErrorPolicy struct {
	Enabled    bool
	Marker     string
	Comment    string
	ReassignTo string
}

type agentErrorContext struct {
	ClawID          string
	Reason          string
	Integration     string
	IssueID         string
	OriginalOwners  []trackerOwner
	AgentOwners     []trackerOwner
	PipelineContext pipelineContext
}

func (s *Server) handleAgentError(clawID, reason string) {
	ctx, ok := s.loadAgentErrorContext(clawID, reason)
	if !ok {
		return
	}
	policy := s.resolveAgentErrorPolicy(ctx)
	if !policy.Enabled {
		return
	}
	switch ctx.Integration {
	case "github-issues":
		if err := s.handleGitHubIssueAgentError(ctx, policy); err != nil {
			log.Printf("[agent-error] github issue %s: %v", ctx.IssueID, err)
		}
	default:
		// Other integrations continue to use their existing stop comments until
		// their adapters grow marker/reassignment support.
		return
	}
}

func (s *Server) loadAgentErrorContext(clawID, reason string) (agentErrorContext, bool) {
	ctx := agentErrorContext{ClawID: clawID, Reason: reason}
	pctx, _ := s.findPipelineContextForClaw(clawID)
	ctx.PipelineContext = pctx
	ctx.IssueID = pctx.IssueID
	ctx.Integration = pctx.Integration()

	var integration, issueID, originalOwnersJSON, agentOwnersJSON string
	err := s.db.QueryRow(`
		SELECT integration, issue_id, original_owners, agent_owners
		  FROM claw_tracker_contexts WHERE claw_id=?`,
		clawID,
	).Scan(&integration, &issueID, &originalOwnersJSON, &agentOwnersJSON)
	if err == nil {
		if integration != "" {
			ctx.Integration = integration
		}
		if issueID != "" {
			ctx.IssueID = issueID
		}
		_ = json.Unmarshal([]byte(originalOwnersJSON), &ctx.OriginalOwners)
		_ = json.Unmarshal([]byte(agentOwnersJSON), &ctx.AgentOwners)
	}

	if ctx.Integration == "" || ctx.IssueID == "" {
		return ctx, false
	}
	return ctx, true
}

func (s *Server) resolveAgentErrorPolicy(ctx agentErrorContext) agentErrorPolicy {
	policy := agentErrorPolicy{
		Enabled:    true,
		Comment:    defaultAgentErrorComment,
		ReassignTo: agentErrorReassignPreviousOwner,
	}
	switch ctx.Integration {
	case "github-issues":
		policy.Marker = "agent-error"
	case "linear":
		policy.Marker = "Agent Error"
	}

	var override *types.AgentErrorConfig
	if ctx.PipelineContext.Workflow != nil {
		override = ctx.PipelineContext.Workflow.AgentError
	} else if ctx.PipelineContext.Factory != nil {
		override = ctx.PipelineContext.Factory.AgentError
	}
	if override == nil {
		return policy
	}
	if override.Enabled != nil {
		policy.Enabled = *override.Enabled
	}
	if override.Marker != "" {
		policy.Marker = override.Marker
	}
	if override.Comment != "" {
		policy.Comment = override.Comment
	}
	if override.ReassignTo != "" {
		policy.ReassignTo = override.ReassignTo
	}
	return policy
}

func (s *Server) saveTrackerContext(clawID, integration, issueID string, originalOwners, agentOwners []trackerOwner) {
	if clawID == "" || integration == "" || issueID == "" {
		return
	}
	originalJSON, _ := json.Marshal(originalOwners)
	agentJSON, _ := json.Marshal(agentOwners)
	if _, err := s.db.Exec(`
		INSERT INTO claw_tracker_contexts(claw_id, integration, issue_id, original_owners, agent_owners, created_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(claw_id) DO UPDATE SET
			integration=excluded.integration,
			issue_id=excluded.issue_id,
			original_owners=excluded.original_owners,
			agent_owners=excluded.agent_owners`,
		clawID, integration, issueID, string(originalJSON), string(agentJSON), time.Now().UTC(),
	); err != nil {
		log.Printf("[agent-error] failed to save tracker context for claw %s: %v", clawID, err)
	}
}

func (s *Server) handleGitHubIssueAgentError(ctx agentErrorContext, policy agentErrorPolicy) error {
	repo, issueNumber, ok := parseGitHubIssueID(ctx.IssueID)
	if !ok {
		return fmt.Errorf("invalid GitHub issue id %q", ctx.IssueID)
	}
	token := s.resolveGitHubIssuesTokenForPipeline(ctx.PipelineContext)
	if token == "" {
		return fmt.Errorf("no GitHub Issues token")
	}
	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}

	currentOwners, err := fetchGitHubIssueOwners(base, token, repo, issueNumber)
	if err != nil {
		return fmt.Errorf("fetch current assignees: %w", err)
	}
	assignmentResult, err := s.applyGitHubAgentErrorAssignment(base, token, repo, issueNumber, currentOwners, ctx.OriginalOwners, ctx.AgentOwners, policy)
	if err != nil {
		assignmentResult = "Assignment was left unchanged because reassignment failed: " + err.Error()
		log.Printf("[agent-error] github issue %s assignment failed: %v", ctx.IssueID, err)
	}

	if strings.TrimSpace(policy.Marker) != "" {
		if err := addGitHubIssueLabelWithCreate(base, repo, issueNumber, strings.TrimSpace(policy.Marker), token); err != nil {
			log.Printf("[agent-error] github issue %s label %q failed: %v", ctx.IssueID, policy.Marker, err)
		}
	}

	comment := renderAgentErrorComment(policy.Comment, ctx, assignmentResult)
	if err := commentGitHubIssueWithBase(base, token, repo, issueNumber, comment); err != nil {
		return fmt.Errorf("comment: %w", err)
	}
	return nil
}

func (s *Server) applyGitHubAgentErrorAssignment(base, token, repo string, issueNumber int, current, original, agentOwners []trackerOwner, policy agentErrorPolicy) (string, error) {
	if policy.ReassignTo == agentErrorReassignNone {
		return "Assignment was left unchanged by policy.", nil
	}
	if len(original) == 0 {
		return "No original owner was recorded. Assignment was left unchanged.", nil
	}
	mentions := ownerMentions(original)
	if ownerSetsIntersect(current, original) {
		return "Assignment was left unchanged because the issue is already assigned to an original owner.", nil
	}
	if len(current) > 0 && !onlyKnownOwners(current, agentOwners) {
		return "Assignment was left unchanged because the issue is assigned to another user.", nil
	}
	if err := addGitHubIssueAssignees(base, token, repo, issueNumber, ownerLogins(original)); err != nil {
		return "", err
	}
	if len(current) > 0 && len(agentOwners) > 0 {
		if err := removeGitHubIssueAssignees(base, token, repo, issueNumber, ownerLogins(agentOwners)); err != nil {
			return "", err
		}
	}
	return "Reassigned this issue back to " + mentions + ".", nil
}

func renderAgentErrorComment(template string, ctx agentErrorContext, assignmentResult string) string {
	originalLine := "No original owner was recorded."
	if mentions := ownerMentions(ctx.OriginalOwners); mentions != "" {
		originalLine = "Original owner: " + mentions
	}
	replacements := map[string]string{
		"{{ .Reason }}":               ctx.Reason,
		"{{ .OriginalOwnerLine }}":    originalLine,
		"{{ .OriginalOwnerMention }}": ownerMentions(ctx.OriginalOwners),
		"{{ .AssignmentResult }}":     assignmentResult,
		"{{ .ClawID }}":               ctx.ClawID,
	}
	out := template
	for from, to := range replacements {
		out = strings.ReplaceAll(out, from, to)
	}
	return strings.TrimSpace(out)
}

func parseGitHubIssueID(issueID string) (string, int, bool) {
	parts := strings.Split(issueID, "/")
	if len(parts) != 3 {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return parts[0] + "/" + parts[1], n, true
}

func fetchGitHubIssueOwners(baseURL, token, repo string, issueNumber int) ([]trackerOwner, error) {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/repos/%s/issues/%d", baseURL, repo, issueNumber), nil)
	if err != nil {
		return nil, err
	}
	setGitHubHeaders(req, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github API GET issue: %d %s", resp.StatusCode, string(body))
	}
	var result struct {
		Assignee *struct {
			Login string `json:"login"`
		} `json:"assignee"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	var owners []trackerOwner
	if result.Assignee != nil {
		owners = appendGitHubOwner(owners, result.Assignee.Login)
	}
	return owners, nil
}

func addGitHubIssueAssignees(baseURL, token, repo string, issueNumber int, logins []string) error {
	if len(logins) == 0 {
		return nil
	}
	return githubIssueAssigneesRequest(baseURL, token, repo, issueNumber, http.MethodPost, logins)
}

func removeGitHubIssueAssignees(baseURL, token, repo string, issueNumber int, logins []string) error {
	if len(logins) == 0 {
		return nil
	}
	return githubIssueAssigneesRequest(baseURL, token, repo, issueNumber, http.MethodDelete, logins)
}

func githubIssueAssigneesRequest(baseURL, token, repo string, issueNumber int, method string, logins []string) error {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	body, _ := json.Marshal(map[string][]string{"assignees": logins})
	req, err := http.NewRequest(method, fmt.Sprintf("%s/repos/%s/issues/%d/assignees", baseURL, repo, issueNumber), bytes.NewReader(body))
	if err != nil {
		return err
	}
	setGitHubHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github API %s assignees: %d %s", method, resp.StatusCode, string(respBody))
	}
	return nil
}

func addGitHubIssueLabelWithCreate(baseURL, repo string, issueNumber int, label, token string) error {
	err := githubAPIAddLabel(baseURL, repo, issueNumber, label, token)
	if err == nil {
		return nil
	}
	if !strings.Contains(strings.ToLower(err.Error()), "label") && !strings.Contains(err.Error(), "404") {
		return err
	}
	if createErr := createGitHubIssueLabel(baseURL, repo, label, token); createErr != nil {
		return err
	}
	return githubAPIAddLabel(baseURL, repo, issueNumber, label, token)
}

func createGitHubIssueLabel(baseURL, repo, label, token string) error {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	body, _ := json.Marshal(map[string]string{
		"name":        label,
		"color":       "d73a4a",
		"description": "Agent stopped because of an unrecoverable error",
	})
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/repos/%s/labels", baseURL, repo), bytes.NewReader(body))
	if err != nil {
		return err
	}
	setGitHubHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(string(respBody), "already_exists") {
		return nil
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github API create label: %d %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func commentGitHubIssueWithBase(baseURL, token, repo string, issueNumber int, body string) error {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	commentBody := map[string]interface{}{"body": body}
	b, _ := json.Marshal(commentBody)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/repos/%s/issues/%d/comments", baseURL, repo, issueNumber), bytes.NewReader(b))
	if err != nil {
		return err
	}
	setGitHubHeaders(req, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github API comment: %d %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func setGitHubHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func githubOwnersFromWebhook(payload githubIssuesWebhookPayload) []trackerOwner {
	var owners []trackerOwner
	if payload.Issue.Assignee != nil {
		owners = appendGitHubOwner(owners, payload.Issue.Assignee.Login)
	}
	return owners
}

func (s *Server) githubOriginalOwnersFromIssue(payload githubIssuesWebhookPayload) []trackerOwner {
	return githubOwnersFromWebhook(payload)
}

func appendGitHubOwner(owners []trackerOwner, login string) []trackerOwner {
	login = strings.TrimSpace(login)
	if login == "" {
		return owners
	}
	for _, owner := range owners {
		if strings.EqualFold(owner.Login, login) {
			return owners
		}
	}
	return append(owners, trackerOwner{Login: login, Mention: "@" + login})
}

func ownerLogins(owners []trackerOwner) []string {
	out := make([]string, 0, len(owners))
	for _, owner := range owners {
		login := strings.TrimSpace(owner.Login)
		if login != "" {
			out = append(out, login)
		}
	}
	return out
}

func ownerMentions(owners []trackerOwner) string {
	mentions := make([]string, 0, len(owners))
	for _, owner := range owners {
		mention := strings.TrimSpace(owner.Mention)
		if mention == "" && owner.Login != "" {
			mention = "@" + owner.Login
		}
		if mention == "" {
			mention = strings.TrimSpace(owner.Name)
		}
		if mention != "" {
			mentions = append(mentions, mention)
		}
	}
	return strings.Join(mentions, ", ")
}

func ownerSetsIntersect(a, b []trackerOwner) bool {
	for _, left := range a {
		for _, right := range b {
			if sameTrackerOwner(left, right) {
				return true
			}
		}
	}
	return false
}

func onlyKnownOwners(current, known []trackerOwner) bool {
	if len(current) == 0 || len(known) == 0 {
		return false
	}
	for _, owner := range current {
		found := false
		for _, knownOwner := range known {
			if sameTrackerOwner(owner, knownOwner) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sameTrackerOwner(a, b trackerOwner) bool {
	switch {
	case a.ID != "" && b.ID != "":
		return a.ID == b.ID
	case a.Login != "" && b.Login != "":
		return strings.EqualFold(a.Login, b.Login)
	case a.Name != "" && b.Name != "":
		return strings.EqualFold(a.Name, b.Name)
	default:
		return false
	}
}
