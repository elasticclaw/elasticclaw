package hub

import (
	"context"
	"database/sql"
	"math"
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

func TestTaskRunUsageRecordsEachRunAndIgnoresContextTotal(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	run1 := usageSnapshot("a", 10_000, 1_000, 99_999, "claude-sonnet-5")
	run1.EstimatedCostUSD = ptr(0.05)
	run2 := usageSnapshot("a", 50_000, 2_000, 7, "claude-sonnet-5")
	run2.EstimatedCostUSD = ptr(0.20)
	for _, snap := range []taskRunUsageSnapshot{run1, run1, run2} { // repeated heartbeat
		if err := s.recordTaskRunUsage(claw, snap); err != nil {
			t.Fatal(err)
		}
	}
	var initialIn, initialOut int
	var initialCost float64
	if err := db.QueryRow(`SELECT input_tokens,output_tokens,estimated_cost_usd FROM task_run_summaries WHERE run_id='run-usage'`).Scan(&initialIn, &initialOut, &initialCost); err != nil {
		t.Fatal(err)
	}
	if initialIn != 60_000 || initialOut != 3_000 || initialCost != 0.25 {
		t.Fatalf("first two runs = %d/%d cost=%v, want 60000/3000 cost=0.25", initialIn, initialOut, initialCost)
	}
	// This run's raw total grows from run2's 7, but it is still a distinct
	// per-reply total rather than a cumulative counter.
	run3 := usageSnapshot("a", 60_000, 2_500, 60_000, "claude-sonnet-5")
	run3.EstimatedCostUSD = ptr(0.30)
	run4 := usageSnapshot("a", 5_000, 500, 123_456, "claude-sonnet-5")
	run4.EstimatedCostUSD = ptr(0.01)
	for _, snap := range []taskRunUsageSnapshot{run3, run4, usageSnapshot("b", 3_000, 300, 42, "claude-sonnet-5")} {
		if err := s.recordTaskRunUsage(claw, snap); err != nil {
			t.Fatal(err)
		}
	}
	var in, out, total int
	var cost float64
	if err := db.QueryRow(`SELECT input_tokens,output_tokens,total_tokens,estimated_cost_usd FROM task_run_summaries WHERE run_id='run-usage'`).Scan(&in, &out, &total, &cost); err != nil {
		t.Fatal(err)
	}
	// Each changed input/output snapshot is a complete run. Raw total_tokens
	// is context occupancy and is deliberately absent from these sums.
	if in != 128_000 || out != 6_300 || total != 134_300 {
		t.Fatalf("summary = %d/%d/%d, want 128000/6300/134300", in, out, total)
	}
	const wantCost = 0.5735 // $0.56 gateway costs + $0.0135 hub pricing for session b.
	if math.Abs(cost-wantCost) > 1e-6 {
		t.Fatalf("summary cost = %v, want %v", cost, wantCost)
	}
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM task_run_usage`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("usage rows=%d, want 2", rows)
	}
	din, dout, dtotal, dailyCost := queryUsageDaily(t, db)
	if din != 128_000 || dout != 6_300 || dtotal != 134_300 {
		t.Fatalf("usage_daily = %d/%d/%d, want 128000/6300/134300", din, dout, dtotal)
	}
	if math.Abs(dailyCost-wantCost) > 1e-6 {
		t.Fatalf("usage_daily cost = %v, want %v", dailyCost, wantCost)
	}
}

func TestTaskRunUsageGatewayCostSurvivesCostlessHeartbeat(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	const model = "anthropic/claude-sonnet-5-20260203"
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 1_000_000, 1_000_000, 2_000_000, model)); err != nil {
		t.Fatal(err)
	}
	snap := usageSnapshot("a", 1_000_000, 1_000_000, 9, model)
	snap.EstimatedCostUSD = ptr(3.0)
	if err := s.recordTaskRunUsage(claw, snap); err != nil {
		t.Fatal(err)
	}
	var cost, committedCost float64
	var source string
	if err := db.QueryRow(`SELECT estimated_cost_usd,cost_source FROM task_run_usage WHERE session_key='a'`).Scan(&cost, &source); err != nil {
		t.Fatal(err)
	}
	// claude-sonnet-5 prices the omitted-cost run at $18, then the gateway
	// correction replaces rather than adds to that amount.
	if cost != 3 {
		t.Fatalf("estimated cost = %v, want 3", cost)
	}
	if source != "gateway" {
		t.Fatalf("cost_source = %q, want gateway", source)
	}
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 1_000_000, 1_000_000, 10, model)); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT estimated_cost_usd,cost_source FROM task_run_usage WHERE session_key='a'`).Scan(&cost, &source); err != nil {
		t.Fatal(err)
	}
	if cost != 3 || source != "gateway" {
		t.Fatalf("omitted-cost heartbeat changed stored cost to %v/%q, want 3/gateway", cost, source)
	}
	if err := db.QueryRow(`SELECT committed_cost_usd FROM task_run_usage WHERE session_key='a'`).Scan(&committedCost); err != nil {
		t.Fatal(err)
	}
	if committedCost != 3 {
		t.Fatalf("omitted-cost heartbeat changed committed cost to %v, want 3", committedCost)
	}
	if err := db.QueryRow(`SELECT estimated_cost_usd FROM task_run_summaries WHERE run_id='run-usage'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 3 {
		t.Fatalf("summary cost = %v, want 3", cost)
	}
	_, _, _, dailyCost := queryUsageDaily(t, db)
	if dailyCost != 3 {
		t.Fatalf("usage_daily cost = %v, want 3", dailyCost)
	}
}

func TestTaskRunUsageCrossDayCostCorrectionUsesOriginalDay(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	day1 := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	s.nowFunc = func() time.Time { return day1 }
	// Session A's hub estimate is recorded on day 1.
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 1_000_000, 1_000_000, 2_000_000, "claude-sonnet-5")); err != nil {
		t.Fatal(err)
	}
	// Session B records unrelated real spend in the same dimensions on day 2.
	s.nowFunc = func() time.Time { return day2 }
	b := usageSnapshot("b", 100, 50, 150, "claude-sonnet-5")
	b.EstimatedCostUSD = ptr(7.0)
	if err := s.recordTaskRunUsage(claw, b); err != nil {
		t.Fatal(err)
	}
	// A's lower gateway cost must correct its original bucket in full.
	snap := usageSnapshot("a", 1_000_000, 1_000_000, 2_000_000, "claude-sonnet-5")
	snap.EstimatedCostUSD = ptr(3.0)
	if err := s.recordTaskRunUsage(claw, snap); err != nil {
		t.Fatal(err)
	}
	var day1Cost, day2Cost, total float64
	if err := db.QueryRow(`SELECT cost_usd FROM usage_daily WHERE tenant_id='tenant' AND day=?`, day1.Format("2006-01-02")).Scan(&day1Cost); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT cost_usd FROM usage_daily WHERE tenant_id='tenant' AND day=?`, day2.Format("2006-01-02")).Scan(&day2Cost); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT SUM(cost_usd) FROM usage_daily WHERE tenant_id='tenant'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if day1Cost != 3 || day2Cost != 7 || total != 10 {
		t.Fatalf("daily costs day1/day2/total = %v/%v/%v, want 3/7/10", day1Cost, day2Cost, total)
	}
}

