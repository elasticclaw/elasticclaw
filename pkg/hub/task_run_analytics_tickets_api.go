package hub

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type taskRunAnalyticsTicketsResponse struct {
	Tickets    []taskRunAnalyticsTicketView `json:"tickets"`
	NextCursor string                       `json:"nextCursor,omitempty"`
	Limit      int                          `json:"limit"`
}

type taskRunAnalyticsTicketRunSummary struct {
	RunID        string  `json:"runId"`
	Status       string  `json:"status"`
	Phase        string  `json:"phase"`
	AttemptCount int     `json:"attemptCount"`
	Cost         float64 `json:"cost"`
	TotalTokens  int64   `json:"totalTokens"`
	HumanTouches int     `json:"humanTouches"`
	StartedAt    int64   `json:"startedAt"`
	LastActivity int64   `json:"lastActivity"`
}

type taskRunAnalyticsTicketPRView struct {
	taskRunAnalyticsPRView
	RunID string `json:"runId"`
}

type taskRunAnalyticsTicketStoryEntry struct {
	ID        string `json:"id"`
	EventType string `json:"eventType"`
	Label     string `json:"label"`
	Actor     string `json:"actor"`
	Time      int64  `json:"time"`
	RunID     string `json:"runId"`
	Kind      string `json:"kind"`
	Count     int    `json:"count"`
}

type taskRunAnalyticsTicketView struct {
	IssueID        string                             `json:"issueId"`
	IssueTitle     string                             `json:"issueTitle"`
	Status         string                             `json:"status"`
	Requester      string                             `json:"requester"`
	RequesterRole  string                             `json:"requesterRole,omitempty"`
	Team           string                             `json:"team,omitempty"`
	Priority       string                             `json:"priority"`
	Ask            string                             `json:"ask"`
	Source         string                             `json:"source"`
	ReportedAt     int64                              `json:"reportedAt"`
	RunIDs         []string                           `json:"runIds"`
	Runs           []taskRunAnalyticsTicketRunSummary `json:"runs"`
	RunCount       int                                `json:"runCount"`
	AttemptCount   int                                `json:"attemptCount"`
	FailedRunCount int                                `json:"failedRunCount"`
	Cost           float64                            `json:"cost"`
	TotalTokens    int64                              `json:"totalTokens"`
	HumanTouches   int                                `json:"humanTouches"`
	PRs            []taskRunAnalyticsTicketPRView     `json:"prs"`
	MergedPRCount  int                                `json:"mergedPrCount"`
	OpenPRCount    int                                `json:"openPrCount"`
	TimeToFirstRun int64                              `json:"timeToFirstRun"`
	LeadTime       int64                              `json:"leadTime"`
	LastActivity   int64                              `json:"lastActivity"`
	Story          []taskRunAnalyticsTicketStoryEntry `json:"story"`
}

var taskRunAnalyticsStoryLabels = map[string]string{
	"run_queued": "Work queued", "agent_started": "Agent started working", "pr_opened": "Pull request opened",
	"ci_succeeded": "Checks passed", "review_commented": "Human reviewed", "dashboard_message_sent": "Human stepped in",
	"pr_merged": "Shipped", "pr_closed": "Attempt discarded", "provision_failed": "Blocked: no sandbox available",
	"verification_failed": "Blocked: tests would not pass", "context_exhausted": "Blocked: agent ran out of context", "attempt_retried": "Retried",
}

var taskRunAnalyticsStoryKinds = map[string]string{
	"pr_merged": "good", "ci_succeeded": "good", "provision_failed": "bad", "verification_failed": "bad",
	"context_exhausted": "bad", "pr_closed": "bad", "review_commented": "human", "dashboard_message_sent": "human",
}

