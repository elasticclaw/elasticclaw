package hub

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestTaskRunAnalyticsTicketPageQueriesUseTicketPageIndex(t *testing.T) {
	_, db := newTaskRunAnalyticsAPITestServer(t)
	filters := taskRunAnalyticsFilters{TenantID: "test-tenant-id"}
	where, args := taskRunAnalyticsSummaryWhere(filters)
	orderAt := `MAX(COALESCE(NULLIF(MIN(NULLIF(issue_created_at,0)),0), MIN(started_at), 1), 1)`
	queries := []string{
		`SELECT COUNT(DISTINCT issue_id) FROM task_run_summaries ` + where + ` AND issue_id != ''`,
		`SELECT issue_id, ` + orderAt + ` FROM task_run_summaries ` + where + ` AND issue_id != '' GROUP BY issue_id ORDER BY 2 DESC, issue_id DESC LIMIT ?`,
	}
	for _, query := range queries {
		queryArgs := append([]any{}, args...)
		if strings.Contains(query, "LIMIT ?") {
			queryArgs = append(queryArgs, 2)
		}
		rows, err := db.Query(`EXPLAIN QUERY PLAN `+query, queryArgs...)
		if err != nil {
			t.Fatalf("explain ticket page query: %v", err)
		}
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatalf("scan ticket page plan: %v", err)
			}
			details = append(details, detail)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("read ticket page plan: %v", err)
		}
		rows.Close()
		if !strings.Contains(strings.Join(details, "\n"), "idx_task_run_summaries_ticket_page") {
			t.Fatalf("ticket page query did not use idx_task_run_summaries_ticket_page: %s", strings.Join(details, "; "))
		}
	}
}

