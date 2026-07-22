package hub

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestTaskRunAnalyticsEffectivenessIncludesPriorWindow(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	currentStart := time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC)
	currentEnd := currentStart.AddDate(0, 0, 1).Add(-time.Millisecond)
	priorStart := currentStart.AddDate(0, 0, -1)

	insert := func(runID, status string, startedAt time.Time) {
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
			RunID: runID, AttemptID: runID + "-attempt", ClawID: runID + "-claw",
			TenantID: "test-tenant-id", Status: status, Phase: taskRunPhaseTerminal,
			OwnerType: taskRunOwnerFactory, Factory: "factory", StartedAt: startedAt.UnixMilli(),
			FinishedAt: startedAt.Add(time.Minute).UnixMilli(),
		})
	}

	insert("current-success", taskRunStatusClean, currentStart.Add(time.Hour))
	insert("current-failure", taskRunStatusFailed, currentStart.Add(2*time.Hour))
	insert("prior-success-one", taskRunStatusClean, priorStart.Add(time.Hour))
	insert("prior-success-two", taskRunStatusClean, priorStart.Add(2*time.Hour))
	insert("prior-failure", taskRunStatusFailed, priorStart.Add(3*time.Hour))

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/effectiveness?from="+currentStart.Format(time.RFC3339)+"&to="+currentEnd.Format(time.RFC3339Nano), "test-token")
	var response taskRunAnalyticsEffectivenessResponse
	decodeTaskRunAnalyticsAPI(t, rr, &response)

	if response.SuccessRate != .5 || response.TicketSuccessRate != .5 || response.UniqueTickets != 2 {
		t.Fatalf("current effectiveness = %#v", response)
	}
	if response.Prior == nil {
		t.Fatal("prior effectiveness is missing")
	}
	if response.Prior.SuccessRate != 2.0/3.0 || response.Prior.TicketSuccessRate != 2.0/3.0 || response.Prior.UniqueTickets != 3 {
		t.Fatalf("prior effectiveness = %#v", response.Prior)
	}
}

