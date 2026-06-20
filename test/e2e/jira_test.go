//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	jiraE2EIssueType    = "Bug"
	jiraE2EInitialState = "To do"
	jiraE2ETriggerState = "Ready for Agent"
	jiraE2EWorkingState = "Agent Working"
)

func TestDaytonaJiraWorkflowE2E(t *testing.T) {
	runJiraWorkflowE2E(t, "daytona")
}

func TestReplicatedJiraWorkflowE2E(t *testing.T) {
	runJiraWorkflowE2E(t, "replicated")
}

func runJiraWorkflowE2E(t *testing.T, sandboxProvider string) {
	runID := e2eRunID()
	env := newE2EEnv(t, runID, sandboxProvider)
	env.JiraBaseURL = strings.TrimRight(requiredEnv(t, "ELASTICCLAW_E2E_JIRA_BASE_URL"), "/")
	env.JiraUsername = requiredEnv(t, "ELASTICCLAW_E2E_JIRA_USERNAME")
	env.JiraToken = requiredEnv(t, "ELASTICCLAW_E2E_JIRA_TOKEN")
	env.JiraProjectKey = requiredEnv(t, "ELASTICCLAW_E2E_JIRA_PROJECT_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	workspaceName := "e2e-jira-" + env.RunID
	workflowName := "jira-" + env.RunID
	labelName := "elasticclaw-e2e-" + env.RunID
	webhookSecret := "jira-e2e-secret-" + env.RunID

	cleanupProvider(ctx, t, env)
	hub := startHub(ctx, t, env)
	root := writeJiraWorkspaceFixture(t, env, workspaceName, workflowName, labelName)
	keyPath := writeGitHubAppPrivateKey(t, root, env.GitHubAppPrivateKey)
	jira := jiraClient{baseURL: env.JiraBaseURL, username: env.JiraUsername, token: env.JiraToken}

	var issueKey string
	var agentID string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		var provider, providerID string
		if agentID != "" {
			provider, providerID = hub.agentProvider(cleanupCtx, t, agentID)
			_ = hub.deleteAgent(cleanupCtx, agentID)
		}
		if providerID != "" {
			destroyProviderInstanceByID(cleanupCtx, t, env, provider, providerID)
		}
	})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		cleanupProvider(cleanupCtx, t, env)
	})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		if issueKey != "" {
			_ = jira.deleteIssue(cleanupCtx, issueKey)
		}
		_ = hub.deleteWorkspace(cleanupCtx, workspaceName)
	})

	runCLI(ctx, t, root, env, "workspace", "push", workspaceName)
	runCLI(ctx, t, root, env, "github-app", "create", "e2e",
		"--workspace", workspaceName,
		"--app-id", env.GitHubAppID,
		"--url", env.GitHubAppURL,
		"--installation", env.GitHubInstallation,
		"--private-key-file", keyPath,
	)
	hub.putIssueTrackerWithConfig(ctx, t, workspaceName, map[string]string{
		"type":          "jira",
		"workspace":     "default",
		"baseUrl":       env.JiraBaseURL,
		"username":      env.JiraUsername,
		"token":         env.JiraToken,
		"webhookSecret": webhookSecret,
	})
	runCLI(ctx, t, root, env, "workflow", "push", "--workspace", workspaceName, filepath.Join(root, ".elasticclaw", "workflows", "jira.yaml"))

	issue := jira.createIssue(ctx, t, env.JiraProjectKey, labelName, "Tell a dad joke. Do not make a PR.", "Tell a dad joke. Do not make a PR.\n\nElasticClaw E2E run: "+env.RunID)
	issueKey = issue.Key
	before := jira.getIssue(ctx, t, issueKey)
	if !strings.EqualFold(before.Fields.Status.Name, jiraE2EInitialState) {
		jira.transitionIssue(ctx, t, issueKey, jiraE2EInitialState)
		before = jira.getIssue(ctx, t, issueKey)
	}
	jira.transitionIssue(ctx, t, issueKey, jiraE2ETriggerState)
	after := jira.getIssue(ctx, t, issueKey)

	if jiraManualWebhookDelivery() {
		payload := jiraWebhookPayloadForE2E(after, before.Fields.Status.Name, jiraE2ETriggerState, env.RunID)
		jira.postElasticClawWebhook(ctx, t, env.PublicURL+"/api/workspaces/"+workspaceName+"/webhooks/jira", webhookSecret, payload)
	}

	agentID = waitForOneAgent(ctx, t, hub, issueKey)
	jira.waitForIssueStatus(ctx, t, issueKey, jiraE2EWorkingState)
	waitForAgentStatus(ctx, t, hub, agentID, "connected")
	waitForAgentReply(ctx, t, hub, agentID)

	if jiraManualWebhookDelivery() {
		waitForSingleAgentToRemain(ctx, t, hub, issueKey, 40*time.Second)
	}
}

