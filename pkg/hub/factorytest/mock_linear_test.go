package factorytest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestMockLinearRecordsMutationsByIssueIdentifier(t *testing.T) {
	linear := NewMockLinear(t)
	linear.PollingIssues["ELA-456"] = map[string]interface{}{
		"id":          "issue-uuid-456",
		"identifier":  "ELA-456",
		"title":       "Second issue",
		"description": "Body",
		"url":         "https://linear.app/test/issue/ELA-456",
		"state":       map[string]interface{}{"name": "Backlog"},
		"team": map[string]interface{}{
			"name": "Engineering",
			"key":  "ELA",
			"states": map[string]interface{}{
				"nodes": []map[string]interface{}{
					{"id": "agent-error-id", "name": "Agent Error", "type": "canceled"},
				},
			},
		},
	}

	postMockLinearGraphQL(t, linear.URL, map[string]interface{}{
		"query": "mutation($issueId: String!, $body: String!) { commentCreate(input: { issueId: $issueId, body: $body }) { success } }",
		"variables": map[string]string{
			"issueId": "issue-uuid-456",
			"body":    "failure comment",
		},
	})
	postMockLinearGraphQL(t, linear.URL, map[string]interface{}{
		"query": "mutation($id: String!, $assigneeId: String!) { issueUpdate(id: $id, input: { assigneeId: $assigneeId }) { success } }",
		"variables": map[string]string{
			"id":         "issue-uuid-456",
			"assigneeId": "user-456",
		},
	})

	if comments := linear.Comments("ELA-456"); len(comments) != 1 || comments[0] != "failure comment" {
		t.Fatalf("Comments(ELA-456) = %v, want failure comment", comments)
	}
	if got := linear.IssueAssigneeID("ELA-456"); got != "user-456" {
		t.Fatalf("IssueAssigneeID(ELA-456) = %q, want user-456", got)
	}
	if comments := linear.Comments("ELA-123"); len(comments) != 0 {
		t.Fatalf("Comments(ELA-123) = %v, want none", comments)
	}
}

func postMockLinearGraphQL(t *testing.T, baseURL string, payload map[string]interface{}) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	resp, err := http.Post(baseURL+"/graphql", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post graphql: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("status = %d, want <400", resp.StatusCode)
	}
}
