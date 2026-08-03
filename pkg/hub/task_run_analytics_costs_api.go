package hub

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

var errTaskRunAnalyticsInvalidCostRange = fmt.Errorf("invalid time range")

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

func clampTaskRunAnalyticsUsageDay(day string, start, end time.Time) string {
	startKey, endKey := start.Format("2006-01-02"), end.Format("2006-01-02")
	if day < startKey {
		return startKey
	}
	if day > endKey {
		return endKey
	}
	return day
}

func (s *Server) handleTaskRunAnalyticsCosts(w http.ResponseWriter, r *http.Request) {
	if !s.requestCanViewCosts(r) {
		jsonError(w, http.StatusForbidden, "costs:read permission required")
		return
	}
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
	scope := r.URL.Query().Get("scope")
	if scope != "" && scope != "ledger" {
		jsonError(w, http.StatusBadRequest, "scope must be ledger")
		return
	}
	response, err := s.readTaskRunAnalyticsCostsWithOptions(filters, time.Now().UTC(), days, r.URL.Query().Get("groupBy"), scope)
	if err != nil {
		if err == errTaskRunAnalyticsInvalidCostRange {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	jsonOK(w, response)
}

func (s *Server) readTaskRunAnalyticsCosts(filters taskRunAnalyticsFilters, now time.Time) (taskRunAnalyticsCostsResponse, error) {
	return s.readTaskRunAnalyticsCostsWithOptions(filters, now, 30, "", "")
}

func (s *Server) readTaskRunAnalyticsCostsWithOptions(filters taskRunAnalyticsFilters, now time.Time, days int, groupBy string, scope string) (taskRunAnalyticsCostsResponse, error) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	seriesEnd := today
	if filters.ToStartedAt > 0 {
		seriesEnd = time.UnixMilli(filters.ToStartedAt).UTC().Truncate(24 * time.Hour)
	}
	seriesStart := today.AddDate(0, 0, -days)
	if filters.FromStartedAt > 0 {
		seriesStart = time.UnixMilli(filters.FromStartedAt).UTC().Truncate(24 * time.Hour)
	} else if filters.ToStartedAt > 0 {
		seriesStart = seriesEnd.AddDate(0, 0, -days)
	}
	if seriesEnd.Before(seriesStart) {
		return taskRunAnalyticsCostsResponse{}, errTaskRunAnalyticsInvalidCostRange
	}
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	byDay, err := s.readTaskRunAnalyticsDailyCostTotals(filters, seriesStart, seriesEnd, scope)
	if err != nil {
		return taskRunAnalyticsCostsResponse{}, err
	}

	response := taskRunAnalyticsCostsResponse{DailySeries: make([]taskRunAnalyticsDailyCost, 0, days+1)}
	for day := seriesStart; !day.After(seriesEnd); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		totals := byDay[key]
		response.DailySeries = append(response.DailySeries, taskRunAnalyticsDailyCost{Date: key, CostUsd: totals.costUsd, TotalTokens: totals.totalTokens})
	}
	if err := s.addTaskRunAnalyticsDailyRunCounts(filters, seriesStart, seriesEnd, response.DailySeries, scope); err != nil {
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
	priorCost, err := s.readTaskRunAnalyticsUsageCost(filters, priorStart, priorEnd, scope)
	if err != nil {
		return response, err
	}
	response.Prior.PeriodCostUsd = priorCost
	if filters.FromStartedAt > 0 || filters.ToStartedAt > 0 {
		response.PriorPeriodCostUsd = &priorCost
	}
	if groupBy == "model" {
		response.SeriesByModel, err = s.readTaskRunAnalyticsCostSeriesByModel(filters, seriesStart, seriesEnd, scope)
		if err != nil {
			return response, err
		}
	}
	return response, nil
}

func (s *Server) readTaskRunAnalyticsUsageCost(f taskRunAnalyticsFilters, start, end time.Time, scope string) (float64, error) {
	// The explicit window is authoritative here. In particular, prior-period
	// callers retain the current request's bounds in f, which must not be
	// combined with this (different) window by taskRunAnalyticsSummaryWhere.
	f.FromStartedAt = 0
	f.ToStartedAt = 0
	totals, err := s.readTaskRunAnalyticsDailyCostTotals(f, start, end, scope)
	if err != nil {
		return 0, err
	}
	var cost float64
	for _, total := range totals {
		cost += total.costUsd
	}
	return cost, nil
}

func taskRunAnalyticsCostsUseRunFilters(f taskRunAnalyticsFilters) bool {
	return len(f.Status) > 0 || len(f.Owner) > 0 || len(f.OwnerType) > 0 || len(f.Integration) > 0 || len(f.Repo) > 0 || len(f.WarningType) > 0 || len(f.FailureType) > 0 || f.HumanTouched != nil || f.MergedPRs != nil || f.RequiresPR != nil || f.AnalyticsEnabled != nil
}

func taskRunAnalyticsUsageDimensionsWhere(f taskRunAnalyticsFilters, table string) ([]string, []any) {
	w := []string{table + ".tenant_id=?"}
	a := []any{f.TenantID}
	addTaskRunAnalyticsInFilter(&w, &a, table+".workspace_name", f.Workspace)
	addTaskRunAnalyticsInFilter(&w, &a, table+".factory_name", f.Factory)
	addTaskRunAnalyticsInFilter(&w, &a, table+".workflow_name", f.Workflow)
	addTaskRunAnalyticsInFilter(&w, &a, table+".model", f.Model)
	return w, a
}

func (s *Server) readTaskRunAnalyticsDailyCostTotals(f taskRunAnalyticsFilters, start, end time.Time, scope string) (map[string]taskRunAnalyticsDailyCostTotals, error) {
	var query string
	var args []any
	if scope != "ledger" {
		where, summaryArgs := taskRunAnalyticsSummaryWhere(f)
		query = `SELECT u.usage_day, COALESCE(SUM(u.committed_cost_usd),0), COALESCE(SUM(u.committed_total_tokens),0) FROM task_run_usage u JOIN (SELECT run_id FROM task_run_summaries ` + where + ` AND started_at>=? AND started_at<=?) s ON s.run_id=u.run_id WHERE u.tenant_id=?`
		query += ` GROUP BY u.usage_day`
		args = append(summaryArgs, start.UnixMilli(), end.AddDate(0, 0, 1).Add(-time.Millisecond).UnixMilli(), f.TenantID)
	} else {
		// Ledger scope intentionally reads usage_daily with dimension filters only;
		// run-level filters do not apply to this whole-ledger view.
		where, a := taskRunAnalyticsUsageDimensionsWhere(f, "usage_daily")
		where = append(where, "day>=?", "day<=?")
		args = append(a, start.Format("2006-01-02"), end.Format("2006-01-02"))
		query = `SELECT day, COALESCE(SUM(cost_usd),0), COALESCE(SUM(total_tokens),0) FROM usage_daily WHERE ` + strings.Join(where, " AND ") + ` GROUP BY day`
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[string]taskRunAnalyticsDailyCostTotals{}
	for rows.Next() {
		var day string
		var totals taskRunAnalyticsDailyCostTotals
		if err = rows.Scan(&day, &totals.costUsd, &totals.totalTokens); err != nil {
			return nil, err
		}
		if scope != "ledger" {
			day = clampTaskRunAnalyticsUsageDay(day, start, end)
			current := byDay[day]
			current.costUsd += totals.costUsd
			current.totalTokens += totals.totalTokens
			byDay[day] = current
		} else {
			byDay[day] = totals
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return byDay, nil
}

// RunCount is the number of distinct runs with usage recorded on that day.
func (s *Server) addTaskRunAnalyticsDailyRunCounts(f taskRunAnalyticsFilters, start, end time.Time, series []taskRunAnalyticsDailyCost, scope string) error {
	where, args := taskRunAnalyticsUsageDimensionsWhere(f, "s")
	if scope != "ledger" {
		var summaryArgs []any
		var summaryWhere string
		summaryWhere, summaryArgs = taskRunAnalyticsSummaryWhere(f)
		where = []string{"u.tenant_id=?", "s.run_id=u.run_id"}
		args = append(summaryArgs, start.UnixMilli(), end.AddDate(0, 0, 1).Add(-time.Millisecond).UnixMilli(), f.TenantID)
		rows, err := s.db.Query(`SELECT u.usage_day, u.run_id FROM task_run_usage u JOIN (SELECT run_id FROM task_run_summaries `+summaryWhere+` AND started_at>=? AND started_at<=?) s ON s.run_id=u.run_id WHERE `+strings.Join(where, " AND ")+` GROUP BY u.usage_day, u.run_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		return assignTaskRunAnalyticsFoldedDailyRunCounts(rows, series, start, end)
	}
	where = append(where, "u.usage_day>=?", "u.usage_day<=?")
	args = append(args, start.Format("2006-01-02"), end.Format("2006-01-02"))
	rows, err := s.db.Query(`SELECT u.usage_day, COUNT(DISTINCT u.run_id) FROM task_run_usage u JOIN task_run_summaries s ON s.tenant_id=u.tenant_id AND s.run_id=u.run_id WHERE `+strings.Join(where, " AND ")+` GROUP BY u.usage_day`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	return assignTaskRunAnalyticsDailyRunCounts(rows, series)
}

func assignTaskRunAnalyticsFoldedDailyRunCounts(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, series []taskRunAnalyticsDailyCost, start, end time.Time) error {
	runsByDay := map[string]map[string]struct{}{}
	for rows.Next() {
		var day, runID string
		if err := rows.Scan(&day, &runID); err != nil {
			return err
		}
		day = clampTaskRunAnalyticsUsageDay(day, start, end)
		if runsByDay[day] == nil {
			runsByDay[day] = map[string]struct{}{}
		}
		runsByDay[day][runID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range series {
		series[i].RunCount = len(runsByDay[series[i].Date])
	}
	return nil
}

func assignTaskRunAnalyticsDailyRunCounts(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, series []taskRunAnalyticsDailyCost) error {
	counts := map[string]int{}
	for rows.Next() {
		var day string
		var count int
		if err := rows.Scan(&day, &count); err != nil {
			return err
		}
		counts[day] = count
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range series {
		series[i].RunCount = counts[series[i].Date]
	}
	return nil
}

func (s *Server) readTaskRunAnalyticsCostSeriesByModel(f taskRunAnalyticsFilters, start, end time.Time, scope string) ([]taskRunAnalyticsModelCostSeries, error) {
	var query string
	var args []any
	if scope != "ledger" {
		where, summaryArgs := taskRunAnalyticsSummaryWhere(f)
		query = `SELECT u.model, u.usage_day, COALESCE(SUM(u.committed_cost_usd),0), COALESCE(SUM(u.committed_total_tokens),0) FROM task_run_usage u JOIN (SELECT run_id FROM task_run_summaries ` + where + ` AND started_at>=? AND started_at<=?) s ON s.run_id=u.run_id WHERE u.tenant_id=?`
		query += ` GROUP BY u.model,u.usage_day`
		args = append(summaryArgs, start.UnixMilli(), end.AddDate(0, 0, 1).Add(-time.Millisecond).UnixMilli(), f.TenantID)
	} else {
		where, a := taskRunAnalyticsUsageDimensionsWhere(f, "usage_daily")
		where = append(where, "day>=?", "day<=?")
		query = `SELECT model, day, COALESCE(SUM(cost_usd),0), COALESCE(SUM(total_tokens),0) FROM usage_daily WHERE ` + strings.Join(where, " AND ") + ` GROUP BY model,day`
		args = append(a, start.Format("2006-01-02"), end.Format("2006-01-02"))
	}
	rows, err := s.db.Query(query, args...)
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
		if scope != "ledger" {
			d = clampTaskRunAnalyticsUsageDay(d, start, end)
			current := data[m][d]
			current.c += x.c
			current.t += x.t
			data[m][d] = current
		} else {
			data[m][d] = x
		}
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