func writeJiraWorkspaceFixture(t *testing.T, env e2eEnv, workspaceName, workflowName, labelName string) string {
	t.Helper()
	root := t.TempDir()
	workspaceDir := filepath.Join(root, ".elasticclaw", "workspaces", workspaceName)
	workflowDir := filepath.Join(root, ".elasticclaw", "workflows")
	if err := os.MkdirAll(workspaceDir, 0750); err != nil {
		t.Fatalf("mkdir workspace fixture: %v", err)
	}
	if err := os.MkdirAll(workflowDir, 0750); err != nil {
		t.Fatalf("mkdir workflow fixture: %v", err)
	}
	writeFile(t, filepath.Join(workspaceDir, "elasticclaw-config.yaml"), fmt.Sprintf(`schema_version: v1
name: %s
provider: %s
repositories:
  - repo: %s
    permissions: write
`, workspaceName, env.SandboxProvider, env.GitHubRepo))
	writeFile(t, filepath.Join(workspaceDir, "AGENTS.md"), "You are an ElasticClaw E2E agent. Keep responses concise.\n")
	writeFile(t, filepath.Join(workspaceDir, "TOOLS.md"), "Use tools only when the issue asks for them.\n")
	writeFile(t, filepath.Join(workspaceDir, "CONTEXT.md"), "This is an ElasticClaw Jira E2E test. Follow the Jira issue exactly.\n")
	writeFile(t, filepath.Join(workflowDir, "jira.yaml"), fmt.Sprintf(`schema_version: v1
name: %s

trigger:
  jira:
    event: status_changed
    workspace: default
    projects:
      - %s
    states:
      - %s
    labels:
      - %s

concurrency_group: e2e-jira-%s
working_status: %s

stages:
  - id: working
    label: Working
    entry: true
    on_enter:
      inject: |
        Issue: {{.Issue.Identifier}} - {{.Issue.Title}}
        URL: {{.Issue.URL}}

        Do exactly what this issue asks.
        Do not create a pull request.
`, workflowName, env.JiraProjectKey, jiraE2ETriggerState, labelName, env.RunID, jiraE2EWorkingState))
	return root
}

type jiraClient struct {
	baseURL  string
	username string
	token    string
}

