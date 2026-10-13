package hub

import (
	"database/sql"
	"math"
	"strings"
	"time"
)

type usageCostComponents struct {
	input, output           float64
	readTokens, writeTokens int64
	readCost, writeCost     float64
}

func (c usageCostComponents) cacheCost() float64 { return c.readCost + c.writeCost }
func (c usageCostComponents) totalCost() float64 { return c.input + c.output + c.cacheCost() }
func (c *usageCostComponents) add(v usageCostComponents) {
	c.input += v.input
	c.output += v.output
	c.readTokens += v.readTokens
	c.writeTokens += v.writeTokens
	c.readCost += v.readCost
	c.writeCost += v.writeCost
}
func (c *usageCostComponents) subtract(v usageCostComponents) {
	c.input -= v.input
	c.output -= v.output
	c.readTokens -= v.readTokens
	c.writeTokens -= v.writeTokens
	c.readCost -= v.readCost
	c.writeCost -= v.writeCost
}

func estimateUsageCache(cost, writeReadRatio float64, p tokenPrices) usageCostComponents {
	denominator := p.cacheRead + writeReadRatio*p.cacheWrite
	if cost <= 0 || denominator <= 0 {
		return usageCostComponents{}
	}
	read := cost / denominator
	r, w := int64(math.Round(read)), int64(math.Round(writeReadRatio*read))
	return usageCostComponents{readTokens: r, writeTokens: w, readCost: float64(r) * p.cacheRead, writeCost: float64(w) * p.cacheWrite}
}

func normalizedUsageModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	return model
}

type usageCostRatioEntry struct {
	ratio, writeReadRatio float64
	at                    time.Time
}

func (s *Server) fallbackCacheRatios(tx *sql.Tx, model string, at time.Time) (float64, float64, error) {
	key := normalizedUsageModel(model)
	if cached, ok := s.usageCostRatios.Load(key); ok {
		entry := cached.(usageCostRatioEntry)
		if !at.Before(entry.at) && at.Sub(entry.at) < 10*time.Minute {
			return entry.ratio, entry.writeReadRatio, nil
		}
	}
	ratio, w, err := measureFallbackCacheRatios(tx, model, at)
	if err == nil {
		s.usageCostRatios.Store(key, usageCostRatioEntry{ratio, w, at})
	}
	return ratio, w, err
}

