package hub

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestRepoInstructionFileNamesIncludesAgents(t *testing.T) {
	if !slicesContains(repoInstructionFileNames, "AGENTS.md") {
		t.Fatalf("repoInstructionFileNames must include AGENTS.md: %#v", repoInstructionFileNames)
	}
}

func TestBuildRepoInstructionDiscoveryScriptReferencesKnownFiles(t *testing.T) {
	script := buildRepoInstructionDiscoveryScript("$HOME/.openclaw/workspace", []types.GitHubRepoAccess{{Repo: "elasticclaw/elasticclaw"}})
	for _, want := range []string{"AGENTS.md", "CLAUDE.md", "GEMINI.md", "REPO_INSTRUCTIONS.md"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func TestBuildRepoInstructionDiscoveryScriptWritesIndexAndAgentsReferenceOnce(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "elasticclaw")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "AGENTS.md"), []byte("repo instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("# Workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := buildRepoInstructionDiscoveryScript(dir, []types.GitHubRepoAccess{{Repo: "elasticclaw/elasticclaw"}})
	runBashScript(t, script)
	runBashScript(t, script)

	index, err := os.ReadFile(filepath.Join(dir, "REPO_INSTRUCTIONS.md"))
	if err != nil {
		t.Fatalf("read generated index: %v", err)
	}
	indexText := string(index)
	if !strings.Contains(indexText, "`elasticclaw/AGENTS.md`") {
		t.Fatalf("generated index missing repo AGENTS.md reference:\n%s", indexText)
	}
	if strings.Contains(indexText, "repo instructions") {
		t.Fatalf("generated index should reference repo files, not copy their contents:\n%s", indexText)
	}

	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read workspace AGENTS.md: %v", err)
	}
	if count := strings.Count(string(agents), "## Repository Instructions"); count != 1 {
		t.Fatalf("workspace AGENTS.md should contain one repo instruction section, got %d:\n%s", count, agents)
	}
}

func TestBuildRepoInstructionDiscoveryScriptRemovesStaleIndexWhenNoInstructionFiles(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "elasticclaw")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(dir, "REPO_INSTRUCTIONS.md")
	if err := os.WriteFile(stalePath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := buildRepoInstructionDiscoveryScript(dir, []types.GitHubRepoAccess{{Repo: "elasticclaw/elasticclaw"}})
	runBashScript(t, script)

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale REPO_INSTRUCTIONS.md should be removed, stat err=%v", err)
	}
}

func TestBuildGitHubCredentialHelperRunsRepoInstructionDiscoveryAfterClone(t *testing.T) {
	cfg := &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
		ClawToken:  "test-claw-token",
	}
	script := buildGitHubCredentialHelper(cfg, "https://hub.example.com", "claw-123", []types.GitHubRepoAccess{{Repo: "elasticclaw/elasticclaw"}})

	cloneIdx := strings.Index(script, "git clone https://github.com/elasticclaw/elasticclaw")
	discoveryIdx := strings.Index(script, "REPO_INSTRUCTIONS.md")
	if cloneIdx == -1 {
		t.Fatalf("credential helper script missing clone command:\n%s", script)
	}
	if discoveryIdx == -1 {
		t.Fatalf("credential helper script missing repo instruction discovery:\n%s", script)
	}
	if discoveryIdx < cloneIdx {
		t.Fatalf("repo instruction discovery must run after clone/pull")
	}
}

func TestBootstrapWakeRequiresBootstrapOKForManagedProviders(t *testing.T) {
	for _, provider := range []string{"daytona", "replicated", "exedev"} {
		if allowWakeBeforeBootstrap(provider, 0) {
			t.Fatalf("%s should not allow wake before bootstrap_ok=1", provider)
		}
		if !allowWakeBeforeBootstrap(provider, 1) {
			t.Fatalf("%s should allow wake after bootstrap_ok=1", provider)
		}
	}
	if !allowWakeBeforeBootstrap("noop", 0) {
		t.Fatalf("non-managed provider should preserve existing wake behavior")
	}
}

func TestManagedProviderReadyRegistrationPromotesAfterBootstrapOK(t *testing.T) {
	ready := true
	clawID := "claw-replicated-ready-before-bootstrap"
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, provider, status, bootstrap_ok, tags, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "replicated claw", "elasticclaw", "replicated", "starting", 0, `[]`,
	); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clawWS, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw ws: %v", err)
	}
	t.Cleanup(func() { _ = clawWS.Close(websocket.StatusNormalClosure, "done") })

	if err := wsjson.Write(ctx, clawWS, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID:       clawID,
			Name:         "replicated claw",
			Template:     "elasticclaw",
			Token:        "claw-token",
			GatewayReady: &ready,
		},
	}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(ctx, clawWS, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}
	if registered.Type != "registered" {
		t.Fatalf("registration ack type = %q, want registered", registered.Type)
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "starting" {
		t.Fatalf("status after gated registration = %q, want starting", status)
	}

	if _, err := db.Exec(`UPDATE claws SET bootstrap_ok=1 WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, clawWS, types.WSMessage{
		Type: "heartbeat",
		Payload: map[string]interface{}{
			"gateway_healthy": true,
			"gateway_ready":   true,
			"context_usage":   0,
		},
	}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == "connected" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("status after bootstrap_ok heartbeat = %q, want connected", status)
}

func runBashScript(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s\nscript:\n%s", err, out, script)
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
