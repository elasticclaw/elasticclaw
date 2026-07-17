package hub

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func newUsageTestServer(t *testing.T) (*Server, *sql.DB, string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hub.db")+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	// The production migration enables foreign keys; this minimal fixture does
	// not need the attempt row referenced by task_runs.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	clawID, runID := "claw-usage", "run-usage"
	if _, err := db.Exec(`INSERT INTO task_runs(id,tenant_id,initial_attempt_id,run_kind,owner_type,claw_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, runID, "tenant", uuid.NewString(), "code_task", "manual", clawID, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,task_run_id,created_at) VALUES(?,?,?,?,?,?,?)`, clawID, "tenant", "claw", "base", "connected", runID, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_run_summaries(id,tenant_id,run_id,status,phase,started_at,last_event_at,materialized_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, uuid.NewString(), "tenant", runID, "running", "claimed", 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	return &Server{db: db}, db, clawID
}

func ptr[T any](v T) *T { return &v }

func TestTaskRunUsageSnapshotsAndReset(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	if err := s.recordTaskRunUsage(claw, taskRunUsageSnapshot{SessionKey: "a", InputTokens: ptr(10), OutputTokens: ptr(5), TotalTokens: ptr(15), Model: "claude-sonnet-5"}); err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskRunUsage(claw, taskRunUsageSnapshot{SessionKey: "a", InputTokens: ptr(10), OutputTokens: ptr(5), TotalTokens: ptr(15), Model: "claude-sonnet-5"}); err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskRunUsage(claw, taskRunUsageSnapshot{SessionKey: "b", InputTokens: ptr(3), OutputTokens: ptr(2), TotalTokens: ptr(5), Model: "claude-sonnet-5"}); err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskRunUsage(claw, taskRunUsageSnapshot{SessionKey: "a", InputTokens: ptr(2), OutputTokens: ptr(1), TotalTokens: ptr(3), Model: "claude-sonnet-5"}); err != nil {
		t.Fatal(err)
	}
	var in, out, total int
	var cost float64
	if err := db.QueryRow(`SELECT input_tokens,output_tokens,total_tokens,estimated_cost_usd FROM task_run_summaries WHERE run_id='run-usage'`).Scan(&in, &out, &total, &cost); err != nil {
		t.Fatal(err)
	}
	if in != 5 || out != 3 || total != 8 {
		t.Fatalf("summary = %d/%d/%d, want 5/3/8", in, out, total)
	}
	if cost <= 0 {
		t.Fatalf("expected pricing fallback cost, got %v", cost)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM task_run_usage`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("usage rows=%d, want 2", rows)
	}
}
