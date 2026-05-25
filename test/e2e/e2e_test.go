//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	daytonaProvider "github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	_ "modernc.org/sqlite"
)

const (
	userToken      = "e2e-user-token"
	agentToken     = "e2e-agent-token"
	defaultModel   = "fireworks/accounts/fireworks/models/kimi-k2p6"
	defaultFixture = "elasticclaw/e2e-fixtures"
	daytonaPrefix  = "ec-e2e-"
	maxRunIDLen    = 32
)

func TestDaytonaGitHubIssuesWorkflowE2E(t *testing.T) {
	runID := e2eRunID()
	env := e2eEnv{
		Bin:                 requiredEnv(t, "ELASTICCLAW_E2E_BIN"),
		HubAddr:             envOrDefault("ELASTICCLAW_E2E_HUB_ADDR", "127.0.0.1:8080"),
		PublicURL:           strings.TrimRight(requiredEnv(t, "ELASTICCLAW_E2E_PUBLIC_URL"), "/"),
		GitHubToken:         requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_TOKEN"),
		GitHubRepo:          envOrDefault("ELASTICCLAW_E2E_GITHUB_REPO", defaultFixture),
		GitHubAppID:         requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_APP_ID"),
		GitHubAppURL:        os.Getenv("ELASTICCLAW_E2E_GITHUB_APP_URL"),
		GitHubInstallation:  os.Getenv("ELASTICCLAW_E2E_GITHUB_APP_INSTALLATION"),
		GitHubAppPrivateKey: requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_APP_PRIVATE_KEY"),
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

	workspaceName := "e2e-" + env.RunID
	workflowName := "github-issues-" + env.RunID
	labelName := "agent-ready-" + env.RunID
	webhookSecret := "github-issues-secret-" + env.RunID

	cleanupDaytonaE2ESandboxes(ctx, t, env)
	hub := startHub(ctx, t, env)
	root := writeWorkspaceFixture(t, env, workspaceName, workflowName, labelName)
	keyPath := writeGitHubAppPrivateKey(t, root, env.GitHubAppPrivateKey)

	gh := githubClient{token: env.GitHubToken, repo: env.GitHubRepo}
	var hookID int64
	var issueNumber int
	var agentID string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cleanupCancel()
		if agentID != "" {
			providerID := hub.agentProviderID(cleanupCtx, t, agentID)
			_ = hub.deleteAgent(cleanupCtx, agentID)
			if providerID != "" {
				destroyDaytonaSandboxByID(cleanupCtx, t, env, providerID)
			}
		}
		cleanupDaytonaE2ESandboxes(cleanupCtx, t, env)
		if issueNumber != 0 {
			_ = gh.closeIssue(cleanupCtx, issueNumber)
		}
		_ = gh.deleteLabel(cleanupCtx, labelName)
		if hookID != 0 {
			_ = gh.deleteHook(cleanupCtx, hookID)
		}
		_ = hub.deleteWorkspace(cleanupCtx, workspaceName)
	})

	gh.deleteE2EHooks(ctx, t)
	gh.cleanupE2EIssuesAndLabels(ctx, t)
	runCLI(ctx, t, root, env, "workspace", "push", workspaceName)
	runCLI(ctx, t, root, env, "github-app", "create", "e2e",
		"--workspace", workspaceName,
		"--app-id", env.GitHubAppID,
		"--url", env.GitHubAppURL,
		"--installation", env.GitHubInstallation,
		"--private-key-file", keyPath,
	)
	hub.putIssueTracker(ctx, t, workspaceName, "github-issues", "default", env.GitHubToken, webhookSecret)
	runCLI(ctx, t, root, env, "workflow", "push", "--workspace", workspaceName, filepath.Join(root, ".elasticclaw", "workflows", "github-issues.yaml"))

	hookID = gh.createHook(ctx, t, env.PublicURL+"/api/workspaces/"+workspaceName+"/webhooks/github-issues", webhookSecret)
	gh.ensureLabel(ctx, t, labelName)
	issueNumber = gh.createIssue(ctx, t, "Tell a dad joke. Do not make a PR.", "Tell a dad joke. Do not make a PR.\n\nElasticClaw E2E run: "+env.RunID)
	gh.addLabel(ctx, t, issueNumber, labelName)

	agentName := env.GitHubRepo + "/" + fmt.Sprint(issueNumber)
	agentID = waitForOneAgent(ctx, t, hub, agentName)
	waitForAgentStatus(ctx, t, hub, agentID, "connected")
	waitForAgentReply(ctx, t, hub, agentID)
	assertNoPullRequestCreated(ctx, t, gh, issueNumber)
}

