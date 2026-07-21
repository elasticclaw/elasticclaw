package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// mockGitHubCommitServer serves the PR head endpoint plus per-SHA commit
// bodies, so tests can model authorship and parents (base merges).
func mockGitHubCommitServer(t *testing.T, headSHA string, commits map[string]map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/21") {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state": "open",
				"head":  map[string]interface{}{"sha": headSHA},
			})
			return
		}
		for sha, body := range commits {
			if strings.HasSuffix(r.URL.Path, "/commits/"+sha) {
				json.NewEncoder(w).Encode(body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func commitBody(login, userType string, parents ...string) map[string]interface{} {
	body := map[string]interface{}{}
	if login != "" {
		body["author"] = map[string]interface{}{"login": login, "type": userType}
	}
	parentObjs := make([]interface{}, 0, len(parents))
	for _, sha := range parents {
		parentObjs = append(parentObjs, map[string]interface{}{"sha": sha})
	}
	body["parents"] = parentObjs
	return body
}

// newWebhookHumanPushTestRun builds a server with a pull_request factory, an
// existing claw tracking PR elastic/claw#21, and a task run whose tracked PR
// has agentSHA as the agent baseline.
func newWebhookHumanPushTestRun(t *testing.T, ghBase, clawID, agentSHA string) (*Server, *sql.DB, string, *types.FactoryConfig) {
	t.Helper()
	factory := &types.FactoryConfig{
		Name:        "pr-factory",
		Integration: "github",
		Template:    "elasticclaw",
		Provider:    "noop",
		Repos:       []string{"elastic/claw"},
		Trigger:     &types.GitHubTrigger{On: "pull_request"},
	}
	cfg := &types.HubConfig{
		ClawToken: "test-claw-token",
		Factories: []*types.FactoryConfig{factory},
		Integrations: &types.IntegrationsConfig{
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{Workspace: "default", Token: "test-github-token"}},
		},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}
	s, db := NewTestServerWithConfig(t, cfg, ghBase, "", "")
	prURL := "https://github.com/elastic/claw/pull/21"
	if _, err := db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, status, created_at)
		VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "elasticclaw", "running", now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, created_at)
		VALUES(?,?,?,?,?,?)`, clawID+"-pr", clawID, "elastic/claw", 21, prURL, now()); err != nil {
		t.Fatalf("insert claw PR: %v", err)
	}
	runID, _ := startTaskRunForTest(t, s, clawID, clawID)
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
	return s, db, runID, factory
}

func synchronizePayload(headSHA, senderLogin, senderType string) githubPRPayload {
	payload := githubPRPayload{Action: "synchronize", Number: 21}
	payload.Repository.FullName = "elastic/claw"
	payload.PullRequest.HTMLURL = "https://github.com/elastic/claw/pull/21"
	payload.PullRequest.Head.SHA = headSHA
	payload.Sender.Login = senderLogin
	payload.Sender.Type = senderType
	return payload
}

func TestGitHubWebhookSynchronizeRecordsHumanPush(t *testing.T) {
	srv := mockGitHubCommitServer(t, "human-sha-2", map[string]map[string]interface{}{
		"human-sha-2": commitBody("alice", "User", "agent-sha-1"),
	})
	s, db, runID, _ := newWebhookHumanPushTestRun(t, srv.URL, "claw-wh-human", "agent-sha-1")

	s.processGitHubPREvent(synchronizePayload("human-sha-2", "alice", "User"))

	if got := humanPushEventCount(t, db, runID); got != 1 {
		t.Fatalf("expected 1 human_manual_code_push event, got %d", got)
	}
	if got := lastAgentHeadSHA(t, db, runID); got != "agent-sha-1" {
		t.Fatalf("expected baseline agent-sha-1, got %q", got)
	}

	// A replayed webhook for the same head SHA must not duplicate the event.
	s.processGitHubPREvent(synchronizePayload("human-sha-2", "alice", "User"))
	if got := humanPushEventCount(t, db, runID); got != 1 {
		t.Fatalf("expected dedup to keep 1 event, got %d", got)
	}
}

func TestGitHubWebhookSynchronizeIgnoresBaseMerge(t *testing.T) {
	// GitHub's "Update branch" button: the clicking human is the sender and
	// commit author, but the commit is a merge of base into the agent's head.
	srv := mockGitHubCommitServer(t, "merge-sha", map[string]map[string]interface{}{
		"merge-sha": commitBody("alice", "User", "agent-sha-1", "base-sha"),
	})
	s, db, runID, _ := newWebhookHumanPushTestRun(t, srv.URL, "claw-wh-merge", "agent-sha-1")

	s.processGitHubPREvent(synchronizePayload("merge-sha", "alice", "User"))

	if got := humanPushEventCount(t, db, runID); got != 0 {
		t.Fatalf("expected no human_manual_code_push event for base merge, got %d", got)
	}
	if got := lastAgentHeadSHA(t, db, runID); got != "merge-sha" {
		t.Fatalf("expected baseline advanced to merge-sha, got %q", got)
	}
}

func TestGitHubWebhookSynchronizeIgnoresAgentPush(t *testing.T) {
	srv := mockGitHubCommitServer(t, "agent-sha-2", map[string]map[string]interface{}{
		"agent-sha-2": commitBody("myclaw[bot]", "Bot", "agent-sha-1"),
	})
	s, db, runID, _ := newWebhookHumanPushTestRun(t, srv.URL, "claw-wh-agent", "agent-sha-1")

	s.processGitHubPREvent(synchronizePayload("agent-sha-2", "myclaw[bot]", "Bot"))

	if got := humanPushEventCount(t, db, runID); got != 0 {
		t.Fatalf("expected no human_manual_code_push event for agent push, got %d", got)
	}
	if got := lastAgentHeadSHA(t, db, runID); got != "agent-sha-2" {
		t.Fatalf("expected baseline advanced to agent-sha-2, got %q", got)
	}
}

func TestGitHubWebhookAndPollerDedupHumanPush(t *testing.T) {
	srv := mockGitHubCommitServer(t, "human-sha-2", map[string]map[string]interface{}{
		"human-sha-2": commitBody("alice", "User", "agent-sha-1"),
	})
	s, db, runID, _ := newWebhookHumanPushTestRun(t, srv.URL, "claw-wh-dedup", "agent-sha-1")

	s.processGitHubPREvent(synchronizePayload("human-sha-2", "alice", "User"))
	// The poller sees the same head SHA afterwards and must not double-count.
	s.checkPRMerged(clawPR{
		clawID:   "claw-wh-dedup",
		repo:     "elastic/claw",
		prNumber: 21,
		prURL:    "https://github.com/elastic/claw/pull/21",
	}, "test-token")

	if got := humanPushEventCount(t, db, runID); got != 1 {
		t.Fatalf("expected 1 human_manual_code_push event across webhook and poller, got %d", got)
	}
}

func closedPRPayload(merged bool) githubPRPayload {
	payload := githubPRPayload{Action: "closed", Number: 21}
	payload.Repository.FullName = "elastic/claw"
	payload.PullRequest.HTMLURL = "https://github.com/elastic/claw/pull/21"
	payload.PullRequest.Merged = merged
	if merged {
		payload.PullRequest.MergedAt = "2026-01-01T00:00:00Z"
	}
	payload.PullRequest.ClosedAt = "2026-01-01T00:00:00Z"
	return payload
}

func TestGitHubWebhookClosedPRFallbackUpdatesDeadClawRunWithoutCreatingClaw(t *testing.T) {
	factory := taskRunLifecycleFactoryConfig("pr-factory", "github")
	factory.Repos = []string{"elastic/claw"}
	factory.Trigger = &types.GitHubTrigger{On: "pull_request"}
	s, db := newTaskRunLifecycleTestServer(t, []*types.FactoryConfig{factory})
	if _, err := db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, status, created_at)
		VALUES(?,?,?,?,?,?)`, "deleted-claw", "test-tenant-id", "deleted-claw", "elasticclaw", "running", now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	runID, _ := startTaskRunForTest(t, s, "deleted-claw", "dead-webhook")
	associatePRForTest(t, s, runID, "elastic/claw", 21, taskRunPRStateOpen)
	if _, err := db.Exec(`DELETE FROM claws WHERE id=?`, "deleted-claw"); err != nil {
		t.Fatalf("delete claw: %v", err)
	}

	s.processGitHubPREvent(closedPRPayload(true))
	assertTaskRunSummary(t, db, runID, taskRunStatusClean, taskRunPhaseTerminal, "", "[]", 0, 1, 0, 1, 0)
	var claws int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claws`).Scan(&claws); err != nil {
		t.Fatal(err)
	}
	if claws != 0 {
		t.Fatalf("created %d claws for closed dead-claw PR, want none", claws)
	}
}

func TestGitHubWebhookClosedUnmergedPRRecordsPayloadClosedAt(t *testing.T) {
	factory := taskRunLifecycleFactoryConfig("pr-factory", "github")
	factory.Repos = []string{"elastic/claw"}
	factory.Trigger = &types.GitHubTrigger{On: "pull_request"}
	s, db := newTaskRunLifecycleTestServer(t, []*types.FactoryConfig{factory})
	if _, err := db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, status, created_at)
		VALUES(?,?,?,?,?,?)`, "deleted-claw", "test-tenant-id", "deleted-claw", "elasticclaw", "running", now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	runID, _ := startTaskRunForTest(t, s, "deleted-claw", "dead-webhook-unmerged")
	associatePRForTest(t, s, runID, "elastic/claw", 21, taskRunPRStateOpen)
	if _, err := db.Exec(`DELETE FROM claws WHERE id=?`, "deleted-claw"); err != nil {
		t.Fatalf("delete claw: %v", err)
	}

	payload := closedPRPayload(false)
	s.processGitHubPREvent(payload)
	var closedAt int64
	if err := db.QueryRow(`SELECT closed_at FROM task_run_prs WHERE run_id=? AND repo=? AND pr_number=?`, runID, "elastic/claw", 21).Scan(&closedAt); err != nil {
		t.Fatal(err)
	}
	want := epochMillis(parseRFC3339Timestamp(payload.PullRequest.ClosedAt))
	if closedAt != want {
		t.Fatalf("closed_at=%d, want payload closed_at %d", closedAt, want)
	}
}