// measureFallbackCacheRatios bypasses the per-server cache for deterministic tests.
// Gateway-priced runs and hub-priced runs with real OpenClaw cache counters
// both measure the true cost/io ratio; approximate runs would feed back.
func measureFallbackCacheRatios(tx *sql.Tx, model string, at time.Time) (float64, float64, error) {
	type price struct {
		model         string
		input, output float64
	}
	rows, err := tx.Query(`SELECT model,input_cost_per_token,output_cost_per_token FROM model_prices ORDER BY length(model) DESC`)
	if err != nil {
		return 0, 0, err
	}
	var prices []price
	for rows.Next() {
		var p price
		if err := rows.Scan(&p.model, &p.input, &p.output); err != nil {
			rows.Close()
			return 0, 0, err
		}
		prices = append(prices, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	match := func(model string) (price, bool) {
		model = normalizedUsageModel(model)
		for _, p := range prices {
			if strings.HasPrefix(model, strings.ToLower(p.model)) {
				return p, true
			}
		}
		return price{}, false
	}
	target, _ := match(model)
	cutoff := at.AddDate(0, 0, -30).UnixMilli()
	rows, err = tx.Query(`SELECT model,committed_input_tokens,committed_output_tokens,committed_cost_usd FROM task_run_usage WHERE (cost_source='gateway' OR (cost_source='hub_pricing' AND cost_approximate=0 AND cache_read_tokens IS NOT NULL)) AND committed_cost_usd>0 AND updated_at>=? AND updated_at<=?`, cutoff, at.UnixMilli())
	if err != nil {
		return 0, 0, err
	}
	var sameCount, allCount int
	var sameCost, sameIO, allCost, allIO float64
	for rows.Next() {
		var m string
		var in, out int64
		var cost float64
		if err := rows.Scan(&m, &in, &out, &cost); err != nil {
			rows.Close()
			return 0, 0, err
		}
		p, ok := match(m)
		if !ok {
			continue
		}
		io := float64(in)*p.input + float64(out)*p.output
		allCount++
		allCost += cost
		allIO += io
		if p.model == target.model {
			sameCount++
			sameCost += cost
			sameIO += io
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if sameCount < 3 {
		sameCount, sameCost, sameIO = allCount, allCost, allIO
	}
	ratio := 6.5
	if sameCount >= 3 && sameIO > 0 {
		ratio = math.Max(1, sameCost/sameIO)
	}
	var reads, writes float64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cache_write_tokens),0) FROM task_run_usage WHERE cache_read_tokens>0 AND updated_at>=? AND updated_at<=?`, cutoff, at.UnixMilli()).Scan(&reads, &writes); err != nil {
		return 0, 0, err
	}
	w := 0.12
	if reads > 0 {
		w = writes / reads
	}
	return ratio, w, nil
}

type usageDailyBucket struct{ tenant, workspace, factory, workflow string }

func (b usageDailyBucket) add(tx *sql.Tx, day, model string, in, out, total int, read, write int64, cost, approximate float64, ts int64) error {
	_, err := tx.Exec(`INSERT INTO usage_daily(tenant_id,day,workspace_name,factory_name,workflow_name,model,input_tokens,output_tokens,total_tokens,cache_read_tokens,cache_write_tokens,cost_usd,approx_cost_usd,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,day,workspace_name,factory_name,workflow_name,model) DO UPDATE SET input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,total_tokens=total_tokens+excluded.total_tokens,cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,cost_usd=cost_usd+excluded.cost_usd,approx_cost_usd=approx_cost_usd+excluded.approx_cost_usd,updated_at=excluded.updated_at`, b.tenant, day, b.workspace, b.factory, b.workflow, model, in, out, total, read, write, cost, approximate, ts)
	return err
}

// Estimate provenance is kept only until real counters replace it. Aggregating
// by day/model bounds its size while preserving the ledger's original buckets.
func adjustUsageCacheEstimate(tx *sql.Tx, tenant, runID, session, day, model string, c usageCostComponents, sign int64) error {
	_, err := tx.Exec(`INSERT INTO task_run_usage_estimates(tenant_id,run_id,session_key,day,model,input_cost_usd,output_cost_usd,cache_read_tokens,cache_write_tokens,cache_read_cost_usd,cache_write_cost_usd) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,run_id,session_key,day,model) DO UPDATE SET input_cost_usd=input_cost_usd+excluded.input_cost_usd,output_cost_usd=output_cost_usd+excluded.output_cost_usd,cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,cache_write_tokens=cache_write_tokens+excluded.cache_write_tokens,cache_read_cost_usd=cache_read_cost_usd+excluded.cache_read_cost_usd,cache_write_cost_usd=cache_write_cost_usd+excluded.cache_write_cost_usd`, tenant, runID, session, day, model, c.input*float64(sign), c.output*float64(sign), c.readTokens*sign, c.writeTokens*sign, c.readCost*float64(sign), c.writeCost*float64(sign))
	return err
}

func reverseUsageCacheEstimates(tx *sql.Tx, ledger usageDailyBucket, runID, session string, ts int64) error {
	rows, err := tx.Query(`SELECT day,model,input_cost_usd,output_cost_usd,cache_read_tokens,cache_write_tokens,cache_read_cost_usd,cache_write_cost_usd FROM task_run_usage_estimates WHERE tenant_id=? AND run_id=? AND session_key=?`, ledger.tenant, runID, session)
	if err != nil {
		return err
	}
	type estimate struct {
		day, model string
		c          usageCostComponents
	}
	var estimates []estimate
	for rows.Next() {
		var e estimate
		if err := rows.Scan(&e.day, &e.model, &e.c.input, &e.c.output, &e.c.readTokens, &e.c.writeTokens, &e.c.readCost, &e.c.writeCost); err != nil {
			rows.Close()
			return err
		}
		estimates = append(estimates, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range estimates {
		if err := ledger.add(tx, e.day, e.model, 0, 0, 0, -e.c.readTokens, -e.c.writeTokens, -e.c.cacheCost(), -e.c.totalCost(), ts); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`DELETE FROM task_run_usage_estimates WHERE tenant_id=? AND run_id=? AND session_key=?`, ledger.tenant, runID, session)
	return err
}