type e2eEnv struct {
	Bin                 string
	HubAddr             string
	PublicURL           string
	GitHubToken         string
	GitHubRepo          string
	GitHubAppID         string
	GitHubAppURL        string
	GitHubInstallation  string
	GitHubAppPrivateKey string
	DaytonaAPIKey       string
	FireworksAPIKey     string
	BridgeBinary        string
	BridgeToken         string
	DaytonaPrefix       string
	Model               string
	RunID               string
}

type hubProcess struct {
	baseURL string
	token   string
	cmd     *exec.Cmd
	logPath string
	dbPath  string
}

func startHub(ctx context.Context, t *testing.T, env e2eEnv) *hubProcess {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "hub.yaml")
	dbPath := filepath.Join(dir, "hub.db")
	logPath := filepath.Join(dir, "hub.log")
	baseURL := "http://" + env.HubAddr

	config := fmt.Sprintf(`schema_version: v1
url: %s
public_url: %s
token: %s
claw_token: %s
bridge_image: %s
providers:
  daytona:
    type: daytona
    api_key: %q
default_model: %s
llm_keys:
  - name: fireworks
    provider: fireworks
    api_key: %q
    default: true
    default_model: %s
`, baseURL, env.PublicURL, userToken, agentToken, env.PublicURL+"/__elasticclaw_e2e/claw-bridge-linux-amd64?token="+url.QueryEscape(env.BridgeToken), env.DaytonaAPIKey, env.Model, env.FireworksAPIKey, env.Model)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatalf("write hub config: %v", err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create hub log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cmd := exec.CommandContext(ctx, env.Bin, "hub", "--addr", env.HubAddr, "--db", dbPath, "--no-web-ui")
	cmd.Env = append(os.Environ(),
		"ELASTICCLAW_HUB_CONFIG="+configPath,
		"DAYTONA_API_KEY="+env.DaytonaAPIKey,
		"FIREWORKS_API_KEY="+env.FireworksAPIKey,
		"ELASTICCLAW_E2E_BRIDGE_BINARY="+env.BridgeBinary,
		"ELASTICCLAW_E2E_BRIDGE_TOKEN="+env.BridgeToken,
		"ELASTICCLAW_PROVIDER_NAME_PREFIX="+env.DaytonaPrefix,
		"HOME="+dir,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hub: %v", err)
	}
	hub := &hubProcess{baseURL: baseURL, token: userToken, cmd: cmd, logPath: logPath, dbPath: dbPath}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			if data, err := os.ReadFile(logPath); err == nil {
				t.Logf("hub log:\n%s", string(data))
			}
		}
	})
	waitForHub(ctx, t, hub)
	return hub
}

func waitForHub(ctx context.Context, t *testing.T, hub *hubProcess) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hub.baseURL+"/api/claws", nil)
		req.Header.Set("Authorization", "Bearer "+hub.token)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("hub did not become ready at %s", hub.baseURL)
}

func writeWorkspaceFixture(t *testing.T, env e2eEnv, workspaceName, workflowName, labelName string) string {
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
	writeFile(t, filepath.Join(workspaceDir, "CONTEXT.md"), "This is an ElasticClaw E2E test. Follow the GitHub issue exactly.\n")
	writeFile(t, filepath.Join(workflowDir, "github-issues.yaml"), fmt.Sprintf(`schema_version: v1
name: %s

trigger:
  github_issues:
    event: issue_labeled
    repositories:
      - %s
    states:
      - open
    labels:
      - %s
    labelers:
      - "*"

concurrency_group: %s

stages:
  - id: working
    label: Working
    entry: true
    on_enter:
      remove_labels:
        - %s
      add_labels:
        - agent-working-%s
      inject: |
        Issue: {{.Issue.Identifier}} - {{.Issue.Title}}
        URL: {{.Issue.URL}}

        Do exactly what this issue asks.
        Do not create a pull request.
`, workflowName, env.GitHubRepo, labelName, "e2e-"+env.RunID, labelName, env.RunID))
	return root
}

