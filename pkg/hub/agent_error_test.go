package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type agentErrorGitHubMock struct {
	*httptest.Server
	mu        sync.Mutex
	assignees []string
	labels    []string
	comments  []string
}

func newAgentErrorGitHubMock(t *testing.T, assignees []string) *agentErrorGitHubMock {
	t.Helper()
	m := &agentErrorGitHubMock{assignees: append([]string(nil), assignees...)}
	m.Server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.Close)
	return m
}

func (m *agentErrorGitHubMock) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/repos/testorg/testrepo/issues/42" &&
		r.URL.Path != "/repos/testorg/testrepo/issues/42/labels" &&
		r.URL.Path != "/repos/testorg/testrepo/issues/42/comments" &&
		r.URL.Path != "/repos/testorg/testrepo/issues/42/assignees" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/testorg/testrepo/issues/42":
		assignees := make([]map[string]string, 0, len(m.assignees))
		for _, login := range m.assignees {
			assignees = append(assignees, map[string]string{"login": login})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"number":    42,
			"title":     "Test issue",
			"state":     "open",
			"assignees": assignees,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/testorg/testrepo/issues/42/labels":
		var body struct {
			Labels []string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.labels = appendMissingStrings(m.labels, body.Labels...)
		_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "agent-error"}})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/testorg/testrepo/issues/42/comments":
		var body struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.comments = append(m.comments, body.Body)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": len(m.comments)})
	case r.Method == http.MethodPost && r.URL.Path == "/repos/testorg/testrepo/issues/42/assignees":
		var body struct {
			Assignees []string `json:"assignees"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.assignees = appendMissingStrings(m.assignees, body.Assignees...)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"assignees": m.assignees})
	case r.Method == http.MethodDelete && r.URL.Path == "/repos/testorg/testrepo/issues/42/assignees":
		var body struct {
			Assignees []string `json:"assignees"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		for _, remove := range body.Assignees {
			m.assignees = removeString(m.assignees, remove)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"assignees": m.assignees})
	default:
		http.Error(w, "unexpected method/path", http.StatusInternalServerError)
	}
}

func (m *agentErrorGitHubMock) snapshot() (assignees, labels, comments []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.assignees...), append([]string(nil), m.labels...), append([]string(nil), m.comments...)
}

func TestGitHubAgentErrorLabelsCommentsAndLeavesOriginalAssignee(t *testing.T) {
	github := newAgentErrorGitHubMock(t, []string{"ana"})
	s, db := NewTestServerWithConfig(t, agentErrorGitHubConfig(), github.URL, "", "")

	insertGitHubAgentErrorClaw(t, db, "claw-agent-error-original")
	insertAgentErrorTrackerContext(t, db, "claw-agent-error-original", []trackerOwner{{Login: "ana", Mention: "@ana"}})

	s.stopAgentWithReason("claw-agent-error-original", "Bootstrap failed: missing CONTEXT.md", true)

	waitForAgentErrorComment(t, github, "already assigned to an original owner")
	assignees, labels, comments := github.snapshot()
	if !containsString(labels, "agent-error") {
		t.Fatalf("labels = %v, want agent-error", labels)
	}
	if fmt.Sprint(assignees) != "[ana]" {
		t.Fatalf("assignees = %v, want [ana]", assignees)
	}
	if !strings.Contains(comments[len(comments)-1], "Original owner: @ana") {
		t.Fatalf("comment did not mention original owner:\n%s", comments[len(comments)-1])
	}
	if !strings.Contains(comments[len(comments)-1], "Bootstrap failed: missing CONTEXT.md") {
		t.Fatalf("comment did not include sanitized reason:\n%s", comments[len(comments)-1])
	}
}

func TestGitHubAgentErrorReassignsEmptyIssueToOriginalAssignee(t *testing.T) {
	github := newAgentErrorGitHubMock(t, nil)
	s, db := NewTestServerWithConfig(t, agentErrorGitHubConfig(), github.URL, "", "")

	insertGitHubAgentErrorClaw(t, db, "claw-agent-error-empty")
	insertAgentErrorTrackerContext(t, db, "claw-agent-error-empty", []trackerOwner{{Login: "ana", Mention: "@ana"}})

	s.stopAgentWithReason("claw-agent-error-empty", "Provisioning failed: provider returned 503", true)

	waitForAgentErrorComment(t, github, "Reassigned this issue back to @ana.")
	assignees, labels, comments := github.snapshot()
	if !containsString(labels, "agent-error") {
		t.Fatalf("labels = %v, want agent-error", labels)
	}
	if fmt.Sprint(assignees) != "[ana]" {
		t.Fatalf("assignees = %v, want [ana]", assignees)
	}
	if !strings.Contains(comments[len(comments)-1], "Provisioning failed: provider returned 503") {
		t.Fatalf("comment did not include sanitized reason:\n%s", comments[len(comments)-1])
	}
}

func agentErrorGitHubConfig() *types.HubConfig {
	return &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{{
			Name:        "github-agent-error",
			Integration: "github-issues",
			Workspace:   "test-workspace",
			Template:    "elasticclaw",
			Provider:    "noop",
		}},
		Integrations: &types.IntegrationsConfig{
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{
				Workspace: "test-workspace",
				Token:     "test-github-token",
			}},
		},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}
}

func insertGitHubAgentErrorClaw(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, factory_name, github_issue_id, tags, created_at)
		 VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test claw", "elasticclaw", "connected", "github-agent-error", "testorg/testrepo/42", `["factory:github-agent-error"]`,
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}
}

func insertAgentErrorTrackerContext(t *testing.T, db *sql.DB, clawID string, owners []trackerOwner) {
	t.Helper()
	ownersJSON, _ := json.Marshal(owners)
	_, err := db.Exec(
		`INSERT INTO claw_tracker_contexts(claw_id, integration, issue_id, original_owners, agent_owners, created_at)
		 VALUES(?,?,?,?,?,datetime('now'))`,
		clawID, "github-issues", "testorg/testrepo/42", string(ownersJSON), "[]",
	)
	if err != nil {
		t.Fatalf("insert tracker context: %v", err)
	}
}

func waitForAgentErrorComment(t *testing.T, github *agentErrorGitHubMock, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, comments := github.snapshot()
		if len(comments) > 0 && strings.Contains(comments[len(comments)-1], want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, comments := github.snapshot()
	t.Fatalf("timed out waiting for comment containing %q; comments=%v", want, comments)
}

func appendMissingStrings(items []string, add ...string) []string {
	for _, item := range add {
		if !containsString(items, item) {
			items = append(items, item)
		}
	}
	return items
}

func removeString(items []string, remove string) []string {
	out := items[:0]
	for _, item := range items {
		if item != remove {
			out = append(out, item)
		}
	}
	return out
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
