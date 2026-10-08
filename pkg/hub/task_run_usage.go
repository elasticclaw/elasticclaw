package hub

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

var unmatchedUsagePriceModels sync.Map

type taskRunUsageSnapshot struct {
	SessionKey                             string
	InputTokens, OutputTokens, TotalTokens *int
	EstimatedCostUSD                       *float64
	Model                                  string
	ModelProvider                          string
	// Session-cumulative cache tokens; the bridge sends them only when the
	// gateway cannot price the model, since its own cost already covers cache.
	CacheReadTokens, CacheWriteTokens *int
}

func (s *Server) recordTaskRunUsage(clawID string, snapshot taskRunUsageSnapshot) error {
	if snapshot.SessionKey == "" || (snapshot.InputTokens == nil && snapshot.OutputTokens == nil && snapshot.TotalTokens == nil) {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tenant, runID, workspace, factory, workflow string
	if err = tx.QueryRow(`SELECT tr.tenant_id,tr.id,tr.workspace_name,tr.factory_name,tr.workflow_name FROM claws c JOIN task_runs tr ON tr.id=c.task_run_id AND tr.tenant_id=c.tenant_id WHERE c.id=?`, clawID).Scan(&tenant, &runID, &workspace, &factory, &workflow); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	var oldIn, oldOut, oldTotal, comIn, comOut, comTotal, oldCacheRead, oldCacheWrite int
	var oldModel, oldUsageDay string
	var oldCost sql.NullFloat64
	var comCost float64
	oldSource := "gateway"
	err = tx.QueryRow(`SELECT model,input_tokens,output_tokens,total_tokens,committed_input_tokens,committed_output_tokens,committed_total_tokens,committed_cost_usd,estimated_cost_usd,cost_source,usage_day,COALESCE(cache_read_tokens,0),COALESCE(cache_write_tokens,0) FROM task_run_usage WHERE tenant_id=? AND run_id=? AND session_key=?`, tenant, runID, snapshot.SessionKey).Scan(&oldModel, &oldIn, &oldOut, &oldTotal, &comIn, &comOut, &comTotal, &comCost, &oldCost, &oldSource, &oldUsageDay, &oldCacheRead, &oldCacheWrite)
	found := err == nil
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	in, out, total := oldIn, oldOut, oldTotal
	if snapshot.InputTokens != nil {
		in = *snapshot.InputTokens
	}
	if snapshot.OutputTokens != nil {
		out = *snapshot.OutputTokens
	}
	if snapshot.TotalTokens != nil {
		total = *snapshot.TotalTokens
	}
	// OpenClaw overwrites a session entry after each reply with that run's
	// totals. total_tokens is context occupancy, not billed token usage.
	newRun := !found || in != oldIn || out != oldOut
	// On cost-only heartbeats keep the stored model so corrections target the
	// usage_daily bucket that received the run's tokens; a model change takes
	// effect with the next run's snapshot.
	effectiveModel := snapshot.Model
	if found && (!newRun || effectiveModel == "") {
		effectiveModel = oldModel
	}
	var cost sql.NullFloat64
	source := oldSource
	// OpenClaw reports $0 for models missing from its own price catalog, so a
	// zero cost on a run that used tokens means "unknown", not "free".
	gatewayCostUnknown := snapshot.EstimatedCostUSD != nil && *snapshot.EstimatedCostUSD == 0 && in+out > 0
	if snapshot.EstimatedCostUSD != nil && !gatewayCostUnknown {
		cost = sql.NullFloat64{Float64: *snapshot.EstimatedCostUSD, Valid: true}
		source = "gateway"
	} else if estimated, ok := taskRunPrice(tx, effectiveModel, in, out); ok {
		// The gateway did not report a cost; estimate this run from its tokens.
		cost = sql.NullFloat64{Float64: estimated, Valid: true}
		source = "hub_pricing"
	} else if effectiveModel != "" {
		if _, loaded := unmatchedUsagePriceModels.LoadOrStore(effectiveModel, struct{}{}); !loaded {
			log.Printf("[usage] no static price for model %s", effectiveModel)
		}
	}
	din, dout, dtotal, dcost := 0, 0, 0, 0.0
	if newRun {
		din, dout, dtotal = in, out, in+out
		comIn += in
		comOut += out
		comTotal += in + out
		if cost.Valid {
			dcost = cost.Float64
			comCost += dcost
		}
	} else if cost.Valid && (!oldCost.Valid || cost.Float64 != oldCost.Float64) && !(oldSource == "gateway" && source == "hub_pricing") {
		// A real gateway cost can replace a hub estimate for the same run.
		dcost = cost.Float64
		if oldCost.Valid {
			dcost -= oldCost.Float64
		}
		comCost += dcost
	} else if !cost.Valid || (oldSource == "gateway" && source == "hub_pricing") {
		cost = oldCost
		source = oldSource
	}
	cacheRead, cacheWrite := oldCacheRead, oldCacheWrite
	if snapshot.CacheReadTokens != nil {
		cacheRead = *snapshot.CacheReadTokens
	}
	if snapshot.CacheWriteTokens != nil {
		cacheWrite = *snapshot.CacheWriteTokens
	}
	// A shrinking counter means the gateway restarted its tally, so the whole
	// value is new.
	dRead, dWrite := cacheRead-oldCacheRead, cacheWrite-oldCacheWrite
	if dRead < 0 {
		dRead = cacheRead
	}
	if dWrite < 0 {
		dWrite = cacheWrite
	}
	if source == "hub_pricing" {
		// Per-run token snapshots exclude cache, so hub-priced sessions add the
		// cost of the cache used since the last snapshot.
		if p, ok := modelPrice(tx, effectiveModel); ok {
			cacheCost := float64(dRead)*p.cacheRead + float64(dWrite)*p.cacheWrite
			dcost += cacheCost
			comCost += cacheCost
		}
	}
	usageNow := s.reaperNow()
	ts := usageNow.UnixMilli()
	day := usageNow.UTC().Format("2006-01-02")
	usageDay := oldUsageDay
	if newRun || usageDay == "" {
		usageDay = day
	}
	_, err = tx.Exec(`INSERT INTO task_run_usage(id,tenant_id,run_id,session_key,model,model_provider,input_tokens,output_tokens,total_tokens,committed_input_tokens,committed_output_tokens,committed_total_tokens,committed_cost_usd,estimated_cost_usd,cost_source,usage_day,cache_read_tokens,cache_write_tokens,first_seen_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,run_id,session_key) DO UPDATE SET model=excluded.model,model_provider=excluded.model_provider,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,total_tokens=excluded.total_tokens,committed_input_tokens=excluded.committed_input_tokens,committed_output_tokens=excluded.committed_output_tokens,committed_total_tokens=excluded.committed_total_tokens,committed_cost_usd=excluded.committed_cost_usd,estimated_cost_usd=excluded.estimated_cost_usd,cost_source=excluded.cost_source,usage_day=excluded.usage_day,cache_read_tokens=excluded.cache_read_tokens,cache_write_tokens=excluded.cache_write_tokens,updated_at=excluded.updated_at`, uuid.NewString(), tenant, runID, snapshot.SessionKey, effectiveModel, snapshot.ModelProvider, in, out, total, comIn, comOut, comTotal, comCost, nullFloat(cost), source, usageDay, cacheRead, cacheWrite, ts, ts)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO usage_daily(tenant_id,day,workspace_name,factory_name,workflow_name,model,input_tokens,output_tokens,total_tokens,cache_read_tokens,cache_write_tokens,cost_usd,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,day,workspace_name,factory_name,workflow_name,model) DO UPDATE SET input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,total_tokens=total_tokens+excluded.total_tokens,cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,cost_usd=cost_usd+excluded.cost_usd,updated_at=excluded.updated_at`, tenant, usageDay, workspace, factory, workflow, effectiveModel, din, dout, dtotal, dRead, dWrite, dcost, ts)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE task_run_summaries SET input_tokens=(SELECT COALESCE(SUM(committed_input_tokens),0) FROM task_run_usage WHERE tenant_id=? AND run_id=?),output_tokens=(SELECT COALESCE(SUM(committed_output_tokens),0) FROM task_run_usage WHERE tenant_id=? AND run_id=?),total_tokens=(SELECT COALESCE(SUM(committed_total_tokens),0) FROM task_run_usage WHERE tenant_id=? AND run_id=?),estimated_cost_usd=(SELECT COALESCE(SUM(committed_cost_usd),0) FROM task_run_usage WHERE tenant_id=? AND run_id=?),usage_updated_at=? WHERE tenant_id=? AND run_id=?`, tenant, runID, tenant, runID, tenant, runID, tenant, runID, ts, tenant, runID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func nullFloat(v sql.NullFloat64) interface{} {
	if v.Valid {
		return v.Float64
	}
	return nil
}

type tokenPrices struct{ input, output, cacheRead, cacheWrite float64 }

// modelPrice looks up per-token prices in the seeded model_prices table.
// Gateway model ids are matched loosely: lowercased, provider prefixes
// stripped, and the longest seeded model name that prefixes the id wins.
func modelPrice(tx *sql.Tx, model string) (tokenPrices, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	var p tokenPrices
	if model == "" {
		return p, false
	}
	err := tx.QueryRow(`SELECT input_cost_per_token,output_cost_per_token,cache_read_cost_per_token,cache_write_cost_per_token FROM model_prices WHERE ? LIKE model || '%' ORDER BY length(model) DESC LIMIT 1`, model).Scan(&p.input, &p.output, &p.cacheRead, &p.cacheWrite)
	return p, err == nil
}

// taskRunPrice estimates a run's cost from its input and output tokens.
func taskRunPrice(tx *sql.Tx, model string, in, out int) (float64, bool) {
	p, ok := modelPrice(tx, model)
	if !ok {
		return 0, false
	}
	return float64(in)*p.input + float64(out)*p.output, true
}

// backfillZeroGatewayUsageCostV1 prices usage rows that stored the gateway's
// $0 for a model OpenClaw could not price, before recordTaskRunUsage learned to
// fall back to model_prices. Every run in such a session reported $0, so the
// committed tokens are priced as a whole; the delta lands in the session's
// last usage_day bucket. Rows whose model has no hub price stay untouched.
func backfillZeroGatewayUsageCostV1(db *sql.DB) error {
	const migration = "zero_gateway_usage_cost_v1"
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS hub_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create hub migrations: %w", err)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_migrations WHERE name=?`, migration).Scan(&applied); err != nil {
		return fmt.Errorf("check zero gateway cost backfill: %w", err)
	}
	if applied > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type zeroCostRow struct {
		id, tenant, runID, model, usageDay string
		workspace, factory, workflow       string
		in, out, comIn, comOut             int
		updatedAt                          int64
	}
	rows, err := tx.Query(`SELECT u.id,u.tenant_id,u.run_id,u.model,u.usage_day,tr.workspace_name,tr.factory_name,tr.workflow_name,u.input_tokens,u.output_tokens,u.committed_input_tokens,u.committed_output_tokens,u.updated_at
		FROM task_run_usage u JOIN task_runs tr ON tr.id=u.run_id AND tr.tenant_id=u.tenant_id
		WHERE u.cost_source='gateway' AND COALESCE(u.estimated_cost_usd,0)=0 AND u.committed_cost_usd=0 AND u.committed_input_tokens+u.committed_output_tokens>0`)
	if err != nil {
		return fmt.Errorf("select zero gateway cost usage: %w", err)
	}
	var pending []zeroCostRow
	for rows.Next() {
		var r zeroCostRow
		if err := rows.Scan(&r.id, &r.tenant, &r.runID, &r.model, &r.usageDay, &r.workspace, &r.factory, &r.workflow, &r.in, &r.out, &r.comIn, &r.comOut, &r.updatedAt); err != nil {
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
		committed, ok := taskRunPrice(tx, r.model, r.comIn, r.comOut)
		if !ok {
			continue
		}
		last, _ := taskRunPrice(tx, r.model, r.in, r.out)
		if _, err := tx.Exec(`UPDATE task_run_usage SET estimated_cost_usd=?,committed_cost_usd=?,cost_source='hub_pricing' WHERE id=?`, last, committed, r.id); err != nil {
			return fmt.Errorf("backfill usage cost: %w", err)
		}
		day := r.usageDay
		if day == "" {
			day = time.UnixMilli(r.updatedAt).UTC().Format("2006-01-02")
		}
		if _, err := tx.Exec(`INSERT INTO usage_daily(tenant_id,day,workspace_name,factory_name,workflow_name,model,input_tokens,output_tokens,total_tokens,cost_usd,updated_at) VALUES(?,?,?,?,?,?,0,0,0,?,?) ON CONFLICT(tenant_id,day,workspace_name,factory_name,workflow_name,model) DO UPDATE SET cost_usd=cost_usd+excluded.cost_usd,updated_at=excluded.updated_at`, r.tenant, day, r.workspace, r.factory, r.workflow, r.model, committed, now().UnixMilli()); err != nil {
			return fmt.Errorf("backfill usage_daily cost: %w", err)
		}
		runs[[2]string{r.tenant, r.runID}] = true
	}
	for k := range runs {
		if _, err := tx.Exec(`UPDATE task_run_summaries SET estimated_cost_usd=(SELECT COALESCE(SUM(committed_cost_usd),0) FROM task_run_usage WHERE tenant_id=? AND run_id=?) WHERE tenant_id=? AND run_id=?`, k[0], k[1], k[0], k[1]); err != nil {
			return fmt.Errorf("backfill run summary cost: %w", err)
		}
	}
	if len(runs) > 0 {
		log.Printf("[usage] zero gateway cost backfill: priced %d run(s)", len(runs))
	}
	if _, err := tx.Exec(`INSERT INTO hub_migrations(name, applied_at) VALUES(?, ?)`, migration, now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
