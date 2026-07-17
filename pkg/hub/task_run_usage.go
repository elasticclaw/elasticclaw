package hub

import (
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"
)

type taskRunUsageSnapshot struct {
	SessionKey                             string
	InputTokens, OutputTokens, TotalTokens *int
	EstimatedCostUSD                       *float64
	Model                                  string
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
	if err = tx.QueryRow(`SELECT tr.tenant_id,tr.id,tr.workspace_name,tr.factory_name,tr.workflow_name FROM claws c JOIN task_runs tr ON tr.id=c.task_run_id WHERE c.id=?`, clawID).Scan(&tenant, &runID, &workspace, &factory, &workflow); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	var oldIn, oldOut, oldTotal int
	var oldCost sql.NullFloat64
	err = tx.QueryRow(`SELECT input_tokens,output_tokens,total_tokens,estimated_cost_usd FROM task_run_usage WHERE tenant_id=? AND run_id=? AND session_key=?`, tenant, runID, snapshot.SessionKey).Scan(&oldIn, &oldOut, &oldTotal, &oldCost)
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
	cost := oldCost
	if snapshot.EstimatedCostUSD != nil {
		cost = sql.NullFloat64{Float64: *snapshot.EstimatedCostUSD, Valid: true}
	}
	delta := func(n, old int) int {
		if n < old {
			return n
		}
		return n - old
	}
	din, dout, dtotal := delta(in, oldIn), delta(out, oldOut), delta(total, oldTotal)
	dcost := 0.0
	if cost.Valid {
		if !oldCost.Valid || cost.Float64 < oldCost.Float64 {
			dcost = cost.Float64
		} else {
			dcost = cost.Float64 - oldCost.Float64
		}
	}
	source := "gateway"
	if !cost.Valid {
		if estimated, ok := taskRunPrice(tx, snapshot.Model, din, dout); ok {
			dcost = estimated
			cost = sql.NullFloat64{Float64: estimated + oldCost.Float64, Valid: true}
			source = "hub_pricing"
		}
	}
	ts := now().UnixMilli()
	_, err = tx.Exec(`INSERT INTO task_run_usage(id,tenant_id,run_id,session_key,model,input_tokens,output_tokens,total_tokens,estimated_cost_usd,cost_source,first_seen_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,run_id,session_key) DO UPDATE SET model=excluded.model,input_tokens=excluded.input_tokens,output_tokens=excluded.output_tokens,total_tokens=excluded.total_tokens,estimated_cost_usd=excluded.estimated_cost_usd,cost_source=excluded.cost_source,updated_at=excluded.updated_at`, uuid.NewString(), tenant, runID, snapshot.SessionKey, snapshot.Model, in, out, total, nullFloat(cost), source, ts, ts)
	if err != nil {
		return err
	}
	day := time.Now().UTC().Format("2006-01-02")
	_, err = tx.Exec(`INSERT INTO usage_daily(tenant_id,day,workspace_name,factory_name,workflow_name,model,input_tokens,output_tokens,total_tokens,cost_usd,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,day,workspace_name,factory_name,workflow_name,model) DO UPDATE SET input_tokens=input_tokens+excluded.input_tokens,output_tokens=output_tokens+excluded.output_tokens,total_tokens=total_tokens+excluded.total_tokens,cost_usd=cost_usd+excluded.cost_usd,updated_at=excluded.updated_at`, tenant, day, workspace, factory, workflow, snapshot.Model, din, dout, dtotal, dcost, ts)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE task_run_summaries SET input_tokens=(SELECT COALESCE(SUM(input_tokens),0) FROM task_run_usage WHERE run_id=?),output_tokens=(SELECT COALESCE(SUM(output_tokens),0) FROM task_run_usage WHERE run_id=?),total_tokens=(SELECT COALESCE(SUM(total_tokens),0) FROM task_run_usage WHERE run_id=?),estimated_cost_usd=(SELECT COALESCE(SUM(estimated_cost_usd),0) FROM task_run_usage WHERE run_id=?),usage_updated_at=? WHERE run_id=?`, runID, runID, runID, runID, ts, runID)
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
func taskRunPrice(tx *sql.Tx, model string, in, out int) (float64, bool) {
	var ip, op float64
	err := tx.QueryRow(`SELECT input_cost_per_token,output_cost_per_token FROM model_prices WHERE ? LIKE model || '%' OR model LIKE ? || '%' ORDER BY length(model) DESC LIMIT 1`, strings.ToLower(model), strings.ToLower(model)).Scan(&ip, &op)
	if err != nil {
		return 0, false
	}
	return float64(in)*ip + float64(out)*op, true
}
