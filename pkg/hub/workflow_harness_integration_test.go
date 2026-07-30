//go:build integration

package hub_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newWorkflowHarnessServer creates a TestServer with a factory that uses the
// named workflow fixture from testdata/workflows/*.yaml.
func newWorkflowHarnessServer(t *testing.T, fixtureName string) *factorytest.TestServer {
	t.Helper()
	return newWorkflowHarnessServerWithProvider(t, fixtureName, "noop")
}

// newWorkflowHarnessServerWithProvider creates a TestServer with a specific provider.
func newWorkflowHarnessServerWithProvider(t *testing.T, fixtureName, provider string) *factorytest.TestServer {
	t.Helper()
	pipelineYAML, err := os.ReadFile("testdata/workflows/" + fixtureName + ".yaml")
	if err != nil {
		t.Fatalf("read workflow fixture %s: %v", fixtureName, err)
	}

	gh := factorytest.NewMockGitHub(t)
	li := factorytest.NewMockLinear(t)

	providers := map[string]types.ProviderConfig{
		"noop":     {Type: "noop"},
		"failing":  {Type: "failing"},
		"testexec": {Type: "testexec"},
		"docker":   {Type: "docker"},
	}

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{
			{
				Name:          "harness-factory",
				Integration:   "linear",
				Workspace:     "test-workspace",
				TriggerStatus: "In Progress",
				DoneStatus:    "done-state-id",
				Template:      "elasticclaw",
				Provider:      provider,
				PipelineYAML:  string(pipelineYAML),
			},
		},
		Integrations: &types.IntegrationsConfig{
			Linear: []*types.LinearIntegrationConfig{
				{
					Workspace: "test-workspace",
					Token:     "test-linear-token",
				},
			},
		},
		Providers: providers,
	}

	s, db := hub.NewTestServerWithConfig(t, cfg, gh.URL, li.URL, "")
	s.StartPRWatcherForTest()

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	return &factorytest.TestServer{
		Server:  s,
		HTTPSrv: httpSrv,
		GitHub:  gh,
		Linear:  li,
		DB:      db,
	}
}

// triggerFactoryWebhook sends a Linear webhook to the test server to create a claw.
func triggerFactoryWebhook(t *testing.T, ts *factorytest.TestServer, issueID, prevStatus, newStatus string) {
	t.Helper()
	payload := buildLinearWebhookPayload(issueID, prevStatus, newStatus)
	resp, err := http.Post(ts.URL()+"/api/integrations/linear/webhook", "application/json",
		strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("webhook post failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("webhook post failed: status=%d", resp.StatusCode)
	}
}

func prepareDeterministicGateWorkspace(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	workspaceDir := filepath.Join(home, ".openclaw", "workspace")
	scriptPath := filepath.Join(workspaceDir, "scripts", "deterministic_gate.py")
	writeHarnessFile(t, scriptPath, `#!/usr/bin/env python3
import argparse
import json
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("--mode", choices=["pass", "retry"], required=True)
args = parser.parse_args()

print("$ deterministic validator")

if args.mode == "retry":
    state = Path(".deterministic-gate-state")
    if not state.exists():
        state.write_text("failed-once")
        print(json.dumps({"message": "1 issue found", "status": "issues"}))
    else:
        print(json.dumps({"message": "No issues found", "status": "clean"}))
else:
    print(json.dumps({"message": "No issues found", "status": "clean"}))
`)
	t.Setenv("HOME", home)
	t.Setenv("ELASTICCLAW_TESTEXEC_PROVIDER", "1")
}

func writeHarnessFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func useTestExecProvider(t *testing.T, ts *factorytest.TestServer, clawID string) {
	t.Helper()
	_, err := ts.DB.Exec(`UPDATE claws SET provider='testexec', provider_id=? WHERE id=?`, "testexec-"+clawID[:8], clawID)
	if err != nil {
		t.Fatalf("set testexec provider: %v", err)
	}
}

func assertGateVerdict(t *testing.T, ts *factorytest.TestServer, clawID, stageID, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var verdict string
		err := ts.DB.QueryRow(`SELECT verdict FROM pipeline_gate_results WHERE claw_id=? AND stage_id=?`, clawID, stageID).Scan(&verdict)
		if err == nil && verdict == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var verdict string
	_ = ts.DB.QueryRow(`SELECT COALESCE(verdict,'') FROM pipeline_gate_results WHERE claw_id=? AND stage_id=?`, clawID, stageID).Scan(&verdict)
	t.Fatalf("gate verdict for stage %q = %q, want %q", stageID, verdict, want)
}

func assertPipelineStage(t *testing.T, ts *factorytest.TestServer, clawID, want string) {
	t.Helper()
	var stage string
	if err := ts.DB.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage); err != nil {
		t.Fatalf("select pipeline stage: %v", err)
	}
	if stage != want {
		t.Fatalf("pipeline_stage = %q, want %q", stage, want)
	}
}

