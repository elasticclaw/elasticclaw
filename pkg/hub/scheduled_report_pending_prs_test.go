package hub

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPendingPRsScheduledReport(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	nowAt := time.Now()
	insertPendingPRReportRun(t, db, "first", "ENG-1", "eng", "Older title", nowAt.Add(-72*time.Hour), 1)
	insertPendingPRReportRun(t, db, "second", "ENG-1", "eng", "Latest title", nowAt.Add(-24*time.Hour), 1)
	insertPendingPRReportRun(t, db, "other-workspace", "ENG-1", "product", "Other workspace", nowAt.Add(-48*time.Hour), 1)
	insertPendingPRReportRun(t, db, "empty", "", "eng", "Ignored", nowAt.Add(-96*time.Hour), 1)
	insertPendingPRReportPR(t, db, "first", 1, "open", false, nowAt.Add(-72*time.Hour))
	insertPendingPRReportPR(t, db, "first", 2, "closed", false, nowAt.Add(-72*time.Hour))
	insertPendingPRReportPR(t, db, "second", 3, "open", true, nowAt.Add(-24*time.Hour))
	insertPendingPRReportPR(t, db, "second", 4, "open", false, nowAt.Add(-48*time.Hour))
	insertPendingPRReportPR(t, db, "other-workspace", 5, "open", false, nowAt.Add(-48*time.Hour))

	message, ok, err := buildPendingPRsScheduledReport(context.Background(), s)
	if err != nil || !ok {
		t.Fatalf("build report = %v, %v, want report", ok, err)
	}
	if message.Title != "Pending PRs" || message.Summary[0] != "2 tickets with open PRs" {
		t.Fatalf("message = %#v", message)
	}
	if !strings.Contains(message.Body, "ENG-1 — Latest title") || !strings.Contains(message.Body, "#1 https://example.test/first/1") || !strings.Contains(message.Body, "#4 https://example.test/second/4") {
		t.Fatalf("body = %q", message.Body)
	}
	if strings.Contains(message.Body, "#2") || strings.Contains(message.Body, "#3") || strings.Contains(message.Body, "Ignored") {
		t.Fatalf("body includes closed, merged, or empty-ticket PR: %q", message.Body)
	}
}

func TestPendingPRsScheduledReportCapsTickets(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	nowAt := time.Now()
	for i := 0; i < 21; i++ {
		runID := fmt.Sprintf("cap-%d", i)
		insertPendingPRReportRun(t, db, runID, fmt.Sprintf("ENG-%d", i), "eng", fmt.Sprintf("Ticket %d", i), nowAt.Add(-time.Duration(i+1)*time.Hour), 1)
		insertPendingPRReportPR(t, db, runID, i+1, "open", false, nowAt.Add(-time.Duration(i+1)*time.Hour))
	}

	message, ok, err := buildPendingPRsScheduledReport(context.Background(), s)
	if err != nil || !ok {
		t.Fatalf("build report = %v, %v, want report", ok, err)
	}
	if got := strings.Count(message.Body, " days)"); got != scheduledPendingPRsTicketLimit {
		t.Fatalf("ticket lines = %d, want %d: %q", got, scheduledPendingPRsTicketLimit, message.Body)
	}
	if !strings.HasSuffix(message.Body, "…and 1 more") {
		t.Fatalf("body = %q", message.Body)
	}
}

func TestPendingPRsScheduledReportReturnsFalseWithoutOpenTickets(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)
	insertPendingPRReportRun(t, db, "closed", "ENG-1", "eng", "Closed", time.Now(), 0)

	message, ok, err := buildPendingPRsScheduledReport(context.Background(), s)
	if err != nil || ok || message != nil {
		t.Fatalf("build report = %#v, %v, %v, want nil, false, nil", message, ok, err)
	}
}

func insertPendingPRReportRun(t *testing.T, db *sql.DB, runID, issueID, workspace, title string, startedAt time.Time, openPRCount int) {
	t.Helper()
	startedAtMillis := startedAt.UnixMilli()
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: runID, AttemptID: "attempt-" + runID, ClawID: "claw-" + runID, TenantID: "test-tenant-id",
		Status: taskRunStatusClean, Phase: taskRunPhaseTerminal, OwnerType: taskRunOwnerWorkflow,
		Integration: "linear", StartedAt: startedAtMillis, IssueTitle: title, OpenPRCount: openPRCount,
	})
	if _, err := db.Exec(`UPDATE task_run_summaries SET issue_id=?, integration_workspace=? WHERE run_id=?`, issueID, workspace, runID); err != nil {
		t.Fatalf("set pending PR report ticket: %v", err)
	}
}

func insertPendingPRReportPR(t *testing.T, db *sql.DB, runID string, number int, state string, merged bool, openedAt time.Time) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT INTO task_run_prs(id,tenant_id,run_id,repo,pr_number,pr_url,state,merged,opened_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		fmt.Sprintf("pr-%s-%d", runID, number), "test-tenant-id", runID, "acme/app", number,
		fmt.Sprintf("https://example.test/%s/%d", runID, number), state, boolInt(merged), openedAt.UnixMilli(), openedAt.UnixMilli(), openedAt.UnixMilli()); err != nil {
		t.Fatalf("insert PR: %v", err)
	}
}
