package hub

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTaskRunAnalyticsCostsAggregatesFiltersAndTenantScope(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now, "eng", "gpt-5", 10, 100)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now.AddDate(0, 0, -1), "eng", "gpt-5", 5, 50)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now.AddDate(0, 0, -5), "eng", "gpt-5", 7, 70)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now.AddDate(0, 0, -20), "eng", "gpt-5", 3, 30)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now, "other", "gpt-4", 100, 1000)
	seedTaskRunAnalyticsUsage(t, db, "other-tenant-id", now, "eng", "gpt-5", 1000, 10000)

	response, err := s.readTaskRunAnalyticsCostsWithOptions(taskRunAnalyticsFilters{TenantID: "test-tenant-id", Workspace: []string{"eng"}, Model: []string{"gpt-5"}}, now, 30, "", "ledger")
	if err != nil {
		t.Fatal(err)
	}
	if response.Today.CostUsd != 10 || response.Today.TotalTokens != 100 || response.Week.CostUsd != 22 || response.Month.CostUsd != 22 {
		t.Fatalf("unexpected aggregates: %#v", response)
	}
	if response.Today.DeltaPctVsYesterday == nil || *response.Today.DeltaPctVsYesterday != 100 {
		t.Fatalf("unexpected delta: %#v", response.Today.DeltaPctVsYesterday)
	}
	if len(response.DailySeries) != 31 || response.DailySeries[0].Date != "2026-06-15" || response.DailySeries[0].CostUsd != 0 || response.DailySeries[30].CostUsd != 10 {
		t.Fatalf("unexpected daily series: %#v", response.DailySeries)
	}
}

func TestTaskRunAnalyticsCostsDeltaAndProjection(t *testing.T) {
	t.Run("null delta and no data projection", func(t *testing.T) {
		s, db := newTaskRunAnalyticsAPITestServer(t)
		now := time.Date(2026, time.July, 3, 0, 0, 0, 0, time.UTC)
		response, err := s.readTaskRunAnalyticsCostsWithOptions(taskRunAnalyticsFilters{TenantID: "test-tenant-id"}, now, 30, "", "ledger")
		if err != nil || response.ProjectedMonth != nil {
			t.Fatalf("response = %#v, err = %v", response, err)
		}
		seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now, "eng", "gpt-5", 2, 20)
		response, err = s.readTaskRunAnalyticsCostsWithOptions(taskRunAnalyticsFilters{TenantID: "test-tenant-id"}, now, 30, "", "ledger")
		if err != nil || response.Today.DeltaPctVsYesterday != nil {
			t.Fatalf("response = %#v, err = %v", response, err)
		}
	})
	for _, test := range []struct {
		name  string
		now   time.Time
		costs []float64
		want  string
	}{
		{"low", time.Date(2026, time.July, 3, 0, 0, 0, 0, time.UTC), []float64{10, 10, 10, 10, 10, 10, 10}, "low"},
		{"medium at 7 complete days", time.Date(2026, time.July, 8, 0, 0, 0, 0, time.UTC), []float64{1, 100, 1, 100, 1, 100, 1}, "medium"},
		{"high at 14 complete days", time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC), []float64{10, 10, 10, 10, 10, 10, 10}, "high"},
		{"medium downgrade from high variance", time.Date(2026, time.July, 20, 0, 0, 0, 0, time.UTC), []float64{1, 100, 1, 100, 1, 100, 1}, "medium"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, db := newTaskRunAnalyticsAPITestServer(t)
			for i, cost := range test.costs {
				seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", test.now.AddDate(0, 0, -7+i), "eng", "gpt-5", cost, 1)
			}
			response, err := s.readTaskRunAnalyticsCostsWithOptions(taskRunAnalyticsFilters{TenantID: "test-tenant-id"}, test.now, 30, "", "ledger")
			if err != nil || response.ProjectedMonth == nil || response.ProjectedMonth.Confidence != test.want {
				t.Fatalf("response = %#v, err = %v", response, err)
			}
		})
	}
}