func TestWorkflowHarness_GitHubIssuePrecommit(t *testing.T) {
	ts := newWorkflowHarnessServer(t, "github-issue-precommit")

	issueID := "ELA-123"
	triggerFactoryWebhook(t, ts, issueID, "Backlog", "In Progress")

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	// Wait for initial plan instruction (sent by sendInitialPlanInstruction)
	bridge.WaitForMessage(t, "Initial plan required", 5*time.Second)

	// Wait for entry stage inject
	bridge.WaitForMessage(t, "[READY_TO_COMMIT]", 5*time.Second)

	// Fake agent: ready to commit
	bridge.SendMessage("I've implemented the feature. [READY_TO_COMMIT]")

	// Wait for pre-commit stage inject (after run command)
	bridge.WaitForMessage(t, "Pre-commit checks passed", 5*time.Second)

	// Fake agent: done
	bridge.SendDone("https://github.com/testorg/testrepo/pull/1")

	// Verify claw reached terminal / deleted state (terminal stage terminates immediately)
	// Give the async goroutine time to update the DB
	time.Sleep(200 * time.Millisecond)
	ts.WaitForClawStatus(t, clawID, "deleted", 5*time.Second)

	// Verify pipeline stage is review (terminal)
	var stage string
	ts.DB.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage)
	if stage != "review" {
		t.Fatalf("expected pipeline_stage=review, got %q", stage)
	}
}

func TestWorkflowHarness_RunFailsStop(t *testing.T) {
	// Use the "failing" provider so the run action actually fails
	ts := newWorkflowHarnessServerWithProvider(t, "run-fails-stop", "failing")

	issueID := "ELA-456"
	triggerFactoryWebhook(t, ts, issueID, "Backlog", "In Progress")

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	// Wait for initial plan instruction (sent by sendInitialPlanInstruction)
	bridge.WaitForMessage(t, "Initial plan required", 5*time.Second)

	// Wait for entry stage inject (any message from hub)
	bridge.WaitForMessage(t, "Starting workflow", 5*time.Second)

	// Fake agent: proceed to run-fail stage
	bridge.SendMessage("Proceeding. [PROCEED]")

	// The run action fails (failing provider), so the inject message should NOT appear.
	// Wait briefly to ensure the inject is NOT sent.
	time.Sleep(200 * time.Millisecond)
	for _, m := range bridge.Messages {
		if strings.Contains(m.Content, "should NOT appear") {
			t.Fatal("inject message should NOT appear when run fails without continue_on_error")
		}
	}

	// Verify claw is in run-fail stage
	var stage string
	ts.DB.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage)
	if stage != "run-fail" {
		t.Fatalf("expected pipeline_stage=run-fail, got %q", stage)
	}
}

func TestWorkflowHarness_DeterministicGatePass(t *testing.T) {
	prepareDeterministicGateWorkspace(t)
	ts := newWorkflowHarnessServer(t, "deterministic-gate-pass")

	issueID := "ELA-654"
	triggerFactoryWebhook(t, ts, issueID, "Backlog", "In Progress")

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	useTestExecProvider(t, ts, clawID)
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	bridge.WaitForMessage(t, "Initial plan required", 5*time.Second)
	bridge.WaitForMessage(t, "Starting deterministic validation", 5*time.Second)

	bridge.SendMessage("Ready for validation. [DONE]")

	bridge.WaitForMessage(t, "Gate passed: Validation", 5*time.Second)
	bridge.WaitForMessage(t, "No issues found. Workflow complete.", 5*time.Second)
	assertGateVerdict(t, ts, clawID, "validation", "pass")
	assertPipelineStage(t, ts, clawID, "complete")
}

