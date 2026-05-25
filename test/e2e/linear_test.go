//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDaytonaLinearWorkflowE2E(t *testing.T) {
	runID := e2eRunID()
	env := e2eEnv{
		Bin:                 requiredEnv(t, "ELASTICCLAW_E2E_BIN"),
		HubAddr:             envOrDefault("ELASTICCLAW_E2E_HUB_ADDR", "127.0.0.1:8080"),
		PublicURL:           strings.TrimRight(requiredEnv(t, "ELASTICCLAW_E2E_PUBLIC_URL"), "/"),
		GitHubRepo:          envOrDefault("ELASTICCLAW_E2E_GITHUB_REPO", defaultFixture),
		GitHubAppID:         requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_APP_ID"),
		GitHubAppURL:        os.Getenv("ELASTICCLAW_E2E_GITHUB_APP_URL"),
		GitHubInstallation:  os.Getenv("ELASTICCLAW_E2E_GITHUB_APP_INSTALLATION"),
		GitHubAppPrivateKey: requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_APP_PRIVATE_KEY"),
		LinearAPIKey:        requiredEnv(t, "ELASTICCLAW_E2E_LINEAR_API_KEY"),
		LinearTeamKey:       requiredEnv(t, "ELASTICCLAW_E2E_LINEAR_TEAM_KEY"),
		LinearTriggerState:  envOrDefault("ELASTICCLAW_E2E_LINEAR_TRIGGER_STATE", "Todo"),
		LinearInitialState:  os.Getenv("ELASTICCLAW_E2E_LINEAR_INITIAL_STATE"),
		DaytonaAPIKey:       requiredEnv(t, "DAYTONA_API_KEY"),
		FireworksAPIKey:     requiredEnv(t, "FIREWORKS_API_KEY"),
		BridgeBinary:        requiredEnv(t, "ELASTICCLAW_E2E_BRIDGE_BINARY"),
		BridgeToken:         "bridge-" + runID,
		DaytonaPrefix:       daytonaPrefix,
		Model:               envOrDefault("ELASTICCLAW_E2E_MODEL", defaultModel),
		RunID:               runID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	workspaceName := "e2e-linear-" + env.RunID
	workflowName := "linear-" + env.RunID
	webhookSecret := "linear-e2e-secret-" + env.RunID

	cleanupDaytonaE2ESandboxes(ctx, t, env)
	hub := startHub(ctx, t, env)
	root := writeLinearWorkspaceFixture(t, env, workspaceName, workflowName)
	keyPath := writeGitHubAppPrivateKey(t, root, env.GitHubAppPrivateKey)
	linear := linearClient{token: env.LinearAPIKey}

	var webhookID string
	var issueID string
	var issueIdentifier string
	var agentID string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		var providerID string
		if agentID != "" {
			providerID = hub.agentProviderID(cleanupCtx, t, agentID)
			_ = hub.deleteAgent(cleanupCtx, agentID)
		}
		if providerID != "" {
			destroyDaytonaSandboxByID(cleanupCtx, t, env, providerID)
		}
	})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		cleanupDaytonaE2ESandboxes(cleanupCtx, t, env)
	})
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		if issueID != "" {
			_ = linear.archiveIssue(cleanupCtx, issueID)
		}
		if webhookID != "" {
			_ = linear.deleteWebhook(cleanupCtx, webhookID)
		}
		_ = hub.deleteWorkspace(cleanupCtx, workspaceName)
	})

	team := linear.teamByKey(ctx, t, env.LinearTeamKey)
	linear.deleteE2EWebhooks(ctx, t)
	triggerStateID := team.stateID(t, env.LinearTriggerState)
	initialStateID := team.initialStateID(t, env.LinearInitialState, env.LinearTriggerState)

	runCLI(ctx, t, root, env, "workspace", "push", workspaceName)
	runCLI(ctx, t, root, env, "github-app", "create", "e2e",
		"--workspace", workspaceName,
		"--app-id", env.GitHubAppID,
		"--url", env.GitHubAppURL,
		"--installation", env.GitHubInstallation,
		"--private-key-file", keyPath,
	)
	hub.putIssueTracker(ctx, t, workspaceName, "linear", "default", env.LinearAPIKey, webhookSecret)
	runCLI(ctx, t, root, env, "workflow", "push", "--workspace", workspaceName, filepath.Join(root, ".elasticclaw", "workflows", "linear.yaml"))

	webhookID = linear.createWebhook(ctx, t, env.PublicURL+"/api/workspaces/"+workspaceName+"/webhooks/linear", team.ID, webhookSecret)
	issue := linear.createIssue(ctx, t, team.ID, initialStateID, "Tell a dad joke. Do not make a PR.", "Tell a dad joke. Do not make a PR.\n\nElasticClaw E2E run: "+env.RunID)
	issueID = issue.ID
	issueIdentifier = issue.Identifier
	linear.updateIssueState(ctx, t, issueID, triggerStateID)

	agentID = waitForOneAgent(ctx, t, hub, issueIdentifier)
	waitForAgentStatus(ctx, t, hub, agentID, "connected")
	waitForAgentReply(ctx, t, hub, agentID)
}

