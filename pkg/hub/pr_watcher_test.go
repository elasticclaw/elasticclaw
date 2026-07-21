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
		if s.checkPRMerged(pr, "token") {
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
			errs[i] = s.storePRMention("claw-dup", "owner/repo", 7, "https://github.com/owner/repo/pull/7")
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
