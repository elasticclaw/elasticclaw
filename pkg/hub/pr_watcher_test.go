package hub

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// githubAppTokenTransport supplies GitHub App installation tokens without
// reaching api.github.com. It deliberately rejects unscoped token requests so
// reconciliation tests prove that the repo-scoped lookup is used.
type githubAppTokenTransport struct{ base http.RoundTripper }

func (t githubAppTokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "api.github.com" {
		return t.base.RoundTrip(r)
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
		return githubAppTokenResponse(http.StatusOK, `[{"id":1,"account":{"login":"owner"}}]`), nil
	}
	if r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens" {
		if bytes.Contains(body, []byte(`"repositories"`)) {
			return githubAppTokenResponse(http.StatusCreated, `{"token":"repo-token","expires_at":"2030-01-01T00:00:00Z"}`), nil
		}
		return githubAppTokenResponse(http.StatusForbidden, `{"message":"unscoped token unavailable"}`), nil
	}
	return githubAppTokenResponse(http.StatusNotFound, `{"message":"not found"}`), nil
}

func githubAppTokenResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func testGitHubAppPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestReconcileDeadClawPRsClosesRunsWithRepoTokenAndZeroOpenedAt(t *testing.T) {
	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppTokenTransport{base: oldTransport}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer repo-token" {
			t.Fatalf("authorization = %q, want repo token", r.Header.Get("Authorization"))
		}
		if strings.HasSuffix(r.URL.Path, "/pulls/1") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"state": "closed", "merged": true, "merged_at": "2026-01-01T00:00:00Z"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"state": "closed", "merged": false, "closed_at": "2026-01-01T00:00:00Z"})
	}))
	defer gh.Close()

	s, db := NewTestServerWithConfig(t, &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}, gh.URL, "", "")
	for _, tc := range []struct {
		claw, key string
		number    int
	}{{"dead-merged", "merged", 1}, {"dead-closed", "closed", 2}} {
		if _, err := db.Exec(`
			INSERT INTO claws(id, tenant_id, name, template, status, created_at)
			VALUES(?,?,?,?,?,?)`, tc.claw, "test-tenant-id", tc.claw, "elasticclaw", "running", now()); err != nil {
			t.Fatal(err)
		}
		runID, _ := startTaskRunForTest(t, s, tc.claw, tc.key)
		associatePRForTest(t, s, runID, "owner/repo", tc.number, taskRunPRStateOpen)
		if tc.number == 1 {
			if _, err := db.Exec(`UPDATE task_run_prs SET opened_at=0 WHERE run_id=?`, runID); err != nil {
				t.Fatal(err)
			}
		}
	}

	s.reconcileDeadClawPRs()
	for _, tc := range []struct{ key, want string }{{"merged", taskRunStatusClean}, {"closed", taskRunStatusClean}} {
		var status string
		if err := db.QueryRow(`SELECT status FROM task_run_summaries WHERE factory_name=?`, "factory-"+tc.key).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != tc.want {
			t.Fatalf("%s status=%q, want %q", tc.key, status, tc.want)
		}
	}
}

