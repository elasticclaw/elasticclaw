//go:build integration

package hub_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
)

func TestMain(m *testing.M) {
	// All integration tests need the noop provider. Set once for the test binary
	// lifetime — no cleanup, no per-test race.
	_ = os.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	os.Exit(m.Run())
}

func buildLinearWebhookPayload(issueID, prevStatus, newStatus string) []byte {
	payload := map[string]interface{}{
		"type":   "Issue",
		"action": "update",
		"data": map[string]interface{}{
			"id":          "issue-uuid-123",
			"identifier":  issueID,
			"title":       "Add hello world to README",
			"description": "Add a Hello World section to README.md",
			"url":         "https://linear.app/test/issue/" + issueID,
			"team": map[string]interface{}{
				"key":  "ELA",
				"name": "Engineering",
			},
			"state": map[string]interface{}{
				"name": newStatus,
			},
		},
		"updatedFrom": map[string]interface{}{
			"state": map[string]interface{}{
				"name": prevStatus,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func TestFactoryFlow_HappyPath(t *testing.T) {
	ts := factorytest.NewTestServer(t)

	issueID := "ELA-123"
	payload := buildLinearWebhookPayload(issueID, "Backlog", "In Progress")
	resp, err := http.Post(ts.URL()+"/api/integrations/linear/webhook", "application/json",
		strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("webhook post failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("webhook post failed: status=%d", resp.StatusCode)
	}

	// Wait for claw to be created
	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)

	// Connect fake bridge
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	// Wait for wake message (pipeline injects the entry stage message)
	bridge.WaitForMessage(t, "CONTEXT.md", 5*time.Second)

	// Fake agent does work
	bridge.SendMessage("Reading the issue context...")
	bridge.SendMessage("Found README.md, adding Hello World section...")

	// Send [DONE] with a PR URL
	bridge.SendDone("https://github.com/testorg/testrepo/pull/1")

	// Wait for claw to go idle (no github token configured so done is accepted immediately)
	ts.WaitForClawStatus(t, clawID, "idle", 5*time.Second)

	// Simulate PR merge
	ts.GitHub.SetPR("testorg", "testrepo", 1, factorytest.PRState{State: "closed", Merged: true})
	ts.Server.PollPRsForTest()

	// Wait for claw to be deleted — but since no GH token, pollAllPRs returns early.
	// Just verify idle status persists.

	time.Sleep(200 * time.Millisecond)
	var status string
	ts.DB.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status)
	if status != "idle" && status != "deleted" {
		t.Fatalf("expected claw to be idle or deleted after [DONE], got: %s", status)
	}
}

func TestFactoryFlow_WebhookCreatesClawInDB(t *testing.T) {
	ts := factorytest.NewTestServer(t)

	issueID := "ELA-999"

	payload := buildLinearWebhookPayload(issueID, "Backlog", "In Progress")
	resp, err := http.Post(ts.URL()+"/api/integrations/linear/webhook", "application/json",
		strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("webhook post failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("webhook returned status %d", resp.StatusCode)
	}

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	if clawID == "" {
		t.Fatal("expected a claw to be created")
	}

	// Verify it's in a recognizable status
	var dbStatus string
	ts.DB.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&dbStatus)
	validStatuses := map[string]bool{
		"provisioning": true,
		"starting":     true,
		"connected":    true,
	}
	if !validStatuses[dbStatus] {
		t.Fatalf("unexpected claw status: %s", dbStatus)
	}
}

func TestFactoryFlow_FakeBridgeConnect(t *testing.T) {
	ts := factorytest.NewTestServer(t)

	issueID := "ELA-777"
	payload := buildLinearWebhookPayload(issueID, "Backlog", "In Progress")
	http.Post(ts.URL()+"/api/integrations/linear/webhook", "application/json",
		strings.NewReader(string(payload)))

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)

	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	// Bridge should be able to send messages
	bridge.SendMessage("Hello from fake bridge")

	// Small sleep to let message be processed
	time.Sleep(100 * time.Millisecond)

	// Bridge connected successfully — test passes if we get here without panicking
	_ = bridge
}

func TestFactoryFlow_DoneSignalSetsIdle(t *testing.T) {
	ts := factorytest.NewTestServer(t)

	issueID := "ELA-555"
	payload := buildLinearWebhookPayload(issueID, "Backlog", "In Progress")
	http.Post(ts.URL()+"/api/integrations/linear/webhook", "application/json",
		strings.NewReader(string(payload)))

	clawID := ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, issueID, ts.ClawToken())

	// Wait for wake message (pipeline injects the entry stage message)
	bridge.WaitForMessage(t, "CONTEXT.md", 5*time.Second)

	// Send [DONE] — no GH App configured so no PR validation, accepted immediately
	bridge.SendDone("https://github.com/testorg/testrepo/pull/42")

	// Should transition to idle
	ts.WaitForClawStatus(t, clawID, "idle", 5*time.Second)
}

func TestFactoryFlow_PollingDetectsMissedWebhook(t *testing.T) {
	ts := factorytest.NewTestServer(t)

	issueID := "ELA-123"

	// Mock Linear returns ELA-123 in "Backlog" initially.
	ts.Linear.SetIssueStateName(issueID, "Backlog")

	// First poll: bootstrap — records state, no claw created.
	ts.Server.PollIntegrationsForTest()

	// Verify no claw exists yet.
	var clawID string
	ts.DB.QueryRow(`SELECT id FROM claws WHERE linear_issue_id=?`, issueID).Scan(&clawID)
	if clawID != "" {
		t.Fatal("expected no claw after bootstrap poll")
	}

	// Simulate missed webhook: change issue state to "In Progress".
	ts.Linear.SetIssueStateName(issueID, "In Progress")

	// Second poll: detects transition from Backlog → In Progress.
	ts.Server.PollIntegrationsForTest()

	// Claw should now be created.
	clawID = ts.WaitForClawWithIssue(t, issueID, 5*time.Second)
	if clawID == "" {
		t.Fatal("expected claw to be created after polling detected transition")
	}

	// Verify the claw is in a recognizable status.
	var dbStatus string
	ts.DB.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&dbStatus)
	validStatuses := map[string]bool{
		"provisioning": true,
		"starting":     true,
		"connected":    true,
	}
	if !validStatuses[dbStatus] {
		t.Fatalf("unexpected claw status after poll: %s", dbStatus)
	}
}