func (s *Server) handleTaskRunAnalyticsTickets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	filters, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	cursorAt, cursorIssueID, err := decodeTaskRunAnalyticsCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	tickets, err := s.readTaskRunAnalyticsTickets(filters, githubLoginFromContext(r.Context()))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	limit := taskRunAnalyticsLimit(r.URL.Query().Get("limit"))
	start := 0
	if cursorAt > 0 {
		for start < len(tickets) && (ticketCursorAt(tickets[start]) > cursorAt || (ticketCursorAt(tickets[start]) == cursorAt && tickets[start].IssueID >= cursorIssueID)) {
			start++
		}
	}
	tickets = tickets[start:]
	nextCursor := ""
	if len(tickets) > limit {
		last := tickets[limit-1]
		nextCursor = encodeTaskRunAnalyticsCursor(ticketCursorAt(last), last.IssueID)
		tickets = tickets[:limit]
	}
	jsonOK(w, taskRunAnalyticsTicketsResponse{Tickets: tickets, NextCursor: nextCursor, Limit: limit})
}

// Tickets are ordered by reported time (falling back to their first run) and issue ID.
func ticketCursorAt(ticket taskRunAnalyticsTicketView) int64 {
	if ticket.ReportedAt > 0 {
		return ticket.ReportedAt
	}
	if len(ticket.Runs) > 0 {
		return ticket.Runs[0].StartedAt
	}
	return 1
}

