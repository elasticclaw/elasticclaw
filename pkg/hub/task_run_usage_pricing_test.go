package hub

import (
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"
)

func seedRatioUsage(t *testing.T, db *sql.DB, model string, cost float64, at time.Time) {
	t.Helper()
	id := fmt.Sprintf("ratio-%s-%d-%v", model, at.UnixNano(), cost)
	_, err := db.Exec(`INSERT INTO task_run_usage(id,tenant_id,run_id,session_key,model,committed_input_tokens,committed_output_tokens,committed_cost_usd,cost_source,first_seen_at,updated_at) VALUES(?,'tenant','run-usage',?,?,1000000,100000,?,'gateway',?,?)`, id, id, model, cost, at.UnixMilli(), at.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}

func TestFallbackCacheCostRatio(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	type sample struct {
		model string
		cost  float64
		age   int
	}
	for _, tc := range []struct {
		name string
		rows []sample
		want float64
	}{
		{"same model weighted cost", []sample{{"ANTHROPIC/CLAUDE-SONNET-5-5-20261001", 6, 0}, {"claude-sonnet-5-5", 12, 1}, {"claude-sonnet-5-5", 18, 2}, {"claude-sonnet-5", 90, 0}}, 4},
		{"all models with own prices", []sample{{"claude-sonnet-5-5", 6, 0}, {"anthropic/claude-sonnet-4-6", 18, 0}, {"claude-sonnet-4-6", 27, 0}}, 51.0 / 12},
		{"default", []sample{{"claude-sonnet-5-5", 6, 0}, {"unknown-model", 900, 0}, {"unknown-model", 800, 1}}, 6.5},
		{"clamp", []sample{{"claude-sonnet-5-5", .1, 0}, {"claude-sonnet-5-5", .2, 1}, {"claude-sonnet-5-5", .3, 2}}, 1},
		{"30 day window", []sample{{"claude-sonnet-5-5", 6, 0}, {"claude-sonnet-5-5", 6, 1}, {"claude-sonnet-5-5", 6, 30}, {"claude-sonnet-5-5", 900, 31}, {"claude-sonnet-5-5", 800, -1}}, 2},
		{"old rows cannot meet minimum", []sample{{"claude-sonnet-5-5", 6, 0}, {"claude-sonnet-5-5", 6, 1}, {"claude-sonnet-5-5", 6, 31}}, 6.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db, _ := newUsageTestServer(t)
			defer db.Close()
			for i, row := range tc.rows {
				seedRatioUsage(t, db, row.model, row.cost, at.AddDate(0, 0, -row.age).Add(time.Duration(i)*time.Nanosecond))
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ratio, w, err := measureFallbackCacheRatios(tx, "anthropic/claude-sonnet-5-5-20261001", at)
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(ratio-tc.want) > 1e-9 || w != .12 {
				t.Fatalf("ratio/w = %v/%v, want %v/.12", ratio, w, tc.want)
			}
		})
	}
}

func TestFallbackCacheRatiosZeroIOAndMeasuredWrites(t *testing.T) {
	_, db, _ := newUsageTestServer(t)
	defer db.Close()
	at := time.Now().UTC()
	for i := 0; i < 3; i++ {
		seedRatioUsage(t, db, "claude-sonnet-5-5", float64(i+1), at)
	}
	if _, err := db.Exec(`UPDATE task_run_usage SET committed_input_tokens=0,committed_output_tokens=0,cache_read_tokens=100,cache_write_tokens=25`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ratio, w, err := measureFallbackCacheRatios(tx, "claude-sonnet-5-5", at)
	if err != nil {
		t.Fatal(err)
	}
	if ratio != 6.5 || w != .25 {
		t.Fatalf("ratio/w = %v/%v", ratio, w)
	}
}

func TestFallbackCacheRatiosExpire(t *testing.T) {
	s, db, _ := newUsageTestServer(t)
	defer db.Close()
	at := time.Now().UTC()
	read := func(at time.Time) float64 {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		r, _, err := s.fallbackCacheRatios(tx, "claude-sonnet-5-5", at)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if got := read(at); got != 6.5 {
		t.Fatal(got)
	}
	for i := 0; i < 3; i++ {
		seedRatioUsage(t, db, "claude-sonnet-5-5", 6, at.Add(-time.Duration(i)*time.Second))
	}
	if got := read(at.Add(9 * time.Minute)); got != 6.5 {
		t.Fatal(got)
	}
	if got := read(at.Add(10 * time.Minute)); got != 2 {
		t.Fatal(got)
	}
}

func recordUsage(t *testing.T, s *Server, claw string, snapshots ...taskRunUsageSnapshot) {
	t.Helper()
	for _, snap := range snapshots {
		if err := s.recordTaskRunUsage(claw, snap); err != nil {
			t.Fatal(err)
		}
	}
}

func assertUsageCosts(t *testing.T, db *sql.DB, cost, approximate float64, read, write int64, approximateFlag bool) {
	t.Helper()
	var committed, daily, approx, summary, components float64
	var cr, cw, dr, dw int64
	var rowFlag, summaryFlag bool
	if err := db.QueryRow(`SELECT committed_cost_usd,committed_cache_read_tokens,committed_cache_write_tokens,cost_approximate,committed_input_cost_usd+committed_output_cost_usd+committed_cache_read_cost_usd+committed_cache_write_cost_usd FROM task_run_usage WHERE session_key='a'`).Scan(&committed, &cr, &cw, &rowFlag, &components); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT SUM(cost_usd),SUM(approx_cost_usd),SUM(cache_read_tokens),SUM(cache_write_tokens) FROM usage_daily`).Scan(&daily, &approx, &dr, &dw); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT estimated_cost_usd,cost_approximate FROM task_run_summaries WHERE run_id='run-usage'`).Scan(&summary, &summaryFlag); err != nil {
		t.Fatal(err)
	}
	for label, value := range map[string]float64{"committed": committed, "daily": daily, "summary": summary} {
		if math.Abs(value-cost) > 1e-9 {
			t.Errorf("%s=%v, want %v", label, value, cost)
		}
	}
	if math.Abs(approx-approximate) > 1e-9 || cr != read || cw != write || dr != read || dw != write || rowFlag != approximateFlag || summaryFlag != approximateFlag {
		t.Fatalf("approx=%v tokens=%d/%d daily=%d/%d flags=%v/%v, want approx=%v tokens=%d/%d flag=%v", approx, cr, cw, dr, dw, rowFlag, summaryFlag, approximate, read, write, approximateFlag)
	}
	if components > committed+1e-9 {
		t.Fatalf("components=%v exceed total=%v", components, committed)
	}
}

func TestTaskRunUsageEstimatedCacheComponents(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	snap := usageSnapshot("a", 100000, 10000, 1, "claude-sonnet-5-5")
	recordUsage(t, s, claw, snap, snap)
	assertUsageCosts(t, db, 1.95, 1.95, 3300000, 396000, true)
	var in, out, read, write, est, runCost float64
	var runEstimated bool
	if err := db.QueryRow(`SELECT committed_input_cost_usd,committed_output_cost_usd,committed_cache_read_cost_usd,committed_cache_write_cost_usd,est_cache_cost_usd,estimated_cost_usd,run_cache_estimated FROM task_run_usage WHERE session_key='a'`).Scan(&in, &out, &read, &write, &est, &runCost, &runEstimated); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string][2]float64{"input": {in, .2}, "output": {out, .1}, "read": {read, .66}, "write": {write, .99}, "estimate": {est, 1.65}, "run": {runCost, 1.95}} {
		if math.Abs(v[0]-v[1]) > 1e-9 {
			t.Errorf("%s=%v, want %v", k, v[0], v[1])
		}
	}
	if !runEstimated {
		t.Fatal("run estimate flag missing")
	}
}

func TestTaskRunUsageReversesEstimatesForRealCounters(t *testing.T) {
	for _, newRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("newRun=%v", newRun), func(t *testing.T) {
			s, db, claw := newUsageTestServer(t)
			defer db.Close()
			snap := usageSnapshot("a", 100000, 10000, 1, "claude-sonnet-5-5")
			recordUsage(t, s, claw, snap)
			want := .75
			if newRun {
				snap.InputTokens = ptr(200000)
				snap.OutputTokens = ptr(20000)
				want = 1.35
			}
			snap.CacheReadTokens = ptr(1000000)
			snap.CacheWriteTokens = ptr(100000)
			recordUsage(t, s, claw, snap, snap)
			assertUsageCosts(t, db, want, 0, 1000000, 100000, false)
			var left int
			if err := db.QueryRow(`SELECT est_cache_read_tokens+est_cache_write_tokens+run_cache_estimated FROM task_run_usage WHERE session_key='a'`).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 0 {
				t.Fatal("estimate left over", left)
			}
		})
	}
}

