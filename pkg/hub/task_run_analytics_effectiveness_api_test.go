package hub

import (
	"testing"
	"time"
)

func TestTaskRunAnalyticsCostDriversRunFilterConsistentSparkline(t *testing.T) {
	t.Run("run filters use per-run usage", func(t *testing.T) {
		s, db := newTaskRunAnalyticsAPITestServer(t)
		day := time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC)
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "included", AttemptID: "included-attempt", ClawID: "included-claw", TenantID: "test-tenant-id", Status: taskRunStatusFailed, OwnerType: taskRunOwnerFactory, Factory: "factory-a", Repo: "acme/included", Model: "gpt-5", StartedAt: day.UnixMilli(), EstimatedCostUsd: 12})
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "excluded", AttemptID: "excluded-attempt", ClawID: "excluded-claw", TenantID: "test-tenant-id", Status: taskRunStatusClean, OwnerType: taskRunOwnerFactory, Factory: "factory-a", Repo: "acme/excluded", Model: "gpt-5", StartedAt: day.UnixMilli(), EstimatedCostUsd: 99})
		seedTaskRunAnalyticsRunUsage(t, db, "included", day, 12, 120)
		seedTaskRunAnalyticsRunUsage(t, db, "excluded", day, 99, 990)
		seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", day, "eng", "gpt-5", 111, 1110)

		out, err := s.readTaskRunAnalyticsCostDrivers(taskRunAnalyticsFilters{TenantID: "test-tenant-id", Repo: []string{"acme/included"}, FromStartedAt: day.UnixMilli(), ToStartedAt: day.UnixMilli()}, "factory")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Name != "factory-a" || out[0].CostUsd != 12 {
			t.Fatalf("unexpected cost drivers: %#v", out)
		}
		var sparklineCost float64
		for _, cost := range out[0].DailyCost {
			sparklineCost += cost.CostUsd
		}
		if sparklineCost != 12 {
			t.Fatalf("sparkline cost = %v, want 12", sparklineCost)
		}
	})

	t.Run("no run filters use usage daily", func(t *testing.T) {
		s, db := newTaskRunAnalyticsAPITestServer(t)
		day := time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC)
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "factory", AttemptID: "factory-attempt", ClawID: "factory-claw", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Factory: "factory", StartedAt: day.UnixMilli()})
		seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", day, "eng", "gpt-5", 7, 70)

		out, err := s.readTaskRunAnalyticsCostDrivers(taskRunAnalyticsFilters{TenantID: "test-tenant-id", FromStartedAt: day.UnixMilli(), ToStartedAt: day.UnixMilli()}, "factory")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Name != "factory" || len(out[0].DailyCost) != 1 || out[0].DailyCost[0].CostUsd != 7 {
			t.Fatalf("unexpected cost drivers: %#v", out)
		}
	})
}
