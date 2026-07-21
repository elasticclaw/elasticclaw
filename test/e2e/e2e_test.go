//go:build e2e

package e2e

import (
	"bytes"
	"context"
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
	dockerProvider "github.com/elasticclaw/elasticclaw/pkg/provider/docker"
	replicatedProvider "github.com/elasticclaw/elasticclaw/pkg/provider/replicated"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	userToken      = "e2e-user-token"
	agentToken     = "e2e-agent-token"
	defaultModel   = "fireworks/accounts/fireworks/models/kimi-k2p6"
	defaultFixture = "elasticclaw/e2e-fixtures"
	daytonaPrefix  = "ec-e2e"
	cmxPrefix      = "ec-e2e-cmx"
	dockerPrefix   = "ec-e2e-docker"
	maxRunIDLen    = 32
)

func TestDaytonaGitHubIssuesWorkflowE2E(t *testing.T) {
	runGitHubIssuesWorkflowE2E(t, "daytona")
}

func TestReplicatedGitHubIssuesWorkflowE2E(t *testing.T) {
	runGitHubIssuesWorkflowE2E(t, "replicated")
}

func TestDockerWorkflowE2E(t *testing.T) {
	runID := e2eRunID()
	env := newE2EEnv(t, runID, "docker")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	workspaceName := "e2e-docker-" + env.RunID
	workflowName := "docker-" + env.RunID

	hub := startHub(ctx, t, env)
	root := writeDockerWorkspaceFixture(t, env, workspaceName, workflowName)

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
		_ = hub.deleteWorkspace(cleanupCtx, workspaceName)
	})

	runCLI(ctx, t, root, env, "workspace", "push", workspaceName)
	runCLI(ctx, t, root, env, "workflow", "push", "--workspace", workspaceName, filepath.Join(root, ".elasticclaw", "workflows", "docker.yaml"))

	var trigger struct {
		ClawID string `json:"claw_id"`
		Status string `json:"status"`
	}
	hub.api(ctx, t, http.MethodPost, "/api/workspaces/"+workspaceName+"/workflows/"+workflowName+"/trigger", map[string]any{"inputs": map[string]string{}}, &trigger)
	if trigger.ClawID == "" {
		t.Fatalf("manual workflow trigger returned empty claw_id: %#v", trigger)
	}
	agentID = trigger.ClawID
	waitForAgentStatus(ctx, t, hub, agentID, "connected")

	// Verify that the Docker provider executed the deterministic run action,
	// captured output, and the gate passed (per #530). The injected message
	// references the captured {{ .Outputs.docker_smoke.status }}.
	deadline := time.Now().Add(30 * time.Second)
	foundRun := false
	var lastMsgs []types.HubMessage
	for time.Now().Before(deadline) {
		lastMsgs = hub.listMessages(ctx, t, agentID)
		for _, m := range lastMsgs {
			if strings.Contains(m.Content, "Docker provider run action executed") || strings.Contains(m.Content, "status\":\"passed\"") {
				foundRun = true
				break
			}
		}
		if foundRun {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !foundRun {
		t.Logf("messages seen for docker claw %s: %+v", agentID, lastMsgs)
		t.Fatalf("docker run action + output capture + gate did not produce expected evidence (see #530)")
	}
}

func TestHubListenAddrBindsDockerHubForContainerAccess(t *testing.T) {
	tests := []struct {
		name string
		env  e2eEnv
		want string
	}{
		{
			name: "docker loopback",
			env:  e2eEnv{SandboxProvider: "docker", HubAddr: "127.0.0.1:8080"},
			want: "0.0.0.0:8080",
		},
		{
			name: "docker localhost",
			env:  e2eEnv{SandboxProvider: "docker", HubAddr: "localhost:8080"},
			want: "0.0.0.0:8080",
		},
		{
			name: "non docker unchanged",
			env:  e2eEnv{SandboxProvider: "daytona", HubAddr: "127.0.0.1:8080"},
			want: "127.0.0.1:8080",
		},
		{
			name: "docker remote unchanged",
			env:  e2eEnv{SandboxProvider: "docker", HubAddr: "10.0.0.5:8080"},
			want: "10.0.0.5:8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hubListenAddr(tt.env); got != tt.want {
				t.Fatalf("hubListenAddr() = %q, want %q", got, tt.want)
			}
		})
	}
}