func TestTaskRunAnalyticsEffectivenessTicketAggregates(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	day1 := time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC)
	insert := func(runID, issueID, title, status string, startedAt time.Time, cost float64) {
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
			RunID: runID, AttemptID: runID + "-attempt", ClawID: runID + "-claw", TenantID: "test-tenant-id",
			Status: status, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory, Factory: "factory",
			StartedAt: startedAt.UnixMilli(), FinishedAt: startedAt.Add(time.Minute).UnixMilli(), EstimatedCostUsd: cost, IssueTitle: title,
		})
		if _, err := db.Exec(`UPDATE task_run_summaries SET issue_id=? WHERE run_id=?`, issueID, runID); err != nil {
			t.Fatal(err)
		}
	}
	// Ticket A crosses days; its first run determines its day and its latest run determines its title.
	insert("a-1", "A", "old A", taskRunStatusFailed, day1.Add(time.Hour), 5)
	insert("a-2", "A", "latest A", taskRunStatusClean, day1.AddDate(0, 0, 1).Add(time.Hour), 10)
	insert("b-1", "B", "B title", taskRunStatusRunning, day1.AddDate(0, 0, 1), 30)
	for i, cost := range []float64{5, 5, 10} {
		insert(fmt.Sprintf("c-%d", i+1), "C", "C title", taskRunStatusFailed, day1.AddDate(0, 0, 2).Add(time.Duration(i+1)*time.Hour), cost)
	}
	for i := 1; i <= 4; i++ {
		insert(fmt.Sprintf("d-%d", i), "D", "D title", taskRunStatusWarning, day1.AddDate(0, 0, 3).Add(time.Duration(i)*time.Hour), 10)
	}
	insert("zero", "ZERO", "zero title", taskRunStatusFailed, day1.AddDate(0, 0, 4), 0)
	insert("no-ticket", "", "ignored", taskRunStatusClean, day1.AddDate(0, 0, 5), 99)

	out, err := s.readTaskRunAnalyticsEffectiveness(taskRunAnalyticsFilters{TenantID: "test-tenant-id", FromStartedAt: day1.UnixMilli(), ToStartedAt: day1.AddDate(0, 0, 6).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.TicketsByDay) != 5 || out.TicketsByDay[0] != (taskRunAnalyticsTicketsDay{Date: "2026-07-10", Delivered: 1}) || out.TicketsByDay[1] != (taskRunAnalyticsTicketsDay{Date: "2026-07-11", InProgress: 1}) || out.TicketsByDay[2] != (taskRunAnalyticsTicketsDay{Date: "2026-07-12", Failed: 1}) || out.TicketsByDay[3] != (taskRunAnalyticsTicketsDay{Date: "2026-07-13", Delivered: 1}) || out.TicketsByDay[4] != (taskRunAnalyticsTicketsDay{Date: "2026-07-14", Failed: 1}) {
		t.Fatalf("tickets by day = %#v", out.TicketsByDay)
	}
	wantBuckets := []taskRunAnalyticsRunsPerTicket{{Bucket: "1", Tickets: 2}, {Bucket: "2", Tickets: 1}, {Bucket: "3", Tickets: 1}, {Bucket: "4+", Tickets: 1}}
	if !reflect.DeepEqual(out.RunsPerTicket, wantBuckets) {
		t.Fatalf("runs per ticket = %#v, want %#v", out.RunsPerTicket, wantBuckets)
	}
	wantTop := []taskRunAnalyticsTopTicket{
		{IssueID: "D", IssueTitle: "D title", CostUsd: 40, Runs: 4, Outcome: "delivered"},
		{IssueID: "B", IssueTitle: "B title", CostUsd: 30, Runs: 1, Outcome: "in_progress"},
		{IssueID: "C", IssueTitle: "C title", CostUsd: 20, Runs: 3, Outcome: "failed"},
		{IssueID: "A", IssueTitle: "latest A", CostUsd: 15, Runs: 2, Outcome: "delivered"},
	}
	if !reflect.DeepEqual(out.TopTicketsByCost, wantTop) {
		t.Fatalf("top tickets = %#v, want %#v", out.TopTicketsByCost, wantTop)
	}

	empty, err := s.readTaskRunAnalyticsEffectiveness(taskRunAnalyticsFilters{TenantID: "test-tenant-id", Repo: []string{"no-match"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty.RunsPerTicket, []taskRunAnalyticsRunsPerTicket{{Bucket: "1"}, {Bucket: "2"}, {Bucket: "3"}, {Bucket: "4+"}}) {
		t.Fatalf("zero-filled buckets = %#v", empty.RunsPerTicket)
	}
}

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

	t.Run("run filters fold next-day usage into window end", func(t *testing.T) {
		s, db := newTaskRunAnalyticsAPITestServer(t)
		day := time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC)
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "cross-day", AttemptID: "cross-day-attempt", ClawID: "cross-day-claw", TenantID: "test-tenant-id", Status: taskRunStatusFailed, OwnerType: taskRunOwnerFactory, Factory: "factory-a", Repo: "acme/included", Model: "gpt-5", StartedAt: day.UnixMilli(), EstimatedCostUsd: 12})
		seedTaskRunAnalyticsRunUsage(t, db, "cross-day", day.AddDate(0, 0, 1), 12, 120)

		out, err := s.readTaskRunAnalyticsCostDrivers(taskRunAnalyticsFilters{TenantID: "test-tenant-id", Repo: []string{"acme/included"}, FromStartedAt: day.UnixMilli(), ToStartedAt: day.UnixMilli()}, "factory")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || len(out[0].DailyCost) != 1 || out[0].DailyCost[0].CostUsd != 12 {
			t.Fatalf("cross-day sparkline usage was not folded: %#v", out)
		}
	})

	t.Run("no run filters still use per-run usage", func(t *testing.T) {
		s, db := newTaskRunAnalyticsAPITestServer(t)
		day := time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC)
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "factory", AttemptID: "factory-attempt", ClawID: "factory-claw", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Factory: "factory", Model: "gpt-5", StartedAt: day.UnixMilli(), EstimatedCostUsd: 7})
		seedTaskRunAnalyticsRunUsage(t, db, "factory", day, 7, 70)
		seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", day, "eng", "gpt-5", 999, 9990)

		out, err := s.readTaskRunAnalyticsCostDrivers(taskRunAnalyticsFilters{TenantID: "test-tenant-id", FromStartedAt: day.UnixMilli(), ToStartedAt: day.UnixMilli()}, "factory")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Name != "factory" || len(out[0].DailyCost) != 1 || out[0].DailyCost[0].CostUsd != 7 {
			t.Fatalf("unexpected cost drivers: %#v", out)
		}
	})

	t.Run("default eligibility excludes ineligible run from totals and sparkline", func(t *testing.T) {
		s, db := newTaskRunAnalyticsAPITestServer(t)
		day := time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC)
		ineligible := false
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "eligible", AttemptID: "eligible-attempt", ClawID: "eligible-claw", TenantID: "test-tenant-id", Status: taskRunStatusClean, OwnerType: taskRunOwnerFactory, Workflow: "workflow-a", Model: "gpt-5", StartedAt: day.UnixMilli(), EstimatedCostUsd: 10})
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "ineligible", AttemptID: "ineligible-attempt", ClawID: "ineligible-claw", TenantID: "test-tenant-id", Status: taskRunStatusClean, OwnerType: taskRunOwnerFactory, Workflow: "workflow-a", Model: "gpt-5", AnalyticsEnabled: &ineligible, StartedAt: day.UnixMilli(), EstimatedCostUsd: 50})
		seedTaskRunAnalyticsRunUsage(t, db, "eligible", day, 10, 100)
		seedTaskRunAnalyticsRunUsage(t, db, "ineligible", day, 50, 500)

		// No explicit requires_pr / analytics_enabled filter: defaults kick in.
		out, err := s.readTaskRunAnalyticsCostDrivers(taskRunAnalyticsFilters{TenantID: "test-tenant-id", FromStartedAt: day.UnixMilli(), ToStartedAt: day.UnixMilli()}, "workflow")
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Name != "workflow-a" || out[0].CostUsd != 10 {
			t.Fatalf("unexpected cost drivers: %#v", out)
		}
		var sparklineCost float64
		for _, cost := range out[0].DailyCost {
			sparklineCost += cost.CostUsd
		}
		if sparklineCost != 10 {
			t.Fatalf("sparkline cost = %v, want 10 (must agree with row total %v)", sparklineCost, out[0].CostUsd)
		}
	})
}