func TestTaskRunAnalyticsCostsProjectionTreatsTodayAsFullDay(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	for day := now.AddDate(0, 0, -7); day.Before(now); day = day.AddDate(0, 0, 1) {
		seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", day, "eng", "gpt-5", 10, 1)
	}
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now, "eng", "gpt-5", 2, 1)

	response, err := s.readTaskRunAnalyticsCostsWithOptions(taskRunAnalyticsFilters{TenantID: "test-tenant-id"}, now, 30, "", "ledger")
	if err != nil || response.ProjectedMonth == nil {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if response.Month.CostUsd != 72 || response.ProjectedMonth.CostUsd != 240 {
		t.Fatalf("month = %v, projected month = %v", response.Month.CostUsd, response.ProjectedMonth.CostUsd)
	}
}

func TestTaskRunAnalyticsProjectionConfidenceUsesSampleStandardDeviation(t *testing.T) {
	costs := []float64{36.5, 100, 100, 100, 100, 100, 163.5}
	if got := taskRunAnalyticsProjectionConfidence(14, costs, averageTaskRunAnalyticsCosts(costs)); got != "medium" {
		t.Fatalf("confidence = %q, want medium", got)
	}
}

func TestTaskRunAnalyticsCostsHandler(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", time.Now().UTC(), "eng", "gpt-5", 12.5, 123)
	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/costs?scope=ledger", "test-token")
	var response taskRunAnalyticsCostsResponse
	decodeTaskRunAnalyticsAPI(t, rr, &response)
	if response.Today.CostUsd != 12.5 || response.Today.TotalTokens != 123 {
		t.Fatalf("unexpected handler response: %#v", response.Today)
	}
}

func TestTaskRunAnalyticsCostsDefaultScopeExcludesIneligibleRunsLedgerIncludesAll(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "eligible", AttemptID: "eligible-attempt", ClawID: "eligible-claw", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Factory: "factory", Model: "gpt-5", StartedAt: now.UnixMilli()})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "ineligible", AttemptID: "ineligible-attempt", ClawID: "ineligible-claw", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Factory: "factory", Model: "gpt-5", StartedAt: now.UnixMilli(), AnalyticsEnabled: boolPtr(false)})
	seedTaskRunAnalyticsRunUsage(t, db, "eligible", now, 12, 120)
	seedTaskRunAnalyticsRunUsage(t, db, "ineligible", now, 34, 340)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now, "eligible-ledger", "gpt-5", 12, 120)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now, "ineligible-ledger", "gpt-5", 34, 340)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", now, "untracked-ledger", "gpt-5", 56, 560)

	filters := taskRunAnalyticsFilters{TenantID: "test-tenant-id"}
	response, err := s.readTaskRunAnalyticsCostsWithOptions(filters, now, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if response.Today.CostUsd != 12 {
		t.Fatalf("default scope cost = %v, want eligible run cost 12", response.Today.CostUsd)
	}
	response, err = s.readTaskRunAnalyticsCostsWithOptions(filters, now, 1, "", "ledger")
	if err != nil {
		t.Fatal(err)
	}
	if response.Today.CostUsd != 102 {
		t.Fatalf("ledger scope cost = %v, want full ledger cost 102", response.Today.CostUsd)
	}
}