func TestTaskRunUsageCorrectsEstimatedCache(t *testing.T) {
	for _, realCounters := range []bool{false, true} {
		t.Run(fmt.Sprintf("realCounters=%v", realCounters), func(t *testing.T) {
			s, db, claw := newUsageTestServer(t)
			defer db.Close()
			snap := usageSnapshot("a", 100000, 10000, 1, "claude-sonnet-5-5")
			recordUsage(t, s, claw, snap)
			snap.EstimatedCostUSD = ptr(1.0)
			if realCounters {
				snap.CacheReadTokens = ptr(0)
				snap.CacheWriteTokens = ptr(0)
			}
			recordUsage(t, s, claw, snap, snap)
			assertUsageCosts(t, db, 1, 0, 0, 0, false)
			var sum float64
			if err := db.QueryRow(`SELECT committed_input_cost_usd+committed_output_cost_usd+committed_cache_read_cost_usd+committed_cache_write_cost_usd+run_input_cost_usd+run_output_cost_usd+run_cache_read_tokens+run_cache_write_tokens+run_cache_read_cost_usd+run_cache_write_cost_usd+est_cache_cost_usd FROM task_run_usage WHERE session_key='a'`).Scan(&sum); err != nil {
				t.Fatal(err)
			}
			if math.Abs(sum) > 1e-9 {
				t.Fatal("components left over", sum)
			}
		})
	}
}

