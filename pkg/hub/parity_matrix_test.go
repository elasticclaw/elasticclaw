//go:build integration

package hub_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
)

// trackerDispatcher abstracts the differences between Linear, Shortcut, and GitHub Issues
// so the same scenario can run against all three.
type trackerDispatcher struct {
	name      string
	setIssue  func(ts *factorytest.TestServer, id string, status string)
	webhook   func(ts *factorytest.TestServer, id string, prevStatus, newStatus string) *http.Response
	issueID   string // the test issue/story ID
	trigger   string // the status that triggers the factory
	done      string // the status that marks done
}

var trackers = []trackerDispatcher{
	{
		name: "linear",
		setIssue: func(ts *factorytest.TestServer, id string, status string) {
			ts.Linear.SetIssueStateName(id, status)
		},
		webhook: func(ts *factorytest.TestServer, id string, prevStatus, newStatus string) *http.Response {
			payload := buildLinearWebhookPayload(id, prevStatus, newStatus)
			resp, _ := http.Post(ts.URL()+"/api/integrations/linear/webhook", "application/json",
				strings.NewReader(string(payload)))
			return resp
		},
		issueID: "ELA-123",
		trigger: "In Progress",
		done:    "Done",
	},
	{
		name: "shortcut",
		setIssue: func(ts *factorytest.TestServer, id string, status string) {
			// Map status name to workflow state ID
			var stateID int64
			switch status {
			case "Backlog":
				stateID = 5001
			case "In Progress":
				stateID = 5002
			case "Done":
				stateID = 5003
			}
			storyNum := parseStoryNum(id)
			ts.Shortcut.SetStoryState(storyNum, stateID)
		},
		webhook: func(ts *factorytest.TestServer, id string, prevStatus, newStatus string) *http.Response {
			storyNum := parseStoryNum(id)
			var prevID, newID int64
			switch prevStatus {
			case "Backlog":
				prevID = 5001
			case "In Progress":
				prevID = 5002
			}
			switch newStatus {
			case "In Progress":
				newID = 5002
			case "Done":
				newID = 5003
			}
			payload, sig := ts.Shortcut.BuildWebhookPayload(storyNum, prevID, newID, "test-webhook-secret")
			req, _ := http.NewRequest("POST", ts.URL()+"/api/integrations/shortcut/webhook", strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Payload-Signature", sig)
			resp, _ := http.DefaultClient.Do(req)
			return resp
		},
		issueID: "sc-123",
		trigger: "In Progress",
		done:    "Done",
	},
}

func parseStoryNum(id string) int64 {
	// sc-123 → 123
	var n int64
	fmt.Sscanf(id, "sc-%d", &n)
	return n
}

// scenario defines a test scenario that runs against all trackers.
type scenario struct {
	name string
	fn   func(t *testing.T, td trackerDispatcher, ts *factorytest.TestServer)
}

func runParityMatrix(t *testing.T, sc scenario) {
	for _, td := range trackers {
		t.Run(td.name+"/"+sc.name, func(t *testing.T) {
			t.Helper()
			var ts *factorytest.TestServer
			if td.name == "shortcut" {
				ts = factorytest.NewTestServerWithShortcut(t)
				// Pre-populate the story in the mock
				ts.Shortcut.SetStory(123, factorytest.StoryState{
					Name:            "Test Story",
					WorkflowStateID: 5001, // Backlog
					Description:     "Test description",
				})
			} else {
				ts = factorytest.NewTestServer(t)
			}
			sc.fn(t, td, ts)
		})
	}
}

// S1: webhook spawns claw with correct factory match
func TestParity_WebhookSpawnsClaw(t *testing.T) {
	runParityMatrix(t, scenario{
		name: "webhook spawns claw with correct factory match",
		fn: func(t *testing.T, td trackerDispatcher, ts *factorytest.TestServer) {
			// Set issue to trigger status
			td.setIssue(ts, td.issueID, td.trigger)

			// Fire webhook
			resp := td.webhook(ts, td.issueID, "Backlog", td.trigger)
			if resp.StatusCode != 200 {
				t.Fatalf("webhook returned status %d", resp.StatusCode)
			}

			// Wait for claw
			var clawID string
			if td.name == "shortcut" {
				clawID = ts.WaitForClawWithStory(t, td.issueID, 5*time.Second)
			} else {
				clawID = ts.WaitForClawWithIssue(t, td.issueID, 5*time.Second)
			}
			if clawID == "" {
				t.Fatal("expected claw to be created")
			}
		},
	})
}

// S2: issue status change triggers pipeline stage transition
func TestParity_StatusChangeTriggersPipeline(t *testing.T) {
	runParityMatrix(t, scenario{
		name: "issue status change triggers pipeline stage transition",
		fn: func(t *testing.T, td trackerDispatcher, ts *factorytest.TestServer) {
			// Spawn claw via webhook
			td.setIssue(ts, td.issueID, td.trigger)
			resp := td.webhook(ts, td.issueID, "Backlog", td.trigger)
			if resp.StatusCode != 200 {
				t.Fatalf("webhook returned status %d", resp.StatusCode)
			}

			var clawID string
			if td.name == "shortcut" {
				clawID = ts.WaitForClawWithStory(t, td.issueID, 5*time.Second)
			} else {
				clawID = ts.WaitForClawWithIssue(t, td.issueID, 5*time.Second)
			}

			// Connect fake bridge
			bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, td.issueID, ts.ClawToken())

			// Wait for entry stage inject
			bridge.WaitForMessage(t, "CONTEXT.md", 5*time.Second)
		},
	})
}