func TestTaskRunAnalyticsCostDriversToOnlyRangeAnchorsSparkline(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	now := time.Now().UTC().Truncate(24 * time.Hour)
	to := now.AddDate(0, 0, -60)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "old-run", AttemptID: "old-attempt", ClawID: "old-claw", TenantID: "test-tenant-id", Status: taskRunStatusClean, OwnerType: taskRunOwnerFactory, Factory: "factory", Model: "gpt-5", StartedAt: to.UnixMilli(), EstimatedCostUsd: 12, MergedPRCount: 1})
	seedTaskRunAnalyticsRunUsage(t, db, "old-run", to, 12, 120)
	// Cost drivers always attribute the sparkline by run, never by the ledger;
	// this ledger row exists only to prove it is ignored.
	seedTaskRunAnalyticsUsage(t, db, "test-tenant-id", to, "eng", "gpt-5", 999, 9990)

	out, err := s.readTaskRunAnalyticsCostDrivers(taskRunAnalyticsFilters{TenantID: "test-tenant-id", ToStartedAt: to.UnixMilli()}, "factory")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].CostUsd != 12 || len(out[0].DailyCost) == 0 {
		t.Fatalf("unexpected cost drivers: %#v", out)
	}
	var dailyCost float64
	for _, day := range out[0].DailyCost {
		dailyCost += day.CostUsd
	}
	if dailyCost != out[0].CostUsd {
		t.Fatalf("sparkline cost = %v, total cost = %v", dailyCost, out[0].CostUsd)
	}
}

func TestTaskRunAnalyticsCostDriversInvalidRange(t *testing.T) {
	s, _ := newTaskRunAnalyticsAPITestServer(t)
	now := time.Now().UTC().Truncate(24 * time.Hour)
	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/cost-drivers?from="+now.Format(time.RFC3339)+"&to="+now.AddDate(0, 0, -1).Format(time.RFC3339), "test-token")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid range status = %d, body = %s", rr.Code, rr.Body.String())
	}
}
