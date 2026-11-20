package hub

import (
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/harness/openclaw"
)

type cumulativeUsageStep struct {
	snapshot taskRunUsageSnapshot
	day      int
	runID    string
	epoch    string
}

func TestTaskRunUsageCumulativeEquivalence(t *testing.T) {
	const sonnet = "claude-sonnet-5-5"
	sample := func(in, out int, model string, cost *float64) taskRunUsageSnapshot {
		s := usageSnapshot("a", in, out, 1, model)
		s.EstimatedCostUSD = cost
		return s
	}
	cache := func(s taskRunUsageSnapshot, read, write int) taskRunUsageSnapshot {
		s.CacheReadTokens, s.CacheWriteTokens = ptr(read), ptr(write)
		return s
	}
	session := func(s taskRunUsageSnapshot, key string) taskRunUsageSnapshot {
		s.SessionKey = key
		return s
	}
	steps := func(snapshots ...taskRunUsageSnapshot) []cumulativeUsageStep {
		result := make([]cumulativeUsageStep, len(snapshots))
		for i, snapshot := range snapshots {
			result[i].snapshot = snapshot
		}
		return result
	}
	estimated := sample(100000, 10000, sonnet, nil)
	larger := sample(200000, 20000, sonnet, nil)
	priced := sample(100000, 10000, sonnet, ptr(2.0))
	zero := sample(100000, 10000, sonnet, ptr(0.0))
	corrected := sample(100000, 10000, sonnet, ptr(1.0))
	otherModel := sample(200000, 20000, "claude-sonnet-4-6", nil)
	otherCorrection := sample(200000, 20000, "claude-sonnet-4-6", ptr(2.0))
	partial := estimated
	partial.InputTokens = nil
	partial.OutputTokens = ptr(20000)
	nilTokens := partial
	nilTokens.InputTokens, nilTokens.OutputTokens = nil, nil
	nilTokens.EstimatedCostUSD = ptr(.75)
	contextChanged := priced
	contextChanged.TotalTokens = ptr(999999)

	cases := []struct {
		name  string
		steps []cumulativeUsageStep
	}{
		{"runs and repeated heartbeat", steps(
			sample(10000, 1000, "claude-sonnet-4-6", ptr(.05)),
			sample(10000, 1000, "claude-sonnet-4-6", ptr(.05)),
			sample(50000, 2000, "claude-sonnet-4-6", ptr(.20)),
			sample(60000, 2500, "claude-sonnet-4-6", ptr(.30)),
			sample(5000, 500, "claude-sonnet-4-6", ptr(.01)),
			session(sample(3000, 300, "claude-sonnet-4-6", nil), "b"))},
		{"identical consecutive runs count once", steps(priced, priced, contextChanged)},
		{"gateway cost survives costless heartbeat", steps(estimated, priced, estimated)},
		{"nil cost on a new run after a priced run", steps(priced, larger, larger, sample(300000, 30000, sonnet, ptr(3.0)))},
		{"missing token fields retain last values", steps(estimated, partial, nilTokens, nilTokens)},
		{"last nonempty model", steps(estimated, sample(100000, 10000, "", nil))},
		{"model changes with run", steps(estimated, sample(200000, 20000, "gpt-5-mini", nil))},
		{"correction targets original model", steps(estimated, sample(100000, 10000, "claude-opus-4-8", ptr(.0001)))},
		{"zero cost uses hub pricing", steps(zero, zero, sample(200000, 20000, sonnet, ptr(0.0)))},
		{"zero cost preserves known cost", steps(priced, zero, zero, larger)},
		{"zero cost replaced by gateway correction", steps(zero, corrected, corrected)},
		{"unknown model", steps(sample(100, 40, "unknown-model", nil), sample(100, 40, "unknown-model", ptr(0.0)), sample(200, 80, "unknown-model", nil))},
		{"estimated cache repeat", steps(estimated, estimated)},
		{"real cache exact", steps(cache(estimated, 1000000, 100000), cache(estimated, 1000000, 100000))},
		{"real counters reverse same run estimate", steps(estimated, cache(estimated, 1000000, 100000), cache(estimated, 1000000, 100000))},
		{"real counters reverse estimates on new run", steps(estimated, cache(larger, 1000000, 100000), cache(larger, 1000000, 100000))},
		{"gateway corrects estimated cache", steps(estimated, corrected, corrected)},
		{"gateway corrects cache with real counters", steps(estimated, cache(corrected, 0, 0), cache(corrected, 0, 0))},
		{"gateway corrects exact cache", steps(cache(zero, 1000000, 100000), sample(100000, 10000, sonnet, ptr(.8)))},
		{"cache-only growth and gateway-priced session", steps(
			cache(zero, 1000000, 100000), cache(zero, 2000000, 100000),
			cache(sample(200000, 20000, sonnet, ptr(0.0)), 2000000, 300000),
			session(cache(sample(100000, 10000, sonnet, ptr(1.5)), 9000000, 900000), "b"))},
		{"first counters on gateway-priced session", steps(priced,
			cache(sample(200000, 20000, sonnet, ptr(0.0)), 2000000, 0),
			cache(sample(200000, 20000, sonnet, ptr(0.0)), 3000000, 0))},
		{"stale cache snapshot", steps(cache(zero, 1000000, 0), cache(zero, 2000000, 0), cache(zero, 1000000, 0), cache(zero, 2000000, 0))},
		{"cold cache after gateway run", steps(sample(100000, 10000, "claude-sonnet-4-6", ptr(2.0)), larger, cache(larger, 9000000, 900000))},
		{"cross-day correction uses original day", []cumulativeUsageStep{
			{snapshot: sample(1000000, 1000000, "claude-sonnet-5", nil)},
			{snapshot: session(sample(100, 50, "claude-sonnet-5", ptr(7.0)), "b"), day: 1},
			{snapshot: sample(1000000, 1000000, "claude-sonnet-5", ptr(3.0)), day: 1},
		}},
		{"cache correction preserves day and model", []cumulativeUsageStep{
			{snapshot: estimated}, {snapshot: otherModel, day: 1},
			{snapshot: otherCorrection, day: 1}, {snapshot: cache(otherCorrection, 9000000, 900000), day: 1},
		}},
		{"real counters reverse every estimate bucket", []cumulativeUsageStep{
			{snapshot: estimated}, {snapshot: otherModel, day: 1},
			{snapshot: cache(otherModel, 1000000, 100000), day: 1}, {snapshot: cache(otherModel, 1000000, 100000), day: 1},
		}},
		{"task run switch with same session", []cumulativeUsageStep{
			{snapshot: priced}, {snapshot: sample(200000, 20000, sonnet, ptr(3.0))},
			{snapshot: sample(200000, 20000, sonnet, ptr(3.0)), runID: "run-next"},
			{snapshot: sample(300000, 30000, sonnet, ptr(4.0)), runID: "run-next"},
		}},
		{"session change resets accumulator", steps(priced, session(larger, "b"), session(sample(300000, 30000, sonnet, ptr(3.0)), "b"))},
		{"task run switch without cost", []cumulativeUsageStep{
			{snapshot: priced}, {snapshot: larger},
			{snapshot: larger, runID: "run-next"},
			{snapshot: sample(200000, 20000, sonnet, ptr(3.0)), runID: "run-next"},
			{snapshot: sample(300000, 30000, sonnet, nil), runID: "run-next"},
		}},
		{"return to earlier session", steps(priced, larger, session(sample(50000, 5000, sonnet, ptr(.5)), "b"),
			sample(400000, 40000, sonnet, ptr(4.0)), session(sample(60000, 6000, sonnet, ptr(.6)), "b"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			states := replayCumulativeUsage(t, tc.steps)
			for i := range tc.steps {
				assertCumulativeUsageEqual(t, fmt.Sprintf("sample %d flat+usage", i), states[0][i], states[1][i])
				assertCumulativeUsageEqual(t, fmt.Sprintf("sample %d usage only", i), states[0][i], states[2][i])
			}
		})
	}
}

func TestTaskRunUsageCumulativeEpochChange(t *testing.T) {
	first := usageSnapshot("a", 100, 10, 1, "unknown-model")
	first.EstimatedCostUSD = ptr(1.0)
	restart := usageSnapshot("a", 200, 20, 1, "unknown-model")
	restart.EstimatedCostUSD = ptr(2.0)
	next := usageSnapshot("a", 300, 30, 1, "unknown-model")
	next.EstimatedCostUSD = ptr(3.0)
	steps := []cumulativeUsageStep{{snapshot: first}, {snapshot: restart, epoch: "restarted"}, {snapshot: next}}
	states := replayCumulativeUsage(t, steps)
	for i := range steps {
		assertCumulativeUsageEqual(t, fmt.Sprintf("sample %d flat+usage", i), states[0][i], states[1][i])
	}
	assertCumulativeUsageEqual(t, "before restart", states[0][0], states[2][0])
	// With no flat sample, a new epoch cannot distinguish usage already billed
	// from the run in progress. Rebase without billing that run, then resume.
	assertCumulativeUsageEqual(t, "restart rebase charges nothing", states[2][0], states[2][1])
	want := replayCumulativeUsage(t, []cumulativeUsageStep{{snapshot: first}, {snapshot: next}})
	assertCumulativeUsageEqual(t, "next run after rebase", want[0][1], states[2][2])
}

type cumulativeUsageState map[string][][]any

func replayCumulativeUsage(t *testing.T, steps []cumulativeUsageStep) [3][]cumulativeUsageState {
	t.Helper()
	var result [3][]cumulativeUsageState
	for mode := range result {
		s, db, claw := newUsageTestServer(t)
		t.Cleanup(func() { db.Close() })
		at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
		s.nowFunc = func() time.Time { return at }
		accumulator := openclaw.NewUsageAccumulator("initial")
		seen := make(map[string]bool)
		runID := "run-usage"
		for i, step := range steps {
			at = time.Date(2026, 10, 10+step.day, 12, 0, 0, 0, time.UTC)
			if step.runID != "" && step.runID != runID {
				runID = step.runID
				for _, query := range []string{
					`INSERT OR IGNORE INTO task_runs(id,tenant_id,initial_attempt_id,run_kind,owner_type,claw_id,created_at,updated_at) VALUES(?,'tenant','attempt-next','code_task','manual','claw-usage',0,0)`,
					`INSERT OR IGNORE INTO task_run_summaries(id,tenant_id,run_id,status,phase,started_at,last_event_at,materialized_at,updated_at) VALUES('summary-next','tenant',?,'running','claimed',0,0,0,0)`,
					`UPDATE claws SET task_run_id=? WHERE id='claw-usage'`,
				} {
					if _, err := db.Exec(query, runID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if step.epoch != "" {
				accumulator = openclaw.NewUsageAccumulator(step.epoch)
			}
			snapshot := step.snapshot
			if mode != 0 {
				cumulative := accumulator.Observe(snapshot.SessionKey, snapshot.InputTokens, snapshot.OutputTokens, snapshot.EstimatedCostUSD, snapshot.Model, snapshot.ModelProvider, snapshot.CacheReadTokens, snapshot.CacheWriteTokens)
				snapshot.Cumulative = &cumulative
				key := runID + "/" + snapshot.SessionKey
				if mode == 2 && seen[key] {
					// Context occupancy is metadata, absent from UsageSnapshot;
					// only billed input/output tokens and cost come from usage.
					snapshot.InputTokens, snapshot.OutputTokens, snapshot.EstimatedCostUSD = nil, nil, nil
				}
				seen[key] = true
			}
			if err := s.recordTaskRunUsage(claw, snapshot); err != nil {
				t.Fatalf("mode %d sample %d: %v", mode, i, err)
			}
			result[mode] = append(result[mode], readCumulativeUsageState(t, db))
		}
	}
	return result
}

func readCumulativeUsageState(t *testing.T, db *sql.DB) cumulativeUsageState {
	t.Helper()
	queries := map[string]string{
		"usage_daily":        `SELECT tenant_id,day,workspace_name,factory_name,workflow_name,model,input_tokens,output_tokens,total_tokens,cache_read_tokens,cache_write_tokens,cost_usd,approx_cost_usd FROM usage_daily ORDER BY tenant_id,day,workspace_name,factory_name,workflow_name,model`,
		"task_run_summaries": `SELECT tenant_id,run_id,input_tokens,output_tokens,total_tokens,estimated_cost_usd,cost_approximate FROM task_run_summaries ORDER BY tenant_id,run_id`,
		"task_run_usage": `SELECT tenant_id,run_id,session_key,model,model_provider,input_tokens,output_tokens,total_tokens,estimated_cost_usd,cost_source,usage_day,cache_read_tokens,cache_write_tokens,
			committed_input_tokens,committed_output_tokens,committed_total_tokens,committed_cost_usd,committed_input_cost_usd,committed_output_cost_usd,committed_cache_read_tokens,committed_cache_write_tokens,committed_cache_read_cost_usd,committed_cache_write_cost_usd,
			run_input_cost_usd,run_output_cost_usd,run_cache_read_tokens,run_cache_write_tokens,run_cache_read_cost_usd,run_cache_write_cost_usd,run_cache_estimated,
			est_cache_read_tokens,est_cache_write_tokens,est_cache_cost_usd,est_cache_read_cost_usd,est_cache_write_cost_usd,cost_approximate,cost_note FROM task_run_usage ORDER BY tenant_id,run_id,session_key`,
	}
	state := make(cumulativeUsageState)
	for name, query := range queries {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values, destinations := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			state[name] = append(state[name], values)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	return state
}

func assertCumulativeUsageEqual(t *testing.T, label string, want, got cumulativeUsageState) {
	t.Helper()
	for table, expected := range want {
		actual := got[table]
		if len(expected) != len(actual) {
			t.Fatalf("%s %s rows: got %d, want %d\ngot: %#v\nwant: %#v", label, table, len(actual), len(expected), actual, expected)
		}
		for row := range expected {
			for column, value := range expected[row] {
				if f, ok := value.(float64); ok {
					g, ok := actual[row][column].(float64)
					if ok && math.Abs(f-g) < 1e-9 {
						continue
					}
				}
				if !reflect.DeepEqual(value, actual[row][column]) {
					t.Fatalf("%s %s row %d column %d: got %#v, want %#v", label, table, row, column, actual[row][column], value)
				}
			}
		}
	}
}

func TestTaskRunUsageCumulativeBaseline(t *testing.T) {
	t.Run("cumulative-only first sample", func(t *testing.T) {
		flat := usageSnapshot("a", 100, 10, 0, "unknown-model")
		flat.EstimatedCostUSD = ptr(1.0)
		s, db, claw := newUsageTestServer(t)
		defer db.Close()
		accumulator := openclaw.NewUsageAccumulator("epoch")
		c := accumulator.Observe("a", flat.InputTokens, flat.OutputTokens, flat.EstimatedCostUSD, flat.Model, "provider", nil, nil)
		recordUsage(t, s, claw, taskRunUsageSnapshot{Cumulative: &c})
		var in, out int
		var cost float64
		var model, provider string
		if err := db.QueryRow(`SELECT committed_input_tokens,committed_output_tokens,committed_cost_usd,model,model_provider FROM task_run_usage`).Scan(&in, &out, &cost, &model, &provider); err != nil {
			t.Fatal(err)
		}
		if in != 100 || out != 10 || cost != 1 || model != flat.Model || provider != "provider" {
			t.Fatalf("first cumulative sample = %d/%d/%v/%s/%s", in, out, cost, model, provider)
		}
	})
	t.Run("legacy baseline and costless heartbeat", func(t *testing.T) {
		s, db, claw := newUsageTestServer(t)
		defer db.Close()
		flat := usageSnapshot("a", 100, 10, 1, "unknown-model")
		flat.EstimatedCostUSD = ptr(1.0)
		recordUsage(t, s, claw, flat)
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
		var epoch sql.NullString
		if err := db.QueryRow(`SELECT cum_epoch FROM task_run_usage`).Scan(&epoch); err != nil || epoch.Valid {
			t.Fatalf("legacy baseline = %v, err = %v", epoch, err)
		}
		before := readCumulativeUsageState(t, db)
		accumulator := openclaw.NewUsageAccumulator("epoch")
		c := accumulator.Observe("a", flat.InputTokens, flat.OutputTokens, flat.EstimatedCostUSD, flat.Model, "", nil, nil)
		flat.Cumulative = &c
		recordUsage(t, s, claw, flat)
		assertCumulativeUsageEqual(t, "legacy row adoption", before, readCumulativeUsageState(t, db))
		c.CostUSD, flat.EstimatedCostUSD = nil, nil
		recordUsage(t, s, claw, flat)
		var cumulativeCost float64
		if err := db.QueryRow(`SELECT cum_cost_usd FROM task_run_usage`).Scan(&cumulativeCost); err != nil || cumulativeCost != 1 {
			t.Fatalf("costless baseline = %v, err = %v", cumulativeCost, err)
		}
		assertCumulativeUsageEqual(t, "costless heartbeat", before, readCumulativeUsageState(t, db))
	})
	t.Run("mismatched session ignores cumulative sample", func(t *testing.T) {
		s, db, claw := newUsageTestServer(t)
		defer db.Close()
		flat := usageSnapshot("a", 100, 10, 1, "unknown-model")
		flat.EstimatedCostUSD = ptr(1.0)
		recordUsage(t, s, claw, flat)
		before := readCumulativeUsageState(t, db)
		c := openclaw.NewUsageAccumulator("epoch").Observe("b", ptr(999), ptr(999), ptr(9.0), "another-model", "", nil, nil)
		flat.Cumulative = &c
		recordUsage(t, s, claw, flat)
		assertCumulativeUsageEqual(t, "mismatched session", before, readCumulativeUsageState(t, db))
		var epoch sql.NullString
		if err := db.QueryRow(`SELECT cum_epoch FROM task_run_usage`).Scan(&epoch); err != nil || epoch.Valid {
			t.Fatalf("mismatched baseline = %v, err = %v", epoch, err)
		}
	})
	t.Run("baseline rolls back with ledger", func(t *testing.T) {
		s, db, claw := newUsageTestServer(t)
		defer db.Close()
		flat := usageSnapshot("a", 100, 10, 1, "unknown-model")
		flat.EstimatedCostUSD = ptr(1.0)
		accumulator := openclaw.NewUsageAccumulator("epoch")
		c := accumulator.Observe("a", flat.InputTokens, flat.OutputTokens, flat.EstimatedCostUSD, flat.Model, "", nil, nil)
		flat.Cumulative = &c
		recordUsage(t, s, claw, flat)
		before := readCumulativeUsageState(t, db)
		if _, err := db.Exec(`CREATE TRIGGER reject_usage_summary BEFORE UPDATE ON task_run_summaries BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
			t.Fatal(err)
		}
		c = accumulator.Observe("a", ptr(200), ptr(20), ptr(2.0), flat.Model, "", nil, nil)
		if err := s.recordTaskRunUsage(claw, taskRunUsageSnapshot{Cumulative: &c}); err == nil {
			t.Fatal("expected summary failure")
		}
		assertCumulativeUsageEqual(t, "failed transaction", before, readCumulativeUsageState(t, db))
		var in, out int
		var cost float64
		if err := db.QueryRow(`SELECT cum_input_tokens,cum_output_tokens,cum_cost_usd FROM task_run_usage`).Scan(&in, &out, &cost); err != nil {
			t.Fatal(err)
		}
		if in != 100 || out != 10 || cost != 1 {
			t.Fatalf("baseline escaped rollback: %d/%d/%v", in, out, cost)
		}
	})
}

// A plain difference of cumulative totals cannot see an empty run, and a
// restart without cost loses the cost base, so the flat run OpenClaw bridges
// send alongside the snapshot stays authoritative.
func TestTaskRunUsageCumulativeFlatStaysAuthoritative(t *testing.T) {
	sample := func(in, out int, cost *float64) taskRunUsageSnapshot {
		s := usageSnapshot("a", in, out, 1, "unknown-model")
		s.EstimatedCostUSD = cost
		return s
	}
	for _, tc := range []struct {
		name  string
		steps []cumulativeUsageStep
	}{
		{"empty run", []cumulativeUsageStep{{snapshot: sample(100, 10, ptr(1.0))}, {snapshot: sample(0, 0, ptr(0.0))}, {snapshot: sample(100, 10, ptr(2.0))}}},
		{"restart without cost", []cumulativeUsageStep{
			{snapshot: sample(100, 10, ptr(1.0))}, {snapshot: sample(200, 20, ptr(2.0))},
			{snapshot: sample(300, 30, nil), epoch: "restarted"}, {snapshot: sample(300, 30, ptr(4.0))},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := replayCumulativeUsage(t, tc.steps)
			for i := range tc.steps {
				assertCumulativeUsageEqual(t, fmt.Sprintf("sample %d flat+usage", i), states[0][i], states[1][i])
			}
		})
	}
}