func TestReconcileDeadClawPRsClosesUnmergedPRSetsSuccessStatus(t *testing.T) {
	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppTokenTransport{base: oldTransport}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer repo-token" {
			t.Fatalf("authorization = %q, want repo token", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"state": "closed", "merged": false, "closed_at": "2026-01-01T00:00:00Z"})
	}))
	defer gh.Close()

	s, db := NewTestServerWithConfig(t, &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}, gh.URL, "", "")
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`, "dead-unmerged", "test-tenant-id", "dead-unmerged", "elasticclaw", "running", now()); err != nil {
		t.Fatal(err)
	}
	runID, _ := startTaskRunForTest(t, s, "dead-unmerged", "unmerged")
	associatePRForTest(t, s, runID, "owner/repo", 3, taskRunPRStateOpen)

	s.reconcileDeadClawPRs()
	assertTaskRunSummary(t, db, runID, taskRunStatusClean, taskRunPhaseTerminal, "", `["pr_closed_unmerged"]`, 0, 1, 0, 0, 1)
	var closedAt int64
	if err := db.QueryRow(`SELECT closed_at FROM task_run_prs WHERE run_id=? AND repo=? AND pr_number=?`, runID, "owner/repo", 3).Scan(&closedAt); err != nil {
		t.Fatal(err)
	}
	if closedAt == 0 {
		t.Fatal("expected closed_at to be populated")
	}
}

func TestReconcileDeadClawPRsClosesMergedPRForTerminalNonPRRun(t *testing.T) {
	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppTokenTransport{base: oldTransport}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer repo-token" {
			t.Fatalf("authorization = %q, want repo token", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"state": "closed", "merged": true, "merged_at": "2026-01-01T00:00:00Z"})
	}))
	defer gh.Close()

	s, db := NewTestServerWithConfig(t, &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}, gh.URL, "", "")
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`, "dead-terminal", "test-tenant-id", "dead-terminal", "elasticclaw", "running", now()); err != nil {
		t.Fatal(err)
	}
	runID, attemptID := startTaskRunWithRequiresPRForTest(t, s, "dead-terminal", "terminal", false)
	recordTaskRunEventForTest(t, s, TaskRunEvent{TenantID: "test-tenant-id", RunID: runID, AttemptID: attemptID, EventKey: "terminal:completed", EventType: taskRunEventTaskCompleted, ActorType: taskRunActorAgent, Source: taskRunSourceHub})
	associatePRForTest(t, s, runID, "owner/repo", 5, taskRunPRStateOpen)
	assertTaskRunSummary(t, db, runID, taskRunStatusClean, taskRunPhaseTerminal, "", "[]", 0, 1, 1, 0, 0)

	s.reconcileDeadClawPRs()
	assertTaskRunPR(t, db, runID, "owner/repo", 5, taskRunPRStateClosed, true)
	var mergedAt int64
	if err := db.QueryRow(`SELECT merged_at FROM task_run_summaries WHERE run_id=?`, runID).Scan(&mergedAt); err != nil {
		t.Fatal(err)
	}
	if mergedAt == 0 {
		t.Fatal("expected merged_at to be populated")
	}
}

func TestReconcileDeadClawPRsSkipsAgeCappedRows(t *testing.T) {
	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppTokenTransport{base: oldTransport}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	var requests atomic.Int64
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"state": "closed", "merged": false, "closed_at": "2026-01-01T00:00:00Z"})
	}))
	defer gh.Close()

	s, db := NewTestServerWithConfig(t, &types.HubConfig{GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}}}, gh.URL, "", "")
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`, "dead-old", "test-tenant-id", "dead-old", "elasticclaw", "running", now()); err != nil {
		t.Fatal(err)
	}
	runID, _ := startTaskRunForTest(t, s, "dead-old", "old")
	associatePRForTest(t, s, runID, "owner/repo", 4, taskRunPRStateOpen)
	if _, err := db.Exec(`UPDATE task_run_prs SET opened_at=? WHERE run_id=?`, epochMillis(now().Add(-100*24*time.Hour)), runID); err != nil {
		t.Fatal(err)
	}

	s.reconcileDeadClawPRs()
	if got := requests.Load(); got != 0 {
		t.Fatalf("GitHub requests = %d, want 0", got)
	}
	assertTaskRunPR(t, db, runID, "owner/repo", 4, taskRunPRStateOpen, false)
	assertTaskRunSummary(t, db, runID, taskRunStatusRunning, taskRunPhaseWaitingForMerge, "", "[]", 0, 1, 1, 0, 0)
}

func insertWatcherTestPR(t *testing.T, db *sql.DB, clawID, prID string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "elasticclaw", "connected", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES(?,?,?,?,?,?)`, prID, clawID, "owner/repo", 1, "https://github.com/owner/repo/pull/1", now()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPRMergedStopsAfterPermanentFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer server.Close()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, server.URL, "", "")
	insertWatcherTestPR(t, db, "claw-404", "pr-404")
	pr := clawPR{id: "pr-404", clawID: "claw-404", repo: "owner/repo", prNumber: 1, prURL: "https://github.com/owner/repo/pull/1"}
	for i := 0; i < prMergedPermanentFailureLimit; i++ {
		s.checkPRMerged(pr, "token")
	}
	// stopAgentWithReason runs in a goroutine and may sleep up to 6s
	// (clawStopRevaluationDelays) when the retry disposition is indeterminate
	// under DB contention, so give it a generous deadline.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var status string
		_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, pr.clawID).Scan(&status)
		if status == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("claw status = %q, want error", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCheckPRMergedDoesNotCountTransientFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary", http.StatusInternalServerError)
	}))
	defer server.Close()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, server.URL, "", "")
	insertWatcherTestPR(t, db, "claw-500", "pr-500")
	pr := clawPR{id: "pr-500", clawID: "claw-500", repo: "owner/repo", prNumber: 1, prURL: "https://github.com/owner/repo/pull/1"}
	for i := 0; i < prMergedPermanentFailureLimit+1; i++ {
		s.checkPRMerged(pr, "token")
	}
	var failures int
	var status string
	_ = db.QueryRow(`SELECT permanent_failure_count FROM claw_prs WHERE id=?`, pr.id).Scan(&failures)
	_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, pr.clawID).Scan(&status)
	if failures != 0 || status != "connected" {
		t.Fatalf("failures=%d status=%q, want 0 and connected", failures, status)
	}
}

