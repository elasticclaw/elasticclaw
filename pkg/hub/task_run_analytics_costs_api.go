package hub

import (
	"math"
	"net/http"
	"strings"
	"time"
)

type taskRunAnalyticsCostsResponse struct {
	Today          taskRunAnalyticsCostPeriod  `json:"today"`
	Week           taskRunAnalyticsCostAmount  `json:"week"`
	Month          taskRunAnalyticsCostAmount  `json:"month"`
	ProjectedMonth *taskRunAnalyticsProjection `json:"projectedMonth"`
	DailySeries    []taskRunAnalyticsDailyCost `json:"dailySeries"`
}

type taskRunAnalyticsCostPeriod struct {
	CostUsd             float64  `json:"costUsd"`
	TotalTokens         int64    `json:"totalTokens"`
	DeltaPctVsYesterday *float64 `json:"deltaPctVsYesterday"`
}

type taskRunAnalyticsCostAmount struct {
	CostUsd float64 `json:"costUsd"`
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
	response, err := s.readTaskRunAnalyticsCosts(filters, time.Now().UTC())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	jsonOK(w, response)
}

func (s *Server) readTaskRunAnalyticsCosts(filters taskRunAnalyticsFilters, now time.Time) (taskRunAnalyticsCostsResponse, error) {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	seriesStart := today.AddDate(0, 0, -30)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	where := []string{"tenant_id=?", "day >= ?", "day <= ?"}
	args := []any{filters.TenantID, seriesStart.Format("2006-01-02"), today.Format("2006-01-02")}
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

	response := taskRunAnalyticsCostsResponse{DailySeries: make([]taskRunAnalyticsDailyCost, 0, 31)}
	for day := seriesStart; !day.After(today); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		totals := byDay[key]
		response.DailySeries = append(response.DailySeries, taskRunAnalyticsDailyCost{Date: key, CostUsd: totals.costUsd, TotalTokens: totals.totalTokens})
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
	return response, nil
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
