package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestTaskRunAnalyticsAPISummaryRunsFiltersAndPagination(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	ts := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-clean", AttemptID: "attempt-clean", ClawID: "claw-clean", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "eng", Factory: "bugfix", Integration: "github", Repo: "elastic/claw", Model: "gpt-5",
		StartedAt: ts + 3000, MergedAt: ts + 5000, FinishedAt: ts + 5000, PRCount: 1, MergedPRCount: 1,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-warning", AttemptID: "attempt-warning", ClawID: "claw-warning", TenantID: "test-tenant-id",
		Status: taskRunStatusHumanInTheLoop, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow,
		Workspace: "platform", Workflow: "review", Integration: "linear", Repo: "elastic/api", Model: "gpt-4.1",
		WarningTypes: []string{taskRunWarningHumanPRComment}, StartedAt: ts + 2000, MergedAt: ts + 4000,
		FinishedAt: ts + 4000, HumanInteractions: 2, PRCount: 2, ClosedPRCount: 1, MergedPRCount: 1,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-failed", AttemptID: "attempt-failed", ClawID: "claw-failed", TenantID: "test-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "eng", Factory: "bugfix", Integration: "github", Repo: "elastic/claw", Model: "gpt-5",
		FailureType: taskRunFailureDoneWithoutPR, StartedAt: ts + 1000, FinishedAt: ts + 2500,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-running", AttemptID: "attempt-running", ClawID: "claw-running", TenantID: "test-tenant-id",
		Status: taskRunStatusRunning, Phase: taskRunPhaseWaitingForMerge, OwnerType: taskRunOwnerWorkflow,
		Workspace: "platform", Workflow: "review", Integration: "linear", Repo: "elastic/api", Model: "gpt-5",
		StartedAt: ts, HumanInteractions: 1, PRCount: 1, OpenPRCount: 1,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-non-pr", AttemptID: "attempt-non-pr", ClawID: "claw-non-pr", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow,
		Workspace: "internal", Workflow: "note", Integration: "external", Repo: "elastic/internal", Model: "gpt-5",
		StartedAt: ts + 8000, RequiresPR: boolPtr(false), ExcludedReason: "not_pr_task",
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-disabled", AttemptID: "attempt-disabled", ClawID: "claw-disabled", TenantID: "test-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "disabled", Factory: "hidden", Integration: "github", Repo: "elastic/hidden", Model: "gpt-4.1",
		FailureType: taskRunFailureTimeout, StartedAt: ts + 7000, AnalyticsEnabled: boolPtr(false), ExcludedReason: "analytics_disabled",
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-other-tenant", AttemptID: "attempt-other", ClawID: "claw-other", TenantID: "other-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "secret", Factory: "hidden", Integration: "github", Repo: "secret/repo", Model: "hidden",
		FailureType: taskRunFailureTimeout, StartedAt: ts + 6000, FinishedAt: ts + 7000,
	})

	summaryRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/summary?workspace=eng&status=failed&failure_type=done_without_pr", "test-token")
	if summaryRR.Code != http.StatusOK {
		t.Fatalf("summary status = %d, body = %s", summaryRR.Code, summaryRR.Body.String())
	}
	var summary taskRunAnalyticsSummaryResponse
	decodeTaskRunAnalyticsAPI(t, summaryRR, &summary)
	if summary.TotalRuns != 1 || summary.ByStatus[taskRunStatusFailed] != 1 || summary.FailureBreakdown[taskRunFailureDoneWithoutPR] != 1 {
		t.Fatalf("unexpected filtered summary: %#v", summary)
	}

	allSummaryRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/summary", "test-token")
	var allSummary taskRunAnalyticsSummaryResponse
	decodeTaskRunAnalyticsAPI(t, allSummaryRR, &allSummary)
	if allSummary.TotalRuns != 4 || allSummary.ByStatus[taskRunStatusClean] != 1 || allSummary.ByStatus[taskRunStatusHumanInTheLoop] != 1 || allSummary.ByStatus[taskRunStatusRunning] != 1 || allSummary.ByStatus[taskRunStatusFailed] != 1 {
		t.Fatalf("summary should include only authenticated tenant runs, got %#v", allSummary)
	}
	if allSummary.WarningBreakdown[taskRunWarningHumanPRComment] != 1 || allSummary.HumanInteractions != 3 {
		t.Fatalf("summary warning/human counts mismatch: %#v", allSummary)
	}
	if allSummary.PRCounts.Total != 4 || allSummary.PRCounts.Open != 1 || allSummary.PRCounts.Merged != 2 || allSummary.PRCounts.Closed != 1 {
		t.Fatalf("summary PR counts mismatch: %#v", allSummary.PRCounts)
	}

	page1RR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs?limit=2", "test-token")
	var page1 taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, page1RR, &page1)
	if page1.Limit != 2 || len(page1.Runs) != 2 || page1.NextCursor == "" {
		t.Fatalf("unexpected first page: %#v", page1)
	}
	if page1.Runs[0].RunID != "run-clean" || page1.Runs[1].RunID != "run-warning" {
		t.Fatalf("runs are not ordered by started_at desc, run_id desc: %#v", page1.Runs)
	}
	if strings.Contains(page1RR.Body.String(), "run-other-tenant") {
		t.Fatalf("runs response leaked another tenant: %s", page1RR.Body.String())
	}
	if strings.Contains(page1RR.Body.String(), "run-disabled") || strings.Contains(page1RR.Body.String(), "run-non-pr") {
		t.Fatalf("runs response included excluded/non-PR run by default: %s", page1RR.Body.String())
	}

	trailingSlashRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/?limit=1", "test-token")
	if trailingSlashRR.Code != http.StatusOK {
		t.Fatalf("trailing slash runs status = %d, body = %s", trailingSlashRR.Code, trailingSlashRR.Body.String())
	}
	var trailingSlashPage taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, trailingSlashRR, &trailingSlashPage)
	if trailingSlashPage.Limit != 1 || len(trailingSlashPage.Runs) != 1 {
		t.Fatalf("unexpected trailing slash page: %#v", trailingSlashPage)
	}

	page2RR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs?limit=2&cursor="+page1.NextCursor, "test-token")
	var page2 taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, page2RR, &page2)
	if len(page2.Runs) != 2 || page2.NextCursor != "" || page2.Runs[0].RunID != "run-failed" || page2.Runs[1].RunID != "run-running" {
		t.Fatalf("unexpected second page: %#v", page2)
	}

	warningRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs?warning_type=human_pr_comment", "test-token")
	var warningPage taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, warningRR, &warningPage)
	if len(warningPage.Runs) != 1 || warningPage.Runs[0].RunID != "run-warning" {
		t.Fatalf("warning_type filter mismatch: %#v", warningPage)
	}

	ownerRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs?owner=bugfix", "test-token")
	var ownerPage taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, ownerRR, &ownerPage)
	if len(ownerPage.Runs) != 2 || ownerPage.Runs[0].RunID != "run-clean" || ownerPage.Runs[1].RunID != "run-failed" {
		t.Fatalf("owner filter mismatch: %#v", ownerPage)
	}

	humanTouchedRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs?humanTouched=true", "test-token")
	var humanTouchedPage taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, humanTouchedRR, &humanTouchedPage)
	if len(humanTouchedPage.Runs) != 2 || humanTouchedPage.Runs[0].RunID != "run-warning" || humanTouchedPage.Runs[1].RunID != "run-running" {
		t.Fatalf("humanTouched filter mismatch: %#v", humanTouchedPage)
	}

	mergedPRsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/summary?mergedPrs=true", "test-token")
	var mergedPRsSummary taskRunAnalyticsSummaryResponse
	decodeTaskRunAnalyticsAPI(t, mergedPRsRR, &mergedPRsSummary)
	if mergedPRsSummary.TotalRuns != 2 || mergedPRsSummary.PRCounts.Merged != 2 || mergedPRsSummary.ByStatus[taskRunStatusClean] != 1 || mergedPRsSummary.ByStatus[taskRunStatusHumanInTheLoop] != 1 {
		t.Fatalf("mergedPrs summary mismatch: %#v", mergedPRsSummary)
	}
	mergedPRsRunsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs?mergedPrs=true", "test-token")
	var mergedPRsPage taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, mergedPRsRunsRR, &mergedPRsPage)
	if len(mergedPRsPage.Runs) != 2 || mergedPRsPage.Runs[0].RunID != "run-clean" || mergedPRsPage.Runs[1].RunID != "run-warning" {
		t.Fatalf("mergedPrs runs mismatch: %#v", mergedPRsPage)
	}

	nonPRRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs?requires_pr=false", "test-token")
	var nonPRPage taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, nonPRRR, &nonPRPage)
	if len(nonPRPage.Runs) != 1 || nonPRPage.Runs[0].RunID != "run-non-pr" {
		t.Fatalf("requires_pr=false filter mismatch: %#v", nonPRPage)
	}

	disabledRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/summary?analytics_enabled=false", "test-token")
	var disabledSummary taskRunAnalyticsSummaryResponse
	decodeTaskRunAnalyticsAPI(t, disabledRR, &disabledSummary)
	if disabledSummary.TotalRuns != 1 || disabledSummary.ByStatus[taskRunStatusFailed] != 1 || disabledSummary.FailureBreakdown[taskRunFailureTimeout] != 1 {
		t.Fatalf("analytics_enabled=false summary mismatch: %#v", disabledSummary)
	}
}