func TestCheckPRMergedResetsCounterBetweenPermanentFailures(t *testing.T) {
	// Non-consecutive permanent failures interleaved with transient errors must
	// not accumulate toward the "consecutive polls" limit.
	var n atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1)%2 == 1 {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) // permanent
		} else {
			http.Error(w, "temporary", http.StatusInternalServerError) // transient
		}
	}))
	defer server.Close()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, server.URL, "", "")
	insertWatcherTestPR(t, db, "claw-mix", "pr-mix")
	pr := clawPR{id: "pr-mix", clawID: "claw-mix", repo: "owner/repo", prNumber: 1, prURL: "https://github.com/owner/repo/pull/1"}
	for i := 0; i < (prMergedPermanentFailureLimit+1)*2; i++ {
		if _, terminated := s.checkPRMerged(pr, "token"); terminated {
			t.Fatalf("checkPRMerged terminated on non-consecutive failures")
		}
	}
	var failures int
	var status string
	_ = db.QueryRow(`SELECT permanent_failure_count FROM claw_prs WHERE id=?`, pr.id).Scan(&failures)
	_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, pr.clawID).Scan(&status)
	if failures >= prMergedPermanentFailureLimit || status != "connected" {
		t.Fatalf("failures=%d status=%q; want < %d and connected", failures, status, prMergedPermanentFailureLimit)
	}
}

func prConditionsCIPipelineContext() pipelineContext {
	yaml := `
stages:
  - id: watch
  - id: done
    triggers:
      - pr_conditions:
          ci: passing
`
	return pipelineContext{Workflow: &types.WorkflowConfig{PipelineYAML: yaml}}
}

func TestCheckPRConditionsStatus(t *testing.T) {
	ctx := prConditionsCIPipelineContext()
	pr := clawPR{id: "pr-cond", clawID: "claw-cond", repo: "owner/repo", prNumber: 1, prURL: "https://github.com/owner/repo/pull/1"}

	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    prConditionsStatus
	}{
		{
			name: "transient error on PR fetch never times out",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "temporary", http.StatusInternalServerError)
			},
			want: prConditionsTransientError,
		},
		{
			name: "no check runs is a genuine stall",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/check-runs") {
					_, _ = w.Write([]byte(`{"check_runs":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{"head":{"sha":"abc"},"state":"open"}`))
			},
			want: prConditionsStuck,
		},
		{
			name: "CI still running is healthy progress",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/check-runs") {
					_, _ = w.Write([]byte(`{"check_runs":[{"status":"in_progress","conclusion":""}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"head":{"sha":"abc"},"state":"open"}`))
			},
			want: prConditionsWaiting,
		},
		{
			name: "all checks passing satisfies conditions",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "/check-runs") {
					_, _ = w.Write([]byte(`{"check_runs":[{"status":"completed","conclusion":"success"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"head":{"sha":"abc"},"state":"open"}`))
			},
			want: prConditionsSatisfied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, server.URL, "", "")
			stage, status := s.checkPRConditions(pr, "token", ctx)
			if status != tc.want {
				t.Fatalf("status = %d, want %d", status, tc.want)
			}
			if (status == prConditionsSatisfied) != (stage != nil) {
				t.Fatalf("stage=%v inconsistent with status=%d", stage, status)
			}
		})
	}
}

