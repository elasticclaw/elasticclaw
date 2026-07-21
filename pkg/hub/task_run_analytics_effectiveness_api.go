package hub

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type taskRunAnalyticsEffectivenessResponse struct {
	OutcomesByDay   []taskRunAnalyticsOutcomesDay `json:"outcomesByDay"`
	Funnel          taskRunAnalyticsFunnel        `json:"funnel"`
	CostPerMergedPr taskRunAnalyticsCostPerMerged `json:"costPerMergedPr"`
	MergeRate       float64                       `json:"mergeRate"`
	SuccessRate     float64                       `json:"successRate"`
}
type taskRunAnalyticsOutcomesDay struct {
	Date           string `json:"date"`
	Clean          int    `json:"clean"`
	HumanInTheLoop int    `json:"humanInTheLoop"`
	Warning        int    `json:"warning"`
	Failed         int    `json:"failed"`
}
type taskRunAnalyticsFunnel struct {
	Started       int `json:"started"`
	AgentFinished int `json:"agentFinished"`
	PROpened      int `json:"prOpened"`
	PRMerged      int `json:"prMerged"`
}
type taskRunAnalyticsCostPerMerged struct {
	Weekly  []taskRunAnalyticsWeeklyCost `json:"weekly"`
	Average float64                      `json:"average"`
}
type taskRunAnalyticsWeeklyCost struct {
	WeekStart       string  `json:"weekStart"`
	CostUsd         float64 `json:"costUsd"`
	MergedPrs       int     `json:"mergedPrs"`
	CostPerMergedPr float64 `json:"costPerMergedPr"`
}

