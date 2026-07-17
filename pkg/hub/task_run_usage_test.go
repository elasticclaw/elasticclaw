package hub

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket/wsjson"
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

func usageSnapshot(session string, in, out, total int, model string) taskRunUsageSnapshot {
	return taskRunUsageSnapshot{SessionKey: session, InputTokens: ptr(in), OutputTokens: ptr(out), TotalTokens: ptr(total), Model: model}
}

func queryUsageDaily(t *testing.T, db *sql.DB) (in, out, total int, cost float64) {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	if err := db.QueryRow(`SELECT input_tokens,output_tokens,total_tokens,cost_usd FROM usage_daily WHERE tenant_id='tenant' AND day=?`, day).Scan(&in, &out, &total, &cost); err != nil {
		t.Fatal(err)
	}
	return
}

func TestTaskRunUsageSnapshotsAndReset(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	for _, snap := range []taskRunUsageSnapshot{
		usageSnapshot("a", 10, 5, 15, "claude-sonnet-5"),
		usageSnapshot("a", 10, 5, 15, "claude-sonnet-5"), // idempotent re-delivery: no delta
		usageSnapshot("b", 3, 2, 5, "claude-sonnet-5"),
		usageSnapshot("a", 2, 1, 3, "claude-sonnet-5"), // gateway reset: new values become the delta
	} {
		if err := s.recordTaskRunUsage(claw, snap); err != nil {
			t.Fatal(err)
		}
	}
	var in, out, total int
	var cost float64
	if err := db.QueryRow(`SELECT input_tokens,output_tokens,total_tokens,estimated_cost_usd FROM task_run_summaries WHERE run_id='run-usage'`).Scan(&in, &out, &total, &cost); err != nil {
		t.Fatal(err)
	}
	// Summary keeps pre-reset usage: session a = 10/5/15 committed + 2/1/3
	// current, session b = 3/2/5.
	if in != 15 || out != 8 || total != 23 {
		t.Fatalf("summary = %d/%d/%d, want 15/8/23", in, out, total)
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
	// usage_daily accumulates deltas: 10/5/15 + 0 (idempotent) + 3/2/5 + 2/1/3 (reset).
	din, dout, dtotal, _ := queryUsageDaily(t, db)
	if din != 15 || dout != 8 || dtotal != 23 {
		t.Fatalf("usage_daily = %d/%d/%d, want 15/8/23", din, dout, dtotal)
	}
}

func TestTaskRunUsagePartialResetTreatedAsFullReset(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 10, 5, 15, "claude-sonnet-5")); err != nil {
		t.Fatal(err)
	}
	// input drops (reset) but output is higher than before: the whole snapshot
	// must be treated as a fresh baseline, not mixed per-field deltas.
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 4, 9, 13, "claude-sonnet-5")); err != nil {
		t.Fatal(err)
	}
	din, dout, dtotal, _ := queryUsageDaily(t, db)
	if din != 14 || dout != 14 || dtotal != 28 {
		t.Fatalf("usage_daily = %d/%d/%d, want 14/14/28", din, dout, dtotal)
	}
}

func TestTaskRunUsagePricingFallbackKeepsGrowing(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	// Provider-qualified gateway model id must still match the seeded price.
	const model = "anthropic/claude-sonnet-5-20260203"
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 1_000_000, 1_000_000, 2_000_000, model)); err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 2_000_000, 2_000_000, 4_000_000, model)); err != nil {
		t.Fatal(err)
	}
	var cost float64
	var source string
	if err := db.QueryRow(`SELECT estimated_cost_usd,cost_source FROM task_run_usage WHERE session_key='a'`).Scan(&cost, &source); err != nil {
		t.Fatal(err)
	}
	// claude-sonnet-5 is $3/$15 per MTok: 2 MTok in + 2 MTok out = $36.
	if cost < 35.99 || cost > 36.01 {
		t.Fatalf("cumulative estimated cost = %v, want ~36 (estimate must keep growing)", cost)
	}
	if source != "hub_pricing" {
		t.Fatalf("cost_source = %q, want hub_pricing", source)
	}
	_, _, _, dailyCost := queryUsageDaily(t, db)
	if dailyCost < 35.99 || dailyCost > 36.01 {
		t.Fatalf("usage_daily cost = %v, want ~36", dailyCost)
	}
}

func TestTaskRunUsageUnknownModelGetsNoEstimatedCost(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	for _, model := range []string{"", "gpt-9-mega"} {
		if err := s.recordTaskRunUsage(claw, usageSnapshot("sess-"+model, 10, 5, 15, model)); err != nil {
			t.Fatal(err)
		}
	}
	var withCost int
	if err := db.QueryRow(`SELECT count(*) FROM task_run_usage WHERE estimated_cost_usd IS NOT NULL`).Scan(&withCost); err != nil {
		t.Fatal(err)
	}
	if withCost != 0 {
		t.Fatalf("unknown/missing models must not be priced, got %d costed rows", withCost)
	}
}

func TestHeartbeatIngestsTaskRunUsage(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "usage-heartbeat"
	conn := watchdogClaw(t, s, clawID)
	runID, _, err := s.ensureTaskRunForClaw(clawID, TaskRunStart{RunKind: taskRunKindPRTask, OwnerType: taskRunOwnerFactory, AnalyticsEnabled: true, Tags: []string{}})
	if err != nil {
		t.Fatalf("create task run: %v", err)
	}
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{Type: "heartbeat", Payload: map[string]any{
		"gateway_healthy": true, "session_key": "sess-1",
		"input_tokens": 100, "output_tokens": 40, "total_tokens": 140,
		"estimated_cost_usd": 0.05, "model": "claude-sonnet-5", "model_provider": "anthropic",
	}}); err != nil {
		t.Fatal(err)
	}
	eventuallyWatchdog(t, func() bool {
		var n int
		_ = db.QueryRow(`SELECT count(*) FROM task_run_usage WHERE run_id=? AND session_key='sess-1'`, runID).Scan(&n)
		return n == 1
	}, "usage row ingested from heartbeat")
	var in, out, total int
	var cost float64
	var model, provider, source string
	if err := db.QueryRow(`SELECT input_tokens,output_tokens,total_tokens,estimated_cost_usd,model,model_provider,cost_source FROM task_run_usage WHERE run_id=?`, runID).Scan(&in, &out, &total, &cost, &model, &provider, &source); err != nil {
		t.Fatal(err)
	}
	if in != 100 || out != 40 || total != 140 || cost != 0.05 || model != "claude-sonnet-5" || provider != "anthropic" || source != "gateway" {
		t.Fatalf("unexpected usage row: %d/%d/%d cost=%v model=%q provider=%q source=%q", in, out, total, cost, model, provider, source)
	}
	eventuallyWatchdog(t, func() bool {
		var sumTotal int
		_ = db.QueryRow(`SELECT total_tokens FROM task_run_summaries WHERE run_id=?`, runID).Scan(&sumTotal)
		return sumTotal == 140
	}, "summary usage columns updated")
}