func TestCompleteIssueLessDoneClawKeepsRunningOnStoreFailure(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	const clawID = "claw-store-fail"
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "elasticclaw", "connected", now()); err != nil {
		t.Fatal(err)
	}
	// Force storePRMention's INSERT to fail so the PR is never tracked.
	if _, err := db.Exec(`DROP TABLE claw_prs`); err != nil {
		t.Fatal(err)
	}

	s.completeIssueLessDoneClaw(clawID, "test-tenant-id", []string{"https://github.com/owner/repo/pull/7"})

	var status string
	_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status)
	if status == "idle" {
		t.Fatalf("claw idled despite failed PR registration; expected it to keep running")
	}
	var msg string
	_ = db.QueryRow(`SELECT content FROM messages WHERE claw_id=? ORDER BY created_at DESC LIMIT 1`, clawID).Scan(&msg)
	if !strings.Contains(msg, "Please resend") {
		t.Fatalf("expected resend nudge message, got %q", msg)
	}
}

func TestPRConditionsRemainEligibleWhenTransitionFails(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-pr-conditions"
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "PR-1", "elasticclaw", "connected", "ready"); err != nil {
		t.Fatal(err)
	}
	pr := clawPR{id: "pr-conditions", clawID: clawID, prURL: "https://github.com/o/r/pull/1"}
	if _, err := db.Exec(`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, created_at) VALUES(?,?,?,?,?,datetime('now'))`, pr.id, pr.clawID, "o/r", 1, pr.prURL); err != nil {
		t.Fatal(err)
	}
	stage := pipeline.Stage{ID: "ready"} // already claimed: transition must fail
	s.firePRConditions(pr, stage, pipelineContext{})
	s.firePRConditions(pr, stage, pipelineContext{}) // next poll remains eligible

	var fired int
	if err := db.QueryRow(`SELECT pr_conditions_fired FROM claw_prs WHERE id=?`, pr.id).Scan(&fired); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatalf("pr_conditions_fired = %d, want 0 after failed transitions", fired)
	}
}

// TestStorePRMentionConcurrentDuplicate exercises the race between the [DONE]
// handler and the message scanner both registering the same PR: the loser of
// the INSERT must treat the duplicate as idempotent success, not an error.
func TestStorePRMentionConcurrentDuplicate(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, "claw-dup", "test-tenant-id", "claw-dup", "elasticclaw", "connected", now()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.storePRMention("claw-dup", "owner/repo", 7, "https://github.com/owner/repo/pull/7", true)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("storePRMention[%d] = %v, want nil", i, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id='claw-dup'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("claw_prs rows = %d, want 1", count)
	}
}

// ciStatusFixture wires a server against a stub GitHub that reports a fixed
// head SHA and a fixed set of check runs for it.
func ciStatusFixture(t *testing.T, clawID, headSHA, checkRunsJSON string) (*Server, *sql.DB, clawPR) {
	t.Helper()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/check-runs") {
			_, _ = w.Write([]byte(`{"check_runs":` + checkRunsJSON + `}`))
			return
		}
		_, _ = w.Write([]byte(`{"head":{"sha":"` + headSHA + `"},"state":"open"}`))
	}))
	t.Cleanup(gh.Close)

	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, gh.URL, "", "")
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`,
		clawID, "test-tenant-id", clawID, "elasticclaw", "connected", now()); err != nil {
		t.Fatal(err)
	}
	pr := clawPR{id: "pr-" + clawID, clawID: clawID, repo: "owner/repo", prNumber: 42, prURL: "https://github.com/owner/repo/pull/42"}
	if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES(?,?,?,?,?,?)`,
		pr.id, pr.clawID, pr.repo, pr.prNumber, pr.prURL, now()); err != nil {
		t.Fatal(err)
	}
	return s, db, pr
}

