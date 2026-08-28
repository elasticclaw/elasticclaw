package hub

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
)

const scheduledPendingPRsTicketLimit = 20
const scheduledPendingPRsBodyLimit = 2800

func init() {
	registerScheduledReport("pending_prs", buildPendingPRsScheduledReport)
}

type scheduledPendingPRTicket struct {
	key        string
	issueID    string
	issueTitle string
	runIDs     []string
	prs        []taskRunAnalyticsPRView
}

// scheduledPendingPRsTicketsSQL selects one row per ticket that currently has
// at least one open PR, keyed by (integration, workspace, issue_id). It joins
// task_run_prs directly — the same truth the rendered PR list reads — instead
// of the summaries' open_pr_count, and applies the requires_pr=1 AND
// analytics_enabled=1 defaults every analytics view applies
// (taskRunAnalyticsSummaryWhere), so the digest never names a ticket the
// linked /analytics page hides. Deliberately no time window on the runs: a
// ticket of ANY age with a still-open PR is exactly what this report exists
// to surface.
const scheduledPendingPRsTicketsSQL = `
	SELECT s.integration, s.integration_workspace, s.issue_id, MIN(p.opened_at) AS oldest_open_at
	  FROM task_run_summaries s
	  JOIN task_run_prs p ON p.tenant_id=s.tenant_id AND p.run_id=s.run_id
	 WHERE s.tenant_id=? AND s.issue_id != '' AND s.requires_pr=1 AND s.analytics_enabled=1
	   AND p.state='open' AND p.merged=0
	 GROUP BY s.integration, s.integration_workspace, s.issue_id`

func buildPendingPRsScheduledReport(ctx context.Context, s *Server) (*notify.Message, bool, error) {
	tenantID, err := s.githubTenantID()
	if err != nil {
		return nil, false, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+scheduledPendingPRsTicketsSQL+`)`, tenantID).Scan(&total); err != nil {
		return nil, false, err
	}
	if total == 0 {
		return nil, false, nil
	}
	// Cap the ticket set in SQL — keeping the oldest open PRs, the ones the
	// report most wants eyes on — so rendering ~20 lines never loads every
	// run and PR of every pending ticket.
	rows, err := s.db.QueryContext(ctx, `
		WITH pending_tickets AS (`+scheduledPendingPRsTicketsSQL+`
		 ORDER BY oldest_open_at, issue_id
		 LIMIT ?)
		SELECT s.integration, s.integration_workspace, s.issue_id, s.issue_title, s.run_id
		  FROM task_run_summaries s
		  JOIN pending_tickets p
		    ON p.integration=s.integration
		   AND p.integration_workspace=s.integration_workspace
		   AND p.issue_id=s.issue_id
		 WHERE s.tenant_id=? AND s.requires_pr=1 AND s.analytics_enabled=1
		 ORDER BY s.started_at DESC`, tenantID, scheduledPendingPRsTicketLimit, tenantID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	ticketsByKey := map[string]*scheduledPendingPRTicket{}
	var runIDs []string
	for rows.Next() {
		var integration, workspace, issueID, issueTitle, runID string
		if err := rows.Scan(&integration, &workspace, &issueID, &issueTitle, &runID); err != nil {
			return nil, false, err
		}
		key := taskRunAnalyticsTicketKey(integration, workspace, issueID)
		ticket := ticketsByKey[key]
		if ticket == nil {
			ticket = &scheduledPendingPRTicket{key: key, issueID: issueID}
			ticketsByKey[key] = ticket
		}
		if len(ticket.runIDs) == 0 {
			ticket.issueTitle = issueTitle
		}
		ticket.runIDs = append(ticket.runIDs, runID)
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	prsByRun, err := s.readTaskRunAnalyticsPRsForRuns(tenantID, runIDs)
	if err != nil {
		return nil, false, err
	}
	var tickets []*scheduledPendingPRTicket
	for _, ticket := range ticketsByKey {
		for _, runID := range ticket.runIDs {
			for _, pr := range prsByRun[runID] {
				if pr.State == "open" && !pr.Merged {
					ticket.prs = append(ticket.prs, pr)
				}
			}
		}
		if len(ticket.prs) > 0 {
			tickets = append(tickets, ticket)
		}
	}
	if len(tickets) == 0 {
		return nil, false, nil
	}

	sort.Slice(tickets, func(i, j int) bool {
		return scheduledPendingPROldestAt(tickets[i]) < scheduledPendingPROldestAt(tickets[j])
	})
	nowAt := time.Now()
	lines := make([]string, 0, len(tickets))
	for _, ticket := range tickets {
		links := make([]string, 0, len(ticket.prs))
		for _, pr := range ticket.prs {
			links = append(links, fmt.Sprintf("#%d %s", pr.PRNumber, pr.URL))
		}
		ageDays := int(nowAt.Sub(time.UnixMilli(scheduledPendingPROldestAt(ticket))).Hours() / 24)
		if ageDays < 0 {
			ageDays = 0
		}
		title := ticket.issueTitle
		if title == "" {
			title = ticket.issueID
		}
		lines = append(lines, fmt.Sprintf("%s — %s: %s (%d days)", ticket.issueID, title, strings.Join(links, ", "), ageDays))
	}
	message := &notify.Message{
		Title:    "Pending PRs",
		Emoji:    ":information_source:",
		Severity: notify.SeverityInfo,
		Body:     scheduledPendingPRsBody(lines, total),
		Summary:  []string{fmt.Sprintf("%d tickets with open PRs", total)},
	}
	if hubURL := strings.TrimRight(s.clawHubURL(), "/"); hubURL != "" {
		message.Link = notify.Link{URL: hubURL + "/analytics", Label: "View tickets"}
	}
	return message, true, nil
}

// scheduledPendingPRsBody fits the ticket lines into the body limit by
// dropping whole trailing lines FIRST and appending the overflow line last,
// so "…and N more" survives the cut instead of being the first thing
// truncated. N counts every ticket not rendered as its own line — the ones
// past the SQL cap and the ones dropped here.
func scheduledPendingPRsBody(lines []string, totalTickets int) string {
	compose := func(lines []string) string {
		body := strings.Join(lines, "\n")
		if more := totalTickets - len(lines); more > 0 {
			body += fmt.Sprintf("\n…and %d more", more)
		}
		return body
	}
	body := compose(lines)
	for runeLen(body) > scheduledPendingPRsBodyLimit && len(lines) > 1 {
		lines = lines[:len(lines)-1]
		body = compose(lines)
	}
	if runeLen(body) <= scheduledPendingPRsBodyLimit {
		return body
	}
	// A single ticket line alone exceeds the limit: truncate the line itself,
	// still keeping the overflow count intact after it.
	overflow := ""
	if more := totalTickets - 1; more > 0 {
		overflow = fmt.Sprintf("\n…and %d more", more)
	}
	return truncateRunes(lines[0], scheduledPendingPRsBodyLimit-runeLen(overflow)-1) + "…" + overflow
}

func scheduledPendingPROldestAt(ticket *scheduledPendingPRTicket) int64 {
	oldestAt := ticket.prs[0].OpenedAt
	for _, pr := range ticket.prs[1:] {
		if pr.OpenedAt < oldestAt {
			oldestAt = pr.OpenedAt
		}
	}
	return oldestAt
}
