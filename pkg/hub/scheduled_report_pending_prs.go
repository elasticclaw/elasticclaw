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
// opened_at=0 on an open PR is a row shape the codebase expects (the schema
// default; pr_watcher matches it explicitly) and means the opening time is
// unknown, not 1970 — such rows are excluded from MIN so a ticket carrying
// only unknowns gets a NULL oldest_open_at and sorts last, never displacing a
// genuinely old ticket from the cap.
// PR state is resolved ACROSS rows per (repo, pr_number), mirroring the
// claw_prs backfill in db.go: pr_watcher only visits live claws, so a run can
// hold a stranded (open, merged=0) row for a PR another run's row already
// records as merged or closed — and with no time window (correctly, per the
// report's contract) such a zombie row would otherwise surface the ticket
// forever, oldest-first, permanently occupying the cap.
const scheduledPendingPRsTicketsSQL = `
	SELECT s.integration, s.integration_workspace, s.issue_id,
	       MIN(CASE WHEN p.opened_at > 0 THEN p.opened_at END) AS oldest_open_at
	  FROM task_run_summaries s
	  JOIN task_run_prs p ON p.tenant_id=s.tenant_id AND p.run_id=s.run_id
	 WHERE s.tenant_id=? AND s.issue_id != '' AND s.requires_pr=1 AND s.analytics_enabled=1
	   AND p.state='open' AND p.merged=0
	   AND NOT EXISTS (
	       SELECT 1 FROM task_run_prs done
	        WHERE done.tenant_id=p.tenant_id AND done.repo=p.repo AND done.pr_number=p.pr_number
	          AND (done.merged=1 OR done.state='closed'))
	 GROUP BY s.integration, s.integration_workspace, s.issue_id`

func buildPendingPRsScheduledReport(ctx context.Context, s *Server) (*notify.Message, bool, error) {
	tenantID, err := s.githubTenantIDContext(ctx)
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
		 ORDER BY oldest_open_at IS NULL, oldest_open_at, issue_id
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

	prsByRun, err := s.readTaskRunAnalyticsPRsForRunsContext(ctx, tenantID, runIDs)
	if err != nil {
		return nil, false, err
	}
	resolved, err := s.scheduledPendingPRsResolvedElsewhere(ctx, tenantID, prsByRun)
	if err != nil {
		return nil, false, err
	}
	var tickets []*scheduledPendingPRTicket
	for _, ticket := range ticketsByKey {
		// task_run_prs is unique per (run, repo, pr_number), so a PR carried
		// by several runs of the same ticket (a retry continuing the same
		// branch) appears once per run; render each PR once, keeping the
		// earliest known opened_at for the age.
		seen := map[string]int{}
		for _, runID := range ticket.runIDs {
			for _, pr := range prsByRun[runID] {
				if pr.State != "open" || pr.Merged {
					continue
				}
				key := fmt.Sprintf("%s#%d", pr.Repo, pr.PRNumber)
				// Same cross-row resolution as the ticket query: a stranded
				// open row must not render a PR any other row for the tenant
				// records as merged or closed.
				if resolved[key] {
					continue
				}
				if at, ok := seen[key]; ok {
					if pr.OpenedAt > 0 && (ticket.prs[at].OpenedAt == 0 || pr.OpenedAt < ticket.prs[at].OpenedAt) {
						ticket.prs[at] = pr
					}
					continue
				}
				seen[key] = len(ticket.prs)
				ticket.prs = append(ticket.prs, pr)
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
		oi, oj := scheduledPendingPROldestAt(tickets[i]), scheduledPendingPROldestAt(tickets[j])
		if (oi > 0) != (oj > 0) {
			// Unknown ages sort last, matching the SQL cap's ordering.
			return oi > 0
		}
		return oi < oj
	})
	nowAt := time.Now()
	lines := make([]string, 0, len(tickets))
	for _, ticket := range tickets {
		links := make([]string, 0, len(ticket.prs))
		for _, pr := range ticket.prs {
			links = append(links, fmt.Sprintf("#%d %s", pr.PRNumber, pr.URL))
		}
		title := ticket.issueTitle
		if title == "" {
			title = ticket.issueID
		}
		line := fmt.Sprintf("%s — %s: %s", ticket.issueID, title, strings.Join(links, ", "))
		if oldestAt := scheduledPendingPROldestAt(ticket); oldestAt > 0 {
			ageDays := int(nowAt.Sub(time.UnixMilli(oldestAt)).Hours() / 24)
			if ageDays < 0 {
				ageDays = 0
			}
			line += fmt.Sprintf(" (%d days)", ageDays)
		}
		lines = append(lines, line)
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

// scheduledPendingPRsResolvedElsewhere returns, keyed "repo#number", the PRs
// among prsByRun's open rows that ANY task_run_prs row for the tenant marks
// merged or closed. The rows of the capped tickets alone cannot answer this: a
// zombie open row's resolving row may belong to a run of a ticket outside the
// cap, so resolution is read tenant-wide — mirroring the ticket query's
// NOT EXISTS, which alone cannot cover a ticket that qualifies via another,
// genuinely open PR.
func (s *Server) scheduledPendingPRsResolvedElsewhere(ctx context.Context, tenantID string, prsByRun map[string][]taskRunAnalyticsPRView) (map[string]bool, error) {
	conds := make([]string, 0, 8)
	args := []any{tenantID}
	seen := map[string]bool{}
	for _, prs := range prsByRun {
		for _, pr := range prs {
			key := fmt.Sprintf("%s#%d", pr.Repo, pr.PRNumber)
			if pr.State != "open" || pr.Merged || seen[key] {
				continue
			}
			seen[key] = true
			conds = append(conds, "(repo=? AND pr_number=?)")
			args = append(args, pr.Repo, pr.PRNumber)
		}
	}
	resolved := map[string]bool{}
	if len(conds) == 0 {
		return resolved, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT repo, pr_number FROM task_run_prs
		 WHERE tenant_id=? AND (merged=1 OR state='closed') AND (`+strings.Join(conds, " OR ")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var repo string
		var number int
		if err := rows.Scan(&repo, &number); err != nil {
			return nil, err
		}
		resolved[fmt.Sprintf("%s#%d", repo, number)] = true
	}
	return resolved, rows.Err()
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

// scheduledPendingPROldestAt returns the oldest KNOWN opened_at among the
// ticket's open PRs, or 0 when none carries one: opened_at=0 means unknown,
// and treating it as the epoch would render a ~20000-day age and sort the
// ticket ahead of genuinely old ones.
func scheduledPendingPROldestAt(ticket *scheduledPendingPRTicket) int64 {
	var oldestAt int64
	for _, pr := range ticket.prs {
		if pr.OpenedAt > 0 && (oldestAt == 0 || pr.OpenedAt < oldestAt) {
			oldestAt = pr.OpenedAt
		}
	}
	return oldestAt
}
