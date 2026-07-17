package hub

import (
	"database/sql"
	"log"
	"strings"
	"sync"

	"github.com/google/uuid"
)

var unmatchedUsagePriceModels sync.Map

type taskRunUsageSnapshot struct {
	SessionKey                             string
	InputTokens, OutputTokens, TotalTokens *int
	EstimatedCostUSD                       *float64
	Model                                  string
	ModelProvider                          string
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
	var oldIn, oldOut, oldTotal, comIn, comOut, comTotal int
	var oldModel, oldUsageDay string
	var oldCost sql.NullFloat64
	var comCost float64
	oldSource := "gateway"
	err = tx.QueryRow(`SELECT model,input_tokens,output_tokens,total_tokens,committed_input_tokens,committed_output_tokens,committed_total_tokens,committed_cost_usd,estimated_cost_usd,cost_source,usage_day FROM task_run_usage WHERE tenant_id=? AND run_id=? AND session_key=?`, tenant, runID, snapshot.SessionKey).Scan(&oldModel, &oldIn, &oldOut, &oldTotal, &comIn, &comOut, &comTotal, &comCost, &oldCost, &oldSource, &oldUsageDay)
	found := err == nil
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	effectiveModel := snapshot.Model
	if effectiveModel == "" && found {
		effectiveModel = oldModel
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
	var cost sql.NullFloat64
	source := oldSource
	if snapshot.EstimatedCostUSD != nil {
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
	// OpenClaw overwrites a session entry after each reply with that run's
	// totals. total_tokens is context occupancy, not billed token usage.
	newRun := !found || in != oldIn || out != oldOut
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
	usageNow := now()
	if s.nowFunc != nil {
		usageNow = s.nowFunc()
	}
	ts := usageNow.UnixMilli()
	day := usageNow.UTC().Format("2006-01-02")
	usageDay := oldUsageDay
	if newRun || usageDay == "" {
		usageDay = day
	}
	_, err = tx.Exec(`INSERT INTO task_run_usage(id,tenant_id,run_id,session_key,model,model_provider,input_tokens,output_tokens,total_tokens,committed_input_tokens,committed_output_tokens,committed_total_tokens,committed_cost_usd,estimated_cost_usd,cost_source,usage_day,first_seen_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,run_id,session_key) DO UPDATE SET model=excluded.model,model_provider=excluded.model_provider,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,total_tokens=excluded.total_tokens,committed_input_tokens=excluded.committed_input_tokens,committed_output_tokens=excluded.committed_output_tokens,committed_total_tokens=excluded.committed_total_tokens,committed_cost_usd=excluded.committed_cost_usd,estimated_cost_usd=excluded.estimated_cost_usd,cost_source=excluded.cost_source,usage_day=excluded.usage_day,updated_at=excluded.updated_at`, uuid.NewString(), tenant, runID, snapshot.SessionKey, effectiveModel, snapshot.ModelProvider, in, out, total, comIn, comOut, comTotal, comCost, nullFloat(cost), source, usageDay, ts, ts)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO usage_daily(tenant_id,day,workspace_name,factory_name,workflow_name,model,input_tokens,output_tokens,total_tokens,cost_usd,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,day,workspace_name,factory_name,workflow_name,model) DO UPDATE SET input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,total_tokens=total_tokens+excluded.total_tokens,cost_usd=cost_usd+excluded.cost_usd,updated_at=excluded.updated_at`, tenant, usageDay, workspace, factory, workflow, effectiveModel, din, dout, dtotal, dcost, ts)
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

// taskRunPrice estimates a run's cost from its token counts using the seeded
// model_prices table. Gateway model ids are
// matched loosely: lowercased, provider prefixes stripped, and the longest
// seeded model name that prefixes the id wins.
func taskRunPrice(tx *sql.Tx, model string, in, out int) (float64, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		return 0, false
	}
	var ip, op float64
	err := tx.QueryRow(`SELECT input_cost_per_token,output_cost_per_token FROM model_prices WHERE ? LIKE model || '%' ORDER BY length(model) DESC LIMIT 1`, model).Scan(&ip, &op)
	if err != nil {
		return 0, false
	}
	return float64(in)*ip + float64(out)*op, true
}
