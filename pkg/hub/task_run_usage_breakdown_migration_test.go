package hub

import (
	"math"
	"testing"
)

func TestMigrateFallbackCostBreakdown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		note        string
		cacheRead   any
		cacheWrite  any
		cost        float64
		runCost     float64
		approximate int
	}{
		{name: "estimated", note: "cache_estimate_x6.5", cost: 1.95, runCost: 0.975, approximate: 1},
		{name: "real cache", cacheRead: 1_000_000, cacheWrite: 120_000, cost: 0.8, runCost: 0.4},
		{name: "input output only", cost: 0.3, runCost: 0.15},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db, _ := newUsageTestServer(t)
			defer db.Close()
			if _, err := db.Exec(`INSERT INTO task_run_usage(id,tenant_id,run_id,session_key,model,input_tokens,output_tokens,committed_input_tokens,committed_output_tokens,committed_cost_usd,estimated_cost_usd,cost_source,usage_day,cost_note,cache_read_tokens,cache_write_tokens,first_seen_at,updated_at) VALUES('legacy','tenant','run-usage','legacy','anthropic/Claude-Sonnet-5-5',50000,5000,100000,10000,?,?,'hub_pricing','2026-10-08',?,?,?,0,0)`, tc.cost, tc.runCost, tc.note, tc.cacheRead, tc.cacheWrite); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO usage_daily(tenant_id,day,model,cost_usd,updated_at) VALUES('tenant','2026-10-08','anthropic/Claude-Sonnet-5-5',?,0)`, tc.cost); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DELETE FROM hub_migrations WHERE name='fallback_cost_breakdown_v1'`); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := migrate(db); err != nil {
					t.Fatal(err)
				}
			}
			var inputCost, outputCost, readCost, writeCost, estCost, estReadCost, estWriteCost float64
			var runInputCost, runOutputCost, runReadCost, runWriteCost float64
			var read, write, estRead, estWrite, runRead, runWrite int64
			var approximate, runEstimated int
			if err := db.QueryRow(`SELECT committed_input_cost_usd,committed_output_cost_usd,committed_cache_read_tokens,committed_cache_write_tokens,committed_cache_read_cost_usd,committed_cache_write_cost_usd,est_cache_read_tokens,est_cache_write_tokens,est_cache_cost_usd,est_cache_read_cost_usd,est_cache_write_cost_usd,run_input_cost_usd,run_output_cost_usd,run_cache_read_tokens,run_cache_write_tokens,run_cache_read_cost_usd,run_cache_write_cost_usd,run_cache_estimated,cost_approximate FROM task_run_usage WHERE id='legacy'`).Scan(&inputCost, &outputCost, &read, &write, &readCost, &writeCost, &estRead, &estWrite, &estCost, &estReadCost, &estWriteCost, &runInputCost, &runOutputCost, &runRead, &runWrite, &runReadCost, &runWriteCost, &runEstimated, &approximate); err != nil {
				t.Fatal(err)
			}
			if math.Abs(inputCost-0.2) > 1e-12 || math.Abs(outputCost-0.1) > 1e-12 || math.Abs(inputCost+outputCost+readCost+writeCost-tc.cost) > 1e-12 {
				t.Fatalf("components = %v/%v/%v/%v, want total %v", inputCost, outputCost, readCost, writeCost, tc.cost)
			}
			if math.Abs(runInputCost+runOutputCost+runReadCost+runWriteCost-tc.runCost) > 1e-12 {
				t.Fatalf("current run components do not sum to %v", tc.runCost)
			}
			var summaryApproximate int
			var dailyCost, dailyApproximate float64
			var dailyRead, dailyWrite int64
			if err := db.QueryRow(`SELECT cost_approximate FROM task_run_summaries WHERE run_id='run-usage'`).Scan(&summaryApproximate); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT cost_usd,approx_cost_usd,cache_read_tokens,cache_write_tokens FROM usage_daily WHERE tenant_id='tenant' AND day='2026-10-08'`).Scan(&dailyCost, &dailyApproximate, &dailyRead, &dailyWrite); err != nil {
				t.Fatal(err)
			}
			if approximate != tc.approximate || summaryApproximate != tc.approximate || math.Abs(dailyCost-tc.cost) > 1e-12 || math.Abs(dailyApproximate-float64(tc.approximate)*tc.cost) > 1e-12 {
				t.Fatalf("approximate row/summary=%d/%d daily cost/approx=%v/%v", approximate, summaryApproximate, dailyCost, dailyApproximate)
			}
			var buckets int
			if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_usage_estimates`).Scan(&buckets); err != nil {
				t.Fatal(err)
			}
			if tc.approximate == 1 {
				if read != 3_300_000 || write != 396_000 || estRead != read || estWrite != write || dailyRead != read || dailyWrite != write || runRead != read/2 || runWrite != write/2 || runEstimated != 1 || buckets != 1 || math.Abs(estCost-1.65) > 1e-12 || math.Abs(estReadCost+estWriteCost-estCost) > 1e-12 {
					t.Fatalf("estimated cache = %d/%d, stored %d/%d, daily %d/%d, current %d/%d, cost %v", read, write, estRead, estWrite, dailyRead, dailyWrite, runRead, runWrite, estCost)
				}
				var bucketCost float64
				if err := db.QueryRow(`SELECT input_cost_usd+output_cost_usd+cache_read_cost_usd+cache_write_cost_usd FROM task_run_usage_estimates`).Scan(&bucketCost); err != nil {
					t.Fatal(err)
				}
				if math.Abs(bucketCost-tc.cost) > 1e-12 {
					t.Fatalf("estimate bucket cost=%v, want %v", bucketCost, tc.cost)
				}
			} else if estRead != 0 || estWrite != 0 || estCost != 0 || estReadCost != 0 || estWriteCost != 0 || runEstimated != 0 || buckets != 0 {
				t.Fatal("real fallback has an estimated contribution")
			}
		})
	}
}

