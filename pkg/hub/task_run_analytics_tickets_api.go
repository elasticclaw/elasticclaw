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
	Total      int                          `json:"total"`
}

type taskRunAnalyticsTicketRunSummary struct {
	RunID        string  `json:"runId"`
	Status       string  `json:"status"`
	Phase        string  `json:"phase"`
	Model        string  `json:"model"`
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
	Repo           string                             `json:"repo,omitempty"`
	WorkflowName   string                             `json:"workflowName,omitempty"`
	WorkspaceName  string                             `json:"workspaceName,omitempty"`
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

// agent_idle is intentionally unmapped because the kit has no user-facing idle story entry.
var taskRunAnalyticsStoryLabels = map[string]string{
	"run_queued": "Work queued", "agent_started": "Agent started working", "pr_opened": "Pull request opened",
	"ci_succeeded": "Checks passed", "human_review_comment": "Human reviewed", "human_dashboard_message": "Human stepped in",
	"pr_merged": "Shipped", "pr_closed_unmerged": "Attempt discarded", "provision_failed": "Blocked: no sandbox available",
	// No hub-native verification-failed event type exists yet.
	"verification_failed": "Blocked: tests would not pass", "context_exhausted": "Blocked: agent ran out of context",
	// Currently unreachable pending a hub-native retry event type.
	"attempt_retried": "Retried",
}

var taskRunAnalyticsStoryKinds = map[string]string{
	"pr_merged": "good", "ci_succeeded": "good", "provision_failed": "bad", "verification_failed": "bad",
	"context_exhausted": "bad", "pr_closed_unmerged": "bad", "human_review_comment": "human", "human_dashboard_message": "human",
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
	groups, err := s.readTaskRunAnalyticsTicketGroups(filters, githubLoginFromContext(r.Context()))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	total := len(groups)
	limit := taskRunAnalyticsLimit(r.URL.Query().Get("limit"))
	start := 0
	if cursorAt > 0 {
		for start < len(groups) && (groups[start].cursorAt() > cursorAt || (groups[start].cursorAt() == cursorAt && groups[start].issueID >= cursorIssueID)) {
			start++
		}
	}
	groups = groups[start:]
	nextCursor := ""
	if len(groups) > limit {
		last := groups[limit-1]
		nextCursor = encodeTaskRunAnalyticsCursor(last.cursorAt(), last.issueID)
		groups = groups[:limit]
	}
	tickets := make([]taskRunAnalyticsTicketView, 0, len(groups))
	for _, group := range groups {
		ticket, err := s.buildTaskRunAnalyticsTicket(filters.TenantID, group.issueID, group.runs)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		tickets = append(tickets, ticket)
	}
	jsonOK(w, taskRunAnalyticsTicketsResponse{Tickets: tickets, NextCursor: nextCursor, Limit: limit, Total: total})
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

type taskRunAnalyticsTicketGroup struct {
	issueID    string
	runs       []taskRunAnalyticsRunView
	reportedAt int64
}

func (group taskRunAnalyticsTicketGroup) cursorAt() int64 {
	if group.reportedAt > 0 {
		return group.reportedAt
	}
	if len(group.runs) > 0 {
		return group.runs[0].StartedAt
	}
	return 1
}

// readTaskRunAnalyticsTicketGroups loads only the run data needed to group and
// page tickets. Full ticket expansion happens only after pagination.
func (s *Server) readTaskRunAnalyticsTicketGroups(filters taskRunAnalyticsFilters, githubLogin string) ([]taskRunAnalyticsTicketGroup, error) {
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
	tickets := make([]taskRunAnalyticsTicketGroup, 0, len(groups))
	for issueID, runs := range groups {
		sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt < runs[j].StartedAt })
		var reportedAt int64
		for _, run := range runs {
			if run.IssueCreatedAt > 0 && (reportedAt == 0 || run.IssueCreatedAt < reportedAt) {
				reportedAt = run.IssueCreatedAt
			}
		}
		tickets = append(tickets, taskRunAnalyticsTicketGroup{issueID: issueID, runs: runs, reportedAt: reportedAt})
	}
	sort.Slice(tickets, func(i, j int) bool {
		left, right := tickets[i].cursorAt(), tickets[j].cursorAt()
		if left == right {
			return tickets[i].issueID > tickets[j].issueID
		}
		return left > right
	})
	return tickets, nil
}

// readTaskRunAnalyticsTickets retains the full-expansion API for callers that
// need every ticket rather than a paginated handler response.
func (s *Server) readTaskRunAnalyticsTickets(filters taskRunAnalyticsFilters, githubLogin string) ([]taskRunAnalyticsTicketView, error) {
	groups, err := s.readTaskRunAnalyticsTicketGroups(filters, githubLogin)
	if err != nil {
		return nil, err
	}
	tickets := make([]taskRunAnalyticsTicketView, 0, len(groups))
	for _, group := range groups {
		ticket, err := s.buildTaskRunAnalyticsTicket(filters.TenantID, group.issueID, group.runs)
		if err != nil {
			return nil, err
		}
		tickets = append(tickets, ticket)
	}
	return tickets, nil
}

func (s *Server) buildTaskRunAnalyticsTicket(tenantID, issueID string, runs []taskRunAnalyticsRunView) (taskRunAnalyticsTicketView, error) {
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt < runs[j].StartedAt })
	ticket := taskRunAnalyticsTicketView{IssueID: issueID, IssueTitle: runs[0].IssueTitle, Source: runs[0].Integration, Repo: runs[0].Repo, WorkflowName: runs[0].WorkflowName, WorkspaceName: runs[0].WorkspaceName, RunIDs: []string{}, Runs: []taskRunAnalyticsTicketRunSummary{}, PRs: []taskRunAnalyticsTicketPRView{}, Story: []taskRunAnalyticsTicketStoryEntry{}}
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
		ticket.Runs = append(ticket.Runs, taskRunAnalyticsTicketRunSummary{RunID: run.RunID, Status: run.Status, Phase: run.Phase, Model: run.Model, AttemptCount: len(attempts), Cost: cost, TotalTokens: tokens, HumanTouches: run.HumanInteractionCount, StartedAt: run.StartedAt, LastActivity: run.LastEventAt})
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
			if pr.Merged {
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
	// Resolve fallback reported time before deriving ticket timing metrics.
	s.enrichTaskRunAnalyticsTicket(&ticket, runs[0])
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
		if len(events) > 0 && ticket.LeadTime == 0 {
			ticket.LeadTime = ticket.LastActivity - ticket.ReportedAt
		}
	}
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
		if pr.Merged {
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
		// Currently unreachable via the event pipeline pending a hub-native retry event type.
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
					ticket.Requester, ticket.Priority, ticket.Ask, ticket.Team = details.Creator.Name, details.PriorityLabel, details.Description, details.Team.Name
					// Neither Linear nor GitHub exposes a reliable requester-role field for an issue.
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
