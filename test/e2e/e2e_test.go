package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	userToken  = "e2e-user-token"
	agentToken = "e2e-agent-token"
)

func TestE2EEnvironmentContract(t *testing.T) {
	cfg := githubIssuesWorkflow("elasticclaw/e2e-fixtures", "agent-ready")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("github issues workflow fixture is invalid: %v", err)
	}
	if os.Getenv("ELASTICCLAW_E2E") != "1" {
		t.Skip("set ELASTICCLAW_E2E=1 to run external E2E tests")
	}
	requiredEnv(t,
		"ELASTICCLAW_E2E_BIN",
		"ELASTICCLAW_E2E_PUBLIC_URL",
		"ELASTICCLAW_E2E_GITHUB_TOKEN",
		"ELASTICCLAW_E2E_GITHUB_REPO",
	)
}

func TestGitHubIssuesWebhookNoop(t *testing.T) {
	if os.Getenv("ELASTICCLAW_E2E") != "1" {
		t.Skip("set ELASTICCLAW_E2E=1 to run external E2E tests")
	}

	bin := requiredEnv(t, "ELASTICCLAW_E2E_BIN")
	publicURL := strings.TrimRight(requiredEnv(t, "ELASTICCLAW_E2E_PUBLIC_URL"), "/")
	githubToken := requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_TOKEN")
	repo := requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_REPO")

	runID := e2eRunID()
	workspaceName := "e2e-" + runID
	workflowName := "github-issues-" + runID
	labelName := "agent-ready-" + runID
	webhookSecret := "github-issues-secret-" + runID

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	hub := startHub(ctx, t, bin, publicURL)
	gh := githubClient{token: githubToken, repo: repo}

	var hookID int64
	var issueNumber int
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if issueNumber != 0 {
			_ = gh.closeIssue(cleanupCtx, issueNumber)
		}
		if hookID != 0 {
			_ = gh.deleteHook(cleanupCtx, hookID)
		}
		_ = hub.deleteWorkspace(cleanupCtx, workspaceName)
	})

	hub.pushWorkspace(ctx, t, workspaceName, repo)
	hub.pushWorkflow(ctx, t, workspaceName, githubIssuesWorkflowForRun(workflowName, repo, labelName, runID))
	hub.putIssueTracker(ctx, t, workspaceName, "github-issues", "default", githubToken, webhookSecret)

	hookID = gh.createHook(ctx, t, publicURL+"/api/workspaces/"+workspaceName+"/webhooks/github-issues", webhookSecret)
	gh.ensureLabel(ctx, t, labelName)
	issueNumber = gh.createIssue(ctx, t, "ElasticClaw E2E "+runID, "Created by ElasticClaw E2E run "+runID)
	gh.addLabel(ctx, t, issueNumber, labelName)

	wantName := repo + "/" + fmt.Sprint(issueNumber)
	waitForExactlyOneAgent(ctx, t, hub, wantName)
}

type hubProcess struct {
	baseURL string
	token   string
	cmd     *exec.Cmd
	logPath string
}