// S3: move_issue resolves correct tracker from factory integration
func TestParity_MoveIssueResolvesTracker(t *testing.T) {
	runParityMatrix(t, scenario{
		name: "move_issue resolves correct tracker from factory integration",
		fn: func(t *testing.T, td trackerDispatcher, ts *factorytest.TestServer) {
			// This scenario verifies that when a factory specifies an integration,
			// the pipeline's move_issue action targets the correct tracker.
			// For now, we verify the factory config is wired correctly by checking
			// the claw is created with the right tracker metadata.

			td.setIssue(ts, td.issueID, td.trigger)
			resp := td.webhook(ts, td.issueID, "Backlog", td.trigger)
			if resp.StatusCode != 200 {
				t.Fatalf("webhook returned status %d", resp.StatusCode)
			}

			var clawID string
			if td.name == "shortcut" {
				clawID = ts.WaitForClawWithStory(t, td.issueID, 5*time.Second)
			} else {
				clawID = ts.WaitForClawWithIssue(t, td.issueID, 5*time.Second)
			}

			// Verify the claw has the correct tracker field set
			var trackerField string
			if td.name == "shortcut" {
				trackerField = "shortcut_story_id"
			} else {
				trackerField = "linear_issue_id"
			}
			var dbID string
			err := ts.DB.QueryRow(`SELECT `+trackerField+` FROM claws WHERE id=?`, clawID).Scan(&dbID)
			if err != nil {
				t.Fatalf("claw missing %s: %v", trackerField, err)
			}
			if dbID != td.issueID {
				t.Fatalf("claw %s: want %q got %q", trackerField, td.issueID, dbID)
			}
		},
	})
}

// S4: webhook + poll see the same event → exactly one claw spawned (OQ-3)
func TestParity_WebhookPollDedup(t *testing.T) {
	runParityMatrix(t, scenario{
		name: "webhook and poll deduplicate",
		fn: func(t *testing.T, td trackerDispatcher, ts *factorytest.TestServer) {
			// Set issue to trigger status
			td.setIssue(ts, td.issueID, td.trigger)

			// Fire webhook
			resp := td.webhook(ts, td.issueID, "Backlog", td.trigger)
			if resp.StatusCode != 200 {
				t.Fatalf("webhook returned status %d", resp.StatusCode)
			}

			// Wait for first claw
			var clawID1 string
			if td.name == "shortcut" {
				clawID1 = ts.WaitForClawWithStory(t, td.issueID, 5*time.Second)
			} else {
				clawID1 = ts.WaitForClawWithIssue(t, td.issueID, 5*time.Second)
			}

			// Trigger integration poll
			ts.Server.PollIntegrationsForTest()

			// Wait a bit for poll to process
			time.Sleep(200 * time.Millisecond)

			// Count claws for this issue — should be exactly 1
			var count int
			if td.name == "shortcut" {
				err := ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE shortcut_story_id=? AND status NOT IN ('deleted')`, td.issueID).Scan(&count)
				if err != nil {
					t.Fatalf("count query failed: %v", err)
				}
			} else {
				err := ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE linear_issue_id=? AND status NOT IN ('deleted')`, td.issueID).Scan(&count)
				if err != nil {
					t.Fatalf("count query failed: %v", err)
				}
			}

			if count != 1 {
				t.Fatalf("expected exactly 1 claw, got %d (OQ-3 dedup bug)", count)
			}
			_ = clawID1
		},
	})
}