func writeGitHubAppPrivateKey(t *testing.T, root, privateKey string) string {
	t.Helper()
	path := filepath.Join(root, "github-app.pem")
	writeFile(t, path, privateKey)
	return path
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func runCLI(ctx context.Context, t *testing.T, workdir string, env e2eEnv, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, env.Bin, args...)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(),
		"ELASTICCLAW_HUB_URL=http://"+env.HubAddr,
		"ELASTICCLAW_CLAW_TOKEN="+userToken,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("elasticclaw %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
}

func (h *hubProcess) putIssueTracker(ctx context.Context, t *testing.T, workspaceName, trackerType, name, token, webhookSecret string) {
	t.Helper()
	body := map[string]string{
		"type":          trackerType,
		"workspace":     name,
		"token":         token,
		"webhookSecret": webhookSecret,
	}
	h.api(ctx, t, http.MethodPut, "/api/workspaces/"+workspaceName+"/issue-trackers", body, nil)
}

func (h *hubProcess) deleteWorkspace(ctx context.Context, workspaceName string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, h.baseURL+"/api/workspaces?name="+workspaceName, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete workspace: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (h *hubProcess) deleteAgent(ctx context.Context, agentID string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, h.baseURL+"/api/claws/"+agentID, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete agent: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (h *hubProcess) agentProviderID(ctx context.Context, t *testing.T, agentID string) string {
	t.Helper()
	db, err := sql.Open("sqlite", h.dbPath+"?_time_format=sqlite&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open E2E hub db for provider cleanup: %v", err)
	}
	defer db.Close()
	var provider, providerID string
	err = db.QueryRowContext(ctx, `SELECT COALESCE(provider,''), COALESCE(provider_id,'') FROM claws WHERE id = ?`, agentID).Scan(&provider, &providerID)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read E2E agent provider id: %v", err)
	}
	if provider != "daytona" {
		return ""
	}
	return providerID
}

func (h *hubProcess) listAgents(ctx context.Context, t *testing.T) []types.Claw {
	t.Helper()
	var agents []types.Claw
	h.api(ctx, t, http.MethodGet, "/api/claws", nil, &agents)
	return agents
}

func (h *hubProcess) listMessages(ctx context.Context, t *testing.T, agentID string) []types.HubMessage {
	t.Helper()
	var messages []types.HubMessage
	h.api(ctx, t, http.MethodGet, "/api/messages/"+agentID, nil, &messages)
	return messages
}

func (h *hubProcess) api(ctx context.Context, t *testing.T, method, path string, body interface{}, out interface{}) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s returned %s: %s", method, path, resp.Status, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			t.Fatalf("decode %s %s: %v\n%s", method, path, err, string(respBody))
		}
	}
}

