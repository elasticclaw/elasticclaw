package hub

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

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
	var n int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n%2 == 1 {
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
