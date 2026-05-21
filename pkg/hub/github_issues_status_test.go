package hub_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
)

func TestGitHubIssuesFactoryKeepsProvisioningStatusAfterProviderCreate(t *testing.T) {
	ts := factorytest.NewTestServerWithGitHubIssues(t)
	ts.GitHubIssues.SetIssue("testorg/testrepo", 42, factorytest.IssueState{
		Title:  "Test Issue",
		Body:   "Test body",
		State:  "open",
		Labels: []string{},
	})

	payload, sig := ts.GitHubIssues.BuildWebhookPayload("testorg/testrepo", 42, "closed", "open")
	req, err := http.NewRequest("POST", ts.URL()+"/api/integrations/github-issues/webhook", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("github issues webhook request build failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Delivery", "delivery-testorg/testrepo-42")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("github issues webhook post failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook returned status %d", resp.StatusCode)
	}

	td := factorytest.TrackerDispatcher{Name: "github-issues", IssueID: "testorg/testrepo/42"}
	clawID := factorytest.WaitForClawWithTracker(t, ts, td, 5*time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		if err := ts.DB.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
			t.Fatalf("load claw status: %v", err)
		}
		if status == "online" {
			t.Fatalf("GitHub Issues factory marked claw %s online before bridge/bootstrap", clawID[:8])
		}
		time.Sleep(50 * time.Millisecond)
	}

	var status string
	if err := ts.DB.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatalf("load claw status: %v", err)
	}
	if status != "provisioning" {
		t.Fatalf("status = %q, want provisioning", status)
	}
}
