package factorytest

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TrackerDispatcher abstracts the differences between Linear, Shortcut, and
// GitHub Issues so the same scenario can run against all three.
type TrackerDispatcher struct {
	Name     string
	SetIssue func(ts *TestServer, id string, status string)
	Webhook  func(t *testing.T, ts *TestServer, id string, prevStatus, newStatus string) *http.Response
	IssueID  string // the test issue/story ID
	Trigger  string // the status that triggers the factory
	Done     string // the status that marks done
}

// Trackers holds the parity-matrix dispatchers for all supported trackers.
var Trackers = []TrackerDispatcher{
	{
		Name: "linear",
		SetIssue: func(ts *TestServer, id string, status string) {
			ts.Linear.SetIssueStateName(id, status)
		},
		Webhook: func(t *testing.T, ts *TestServer, id string, prevStatus, newStatus string) *http.Response {
			t.Helper()
			payload, _ := ts.Linear.BuildWebhookPayload(id, prevStatus, newStatus)
			resp, err := http.Post(ts.URL()+"/api/integrations/linear/webhook", "application/json",
				strings.NewReader(string(payload)))
			if err != nil {
				t.Fatalf("linear webhook post failed: %v", err)
			}
			return resp
		},
		IssueID: "ELA-123",
		Trigger: "In Progress",
		Done:    "Done",
	},
	{
		Name: "shortcut",
		SetIssue: func(ts *TestServer, id string, status string) {
			stateID := ts.Shortcut.StateIDForName(status)
			storyNum := ParseStoryNum(id)
			ts.Shortcut.SetStoryState(storyNum, stateID)
		},
		Webhook: func(t *testing.T, ts *TestServer, id string, prevStatus, newStatus string) *http.Response {
			t.Helper()
			storyNum := ParseStoryNum(id)
			prevID := ts.Shortcut.StateIDForName(prevStatus)
			newID := ts.Shortcut.StateIDForName(newStatus)
			payload, sig := ts.Shortcut.BuildWebhookPayload(storyNum, prevID, newID, "test-webhook-secret")
			req, err := http.NewRequest("POST", ts.URL()+"/api/integrations/shortcut/webhook", strings.NewReader(string(payload)))
			if err != nil {
				t.Fatalf("shortcut webhook request build failed: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Payload-Signature", sig)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("shortcut webhook post failed: %v", err)
			}
			return resp
		},
		IssueID: "sc-123",
		Trigger: "In Progress",
		Done:    "Done",
	},
	{
		Name: "github-issues",
		SetIssue: func(ts *TestServer, id string, status string) {
			// id is "testorg/testrepo/42"
			parts := strings.Split(id, "/")
			if len(parts) != 3 {
				return
			}
			repo := parts[0] + "/" + parts[1]
			var num int
			fmt.Sscanf(parts[2], "%d", &num)
			ts.GitHubIssues.SetIssueState(repo, num, status)
		},
		Webhook: func(t *testing.T, ts *TestServer, id string, prevStatus, newStatus string) *http.Response {
			t.Helper()
			parts := strings.Split(id, "/")
			if len(parts) != 3 {
				t.Fatalf("invalid github issue id: %s", id)
			}
			repo := parts[0] + "/" + parts[1]
			var num int
			fmt.Sscanf(parts[2], "%d", &num)
			payload, sig := ts.GitHubIssues.BuildWebhookPayload(repo, num, prevStatus, newStatus)
			req, err := http.NewRequest("POST", ts.URL()+"/api/integrations/github-issues/webhook", strings.NewReader(string(payload)))
			if err != nil {
				t.Fatalf("github issues webhook request build failed: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Hub-Signature-256", sig)
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("delivery-%s-%d", repo, num))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("github issues webhook post failed: %v", err)
			}
			return resp
		},
		IssueID: "testorg/testrepo/42",
		Trigger: "open",
		Done:    "closed",
	},
}

// ParseStoryNum converts "sc-123" → 123.
func ParseStoryNum(id string) int64 {
	var n int64
	fmt.Sscanf(id, "sc-%d", &n)
	return n
}

// Scenario defines a test scenario that runs against all trackers.
type Scenario struct {
	Name string
	Fn   func(t *testing.T, td TrackerDispatcher, ts *TestServer)
}

// RunParityMatrix runs sc against every tracker in Trackers, creating the
// appropriate TestServer for each.
func RunParityMatrix(t *testing.T, sc Scenario) {
	for _, td := range Trackers {
		t.Run(td.Name+"/"+sc.Name, func(t *testing.T) {
			t.Helper()
			var ts *TestServer
			switch td.Name {
			case "shortcut":
				ts = NewTestServerWithShortcut(t)
				ts.Shortcut.SetStory(123, StoryState{
					Name:            "Test Story",
					WorkflowStateID: 5001, // Backlog
					Description:     "Test description",
				})
			case "github-issues":
				ts = NewTestServerWithGitHubIssues(t)
				ts.GitHubIssues.SetIssue("testorg/testrepo", 42, IssueState{
					Title:  "Test Issue",
					Body:   "Test body",
					State:  "open",
					Labels: []string{},
				})
				// Set the initial state in the mock so pre-flight checks succeed
				ts.GitHubIssues.SetIssueState("testorg/testrepo", 42, "open")
			default:
				ts = NewTestServer(t)
			}
			sc.Fn(t, td, ts)
		})
	}
}

// WaitForClawWithTracker waits for a claw to appear for the given tracker and
// issue ID, using the appropriate DB column.
func WaitForClawWithTracker(t *testing.T, ts *TestServer, td TrackerDispatcher, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var clawID string
		var err error
		switch td.Name {
		case "shortcut":
			err = ts.DB.QueryRow(`SELECT id FROM claws WHERE shortcut_story_id=?`, td.IssueID).Scan(&clawID)
		case "github-issues":
			err = ts.DB.QueryRow(`SELECT id FROM claws WHERE github_issue_id=?`, td.IssueID).Scan(&clawID)
		default:
			err = ts.DB.QueryRow(`SELECT id FROM claws WHERE linear_issue_id=?`, td.IssueID).Scan(&clawID)
		}
		if err == nil && clawID != "" {
			return clawID
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("WaitForClawWithTracker: no claw for %s/%s after %v", td.Name, td.IssueID, timeout)
	return ""
}

// CountClawsForTracker counts all claws (including deleted) for the given
// tracker and issue ID.
func CountClawsForTracker(t *testing.T, ts *TestServer, td TrackerDispatcher) int {
	t.Helper()
	var count int
	var err error
	switch td.Name {
	case "shortcut":
		err = ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE shortcut_story_id=?`, td.IssueID).Scan(&count)
	case "github-issues":
		err = ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE github_issue_id=?`, td.IssueID).Scan(&count)
	default:
		err = ts.DB.QueryRow(`SELECT COUNT(*) FROM claws WHERE linear_issue_id=?`, td.IssueID).Scan(&count)
	}
	if err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	return count
}