func runGitHubIssuesWorkflowE2E(t *testing.T, sandboxProvider string) {
	runID := e2eRunID()
	env := newE2EEnv(t, runID, sandboxProvider)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	workspaceName := "e2e-" + env.RunID
	workflowName := "github-issues-" + env.RunID
	labelName := "agent-ready-" + env.RunID
	webhookSecret := "github-issues-secret-" + env.RunID

	cleanupProvider(ctx, t, env)
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
		if issueNumber != 0 {
			_ = gh.closeIssue(cleanupCtx, issueNumber)
		}
		_ = gh.deleteLabel(cleanupCtx, labelName)
		if hookID != 0 {
			_ = gh.deleteHook(cleanupCtx, hookID)
		}
		_ = hub.deleteWorkspace(cleanupCtx, workspaceName)
	})

	gh.cleanupGitHubE2EResources(ctx, t, workspaceName, env.RunID, labelName)
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
	SandboxProvider     string
	GitHubToken         string
	GitHubRepo          string
	GitHubAppID         string
	GitHubAppURL        string
	GitHubInstallation  string
	GitHubAppPrivateKey string
	LinearAPIKey        string
	LinearTeamKey       string
	LinearTriggerState  string
	LinearInitialState  string
	JiraBaseURL         string
	JiraUsername        string
	JiraToken           string
	JiraProjectKey      string
	DaytonaAPIKey       string
	ReplicatedToken     string
	ReplicatedAPIURL    string
	ReplicatedType      string
	ReplicatedTTL       string
	DockerImage         string
	FireworksAPIKey     string
	BridgeBinary        string
	BridgeToken         string
	ProviderPrefix      string
	Model               string
	RunID               string
}

func newE2EEnv(t *testing.T, runID, sandboxProvider string) e2eEnv {
	t.Helper()
	env := e2eEnv{
		Bin:                 requiredEnv(t, "ELASTICCLAW_E2E_BIN"),
		HubAddr:             envOrDefault("ELASTICCLAW_E2E_HUB_ADDR", "127.0.0.1:8080"),
		PublicURL:           strings.TrimRight(requiredEnv(t, "ELASTICCLAW_E2E_PUBLIC_URL"), "/"),
		SandboxProvider:     sandboxProvider,
		GitHubToken:         os.Getenv("ELASTICCLAW_E2E_GITHUB_TOKEN"),
		GitHubRepo:          envOrDefault("ELASTICCLAW_E2E_GITHUB_REPO", defaultFixture),
		GitHubAppID:         os.Getenv("ELASTICCLAW_E2E_GITHUB_APP_ID"),
		GitHubAppURL:        os.Getenv("ELASTICCLAW_E2E_GITHUB_APP_URL"),
		GitHubInstallation:  os.Getenv("ELASTICCLAW_E2E_GITHUB_APP_INSTALLATION"),
		GitHubAppPrivateKey: os.Getenv("ELASTICCLAW_E2E_GITHUB_APP_PRIVATE_KEY"),
		LinearAPIKey:        os.Getenv("ELASTICCLAW_E2E_LINEAR_API_KEY"),
		LinearTeamKey:       os.Getenv("ELASTICCLAW_E2E_LINEAR_TEAM_KEY"),
		LinearTriggerState:  envOrDefault("ELASTICCLAW_E2E_LINEAR_TRIGGER_STATE", "Todo"),
		LinearInitialState:  os.Getenv("ELASTICCLAW_E2E_LINEAR_INITIAL_STATE"),
		JiraBaseURL:         strings.TrimRight(os.Getenv("ELASTICCLAW_E2E_JIRA_BASE_URL"), "/"),
		JiraUsername:        os.Getenv("ELASTICCLAW_E2E_JIRA_USERNAME"),
		JiraToken:           os.Getenv("ELASTICCLAW_E2E_JIRA_TOKEN"),
		JiraProjectKey:      os.Getenv("ELASTICCLAW_E2E_JIRA_PROJECT_KEY"),
		FireworksAPIKey:     requiredEnv(t, "FIREWORKS_API_KEY"),
		BridgeBinary:        requiredEnv(t, "ELASTICCLAW_E2E_BRIDGE_BINARY"),
		BridgeToken:         "bridge-" + runID,
		Model:               envOrDefault("ELASTICCLAW_E2E_MODEL", defaultModel),
		RunID:               runID,
	}
	if sandboxProvider != "docker" {
		env.GitHubToken = requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_TOKEN")
		env.GitHubAppID = requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_APP_ID")
		env.GitHubAppPrivateKey = requiredEnv(t, "ELASTICCLAW_E2E_GITHUB_APP_PRIVATE_KEY")
	}
	switch sandboxProvider {
	case "daytona":
		env.DaytonaAPIKey = requiredEnv(t, "DAYTONA_API_KEY")
		env.ProviderPrefix = e2eProviderPrefix(daytonaPrefix, runID)
	case "replicated":
		env.ReplicatedToken = requiredEnv(t, "REPLICATED_API_TOKEN")
		env.ReplicatedAPIURL = os.Getenv("ELASTICCLAW_E2E_REPLICATED_API_URL")
		env.ReplicatedType = envOrDefault("ELASTICCLAW_E2E_REPLICATED_INSTANCE_TYPE", "r1.small")
		env.ReplicatedTTL = envOrDefault("ELASTICCLAW_E2E_REPLICATED_TTL", "1h")
		env.ProviderPrefix = e2eProviderPrefix(cmxPrefix, runID)
	case "docker":
		env.DockerImage = os.Getenv("ELASTICCLAW_E2E_DOCKER_IMAGE")
		env.ProviderPrefix = e2eProviderPrefix(dockerPrefix, runID)
	default:
		t.Fatalf("unsupported E2E sandbox provider %q", sandboxProvider)
	}
	return env
}

