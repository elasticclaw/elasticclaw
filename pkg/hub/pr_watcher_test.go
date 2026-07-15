package hub

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
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
	deadline := time.Now().Add(time.Second)
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