func (s *Server) handleTaskRunAnalyticsEffectiveness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	f, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.readTaskRunAnalyticsEffectiveness(f)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	jsonOK(w, out)
}
func (s *Server) readTaskRunAnalyticsEffectiveness(f taskRunAnalyticsFilters) (taskRunAnalyticsEffectivenessResponse, error) {
	w, a := taskRunAnalyticsSummaryWhere(f)
	rows, err := s.db.Query(`SELECT DATE(started_at/1000,'unixepoch'), status, COUNT(*), COALESCE(SUM(estimated_cost_usd),0), COALESCE(SUM(merged_pr_count),0), COALESCE(SUM(CASE WHEN merged_pr_count>0 THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN started_at>0 THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN agent_started_at>0 THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN pr_opened_at>0 THEN 1 ELSE 0 END),0) FROM task_run_summaries `+w+` GROUP BY 1,status`, a...)
	if err != nil {
		return taskRunAnalyticsEffectivenessResponse{}, err
	}
	defer rows.Close()
	out := taskRunAnalyticsEffectivenessResponse{}
	byDay := map[string]*taskRunAnalyticsOutcomesDay{}
	weekly := map[string]*taskRunAnalyticsWeeklyCost{}
	var finished, success, opened, merged, mergedRuns int
	for rows.Next() {
		var d, status string
		var n, mp, mpr, st, af, po int
		var cost float64
		if err = rows.Scan(&d, &status, &n, &cost, &mp, &mpr, &st, &af, &po); err != nil {
			return out, err
		}
		x := byDay[d]
		if x == nil {
			x = &taskRunAnalyticsOutcomesDay{Date: d}
			byDay[d] = x
		}
		switch status {
		case taskRunStatusClean:
			x.Clean = n
		case taskRunStatusHumanInTheLoop:
			x.HumanInTheLoop = n
		case taskRunStatusWarning:
			x.Warning = n
		case taskRunStatusFailed:
			x.Failed = n
		}
		if status != taskRunStatusRunning {
			finished += n
			if status == taskRunStatusClean || status == taskRunStatusHumanInTheLoop || status == taskRunStatusWarning {
				success += n
			}
		}
		out.Funnel.Started += st
		out.Funnel.AgentFinished += af
		out.Funnel.PROpened += po
		out.Funnel.PRMerged += mpr
		opened += po
		merged += mp
		mergedRuns += mpr
		t, _ := time.Parse("2006-01-02", d)
		ws := t.AddDate(0, 0, -(int(t.Weekday())+6)%7).Format("2006-01-02")
		q := weekly[ws]
		if q == nil {
			q = &taskRunAnalyticsWeeklyCost{WeekStart: ws}
			weekly[ws] = q
		}
		q.CostUsd += cost
		q.MergedPrs += mp
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	for _, x := range byDay {
		out.OutcomesByDay = append(out.OutcomesByDay, *x)
	}
	sort.Slice(out.OutcomesByDay, func(i, j int) bool { return out.OutcomesByDay[i].Date < out.OutcomesByDay[j].Date })
	for _, x := range weekly {
		if x.MergedPrs > 0 {
			x.CostPerMergedPr = x.CostUsd / float64(x.MergedPrs)
		}
		out.CostPerMergedPr.Weekly = append(out.CostPerMergedPr.Weekly, *x)
	}
	sort.Slice(out.CostPerMergedPr.Weekly, func(i, j int) bool {
		return out.CostPerMergedPr.Weekly[i].WeekStart < out.CostPerMergedPr.Weekly[j].WeekStart
	})
	if merged > 0 {
		var cost float64
		for _, x := range out.CostPerMergedPr.Weekly {
			cost += x.CostUsd
		}
		out.CostPerMergedPr.Average = cost / float64(merged)
	}
	if opened > 0 {
		out.MergeRate = float64(mergedRuns) / float64(opened)
	}
	if finished > 0 {
		out.SuccessRate = float64(success) / float64(finished)
	}
	return out, nil
}

type taskRunAnalyticsCostDriver struct {
	Name            string                      `json:"name"`
	Runs            int                         `json:"runs"`
	SuccessRate     float64                     `json:"successRate"`
	CostUsd         float64                     `json:"costUsd"`
	MergedPrs       int                         `json:"mergedPrs"`
	CostPerMergedPr float64                     `json:"costPerMergedPr"`
	DailyCost       []taskRunAnalyticsDailyCost `json:"dailyCost"`
}

func (s *Server) handleTaskRunAnalyticsCostDrivers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, 405, "method not allowed")
		return
	}
	f, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, 400, err.Error())
		return
	}
	group := r.URL.Query().Get("groupBy")
	if group == "" {
		group = "factory"
	}
	if group != "factory" && group != "workflow" {
		jsonError(w, 400, "groupBy must be factory or workflow")
		return
	}
	out, err := s.readTaskRunAnalyticsCostDrivers(f, group)
	if err != nil {
		if err == errTaskRunAnalyticsInvalidCostRange {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonError(w, 500, "db error")
		return
	}
	jsonOK(w, out)
}
func (s *Server) readTaskRunAnalyticsCostDrivers(f taskRunAnalyticsFilters, group string) ([]taskRunAnalyticsCostDriver, error) {
	if f.FromStartedAt == 0 && f.ToStartedAt == 0 {
		end := time.Now().UTC().Truncate(24 * time.Hour)
		f.FromStartedAt = end.AddDate(0, 0, -30).UnixMilli()
		f.ToStartedAt = end.Add(24*time.Hour - time.Millisecond).UnixMilli()
	} else if f.FromStartedAt == 0 && f.ToStartedAt > 0 {
		// Only `to` was supplied. Anchor the missing lower bound on it (mirroring
		// the costs endpoint's to-only anchoring) so totals and the sparkline use
		// the same window.
		toDay := time.UnixMilli(f.ToStartedAt).UTC().Truncate(24 * time.Hour)
		f.FromStartedAt = toDay.AddDate(0, 0, -30).UnixMilli()
	}
	if f.FromStartedAt > 0 && f.ToStartedAt > 0 && f.ToStartedAt < f.FromStartedAt {
		return nil, errTaskRunAnalyticsInvalidCostRange
	}
	col := "factory_name"
	if group == "workflow" {
		col = "workflow_name"
	}
	w, a := taskRunAnalyticsSummaryWhere(f)
	q := fmt.Sprintf(`SELECT %s, COUNT(*), COALESCE(SUM(estimated_cost_usd),0), COALESCE(SUM(merged_pr_count),0), COALESCE(SUM(CASE WHEN status IN ('clean','human_in_the_loop','warning') THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN status!='running' THEN 1 ELSE 0 END),0) FROM task_run_summaries %s GROUP BY %s ORDER BY 3 DESC`, col, w, col)
	rows, err := s.db.Query(q, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []taskRunAnalyticsCostDriver{}
	for rows.Next() {
		var x taskRunAnalyticsCostDriver
		var suc, fin int
		if err = rows.Scan(&x.Name, &x.Runs, &x.CostUsd, &x.MergedPrs, &suc, &fin); err != nil {
			return nil, err
		}
		if fin > 0 {
			x.SuccessRate = float64(suc) / float64(fin)
		}
		if x.MergedPrs > 0 {
			x.CostPerMergedPr = x.CostUsd / float64(x.MergedPrs)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	start := time.Now().UTC().AddDate(0, 0, -30).Truncate(24 * time.Hour)
	end := time.Now().UTC().Truncate(24 * time.Hour)
	if f.FromStartedAt > 0 {
		start = time.UnixMilli(f.FromStartedAt).UTC().Truncate(24 * time.Hour)
	}
	if f.ToStartedAt > 0 {
		end = time.UnixMilli(f.ToStartedAt).UTC().Truncate(24 * time.Hour)
	}
	for i := range out {
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			out[i].DailyCost = append(out[i].DailyCost, taskRunAnalyticsDailyCost{Date: d.Format("2006-01-02")})
		}
	}
	// Cost drivers has no ledger mode: the sparkline always uses per-run
	// attribution against the same eligibility-filtered run set as the row
	// totals above, so totals and sparkline never disagree.
	usageWhere, usageArgs := taskRunAnalyticsUsageModelWhere(f)
	q = `SELECT s.` + col + `, u.usage_day, COALESCE(SUM(u.committed_cost_usd),0) FROM task_run_usage u JOIN (SELECT run_id, ` + col + ` FROM task_run_summaries ` + w + `) s ON s.run_id=u.run_id WHERE u.tenant_id=?`
	if len(usageWhere) > 0 {
		q += ` AND ` + strings.Join(usageWhere, " AND ")
	}
	q += ` GROUP BY s.` + col + `, u.usage_day`
	rs, e := s.db.Query(q, append(append(a, f.TenantID), usageArgs...)...)
	if e != nil {
		return nil, e
	}
	values := map[string]map[string]float64{}
	for rs.Next() {
		var name, day string
		var cost float64
		if e = rs.Scan(&name, &day, &cost); e != nil {
			rs.Close()
			return nil, e
		}
		if values[name] == nil {
			values[name] = map[string]float64{}
		}
		day = clampTaskRunAnalyticsUsageDay(day, start, end)
		values[name][day] += cost
	}
	if e = rs.Err(); e != nil {
		rs.Close()
		return nil, e
	}
	rs.Close()
	for i := range out {
		for j := range out[i].DailyCost {
			out[i].DailyCost[j].CostUsd = values[out[i].Name][out[i].DailyCost[j].Date]
		}
	}
	return out, nil
}