func TestTaskRunAnalyticsEffectivenessCountsClosedUnmergedAsSuccessNotMerge(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "closed", AttemptID: "a", ClawID: "c", TenantID: "test-tenant-id", Status: taskRunStatusWarningSuccess, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory, Factory: "f", StartedAt: 1760000000000, FinishedAt: 1760000001000, PRCount: 1, ClosedPRCount: 1, WarningTypes: []string{taskRunWarningPRClosedUnmerged}})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "merged", AttemptID: "b", ClawID: "d", TenantID: "test-tenant-id", Status: taskRunStatusCleanSuccess, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory, Factory: "f", StartedAt: 1760000000001, FinishedAt: 1760000001001, PRCount: 1, MergedPRCount: 1})
	if _, err := db.Exec(`UPDATE task_run_summaries SET pr_opened_at=started_at WHERE run_id IN ('closed','merged')`); err != nil {
		t.Fatal(err)
	}
	got, err := s.readTaskRunAnalyticsEffectiveness(taskRunAnalyticsFilters{TenantID: "test-tenant-id"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SuccessRate != 1 || got.MergeRate != .5 || got.Funnel.PRMerged != 1 {
		t.Fatalf("effectiveness = %#v", got)
	}
}

func TestTaskRunAnalyticsAPIGeneralStats(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "stats-authoritative", AttemptID: "attempt-stats-authoritative", ClawID: "claw-stats-authoritative", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, StartedAt: base, PRCount: 1})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "stats-fallback", AttemptID: "attempt-stats-fallback", ClawID: "claw-stats-fallback", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, StartedAt: base + 1000, PRCount: 1})
	if _, err := db.Exec(`UPDATE task_run_summaries SET issue_created_at=?, agent_started_at=?, pr_opened_at=?, merged_at=? WHERE run_id='stats-authoritative'`, base-1000, base+100, base+5000, base+9000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE task_run_summaries SET agent_started_at=?, pr_opened_at=?, merged_at=? WHERE run_id='stats-fallback'`, base+1100, base+6000, base+10000); err != nil {
		t.Fatal(err)
	}
	// GitHub-triggered shape: the PR pre-dates the run, so opened-started and
	// opened-agent are negative and must be skipped; merged-opened still counts.
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "stats-github", AttemptID: "attempt-stats-github", ClawID: "claw-stats-github", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, StartedAt: base + 20000, PRCount: 1})
	if _, err := db.Exec(`UPDATE task_run_summaries SET agent_started_at=?, pr_opened_at=?, merged_at=? WHERE run_id='stats-github'`, base+20100, base+15000, base+18000); err != nil {
		t.Fatal(err)
	}
	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/general-stats", "test-token")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response taskRunGeneralStatsResponse
	decodeTaskRunAnalyticsAPI(t, rr, &response)
	if response.TicketToPr.AvgMs == nil || *response.TicketToPr.AvgMs != 5500 || response.TicketToPr.Samples != 2 || response.TicketToPr.AuthoritativeSamples == nil || *response.TicketToPr.AuthoritativeSamples != 1 {
		t.Fatalf("ticket stats: %#v", response.TicketToPr)
	}
	if response.PROpenToMerge.AvgMs == nil || *response.PROpenToMerge.AvgMs != 3666 || response.PROpenToMerge.Samples != 3 {
		t.Fatalf("merge stats: %#v", response.PROpenToMerge)
	}
	if response.AIImpl.AvgMs == nil || *response.AIImpl.AvgMs != 4900 || response.AIImpl.Samples != 2 {
		t.Fatalf("AI stats: %#v", response.AIImpl)
	}
}

func TestTaskRunAnalyticsAPIGeneralStatsEmptyAndAuthoritativeSamples(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)

	// No qualifying rows: every avgMs must serialize as JSON null with samples 0.
	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/general-stats", "test-token")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"ticketToPrMs":{"avgMs":null,"samples":0,"authoritativeSamples":0}`) ||
		!strings.Contains(body, `"prOpenToMergeMs":{"avgMs":null,"samples":0}`) ||
		!strings.Contains(body, `"aiImplMs":{"avgMs":null,"samples":0}`) {
		t.Fatalf("unexpected empty-stats body: %s", body)
	}

	// A single run with issue_created_at set contributes one authoritative sample.
	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "stats-auth-only", AttemptID: "attempt-stats-auth-only", ClawID: "claw-stats-auth-only", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, StartedAt: base, PRCount: 1})
	if _, err := db.Exec(`UPDATE task_run_summaries SET issue_created_at=?, pr_opened_at=? WHERE run_id='stats-auth-only'`, base-2000, base+3000); err != nil {
		t.Fatal(err)
	}
	// A fallback row with a negative delta is skipped, not sampled, so it must
	// not affect either ticket sample count.
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "stats-github-skip", AttemptID: "attempt-stats-github-skip", ClawID: "claw-stats-github-skip", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, StartedAt: base + 10000, PRCount: 1})
	if _, err := db.Exec(`UPDATE task_run_summaries SET pr_opened_at=? WHERE run_id='stats-github-skip'`, base+8000); err != nil {
		t.Fatal(err)
	}
	rr = requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/general-stats", "test-token")
	var response taskRunGeneralStatsResponse
	decodeTaskRunAnalyticsAPI(t, rr, &response)
	if response.TicketToPr.AvgMs == nil || *response.TicketToPr.AvgMs != 5000 || response.TicketToPr.Samples != 1 ||
		response.TicketToPr.AuthoritativeSamples == nil || *response.TicketToPr.AuthoritativeSamples != 1 {
		t.Fatalf("ticket stats: %#v", response.TicketToPr)
	}
}

