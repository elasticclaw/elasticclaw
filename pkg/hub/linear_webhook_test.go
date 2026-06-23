package hub_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/hub/factorytest"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestLinearWorkflowCreateFailureCommentsMovesAndAssignsTriggerActor(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	linear := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{ClawToken: "test-claw-token"}
	s, db := hub.NewTestServerWithConfig(t, cfg, "", linear.URL, "")
	saveLinearIssueWorkflowFixtureWithProviderAndErrorStatus(t, "workspace-a", "missing-provider", "Agent Error")
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "linear", "default", "test-linear-token", "")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	payload := linearWebhookPayloadWithActor(t, linear, "ELA-123", "Backlog", "Todo", map[string]interface{}{
		"id":   "user-123",
		"type": "user",
		"name": "Marc Developer",
		"url":  "https://linear.app/test/profiles/user-123",
	})
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/api/workspaces/workspace-a/webhooks/linear", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	comment := waitForLinearIssuePostedComment(t, linear, "ELA-123")
	for _, want := range []string{
		"Marc Developer",
		"ElasticClaw could not finish this implementation.",
		"Status code: 500",
		"ElasticClaw could not find a valid execution provider.",
		"Check the ElasticClaw workspace/workflow configuration, then re-trigger the workflow.",
	} {
		if !strings.Contains(comment, want) {
			t.Fatalf("comment missing %q:\n%s", want, comment)
		}
	}
	if strings.Contains(comment, "missing-provider") {
		t.Fatalf("comment exposed raw provider error:\n%s", comment)
	}
	waitForLinearIssueState(t, linear, "ELA-123", "Agent Error")
	waitForLinearIssueAssignee(t, linear, "ELA-123", "user-123")
	var claws int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE linear_issue_id='ELA-123'`).Scan(&claws); err != nil {
		t.Fatalf("count claws: %v", err)
	}
	if claws != 0 {
		t.Fatalf("created %d claws, want 0 for creation failure", claws)
	}
}

func TestLinearWorkflowCreateFailureDoesNotReprocessErrorStatusWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	linear := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{ClawToken: "test-claw-token"}
	s, _ := hub.NewTestServerWithConfig(t, cfg, "", linear.URL, "")
	saveLinearIssueWorkflowFixtureWithProviderAndErrorStatus(t, "workspace-a", "missing-provider", "Agent Error")
	hub.SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "linear", "default", "test-linear-token", "")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	postLinearWebhook(t, httpSrv.URL, "workspace-a", linearWebhookPayloadWithActor(t, linear, "ELA-123", "Backlog", "Todo", map[string]interface{}{
		"id":   "user-123",
		"type": "user",
		"name": "Marc Developer",
		"url":  "https://linear.app/test/profiles/user-123",
	}))
	waitForLinearIssueState(t, linear, "ELA-123", "Agent Error")

	postLinearWebhook(t, httpSrv.URL, "workspace-a", linearWebhookPayloadWithActor(t, linear, "ELA-123", "Todo", "Agent Error", map[string]interface{}{
		"id":   "user-123",
		"type": "user",
		"name": "Marc Developer",
		"url":  "https://linear.app/test/profiles/user-123",
	}))

	assertLinearIssueCommentCountStable(t, linear, "ELA-123", 1)
}

func TestLinearWorkflowExcludeLabelsRoutesGenericAndBugWorkflows(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	linear := factorytest.NewMockLinear(t)

	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{
			"noop": {Type: "noop"},
		},
	}
	s, db := hub.NewTestServerWithConfig(t, cfg, "", linear.URL, "")
	saveLinearRoutingWorkflowFixture(t, "workspace-a")

	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	postLinearWebhook(t, httpSrv.URL, "workspace-a", linearWebhookPayloadWithLabels(t, linear, "ELA-124", "Backlog", "Ready For Agent", nil))
	waitForLinearClawCount(t, db, "ELA-124", 1)

	postLinearWebhook(t, httpSrv.URL, "workspace-a", linearWebhookPayloadWithLabels(t, linear, "ELA-125", "Backlog", "Ready For Agent", []string{"Bug"}))
	waitForLinearClawCount(t, db, "ELA-125", 1)
	assertLinearClawTagsContain(t, db, "ELA-125", "workflow:bug-workflow")
}

func saveLinearRoutingWorkflowFixture(t *testing.T, workspace string) {
	t.Helper()
	hub.SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          workspace,
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: " + workspace + "\nprovider: noop\n",
				"CONTEXT.md":              "Test context\n",
			},
		},
		[]*types.WorkflowConfig{
			{
				SchemaVersion: "v1",
				Name:          "generic-workflow",
				Trigger: &types.WorkflowTrigger{
					Linear: &types.LinearWorkflowTrigger{
						Event:         "status_changed",
						Team:          "ELA",
						States:        []string{"Ready For Agent"},
						ExcludeLabels: []string{"Bug"},
					},
				},
				Stages: workflowPollTestStages(),
			},
			{
				SchemaVersion: "v1",
				Name:          "bug-workflow",
				Trigger: &types.WorkflowTrigger{
					Linear: &types.LinearWorkflowTrigger{
						Event:  "status_changed",
						Team:   "ELA",
						States: []string{"Ready For Agent"},
						Labels: []string{"Bug"},
					},
				},
				Stages: workflowPollTestStages(),
			},
		},
	)
}

func linearWebhookPayloadWithLabels(t *testing.T, linear *factorytest.MockLinear, issueID, prevStatus, newStatus string, labels []string) []byte {
	t.Helper()
	payload, _ := linear.BuildWebhookPayload(issueID, prevStatus, newStatus)
	var data map[string]interface{}
	if err := json.Unmarshal(payload, &data); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	rawLabels := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		rawLabels = append(rawLabels, map[string]string{"name": label})
	}
	dataMap := data["data"].(map[string]interface{})
	dataMap["labels"] = rawLabels
	out, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return out
}

func waitForLinearClawCount(t *testing.T, db interface {
	QueryRow(string, ...interface{}) *sql.Row
}, issueID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM claws WHERE linear_issue_id=?`, issueID).Scan(&count); err != nil {
			t.Fatalf("count claws: %v", err)
		}
		if count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("claws for %s = %d, want %d", issueID, count, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertLinearClawTagsContain(t *testing.T, db interface {
	QueryRow(string, ...interface{}) *sql.Row
}, issueID, want string) {
	t.Helper()
	var tags string
	if err := db.QueryRow(`SELECT tags FROM claws WHERE linear_issue_id=? LIMIT 1`, issueID).Scan(&tags); err != nil {
		t.Fatalf("query claw tags: %v", err)
	}
	if !strings.Contains(tags, want) {
		t.Fatalf("tags for %s = %s, want %q", issueID, tags, want)
	}
}

func saveLinearIssueWorkflowFixtureWithProviderAndErrorStatus(t *testing.T, workspace, provider, agentStatusError string) {
	t.Helper()
	hub.SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          workspace,
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: " + workspace + "\nprovider: " + provider + "\n",
				"CONTEXT.md":              "Test context\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion: "v1",
			Name:          "linear-workflow",
			Trigger: &types.WorkflowTrigger{
				Linear: &types.LinearWorkflowTrigger{
					Event:            "status_changed",
					Team:             "ELA",
					States:           []string{"Todo"},
					AgentStatusError: agentStatusError,
				},
			},
			Stages: []types.WorkflowStage{{
				ID:    "working",
				Label: "Working",
				Entry: true,
				OnEnter: map[string]interface{}{
					"inject": "Read your CONTEXT.md and start working on the issue.\n",
				},
			}},
		}},
	)
}