func waitForOneAgent(ctx context.Context, t *testing.T, hub *hubProcess, name string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		agents := hub.listAgents(ctx, t)
		count := 0
		var id string
		for _, agent := range agents {
			if agent.Name == name {
				count++
				id = agent.ID
			}
		}
		if count == 1 {
			return id
		}
		if count > 1 {
			t.Fatalf("found %d agents named %s, want exactly 1", count, name)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for one agent named %s", name)
	return ""
}

func waitForAgentStatus(ctx context.Context, t *testing.T, hub *hubProcess, agentID, want string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Minute)
	for time.Now().Before(deadline) {
		var agent types.Claw
		hub.api(ctx, t, http.MethodGet, "/api/claws/"+agentID, nil, &agent)
		if string(agent.Status) == want {
			return
		}
		if agent.Status == "error" {
			t.Fatalf("agent %s entered error state", agentID)
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("timed out waiting for agent %s status %q", agentID, want)
}

func waitForAgentReply(ctx context.Context, t *testing.T, hub *hubProcess, agentID string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		for _, msg := range hub.listMessages(ctx, t, agentID) {
			if msg.Role == "claw" && strings.TrimSpace(msg.Content) != "" {
				return
			}
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("timed out waiting for agent %s to reply", agentID)
}

func cleanupDaytonaE2ESandboxes(ctx context.Context, t *testing.T, env e2eEnv) {
	t.Helper()
	provider, err := daytonaProvider.New(map[string]interface{}{"api_key": env.DaytonaAPIKey})
	if err != nil {
		t.Fatalf("create Daytona provider for E2E cleanup: %v", err)
	}

	deleteMatching := func() (int, error) {
		instances, err := provider.List(ctx)
		if err != nil {
			return 0, err
		}
		matching := 0
		for _, instance := range instances {
			if !strings.HasPrefix(instance.Name, env.DaytonaPrefix) {
				continue
			}
			matching++
			if err := provider.Destroy(ctx, instance.ID, false); err != nil {
				if !isBenignDaytonaDeleteError(err) {
					return matching, fmt.Errorf("delete Daytona E2E sandbox %s (%s): %w", instance.Name, instance.ID, err)
				}
			}
		}
		return matching, nil
	}

	deadline := time.Now().Add(3 * time.Minute)
	for {
		matching, err := deleteMatching()
		if err != nil {
			t.Fatalf("cleanup Daytona E2E sandboxes: %v", err)
		}
		if matching == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Daytona E2E sandboxes with prefix %q to terminate", env.DaytonaPrefix)
		}
		time.Sleep(5 * time.Second)
	}
}

func destroyDaytonaSandboxByID(ctx context.Context, t *testing.T, env e2eEnv, sandboxID string) {
	t.Helper()
	provider, err := daytonaProvider.New(map[string]interface{}{"api_key": env.DaytonaAPIKey})
	if err != nil {
		t.Fatalf("create Daytona provider for E2E sandbox cleanup: %v", err)
	}
	if err := provider.Destroy(ctx, sandboxID, false); err != nil && !isBenignDaytonaDeleteError(err) {
		t.Fatalf("delete Daytona E2E sandbox %s: %v", sandboxID, err)
	}

	deadline := time.Now().Add(3 * time.Minute)
	for {
		status, err := provider.Status(ctx, sandboxID)
		if err != nil {
			if isBenignDaytonaDeleteError(err) {
				return
			}
			t.Fatalf("check Daytona E2E sandbox %s deletion: %v", sandboxID, err)
		}
		if status == types.StatusNotFound {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Daytona E2E sandbox %s to terminate; status=%s", sandboxID, status)
		}
		time.Sleep(5 * time.Second)
	}
}

func isBenignDaytonaDeleteError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "destroy") ||
		strings.Contains(msg, "delet") ||
		strings.Contains(msg, "terminat")
}

type githubClient struct {
	token string
	repo  string
}

func (g githubClient) createHook(ctx context.Context, t *testing.T, url, secret string) int64 {
	t.Helper()
	body := map[string]interface{}{
		"name":   "web",
		"active": true,
		"events": []string{"issues"},
		"config": map[string]string{
			"url":          url,
			"content_type": "json",
			"secret":       secret,
			"insecure_ssl": "0",
		},
	}
	var out struct {
		ID int64 `json:"id"`
	}
	g.api(ctx, t, http.MethodPost, "hooks", body, &out)
	return out.ID
}

