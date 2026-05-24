//go:build integration

package hub_test

import (
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
)

// S1: webhook spawns claw with correct factory match
func TestParity_WebhookSpawnsClaw(t *testing.T) {
	factorytest.RunParityMatrix(t, factorytest.Scenario{
		Name: "webhook spawns claw with correct factory match",
		Fn: func(t *testing.T, td factorytest.TrackerDispatcher, ts *factorytest.TestServer) {
			td.SetIssue(ts, td.IssueID, td.Trigger)

			resp := td.Webhook(t, ts, td.IssueID, "Backlog", td.Trigger)
			if resp == nil {
				t.Fatal("webhook returned nil response")
			}
			if resp.StatusCode != 200 {
				t.Fatalf("webhook returned status %d", resp.StatusCode)
			}

			clawID := factorytest.WaitForClawWithTracker(t, ts, td, 5*time.Second)
			if clawID == "" {
				t.Fatal("expected claw to be created")
			}
		},
	})
}

// S2: issue status change triggers pipeline stage transition
func TestParity_StatusChangeTriggersPipeline(t *testing.T) {
	factorytest.RunParityMatrix(t, factorytest.Scenario{
		Name: "issue status change triggers pipeline stage transition",
		Fn: func(t *testing.T, td factorytest.TrackerDispatcher, ts *factorytest.TestServer) {
			td.SetIssue(ts, td.IssueID, td.Trigger)
			resp := td.Webhook(t, ts, td.IssueID, "Backlog", td.Trigger)
			if resp == nil {
				t.Fatal("webhook returned nil response")
			}
			if resp.StatusCode != 200 {
				t.Fatalf("webhook returned status %d", resp.StatusCode)
			}

			clawID := factorytest.WaitForClawWithTracker(t, ts, td, 5*time.Second)

			bridge := factorytest.ConnectFakeBridge(t, ts.URL(), clawID, td.IssueID, ts.ClawToken())
			bridge.WaitForMessage(t, "CONTEXT.md", 5*time.Second)
		},
	})
}

// S3: claw created via factory webhook carries correct tracker metadata
func TestParity_FactoryTrackerMetadata(t *testing.T) {
	factorytest.RunParityMatrix(t, factorytest.Scenario{
		Name: "claw carries correct tracker metadata from factory integration",
		Fn: func(t *testing.T, td factorytest.TrackerDispatcher, ts *factorytest.TestServer) {
			td.SetIssue(ts, td.IssueID, td.Trigger)
			resp := td.Webhook(t, ts, td.IssueID, "Backlog", td.Trigger)
			if resp == nil {
				t.Fatal("webhook returned nil response")
			}
			if resp.StatusCode != 200 {
				t.Fatalf("webhook returned status %d", resp.StatusCode)
			}

			clawID := factorytest.WaitForClawWithTracker(t, ts, td, 5*time.Second)

			var trackerField string
			switch td.Name {
			case "shortcut":
				trackerField = "shortcut_story_id"
			case "github-issues":
				trackerField = "github_issue_id"
			default:
				trackerField = "linear_issue_id"
			}
			var dbID string
			err := ts.DB.QueryRow(`SELECT `+trackerField+` FROM claws WHERE id=?`, clawID).Scan(&dbID)
			if err != nil {
				t.Fatalf("claw missing %s: %v", trackerField, err)
			}
			if dbID != td.IssueID {
				t.Fatalf("claw %s: want %q got %q", trackerField, td.IssueID, dbID)
			}
		},
	})
}

// S4: webhook + poll see the same event → exactly one claw spawned (OQ-3)
func TestParity_WebhookPollDedup(t *testing.T) {
	factorytest.RunParityMatrix(t, factorytest.Scenario{
		Name: "webhook and poll deduplicate",
		Fn: func(t *testing.T, td factorytest.TrackerDispatcher, ts *factorytest.TestServer) {
			td.SetIssue(ts, td.IssueID, td.Trigger)

			resp := td.Webhook(t, ts, td.IssueID, "Backlog", td.Trigger)
			if resp == nil {
				t.Fatal("webhook returned nil response")
			}
			if resp.StatusCode != 200 {
				t.Fatalf("webhook returned status %d", resp.StatusCode)
			}

			clawID1 := factorytest.WaitForClawWithTracker(t, ts, td, 5*time.Second)

			ts.Server.PollIntegrationsForTest()

			if td.Name != "github-issues" {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					var sawPoll bool
					switch td.Name {
					case "shortcut":
						sawPoll = ts.Shortcut.SawPollCall()
					default:
						sawPoll = ts.Linear.SawPollCall()
					}
					if sawPoll {
						lastCount := -1
						for i := 0; i < 20; i++ {
							count := factorytest.CountClawsForTracker(t, ts, td)
							if count == lastCount {
								break
							}
							lastCount = count
							time.Sleep(50 * time.Millisecond)
						}
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			}

			count := factorytest.CountClawsForTracker(t, ts, td)
			if count != 1 {
				t.Fatalf("expected exactly 1 claw ever created, got %d (OQ-3 dedup bug)", count)
			}

			// Strengthening: assert the surviving claw is the original one,
			// not a "delete first, keep second" replacement.
			clawIDNow := factorytest.WaitForClawWithTracker(t, ts, td, 5*time.Second)
			if clawIDNow != clawID1 {
				t.Fatalf("dedup replaced claw: original %s, now %s", clawID1, clawIDNow)
			}
		},
	})
}