func TestGitHubWebhookClosedPRUsesLiveClawRunInsteadOfStaleRow(t *testing.T) {
	s, db, liveRunID, _ := newWebhookHumanPushTestRun(t, "", "claw-live-closed", "agent-sha")
	if _, err := db.Exec(`
		INSERT INTO claws(id, tenant_id, name, template, status, created_at)
		VALUES(?,?,?,?,?,?)`, "claw-stale-closed", "test-tenant-id", "claw-stale-closed", "elasticclaw", "running", now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	staleRunID, _ := startTaskRunForTest(t, s, "claw-stale-closed", "stale-closed")
	associatePRForTest(t, s, staleRunID, "elastic/claw", 21, taskRunPRStateOpen)
	// Make the stale row the one the removed unordered early intercept could
	// select. The live claw path must still close only its own task run.
	if _, err := db.Exec(`UPDATE task_run_prs SET opened_at=? WHERE run_id=?`, epochMillis(now().Add(time.Hour)), staleRunID); err != nil {
		t.Fatal(err)
	}

	s.processGitHubPREvent(closedPRPayload(true))
	assertTaskRunSummary(t, db, liveRunID, taskRunStatusClean, taskRunPhaseTerminal, "", "[]", 0, 1, 0, 1, 0)
	var staleStatus string
	if err := db.QueryRow(`SELECT status FROM task_run_summaries WHERE run_id=?`, staleRunID).Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if staleStatus != taskRunStatusRunning {
		t.Fatalf("stale run status=%q, want running", staleStatus)
	}
}

func TestFindOpenTaskRunPRPrefersMostRecentlyOpenedRow(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	for _, clawID := range []string{"claw-old-pr", "claw-new-pr"} {
		if _, err := db.Exec(`
			INSERT INTO claws(id, tenant_id, name, template, status, created_at)
			VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "elasticclaw", "running", now()); err != nil {
			t.Fatalf("insert claw: %v", err)
		}
	}
	olderRunID, _ := startTaskRunForTest(t, s, "claw-old-pr", "old-pr")
	newerRunID, _ := startTaskRunForTest(t, s, "claw-new-pr", "new-pr")
	associatePRForTest(t, s, olderRunID, "elastic/claw", 21, taskRunPRStateOpen)
	associatePRForTest(t, s, newerRunID, "elastic/claw", 21, taskRunPRStateOpen)
	if _, err := db.Exec(`UPDATE task_run_prs SET opened_at=? WHERE run_id=?`, epochMillis(now().Add(-time.Hour)), olderRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE task_run_prs SET opened_at=? WHERE run_id=?`, epochMillis(now()), newerRunID); err != nil {
		t.Fatal(err)
	}
	got, ok := s.findOpenTaskRunPR("elastic/claw", 21)
	if !ok || got != newerRunID {
		t.Fatalf("findOpenTaskRunPR = (%q, %v), want (%q, true)", got, ok, newerRunID)
	}
}