func TestTaskRunAnalyticsAPIGeneralStatsIssueCreatedAtAtRunCreation(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "stats-created-at-run-start", AttemptID: "attempt-stats-created-at-run-start", ClawID: "claw-stats-created-at-run-start", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, StartedAt: base, IssueCreatedAt: base - 2000, PRCount: 1})
	if _, err := db.Exec(`UPDATE task_run_summaries SET pr_opened_at=? WHERE run_id='stats-created-at-run-start'`, base+3000); err != nil {
		t.Fatal(err)
	}

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/general-stats", "test-token")
	var response taskRunGeneralStatsResponse
	decodeTaskRunAnalyticsAPI(t, rr, &response)
	if response.TicketToPr.AvgMs == nil || *response.TicketToPr.AvgMs != 5000 || response.TicketToPr.Samples != 1 ||
		response.TicketToPr.AuthoritativeSamples == nil || *response.TicketToPr.AuthoritativeSamples != 1 {
		t.Fatalf("ticket stats: %#v", response.TicketToPr)
	}
}

func TestTaskRunAnalyticsAPIRunDetailsAttemptsEventsAndPRs(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	ts := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-detail", AttemptID: "attempt-detail", ClawID: "claw-detail", TenantID: "test-tenant-id",
		Status: taskRunStatusHumanInTheLoop, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "eng", Factory: "bugfix", Integration: "github", Repo: "elastic/claw", Model: "gpt-5",
		WarningTypes: []string{taskRunWarningHumanReviewComment}, StartedAt: ts, FinishedAt: ts + 1000,
		HumanInteractions: 1, PRCount: 1, MergedPRCount: 1,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-other-tenant", AttemptID: "attempt-other", ClawID: "claw-other", TenantID: "other-tenant-id",
		Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "secret", Factory: "hidden", Integration: "github", Repo: "secret/repo", Model: "hidden",
		StartedAt: ts,
	})

	detailRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-detail", "test-token")
	var detail taskRunAnalyticsRunDetailResponse
	decodeTaskRunAnalyticsAPI(t, detailRR, &detail)
	if detail.Run.RunID != "run-detail" || detail.Run.WarningTypes[0] != taskRunWarningHumanReviewComment {
		t.Fatalf("unexpected detail response: %#v", detail)
	}

	attemptsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-detail/attempts", "test-token")
	var attempts taskRunAnalyticsAttemptsResponse
	decodeTaskRunAnalyticsAPI(t, attemptsRR, &attempts)
	if len(attempts.Attempts) != 1 || attempts.Attempts[0].AttemptID != "attempt-detail" || attempts.Attempts[0].Status != "succeeded" {
		t.Fatalf("unexpected attempts response: %#v", attempts)
	}

	eventsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-detail/events", "test-token")
	var events taskRunAnalyticsEventsResponse
	decodeTaskRunAnalyticsAPI(t, eventsRR, &events)
	if len(events.Events) != 2 || events.Events[1].EventType != taskRunEventHumanReviewComment || events.Events[1].Detail["path"] != "pkg/hub/server.go" {
		t.Fatalf("unexpected events response: %#v", events)
	}

	prsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-detail/prs", "test-token")
	var prs taskRunAnalyticsPRsResponse
	decodeTaskRunAnalyticsAPI(t, prsRR, &prs)
	if len(prs.PRs) != 1 || prs.PRs[0].Repo != "elastic/claw" || !prs.PRs[0].Merged {
		t.Fatalf("unexpected PRs response: %#v", prs)
	}

	notFoundRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-other-tenant", "test-token")
	if notFoundRR.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant detail status = %d, body = %s", notFoundRR.Code, notFoundRR.Body.String())
	}
}

