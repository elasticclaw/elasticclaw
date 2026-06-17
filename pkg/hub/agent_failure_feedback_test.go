package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestStopAgentWithReasonWorkflowGitHubFailureFeedbackUsesPersistedTriggerActor(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	ghi := newStopFeedbackGitHubMock(t)

	cfg := &types.HubConfig{ClawToken: "test-claw-token"}
	s, db := NewTestServerWithConfig(t, cfg, ghi.URL, "", "")
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "workspace-a",
			Files: map[string]string{
				"elasticclaw-config.yaml": "schema_version: v1\nname: workspace-a\nprovider: noop\n",
				"CONTEXT.md":              "Test context\n",
			},
		},
		[]*types.WorkflowConfig{{
			SchemaVersion: "v1",
			Name:          "test-workflow",
			Trigger: &types.WorkflowTrigger{
				GitHubIssues: &types.GitHubIssuesWorkflowTrigger{
					Event:            "issue_reopened",
					Repositories:     []string{"testorg/testrepo"},
					States:           []string{"open"},
					AgentStatusError: "agent-error",
				},
			},
			Stages: []types.WorkflowStage{{
				ID:    "working",
				Entry: true,
			}},
		}},
	)
	SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "github-issues", "default", "test-github-issues-token", "")
	ghi.setIssue("testorg/testrepo", 42)

	_, err := db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, provider, status, tags, github_issue_id, trigger_actor_json, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,datetime('now'))`,
		"claw-github-feedback", "test-tenant-id", "testorg/testrepo/42", "workspace-a", "noop", "connected",
		`["workspace:workspace-a","workflow:test-workflow"]`, "testorg/testrepo/42", `{"login":"alice","type":"User"}`,
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	s.stopAgentWithReason("claw-github-feedback", "Bootstrap failed: HTTP 502", true)

	comment := waitForMockGitHubIssueComment(t, ghi, "testorg/testrepo", 42)
	for _, want := range []string{
		"@alice ElasticClaw could not finish this implementation.",
		"Status code: 502",
		"ElasticClaw started the workspace but could not finish preparing it.",
	} {
		if !strings.Contains(comment, want) {
			t.Fatalf("comment missing %q:\n%s", want, comment)
		}
	}
	waitForMockGitHubIssueAssignee(t, ghi, "testorg/testrepo", 42, "alice")
	waitForMockGitHubIssueLabel(t, ghi, "testorg/testrepo", 42, "agent-error")
}

func TestBuildAgentFailureFeedbackCommentWithoutTriggerActor(t *testing.T) {
	comment := buildAgentFailureFeedbackComment(agentFailureFeedback{
		Integration: "github-issues",
		Failure:     classifyAgentFailure("Bootstrap failed: HTTP 502"),
	})
	if strings.Contains(comment, "ElasticClaw ElasticClaw") {
		t.Fatalf("comment duplicated product name:\n%s", comment)
	}
	if strings.Contains(comment, "assigned it back to you") {
		t.Fatalf("comment claimed reassignment without an actor:\n%s", comment)
	}
	if !strings.Contains(comment, "Please review the workflow setup before retrying.") {
		t.Fatalf("comment missing no-actor action text:\n%s", comment)
	}
}

func TestBuildAgentFailureFeedbackCommentDoesNotClaimBotAssignment(t *testing.T) {
	comment := buildAgentFailureFeedbackComment(agentFailureFeedback{
		Integration:      "github-issues",
		TriggerActor:     triggerActor{Login: "elasticclaw-app[bot]", Type: "Bot"},
		AgentStatusError: "agent-error",
		Failure:          classifyAgentFailure("Bootstrap failed: HTTP 502"),
	})
	if strings.Contains(comment, "assigned it back to you") || strings.Contains(comment, "assigned this issue back to you") {
		t.Fatalf("comment claimed bot reassignment:\n%s", comment)
	}
	if !strings.Contains(comment, "I marked this issue for review") {
		t.Fatalf("comment missing status-only action text:\n%s", comment)
	}
}

func TestCommentWorkflowAgentStopToTrackerHandlesMissingTriggerConfig(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	ghi := newStopFeedbackGitHubMock(t)

	cfg := &types.HubConfig{ClawToken: "test-claw-token"}
	s, _ := NewTestServerWithConfig(t, cfg, ghi.URL, "", "")
	SaveWorkspaceForTest(t,
		&types.WorkspaceConfig{
			SchemaVersion: "v1",
			Name:          "workspace-a",
			Files:         map[string]string{"elasticclaw-config.yaml": "schema_version: v1\nname: workspace-a\n"},
		},
		nil,
	)
	SaveWorkspaceIssueTrackerForTest(t, "workspace-a", "github-issues", "default", "test-github-issues-token", "")

	s.commentWorkflowAgentStopToTracker("claw-missing-trigger", pipelineContext{
		Workspace: &types.WorkspaceConfig{Name: "workspace-a"},
		Workflow:  &types.WorkflowConfig{Name: "test-workflow", Integration: "github-issues"},
		IssueID:   "testorg/testrepo/42",
	}, "Bootstrap failed: HTTP 502")

	comment := waitForMockGitHubIssueComment(t, ghi, "testorg/testrepo", 42)
	if !strings.Contains(comment, "ElasticClaw could not finish this implementation.") {
		t.Fatalf("comment missing failure text:\n%s", comment)
	}
}

func TestAssignLinearIssueReportsGraphQLErrors(t *testing.T) {
	srv := newAssignLinearIssueMock(t, func(w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"errors": []map[string]string{{"message": "permission denied"}},
		})
	})

	err := assignLinearIssueWithBase(srv.URL, "test-token", "ELA-123", "user-123")
	if err == nil || !strings.Contains(err.Error(), "GraphQL error: permission denied") {
		t.Fatalf("assignLinearIssueWithBase error = %v, want GraphQL error", err)
	}
}

func TestAssignLinearIssueReportsSuccessFalse(t *testing.T) {
	srv := newAssignLinearIssueMock(t, func(w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"issueUpdate": map[string]interface{}{"success": false},
			},
		})
	})

	err := assignLinearIssueWithBase(srv.URL, "test-token", "ELA-123", "user-123")
	if err == nil || !strings.Contains(err.Error(), "issueUpdate returned success=false") {
		t.Fatalf("assignLinearIssueWithBase error = %v, want success=false error", err)
	}
}

func newAssignLinearIssueMock(t *testing.T, mutationResponse func(http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req.Query, "issueUpdate") {
			mutationResponse(w)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"issue": map[string]interface{}{"id": "issue-uuid-123"},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type stopFeedbackGitHubMock struct {
	*httptest.Server
	mu       sync.Mutex
	comments map[string][]string
	labels   map[string][]string
	assignee map[string]string
}

func newStopFeedbackGitHubMock(t *testing.T) *stopFeedbackGitHubMock {
	t.Helper()
	m := &stopFeedbackGitHubMock{
		comments: map[string][]string{},
		labels:   map[string][]string{},
		assignee: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		if len(parts) < 4 || parts[2] != "issues" {
			http.NotFound(w, r)
			return
		}
		repo := parts[0] + "/" + parts[1]
		var number int
		if _, err := fmt.Sscanf(parts[3], "%d", &number); err != nil {
			http.Error(w, "bad issue number", http.StatusBadRequest)
			return
		}
		key := fmt.Sprintf("%s#%d", repo, number)
		switch {
		case r.Method == http.MethodGet && len(parts) == 4:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":   number,
				"title":    "Test Issue",
				"body":     "Body",
				"html_url": fmt.Sprintf("https://github.com/%s/issues/%d", repo, number),
				"state":    "open",
				"labels":   []map[string]string{},
				"user":     map[string]string{"login": "alice"},
			})
		case r.Method == http.MethodPost && len(parts) == 5 && parts[4] == "comments":
			var req struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			m.mu.Lock()
			m.comments[key] = append(m.comments[key], req.Body)
			m.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{"body": req.Body})
		case r.Method == http.MethodPost && len(parts) == 5 && parts[4] == "labels":
			var req struct {
				Labels []string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			m.mu.Lock()
			m.labels[key] = append(m.labels[key], req.Labels...)
			m.mu.Unlock()
			json.NewEncoder(w).Encode(req.Labels)
		case r.Method == http.MethodPatch && len(parts) == 4:
			var req struct {
				Assignees []string `json:"assignees"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			m.mu.Lock()
			if len(req.Assignees) > 0 {
				m.assignee[key] = req.Assignees[0]
			}
			m.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{"number": number})
		default:
			http.NotFound(w, r)
		}
	})
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Close)
	return m
}