func writeLinearWorkspaceFixture(t *testing.T, env e2eEnv, workspaceName, workflowName string) string {
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
provider: daytona
repositories:
  - repo: %s
    permissions: write
`, workspaceName, env.GitHubRepo))
	writeFile(t, filepath.Join(workspaceDir, "AGENTS.md"), "You are an ElasticClaw E2E agent. Keep responses concise.\n")
	writeFile(t, filepath.Join(workspaceDir, "TOOLS.md"), "Use tools only when the issue asks for them.\n")
	writeFile(t, filepath.Join(workspaceDir, "CONTEXT.md"), "This is an ElasticClaw Linear E2E test. Follow the Linear issue exactly.\n")
	writeFile(t, filepath.Join(workflowDir, "linear.yaml"), fmt.Sprintf(`schema_version: v1
name: %s

trigger:
  linear:
    event: status_changed
    team: %s
    states:
      - %s

concurrency_group: e2e-linear-%s

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
`, workflowName, env.LinearTeamKey, env.LinearTriggerState, env.RunID))
	return root
}

type linearClient struct {
	token string
}

type linearTeam struct {
	ID     string
	Key    string
	States []linearState
}

type linearState struct {
	ID   string
	Name string
	Type string
}

type linearIssue struct {
	ID         string
	Identifier string
	URL        string
}

func (c linearClient) teamByKey(ctx context.Context, t *testing.T, key string) linearTeam {
	t.Helper()
	var out struct {
		Data struct {
			Teams struct {
				Nodes []struct {
					ID     string `json:"id"`
					Key    string `json:"key"`
					States struct {
						Nodes []linearState `json:"nodes"`
					} `json:"states"`
				} `json:"nodes"`
			} `json:"teams"`
		} `json:"data"`
	}
	c.graphql(ctx, t, `query {
  teams {
    nodes {
      id
      key
      states {
        nodes { id name type }
      }
    }
  }
}`, nil, &out)
	for _, team := range out.Data.Teams.Nodes {
		if strings.EqualFold(team.Key, key) {
			return linearTeam{ID: team.ID, Key: team.Key, States: team.States.Nodes}
		}
	}
	t.Fatalf("Linear team %q not found", key)
	return linearTeam{}
}

func (team linearTeam) stateID(t *testing.T, name string) string {
	t.Helper()
	for _, state := range team.States {
		if strings.EqualFold(state.Name, name) {
			return state.ID
		}
	}
	t.Fatalf("Linear team %q has no state named %q", team.Key, name)
	return ""
}

func (team linearTeam) initialStateID(t *testing.T, requested, trigger string) string {
	t.Helper()
	if strings.TrimSpace(requested) != "" {
		return team.stateID(t, requested)
	}
	for _, state := range team.States {
		if !strings.EqualFold(state.Name, trigger) && strings.EqualFold(state.Type, "unstarted") {
			return state.ID
		}
	}
	for _, state := range team.States {
		if !strings.EqualFold(state.Name, trigger) {
			return state.ID
		}
	}
	t.Fatalf("Linear team %q needs at least one state other than trigger state %q", team.Key, trigger)
	return ""
}

func (c linearClient) createWebhook(ctx context.Context, t *testing.T, url, teamID, secret string) string {
	t.Helper()
	var out struct {
		Data struct {
			WebhookCreate struct {
				Success bool `json:"success"`
				Webhook struct {
					ID string `json:"id"`
				} `json:"webhook"`
			} `json:"webhookCreate"`
		} `json:"data"`
	}
	vars := map[string]interface{}{
		"input": map[string]interface{}{
			"url":           url,
			"teamId":        teamID,
			"secret":        secret,
			"resourceTypes": []string{"Issue"},
		},
	}
	c.graphql(ctx, t, `mutation($input: WebhookCreateInput!) {
  webhookCreate(input: $input) {
    success
    webhook { id }
  }
}`, vars, &out)
	if !out.Data.WebhookCreate.Success || out.Data.WebhookCreate.Webhook.ID == "" {
		t.Fatalf("Linear webhookCreate did not return a webhook id")
	}
	return out.Data.WebhookCreate.Webhook.ID
}

func (c linearClient) deleteE2EWebhooks(ctx context.Context, t *testing.T) {
	t.Helper()
	var out struct {
		Data struct {
			Webhooks struct {
				Nodes []struct {
					ID  string `json:"id"`
					URL string `json:"url"`
				} `json:"nodes"`
			} `json:"webhooks"`
		} `json:"data"`
	}
	c.graphql(ctx, t, `query {
  webhooks {
    nodes { id url }
  }
}`, nil, &out)
	for _, webhook := range out.Data.Webhooks.Nodes {
		if strings.Contains(webhook.URL, "/api/workspaces/e2e-linear-") && strings.HasSuffix(webhook.URL, "/webhooks/linear") {
			if err := c.deleteWebhook(ctx, webhook.ID); err != nil {
				t.Fatalf("delete stale Linear E2E webhook %s: %v", webhook.ID, err)
			}
		}
	}
}

func (c linearClient) deleteWebhook(ctx context.Context, id string) error {
	var out map[string]interface{}
	return c.graphqlNoFatal(ctx, `mutation($id: String!) {
  webhookDelete(id: $id) { success }
}`, map[string]interface{}{"id": id}, &out)
}

func (c linearClient) createIssue(ctx context.Context, t *testing.T, teamID, stateID, title, description string) linearIssue {
	t.Helper()
	var out struct {
		Data struct {
			IssueCreate struct {
				Success bool `json:"success"`
				Issue   struct {
					ID         string `json:"id"`
					Identifier string `json:"identifier"`
					URL        string `json:"url"`
				} `json:"issue"`
			} `json:"issueCreate"`
		} `json:"data"`
	}
	vars := map[string]interface{}{
		"input": map[string]interface{}{
			"teamId":      teamID,
			"stateId":     stateID,
			"title":       title,
			"description": description,
		},
	}
	c.graphql(ctx, t, `mutation($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue { id identifier url }
  }
}`, vars, &out)
	if !out.Data.IssueCreate.Success || out.Data.IssueCreate.Issue.ID == "" {
		t.Fatalf("Linear issueCreate did not return an issue")
	}
	return linearIssue{
		ID:         out.Data.IssueCreate.Issue.ID,
		Identifier: out.Data.IssueCreate.Issue.Identifier,
		URL:        out.Data.IssueCreate.Issue.URL,
	}
}

func (c linearClient) updateIssueState(ctx context.Context, t *testing.T, issueID, stateID string) {
	t.Helper()
	var out struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	c.graphql(ctx, t, `mutation($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) {
    success
  }
}`, map[string]interface{}{"id": issueID, "input": map[string]interface{}{"stateId": stateID}}, &out)
	if !out.Data.IssueUpdate.Success {
		t.Fatalf("Linear issueUpdate did not report success")
	}
}

func (c linearClient) archiveIssue(ctx context.Context, id string) error {
	var out map[string]interface{}
	return c.graphqlNoFatal(ctx, `mutation($id: String!) {
  issueArchive(id: $id) { success }
}`, map[string]interface{}{"id": id}, &out)
}

func (c linearClient) graphql(ctx context.Context, t *testing.T, query string, variables map[string]interface{}, out interface{}) {
	t.Helper()
	if err := c.graphqlNoFatal(ctx, query, variables, out); err != nil {
		t.Fatalf("Linear GraphQL request failed: %v", err)
	}
}

func (c linearClient) graphqlNoFatal(ctx context.Context, query string, variables map[string]interface{}, out interface{}) error {
	reqBody := map[string]interface{}{"query": query}
	if variables != nil {
		reqBody["variables"] = variables
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.linear.app/graphql", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("linear API %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return fmt.Errorf("decode Linear response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		var messages []string
		for _, graphErr := range envelope.Errors {
			messages = append(messages, graphErr.Message)
		}
		return fmt.Errorf("linear GraphQL errors: %s", strings.Join(messages, "; "))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode Linear data: %w", err)
		}
	}
	return nil
}