func ciMessages(t *testing.T, db *sql.DB, clawID string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT content FROM messages WHERE claw_id=? ORDER BY created_at, rowid`, clawID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func ciWatermark(t *testing.T, db *sql.DB, prID string) (string, string) {
	t.Helper()
	var sha, conclusion string
	if err := db.QueryRow(`SELECT last_ci_sha, last_ci_conclusion FROM claw_prs WHERE id=?`, prID).Scan(&sha, &conclusion); err != nil {
		t.Fatal(err)
	}
	return sha, conclusion
}

// TestCheckCIStatusGreenWakesIdleClawOnce is the regression test for the CI-green
// deadlock: an agent that pushed a fix and ended its turn waiting on CI got no
// event at all, because only failures were reported.
func TestCheckCIStatusGreenWakesIdleClawOnce(t *testing.T) {
	const clawID = "claw-ci-green"
	const headSHA = "04cc3f49aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	allGreen := `[{"name":"verify","status":"completed","conclusion":"success"},
	              {"name":"gitleaks","status":"completed","conclusion":"success"}]`
	s, db, pr := ciStatusFixture(t, clawID, headSHA, allGreen)

	s.checkCIStatus(pr, "token")

	msgs := ciMessages(t, db, clawID)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d (%v), want 1", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "All CI checks passed on PR #42") || !strings.Contains(msgs[0], "04cc3f4") {
		t.Fatalf("unexpected CI-green message: %q", msgs[0])
	}
	var role string
	if err := db.QueryRow(`SELECT role FROM messages WHERE claw_id=?`, clawID).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "hub" {
		t.Fatalf("role = %q, want hub (same channel as review-comment wake-ups)", role)
	}
	sha, conclusion := ciWatermark(t, db, pr.id)
	if sha != headSHA || conclusion != ciConclusionSuccess {
		t.Fatalf("watermark = (%q,%q), want (%q,success)", sha, conclusion, headSHA)
	}

	// Re-poll with the watermark the previous poll wrote: no second wake-up.
	pr.lastCISHA, pr.lastCIConclusion = sha, conclusion
	s.checkCIStatus(pr, "token")
	if msgs := ciMessages(t, db, clawID); len(msgs) != 1 {
		t.Fatalf("re-poll injected again: messages = %d (%v), want 1", len(msgs), msgs)
	}
}

func TestCheckCIStatusAfterPRRowRemovalRecordsEventWithoutMessage(t *testing.T) {
	s, db, pr := ciStatusFixture(t, "claw-ci-row-removed", "abcdef0123456789", `[{"name":"verify","status":"completed","conclusion":"success"}]`)
	const runID = "run-ci-row-removed"
	if _, err := db.Exec(`INSERT INTO task_runs(id,tenant_id,initial_attempt_id,run_kind,owner_type,claw_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		runID, "test-tenant-id", "attempt-ci-row-removed", taskRunKindCodeTask, taskRunOwnerManual, pr.clawID, now().UnixMilli(), now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE claws SET task_run_id=? WHERE id=?`, runID, pr.clawID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM claw_prs WHERE id=?`, pr.id); err != nil {
		t.Fatal(err)
	}
	s.checkCIStatus(pr, "token")
	if messages := ciMessages(t, db, pr.clawID); len(messages) != 0 {
		t.Fatalf("messages = %v, want none after PR row removal", messages)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE event_type=?`, taskRunEventCISucceeded).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("ci events = %d, want 1", events)
	}
}

// A stale in-memory clawPR (e.g. two overlapping polls) must not re-notify:
// the conditional UPDATE is the authority, not the cached watermark.
func TestCheckCIStatusGreenClaimBlocksConcurrentRepoll(t *testing.T) {
	const clawID = "claw-ci-green-claim"
	s, db, pr := ciStatusFixture(t, clawID, "abcdef0123456789", `[{"name":"verify","status":"completed","conclusion":"success"}]`)

	s.checkCIStatus(pr, "token")
	s.checkCIStatus(pr, "token") // same stale pr value, watermark already claimed

	if msgs := ciMessages(t, db, clawID); len(msgs) != 1 {
		t.Fatalf("messages = %d (%v), want 1", len(msgs), msgs)
	}
}

func TestCheckCIStatusGreenDoesNotInterruptBusyClaw(t *testing.T) {
	const clawID = "claw-ci-green-busy"
	s, db, pr := ciStatusFixture(t, clawID, "beefbeefbeefbeef", `[{"name":"verify","status":"completed","conclusion":"success"}]`)
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id", awaitingResponse: true}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()

	s.checkCIStatus(pr, "token")

	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND delivered_at IS NULL`, clawID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending messages = %d, want 1 (queued, not delivered mid-turn)", pending)
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if !cc.awaitingResponse || !cc.streamingStartedAt.IsZero() {
		t.Fatal("CI-green injection started a turn on a busy claw")
	}
}

func TestCheckCIStatusRunningNeitherNotifiesNorAdvances(t *testing.T) {
	const clawID = "claw-ci-running"
	s, db, pr := ciStatusFixture(t, clawID, "0011223344556677",
		`[{"name":"verify","status":"in_progress","conclusion":null}]`)

	s.checkCIStatus(pr, "token")

	if msgs := ciMessages(t, db, clawID); len(msgs) != 0 {
		t.Fatalf("messages = %v, want none while CI is running", msgs)
	}
	if sha, conclusion := ciWatermark(t, db, pr.id); sha != "" || conclusion != "" {
		t.Fatalf("watermark = (%q,%q), want empty while CI is running", sha, conclusion)
	}
}

func TestCheckCIStatusNoCheckRunsIsNotGreen(t *testing.T) {
	const clawID = "claw-ci-none"
	s, db, pr := ciStatusFixture(t, clawID, "7766554433221100", `[]`)

	s.checkCIStatus(pr, "token")

	if msgs := ciMessages(t, db, clawID); len(msgs) != 0 {
		t.Fatalf("messages = %v, want none when CI has not reported", msgs)
	}
	if sha, _ := ciWatermark(t, db, pr.id); sha != "" {
		t.Fatalf("watermark = %q, want empty when CI has not reported", sha)
	}
}

func TestCheckCIStatusFailureMessageUnchangedAndRerunCanTurnGreen(t *testing.T) {
	const clawID = "claw-ci-flip"
	const headSHA = "1234567890abcdef"
	s, db, pr := ciStatusFixture(t, clawID, headSHA,
		`[{"name":"verify","status":"completed","conclusion":"failure","details_url":"https://ci/1"}]`)

	s.checkCIStatus(pr, "token")

	msgs := ciMessages(t, db, clawID)
	want := "CI failed on PR #42 ([owner/repo](https://github.com/owner/repo/pull/42)):\n\n" +
		"**verify** — [view logs](https://ci/1)\n\nPlease fix these failures on the same branch."
	if len(msgs) != 1 || msgs[0] != want {
		t.Fatalf("failure message = %v, want %q", msgs, want)
	}
	sha, conclusion := ciWatermark(t, db, pr.id)
	if sha != headSHA || conclusion != ciConclusionFailure {
		t.Fatalf("watermark = (%q,%q), want (%q,failure)", sha, conclusion, headSHA)
	}

	// A re-run of the *same* SHA that turns green must still wake the agent.
	green, _, prGreen := ciStatusFixture(t, clawID+"-2", headSHA, `[{"name":"verify","status":"completed","conclusion":"success"}]`)
	if _, err := green.db.Exec(`UPDATE claw_prs SET last_ci_sha=?, last_ci_conclusion=? WHERE id=?`, headSHA, ciConclusionFailure, prGreen.id); err != nil {
		t.Fatal(err)
	}
	prGreen.lastCISHA, prGreen.lastCIConclusion = headSHA, ciConclusionFailure
	green.checkCIStatus(prGreen, "token")
	greenMsgs := ciMessages(t, green.db, prGreen.clawID)
	if len(greenMsgs) != 1 || !strings.Contains(greenMsgs[0], "All CI checks passed") {
		t.Fatalf("failure->success on same SHA did not notify: %v", greenMsgs)
	}
}

// A completed-but-not-green conclusion (cancelled, action_required, stale,
// startup_failure, or anything unknown) must never be announced as green: the
// claw would emit its stage signal token on a PR that is not actually passing.
func TestCheckCIStatusNonGreenTerminalConclusionsAreNotGreen(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "action_required", "stale", "startup_failure", "some_future_value"} {
		t.Run(conclusion, func(t *testing.T) {
			clawID := "claw-ci-" + conclusion
			const headSHA = "abcdef1234567890"
			s, db, pr := ciStatusFixture(t, clawID, headSHA,
				`[{"name":"verify","status":"completed","conclusion":"success"},
				  {"name":"deploy","status":"completed","conclusion":"`+conclusion+`","details_url":"https://ci/2"}]`)

			s.checkCIStatus(pr, "token")

			msgs := ciMessages(t, db, clawID)
			if len(msgs) != 1 {
				t.Fatalf("messages = %v, want exactly 1", msgs)
			}
			if strings.Contains(msgs[0], "All CI checks passed") {
				t.Fatalf("%q reported as green: %q", conclusion, msgs[0])
			}
			if !strings.Contains(msgs[0], "**deploy ("+conclusion+")**") {
				t.Fatalf("message does not name the non-green check: %q", msgs[0])
			}
			if _, got := ciWatermark(t, db, pr.id); got != ciConclusionFailure {
				t.Fatalf("watermark conclusion = %q, want failure", got)
			}
		})
	}
}

