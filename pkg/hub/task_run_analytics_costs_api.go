package hub

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type taskRunAnalyticsCostsResponse struct {
	Today              taskRunAnalyticsCostPeriod        `json:"today"`
	Week               taskRunAnalyticsCostAmount        `json:"week"`
	Month              taskRunAnalyticsCostAmount        `json:"month"`
	ProjectedMonth     *taskRunAnalyticsProjection       `json:"projectedMonth"`
	DailySeries        []taskRunAnalyticsDailyCost       `json:"dailySeries"`
	Prior              taskRunAnalyticsPriorCost         `json:"prior"`
	PriorPeriodCostUsd *float64                          `json:"priorPeriodCostUsd,omitempty"`
	SeriesByModel      []taskRunAnalyticsModelCostSeries `json:"seriesByModel,omitempty"`
}

type taskRunAnalyticsCostPeriod struct {
	CostUsd             float64  `json:"costUsd"`
	TotalTokens         int64    `json:"totalTokens"`
	DeltaPctVsYesterday *float64 `json:"deltaPctVsYesterday"`
}

type taskRunAnalyticsCostAmount struct {
	CostUsd float64 `json:"costUsd"`
}
type taskRunAnalyticsPriorCost struct {
	PeriodCostUsd float64 `json:"periodCostUsd"`
}

type taskRunAnalyticsProjection struct {
	CostUsd    float64 `json:"costUsd"`
	Confidence string  `json:"confidence"`
	Basis      string  `json:"basis"`
}

type taskRunAnalyticsDailyCost struct {
	Date        string  `json:"date"`
	CostUsd     float64 `json:"costUsd"`
	TotalTokens int64   `json:"totalTokens"`
	RunCount    int     `json:"runCount"`
}

type taskRunAnalyticsModelCostSeries struct {
	Model       string                      `json:"model"`
	DailySeries []taskRunAnalyticsDailyCost `json:"dailySeries"`
}

type taskRunAnalyticsDailyCostTotals struct {
	costUsd     float64
	totalTokens int64
}

func (s *Server) handleTaskRunAnalyticsCosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	filters, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &days); err != nil || days < 1 || days > 366 {
			jsonError(w, http.StatusBadRequest, "days must be between 1 and 366")
			return
		}
	}
	response, err := s.readTaskRunAnalyticsCostsWithOptions(filters, time.Now().UTC(), days, r.URL.Query().Get("groupBy"))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	jsonOK(w, response)
}

func (s *Server) readTaskRunAnalyticsCosts(filters taskRunAnalyticsFilters, now time.Time) (taskRunAnalyticsCostsResponse, error) {
	return s.readTaskRunAnalyticsCostsWithOptions(filters, now, 30, "")
}