func TestTaskRunUsageRealCacheIsExact(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	snap := usageSnapshot("a", 100000, 10000, 1, "claude-sonnet-5-5")
	snap.CacheReadTokens = ptr(1000000)
	snap.CacheWriteTokens = ptr(100000)
	recordUsage(t, s, claw, snap, snap)
	assertUsageCosts(t, db, .75, 0, 1000000, 100000, false)
}

func TestTaskRunUsageReversalPreservesDayAndModel(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return at }
	snap := usageSnapshot("a", 100000, 10000, 1, "claude-sonnet-5-5")
	recordUsage(t, s, claw, snap)
	at = at.AddDate(0, 0, 1)
	snap.InputTokens = ptr(200000)
	snap.OutputTokens = ptr(20000)
	snap.Model = "claude-sonnet-4-6"
	recordUsage(t, s, claw, snap)
	// Correct only the latest run. The first run remains approximate.
	snap.EstimatedCostUSD = ptr(2.0)
	recordUsage(t, s, claw, snap)
	assertUsageCosts(t, db, 3.95, 1.95, 3300000, 396000, true)
	ledger := func() string {
		var out string
		if err := db.QueryRow(`SELECT group_concat(day||'/'||model||'/'||cost_usd||'/'||approx_cost_usd||'/'||cache_read_tokens||'/'||cache_write_tokens,';') FROM (SELECT * FROM usage_daily ORDER BY day)`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := ledger()
	// First real counters on the gateway-priced session become a baseline:
	// they mix cache the gateway already charged with the first run's, so the
	// first run keeps its estimate instead of losing its cache cost.
	snap.CacheReadTokens = ptr(9000000)
	snap.CacheWriteTokens = ptr(900000)
	recordUsage(t, s, claw, snap)
	assertUsageCosts(t, db, 3.95, 1.95, 3300000, 396000, true)
	if after := ledger(); after != before {
		t.Fatalf("ledger changed on baseline:\n%s\n%s", before, after)
	}
}

func TestFallbackCacheCostRatioUsesExactHubPricedRuns(t *testing.T) {
	_, db, _ := newUsageTestServer(t)
	defer db.Close()
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, cost := range []float64{6, 12, 18, 900, 800} {
		seedRatioUsage(t, db, "claude-sonnet-5-5", cost, at.Add(time.Duration(i)*time.Nanosecond))
	}
	// Hub-priced rows with real cache counters are exact. The 900 and 800 rows
	// carry estimates, hub- and gateway-priced, and must not feed the ratio.
	if _, err := db.Exec(`UPDATE task_run_usage SET cost_source=CASE WHEN committed_cost_usd=800 THEN 'gateway' ELSE 'hub_pricing' END,cache_read_tokens=1,cost_approximate=CASE WHEN committed_cost_usd>=800 THEN 1 ELSE 0 END`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ratio, _, err := measureFallbackCacheRatios(tx, "claude-sonnet-5-5", at)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(ratio-4) > 1e-9 {
		t.Fatalf("ratio=%v, want 36/9=4", ratio)
	}
}

func TestTaskRunUsageColdCacheAfterGatewayRunKeepsEstimate(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	gateway := usageSnapshot("a", 100000, 10000, 1, "claude-sonnet-4-6")
	gateway.EstimatedCostUSD = ptr(2.0)
	// The model switches; the first sessions.usage poll comes back cold, so
	// the run is estimated and the hub-priced source is the latest one.
	fallback := usageSnapshot("a", 200000, 20000, 1, "claude-sonnet-5-5")
	recordUsage(t, s, claw, gateway, fallback)
	var before float64
	if err := db.QueryRow(`SELECT committed_cost_usd FROM task_run_usage WHERE session_key='a'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if math.Abs(before-5.9) > 1e-9 {
		t.Fatalf("before=%v, want 2 gateway + 0.6*6.5 estimate", before)
	}
	// Cumulative counters include the gateway run's cache, already charged.
	fallback.CacheReadTokens = ptr(9000000)
	fallback.CacheWriteTokens = ptr(900000)
	recordUsage(t, s, claw, fallback)
	var after, daily float64
	var approx bool
	if err := db.QueryRow(`SELECT committed_cost_usd,cost_approximate FROM task_run_usage WHERE session_key='a'`).Scan(&after, &approx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM usage_daily`).Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if math.Abs(after-before) > 1e-9 || math.Abs(daily-before) > 1e-9 || !approx {
		t.Fatalf("after=%v daily=%v approx=%v, want baseline keeping %v", after, daily, approx, before)
	}
}

func TestFallbackCacheCostRatioWeightsTokenPrices(t *testing.T) {
	_, db, _ := newUsageTestServer(t)
	defer db.Close()
	at := time.Now().UTC()
	for _, cost := range []float64{6, 12, 18} {
		seedRatioUsage(t, db, "claude-sonnet-5-5", cost, at)
	}
	if _, err := db.Exec(`UPDATE task_run_usage SET committed_input_tokens=2000000,committed_output_tokens=200000 WHERE committed_cost_usd=18`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ratio, _, err := measureFallbackCacheRatios(tx, "claude-sonnet-5-5", at)
	if err != nil {
		t.Fatal(err)
	}
	if ratio != 3 {
		t.Fatalf("ratio=%v, want 36/12=3", ratio)
	}
}

func TestTaskRunUsageRealCountersReverseEveryEstimateBucket(t *testing.T) {
	s, db, claw := newUsageTestServer(t)
	defer db.Close()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return at }
	snap := usageSnapshot("a", 100000, 10000, 1, "claude-sonnet-5-5")
	recordUsage(t, s, claw, snap)
	at = at.AddDate(0, 0, 1)
	snap.InputTokens = ptr(200000)
	snap.OutputTokens = ptr(20000)
	snap.Model = "claude-sonnet-4-6"
	recordUsage(t, s, claw, snap)
	snap.CacheReadTokens = ptr(1000000)
	snap.CacheWriteTokens = ptr(100000)
	recordUsage(t, s, claw, snap, snap)
	assertUsageCosts(t, db, 1.875, 0, 1000000, 100000, false)
	var firstCost, firstApprox float64
	var firstTokens int64
	if err := db.QueryRow(`SELECT cost_usd,approx_cost_usd,cache_read_tokens+cache_write_tokens FROM usage_daily WHERE day='2026-10-10'`).Scan(&firstCost, &firstApprox, &firstTokens); err != nil {
		t.Fatal(err)
	}
	if math.Abs(firstCost-.3) > 1e-9 || math.Abs(firstApprox) > 1e-9 || firstTokens != 0 {
		t.Fatalf("first day=%v/%v/%d", firstCost, firstApprox, firstTokens)
	}
	var current float64
	if err := db.QueryRow(`SELECT estimated_cost_usd FROM task_run_usage WHERE session_key='a'`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if math.Abs(current-1.575) > 1e-9 {
		t.Fatalf("current=%v", current)
	}
}

func TestTaskRunUsageLegacyEstimateReversal(t *testing.T) {
	for _, gateway := range []bool{false, true} {
		t.Run(fmt.Sprintf("gateway=%v", gateway), func(t *testing.T) {
			s, db, claw := newUsageTestServer(t)
			defer db.Close()
			// Preserve a fractional historical total through integer token rounding.
			if _, err := db.Exec(`INSERT INTO task_run_usage(id,tenant_id,run_id,session_key,model,input_tokens,output_tokens,committed_input_tokens,committed_output_tokens,committed_cost_usd,estimated_cost_usd,cost_source,usage_day,cost_note,first_seen_at,updated_at) VALUES('legacy','tenant','run-usage','a','claude-sonnet-5-5',100000,10000,100000,10000,1.95000013,1.95000013,'hub_pricing','2026-10-10','cache_estimate_x6.5',0,0)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO usage_daily(tenant_id,day,model,cost_usd,updated_at) VALUES('tenant','2026-10-10','claude-sonnet-5-5',1.95000013,0)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DELETE FROM hub_migrations WHERE name='fallback_cost_breakdown_v1'`); err != nil {
				t.Fatal(err)
			}
			if err := backfillFallbackCostBreakdownV1(db); err != nil {
				t.Fatal(err)
			}
			snap := usageSnapshot("a", 100000, 10000, 1, "claude-sonnet-5-5")
			if gateway {
				snap.EstimatedCostUSD = ptr(.8)
				recordUsage(t, s, claw, snap)
				assertUsageCosts(t, db, .8, 0, 0, 0, false)
			} else {
				snap.CacheReadTokens = ptr(1000000)
				snap.CacheWriteTokens = ptr(100000)
				recordUsage(t, s, claw, snap)
				assertUsageCosts(t, db, .75, 0, 1000000, 100000, false)
			}
			var note string
			if err := db.QueryRow(`SELECT cost_note FROM task_run_usage WHERE session_key='a'`).Scan(&note); err != nil {
				t.Fatal(err)
			}
			if note != "" {
				t.Fatalf("legacy note left over: %s", note)
			}
		})
	}
}