type jiraE2EIssue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Self   string `json:"self"`
	Fields struct {
		Summary     string   `json:"summary"`
		Description any      `json:"description"`
		Labels      []string `json:"labels"`
		Updated     string   `json:"updated"`
		Status      struct {
			Name string `json:"name"`
		} `json:"status"`
		Project struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"project"`
		Assignee *struct {
			AccountID    string `json:"accountId"`
			DisplayName  string `json:"displayName"`
			EmailAddress string `json:"emailAddress"`
		} `json:"assignee"`
	} `json:"fields"`
}

func (c jiraClient) createIssue(ctx context.Context, t *testing.T, projectKey, labelName, summary, description string) jiraE2EIssue {
	t.Helper()
	body := map[string]any{
		"fields": map[string]any{
			"project":     map[string]string{"key": projectKey},
			"issuetype":   map[string]string{"name": jiraE2EIssueType},
			"summary":     summary,
			"description": jiraADFDocument(description),
			"labels":      []string{labelName},
		},
	}
	var out jiraE2EIssue
	c.api(ctx, t, http.MethodPost, "/rest/api/3/issue", body, &out)
	if out.Key == "" {
		t.Fatalf("Jira issueCreate did not return an issue key")
	}
	return out
}

func (c jiraClient) getIssue(ctx context.Context, t *testing.T, key string) jiraE2EIssue {
	t.Helper()
	var out jiraE2EIssue
	c.api(ctx, t, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"?fields=summary,description,status,labels,assignee,project,updated", nil, &out)
	if out.Key == "" {
		t.Fatalf("Jira issue %q did not return an issue key", key)
	}
	return out
}

func (c jiraClient) transitionIssue(ctx context.Context, t *testing.T, key, targetStatus string) {
	t.Helper()
	if strings.TrimSpace(targetStatus) == "" {
		return
	}
	var transitions struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	c.api(ctx, t, http.MethodGet, "/rest/api/3/issue/"+url.PathEscape(key)+"/transitions", nil, &transitions)
	transitionID := ""
	for _, transition := range transitions.Transitions {
		if strings.EqualFold(transition.Name, targetStatus) || strings.EqualFold(transition.To.Name, targetStatus) {
			transitionID = transition.ID
			break
		}
	}
	if transitionID == "" {
		t.Fatalf("Jira issue %s has no transition to status %q", key, targetStatus)
	}
	c.api(ctx, t, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/transitions", map[string]any{
		"transition": map[string]string{"id": transitionID},
	}, nil)
}

func (c jiraClient) waitForIssueStatus(ctx context.Context, t *testing.T, key, targetStatus string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var lastStatus string
	for time.Now().Before(deadline) {
		issue := c.getIssue(ctx, t, key)
		lastStatus = issue.Fields.Status.Name
		if strings.EqualFold(lastStatus, targetStatus) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for Jira issue %s status %q; last status %q", key, targetStatus, lastStatus)
}

func (c jiraClient) deleteIssue(ctx context.Context, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/rest/api/3/issue/"+url.PathEscape(key), nil)
	if err != nil {
		return err
	}
	c.applyAuth(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete Jira issue: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (c jiraClient) postElasticClawWebhook(ctx context.Context, t *testing.T, webhookURL, secret string, body map[string]any) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Jira webhook payload: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("build Jira webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ElasticClaw-Webhook-Secret", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post Jira webhook: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("Jira webhook returned %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
}

func (c jiraClient) api(ctx context.Context, t *testing.T, method, path string, body interface{}, out interface{}) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal Jira %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		t.Fatalf("build Jira %s %s: %v", method, path, err)
	}
	c.applyAuth(req)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Jira %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("Jira %s %s returned %s: %s", method, path, resp.Status, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			t.Fatalf("decode Jira %s %s: %v\n%s", method, path, err, string(respBody))
		}
	}
}

func (c jiraClient) applyAuth(req *http.Request) {
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.token)))
}

func jiraADFDocument(text string) map[string]any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []map[string]any{{
			"type": "paragraph",
			"content": []map[string]string{{
				"type": "text",
				"text": text,
			}},
		}},
	}
}

func jiraWebhookPayloadForE2E(issue jiraE2EIssue, previousStatus, currentStatus, runID string) map[string]any {
	user := map[string]any{"displayName": "ElasticClaw E2E"}
	if issue.Fields.Assignee != nil {
		user = map[string]any{
			"accountId":    issue.Fields.Assignee.AccountID,
			"displayName":  issue.Fields.Assignee.DisplayName,
			"emailAddress": issue.Fields.Assignee.EmailAddress,
		}
	}
	return map[string]any{
		"webhookEvent": "jira:issue_updated",
		"timestamp":    time.Now().UnixMilli(),
		"user":         user,
		"issue":        issue,
		"changelog": map[string]any{
			"id": "elasticclaw-e2e-" + runID,
			"items": []map[string]string{{
				"field":      "status",
				"fromString": previousStatus,
				"toString":   currentStatus,
			}},
		},
	}
}

func jiraManualWebhookDelivery() bool {
	value := strings.TrimSpace(os.Getenv("ELASTICCLAW_E2E_JIRA_MANUAL_WEBHOOK"))
	return value == "" || strings.EqualFold(value, "1") || strings.EqualFold(value, "true")
}

func waitForSingleAgentToRemain(ctx context.Context, t *testing.T, hub *hubProcess, name string, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		count := 0
		for _, agent := range hub.listAgents(ctx, t) {
			if agent.Name == name {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("found %d agents named %s after Jira poller ran, want exactly 1", count, name)
		}
		time.Sleep(5 * time.Second)
	}
}