func (s *Server) readTaskRunAnalyticsTickets(filters taskRunAnalyticsFilters, githubLogin string) ([]taskRunAnalyticsTicketView, error) {
	groups := map[string][]taskRunAnalyticsRunView{}
	addRun := func(run taskRunAnalyticsRunView) bool {
		if run.IssueID != "" {
			groups[run.IssueID] = append(groups[run.IssueID], run)
		}
		return true
	}
	accessCfg := s.taskRunAnalyticsViewACL(githubLogin)
	var err error
	if accessCfg != nil {
		err = s.forEachViewableTaskRunAnalyticsRun(filters, 0, "", githubLogin, accessCfg, addRun)
	} else {
		var cursorAt int64
		var cursorRunID string
		for {
			var batch []taskRunAnalyticsRunView
			var nextCursor string
			batch, nextCursor, err = s.readTaskRunAnalyticsRuns(filters, taskRunAnalyticsMaxLimit, cursorAt, cursorRunID)
			if err != nil || nextCursor == "" {
				for _, run := range batch {
					addRun(run)
				}
				break
			}
			for _, run := range batch {
				addRun(run)
			}
			cursorAt, cursorRunID, err = decodeTaskRunAnalyticsCursor(nextCursor)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	tickets := make([]taskRunAnalyticsTicketView, 0, len(groups))
	for issueID, runs := range groups {
		ticket, err := s.buildTaskRunAnalyticsTicket(filters.TenantID, issueID, runs)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, ticket)
	}
	sort.Slice(tickets, func(i, j int) bool {
		left, right := ticketCursorAt(tickets[i]), ticketCursorAt(tickets[j])
		if left == right {
			return tickets[i].IssueID > tickets[j].IssueID
		}
		return left > right
	})
	return tickets, nil
}

func (s *Server) buildTaskRunAnalyticsTicket(tenantID, issueID string, runs []taskRunAnalyticsRunView) (taskRunAnalyticsTicketView, error) {
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt < runs[j].StartedAt })
	ticket := taskRunAnalyticsTicketView{IssueID: issueID, IssueTitle: runs[0].IssueTitle, Source: runs[0].Integration, RunIDs: []string{}, Runs: []taskRunAnalyticsTicketRunSummary{}, PRs: []taskRunAnalyticsTicketPRView{}, Story: []taskRunAnalyticsTicketStoryEntry{}}
	events := []taskRunAnalyticsTicketStoryEntry{}
	for _, run := range runs {
		attempts, err := s.readTaskRunAnalyticsAttempts(tenantID, run.RunID)
		if err != nil {
			return ticket, err
		}
		prs, err := s.readTaskRunAnalyticsPRs(tenantID, run.RunID)
		if err != nil {
			return ticket, err
		}
		runEvents, err := s.readTaskRunAnalyticsEvents(tenantID, run.RunID)
		if err != nil {
			return ticket, err
		}
		ticket.RunIDs = append(ticket.RunIDs, run.RunID)
		cost, tokens := taskRunTicketCostAndTokens(run)
		ticket.Runs = append(ticket.Runs, taskRunAnalyticsTicketRunSummary{RunID: run.RunID, Status: run.Status, Phase: run.Phase, AttemptCount: len(attempts), Cost: cost, TotalTokens: tokens, HumanTouches: run.HumanInteractionCount, StartedAt: run.StartedAt, LastActivity: run.LastEventAt})
		ticket.AttemptCount += len(attempts)
		ticket.Cost += cost
		ticket.TotalTokens += tokens
		ticket.HumanTouches += run.HumanInteractionCount
		if run.Status == "failed" {
			ticket.FailedRunCount++
		}
		if ticket.ReportedAt == 0 || (run.IssueCreatedAt > 0 && run.IssueCreatedAt < ticket.ReportedAt) {
			ticket.ReportedAt = run.IssueCreatedAt
		}
		for _, pr := range prs {
			ticket.PRs = append(ticket.PRs, taskRunAnalyticsTicketPRView{taskRunAnalyticsPRView: pr, RunID: run.RunID})
			if pr.State == "merged" {
				ticket.MergedPRCount++
			}
			if pr.State == "open" {
				ticket.OpenPRCount++
			}
		}
		for _, event := range runEvents {
			events = append(events, ticketStoryEntry(event, run.RunID))
		}
	}
	ticket.RunCount = len(runs)
	ticket.Status = deriveTaskRunAnalyticsTicketStatus(ticket.Runs, ticket.PRs)
	sort.Slice(events, func(i, j int) bool {
		if events[i].Time != events[j].Time {
			return events[i].Time < events[j].Time
		}
		if events[i].RunID != events[j].RunID {
			return events[i].RunID < events[j].RunID
		}
		return events[i].ID < events[j].ID
	})
	ticket.Story = collapseTaskRunAnalyticsTicketStory(events)
	firstStart := runs[0].StartedAt
	for _, event := range events {
		if event.Time > ticket.LastActivity {
			ticket.LastActivity = event.Time
		}
		if event.EventType == "pr_merged" && ticket.LeadTime == 0 && ticket.ReportedAt > 0 {
			ticket.LeadTime = event.Time - ticket.ReportedAt
		}
	}
	if ticket.LastActivity == 0 {
		ticket.LastActivity = runs[len(runs)-1].StartedAt
	}
	if ticket.ReportedAt > 0 {
		ticket.TimeToFirstRun = firstStart - ticket.ReportedAt
		if ticket.LeadTime == 0 {
			ticket.LeadTime = ticket.LastActivity - ticket.ReportedAt
		}
	}
	s.enrichTaskRunAnalyticsTicket(&ticket, runs[0])
	return ticket, nil
}

func taskRunTicketCostAndTokens(run taskRunAnalyticsRunView) (float64, int64) {
	var cost float64
	var tokens int64
	if run.EstimatedCostUsd != nil {
		cost = *run.EstimatedCostUsd
	}
	if run.TotalTokens != nil {
		tokens = *run.TotalTokens
	}
	return cost, tokens
}

func deriveTaskRunAnalyticsTicketStatus(runs []taskRunAnalyticsTicketRunSummary, prs []taskRunAnalyticsTicketPRView) string {
	for _, pr := range prs {
		if pr.State == "merged" {
			return "delivered"
		}
	}
	for _, pr := range prs {
		if pr.State == "open" {
			return "pr_open"
		}
	}
	allFailed := len(runs) > 0
	for _, run := range runs {
		if run.Status == "running" {
			return "in_progress"
		}
		if run.Status != "failed" {
			allFailed = false
		}
	}
	if allFailed {
		return "failed"
	}
	return "in_progress"
}