func TestTaskRunAnalyticsCostsRunFiltersUseUsageByRun(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	startedAt := now.AddDate(0, 0, -1).UnixMilli()
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "included", AttemptID: "included-attempt", ClawID: "included-claw", TenantID: "test-tenant-id", Status: taskRunStatusFailed, OwnerType: taskRunOwnerFactory, Workspace: "eng", Factory: "factory", Repo: "acme/included", Model: "gpt-5", StartedAt: startedAt, FailureType: taskRunFailureTimeout})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "excluded", AttemptID: "excluded-attempt", ClawID: "excluded-claw", TenantID: "test-tenant-id", Status: taskRunStatusClean, OwnerType: taskRunOwnerFactory, Workspace: "eng", Factory: "factory", Repo: "acme/excluded", Model: "gpt-5", StartedAt: startedAt})
	seedTaskRunAnalyticsRunUsage(t, db, "included", now, 12, 120)
	seedTaskRunAnalyticsRunUsage(t, db, "excluded", now, 99, 990)

	response, err := s.readTaskRunAnalyticsCosts(taskRunAnalyticsFilters{TenantID: "test-tenant-id", Status: []string{taskRunStatusFailed}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if response.Today.CostUsd != 12 || response.Today.TotalTokens != 120 || response.DailySeries[len(response.DailySeries)-1].RunCount != 1 {
		t.Fatalf("run-filtered costs and daily run count disagree: %#v", response)
	}
	if response.Prior.PeriodCostUsd != 0 {
		t.Fatalf("prior cost included filtered-out usage: %#v", response.Prior)
	}
}

func TestTaskRunAnalyticsCostsUseRunFiltersDetectsEligibilityFilters(t *testing.T) {
	for _, test := range []struct {
		name string
		f    taskRunAnalyticsFilters
		want bool
	}{
		{name: "no filters", want: false},
		{name: "requires PR", f: taskRunAnalyticsFilters{RequiresPR: boolPtr(true)}, want: true},
		{name: "analytics enabled", f: taskRunAnalyticsFilters{AnalyticsEnabled: boolPtr(true)}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := taskRunAnalyticsCostsUseRunFilters(test.f); got != test.want {
				t.Fatalf("taskRunAnalyticsCostsUseRunFilters(%#v) = %v, want %v", test.f, got, test.want)
			}
		})
	}
}

func TestTaskRunAnalyticsCostsIncludesCrossDayUsage(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	filters := taskRunAnalyticsFilters{TenantID: "test-tenant-id", Status: []string{taskRunStatusFailed}}
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "cross-day", AttemptID: "cross-day-attempt", ClawID: "cross-day-claw", TenantID: "test-tenant-id", Status: taskRunStatusFailed, OwnerType: taskRunOwnerFactory, Factory: "factory", Model: "gpt-5", StartedAt: now.Add(-time.Hour).UnixMilli()})
	seedTaskRunAnalyticsRunUsage(t, db, "cross-day", now.AddDate(0, 0, 1), 12, 120)

	response, err := s.readTaskRunAnalyticsCosts(filters, now)
	if err != nil {
		t.Fatal(err)
	}
	last := response.DailySeries[len(response.DailySeries)-1]
	if response.Today.CostUsd != 12 || response.Today.TotalTokens != 120 || last.CostUsd != 12 || last.TotalTokens != 120 || last.RunCount != 1 || response.Week.CostUsd != 12 || response.Month.CostUsd != 12 {
		t.Fatalf("cross-day usage was not folded into today: %#v", response)
	}

	byModel, err := s.readTaskRunAnalyticsCostsWithOptions(filters, now, 30, "model", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(byModel.SeriesByModel) != 1 {
		t.Fatalf("series by model = %#v", byModel.SeriesByModel)
	}
	modelLast := byModel.SeriesByModel[0].DailySeries[len(byModel.SeriesByModel[0].DailySeries)-1]
	if byModel.SeriesByModel[0].Model != "gpt-5" || modelLast.CostUsd != 12 || modelLast.TotalTokens != 120 {
		t.Fatalf("cross-day model usage was not folded into today: %#v", byModel.SeriesByModel)
	}

	priorEnd := now.AddDate(0, 0, -31).Truncate(24 * time.Hour)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "prior-cross-day", AttemptID: "prior-cross-day-attempt", ClawID: "prior-cross-day-claw", TenantID: "test-tenant-id", Status: taskRunStatusFailed, OwnerType: taskRunOwnerFactory, Factory: "factory", Model: "gpt-5", StartedAt: priorEnd.UnixMilli()})
	seedTaskRunAnalyticsRunUsage(t, db, "prior-cross-day", priorEnd.AddDate(0, 0, 1), 7, 70)
	response, err = s.readTaskRunAnalyticsCosts(filters, now)
	if err != nil {
		t.Fatal(err)
	}
	if response.Prior.PeriodCostUsd != 7 {
		t.Fatalf("prior cross-day usage was dropped: %#v", response.Prior)
	}
}