func TestTaskRunAnalyticsAPIUsageDistinguishesRecordedZeroFromMissing(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	ts := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-usage-zero", AttemptID: "attempt-usage-zero", ClawID: "claw-usage-zero", TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, StartedAt: ts + 1, UsageUpdatedAt: ts + 1,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-usage-missing", AttemptID: "attempt-usage-missing", ClawID: "claw-usage-missing", TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, StartedAt: ts,
	})

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", "test-token")
	var response struct {
		Runs []map[string]json.RawMessage `json:"runs"`
	}
	decodeTaskRunAnalyticsAPI(t, rr, &response)
	if len(response.Runs) != 2 {
		t.Fatalf("run count = %d, want 2", len(response.Runs))
	}
	assertTaskRunAnalyticsUsageJSON(t, response.Runs[0], true)
	assertTaskRunAnalyticsUsageJSON(t, response.Runs[1], false)

	detailRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-usage-zero", "test-token")
	var detail struct {
		Run map[string]json.RawMessage `json:"run"`
	}
	decodeTaskRunAnalyticsAPI(t, detailRR, &detail)
	assertTaskRunAnalyticsUsageJSON(t, detail.Run, true)
}

func assertTaskRunAnalyticsUsageJSON(t *testing.T, run map[string]json.RawMessage, recorded bool) {
	t.Helper()
	for _, field := range []string{"inputTokens", "outputTokens", "totalTokens", "estimatedCostUsd"} {
		value, ok := run[field]
		if ok != recorded {
			t.Errorf("%s present = %t, want %t; run = %v", field, ok, recorded, run)
			continue
		}
		if recorded && string(value) != "0" {
			t.Errorf("%s = %s, want 0", field, value)
		}
	}
}