func TestDeriveTaskRunAnalyticsTicketStatusFixtureTickets(t *testing.T) {
	tests := []struct {
		issueID string
		runs    []taskRunAnalyticsTicketRunSummary
		prs     []taskRunAnalyticsTicketPRView
		want    string
	}{
		{issueID: "ADV-812", runs: []taskRunAnalyticsTicketRunSummary{{RunID: "run_8f21c4", Status: "clean"}, {RunID: "run_c41d90", Status: "human_in_the_loop"}, {RunID: "run_6b21f8", Status: "warning"}, {RunID: "run_3c05a1", Status: "failed"}}, prs: []taskRunAnalyticsTicketPRView{{taskRunAnalyticsPRView: taskRunAnalyticsPRView{ID: "pr1", State: "closed", Merged: true}, RunID: "run_8f21c4"}, {taskRunAnalyticsPRView: taskRunAnalyticsPRView{ID: "pr8", State: "closed"}, RunID: "run_c41d90"}, {taskRunAnalyticsPRView: taskRunAnalyticsPRView{ID: "pr9", State: "closed"}, RunID: "run_6b21f8"}}, want: "delivered"},
		{issueID: "PLT-31", runs: []taskRunAnalyticsTicketRunSummary{{RunID: "run_4a12bd", Status: "clean"}, {RunID: "run_d90c33", Status: "failed"}}, prs: []taskRunAnalyticsTicketPRView{{taskRunAnalyticsPRView: taskRunAnalyticsPRView{ID: "pr6", State: "open"}, RunID: "run_4a12bd"}}, want: "pr_open"},
		{issueID: "SUP-201", runs: []taskRunAnalyticsTicketRunSummary{{RunID: "run_5c33bb", Status: "failed"}, {RunID: "run_e77b02", Status: "failed"}}, want: "failed"},
		{issueID: "ADV-806", runs: []taskRunAnalyticsTicketRunSummary{{RunID: "run_1b77c0", Status: "running"}}, want: "in_progress"},
	}
	for _, test := range tests {
		t.Run(test.issueID, func(t *testing.T) {
			if got := deriveTaskRunAnalyticsTicketStatus(test.runs, test.prs); got != test.want {
				t.Fatalf("deriveTaskRunAnalyticsTicketStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCollapseTaskRunAnalyticsTicketStory(t *testing.T) {
	story := collapseTaskRunAnalyticsTicketStory([]taskRunAnalyticsTicketStoryEntry{
		{ID: "e1", EventType: "run_queued", Label: taskRunAnalyticsStoryLabels["run_queued"], Time: 1, Count: 1},
		{ID: "e2", EventType: "attempt_retried", Label: taskRunAnalyticsStoryLabels["attempt_retried"], Time: 2, Count: 1},
		{ID: "e3", EventType: "attempt_retried", Label: taskRunAnalyticsStoryLabels["attempt_retried"], Time: 3, Count: 1},
		{ID: "e4", EventType: "ci_failed", Time: 4, Count: 1},
	})
	if len(story) != 2 {
		t.Fatalf("story entries = %d, want 2: %#v", len(story), story)
	}
	if story[1].EventType != "attempt_retried" || story[1].Count != 2 {
		t.Fatalf("retry entry = %#v, want one collapsed retry with count 2", story[1])
	}
}

func TestTaskRunAnalyticsTicketStoryUsesHubNativeEventTypes(t *testing.T) {
	tests := []struct {
		eventType string
		label     string
		kind      string
	}{
		{eventType: "human_review_comment", label: "Human reviewed", kind: "human"},
		{eventType: "human_dashboard_message", label: "Human stepped in", kind: "human"},
		{eventType: "pr_closed_unmerged", label: "Attempt discarded", kind: "bad"},
	}
	for _, test := range tests {
		t.Run(test.eventType, func(t *testing.T) {
			entry := ticketStoryEntry(taskRunAnalyticsEventView{EventType: test.eventType}, "run-1")
			if entry.Label != test.label || entry.Kind != test.kind {
				t.Fatalf("ticketStoryEntry(%q) = %#v", test.eventType, entry)
			}
		})
	}
}

func TestTaskRunAnalyticsTicketLeadTimeWithoutEvents(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	const reportedAt = int64(1_000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "eventless-running", AttemptID: "attempt-eventless", ClawID: "claw-eventless", TenantID: "test-tenant-id",
		Status: taskRunStatusRunning, Phase: taskRunPhaseAgentRunning, OwnerType: taskRunOwnerWorkflow,
		Workspace: "eng", Workflow: "tickets", Integration: "external", Repo: "elastic/claw",
		StartedAt: 2_000, IssueCreatedAt: reportedAt, IssueTitle: "Eventless ticket",
	})
	if _, err := db.Exec("DELETE FROM task_run_events WHERE run_id=?", "eventless-running"); err != nil {
		t.Fatalf("delete fixture events: %v", err)
	}
	for _, table := range []string{"task_runs", "task_run_summaries"} {
		if _, err := db.Exec("UPDATE "+table+" SET issue_id=? WHERE "+map[string]string{"task_runs": "id", "task_run_summaries": "run_id"}[table]+"=?", "EVENTLESS-1", "eventless-running"); err != nil {
			t.Fatalf("set issue ID for %s: %v", table, err)
		}
	}

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/tickets", "test-token")
	if rr.Code != http.StatusOK {
		t.Fatalf("tickets status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var response taskRunAnalyticsTicketsResponse
	decodeTaskRunAnalyticsAPI(t, rr, &response)
	if len(response.Tickets) != 1 || response.Tickets[0].LeadTime != 0 {
		t.Fatalf("eventless ticket lead time mismatch: %#v", response.Tickets)
	}
}

func TestTaskRunAnalyticsTicketsHandlerAggregatesAndPaginates(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	const reportedAt = int64(1_000)
	insert := func(runID, issueID string, startedAt, issueCreatedAt int64, cost float64, tokens int64, merged, open int) {
		insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
			RunID: runID, AttemptID: "attempt-" + runID, ClawID: "claw-" + runID, TenantID: "test-tenant-id",
			Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow,
			Workspace: "eng", Workflow: "tickets", Integration: "external", Repo: "elastic/claw",
			StartedAt: startedAt, IssueCreatedAt: issueCreatedAt, FinishedAt: startedAt + 500,
			HumanInteractions: 1, PRCount: merged + open, MergedPRCount: merged, OpenPRCount: open,
			EstimatedCostUsd: cost, TotalTokens: tokens, IssueTitle: issueID, UsageUpdatedAt: startedAt + 500,
		})
		for _, table := range []string{"task_runs", "task_run_summaries"} {
			if _, err := db.Exec("UPDATE "+table+" SET issue_id=? WHERE "+map[string]string{"task_runs": "id", "task_run_summaries": "run_id"}[table]+"=?", issueID, runID); err != nil {
				t.Fatalf("set issue ID for %s: %v", runID, err)
			}
		}
	}
	insert("ticket-one-a", "TICKET-1", 2_000, reportedAt, 1.25, 100, 1, 0)
	insert("ticket-one-b", "TICKET-1", 3_000, reportedAt, 2.75, 200, 0, 1)
	insert("ticket-two", "TICKET-2", 4_000, 5_000, 3, 300, 0, 0)
	insert("ticket-three", "TICKET-3", 5_000, 6_000, 4, 400, 0, 0)
	if _, err := db.Exec(`INSERT INTO task_run_events(id, tenant_id, run_id, event_key, source, event_type, event_time, observed_at, created_at)
		VALUES('merged-ticket-one', 'test-tenant-id', 'ticket-one-a', 'merged-ticket-one', 'github', 'pr_merged', 7_000, 7_000, 7_000)`); err != nil {
		t.Fatalf("insert merged event: %v", err)
	}

	page1RR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/tickets?limit=2", "test-token")
	if page1RR.Code != http.StatusOK {
		t.Fatalf("tickets page 1 status = %d, body = %s", page1RR.Code, page1RR.Body.String())
	}
	var page1 taskRunAnalyticsTicketsResponse
	decodeTaskRunAnalyticsAPI(t, page1RR, &page1)
	if len(page1.Tickets) != 2 || page1.Tickets[0].IssueID != "TICKET-3" || page1.Tickets[1].IssueID != "TICKET-2" || page1.NextCursor == "" {
		t.Fatalf("unexpected first ticket page: %#v", page1)
	}

	page2RR := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/tickets?limit=2&cursor="+page1.NextCursor, "test-token")
	var page2 taskRunAnalyticsTicketsResponse
	decodeTaskRunAnalyticsAPI(t, page2RR, &page2)
	if len(page2.Tickets) != 1 || page2.NextCursor != "" {
		t.Fatalf("unexpected second ticket page: %#v", page2)
	}
	ticket := page2.Tickets[0]
	if ticket.IssueID != "TICKET-1" || ticket.Cost != 4 || ticket.TotalTokens != 300 || ticket.HumanTouches != 2 || ticket.AttemptCount != 2 || ticket.RunCount != 2 {
		t.Fatalf("ticket aggregation mismatch: %#v", ticket)
	}
	if ticket.MergedPRCount != 1 || ticket.OpenPRCount != 1 {
		t.Fatalf("ticket PR counts mismatch: %#v", ticket)
	}
	if ticket.ReportedAt != reportedAt || ticket.TimeToFirstRun != 1_000 || ticket.LeadTime != 6_000 {
		t.Fatalf("ticket timing mismatch: %#v", ticket)
	}
}

func TestTaskRunAnalyticsTicketsHandlerHonorsMultiValueDimensionFilters(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	for _, fixture := range []apiRunFixture{
		{RunID: "ticket-model-a", AttemptID: "attempt-ticket-model-a", ClawID: "claw-ticket-model-a", TenantID: "test-tenant-id", Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow, Workflow: "alpha", Model: "x", StartedAt: 3_000, IssueTitle: "A"},
		{RunID: "ticket-model-b", AttemptID: "attempt-ticket-model-b", ClawID: "claw-ticket-model-b", TenantID: "test-tenant-id", Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow, Workflow: "beta", Model: "y", StartedAt: 2_000, IssueTitle: "B"},
		{RunID: "ticket-other", AttemptID: "attempt-ticket-other", ClawID: "claw-ticket-other", TenantID: "test-tenant-id", Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow, Workflow: "gamma", Model: "z", StartedAt: 1_000, IssueTitle: "Other"},
	} {
		insertTaskRunAnalyticsAPIRun(t, db, fixture)
	}

	for _, test := range []struct {
		name  string
		query string
		want  int
	}{
		{name: "one dimension returns matching ticket union", query: "model=x,y", want: 2},
		{name: "dimensions combine with and semantics", query: "workflow=alpha,beta&model=x", want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/tickets?"+test.query, "test-token")
			if rr.Code != http.StatusOK {
				t.Fatalf("tickets status = %d, body = %s", rr.Code, rr.Body.String())
			}
			var response taskRunAnalyticsTicketsResponse
			decodeTaskRunAnalyticsAPI(t, rr, &response)
			if response.Total != test.want || len(response.Tickets) != test.want {
				t.Fatalf("tickets = %#v, want %d", response, test.want)
			}
		})
	}
}

// Regression test for finding #16: a fresh cached ticket_metadata row with a
// zero reported_at (e.g. read before the tracker backfill lands) must not
// clobber a non-zero, run-derived ReportedAt already set on the ticket.
func TestApplyOrScheduleTaskRunAnalyticsTicketMetadataKeepsNonZeroReportedAt(t *testing.T) {
	s, _ := newTaskRunAnalyticsAPITestServer(t)
	ticket := &taskRunAnalyticsTicketView{IssueID: "TICKET-KEEP", ReportedAt: 12_345}
	metadata := taskRunAnalyticsTicketMetadata{updatedAt: time.Now().UnixMilli(), reportedAt: 0}
	s.applyOrScheduleTaskRunAnalyticsTicketMetadata("test-tenant-id", ticket, taskRunAnalyticsRunView{}, metadata)
	if ticket.ReportedAt != 12_345 {
		t.Fatalf("ReportedAt = %d, want run-derived value 12345 preserved", ticket.ReportedAt)
	}
}

func TestApplyOrScheduleTaskRunAnalyticsTicketMetadataKeepsCachedValuesAfterEnrichmentFailure(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	const issueID = "TICKET-ENRICHMENT-FAILURE"
	const updatedAt = int64(123456)
	if _, err := db.Exec(`INSERT INTO ticket_metadata(tenant_id,issue_id,requester,requester_role,team,priority,ask,reported_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, "test-tenant-id", issueID, "requester", "manager", "platform", "high", "fix it", 999, updatedAt); err != nil {
		t.Fatalf("insert cached metadata: %v", err)
	}

	s.applyOrScheduleTaskRunAnalyticsTicketMetadata("test-tenant-id", &taskRunAnalyticsTicketView{IssueID: issueID}, taskRunAnalyticsRunView{}, taskRunAnalyticsTicketMetadata{})
	key := "test-tenant-id:" + issueID
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, inflight := ticketMetadataInflight.Load(key); !inflight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ticket metadata enrichment did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	var requester, role, team, priority, ask string
	var reportedAt, gotUpdatedAt int64
	if err := db.QueryRow(`SELECT requester,requester_role,team,priority,ask,reported_at,updated_at FROM ticket_metadata WHERE tenant_id=? AND issue_id=?`, "test-tenant-id", issueID).Scan(&requester, &role, &team, &priority, &ask, &reportedAt, &gotUpdatedAt); err != nil {
		t.Fatalf("read cached metadata: %v", err)
	}
	if requester != "requester" || role != "manager" || team != "platform" || priority != "high" || ask != "fix it" || reportedAt != 999 || gotUpdatedAt != updatedAt {
		t.Fatalf("metadata after failed enrichment = %q/%q/%q/%q/%q/%d/%d, want unchanged", requester, role, team, priority, ask, reportedAt, gotUpdatedAt)
	}
}

// Regression test for finding #3: both the hydration and handler paths must
// reject an empty run group before buildTaskRunAnalyticsTicket reaches runs[0].
func TestTaskRunAnalyticsTicketsHandlerSkipsEmptyGroups(t *testing.T) {
	// This is the id/order-to-hydration handoff: the ordered ID list includes
	// TICKET-EMPTY, while hydration returns no run for it.
	groups := taskRunAnalyticsTicketGroupsFromHydration(
		[]string{"TICKET-EMPTY", "TICKET-POPULATED"},
		[]taskRunAnalyticsRunView{{RunID: "run-populated", IssueID: "TICKET-POPULATED"}},
	)
	if len(groups) != 1 || groups[0].issueID != "TICKET-POPULATED" {
		t.Fatalf("hydrated groups = %#v, want only populated group", groups)
	}

	empty := taskRunAnalyticsTicketGroup{issueID: "TICKET-EMPTY"}
	populated := taskRunAnalyticsTicketGroup{issueID: "TICKET-POPULATED", runs: []taskRunAnalyticsRunView{{RunID: "run-populated"}}}
	if groups := nonEmptyTaskRunAnalyticsTicketGroups([]taskRunAnalyticsTicketGroup{empty, populated}); len(groups) != 1 || groups[0].issueID != populated.issueID {
		t.Fatalf("handler groups = %#v, want only populated group", groups)
	}
	if group, ok := taskRunAnalyticsTicketGroupWithRuns(empty.issueID, nil, 0); ok || group.issueID != "" {
		t.Fatalf("empty hydration group = %#v, %t; want rejected", group, ok)
	}
}

func TestTaskRunAnalyticsTicketsHandlerUsesConstantQueriesPerPage(t *testing.T) {
	countQueries := func(ticketCount int) int64 {
		t.Helper()
		s, db, count := newCountingTaskRunAnalyticsAPITestServer(t)
		for i := 0; i < ticketCount; i++ {
			runID := fmt.Sprintf("query-count-run-%02d", i)
			issueID := fmt.Sprintf("QUERY-COUNT-%02d", i)
			insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
				RunID: runID, AttemptID: "attempt-" + runID, ClawID: "claw-" + runID, TenantID: "test-tenant-id",
				Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow,
				Workspace: "eng", Workflow: "tickets", Integration: "external", StartedAt: int64(10_000 + i), IssueTitle: issueID,
			})
			if _, err := db.Exec(`UPDATE task_run_summaries SET issue_id=? WHERE run_id=?`, issueID, runID); err != nil {
				t.Fatalf("set issue ID: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO ticket_metadata(tenant_id,issue_id,updated_at) VALUES(?,?,?)`, "test-tenant-id", issueID, time.Now().UnixMilli()); err != nil {
				t.Fatalf("seed ticket metadata: %v", err)
			}
		}
		count.Store(0)
		rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/tickets?limit=50", "test-token")
		if rr.Code != http.StatusOK {
			t.Fatalf("tickets status = %d, body = %s", rr.Code, rr.Body.String())
		}
		return count.Load()
	}

	three := countQueries(3)
	fifteen := countQueries(15)
	if three != fifteen {
		t.Fatalf("ticket page queries grew with ticket count: 3 tickets = %d, 15 tickets = %d", three, fifteen)
	}
}

var taskRunAnalyticsCountingDriverSequence atomic.Uint64

type taskRunAnalyticsCountingDriver struct{ count *atomic.Int64 }

func (d taskRunAnalyticsCountingDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return taskRunAnalyticsCountingConn{Conn: conn, count: d.count}, nil
}

type taskRunAnalyticsCountingConn struct {
	driver.Conn
	count *atomic.Int64
}

func (c taskRunAnalyticsCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.count.Add(1)
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		return queryer.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c taskRunAnalyticsCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.count.Add(1)
	if executer, ok := c.Conn.(driver.ExecerContext); ok {
		return executer.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func newCountingTaskRunAnalyticsAPITestServer(t *testing.T) (*Server, *sql.DB, *atomic.Int64) {
	t.Helper()
	s, _ := newTaskRunAnalyticsAPITestServer(t)
	count := &atomic.Int64{}
	driverName := fmt.Sprintf("task-run-analytics-counting-%d", taskRunAnalyticsCountingDriverSequence.Add(1))
	sql.Register(driverName, taskRunAnalyticsCountingDriver{count: count})
	db, err := sql.Open(driverName, ":memory:?_time_format=sqlite&_txlock=immediate&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open counting database: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate counting database: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,datetime('now'))`, "test-tenant-id", "test", "test-token", ""); err != nil {
		db.Close()
		t.Fatalf("seed counting tenant: %v", err)
	}
	s.db = db
	t.Cleanup(func() { db.Close() })
	return s, db, count
}