func ticketStoryEntry(event taskRunAnalyticsEventView, runID string) taskRunAnalyticsTicketStoryEntry {
	actor := event.ActorDisplayName
	if actor == "" {
		actor = event.ActorLogin
	}
	return taskRunAnalyticsTicketStoryEntry{ID: event.ID, EventType: event.EventType, Label: taskRunAnalyticsStoryLabels[event.EventType], Actor: actor, Time: event.EventTime, RunID: runID, Kind: taskRunAnalyticsStoryKinds[event.EventType], Count: 1}
}
func collapseTaskRunAnalyticsTicketStory(events []taskRunAnalyticsTicketStoryEntry) []taskRunAnalyticsTicketStoryEntry {
	story := []taskRunAnalyticsTicketStoryEntry{}
	for _, event := range events {
		if event.Label == "" {
			continue
		}
		if event.Kind == "" {
			event.Kind = "neutral"
		}
		if event.EventType == "attempt_retried" && len(story) > 0 && story[len(story)-1].EventType == "attempt_retried" {
			story[len(story)-1].Count++
			continue
		}
		story = append(story, event)
	}
	return story
}

func (s *Server) enrichTaskRunAnalyticsTicket(ticket *taskRunAnalyticsTicketView, run taskRunAnalyticsRunView) {
	if ticket.Source == "linear" {
		if _, workflow, ok, err := s.resolveWorkflowConfig(run.WorkspaceName, run.WorkflowName); err == nil && ok {
			if token := s.resolveLinearTokenForWorkflow(run.WorkspaceName, workflow); token != "" {
				if details, err := s.fetchLinearIssueDetails(token, ticket.IssueID); err == nil {
					ticket.Requester, ticket.Priority, ticket.Ask = details.Creator.Name, details.PriorityLabel, details.Description
					if ticket.ReportedAt == 0 {
						if parsed, err := parseTicketTimestamp(details.CreatedAt); err == nil {
							ticket.ReportedAt = parsed
						}
					}
					return
				} else {
					log.Printf("[task-run-analytics] Linear ticket enrichment failed for %s: %v", ticket.IssueID, err)
				}
			}
		}
	}
	if ticket.Source == "github-issues" {
		s.enrichTaskRunAnalyticsGitHubTicket(ticket, run)
	}
}

func (s *Server) enrichTaskRunAnalyticsGitHubTicket(ticket *taskRunAnalyticsTicketView, run taskRunAnalyticsRunView) {
	parts := strings.Split(ticket.IssueID, "/")
	if len(parts) != 3 {
		return
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil {
		return
	}
	_, workflow, ok, err := s.resolveWorkflowConfig(run.WorkspaceName, run.WorkflowName)
	if err != nil || !ok {
		return
	}
	token := s.resolveGitHubIssuesTokenForWorkflow(run.WorkspaceName, workflow)
	if token == "" {
		return
	}
	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	issue, err := s.queryGitHubIssue(parts[0]+"/"+parts[1], token, base, number)
	if err != nil {
		log.Printf("[task-run-analytics] GitHub ticket enrichment failed for %s: %v", ticket.IssueID, err)
		return
	}
	ticket.Requester, ticket.Ask = issue.User.Login, issue.Body
	if ticket.IssueTitle == "" {
		ticket.IssueTitle = issue.Title
	}
	if ticket.ReportedAt == 0 {
		if parsed, err := parseTicketTimestamp(issue.CreatedAt); err == nil {
			ticket.ReportedAt = parsed
		}
	}
}

func parseTicketTimestamp(value string) (int64, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, fmt.Errorf("parse timestamp: %w", err)
	}
	return parsed.UnixMilli(), nil
}