func (m *stopFeedbackGitHubMock) setIssue(_ string, _ int) {}

func waitForMockGitHubIssueComment(t *testing.T, ghi *stopFeedbackGitHubMock, repo string, number int) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		comments := ghi.commentsFor(repo, number)
		if len(comments) > 0 {
			return comments[len(comments)-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for GitHub issue comment")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForMockGitHubIssueAssignee(t *testing.T, ghi *stopFeedbackGitHubMock, repo string, number int, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := ghi.assigneeFor(repo, number); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("assignee = %q, want %q", ghi.assigneeFor(repo, number), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForMockGitHubIssueLabel(t *testing.T, ghi *stopFeedbackGitHubMock, repo string, number int, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if containsMockString(ghi.labelsFor(repo, number), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("labels = %v, want %q", ghi.labelsFor(repo, number), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (m *stopFeedbackGitHubMock) commentsFor(repo string, number int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.comments[fmt.Sprintf("%s#%d", repo, number)]...)
}

func (m *stopFeedbackGitHubMock) labelsFor(repo string, number int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.labels[fmt.Sprintf("%s#%d", repo, number)]...)
}

func (m *stopFeedbackGitHubMock) assigneeFor(repo string, number int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.assignee[fmt.Sprintf("%s#%d", repo, number)]
}

func containsMockString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