func TestTaskRunAnalyticsAPIOutputs(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	ts := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-outputs", AttemptID: "attempt-one", ClawID: "claw-one", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: ts,
	})
	if _, err := db.Exec(`
		INSERT INTO task_run_attempts(id, tenant_id, run_id, attempt_id, attempt_number, claw_id, status, started_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"attempt-two", "test-tenant-id", "run-outputs", "attempt-two", 2, "claw-two", "succeeded", ts+100, ts+100, ts+100,
	); err != nil {
		t.Fatalf("insert second attempt: %v", err)
	}
	for _, output := range []struct {
		clawID, stageID, name, stdout, stderr string
		exitCode                              int
		createdAt                             time.Time
	}{
		{clawID: "claw-one", stageID: "prepare", name: "checkout", stdout: "ready", createdAt: time.UnixMilli(ts)},
		{clawID: "claw-two", stageID: "verify", name: "tests", stderr: "failed test", exitCode: 1, createdAt: time.UnixMilli(ts + 200)},
		{clawID: "unrelated", stageID: "hidden", name: "other", stdout: "ignore", createdAt: time.UnixMilli(ts + 300)},
	} {
		if _, err := db.Exec(`
			INSERT INTO pipeline_outputs(claw_id, stage_id, output_name, stdout, stderr, exit_code, created_at)
			VALUES(?,?,?,?,?,?,?)`, output.clawID, output.stageID, output.name, output.stdout, output.stderr, output.exitCode, output.createdAt); err != nil {
			t.Fatalf("insert pipeline output: %v", err)
		}
	}

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-outputs/outputs", "test-token")
	var response taskRunAnalyticsOutputsResponse
	decodeTaskRunAnalyticsAPI(t, rr, &response)
	if len(response.Outputs) != 2 {
		t.Fatalf("outputs = %#v", response.Outputs)
	}
	if response.Outputs[0].AttemptID != "attempt-one" || response.Outputs[0].Stdout != "ready" || response.Outputs[0].CreatedAt != ts {
		t.Fatalf("unexpected first output: %#v", response.Outputs[0])
	}
	if response.Outputs[1].AttemptID != "attempt-two" || response.Outputs[1].ExitCode != 1 || response.Outputs[1].Stderr != "failed test" {
		t.Fatalf("unexpected second output: %#v", response.Outputs[1])
	}

	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-empty", AttemptID: "attempt-empty", ClawID: "claw-empty", TenantID: "test-tenant-id",
		OwnerType: taskRunOwnerFactory, Factory: "bugfix", StartedAt: ts + 1000,
	})
	emptyRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-empty/outputs", "test-token")
	var empty taskRunAnalyticsOutputsResponse
	decodeTaskRunAnalyticsAPI(t, emptyRR, &empty)
	if empty.Outputs == nil || len(empty.Outputs) != 0 {
		t.Fatalf("empty outputs should be an empty array, got %#v", empty.Outputs)
	}
}

func TestTaskRunAnalyticsAPIAccessControl(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Auth: &types.AuthConfig{
			SessionSecret: "analytics-session-secret",
			Access:        &types.AccessConfig{ViewRequiresTags: []string{"owner={user}"}},
		},
	}, "", "", "")
	ts := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-alice", AttemptID: "attempt-alice", ClawID: "claw-alice", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, OwnerType: taskRunOwnerFactory, Factory: "bugfix", StartedAt: ts + 100,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-bob", AttemptID: "attempt-bob", ClawID: "claw-bob", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, OwnerType: taskRunOwnerFactory, Factory: "bugfix", StartedAt: ts,
	})
	for _, claw := range []struct{ id, tags string }{
		{id: "claw-alice", tags: `["owner=alice"]`},
		{id: "claw-bob", tags: `["owner=bob"]`},
	} {
		if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`, claw.id, "test-tenant-id", claw.id, claw.tags); err != nil {
			t.Fatalf("insert claw: %v", err)
		}
	}
	bobSession, err := signGitHubSession("analytics-session-secret", "bob", "", "")
	if err != nil {
		t.Fatal(err)
	}

	listRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", bobSession)
	var list taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, listRR, &list)
	if len(list.Runs) != 1 || list.Runs[0].RunID != "run-bob" {
		t.Fatalf("OAuth list was not ACL-filtered: %#v", list.Runs)
	}
	for _, path := range []string{
		"/api/analytics/runs/run-alice",
		"/api/analytics/runs/run-alice/attempts",
		"/api/analytics/runs/run-alice/events",
		"/api/analytics/runs/run-alice/prs",
		"/api/analytics/runs/run-alice/outputs",
	} {
		rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, path, bobSession)
		if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
			t.Fatalf("OAuth request %s status = %d, body = %s", path, rr.Code, rr.Body.String())
		}
	}
	allowedRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-bob", bobSession)
	if allowedRR.Code != http.StatusOK {
		t.Fatalf("allowed OAuth detail status = %d, body = %s", allowedRR.Code, allowedRR.Body.String())
	}
	if _, err := db.Exec(`DELETE FROM claws WHERE id=?`, "claw-bob"); err != nil {
		t.Fatalf("delete claw: %v", err)
	}
	missingClawListRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", bobSession)
	var missingClawList taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, missingClawListRR, &missingClawList)
	if len(missingClawList.Runs) != 0 {
		t.Fatalf("OAuth list should hide runs with missing claws when ACLs are configured: %#v", missingClawList.Runs)
	}
	missingClawDetailRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-bob", bobSession)
	if missingClawDetailRR.Code != http.StatusNotFound {
		t.Fatalf("OAuth detail with configured ACL and missing claw status = %d, body = %s", missingClawDetailRR.Code, missingClawDetailRR.Body.String())
	}

	plainListRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", "test-token")
	var plainList taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, plainListRR, &plainList)
	if len(plainList.Runs) != 2 {
		t.Fatalf("plain token list should be unrestricted: %#v", plainList.Runs)
	}
	plainDetailRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-alice/attempts", "test-token")
	if plainDetailRR.Code != http.StatusOK {
		t.Fatalf("plain token detail status = %d, body = %s", plainDetailRR.Code, plainDetailRR.Body.String())
	}
}