func (s *Server) readTaskRunAnalyticsCostsWithOptions(filters taskRunAnalyticsFilters, now time.Time, days int, groupBy string) (taskRunAnalyticsCostsResponse, error) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	seriesStart := today.AddDate(0, 0, -days)
	if filters.FromStartedAt > 0 {
		seriesStart = time.UnixMilli(filters.FromStartedAt).UTC().Truncate(24 * time.Hour)
	}
	seriesEnd := today
	if filters.ToStartedAt > 0 {
		seriesEnd = time.UnixMilli(filters.ToStartedAt).UTC().Truncate(24 * time.Hour)
	}
	if seriesEnd.Before(seriesStart) {
		return taskRunAnalyticsCostsResponse{}, fmt.Errorf("invalid time range")
	}
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	where := []string{"tenant_id=?", "day >= ?", "day <= ?"}
	args := []any{filters.TenantID, seriesStart.Format("2006-01-02"), seriesEnd.Format("2006-01-02")}
	addTaskRunAnalyticsInFilter(&where, &args, "workspace_name", filters.Workspace)
	addTaskRunAnalyticsInFilter(&where, &args, "factory_name", filters.Factory)
	addTaskRunAnalyticsInFilter(&where, &args, "workflow_name", filters.Workflow)
	addTaskRunAnalyticsInFilter(&where, &args, "model", filters.Model)

	rows, err := s.db.Query(`SELECT day, COALESCE(SUM(cost_usd), 0), COALESCE(SUM(total_tokens), 0) FROM usage_daily WHERE `+strings.Join(where, " AND ")+` GROUP BY day`, args...)
	if err != nil {
		return taskRunAnalyticsCostsResponse{}, err
	}
	defer rows.Close()
	byDay := map[string]taskRunAnalyticsDailyCostTotals{}
	for rows.Next() {
		var day string
		var totals taskRunAnalyticsDailyCostTotals
		if err := rows.Scan(&day, &totals.costUsd, &totals.totalTokens); err != nil {
			return taskRunAnalyticsCostsResponse{}, err
		}
		byDay[day] = totals
	}
	if err := rows.Err(); err != nil {
		return taskRunAnalyticsCostsResponse{}, err
	}

	response := taskRunAnalyticsCostsResponse{DailySeries: make([]taskRunAnalyticsDailyCost, 0, days+1)}
	for day := seriesStart; !day.After(seriesEnd); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		totals := byDay[key]
		response.DailySeries = append(response.DailySeries, taskRunAnalyticsDailyCost{Date: key, CostUsd: totals.costUsd, TotalTokens: totals.totalTokens})
	}
	if err := s.addTaskRunAnalyticsDailyRunCounts(filters, response.DailySeries); err != nil {
		return response, err
	}
	todayTotals := byDay[today.Format("2006-01-02")]
	yesterdayTotals := byDay[today.AddDate(0, 0, -1).Format("2006-01-02")]
	response.Today.CostUsd = todayTotals.costUsd
	response.Today.TotalTokens = todayTotals.totalTokens
	if yesterdayTotals.costUsd != 0 {
		delta := (todayTotals.costUsd - yesterdayTotals.costUsd) / yesterdayTotals.costUsd * 100
		response.Today.DeltaPctVsYesterday = &delta
	}

	last7Costs := make([]float64, 0, 7)
	hasProjectionData := false
	for day := monthStart; !day.After(today); day = day.AddDate(0, 0, 1) {
		totals, exists := byDay[day.Format("2006-01-02")]
		response.Month.CostUsd += totals.costUsd
		hasProjectionData = hasProjectionData || exists
	}
	for day := today.AddDate(0, 0, -6); !day.After(today); day = day.AddDate(0, 0, 1) {
		response.Week.CostUsd += byDay[day.Format("2006-01-02")].costUsd
	}
	for day := today.AddDate(0, 0, -7); day.Before(today); day = day.AddDate(0, 0, 1) {
		totals, exists := byDay[day.Format("2006-01-02")]
		last7Costs = append(last7Costs, totals.costUsd)
		hasProjectionData = hasProjectionData || exists
	}
	if hasProjectionData {
		mean := averageTaskRunAnalyticsCosts(last7Costs)
		daysInMonth := time.Date(today.Year(), today.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
		// Project today as a full mean day because its recorded spend is still partial.
		projectedCost := (response.Month.CostUsd - todayTotals.costUsd) + mean*float64(daysInMonth-today.Day()+1)
		response.ProjectedMonth = &taskRunAnalyticsProjection{
			CostUsd:    projectedCost,
			Confidence: taskRunAnalyticsProjectionConfidence(today.Day()-1, last7Costs, mean),
			Basis:      "7d-avg",
		}
	}
	// Prior period cost is meaningful for an explicit window; retain the
	// lightweight prior object for all callers.
	priorStart := seriesStart.AddDate(0, 0, -int(seriesEnd.Sub(seriesStart).Hours()/24)-1)
	priorEnd := seriesStart.AddDate(0, 0, -1)
	priorCost, err := s.readTaskRunAnalyticsUsageCost(filters, priorStart, priorEnd)
	if err != nil {
		return response, err
	}
	response.Prior.PeriodCostUsd = priorCost
	if filters.FromStartedAt > 0 || filters.ToStartedAt > 0 {
		response.PriorPeriodCostUsd = &priorCost
	}
	if groupBy == "model" {
		response.SeriesByModel, err = s.readTaskRunAnalyticsCostSeriesByModel(filters, seriesStart, seriesEnd)
		if err != nil {
			return response, err
		}
	}
	return response, nil
}

