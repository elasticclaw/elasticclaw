//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
)

// The Linear E2E runs against an in-process fake Linear API: the hub reads
// issues from factorytest.MockLinear and the test delivers a signed
// status_changed webhook itself. Everything else (sandbox, bridge, agent,
// model) stays real. A real Linear workspace made CI depend on its issue
// quota and on cleanup of issues, labels and webhooks from cancelled runs.
const (
	linearE2ETeamKey      = "E2E"
	linearE2EInitialState = "Backlog"
	linearE2ETriggerState = "Todo"
)

func TestDaytonaLinearWorkflowE2E(t *testing.T) {
	runLinearWorkflowE2E(t, "daytona")
}

func TestReplicatedLinearWorkflowE2E(t *testing.T) {
	runLinearWorkflowE2E(t, "replicated")
}

func runLinearWorkflowE2E(t *testing.T, sandboxProvider string) {
	runID := e2eRunID()
	env := newE2EEnv(t, runID, sandboxProvider)
	linear := factorytest.NewMockLinear(t)
	env.LinearAPIURL = linear.URL

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	workspaceName := "e2e-linear-" + env.RunID
	workflowName := "linear-" + env.RunID
	labelName := "elasticclaw-e2e-" + env.RunID
	webhookSecret := "linear-e2e-secret-" + env.RunID
	issueIdentifier := linearE2ETeamKey + "-1"
	issueTitle := "Tell a dad joke. Do not make a PR."
	issueDescription := issueTitle + "\n\nElasticClaw E2E run: " + env.RunID

	// Seed before the hub starts so its poller never races this map write.
	issue := linearE2EIssue(issueIdentifier, issueTitle, issueDescription, labelName, linearE2ETriggerState)
	linear.PollingIssues[issueIdentifier] = issue

	cleanupProvider(ctx, t, env)
	hub := startHub(ctx, t, env)
	root := writeLinearWorkspaceFixture(t, env, workspaceName, workflowName, labelName)
	keyPath := writeGitHubAppPrivateKey(t, root, env.GitHubAppPrivateKey)

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
	hub.putIssueTracker(ctx, t, workspaceName, "linear", "default", "lin_api_e2e_fake_"+env.RunID, webhookSecret)
	runCLI(ctx, t, root, env, "workflow", "push", "--workspace", workspaceName, filepath.Join(root, ".elasticclaw", "workflows", "linear.yaml"))

	postLinearWebhook(ctx, t, env.PublicURL+"/api/workspaces/"+workspaceName+"/webhooks/linear", webhookSecret, linearE2EWebhookPayload(issue, labelName, env.RunID))

	agentID = waitForOneAgent(ctx, t, hub, issueIdentifier)
	waitForAgentStatus(ctx, t, hub, agentID, "connected")
	waitForAgentReply(ctx, t, hub, agentID)
	if !linear.SawAPICall() {
		t.Fatalf("hub never called the fake Linear API at %s", linear.URL)
	}
}

func writeLinearWorkspaceFixture(t *testing.T, env e2eEnv, workspaceName, workflowName, labelName string) string {
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
	writeFile(t, filepath.Join(workspaceDir, "CONTEXT.md"), "This is an ElasticClaw Linear E2E test. Follow the Linear issue exactly.\n")
	writeFile(t, filepath.Join(workflowDir, "linear.yaml"), fmt.Sprintf(`schema_version: v1
name: %s

trigger:
  linear:
    event: status_changed
    team: %s
    states:
      - %s
    labels:
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
`, workflowName, linearE2ETeamKey, linearE2ETriggerState, labelName, env.RunID))
	return root
}

// linearE2EIssue is the issue the fake Linear API serves to the hub.
func linearE2EIssue(identifier, title, description, labelName, state string) map[string]interface{} {
	return map[string]interface{}{
		"id":          "linear-e2e-" + strings.ToLower(identifier),
		"identifier":  identifier,
		"title":       title,
		"description": description,
		"url":         "https://linear.app/elasticclaw-e2e/issue/" + identifier,
		"updatedAt":   time.Now().UTC().Format(time.RFC3339),
		"state":       map[string]interface{}{"name": state},
		"team":        map[string]interface{}{"name": "ElasticClaw E2E", "key": linearE2ETeamKey},
		"labels":      map[string]interface{}{"nodes": []map[string]interface{}{{"name": labelName}}},
		"assignee":    nil,
	}
}

// linearE2EWebhookPayload mirrors the Issue update Linear sends when an issue
// moves from the initial state into the trigger state.
func linearE2EWebhookPayload(issue map[string]interface{}, labelName, runID string) map[string]interface{} {
	return map[string]interface{}{
		"type":      "Issue",
		"action":    "update",
		"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
		"webhookId": "linear-e2e-webhook-" + runID,
		"data": map[string]interface{}{
			"id":          issue["id"],
			"identifier":  issue["identifier"],
			"title":       issue["title"],
			"description": issue["description"],
			"url":         issue["url"],
			"state":       issue["state"],
			"team":        issue["team"],
			"labels":      []map[string]interface{}{{"name": labelName}},
		},
		"updatedFrom": map[string]interface{}{
			"state": map[string]interface{}{"name": linearE2EInitialState},
		},
	}
}

func postLinearWebhook(ctx context.Context, t *testing.T, webhookURL, secret string, payload map[string]interface{}) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal Linear webhook payload: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build Linear webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Linear-Signature", hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post Linear webhook: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("Linear webhook returned %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
}