func linearWebhookPayloadWithActor(t *testing.T, linear *factorytest.MockLinear, issueID, prevStatus, newStatus string, actor map[string]interface{}) []byte {
	t.Helper()
	payload, _ := linear.BuildWebhookPayload(issueID, prevStatus, newStatus)
	var data map[string]interface{}
	if err := json.Unmarshal(payload, &data); err != nil {
		t.Fatalf("parse payload: %v", err)
	}
	data["actor"] = actor
	out, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return out
}

func postLinearWebhook(t *testing.T, serverURL, workspace string, payload []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+"/api/workspaces/"+workspace+"/webhooks/linear", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func waitForLinearIssuePostedComment(t *testing.T, linear *factorytest.MockLinear, issueID string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		comments := linear.Comments(issueID)
		if len(comments) > 0 {
			return comments[len(comments)-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Linear issue comment")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertLinearIssueCommentCountStable(t *testing.T, linear *factorytest.MockLinear, issueID string, want int) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		comments := linear.Comments(issueID)
		if len(comments) != want {
			t.Fatalf("posted %d failure comments, want %d: %#v", len(comments), want, comments)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForLinearIssueState(t *testing.T, linear *factorytest.MockLinear, issueID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := linear.IssueStateName(issueID); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Linear state = %q, want %q", linear.IssueStateName(issueID), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForLinearIssueAssignee(t *testing.T, linear *factorytest.MockLinear, issueID, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := linear.IssueAssigneeID(issueID); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Linear assignee = %q, want %q", linear.IssueAssigneeID(issueID), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