func (s *Server) readTaskRunAnalyticsUsageCost(f taskRunAnalyticsFilters, start, end time.Time) (float64, error) {
	w := []string{"tenant_id=?", "day>=?", "day<=?"}
	a := []any{f.TenantID, start.Format("2006-01-02"), end.Format("2006-01-02")}
	addTaskRunAnalyticsInFilter(&w, &a, "workspace_name", f.Workspace)
	addTaskRunAnalyticsInFilter(&w, &a, "factory_name", f.Factory)
	addTaskRunAnalyticsInFilter(&w, &a, "workflow_name", f.Workflow)
	addTaskRunAnalyticsInFilter(&w, &a, "model", f.Model)
	var cost float64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM usage_daily WHERE `+strings.Join(w, " AND "), a...).Scan(&cost)
	return cost, err
}

func (s *Server) addTaskRunAnalyticsDailyRunCounts(f taskRunAnalyticsFilters, series []taskRunAnalyticsDailyCost) error {
	w, a := taskRunAnalyticsSummaryWhere(f)
	rows, err := s.db.Query(`SELECT DATE(started_at/1000, 'unixepoch'), COUNT(*) FROM task_run_summaries `+w+` GROUP BY 1`, a...)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var d string
		var n int
		if err = rows.Scan(&d, &n); err != nil {
			return err
		}
		counts[d] = n
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range series {
		series[i].RunCount = counts[series[i].Date]
	}
	return nil
}

func (s *Server) readTaskRunAnalyticsCostSeriesByModel(f taskRunAnalyticsFilters, start, end time.Time) ([]taskRunAnalyticsModelCostSeries, error) {
	w := []string{"tenant_id=?", "day>=?", "day<=?"}
	a := []any{f.TenantID, start.Format("2006-01-02"), end.Format("2006-01-02")}
	addTaskRunAnalyticsInFilter(&w, &a, "workspace_name", f.Workspace)
	addTaskRunAnalyticsInFilter(&w, &a, "factory_name", f.Factory)
	addTaskRunAnalyticsInFilter(&w, &a, "workflow_name", f.Workflow)
	addTaskRunAnalyticsInFilter(&w, &a, "model", f.Model)
	rows, err := s.db.Query(`SELECT model, day, COALESCE(SUM(cost_usd),0), COALESCE(SUM(total_tokens),0) FROM usage_daily WHERE `+strings.Join(w, " AND ")+` GROUP BY model,day`, a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type v struct {
		c float64
		t int64
	}
	data := map[string]map[string]v{}
	totals := map[string]float64{}
	for rows.Next() {
		var m, d string
		var x v
		if err = rows.Scan(&m, &d, &x.c, &x.t); err != nil {
			return nil, err
		}
		if data[m] == nil {
			data[m] = map[string]v{}
		}
		data[m][d] = x
		totals[m] += x.c
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(data))
	for m := range data {
		names = append(names, m)
	}
	sort.Slice(names, func(i, j int) bool { return totals[names[i]] > totals[names[j]] })
	out := []taskRunAnalyticsModelCostSeries{}
	other := map[string]v{}
	for i, m := range names {
		if i < 4 {
			out = append(out, taskRunAnalyticsModelCostSeries{Model: m})
		} else {
			for d, x := range data[m] {
				y := other[d]
				y.c += x.c
				y.t += x.t
				other[d] = y
			}
		}
	}
	if len(other) > 0 {
		out = append(out, taskRunAnalyticsModelCostSeries{Model: "other"})
		data["other"] = other
	}
	for i := range out {
		for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
			x := data[out[i].Model][d.Format("2006-01-02")]
			out[i].DailySeries = append(out[i].DailySeries, taskRunAnalyticsDailyCost{Date: d.Format("2006-01-02"), CostUsd: x.c, TotalTokens: x.t})
		}
	}
	return out, nil
}

func averageTaskRunAnalyticsCosts(costs []float64) float64 {
	if len(costs) == 0 {
		return 0
	}
	var total float64
	for _, cost := range costs {
		total += cost
	}
	return total / float64(len(costs))
}

func taskRunAnalyticsProjectionConfidence(elapsed int, costs []float64, mean float64) string {
	if elapsed >= 14 && mean != 0 {
		var variance float64
		for _, cost := range costs {
			variance += (cost - mean) * (cost - mean)
		}
		if len(costs) > 1 && math.Sqrt(variance/float64(len(costs)-1))/mean < 0.35 {
			return "high"
		}
	}
	if elapsed >= 7 {
		return "medium"
	}
	return "low"
}