func e2eProviderPrefix(base, runID string) string {
	return fmt.Sprintf("%s-%s-", base, runID)
}

type hubProcess struct {
	baseURL string
	token   string
	cmd     *exec.Cmd
	logPath string
}

func startHub(ctx context.Context, t *testing.T, env e2eEnv) *hubProcess {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "hub.yaml")
	dbPath := filepath.Join(dir, "hub.db")
	logPath := filepath.Join(dir, "hub.log")
	baseURL := "http://" + env.HubAddr

	providerConfig := ""
	switch env.SandboxProvider {
	case "daytona":
		providerConfig = fmt.Sprintf(`  daytona:
    type: daytona
    api_key: %q
`, env.DaytonaAPIKey)
	case "replicated":
		providerConfig = fmt.Sprintf(`  replicated:
    type: replicated
    token: %q
    default_instance_type: %q
    default_ttl: %q
`, env.ReplicatedToken, env.ReplicatedType, env.ReplicatedTTL)
		if env.ReplicatedAPIURL != "" {
			providerConfig += fmt.Sprintf("    api_url: %q\n", env.ReplicatedAPIURL)
		}
	case "docker":
		providerConfig = `  docker:
    type: docker
`
		if env.DockerImage != "" {
			providerConfig += fmt.Sprintf("    image: %q\n", env.DockerImage)
		}
	}

	config := fmt.Sprintf(`schema_version: v1
url: %s
public_url: %s
token: %s
claw_token: %s
bridge_image: %s
providers:
%s
default_model: %s
llm_keys:
  - name: fireworks
    provider: fireworks
    api_key: %q
    default: true
    default_model: %s
`, baseURL, env.PublicURL, userToken, agentToken, env.PublicURL+"/__elasticclaw_e2e/claw-bridge-linux-amd64?token="+url.QueryEscape(env.BridgeToken), providerConfig, env.Model, env.FireworksAPIKey, env.Model)
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatalf("write hub config: %v", err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create hub log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })

	cmd := exec.Command(env.Bin, "hub", "--addr", hubListenAddr(env), "--db", dbPath, "--no-web-ui")
	cmd.Env = append(os.Environ(),
		"ELASTICCLAW_HUB_CONFIG="+configPath,
		"DAYTONA_API_KEY="+env.DaytonaAPIKey,
		"FIREWORKS_API_KEY="+env.FireworksAPIKey,
		"ELASTICCLAW_E2E_BRIDGE_BINARY="+env.BridgeBinary,
		"ELASTICCLAW_E2E_BRIDGE_TOKEN="+env.BridgeToken,
		"ELASTICCLAW_PROVIDER_NAME_PREFIX="+env.ProviderPrefix,
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
	waitForPublicHub(ctx, t, env.PublicURL)
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

func waitForPublicHub(ctx context.Context, t *testing.T, publicURL string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	healthURL := strings.TrimRight(publicURL, "/") + "/healthz"
	var lastErr string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		req.Header.Set("ngrok-skip-browser-warning", "true")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			lastErr = fmt.Sprintf("status %d", resp.StatusCode)
		} else {
			lastErr = err.Error()
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("public hub URL %s did not become reachable: %s", healthURL, lastErr)
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
provider: %s
repositories:
  - repo: %s
    permissions: write
`, workspaceName, env.SandboxProvider, env.GitHubRepo))
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

func writeDockerWorkspaceFixture(t *testing.T, env e2eEnv, workspaceName, workflowName string) string {
	t.Helper()
	root := t.TempDir()
	workspaceDir := filepath.Join(root, ".elasticclaw", "workspaces", workspaceName)
	workflowDir := filepath.Join(root, ".elasticclaw", "workflows")
	if err := os.MkdirAll(workspaceDir, 0750); err != nil {
		t.Fatalf("mkdir docker workspace fixture: %v", err)
	}
	if err := os.MkdirAll(workflowDir, 0750); err != nil {
		t.Fatalf("mkdir docker workflow fixture: %v", err)
	}
	writeFile(t, filepath.Join(workspaceDir, "elasticclaw-config.yaml"), fmt.Sprintf(`schema_version: v1
name: %s
provider: %s
`, workspaceName, env.SandboxProvider))
	writeFile(t, filepath.Join(workspaceDir, "AGENTS.md"), "You are an ElasticClaw Docker E2E agent. Reply briefly.\n")
	writeFile(t, filepath.Join(workspaceDir, "TOOLS.md"), "Use no external tools for this smoke test.\n")
	writeFile(t, filepath.Join(workspaceDir, "MEMORY.md"), "Docker provider workspace copy must handle /home/claw permissions.\n")
	writeFile(t, filepath.Join(workspaceDir, "CONTEXT.md"), "This is a Docker sandbox provider smoke test.\n")
	writeFile(t, filepath.Join(workflowDir, "docker.yaml"), fmt.Sprintf(`schema_version: v1
name: %s
enable_manual_trigger: true

stages:
  - id: working
    label: Working
    entry: true
    on_enter:
      run:
        command: echo '{"status":"passed"}'
        output: docker_smoke
        timeout: 30s
      inject: |
        Docker provider run action executed. Output captured: {{ .Outputs.docker_smoke.status }}
      gate:
        output: docker_smoke
        pass:
          path: status
          values: [passed]
        required: true
`, workflowName))
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
	h.putIssueTrackerWithConfig(ctx, t, workspaceName, map[string]string{
		"type":          trackerType,
		"workspace":     name,
		"token":         token,
		"webhookSecret": webhookSecret,
	})
}

func (h *hubProcess) putIssueTrackerWithConfig(ctx context.Context, t *testing.T, workspaceName string, body map[string]string) {
	t.Helper()
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

func (h *hubProcess) agentProvider(ctx context.Context, t *testing.T, agentID string) (string, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.baseURL+"/api/claws/"+agentID, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("read agent provider id: %v", err)
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ""
	}
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		t.Logf("read agent provider id: %s: %s", resp.Status, strings.TrimSpace(string(data)))
		return "", ""
	}
	var agent types.Claw
	if err := json.NewDecoder(resp.Body).Decode(&agent); err != nil {
		t.Logf("decode agent provider id: %v", err)
		return "", ""
	}
	return agent.Provider, agent.ProviderID
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

func cleanupProvider(ctx context.Context, t *testing.T, env e2eEnv) {
	t.Helper()
	switch env.SandboxProvider {
	case "daytona":
		cleanupDaytonaE2ESandboxes(ctx, t, env)
	case "replicated":
		// Replicated CMX has no broad sweep here; recorded VM IDs and direct
		// agent cleanup handle VMs created by this run, with CMX TTL as backup.
		return
	case "docker":
		return
	default:
		t.Fatalf("unsupported E2E sandbox provider %q", env.SandboxProvider)
	}
}

func destroyProviderInstanceByID(ctx context.Context, t *testing.T, env e2eEnv, provider, providerID string) {
	t.Helper()
	switch provider {
	case "daytona":
		destroyDaytonaSandboxByID(ctx, t, env, providerID)
	case "replicated":
		destroyReplicatedVMByID(ctx, t, env, providerID)
	case "docker":
		destroyDockerContainerByID(ctx, t, providerID)
	case "":
		return
	default:
		t.Logf("no E2E cleanup handler for provider %q instance %q", provider, providerID)
	}
}

func destroyDockerContainerByID(ctx context.Context, t *testing.T, containerID string) {
	t.Helper()
	provider, err := dockerProvider.New(dockerProvider.Config{})
	if err != nil {
		t.Fatalf("create Docker provider for E2E cleanup: %v", err)
	}
	if err := provider.Destroy(ctx, containerID, false); err != nil {
		t.Fatalf("delete Docker E2E container %s: %v", containerID, err)
	}
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
			if !strings.HasPrefix(instance.Name, env.ProviderPrefix) {
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
			t.Fatalf("timed out waiting for Daytona E2E sandboxes with prefix %q to terminate", env.ProviderPrefix)
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
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := provider.Destroy(ctx, sandboxID, false); err != nil {
			if isBenignDaytonaDeleteError(err) {
				return
			}
			if !isRetryableDaytonaDeleteError(err) && time.Now().After(deadline) {
				t.Fatalf("delete Daytona E2E sandbox %s: %v", sandboxID, err)
			}
		}
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

func destroyReplicatedVMByID(ctx context.Context, t *testing.T, env e2eEnv, vmID string) {
	t.Helper()
	provider, err := replicatedProvider.New(replicatedProvider.Config{
		Token:       env.ReplicatedToken,
		APIURL:      env.ReplicatedAPIURL,
		DefaultType: env.ReplicatedType,
		DefaultTTL:  env.ReplicatedTTL,
	})
	if err != nil {
		t.Fatalf("create Replicated provider for E2E VM cleanup: %v", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := provider.Destroy(ctx, vmID, false); err != nil {
			if isBenignReplicatedDeleteError(err) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("delete Replicated E2E VM %s: %v", vmID, err)
			}
		} else {
			return
		}
		if time.Now().After(deadline) {
			return
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

func isRetryableDaytonaDeleteError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 409") ||
		strings.Contains(msg, "modified by another operation") ||
		strings.Contains(msg, "conflict")
}

func isBenignReplicatedDeleteError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "404") ||
		strings.Contains(msg, "terminated") ||
		strings.Contains(msg, "delet")
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

func (g githubClient) cleanupGitHubE2EResources(ctx context.Context, t *testing.T, workspaceName, runID, labelName string) {
	t.Helper()
	g.deleteE2EHooksForWorkspace(ctx, t, workspaceName)
	g.deleteStaleGitHubE2EHooks(ctx, t, time.Now().UTC())
	g.closeE2EIssuesForRun(ctx, t, runID, labelName)
	_ = g.deleteLabel(ctx, labelName)
	if shouldSweepStaleE2E() {
		g.deleteE2EHooks(ctx, t)
		g.cleanupE2EIssuesAndLabels(ctx, t)
	}
}

const staleGitHubE2EHookAge = 45 * time.Minute

func (g githubClient) deleteStaleGitHubE2EHooks(ctx context.Context, t *testing.T, now time.Time) {
	t.Helper()
	var hooks []struct {
		ID        int64     `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		Config    struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	g.api(ctx, t, http.MethodGet, "hooks", nil, &hooks)
	for _, hook := range hooks {
		if !isStaleGitHubE2EHook(hook.Config.URL, hook.CreatedAt, now) {
			continue
		}
		if err := g.deleteHook(ctx, hook.ID); err != nil {
			t.Fatalf("delete stale E2E hook %d: %v", hook.ID, err)
		}
	}
}

func isStaleGitHubE2EHook(hookURL string, createdAt, now time.Time) bool {
	return isGitHubE2EHookURL(hookURL) && !createdAt.IsZero() && createdAt.Before(now.Add(-staleGitHubE2EHookAge))
}

func shouldSweepStaleE2E() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("ELASTICCLAW_E2E_SWEEP_STALE")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("ELASTICCLAW_E2E_SWEEP_STALE")), "true")
}

