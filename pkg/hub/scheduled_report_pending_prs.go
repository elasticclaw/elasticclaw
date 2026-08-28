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

func buildPendingPRsScheduledReport(ctx context.Context, s *Server) (*notify.Message, bool, error) {
	tenantID, err := s.githubTenantID()
	if err != nil {
		return nil, false, err
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH pending_tickets AS (
			SELECT integration, integration_workspace, issue_id
			  FROM task_run_summaries
			 WHERE tenant_id=? AND issue_id != ''
			 GROUP BY integration, integration_workspace, issue_id
			HAVING SUM(open_pr_count) > 0
		)
		SELECT s.integration, s.integration_workspace, s.issue_id, s.issue_title, s.run_id, s.started_at
		  FROM task_run_summaries s
		  JOIN pending_tickets p
		    ON p.integration=s.integration
		   AND p.integration_workspace=s.integration_workspace
		   AND p.issue_id=s.issue_id
		 WHERE s.tenant_id=?
		 ORDER BY s.started_at DESC`, tenantID, tenantID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	ticketsByKey := map[string]*scheduledPendingPRTicket{}
	var runIDs []string
	for rows.Next() {
		var integration, workspace, issueID, issueTitle, runID string
		var startedAt int64
		if err := rows.Scan(&integration, &workspace, &issueID, &issueTitle, &runID, &startedAt); err != nil {
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
	lines := make([]string, 0, min(len(tickets), scheduledPendingPRsTicketLimit)+1)
	for _, ticket := range tickets[:min(len(tickets), scheduledPendingPRsTicketLimit)] {
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
	if len(tickets) > scheduledPendingPRsTicketLimit {
		lines = append(lines, fmt.Sprintf("…and %d more", len(tickets)-scheduledPendingPRsTicketLimit))
	}
	body := strings.Join(lines, "\n")
	if runeLen(body) > scheduledPendingPRsBodyLimit {
		body = truncateRunes(body, scheduledPendingPRsBodyLimit-1) + "…"
	}
	message := &notify.Message{
		Title:    "Pending PRs",
		Emoji:    ":information_source:",
		Severity: notify.SeverityInfo,
		Body:     body,
		Summary:  []string{fmt.Sprintf("%d tickets with open PRs", len(tickets))},
	}
	if hubURL := strings.TrimRight(s.clawHubURL(), "/"); hubURL != "" {
		message.Link = notify.Link{URL: hubURL + "/analytics", Label: "View tickets"}
	}
	return message, true, nil
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