func TestTaskRunUsageKeepsLastNonEmptyModel(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 10, 5, 15, "claude-sonnet-5")); err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 10, 5, 15, "")); err != nil {
		t.Fatal(err)
	}
	var model string
	if err := db.QueryRow(`SELECT model FROM task_run_usage WHERE session_key='a'`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "claude-sonnet-5" {
		t.Fatalf("model = %q, want claude-sonnet-5", model)
	}
}

func TestTaskRunUsageAttributesChangedRunToNewModel(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 10, 5, 15, "claude-sonnet-5")); err != nil {
		t.Fatal(err)
	}
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 20, 8, 28, "gpt-5-mini")); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT model,input_tokens,output_tokens FROM usage_daily WHERE tenant_id='tenant' ORDER BY model`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]int{}
	for rows.Next() {
		var model string
		var in, out int
		if err := rows.Scan(&model, &in, &out); err != nil {
			t.Fatal(err)
		}
		got[model] = [2]int{in, out}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got["claude-sonnet-5"] != [2]int{10, 5} || got["gpt-5-mini"] != [2]int{20, 8} {
		t.Fatalf("usage_daily attribution = %#v, want separate old and new model rows", got)
	}
}

func TestTaskRunUsageCostCorrectionTargetsOriginalModelBucket(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	// Run recorded under sonnet with a hub_pricing estimate (no gateway cost).
	if err := s.recordTaskRunUsage(claw, usageSnapshot("a", 100, 40, 140, "claude-sonnet-5")); err != nil {
		t.Fatal(err)
	}
	// Same-token correction arrives with a gateway cost under a different
	// model name: the delta must hit the sonnet bucket, not open an opus one.
	correction := usageSnapshot("a", 100, 40, 140, "claude-opus-4-8")
	correction.EstimatedCostUSD = ptr(0.0001)
	if err := s.recordTaskRunUsage(claw, correction); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT model,cost_usd FROM usage_daily WHERE tenant_id='tenant'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]float64{}
	for rows.Next() {
		var model string
		var cost float64
		if err := rows.Scan(&model, &cost); err != nil {
			t.Fatal(err)
		}
		got[model] = cost
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || math.Abs(got["claude-sonnet-5"]-0.0001) > 1e-9 {
		t.Fatalf("usage_daily buckets = %#v, want only claude-sonnet-5 holding the corrected cost", got)
	}
	var model string
	if err := db.QueryRow(`SELECT model FROM task_run_usage WHERE session_key='a'`).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "claude-sonnet-5" {
		t.Fatalf("stored model = %q, want claude-sonnet-5 until the next run", model)
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

func TestMigrateBackfillsUsageDayFromUpdatedAt(t *testing.T) {
	s, db, _ := newUsageTestServer(t)
	defer db.Close()
	_ = s
	// Simulate a pre-migration row: usage_day was introduced with DEFAULT ''.
	updatedAt := time.Date(2026, 7, 10, 15, 30, 0, 0, time.UTC).UnixMilli()
	if _, err := db.Exec(`INSERT INTO task_run_usage(id,tenant_id,run_id,session_key,model,model_provider,input_tokens,output_tokens,total_tokens,committed_input_tokens,committed_output_tokens,committed_total_tokens,committed_cost_usd,estimated_cost_usd,cost_source,usage_day,first_seen_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		uuid.NewString(), "tenant", "run-usage", "sess-legacy", "claude-sonnet-5", "anthropic", 10, 5, 15, 10, 5, 15, 0.01, 0.01, "hub_pricing", "", updatedAt, updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var day string
	if err := db.QueryRow(`SELECT usage_day FROM task_run_usage WHERE session_key='sess-legacy'`).Scan(&day); err != nil {
		t.Fatal(err)
	}
	if day != "2026-07-10" {
		t.Fatalf("usage_day = %q, want 2026-07-10", day)
	}
}
