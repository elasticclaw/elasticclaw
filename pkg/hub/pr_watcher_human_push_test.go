package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// mockGitHubHeadServer serves the two endpoints human code push detection
// hits: the PR (for its head SHA) and the head commit (for authorship).
// commitActor may be nil to simulate a commit with no linked GitHub account.
func mockGitHubHeadServer(t *testing.T, headSHA string, commitActor map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/21"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state": "open",
				"head":  map[string]interface{}{"sha": headSHA},
			})
		case strings.HasSuffix(r.URL.Path, "/commits/"+headSHA):
			body := map[string]interface{}{}
			if commitActor != nil {
				body["author"] = map[string]interface{}{
					"login": commitActor["login"],
					"type":  commitActor["type"],
				}
			}
			json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newHumanPushTestRun creates a server pointed at the mock GitHub API with a
// claw, a task run, and a tracked PR whose agent baseline is agentSHA.
func newHumanPushTestRun(t *testing.T, ghBase, clawID, agentSHA string) (*Server, *sql.DB, string, clawPR) {
	t.Helper()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, ghBase, "", "")
	if _, err := db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, status, created_at)
		VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "elasticclaw", "running", now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	runID, _ := startTaskRunForTest(t, s, clawID, clawID)
	prURL := "https://github.com/elastic/claw/pull/21"
	if err := s.associateTaskRunPR(TaskRunPR{
		TenantID:     "test-tenant-id",
		RunID:        runID,
		Repo:         "elastic/claw",
		PRNumber:     21,
		URL:          prURL,
		HeadSHA:      agentSHA,
		AgentHeadSHA: agentSHA != "",
		State:        taskRunPRStateOpen,
		OccurredAt:   now(),
	}); err != nil {
		t.Fatalf("associate PR: %v", err)
	}
	return s, db, runID, clawPR{clawID: clawID, repo: "elastic/claw", prNumber: 21, prURL: prURL}
}

func humanPushEventCount(t *testing.T, db *sql.DB, runID string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type='human_manual_code_push'`, runID).Scan(&count); err != nil {
		t.Fatalf("count human push events: %v", err)
	}
	return count
}

func lastAgentHeadSHA(t *testing.T, db *sql.DB, runID string) string {
	t.Helper()
	var sha string
	if err := db.QueryRow(`SELECT last_agent_head_sha FROM task_run_prs WHERE run_id=?`, runID).Scan(&sha); err != nil {
		t.Fatalf("read last_agent_head_sha: %v", err)
	}
	return sha
}

func TestCheckHumanCodePushRecordsHumanCommit(t *testing.T) {
	srv := mockGitHubHeadServer(t, "human-sha-2", map[string]string{"login": "alice", "type": "User"})
	s, db, runID, pr := newHumanPushTestRun(t, srv.URL, "claw-push-human", "agent-sha-1")

	s.checkPRMerged(pr, "test-token")

	if got := humanPushEventCount(t, db, runID); got != 1 {
		t.Fatalf("expected 1 human_manual_code_push event, got %d", got)
	}
	// The human push must not advance the agent baseline.
	if got := lastAgentHeadSHA(t, db, runID); got != "agent-sha-1" {
		t.Fatalf("expected baseline agent-sha-1, got %q", got)
	}
	var humanCount int
	var warnings string
	if err := db.QueryRow(`SELECT human_interaction_count, warning_types FROM task_run_summaries WHERE run_id=?`, runID).Scan(&humanCount, &warnings); err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if humanCount != 1 || !strings.Contains(warnings, "human_manual_code_push") {
		t.Fatalf("expected human interaction with human_manual_code_push warning, got count=%d warnings=%s", humanCount, warnings)
	}

	// A second poll of the same head SHA must not duplicate the event.
	s.checkPRMerged(pr, "test-token")
	if got := humanPushEventCount(t, db, runID); got != 1 {
		t.Fatalf("expected dedup to keep 1 event, got %d", got)
	}
}

func TestCheckHumanCodePushIgnoresAgentPushAndAdvancesBaseline(t *testing.T) {
	srv := mockGitHubHeadServer(t, "agent-sha-2", map[string]string{"login": "myclaw[bot]", "type": "Bot"})
	s, db, runID, pr := newHumanPushTestRun(t, srv.URL, "claw-push-agent", "agent-sha-1")

	s.checkPRMerged(pr, "test-token")

	if got := humanPushEventCount(t, db, runID); got != 0 {
		t.Fatalf("expected no human_manual_code_push event, got %d", got)
	}
	if got := lastAgentHeadSHA(t, db, runID); got != "agent-sha-2" {
		t.Fatalf("expected baseline advanced to agent-sha-2, got %q", got)
	}
}

func TestCheckHumanCodePushAdoptsHeadWhenBaselineMissing(t *testing.T) {
	srv := mockGitHubHeadServer(t, "some-sha", map[string]string{"login": "alice", "type": "User"})
	s, db, runID, pr := newHumanPushTestRun(t, srv.URL, "claw-push-nobaseline", "")

	s.checkPRMerged(pr, "test-token")

	// Without a baseline the head is adopted as the agent's SHA — no event,
	// even though the head commit is human-authored (pre-feature rows).
	if got := humanPushEventCount(t, db, runID); got != 0 {
		t.Fatalf("expected no human_manual_code_push event, got %d", got)
	}
	if got := lastAgentHeadSHA(t, db, runID); got != "some-sha" {
		t.Fatalf("expected baseline adopted as some-sha, got %q", got)
	}
}

func TestCheckHumanCodePushTreatsUnlinkedCommitAsAgent(t *testing.T) {
	srv := mockGitHubHeadServer(t, "unlinked-sha", nil)
	s, db, runID, pr := newHumanPushTestRun(t, srv.URL, "claw-push-unlinked", "agent-sha-1")

	s.checkPRMerged(pr, "test-token")

	if got := humanPushEventCount(t, db, runID); got != 0 {
		t.Fatalf("expected no human_manual_code_push event, got %d", got)
	}
	if got := lastAgentHeadSHA(t, db, runID); got != "unlinked-sha" {
		t.Fatalf("expected baseline advanced to unlinked-sha, got %q", got)
	}
}