func TestTaskRunAnalyticsAPIAccessControlAggregates(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Auth: &types.AuthConfig{
			SessionSecret: "analytics-session-secret",
			Access:        &types.AccessConfig{ViewRequiresTags: []string{"owner={user}"}},
		},
	}, "", "", "")
	ts := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-alice", AttemptID: "attempt-alice", ClawID: "claw-alice", TenantID: "test-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "alice-space", Factory: "alice-factory", Integration: "linear", Repo: "alice/repo", Model: "alice-model",
		FailureType: taskRunFailureTimeout, StartedAt: ts + 100, HumanInteractions: 5, PRCount: 3, OpenPRCount: 3,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-bob", AttemptID: "attempt-bob", ClawID: "claw-bob", TenantID: "test-tenant-id",
		Status: taskRunStatusHumanInTheLoop, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "bob-space", Factory: "bob-factory", Integration: "github", Repo: "bob/repo", Model: "bob-model",
		WarningTypes: []string{taskRunWarningHumanPRComment}, StartedAt: ts, HumanInteractions: 1,
		PRCount: 1, MergedPRCount: 1, MergedAt: ts + 500, FinishedAt: ts + 500,
	})
	for _, claw := range []struct{ id, tags string }{
		{id: "claw-alice", tags: `["owner=alice"]`},
		{id: "claw-bob", tags: `["owner=bob"]`},
	} {
		if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`, claw.id, "test-tenant-id", claw.id, claw.tags); err != nil {
			t.Fatalf("insert claw: %v", err)
		}
	}
	bobSession, err := signGitHubSession("analytics-session-secret", "bob", "", "")
	if err != nil {
		t.Fatal(err)
	}

	summaryRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/summary", bobSession)
	var summary taskRunAnalyticsSummaryResponse
	decodeTaskRunAnalyticsAPI(t, summaryRR, &summary)
	if summary.TotalRuns != 1 || summary.HumanInteractions != 1 {
		t.Fatalf("OAuth summary was not ACL-filtered: %#v", summary)
	}
	if summary.ByStatus[taskRunStatusHumanInTheLoop] != 1 || summary.ByStatus[taskRunStatusFailed] != 0 || len(summary.FailureBreakdown) != 0 {
		t.Fatalf("OAuth summary leaked hidden run breakdowns: %#v", summary)
	}
	if summary.WarningBreakdown[taskRunWarningHumanPRComment] != 1 {
		t.Fatalf("OAuth summary missing viewable warning counts: %#v", summary)
	}
	if summary.PRCounts.Total != 1 || summary.PRCounts.Open != 0 || summary.PRCounts.Merged != 1 || summary.PRCounts.Closed != 0 {
		t.Fatalf("OAuth summary PR counts leaked hidden runs: %#v", summary.PRCounts)
	}

	optionsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/filter-options", bobSession)
	var options taskRunAnalyticsFilterOptionsResponse
	decodeTaskRunAnalyticsAPI(t, optionsRR, &options)
	assertStringSliceEqual(t, options.Workspaces, []string{"bob-space"})
	assertStringSliceEqual(t, options.Factories, []string{"bob-factory"})
	assertStringSliceEqual(t, options.Integrations, []string{"github"})
	assertStringSliceEqual(t, options.Repos, []string{"bob/repo"})
	assertStringSliceEqual(t, options.Models, []string{"bob-model"})
	assertStringSliceEqual(t, options.Statuses, []string{taskRunStatusHumanInTheLoop})
	assertStringSliceEqual(t, options.WarningTypes, []string{taskRunWarningHumanPRComment})
	assertStringSliceEqual(t, options.FailureTypes, []string{})
	if strings.Contains(optionsRR.Body.String(), "alice") {
		t.Fatalf("filter-options leaked hidden run values: %s", optionsRR.Body.String())
	}

	plainSummaryRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/summary", "test-token")
	var plainSummary taskRunAnalyticsSummaryResponse
	decodeTaskRunAnalyticsAPI(t, plainSummaryRR, &plainSummary)
	if plainSummary.TotalRuns != 2 || plainSummary.FailureBreakdown[taskRunFailureTimeout] != 1 {
		t.Fatalf("plain token summary should be tenant-wide: %#v", plainSummary)
	}
	plainOptionsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/filter-options", "test-token")
	var plainOptions taskRunAnalyticsFilterOptionsResponse
	decodeTaskRunAnalyticsAPI(t, plainOptionsRR, &plainOptions)
	assertStringSliceEqual(t, plainOptions.Workspaces, []string{"alice-space", "bob-space"})

	if _, err := db.Exec(`DELETE FROM claws WHERE id=?`, "claw-bob"); err != nil {
		t.Fatalf("delete claw: %v", err)
	}
	missingClawSummaryRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/summary", bobSession)
	var missingClawSummary taskRunAnalyticsSummaryResponse
	decodeTaskRunAnalyticsAPI(t, missingClawSummaryRR, &missingClawSummary)
	if missingClawSummary.TotalRuns != 0 || len(missingClawSummary.ByStatus) != 0 {
		t.Fatalf("OAuth summary should exclude runs with missing claws when ACLs are configured: %#v", missingClawSummary)
	}
	missingClawOptionsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/filter-options", bobSession)
	var missingClawOptions taskRunAnalyticsFilterOptionsResponse
	decodeTaskRunAnalyticsAPI(t, missingClawOptionsRR, &missingClawOptions)
	assertStringSliceEqual(t, missingClawOptions.Workspaces, []string{})
	assertStringSliceEqual(t, missingClawOptions.Statuses, []string{})
}

func TestTaskRunAnalyticsAPIAllowsMissingClawsWithoutViewRestrictions(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "test-token",
		Auth:  &types.AuthConfig{SessionSecret: "analytics-session-secret"},
	}, "", "", "")
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-deleted-claw", AttemptID: "attempt-deleted-claw", ClawID: "claw-deleted", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, OwnerType: taskRunOwnerFactory, Factory: "bugfix", StartedAt: 1760000000000,
	})
	session, err := signGitHubSession("analytics-session-secret", "alice", "", "")
	if err != nil {
		t.Fatal(err)
	}

	listRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", session)
	var list taskRunAnalyticsRunsResponse
	decodeTaskRunAnalyticsAPI(t, listRR, &list)
	if len(list.Runs) != 1 || list.Runs[0].RunID != "run-deleted-claw" {
		t.Fatalf("OAuth list should include persisted run without claw when ACLs are unrestricted: %#v", list.Runs)
	}

	detailRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs/run-deleted-claw", session)
	if detailRR.Code != http.StatusOK {
		t.Fatalf("OAuth detail without configured ACL status = %d, body = %s", detailRR.Code, detailRR.Body.String())
	}

	summaryRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/summary", session)
	var summary taskRunAnalyticsSummaryResponse
	decodeTaskRunAnalyticsAPI(t, summaryRR, &summary)
	if summary.TotalRuns != 1 {
		t.Fatalf("OAuth summary without configured ACL should be tenant-wide: %#v", summary)
	}
	optionsRR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/filter-options", session)
	var options taskRunAnalyticsFilterOptionsResponse
	decodeTaskRunAnalyticsAPI(t, optionsRR, &options)
	assertStringSliceEqual(t, options.Factories, []string{"bugfix"})
}

func TestTaskRunAnalyticsAPIFilterOptionsAreTenantScoped(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	ts := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-options-a", AttemptID: "attempt-options-a", ClawID: "claw-options-a", TenantID: "test-tenant-id",
		Status: taskRunStatusHumanInTheLoop, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "eng", Factory: "bugfix", Integration: "github", Repo: "elastic/claw", Model: "gpt-5",
		WarningTypes: []string{taskRunWarningHumanDashboardMessage}, StartedAt: ts,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-options-b", AttemptID: "attempt-options-b", ClawID: "claw-options-b", TenantID: "test-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow,
		Workspace: "platform", Workflow: "review", Integration: "linear", Repo: "elastic/api", Model: "gpt-4.1",
		FailureType: taskRunFailureManualStopDelivery, StartedAt: ts + 1,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-options-hidden", AttemptID: "attempt-options-hidden", ClawID: "claw-options-hidden", TenantID: "other-tenant-id",
		Status: taskRunStatusFailed, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerFactory,
		Workspace: "secret", Factory: "hidden", Integration: "github", Repo: "secret/repo", Model: "hidden",
		FailureType: taskRunFailureTimeout, StartedAt: ts,
	})
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-options-non-pr", AttemptID: "attempt-options-non-pr", ClawID: "claw-options-non-pr", TenantID: "test-tenant-id",
		Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow,
		Workspace: "ignored", Workflow: "ignored", Integration: "external", Repo: "ignored/repo", Model: "ignored-model",
		StartedAt: ts + 2, AnalyticsEnabled: boolPtr(false), RequiresPR: boolPtr(false), ExcludedReason: "not_pr_task",
	})

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/filter-options", "test-token")
	var options taskRunAnalyticsFilterOptionsResponse
	decodeTaskRunAnalyticsAPI(t, rr, &options)
	assertStringSliceEqual(t, options.Workspaces, []string{"eng", "platform"})
	assertStringSliceEqual(t, options.Workflows, []string{"review"})
	assertStringSliceEqual(t, options.Factories, []string{"bugfix"})
	assertStringSliceEqual(t, options.Integrations, []string{"github", "linear"})
	assertStringSliceEqual(t, options.Repos, []string{"elastic/api", "elastic/claw"})
	assertStringSliceEqual(t, options.Models, []string{"gpt-4.1", "gpt-5"})
	assertStringSliceEqual(t, options.Statuses, []string{taskRunStatusFailed, taskRunStatusHumanInTheLoop})
	assertStringSliceEqual(t, options.WarningTypes, []string{taskRunWarningHumanDashboardMessage})
	assertStringSliceEqual(t, options.FailureTypes, []string{taskRunFailureManualStopDelivery})
}

type apiRunFixture struct {
	RunID             string
	AttemptID         string
	ClawID            string
	TenantID          string
	Status            string
	Phase             string
	OwnerType         string
	Workspace         string
	Workflow          string
	Factory           string
	Integration       string
	Repo              string
	Model             string
	WarningTypes      []string
	FailureType       string
	StartedAt         int64
	IssueCreatedAt    int64
	MergedAt          int64
	FinishedAt        int64
	HumanInteractions int
	PRCount           int
	OpenPRCount       int
	MergedPRCount     int
	ClosedPRCount     int
	AnalyticsEnabled  *bool
	RequiresPR        *bool
	ExcludedReason    string
	InputTokens       int64
	OutputTokens      int64
	TotalTokens       int64
	EstimatedCostUsd  float64
	UsageUpdatedAt    int64
	IssueTitle        string
}

func newTaskRunAnalyticsAPITestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	_, err := db.Exec(
		`INSERT OR IGNORE INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,datetime('now'))`,
		"other-tenant-id", "other", "other-token", "other-claw-token",
	)
	if err != nil {
		t.Fatalf("insert other tenant: %v", err)
	}
	return s, db
}

func insertTaskRunAnalyticsAPIRun(t *testing.T, db *sql.DB, fixture apiRunFixture) {
	t.Helper()
	if fixture.Status == "" {
		fixture.Status = taskRunStatusRunning
	}
	if fixture.Phase == "" {
		fixture.Phase = taskRunPhaseAgentRunning
	}
	warningsJSON := "[]"
	if len(fixture.WarningTypes) > 0 {
		b, err := json.Marshal(fixture.WarningTypes)
		if err != nil {
			t.Fatalf("marshal warning types: %v", err)
		}
		warningsJSON = string(b)
	}
	analyticsEnabled := true
	if fixture.AnalyticsEnabled != nil {
		analyticsEnabled = *fixture.AnalyticsEnabled
	}
	requiresPR := true
	if fixture.RequiresPR != nil {
		requiresPR = *fixture.RequiresPR
	}
	_, err := db.Exec(`
		INSERT INTO task_runs(
			id, tenant_id, initial_attempt_id, current_attempt_id, run_kind, owner_type,
			workspace_name, workflow_name, factory_name, owner_display_name, integration,
			issue_id, issue_created_at, claw_id, model, tags, analytics_enabled, requires_pr, excluded_reason, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		fixture.RunID, fixture.TenantID, fixture.AttemptID, fixture.AttemptID, taskRunKindPRTask, fixture.OwnerType,
		fixture.Workspace, fixture.Workflow, fixture.Factory, firstNonEmpty(fixture.Workflow, fixture.Factory),
		fixture.Integration, "ISSUE-"+fixture.RunID, fixture.IssueCreatedAt, fixture.ClawID, fixture.Model, "[]", boolInt(analyticsEnabled), boolInt(requiresPR), fixture.ExcludedReason,
		fixture.StartedAt, fixture.StartedAt,
	)
	if err != nil {
		t.Fatalf("insert task run %s: %v", fixture.RunID, err)
	}
	attemptStatus := "running"
	if fixture.Status == taskRunStatusFailed {
		attemptStatus = "failed"
	} else if fixture.Status == taskRunStatusClean || fixture.Status == taskRunStatusHumanInTheLoop {
		attemptStatus = "succeeded"
	}
	_, err = db.Exec(`
		INSERT INTO task_run_attempts(
			id, tenant_id, run_id, attempt_id, attempt_number, claw_id, status, failure_type,
			started_at, finished_at, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		fixture.AttemptID, fixture.TenantID, fixture.RunID, fixture.AttemptID, 1, fixture.ClawID,
		attemptStatus, fixture.FailureType, fixture.StartedAt, fixture.FinishedAt, fixture.StartedAt, fixture.StartedAt,
	)
	if err != nil {
		t.Fatalf("insert task run attempt %s: %v", fixture.AttemptID, err)
	}
	_, err = db.Exec(`
		INSERT INTO task_run_summaries(
			id, tenant_id, run_id, initial_attempt_id, current_attempt_id, status, phase, attempt_count,
			owner_type, workspace_name, workflow_name, factory_name, owner_display_name, run_kind,
			integration, issue_id, issue_created_at, claw_id, model, repo, primary_pr_url, pr_count, open_pr_count,
			merged_pr_count, closed_pr_count, warning_types, failure_type, human_interaction_count,
			started_at, merged_at, finished_at, last_event_at, materialized_at, updated_at,
		analytics_enabled, requires_pr, excluded_reason, input_tokens, output_tokens, total_tokens,
			estimated_cost_usd, usage_updated_at, issue_title
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"summary-"+fixture.RunID, fixture.TenantID, fixture.RunID, fixture.AttemptID, fixture.AttemptID,
		fixture.Status, fixture.Phase, 1, fixture.OwnerType, fixture.Workspace, fixture.Workflow, fixture.Factory,
		firstNonEmpty(fixture.Workflow, fixture.Factory), taskRunKindPRTask, fixture.Integration,
		"ISSUE-"+fixture.RunID, fixture.IssueCreatedAt, fixture.ClawID, fixture.Model, fixture.Repo,
		"https://github.com/"+fixture.Repo+"/pull/1", fixture.PRCount, fixture.OpenPRCount,
		fixture.MergedPRCount, fixture.ClosedPRCount, warningsJSON, fixture.FailureType,
		fixture.HumanInteractions, fixture.StartedAt, fixture.MergedAt, fixture.FinishedAt,
		fixture.StartedAt, fixture.StartedAt, fixture.StartedAt, boolInt(analyticsEnabled), boolInt(requiresPR), fixture.ExcludedReason,
		fixture.InputTokens, fixture.OutputTokens, fixture.TotalTokens, fixture.EstimatedCostUsd, fixture.UsageUpdatedAt, fixture.IssueTitle,
	)
	if err != nil {
		t.Fatalf("insert task run summary %s: %v", fixture.RunID, err)
	}
	_, err = db.Exec(`
		INSERT INTO task_run_events(
			id, tenant_id, run_id, attempt_id, event_key, source, event_type, event_time, observed_at,
			actor_type, actor_login, interaction_role, target_type, target_id, target_url,
			warning_type, failure_type, detail, created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"event-start-"+fixture.RunID, fixture.TenantID, fixture.RunID, fixture.AttemptID, "start-"+fixture.RunID,
		taskRunSourceHub, taskRunEventTaskStart, fixture.StartedAt, fixture.StartedAt, taskRunActorSystem,
		"", taskRunInteractionAllowedStart, "claw", fixture.ClawID, "", "", "", "{}", fixture.StartedAt,
	)
	if err != nil {
		t.Fatalf("insert start event %s: %v", fixture.RunID, err)
	}
	if len(fixture.WarningTypes) > 0 {
		_, err = db.Exec(`
			INSERT INTO task_run_events(
				id, tenant_id, run_id, attempt_id, event_key, source, event_type, event_time, observed_at,
				actor_type, actor_login, interaction_role, target_type, target_id, target_url,
				warning_type, detail, created_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			"event-warning-"+fixture.RunID, fixture.TenantID, fixture.RunID, fixture.AttemptID, "warning-"+fixture.RunID,
			taskRunSourceGitHub, warningEventTypeForTest(fixture.WarningTypes[0]), fixture.StartedAt+100,
			fixture.StartedAt+100, taskRunActorHuman, "octocat", taskRunInteractionWarning,
			"pull_request", "1", "https://github.com/"+fixture.Repo+"/pull/1",
			fixture.WarningTypes[0], `{"path":"pkg/hub/server.go"}`, fixture.StartedAt+100,
		)
		if err != nil {
			t.Fatalf("insert warning event %s: %v", fixture.RunID, err)
		}
	}
	if fixture.PRCount > 0 {
		_, err = db.Exec(`
			INSERT INTO task_run_prs(
				id, tenant_id, run_id, repo, pr_number, pr_url, head_sha, head_branch,
				last_agent_head_sha, base_branch, state, merged, opened_at, closed_at, merged_at,
				merged_by_login, created_at, updated_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			"pr-"+fixture.RunID, fixture.TenantID, fixture.RunID, fixture.Repo, 1,
			"https://github.com/"+fixture.Repo+"/pull/1", "head-"+fixture.RunID, "feature/"+fixture.RunID,
			"head-"+fixture.RunID, "main", prStateForFixture(fixture), boolInt(fixture.MergedPRCount > 0),
			fixture.StartedAt+200, fixture.FinishedAt, fixture.MergedAt, "maintainer", fixture.StartedAt, fixture.StartedAt,
		)
		if err != nil {
			t.Fatalf("insert task run pr %s: %v", fixture.RunID, err)
		}
	}
}

func requestTaskRunAnalyticsAPI(t *testing.T, s *Server, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func decodeTaskRunAnalyticsAPI(t *testing.T, rr *httptest.ResponseRecorder, out any) {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
}

func assertStringSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("set length mismatch: got %#v want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("set mismatch: got %#v want %#v", got, want)
		}
	}
}

func boolPtr(value bool) *bool {
	return &value
}

func warningEventTypeForTest(warningType string) string {
	switch warningType {
	case taskRunWarningHumanReviewComment:
		return taskRunEventHumanReviewComment
	case taskRunWarningHumanDashboardMessage:
		return taskRunEventHumanDashboardMessage
	default:
		return taskRunEventHumanPRComment
	}
}

func prStateForFixture(fixture apiRunFixture) string {
	if fixture.MergedPRCount > 0 || fixture.ClosedPRCount > 0 {
		return taskRunPRStateClosed
	}
	if fixture.OpenPRCount > 0 {
		return taskRunPRStateOpen
	}
	return taskRunPRStateUnknown
}