func (g githubClient) deleteE2EHooksForWorkspace(ctx context.Context, t *testing.T, workspaceName string) {
	t.Helper()
	g.deleteE2EHooksMatching(ctx, t, func(url string) bool {
		return githubE2EHookURLMatchesWorkspace(url, workspaceName)
	})
}

func (g githubClient) deleteE2EHooks(ctx context.Context, t *testing.T) {
	t.Helper()
	g.deleteE2EHooksMatching(ctx, t, isGitHubE2EHookURL)
}

func (g githubClient) deleteE2EHooksMatching(ctx context.Context, t *testing.T, match func(string) bool) {
	t.Helper()
	var hooks []struct {
		ID     int64 `json:"id"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	g.api(ctx, t, http.MethodGet, "hooks", nil, &hooks)
	for _, hook := range hooks {
		if match(hook.Config.URL) {
			if err := g.deleteHook(ctx, hook.ID); err != nil {
				t.Fatalf("delete orphaned E2E hook %d: %v", hook.ID, err)
			}
		}
	}
}

func githubE2EHookURLMatchesWorkspace(hookURL, workspaceName string) bool {
	return strings.Contains(hookURL, "/api/workspaces/"+workspaceName+"/webhooks/github-issues")
}

func isGitHubE2EHookURL(hookURL string) bool {
	return strings.Contains(hookURL, "/api/workspaces/") && strings.HasSuffix(hookURL, "/webhooks/github-issues")
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
	g.closeE2EIssuesMatching(ctx, t, isE2EIssue)
}

func (g githubClient) closeE2EIssuesForRun(ctx context.Context, t *testing.T, runID, labelName string) {
	t.Helper()
	g.closeE2EIssuesMatching(ctx, t, func(title, body string, labels []struct {
		Name string `json:"name"`
	}) bool {
		return isE2EIssueForRun(title, body, labels, runID, labelName)
	})
}

func (g githubClient) closeE2EIssuesMatching(ctx context.Context, t *testing.T, match func(string, string, []struct {
	Name string `json:"name"`
}) bool) {
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
		if !match(issue.Title, issue.Body, issue.Labels) {
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

func isE2EIssueForRun(title, body string, labels []struct {
	Name string `json:"name"`
}, runID, labelName string) bool {
	if runID != "" && bodyHasE2ERunID(body, runID) {
		return true
	}
	for _, label := range labels {
		if label.Name == labelName {
			return true
		}
	}
	return false
}

func bodyHasE2ERunID(body, runID string) bool {
	marker := "ElasticClaw E2E run: " + runID
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == marker {
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

func hubListenAddr(env e2eEnv) string {
	if env.SandboxProvider != "docker" {
		return env.HubAddr
	}
	host, port, ok := strings.Cut(env.HubAddr, ":")
	if !ok || port == "" {
		return env.HubAddr
	}
	switch host {
	case "127.0.0.1", "localhost", "0.0.0.0":
		return "0.0.0.0:" + port
	default:
		return env.HubAddr
	}
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