func TestMigrateFallbackCostBreakdownLegacyEdges(t *testing.T) {
	_, db, _ := newUsageTestServer(t)
	defer db.Close()
	// "mixed": a legacy estimate that later received real counters ($0.45 of
	// real cache); "corrected": a legacy estimate whose last run the gateway
	// priced.
	if _, err := db.Exec(`INSERT INTO task_run_usage(id,tenant_id,run_id,session_key,model,input_tokens,output_tokens,committed_input_tokens,committed_output_tokens,committed_cost_usd,estimated_cost_usd,cost_source,usage_day,cost_note,cache_read_tokens,cache_write_tokens,first_seen_at,updated_at) VALUES
		('mixed','tenant','run-usage','mixed','claude-sonnet-5-5',50000,5000,100000,10000,2.40,1.20,'hub_pricing','2026-10-08','cache_estimate_x6.5',1000000,100000,0,0),
		('corrected','tenant','run-usage','corrected','claude-sonnet-5-5',50000,5000,100000,10000,2.95,1.00,'gateway','2026-10-08','cache_estimate_x6.5',NULL,NULL,0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage_daily(tenant_id,day,model,cost_usd,updated_at) VALUES('tenant','2026-10-08','claude-sonnet-5-5',5.35,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM hub_migrations WHERE name='fallback_cost_breakdown_v1'`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var estCost, cacheCost float64
	var read, estRead int64
	if err := db.QueryRow(`SELECT est_cache_cost_usd,committed_cache_read_cost_usd+committed_cache_write_cost_usd,committed_cache_read_tokens,est_cache_read_tokens FROM task_run_usage WHERE id='mixed'`).Scan(&estCost, &cacheCost, &read, &estRead); err != nil {
		t.Fatal(err)
	}
	// $2.40 − $0.30 input/output − $0.45 real cache = $1.65 estimated.
	if math.Abs(estCost-1.65) > 1e-12 || math.Abs(cacheCost-2.10) > 1e-12 || read != 1_000_000+estRead {
		t.Fatalf("mixed row est=%v cache=%v read=%d est read=%d", estCost, cacheCost, read, estRead)
	}
	var approximate int
	if err := db.QueryRow(`SELECT cost_approximate FROM task_run_usage WHERE id='corrected'`).Scan(&approximate); err != nil {
		t.Fatal(err)
	}
	var dailyApprox float64
	if err := db.QueryRow(`SELECT approx_cost_usd FROM usage_daily WHERE day='2026-10-08'`).Scan(&dailyApprox); err != nil {
		t.Fatal(err)
	}
	if approximate != 1 || math.Abs(dailyApprox-5.35) > 1e-12 {
		t.Fatalf("corrected row approximate=%d daily approx=%v, want 1 and 5.35", approximate, dailyApprox)
	}
}