func TestTaskRunAnalyticsCostsToOnlyRangeAndInvalidRange(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	to := now.AddDate(0, 0, -10).Truncate(24 * time.Hour)
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", to.AddDate(0, 0, -2), "eng", "gpt-5", 7, 70)
	response, err := s.readTaskRunAnalyticsCostsWithOptions(taskRunAnalyticsFilters{TenantID: "test-tenant-id", ToStartedAt: to.UnixMilli()}, now, 2, "", "ledger")
	if err != nil {
		t.Fatal(err)
	}
	if len(response.DailySeries) != 3 || response.DailySeries[0].Date != "2026-07-03" || response.DailySeries[2].Date != "2026-07-05" || response.DailySeries[0].CostUsd != 7 {
		t.Fatalf("to-only range was not anchored to to: %#v", response.DailySeries)
	}
	_, err = s.readTaskRunAnalyticsCostsWithOptions(taskRunAnalyticsFilters{TenantID: "test-tenant-id", FromStartedAt: now.UnixMilli(), ToStartedAt: now.AddDate(0, 0, -1).UnixMilli()}, now, 30, "", "ledger")
	if err != errTaskRunAnalyticsInvalidCostRange {
		t.Fatalf("invalid range error = %v, want %v", err, errTaskRunAnalyticsInvalidCostRange)
	}
	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/costs?from="+now.Format(time.RFC3339)+"&to="+now.AddDate(0, 0, -1).Format(time.RFC3339)+"&scope=ledger", "test-token")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid range status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestTaskRunAnalyticsRunUsageFields(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "usage", AttemptID: "usage-attempt", ClawID: "usage-claw", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Workspace: "eng", Factory: "factory", StartedAt: 1, InputTokens: 10, OutputTokens: 20, TotalTokens: 30, EstimatedCostUsd: 1.25, UsageUpdatedAt: 1, IssueTitle: "Fix the thing"})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "zero", AttemptID: "zero-attempt", ClawID: "zero-claw", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Workspace: "eng", Factory: "factory", StartedAt: 2})
	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", "test-token")
	var response taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, rr, &response)
	if len(response.Runs) != 2 || response.Runs[0].RunID != "zero" || response.Runs[1].InputTokens == nil || *response.Runs[1].InputTokens != 10 || response.Runs[1].OutputTokens == nil || *response.Runs[1].OutputTokens != 20 || response.Runs[1].TotalTokens == nil || *response.Runs[1].TotalTokens != 30 || response.Runs[1].EstimatedCostUsd == nil || *response.Runs[1].EstimatedCostUsd != 1.25 || response.Runs[1].IssueTitle != "Fix the thing" {
		t.Fatalf("unexpected usage fields: %#v", response.Runs)
	}
	if strings.Contains(rr.Body.String(), `"inputTokens":0`) || strings.Contains(rr.Body.String(), `"issueTitle":""`) {
		t.Fatalf("zero usage fields were not omitted: %s", rr.Body.String())
	}
	detailRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/usage", "test-token")
	var detail taskRunAnalyticsRunDetailResponse
	decodeTaskRunAnalyticsAPI(t, detailRR, &detail)
	if detail.Run.TotalTokens == nil || *detail.Run.TotalTokens != 30 || detail.Run.IssueTitle != "Fix the thing" {
		t.Fatalf("unexpected detail usage fields: %#v", detail.Run)
	}
}

func seedTaskRunAnalyticsUsage(t *testing.T, db *sql.DB, tenant string, day time.Time, workspace, model string, cost float64, tokens int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO usage_daily(tenant_id, day, workspace_name, factory_name, workflow_name, model, total_tokens, cost_usd, updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, tenant, day.UTC().Format("2006-01-02"), workspace, "factory", "workflow", model, tokens, cost, 1)
	if err != nil {
		t.Fatal(err)
	}
}

func seedTaskRunAnalyticsRunUsage(t *testing.T, db *sql.DB, runID string, day time.Time, cost float64, tokens int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO task_run_usage(id, tenant_id, run_id, session_key, model, committed_total_tokens, committed_cost_usd, usage_day, first_seen_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, "usage-"+runID, "test-tenant-id", runID, "session-"+runID, "gpt-5", tokens, cost, day.Format("2006-01-02"), day.UnixMilli(), day.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
}
