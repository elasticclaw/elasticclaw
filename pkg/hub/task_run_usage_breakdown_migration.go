package hub

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
)

func backfillFallbackCostBreakdownV1(db *sql.DB) error {
	const migration = "fallback_cost_breakdown_v1"
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS hub_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_migrations WHERE name=?`, migration).Scan(&applied); err != nil {
		return err
	}
	if applied > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type usageRow struct {
		id, tenant, runID, session, model, day, note, source string
		workspace, factory, workflow                         string
		in, out, committedIn, committedOut                   int64
		read, write                                          sql.NullInt64
		committedCost                                        float64
		runCost                                              sql.NullFloat64
	}
	rows, err := tx.Query(`SELECT u.id,u.tenant_id,u.run_id,u.session_key,u.model,u.usage_day,u.cost_note,u.cost_source,tr.workspace_name,tr.factory_name,tr.workflow_name,u.input_tokens,u.output_tokens,u.committed_input_tokens,u.committed_output_tokens,u.cache_read_tokens,u.cache_write_tokens,u.committed_cost_usd,u.estimated_cost_usd FROM task_run_usage u JOIN task_runs tr ON tr.tenant_id=u.tenant_id AND tr.id=u.run_id WHERE u.cost_source='hub_pricing' OR u.cost_note LIKE 'cache_estimate_x%'`)
	if err != nil {
		return fmt.Errorf("select fallback cost breakdown: %w", err)
	}
	var pending []usageRow
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.id, &r.tenant, &r.runID, &r.session, &r.model, &r.day, &r.note, &r.source, &r.workspace, &r.factory, &r.workflow, &r.in, &r.out, &r.committedIn, &r.committedOut, &r.read, &r.write, &r.committedCost, &r.runCost); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	runs := map[[2]string]bool{}
	for _, r := range pending {
		if r.source == "gateway" {
			// A gateway correction after the legacy estimate: the committed cost
			// still carries earlier estimated runs, but its components are not
			// the hub's to split. Only flag it approximate.
			if _, err := tx.Exec(`UPDATE task_run_usage SET cost_approximate=1 WHERE id=?`, r.id); err != nil {
				return fmt.Errorf("flag corrected legacy estimate: %w", err)
			}
			if _, err := tx.Exec(`UPDATE usage_daily SET approx_cost_usd=approx_cost_usd+?,updated_at=? WHERE tenant_id=? AND day=? AND workspace_name=? AND factory_name=? AND workflow_name=? AND model=?`, r.committedCost, now().UnixMilli(), r.tenant, r.day, r.workspace, r.factory, r.workflow, r.model); err != nil {
				return fmt.Errorf("flag corrected legacy estimate in usage_daily: %w", err)
			}
			runs[[2]string{r.tenant, r.runID}] = true
			continue
		}
		p, ok := modelPrice(tx, r.model)
		if !ok {
			continue
		}
		inputCost, outputCost := float64(r.committedIn)*p.input, float64(r.committedOut)*p.output
		runInputCost, runOutputCost := float64(r.in)*p.input, float64(r.out)*p.output
		read, write := r.read.Int64, r.write.Int64
		readCost, writeCost := float64(read)*p.cacheRead, float64(write)*p.cacheWrite
		var estRead, estWrite int64
		var estCost, estReadCost, estWriteCost float64
		estimated := strings.HasPrefix(r.note, "cache_estimate_x")
		if estimated {
			// Real cache counters reported after the legacy estimate are exact;
			// only the remainder is estimated.
			estCost = math.Max(0, r.committedCost-inputCost-outputCost-readCost-writeCost)
			estRead, estWrite, estReadCost, estWriteCost = legacyFallbackCacheBreakdown(estCost, p)
			read, write, readCost, writeCost = read+estRead, write+estWrite, readCost+estReadCost, writeCost+estWriteCost
		}
		var runRead, runWrite int64
		var runReadCost, runWriteCost float64
		runEstimated := 0
		// Legacy rows retained the last run's total, which recovers its cache
		// contribution but not the exact read/write split of a real delta.
		if cacheCost := readCost + writeCost; cacheCost > 0 && r.runCost.Valid {
			fraction := math.Min(1, math.Max(0, r.runCost.Float64-runInputCost-runOutputCost)/cacheCost)
			runRead, runWrite = int64(math.Round(float64(read)*fraction)), int64(math.Round(float64(write)*fraction))
			runReadCost, runWriteCost = readCost*fraction, writeCost*fraction
			if estimated && fraction > 0 {
				runEstimated = 1
			}
		}
		approximate := 0
		if estCost > 0 || r.note != "" {
			approximate = 1
		}
		if _, err := tx.Exec(`UPDATE task_run_usage SET committed_input_cost_usd=?,committed_output_cost_usd=?,committed_cache_read_tokens=?,committed_cache_write_tokens=?,committed_cache_read_cost_usd=?,committed_cache_write_cost_usd=?,est_cache_read_tokens=?,est_cache_write_tokens=?,est_cache_cost_usd=?,est_cache_read_cost_usd=?,est_cache_write_cost_usd=?,run_input_cost_usd=?,run_output_cost_usd=?,run_cache_read_tokens=?,run_cache_write_tokens=?,run_cache_read_cost_usd=?,run_cache_write_cost_usd=?,run_cache_estimated=?,cost_approximate=? WHERE id=?`, inputCost, outputCost, read, write, readCost, writeCost, estRead, estWrite, estCost, estReadCost, estWriteCost, runInputCost, runOutputCost, runRead, runWrite, runReadCost, runWriteCost, runEstimated, approximate, r.id); err != nil {
			return fmt.Errorf("backfill fallback cost components: %w", err)
		}
		if estimated {
			if _, err := tx.Exec(`INSERT INTO task_run_usage_estimates(tenant_id,run_id,session_key,day,model,input_cost_usd,output_cost_usd,cache_read_tokens,cache_write_tokens,cache_read_cost_usd,cache_write_cost_usd) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, r.tenant, r.runID, r.session, r.day, r.model, inputCost, outputCost, estRead, estWrite, estReadCost, estWriteCost); err != nil {
				return fmt.Errorf("backfill fallback estimate buckets: %w", err)
			}
			if _, err := tx.Exec(`INSERT INTO usage_daily(tenant_id,day,workspace_name,factory_name,workflow_name,model,cache_read_tokens,cache_write_tokens,approx_cost_usd,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,day,workspace_name,factory_name,workflow_name,model) DO UPDATE SET cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,approx_cost_usd=approx_cost_usd+excluded.approx_cost_usd,updated_at=excluded.updated_at`, r.tenant, r.day, r.workspace, r.factory, r.workflow, r.model, estRead, estWrite, r.committedCost, now().UnixMilli()); err != nil {
				return fmt.Errorf("backfill fallback approximate daily cost: %w", err)
			}
		}
		runs[[2]string{r.tenant, r.runID}] = true
	}
	for k := range runs {
		if _, err := tx.Exec(`UPDATE task_run_summaries SET cost_approximate=EXISTS(SELECT 1 FROM task_run_usage WHERE tenant_id=? AND run_id=? AND cost_approximate=1) WHERE tenant_id=? AND run_id=?`, k[0], k[1], k[0], k[1]); err != nil {
			return fmt.Errorf("backfill fallback summary approximation: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO hub_migrations(name,applied_at) VALUES(?,?) ON CONFLICT(name) DO NOTHING`, migration, now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func legacyFallbackCacheBreakdown(cost float64, p tokenPrices) (read, write int64, readCost, writeCost float64) {
	const writeReadRatio = 0.12
	denominator := p.cacheRead + writeReadRatio*p.cacheWrite
	if cost <= 0 || denominator <= 0 {
		return 0, 0, cost, 0
	}
	readExact := cost / denominator
	read, write = int64(math.Round(readExact)), int64(math.Round(writeReadRatio*readExact))
	// Preserve the historical total exactly despite integer token rounding.
	readCost = readExact * p.cacheRead
	writeCost = cost - readCost
	return
}