// neutral and skipped are green: a skipped optional job must not block the PR.
func TestCheckCIStatusNeutralAndSkippedAreGreen(t *testing.T) {
	const clawID = "claw-ci-neutral"
	const headSHA = "fedcba0987654321"
	s, db, pr := ciStatusFixture(t, clawID, headSHA,
		`[{"name":"verify","status":"completed","conclusion":"success"},
		  {"name":"optional","status":"completed","conclusion":"skipped"},
		  {"name":"advisory","status":"completed","conclusion":"neutral"}]`)

	s.checkCIStatus(pr, "token")

	msgs := ciMessages(t, db, clawID)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "All CI checks passed") {
		t.Fatalf("messages = %v, want one green notification", msgs)
	}
	if _, got := ciWatermark(t, db, pr.id); got != ciConclusionSuccess {
		t.Fatalf("watermark conclusion = %q, want success", got)
	}
}

func TestInjectMessageSkipsIdenticalPendingRow(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "inject-message-dedupe"
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,?,?,?,?,?)`, clawID, "test-tenant-id", clawID, "elasticclaw", "connected", now()); err != nil {
		t.Fatal(err)
	}
	cc := &clawConn{id: clawID, tenantID: "test-tenant-id", awaitingResponse: true}
	s.mu.Lock()
	s.claws[clawID] = cc
	s.mu.Unlock()
	s.injectHubMessageByID(clawID, "same text")
	s.injectHubMessageByID(clawID, "same text")
	s.injectHubMessageByID(clawID, "different text")
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND delivered_at IS NULL`, clawID).Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("pending rows=%d err=%v, want 2", pending, err)
	}
	if _, err := db.Exec(`UPDATE messages SET delivered_at=? WHERE claw_id=? AND content='same text'`, now(), clawID); err != nil {
		t.Fatal(err)
	}
	s.injectHubMessageByID(clawID, "same text")
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content='same text'`, clawID).Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("same text rows=%d err=%v, want 2", pending, err)
	}
}