func TestWorkflowHarness_DeterministicGateRetryLoop(t *testing.T) {
	prepareDeterministicGateWorkspace(t)
	ts := newWorkflowHarnessServer(t, "deterministic-gate-retry")

	issueID := "ELA-655"
	triggerFactoryWebhook(t, ts, issueID, "Backlog", "In Progress")

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	useTestExecProvider(t, ts, clawID)
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	bridge.WaitForMessage(t, "Initial plan required", 5*time.Second)
	bridge.WaitForMessage(t, "Starting deterministic validation", 5*time.Second)

	bridge.SendMessage("Ready for validation. [DONE]")

	bridge.WaitForMessage(t, "Gate failed: Validation", 5*time.Second)
	bridge.WaitForMessage(t, "1 issue found. Apply the deterministic fix and say [RETRY].", 5*time.Second)
	assertGateVerdict(t, ts, clawID, "validation", "fail")
	assertPipelineStage(t, ts, clawID, "fix")

	bridge.SendMessage("Retrying after deterministic fix. [RETRY]")

	bridge.WaitForMessage(t, "Gate passed: Validation", 5*time.Second)
	bridge.WaitForMessage(t, "No issues found. Workflow complete.", 5*time.Second)
	assertGateVerdict(t, ts, clawID, "validation", "pass")
	assertPipelineStage(t, ts, clawID, "complete")
}

func TestWorkflowHarness_RunFailsContinue(t *testing.T) {
	// Use the "failing" provider so the run action actually fails
	ts := newWorkflowHarnessServerWithProvider(t, "run-fails-continue", "failing")

	issueID := "ELA-789"
	triggerFactoryWebhook(t, ts, issueID, "Backlog", "In Progress")

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	// Wait for initial plan instruction (sent by sendInitialPlanInstruction)
	bridge.WaitForMessage(t, "Initial plan required", 5*time.Second)

	// Wait for entry stage inject (any message from hub)
	bridge.WaitForMessage(t, "Starting workflow", 5*time.Second)

	// Fake agent: proceed to run-fail-continue stage
	bridge.SendMessage("Proceeding. [PROCEED]")

	// Wait for the warning message about the failed run
	bridge.WaitForMessage(t, "Workflow command failed", 5*time.Second)

	// Wait for the follow-up inject message (should appear because continue_on_error=true)
	bridge.WaitForMessage(t, "This message SHOULD appear", 5*time.Second)

	// Verify claw transitioned to terminal stage
	bridge.SendDone("https://github.com/testorg/testrepo/pull/1")
	// Give the async goroutine time to update the DB
	time.Sleep(200 * time.Millisecond)
	ts.WaitForClawStatus(t, clawID, "deleted", 5*time.Second)

	var stage string
	ts.DB.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage)
	if stage != "terminal" {
		t.Fatalf("expected pipeline_stage=terminal, got %q", stage)
	}
}

func TestWorkflowHarness_LinearMoveIssue(t *testing.T) {
	ts := newWorkflowHarnessServer(t, "linear-move-issue")

	issueID := "ELA-321"
	triggerFactoryWebhook(t, ts, issueID, "Backlog", "In Progress")

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	// Wait for initial plan instruction (sent by sendInitialPlanInstruction)
	bridge.WaitForMessage(t, "Initial plan required", 5*time.Second)

	// Wait for entry stage inject
	bridge.WaitForMessage(t, "[READY_FOR_REVIEW]", 5*time.Second)

	// Fake agent: ready for review
	bridge.SendMessage("Ready for review. [READY_FOR_REVIEW]")

	// Wait for terminal inject
	bridge.WaitForMessage(t, "Issue moved to In Review", 5*time.Second)

	// Verify Linear mock received the move request by checking IssueStates
	// (SawAPICall would return true for routine polling queries too)
	if ts.Linear.IssueStates["issue-uuid-123"] == "" {
		t.Fatal("expected Linear issueUpdate mutation to set issue state")
	}
}

func TestWorkflowHarness_TerminalStage(t *testing.T) {
	ts := newWorkflowHarnessServer(t, "terminal-stage")

	issueID := "ELA-999"
	triggerFactoryWebhook(t, ts, issueID, "Backlog", "In Progress")

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	// Wait for initial plan instruction (sent by sendInitialPlanInstruction)
	bridge.WaitForMessage(t, "Initial plan required", 5*time.Second)

	// Wait for entry stage inject
	bridge.WaitForMessage(t, "[DONE]", 5*time.Second)

	// Fake agent: done (with PR URL to pass validation)
	bridge.SendDone("https://github.com/testorg/testrepo/pull/1")

	// Wait for terminal inject
	bridge.WaitForMessage(t, "Workflow complete", 5*time.Second)

	// Verify the claw is deleted (terminal stage).
	// Give the async goroutine time to update the DB
	time.Sleep(200 * time.Millisecond)
	ts.WaitForClawStatus(t, clawID, "deleted", 5*time.Second)

	// Replay the same message — should not duplicate termination
	// Since the claw is deleted, the bridge is disconnected. Reconnecting
	// with the same clawID should be rejected.
	// (We verify by checking the DB status stays deleted.)
	var status string
	ts.DB.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status)
	if status != "deleted" {
		t.Fatalf("expected status=deleted, got %q", status)
	}
}