func (g githubClient) deleteE2EHooks(ctx context.Context, t *testing.T) {
	t.Helper()
	var hooks []struct {
		ID     int64 `json:"id"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	g.api(ctx, t, http.MethodGet, "hooks", nil, &hooks)
	for _, hook := range hooks {
		if strings.Contains(hook.Config.URL, "/api/workspaces/") && strings.HasSuffix(hook.Config.URL, "/webhooks/github-issues") {
			if err := g.deleteHook(ctx, hook.ID); err != nil {
				t.Fatalf("delete orphaned E2E hook %d: %v", hook.ID, err)
			}
		}
	}
}

func (g githubClient) deleteHook(ctx context.Context, hookID int64) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("https://api.github.com/repos/%s/hooks/%d", g.repo, hookID), nil)
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete hook: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (g githubClient) cleanupE2EIssuesAndLabels(ctx context.Context, t *testing.T) {
	t.Helper()
	g.closeE2EIssues(ctx, t)
	g.deleteE2ELabels(ctx, t)
}

func (g githubClient) closeE2EIssues(ctx context.Context, t *testing.T) {
	t.Helper()
	var issues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	g.api(ctx, t, http.MethodGet, "issues?state=open&per_page=100", nil, &issues)
	for _, issue := range issues {
		if !isE2EIssue(issue.Title, issue.Body, issue.Labels) {
			continue
		}
		if err := g.closeIssue(ctx, issue.Number); err != nil {
			t.Fatalf("close orphaned E2E issue %d: %v", issue.Number, err)
		}
	}
}

func isE2EIssue(title, body string, labels []struct {
	Name string `json:"name"`
}) bool {
	if strings.EqualFold(strings.TrimSpace(title), "Tell a dad joke. Do not make a PR.") {
		return true
	}
	if strings.Contains(body, "ElasticClaw E2E") {
		return true
	}
	for _, label := range labels {
		if strings.HasPrefix(label.Name, "agent-ready-") {
			return true
		}
	}
	return false
}

func (g githubClient) deleteE2ELabels(ctx context.Context, t *testing.T) {
	t.Helper()
	var labels []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	g.api(ctx, t, http.MethodGet, "labels?per_page=100", nil, &labels)
	for _, label := range labels {
		if !strings.HasPrefix(label.Name, "agent-ready-") && label.Description != "ElasticClaw E2E trigger label" {
			continue
		}
		if err := g.deleteLabel(ctx, label.Name); err != nil {
			t.Fatalf("delete orphaned E2E label %q: %v", label.Name, err)
		}
	}
}

func (g githubClient) ensureLabel(ctx context.Context, t *testing.T, label string) {
	t.Helper()
	body := map[string]string{"name": label, "color": "0e8a16", "description": "ElasticClaw E2E trigger label"}
	var out map[string]interface{}
	g.api(ctx, t, http.MethodPost, "labels", body, &out)
}

func (g githubClient) deleteLabel(ctx context.Context, label string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("https://api.github.com/repos/%s/labels/%s", g.repo, url.PathEscape(label)), nil)
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete label: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (g githubClient) createIssue(ctx context.Context, t *testing.T, title, bodyText string) int {
	t.Helper()
	body := map[string]string{"title": title, "body": bodyText}
	var out struct {
		Number int `json:"number"`
	}
	g.api(ctx, t, http.MethodPost, "issues", body, &out)
	return out.Number
}

func (g githubClient) addLabel(ctx context.Context, t *testing.T, issueNumber int, label string) {
	t.Helper()
	body := map[string][]string{"labels": []string{label}}
	var out interface{}
	g.api(ctx, t, http.MethodPost, fmt.Sprintf("issues/%d/labels", issueNumber), body, &out)
}

func (g githubClient) closeIssue(ctx context.Context, issueNumber int) error {
	data, _ := json.Marshal(map[string]string{"state": "closed"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPatch, fmt.Sprintf("https://api.github.com/repos/%s/issues/%d", g.repo, issueNumber), bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("close issue: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func assertNoPullRequestCreated(ctx context.Context, t *testing.T, gh githubClient, issueNumber int) {
	t.Helper()
	var events []struct {
		Event string `json:"event"`
	}
	gh.api(ctx, t, http.MethodGet, fmt.Sprintf("issues/%d/timeline", issueNumber), nil, &events)
	for _, event := range events {
		if event.Event == "cross-referenced" {
			t.Fatalf("issue %d has a cross-reference event; agent may have opened a PR despite instructions", issueNumber)
		}
	}
}

func (g githubClient) api(ctx context.Context, t *testing.T, method, path string, body interface{}, out interface{}) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal github %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com/repos/"+g.repo+"/"+path, reader)
	if err != nil {
		t.Fatalf("build github %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("github %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		if method == http.MethodPost && path == "labels" && resp.StatusCode == http.StatusUnprocessableEntity {
			return
		}
		t.Fatalf("github %s %s returned %s: %s", method, path, resp.Status, strings.TrimSpace(string(respBody)))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			t.Fatalf("decode github %s %s: %v\n%s", method, path, err, string(respBody))
		}
	}
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("missing required env: %s", name)
	}
	return value
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func e2eRunID() string {
	if value := strings.TrimSpace(os.Getenv("ELASTICCLAW_E2E_RUN_ID")); value != "" {
		return sanitizeID(value)
	}
	return sanitizeID(fmt.Sprintf("%d", time.Now().UnixNano()))
}

func sanitizeID(value string) string {
	value = strings.ToLower(value)
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "run"
	}
	if len(out) > maxRunIDLen {
		out = out[:maxRunIDLen]
	}
	return strings.Trim(out, "-")
}