func startHub(ctx context.Context, t *testing.T, bin, publicURL string) *hubProcess {
	t.Helper()
	addr := strings.TrimSpace(os.Getenv("ELASTICCLAW_E2E_HUB_ADDR"))
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	}
	baseURL := "http://" + addr
	dir := t.TempDir()
	configPath := filepath.Join(dir, "hub.yaml")
	dbPath := filepath.Join(dir, "hub.db")
	logPath := filepath.Join(dir, "hub.log")

	config := fmt.Sprintf(`schema_version: v1
url: %s
public_url: %s
token: %s
claw_token: %s
providers:
  noop:
    type: noop
default_model: test/noop
`, baseURL, publicURL, userToken, agentToken)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatalf("write hub config: %v", err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create hub log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cmd := exec.CommandContext(ctx, bin, "hub", "--addr", addr, "--db", dbPath, "--no-web-ui")
	cmd.Env = append(os.Environ(),
		"ELASTICCLAW_HUB_CONFIG="+configPath,
		"ELASTICCLAW_NOOP_PROVIDER=1",
		"HOME="+dir,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hub: %v", err)
	}
	hub := &hubProcess{baseURL: baseURL, token: userToken, cmd: cmd, logPath: logPath}
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
	deadline := time.Now().Add(20 * time.Second)
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

func (h *hubProcess) pushWorkspace(ctx context.Context, t *testing.T, workspaceName, repo string) {
	t.Helper()
	body := map[string]interface{}{
		"workspaces": []map[string]interface{}{{
			"schemaVersion": "v1",
			"name":          workspaceName,
			"repositories": []map[string]string{{
				"repo":        repo,
				"permissions": "write",
			}},
			"files": map[string]string{
				"elasticclaw-config.yaml": fmt.Sprintf("schema_version: v1\nname: %s\nprovider: noop\nrepositories:\n  - repo: %s\n    permissions: write\n", workspaceName, repo),
				"AGENTS.md":               "You are an ElasticClaw E2E test agent.\n",
				"TOOLS.md":                "Use the available tools conservatively.\n",
				"CONTEXT.md":              "This is an automated E2E test workspace.\n",
			},
		}},
	}
	h.api(ctx, t, http.MethodPost, "/api/workspaces", body, nil)
}

func (h *hubProcess) pushWorkflow(ctx context.Context, t *testing.T, workspaceName string, workflow *types.WorkflowConfig) {
	t.Helper()
	body := map[string]interface{}{"workflows": []*types.WorkflowConfig{workflow}}
	h.api(ctx, t, http.MethodPost, "/api/workspaces/"+workspaceName+"/workflows", body, nil)
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

func (h *hubProcess) listAgents(ctx context.Context, t *testing.T) []types.Claw {
	t.Helper()
	var claws []types.Claw
	h.api(ctx, t, http.MethodGet, "/api/claws", nil, &claws)
	return claws
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

func waitForExactlyOneAgent(ctx context.Context, t *testing.T, hub *hubProcess, name string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		claws := hub.listAgents(ctx, t)
		count := 0
		for _, claw := range claws {
			if claw.Name == name {
				count++
			}
		}
		if count == 1 {
			return
		}
		if count > 1 {
			t.Fatalf("found %d agents named %s, want exactly 1", count, name)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for one agent named %s", name)
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

func (g githubClient) ensureLabel(ctx context.Context, t *testing.T, label string) {
	t.Helper()
	body := map[string]string{"name": label, "color": "0e8a16", "description": "ElasticClaw E2E trigger label"}
	var out map[string]interface{}
	g.api(ctx, t, http.MethodPost, "labels", body, &out)
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

func githubIssuesWorkflow(repo, label string) *types.WorkflowConfig {
	return githubIssuesWorkflowForRun("github-issues", repo, label, "contract")
}

func githubIssuesWorkflowForRun(name, repo, label, runID string) *types.WorkflowConfig {
	return &types.WorkflowConfig{
		SchemaVersion:    "v1",
		Name:             name,
		Integration:      "github-issues",
		ConcurrencyGroup: "e2e-" + runID,
		Trigger: &types.WorkflowTrigger{
			GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
				Event:        "issue_labeled",
				Repositories: []string{repo},
				States:       []string{"open"},
				Labels:       []string{label},
				Labelers:     []string{"*"},
			},
		},
		Stages: []types.WorkflowStage{{
			ID:    "working",
			Label: "Working",
			Entry: true,
			OnEnter: map[string]interface{}{
				"inject": "E2E issue {{.Issue.Identifier}} {{.Issue.URL}}\n",
			},
		}},
	}
}

func requiredEnv(t *testing.T, names ...string) string {
	t.Helper()
	var first string
	var missing []string
	for _, name := range names {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			missing = append(missing, name)
			continue
		}
		if first == "" {
			first = value
		}
	}
	if len(missing) > 0 {
		t.Fatalf("missing required env: %s", strings.Join(missing, ", "))
	}
	return first
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
	if len(out) > 48 {
		out = out[:48]
	}
	return strings.Trim(out, "-")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
