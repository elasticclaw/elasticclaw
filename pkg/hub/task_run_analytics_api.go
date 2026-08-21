package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const taskRunAnalyticsDefaultLimit = 50
const taskRunAnalyticsMaxLimit = 200

var allowedTaskRunAnalyticsGroupColumns = map[string]bool{
	"status":       true,
	"failure_type": true,
}

var allowedTaskRunAnalyticsDistinctColumns = map[string]bool{
	"workspace_name": true,
	"workflow_name":  true,
	"factory_name":   true,
	"integration":    true,
	"repo":           true,
	"model":          true,
	"status":         true,
	"failure_type":   true,
}

var allowedTaskRunAnalyticsJSONDistinctColumns = map[string]bool{
	"warning_types": true,
}

type taskRunAnalyticsSummaryResponse struct {
	TotalRuns         int                              `json:"totalRuns"`
	ByStatus          map[string]int                   `json:"byStatus"`
	WarningBreakdown  map[string]int                   `json:"warningBreakdown"`
	FailureBreakdown  map[string]int                   `json:"failureBreakdown"`
	HumanInteractions int                              `json:"humanInteractions"`
	PRCounts          taskRunAnalyticsPRKPI            `json:"prCounts"`
	AppliedFilters    map[string]interface{}           `json:"appliedFilters,omitempty"`
	Prior             *taskRunAnalyticsSummaryResponse `json:"prior,omitempty"`
}

type taskRunAnalyticsPRKPI struct {
	Total  int `json:"total"`
	Open   int `json:"open"`
	Merged int `json:"merged"`
	Closed int `json:"closed"`
}

type taskRunAnalyticsRunsResponse struct {
	Runs       []taskRunAnalyticsRunView `json:"runs"`
	NextCursor string                    `json:"nextCursor,omitempty"`
	Limit      int                       `json:"limit"`
}

type taskRunAnalyticsRunDetailResponse struct {
	Run    taskRunAnalyticsRunView     `json:"run"`
	Stages []taskRunAnalyticsStageView `json:"stages,omitempty"`
}

type taskRunAnalyticsAttemptsResponse struct {
	Attempts []taskRunAnalyticsAttemptView `json:"attempts"`
}

type taskRunAnalyticsEventsResponse struct {
	Events []taskRunAnalyticsEventView `json:"events"`
}

type taskRunAnalyticsPRsResponse struct {
	PRs []taskRunAnalyticsPRView `json:"prs"`
}

type taskRunAnalyticsOutputsResponse struct {
	Outputs []taskRunAnalyticsOutputView `json:"outputs"`
	TraceID string                       `json:"traceId,omitempty"`
}

type taskRunAnalyticsFilterOptionsResponse struct {
	Workspaces   []string `json:"workspaces"`
	Workflows    []string `json:"workflows"`
	Factories    []string `json:"factories"`
	Integrations []string `json:"integrations"`
	Repos        []string `json:"repos"`
	Models       []string `json:"models"`
	Statuses     []string `json:"statuses"`
	WarningTypes []string `json:"warningTypes"`
	FailureTypes []string `json:"failureTypes"`
}

type taskRunAnalyticsRunView struct {
	RunID                 string   `json:"runId"`
	InitialAttemptID      string   `json:"initialAttemptId"`
	CurrentAttemptID      string   `json:"currentAttemptId"`
	Status                string   `json:"status"`
	Phase                 string   `json:"phase"`
	AttemptCount          int      `json:"attemptCount"`
	OwnerType             string   `json:"ownerType"`
	WorkspaceName         string   `json:"workspaceName"`
	WorkflowName          string   `json:"workflowName"`
	FactoryName           string   `json:"factoryName"`
	OwnerID               string   `json:"ownerId"`
	OwnerDisplayName      string   `json:"ownerDisplayName"`
	RunKind               string   `json:"runKind"`
	Integration           string   `json:"integration"`
	IntegrationWorkspace  string   `json:"integrationWorkspace"`
	IssueID               string   `json:"issueId"`
	IssueCreatedAt        int64    `json:"issueCreatedAt"`
	ClawID                string   `json:"clawId"`
	Model                 string   `json:"model"`
	LLMKey                string   `json:"llmKey"`
	Repo                  string   `json:"repo"`
	PrimaryPRURL          string   `json:"primaryPrUrl"`
	PRCount               int      `json:"prCount"`
	OpenPRCount           int      `json:"openPrCount"`
	MergedPRCount         int      `json:"mergedPrCount"`
	ClosedPRCount         int      `json:"closedPrCount"`
	WarningTypes          []string `json:"warningTypes"`
	FailureType           string   `json:"failureType"`
	HumanInteractionCount int      `json:"humanInteractionCount"`
	StartedAt             int64    `json:"startedAt"`
	QueuedAt              int64    `json:"queuedAt"`
	ProvisionStartedAt    int64    `json:"provisionStartedAt"`
	AgentStartedAt        int64    `json:"agentStartedAt"`
	PROpenedAt            int64    `json:"prOpenedAt"`
	ReadyAt               int64    `json:"readyAt"`
	ReadyToMergeMs        int64    `json:"readyToMergeMs"`
	MergedAt              int64    `json:"mergedAt"`
	FinishedAt            int64    `json:"finishedAt"`
	TimeoutAt             int64    `json:"timeoutAt"`
	LastEventAt           int64    `json:"lastEventAt"`
	MaterializedAt        int64    `json:"materializedAt"`
	UpdatedAt             int64    `json:"updatedAt"`
	AnalyticsEnabled      bool     `json:"analyticsEnabled"`
	RequiresPR            bool     `json:"requiresPr"`
	ExcludedReason        string   `json:"excludedReason"`
	InputTokens           *int64   `json:"inputTokens,omitempty"`
	OutputTokens          *int64   `json:"outputTokens,omitempty"`
	TotalTokens           *int64   `json:"totalTokens,omitempty"`
	EstimatedCostUsd      *float64 `json:"estimatedCostUsd,omitempty"`
	IssueTitle            string   `json:"issueTitle,omitempty"`
}

type taskRunAnalyticsStageView struct {
	StageID    string `json:"stageId"`
	Label      string `json:"label"`
	EnteredAt  int64  `json:"enteredAt"`
	ExitedAt   int64  `json:"exitedAt,omitempty"`
	DurationMs int64  `json:"durationMs"`
	Source     string `json:"source"`
}

type taskRunAnalyticsAttemptView struct {
	ID            string `json:"id"`
	AttemptID     string `json:"attemptId"`
	AttemptNumber int    `json:"attemptNumber"`
	TriggerID     string `json:"triggerId"`
	ClawID        string `json:"clawId"`
	Status        string `json:"status"`
	FailureType   string `json:"failureType"`
	StartedAt     int64  `json:"startedAt"`
	FinishedAt    int64  `json:"finishedAt"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

type taskRunAnalyticsEventView struct {
	ID               string         `json:"id"`
	AttemptID        string         `json:"attemptId"`
	EventKey         string         `json:"eventKey"`
	Source           string         `json:"source"`
	SourceEventID    string         `json:"sourceEventId"`
	SourceDeliveryID string         `json:"sourceDeliveryId"`
	EventType        string         `json:"eventType"`
	EventTime        int64          `json:"eventTime"`
	ObservedAt       int64          `json:"observedAt"`
	ActorType        string         `json:"actorType"`
	ActorID          string         `json:"actorId"`
	ActorLogin       string         `json:"actorLogin"`
	ActorDisplayName string         `json:"actorDisplayName"`
	InteractionRole  string         `json:"interactionRole"`
	TargetType       string         `json:"targetType"`
	TargetID         string         `json:"targetId"`
	TargetURL        string         `json:"targetUrl"`
	WarningType      string         `json:"warningType"`
	FailureType      string         `json:"failureType"`
	Detail           map[string]any `json:"detail"`
	CreatedAt        int64          `json:"createdAt"`
}

type taskRunAnalyticsPRView struct {
	ID               string `json:"id"`
	Repo             string `json:"repo"`
	PRNumber         int    `json:"prNumber"`
	URL              string `json:"url"`
	HeadSHA          string `json:"headSha"`
	HeadBranch       string `json:"headBranch"`
	LastAgentHeadSHA string `json:"lastAgentHeadSha"`
	BaseBranch       string `json:"baseBranch"`
	State            string `json:"state"`
	Merged           bool   `json:"merged"`
	OpenedAt         int64  `json:"openedAt"`
	ClosedAt         int64  `json:"closedAt"`
	MergedAt         int64  `json:"mergedAt"`
	MergedByLogin    string `json:"mergedByLogin"`
	CreatedAt        int64  `json:"createdAt"`
	UpdatedAt        int64  `json:"updatedAt"`
}

type taskRunAnalyticsOutputView struct {
	ClawID     string              `json:"clawId"`
	AttemptID  string              `json:"attemptId,omitempty"`
	StageID    string              `json:"stageId"`
	OutputName string              `json:"outputName"`
	Stdout     string              `json:"stdout"`
	Stderr     string              `json:"stderr"`
	ExitCode   int                 `json:"exitCode"`
	SpanID     string              `json:"spanId"`
	SpanKind   string              `json:"spanKind"`
	DurationMs int64               `json:"durationMs"`
	Status     string              `json:"status"`
	Records    []pipelineLogRecord `json:"records"`
	CreatedAt  int64               `json:"createdAt"`
}

type taskRunAnalyticsFilters struct {
	TenantID         string
	FromStartedAt    int64
	ToStartedAt      int64
	Status           []string
	Owner            []string
	OwnerType        []string
	Workspace        []string
	Workflow         []string
	Factory          []string
	Integration      []string
	Repo             []string
	Model            []string
	WarningType      []string
	FailureType      []string
	HumanTouched     *bool
	MergedPRs        *bool
	RequiresPR       *bool
	AnalyticsEnabled *bool
}

type taskRunGeneralStat struct {
	AvgMs                *int64 `json:"avgMs"`
	Samples              int    `json:"samples"`
	AuthoritativeSamples *int   `json:"authoritativeSamples,omitempty"`
}

type taskRunGeneralStatsResponse struct {
	TicketToPr    taskRunGeneralStat           `json:"ticketToPrMs"`
	PROpenToMerge taskRunGeneralStat           `json:"prOpenToMergeMs"`
	AIImpl        taskRunGeneralStat           `json:"aiImplMs"`
	Prior         *taskRunGeneralStatsResponse `json:"prior,omitempty"`
}

func (s *Server) handleTaskRunAnalyticsGeneralStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	filters, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	bh := BusinessHoursFromEnv(r.URL.Query().Get("tz"))
	response, err := s.readTaskRunAnalyticsGeneralStats(filters, bh)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	priorFilters := taskRunAnalyticsPriorFilters(filters, time.Now().UTC())
	prior, err := s.readTaskRunAnalyticsGeneralStats(priorFilters, bh)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	response.Prior = &prior
	jsonOK(w, response)
}

func (s *Server) readTaskRunAnalyticsGeneralStats(filters taskRunAnalyticsFilters, bh BusinessHours) (taskRunGeneralStatsResponse, error) {
	where, args := taskRunAnalyticsSummaryWhere(filters)
	rows, err := s.db.Query(`SELECT issue_created_at, started_at, agent_started_at, pr_opened_at, ready_at, merged_at FROM task_run_summaries `+where, args...)
	if err != nil {
		return taskRunGeneralStatsResponse{}, err
	}
	defer rows.Close()
	var ticketSum, prSum, aiSum int64
	var ticketN, ticketAuthN, prN, aiN int
	for rows.Next() {
		var issue, started, agent, opened, ready, merged int64
		if err := rows.Scan(&issue, &started, &agent, &opened, &ready, &merged); err != nil {
			return taskRunGeneralStatsResponse{}, err
		}
		// Negative deltas are skipped: for GitHub-triggered claws the PR
		// pre-dates the run, so opened-started/opened-agent are meaningless.
		if started > 0 {
			base := issue
			fallback := base == 0
			if fallback {
				base = started
			}
			end := ready
			if end == 0 {
				end = opened
			}
			if end > 0 && end >= base {
				ticketSum += bh.DurationMs(base, end)
				ticketN++
				if !fallback {
					ticketAuthN++
				}
			}
		}
		prStart := ready
		if prStart == 0 {
			prStart = opened
		}
		if prStart > 0 && merged > 0 && merged >= prStart {
			prSum += bh.DurationMs(prStart, merged)
			prN++
		}
		// Agent runtime is machine time: it does not observe business hours, so
		// this stays a wall-clock delta even when tz is supplied.
		if agent > 0 && opened > 0 && opened >= agent {
			aiSum += opened - agent
			aiN++
		}
	}
	if err := rows.Err(); err != nil {
		return taskRunGeneralStatsResponse{}, err
	}
	average := func(sum int64, samples int) *int64 {
		if samples == 0 {
			return nil
		}
		value := sum / int64(samples)
		return &value
	}
	return taskRunGeneralStatsResponse{
		TicketToPr:    taskRunGeneralStat{AvgMs: average(ticketSum, ticketN), Samples: ticketN, AuthoritativeSamples: &ticketAuthN},
		PROpenToMerge: taskRunGeneralStat{AvgMs: average(prSum, prN), Samples: prN},
		AIImpl:        taskRunGeneralStat{AvgMs: average(aiSum, aiN), Samples: aiN},
	}, nil
}

func (s *Server) handleTaskRunAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	filters, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	response, err := s.readTaskRunAnalyticsSummary(filters)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	prior, err := s.readTaskRunAnalyticsSummary(taskRunAnalyticsPriorFilters(filters, time.Now().UTC()))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	response.Prior = &prior
	jsonOK(w, response)
}

func (s *Server) handleTaskRunAnalyticsRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.URL.Path != "/api/analytics/runs" && r.URL.Path != "/api/analytics/runs/" {
		s.handleTaskRunAnalyticsRunSubresource(w, r)
		return
	}
	filters, err := parseTaskRunAnalyticsFilters(r)
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := taskRunAnalyticsLimit(r.URL.Query().Get("limit"))
	cursorStartedAt, cursorRunID, err := decodeTaskRunAnalyticsCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	runs, nextCursor, err := s.readTaskRunAnalyticsRuns(filters, limit, cursorStartedAt, cursorRunID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	bh := BusinessHoursFromEnv(r.URL.Query().Get("tz"))
	for i := range runs {
		runs[i].ReadyToMergeMs = taskRunReadyToMergeMs(runs[i], bh)
	}
	jsonOK(w, taskRunAnalyticsRunsResponse{Runs: runs, NextCursor: nextCursor, Limit: limit})
}

// taskRunReadyToMergeMs is the business-hours span from the PR becoming ready
// for review (falling back to when it was opened) to it being merged.
func taskRunReadyToMergeMs(run taskRunAnalyticsRunView, bh BusinessHours) int64 {
	ready := run.ReadyAt
	if ready == 0 {
		ready = run.PROpenedAt
	}
	return bh.DurationMs(ready, run.MergedAt)
}

// taskRunAnalyticsStages builds per-stage timing views for a run's detail
// response. Duration for each stage is wall-clock: it ends at its own
// exited_at if set, else the next stage's entered_at, else the run's
// finished_at, else time-now for the still-open stage of a running run.
func taskRunAnalyticsStages(stages []taskRunStage, runFinishedAt int64) []taskRunAnalyticsStageView {
	views := make([]taskRunAnalyticsStageView, 0, len(stages))
	for i, stage := range stages {
		var end int64
		switch {
		case stage.ExitedAt != nil:
			end = *stage.ExitedAt
		case i+1 < len(stages):
			end = stages[i+1].EnteredAt
		case runFinishedAt > 0:
			end = runFinishedAt
		default:
			end = now().UnixMilli()
		}
		durationMs := end - stage.EnteredAt
		if durationMs < 0 {
			durationMs = 0
		}
		var exitedAt int64
		if stage.ExitedAt != nil {
			exitedAt = *stage.ExitedAt
		}
		views = append(views, taskRunAnalyticsStageView{
			StageID:    stage.StageID,
			Label:      stage.Label,
			EnteredAt:  stage.EnteredAt,
			ExitedAt:   exitedAt,
			DurationMs: durationMs,
			Source:     stage.Source,
		})
	}
	return views
}

func (s *Server) handleTaskRunAnalyticsRunSubresource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tenantID := tenantFromCtx(r)
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/analytics/runs/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	runID, err := url.PathUnescape(parts[0])
	if err != nil || runID == "" {
		jsonError(w, http.StatusBadRequest, "invalid run id")
		return
	}
	run, found, err := s.readTaskRunAnalyticsRun(tenantID, runID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	if !found {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	if len(parts) == 1 {
		run.ReadyToMergeMs = taskRunReadyToMergeMs(run, BusinessHoursFromEnv(r.URL.Query().Get("tz")))
		stages, err := taskRunStagesForRun(s.db, tenantID, runID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		jsonOK(w, taskRunAnalyticsRunDetailResponse{Run: run, Stages: taskRunAnalyticsStages(stages, run.FinishedAt)})
		return
	}
	if len(parts) != 2 {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	switch parts[1] {
	case "attempts":
		attempts, err := s.readTaskRunAnalyticsAttempts(tenantID, runID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		jsonOK(w, taskRunAnalyticsAttemptsResponse{Attempts: attempts})
	case "events":
		events, err := s.readTaskRunAnalyticsEvents(tenantID, runID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		jsonOK(w, taskRunAnalyticsEventsResponse{Events: events})
	case "prs":
		prs, err := s.readTaskRunAnalyticsPRs(tenantID, runID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		jsonOK(w, taskRunAnalyticsPRsResponse{PRs: prs})
	case "outputs":
		outputs, err := s.readTaskRunAnalyticsOutputs(tenantID, runID, run.ClawID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		// run_id is already the durable identifier shared by all attempts, so it
		// is the run-level trace ID rather than introducing another synthetic ID.
		jsonOK(w, taskRunAnalyticsOutputsResponse{Outputs: outputs, TraceID: runID})
	default:
		jsonError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleTaskRunAnalyticsFilterOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	response, err := s.readTaskRunAnalyticsFilterOptions(tenantFromCtx(r))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	jsonOK(w, response)
}

func parseTaskRunAnalyticsFilters(r *http.Request) (taskRunAnalyticsFilters, error) {
	q := r.URL.Query()
	read := func(key string, aliases ...string) ([]string, error) {
		return splitTaskRunAnalyticsValues(q, append([]string{key}, aliases...)...)
	}
	status, err := read("status")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	owner, err := read("owner", "owner_id", "ownerId", "owner_display_name", "ownerDisplayName")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	ownerType, err := read("owner_type", "ownerType")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	workspace, err := read("workspace", "workspace_name", "workspaceName")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	workflow, err := read("workflow", "workflow_name", "workflowName")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	factory, err := read("factory", "factory_name", "factoryName")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	integration, err := read("integration")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	repo, err := read("repo")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	model, err := read("model")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	warningType, err := read("warning_type", "warningType")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	failureType, err := read("failure_type", "failureType")
	if err != nil {
		return taskRunAnalyticsFilters{}, err
	}
	filters := taskRunAnalyticsFilters{
		TenantID:    tenantFromCtx(r),
		Status:      status,
		Owner:       owner,
		OwnerType:   ownerType,
		Workspace:   workspace,
		Workflow:    workflow,
		Factory:     factory,
		Integration: integration,
		Repo:        repo,
		Model:       model,
		WarningType: warningType,
		FailureType: failureType,
	}
	if filters.FromStartedAt, err = parseTaskRunAnalyticsTime(q, "from", "start", "started_after", "startedAfter"); err != nil {
		return filters, err
	}
	if filters.ToStartedAt, err = parseTaskRunAnalyticsTime(q, "to", "end", "started_before", "startedBefore"); err != nil {
		return filters, err
	}
	if filters.RequiresPR, err = parseOptionalTaskRunAnalyticsBool(q, "requires_pr", "requiresPr"); err != nil {
		return filters, err
	}
	if filters.AnalyticsEnabled, err = parseOptionalTaskRunAnalyticsBool(q, "analytics_enabled", "analyticsEnabled"); err != nil {
		return filters, err
	}
	if filters.HumanTouched, err = parseOptionalTaskRunAnalyticsBool(q, "human_touched", "humanTouched"); err != nil {
		return filters, err
	}
	if filters.MergedPRs, err = parseOptionalTaskRunAnalyticsBool(q, "merged_prs", "mergedPrs"); err != nil {
		return filters, err
	}
	return filters, nil
}

// taskRunAnalyticsPriorFilters returns the immediately preceding, equal-length
// started_at window.  Analytics historically has no default window, so use a
// trailing 30-day current window solely to establish the default comparison.
func taskRunAnalyticsPriorFilters(filters taskRunAnalyticsFilters, now time.Time) taskRunAnalyticsFilters {
	const dayMS int64 = 24 * 60 * 60 * 1000
	if filters.FromStartedAt > 0 && filters.ToStartedAt > 0 && filters.ToStartedAt >= filters.FromStartedAt {
		length := filters.ToStartedAt - filters.FromStartedAt + 1
		filters.ToStartedAt = filters.FromStartedAt - 1
		filters.FromStartedAt -= length
		return filters
	}
	end := now.UTC().UnixMilli()
	filters.ToStartedAt = end - 30*dayMS
	filters.FromStartedAt = filters.ToStartedAt - 30*dayMS + 1
	return filters
}

func (s *Server) readTaskRunAnalyticsSummary(filters taskRunAnalyticsFilters) (taskRunAnalyticsSummaryResponse, error) {
	where, args := taskRunAnalyticsSummaryWhere(filters)
	response := taskRunAnalyticsSummaryResponse{
		ByStatus:         map[string]int{},
		WarningBreakdown: map[string]int{},
		FailureBreakdown: map[string]int{},
	}
	row := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(human_interaction_count),0), COALESCE(SUM(pr_count),0),
		       COALESCE(SUM(open_pr_count),0), COALESCE(SUM(merged_pr_count),0), COALESCE(SUM(closed_pr_count),0)
		  FROM task_run_summaries `+where, args...)
	if err := row.Scan(&response.TotalRuns, &response.HumanInteractions, &response.PRCounts.Total, &response.PRCounts.Open, &response.PRCounts.Merged, &response.PRCounts.Closed); err != nil {
		return response, err
	}

	if err := s.readTaskRunAnalyticsGroupedCounts(`status`, where, args, response.ByStatus); err != nil {
		return response, err
	}
	if err := s.readTaskRunAnalyticsGroupedCounts(`failure_type`, where+` AND failure_type != ''`, args, response.FailureBreakdown); err != nil {
		return response, err
	}
	warnings, err := s.db.Query(`SELECT je.value, COUNT(*) FROM task_run_summaries s, json_each(s.warning_types) je `+where+` GROUP BY je.value`, args...)
	if err != nil {
		return response, err
	}
	defer warnings.Close()
	for warnings.Next() {
		var warningType string
		var count int
		if err := warnings.Scan(&warningType, &count); err != nil {
			return response, err
		}
		if warningType != "" {
			response.WarningBreakdown[warningType] = count
		}
	}
	return response, warnings.Err()
}

func (s *Server) readTaskRunAnalyticsGroupedCounts(column, where string, args []any, into map[string]int) error {
	if !allowedTaskRunAnalyticsGroupColumns[column] {
		return fmt.Errorf("invalid grouping column: %s", column)
	}
	rows, err := s.db.Query(fmt.Sprintf(`SELECT %s, COUNT(*) FROM task_run_summaries %s GROUP BY %s`, column, where, column), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return err
		}
		if key != "" {
			into[key] = count
		}
	}
	return rows.Err()
}

func (s *Server) readTaskRunAnalyticsRuns(filters taskRunAnalyticsFilters, limit int, cursorStartedAt int64, cursorRunID string) ([]taskRunAnalyticsRunView, string, error) {
	where, args := taskRunAnalyticsSummaryWhere(filters)
	if cursorStartedAt > 0 && cursorRunID != "" {
		where += ` AND (started_at < ? OR (started_at = ? AND run_id < ?))`
		args = append(args, cursorStartedAt, cursorStartedAt, cursorRunID)
	}
	args = append(args, limit+1)
	rows, err := s.db.Query(`
		SELECT `+taskRunAnalyticsRunColumns()+`
		  FROM task_run_summaries `+where+`
		 ORDER BY started_at DESC, run_id DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	runs, err := scanTaskRunAnalyticsRuns(rows)
	if err != nil {
		return nil, "", err
	}
	nextCursor := ""
	if len(runs) > limit {
		last := runs[limit-1]
		nextCursor = encodeTaskRunAnalyticsCursor(last.StartedAt, last.RunID)
		runs = runs[:limit]
	}
	if runs == nil {
		runs = []taskRunAnalyticsRunView{}
	}
	return runs, nextCursor, nil
}

func (s *Server) readTaskRunAnalyticsRun(tenantID, runID string) (taskRunAnalyticsRunView, bool, error) {
	rows, err := s.db.Query(`
		SELECT `+taskRunAnalyticsRunColumns()+`
		  FROM task_run_summaries
		 WHERE tenant_id=? AND run_id=?`, tenantID, runID)
	if err != nil {
		return taskRunAnalyticsRunView{}, false, err
	}
	defer rows.Close()
	runs, err := scanTaskRunAnalyticsRuns(rows)
	if err != nil {
		return taskRunAnalyticsRunView{}, false, err
	}
	if len(runs) == 0 {
		return taskRunAnalyticsRunView{}, false, nil
	}
	return runs[0], true, nil
}

func (s *Server) readTaskRunAnalyticsAttempts(tenantID, runID string) ([]taskRunAnalyticsAttemptView, error) {
	rows, err := s.db.Query(`
		SELECT id, attempt_id, attempt_number, trigger_id, claw_id, status, failure_type,
		       started_at, finished_at, created_at, updated_at
		  FROM task_run_attempts
		 WHERE tenant_id=? AND run_id=?
		 ORDER BY attempt_number ASC`, tenantID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []taskRunAnalyticsAttemptView{}
	for rows.Next() {
		var attempt taskRunAnalyticsAttemptView
		if err := rows.Scan(&attempt.ID, &attempt.AttemptID, &attempt.AttemptNumber, &attempt.TriggerID, &attempt.ClawID, &attempt.Status, &attempt.FailureType, &attempt.StartedAt, &attempt.FinishedAt, &attempt.CreatedAt, &attempt.UpdatedAt); err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func (s *Server) readTaskRunAnalyticsAttemptsForRuns(tenantID string, runIDs []string) (map[string][]taskRunAnalyticsAttemptView, error) {
	attemptsByRun := map[string][]taskRunAnalyticsAttemptView{}
	if len(runIDs) == 0 {
		return attemptsByRun, nil
	}
	if len(runIDs) > 500 {
		for start := 0; start < len(runIDs); start += 500 {
			end := start + 500
			if end > len(runIDs) {
				end = len(runIDs)
			}
			chunk, err := s.readTaskRunAnalyticsAttemptsForRuns(tenantID, runIDs[start:end])
			if err != nil {
				return nil, err
			}
			for id, values := range chunk {
				attemptsByRun[id] = append(attemptsByRun[id], values...)
			}
		}
		return attemptsByRun, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")
	args := make([]any, 0, len(runIDs)+1)
	args = append(args, tenantID)
	for _, runID := range runIDs {
		args = append(args, runID)
	}
	rows, err := s.db.Query(`
		SELECT run_id, id, attempt_id, attempt_number, trigger_id, claw_id, status, failure_type,
		       started_at, finished_at, created_at, updated_at
		  FROM task_run_attempts
		 WHERE tenant_id=? AND run_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var attempt taskRunAnalyticsAttemptView
		if err := rows.Scan(&runID, &attempt.ID, &attempt.AttemptID, &attempt.AttemptNumber, &attempt.TriggerID, &attempt.ClawID, &attempt.Status, &attempt.FailureType, &attempt.StartedAt, &attempt.FinishedAt, &attempt.CreatedAt, &attempt.UpdatedAt); err != nil {
			return nil, err
		}
		attemptsByRun[runID] = append(attemptsByRun[runID], attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, attempts := range attemptsByRun {
		sort.Slice(attempts, func(i, j int) bool { return attempts[i].AttemptNumber < attempts[j].AttemptNumber })
	}
	return attemptsByRun, nil
}

func (s *Server) readTaskRunAnalyticsEvents(tenantID, runID string) ([]taskRunAnalyticsEventView, error) {
	rows, err := s.db.Query(`
		SELECT id, attempt_id, event_key, source, source_event_id, source_delivery_id, event_type,
		       event_time, observed_at, actor_type, actor_id, actor_login, actor_display_name,
		       interaction_role, target_type, target_id, target_url, warning_type, failure_type,
		       detail, created_at
		  FROM task_run_events
		 WHERE tenant_id=? AND run_id=?
		 ORDER BY event_time ASC, observed_at ASC, event_key ASC`, tenantID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []taskRunAnalyticsEventView{}
	for rows.Next() {
		var event taskRunAnalyticsEventView
		var detailJSON string
		if err := rows.Scan(&event.ID, &event.AttemptID, &event.EventKey, &event.Source, &event.SourceEventID, &event.SourceDeliveryID, &event.EventType, &event.EventTime, &event.ObservedAt, &event.ActorType, &event.ActorID, &event.ActorLogin, &event.ActorDisplayName, &event.InteractionRole, &event.TargetType, &event.TargetID, &event.TargetURL, &event.WarningType, &event.FailureType, &detailJSON, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Detail = map[string]any{}
		if detailJSON != "" {
			if err := json.Unmarshal([]byte(detailJSON), &event.Detail); err != nil {
				log.Printf("[task-run-analytics] failed to unmarshal event detail for %s: %v", event.ID, err)
			}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Server) readTaskRunAnalyticsEventsForRuns(tenantID string, runIDs []string) (map[string][]taskRunAnalyticsEventView, error) {
	return s.readTaskRunAnalyticsEventsForRunsWithDetail(tenantID, runIDs, true)
}

// readTaskRunAnalyticsEventsForRunsForTickets avoids loading event detail JSON:
// ticket story and timing use only event identity, type, time, and actor fields.
func (s *Server) readTaskRunAnalyticsEventsForRunsForTickets(tenantID string, runIDs []string) (map[string][]taskRunAnalyticsEventView, error) {
	return s.readTaskRunAnalyticsEventsForRunsWithDetail(tenantID, runIDs, false)
}

func (s *Server) readTaskRunAnalyticsEventsForRunsWithDetail(tenantID string, runIDs []string, includeDetail bool) (map[string][]taskRunAnalyticsEventView, error) {
	eventsByRun := map[string][]taskRunAnalyticsEventView{}
	if len(runIDs) == 0 {
		return eventsByRun, nil
	}
	if len(runIDs) > 500 {
		for start := 0; start < len(runIDs); start += 500 {
			end := start + 500
			if end > len(runIDs) {
				end = len(runIDs)
			}
			chunk, err := s.readTaskRunAnalyticsEventsForRunsWithDetail(tenantID, runIDs[start:end], includeDetail)
			if err != nil {
				return nil, err
			}
			for id, values := range chunk {
				eventsByRun[id] = append(eventsByRun[id], values...)
			}
		}
		return eventsByRun, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")
	args := make([]any, 0, len(runIDs)+1)
	args = append(args, tenantID)
	for _, runID := range runIDs {
		args = append(args, runID)
	}
	// Ticket timing uses every event, not only events with a story label. In
	// particular, ci_failed, agent_idle, pr_replaced, and human_* events can
	// be the most recent activity for a ticket.
	columns := `run_id, id, event_type, event_time, actor_login, actor_display_name`
	if includeDetail {
		columns = `run_id, id, attempt_id, event_key, source, source_event_id, source_delivery_id, event_type,
	       event_time, observed_at, actor_type, actor_id, actor_login, actor_display_name,
	       interaction_role, target_type, target_id, target_url, warning_type, failure_type, detail, created_at`
	}
	rows, err := s.db.Query(`
		SELECT `+columns+`
		  FROM task_run_events
		 WHERE tenant_id=? AND run_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID, detailJSON string
		var event taskRunAnalyticsEventView
		if includeDetail {
			scanArgs := []any{&runID, &event.ID, &event.AttemptID, &event.EventKey, &event.Source, &event.SourceEventID, &event.SourceDeliveryID, &event.EventType, &event.EventTime, &event.ObservedAt, &event.ActorType, &event.ActorID, &event.ActorLogin, &event.ActorDisplayName, &event.InteractionRole, &event.TargetType, &event.TargetID, &event.TargetURL, &event.WarningType, &event.FailureType}
			scanArgs = append(scanArgs, &detailJSON)
			scanArgs = append(scanArgs, &event.CreatedAt)
			if err := rows.Scan(scanArgs...); err != nil {
				return nil, err
			}
		} else if err := rows.Scan(&runID, &event.ID, &event.EventType, &event.EventTime, &event.ActorLogin, &event.ActorDisplayName); err != nil {
			return nil, err
		}
		if includeDetail {
			event.Detail = map[string]any{}
		}
		if includeDetail && detailJSON != "" {
			if err := json.Unmarshal([]byte(detailJSON), &event.Detail); err != nil {
				log.Printf("[task-run-analytics] failed to unmarshal event detail for %s: %v", event.ID, err)
			}
		}
		eventsByRun[runID] = append(eventsByRun[runID], event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, events := range eventsByRun {
		sort.Slice(events, func(i, j int) bool {
			if events[i].EventTime != events[j].EventTime {
				return events[i].EventTime < events[j].EventTime
			}
			if includeDetail && events[i].ObservedAt != events[j].ObservedAt {
				return events[i].ObservedAt < events[j].ObservedAt
			}
			if includeDetail {
				return events[i].EventKey < events[j].EventKey
			}
			return events[i].ID < events[j].ID
		})
	}
	return eventsByRun, nil
}

func (s *Server) readTaskRunAnalyticsPRs(tenantID, runID string) ([]taskRunAnalyticsPRView, error) {
	rows, err := s.db.Query(`
		SELECT id, repo, pr_number, pr_url, head_sha, head_branch, last_agent_head_sha, base_branch,
		       state, merged, opened_at, closed_at, merged_at, merged_by_login, created_at, updated_at
		  FROM task_run_prs
		 WHERE tenant_id=? AND run_id=?
		 ORDER BY opened_at ASC, pr_number ASC`, tenantID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prs := []taskRunAnalyticsPRView{}
	for rows.Next() {
		var pr taskRunAnalyticsPRView
		var merged int
		if err := rows.Scan(&pr.ID, &pr.Repo, &pr.PRNumber, &pr.URL, &pr.HeadSHA, &pr.HeadBranch, &pr.LastAgentHeadSHA, &pr.BaseBranch, &pr.State, &merged, &pr.OpenedAt, &pr.ClosedAt, &pr.MergedAt, &pr.MergedByLogin, &pr.CreatedAt, &pr.UpdatedAt); err != nil {
			return nil, err
		}
		pr.Merged = merged == 1
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

func (s *Server) readTaskRunAnalyticsPRsForRuns(tenantID string, runIDs []string) (map[string][]taskRunAnalyticsPRView, error) {
	prsByRun := map[string][]taskRunAnalyticsPRView{}
	if len(runIDs) == 0 {
		return prsByRun, nil
	}
	if len(runIDs) > 500 {
		for start := 0; start < len(runIDs); start += 500 {
			end := start + 500
			if end > len(runIDs) {
				end = len(runIDs)
			}
			chunk, err := s.readTaskRunAnalyticsPRsForRuns(tenantID, runIDs[start:end])
			if err != nil {
				return nil, err
			}
			for id, values := range chunk {
				prsByRun[id] = append(prsByRun[id], values...)
			}
		}
		return prsByRun, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")
	args := make([]any, 0, len(runIDs)+1)
	args = append(args, tenantID)
	for _, runID := range runIDs {
		args = append(args, runID)
	}
	rows, err := s.db.Query(`
		SELECT run_id, id, repo, pr_number, pr_url, head_sha, head_branch, last_agent_head_sha, base_branch,
		       state, merged, opened_at, closed_at, merged_at, merged_by_login, created_at, updated_at
		  FROM task_run_prs
		 WHERE tenant_id=? AND run_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var pr taskRunAnalyticsPRView
		var merged int
		if err := rows.Scan(&runID, &pr.ID, &pr.Repo, &pr.PRNumber, &pr.URL, &pr.HeadSHA, &pr.HeadBranch, &pr.LastAgentHeadSHA, &pr.BaseBranch, &pr.State, &merged, &pr.OpenedAt, &pr.ClosedAt, &pr.MergedAt, &pr.MergedByLogin, &pr.CreatedAt, &pr.UpdatedAt); err != nil {
			return nil, err
		}
		pr.Merged = merged == 1
		prsByRun[runID] = append(prsByRun[runID], pr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, prs := range prsByRun {
		sort.Slice(prs, func(i, j int) bool {
			if prs[i].OpenedAt != prs[j].OpenedAt {
				return prs[i].OpenedAt < prs[j].OpenedAt
			}
			return prs[i].PRNumber < prs[j].PRNumber
		})
	}
	return prsByRun, nil
}

func (s *Server) readTaskRunAnalyticsOutputs(tenantID, runID, runClawID string) ([]taskRunAnalyticsOutputView, error) {
	attemptRows, err := s.db.Query(`
		SELECT attempt_id, claw_id
		  FROM task_run_attempts
		 WHERE tenant_id=? AND run_id=?
		 ORDER BY attempt_number ASC`, tenantID, runID)
	if err != nil {
		return nil, err
	}
	attemptByClaw := map[string]string{}
	clawIDs := []string{}
	seenClaws := map[string]bool{}
	if runClawID != "" {
		clawIDs = append(clawIDs, runClawID)
		seenClaws[runClawID] = true
	}
	for attemptRows.Next() {
		var attemptID, clawID string
		if err := attemptRows.Scan(&attemptID, &clawID); err != nil {
			attemptRows.Close()
			return nil, err
		}
		if clawID == "" {
			continue
		}
		attemptByClaw[clawID] = attemptID
		if !seenClaws[clawID] {
			clawIDs = append(clawIDs, clawID)
			seenClaws[clawID] = true
		}
	}
	if err := attemptRows.Err(); err != nil {
		attemptRows.Close()
		return nil, err
	}
	attemptRows.Close()
	if len(clawIDs) == 0 {
		return []taskRunAnalyticsOutputView{}, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(clawIDs)), ",")
	args := make([]any, 0, len(clawIDs))
	for _, clawID := range clawIDs {
		args = append(args, clawID)
	}
	rows, err := s.db.Query(`
		SELECT claw_id, stage_id, output_name, stdout, stderr, exit_code, span_id, span_kind, duration_ms, status, records, created_at
		  FROM pipeline_outputs
		 WHERE claw_id IN (`+placeholders+`)
		 ORDER BY created_at ASC, claw_id ASC, output_name ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	outputs := []taskRunAnalyticsOutputView{}
	for rows.Next() {
		var output taskRunAnalyticsOutputView
		var createdAt time.Time
		var recordsJSON string
		if err := rows.Scan(&output.ClawID, &output.StageID, &output.OutputName, &output.Stdout, &output.Stderr, &output.ExitCode, &output.SpanID, &output.SpanKind, &output.DurationMs, &output.Status, &recordsJSON, &createdAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(recordsJSON), &output.Records); err != nil {
			return nil, fmt.Errorf("decode pipeline output records: %w", err)
		}
		if output.Records == nil {
			output.Records = []pipelineLogRecord{}
		}
		output.AttemptID = attemptByClaw[output.ClawID]
		output.CreatedAt = epochMillis(createdAt)
		outputs = append(outputs, output)
	}
	return outputs, rows.Err()
}

func (s *Server) readTaskRunAnalyticsFilterOptions(tenantID string) (taskRunAnalyticsFilterOptionsResponse, error) {
	response := taskRunAnalyticsFilterOptionsResponse{
		Workspaces: make([]string, 0), Workflows: make([]string, 0), Factories: make([]string, 0),
		Integrations: make([]string, 0), Repos: make([]string, 0), Models: make([]string, 0),
		Statuses: make([]string, 0), WarningTypes: make([]string, 0), FailureTypes: make([]string, 0),
	}
	var err error
	response.Workspaces, err = s.readDistinctTaskRunAnalyticsValues(tenantID, "workspace_name")
	if err != nil {
		return response, err
	}
	response.Workflows, err = s.readDistinctTaskRunAnalyticsValues(tenantID, "workflow_name")
	if err != nil {
		return response, err
	}
	response.Factories, err = s.readDistinctTaskRunAnalyticsValues(tenantID, "factory_name")
	if err != nil {
		return response, err
	}
	response.Integrations, err = s.readDistinctTaskRunAnalyticsValues(tenantID, "integration")
	if err != nil {
		return response, err
	}
	response.Repos, err = s.readDistinctTaskRunAnalyticsValues(tenantID, "repo")
	if err != nil {
		return response, err
	}
	response.Models, err = s.readDistinctTaskRunAnalyticsValues(tenantID, "model")
	if err != nil {
		return response, err
	}
	response.Statuses, err = s.readDistinctTaskRunAnalyticsValues(tenantID, "status")
	if err != nil {
		return response, err
	}
	response.FailureTypes, err = s.readDistinctTaskRunAnalyticsValues(tenantID, "failure_type")
	if err != nil {
		return response, err
	}
	response.WarningTypes, err = s.readDistinctTaskRunAnalyticsJSONValues(tenantID, "warning_types")
	return response, err
}

func (s *Server) readDistinctTaskRunAnalyticsValues(tenantID, column string) ([]string, error) {
	if !allowedTaskRunAnalyticsDistinctColumns[column] {
		return nil, fmt.Errorf("invalid distinct column: %s", column)
	}
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT DISTINCT %s
		  FROM task_run_summaries
		 WHERE tenant_id=? AND analytics_enabled=1 AND requires_pr=1 AND %s != ''
		 ORDER BY %s`, column, column, column), tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Server) readDistinctTaskRunAnalyticsJSONValues(tenantID, column string) ([]string, error) {
	if !allowedTaskRunAnalyticsJSONDistinctColumns[column] {
		return nil, fmt.Errorf("invalid JSON distinct column: %s", column)
	}
	rows, err := s.db.Query(fmt.Sprintf(`
		SELECT DISTINCT je.value
		  FROM task_run_summaries s, json_each(s.%s) je
		 WHERE s.tenant_id=? AND s.analytics_enabled=1 AND s.requires_pr=1 AND je.value != ''
		 ORDER BY je.value`, column), tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Server) taskRunAnalyticsRunExists(tenantID, runID string) bool {
	var exists int
	err := s.db.QueryRow(`SELECT 1 FROM task_run_summaries WHERE tenant_id=? AND run_id=?`, tenantID, runID).Scan(&exists)
	return err == nil
}

func taskRunAnalyticsSummaryWhere(filters taskRunAnalyticsFilters) (string, []any) {
	where := []string{`tenant_id=?`}
	args := []any{filters.TenantID}
	addTaskRunAnalyticsInFilter(&where, &args, "status", filters.Status)
	addTaskRunAnalyticsOwnerFilter(&where, &args, filters.Owner)
	addTaskRunAnalyticsInFilter(&where, &args, "owner_type", filters.OwnerType)
	addTaskRunAnalyticsInFilter(&where, &args, "workspace_name", filters.Workspace)
	addTaskRunAnalyticsInFilter(&where, &args, "workflow_name", filters.Workflow)
	addTaskRunAnalyticsInFilter(&where, &args, "factory_name", filters.Factory)
	addTaskRunAnalyticsInFilter(&where, &args, "integration", filters.Integration)
	addTaskRunAnalyticsInFilter(&where, &args, "repo", filters.Repo)
	addTaskRunAnalyticsInFilter(&where, &args, "model", filters.Model)
	addTaskRunAnalyticsInFilter(&where, &args, "failure_type", filters.FailureType)
	for _, warningType := range filters.WarningType {
		where = append(where, `EXISTS (SELECT 1 FROM json_each(warning_types) WHERE value=?)`)
		args = append(args, warningType)
	}
	if filters.HumanTouched != nil {
		if *filters.HumanTouched {
			where = append(where, `human_interaction_count > 0`)
		} else {
			where = append(where, `human_interaction_count = 0`)
		}
	}
	if filters.MergedPRs != nil {
		if *filters.MergedPRs {
			where = append(where, `merged_pr_count > 0`)
		} else {
			where = append(where, `merged_pr_count = 0`)
		}
	}
	if filters.FromStartedAt > 0 {
		where = append(where, `started_at >= ?`)
		args = append(args, filters.FromStartedAt)
	}
	if filters.ToStartedAt > 0 {
		where = append(where, `started_at <= ?`)
		args = append(args, filters.ToStartedAt)
	}
	eligibilityFilterProvided := filters.RequiresPR != nil || filters.AnalyticsEnabled != nil
	if filters.RequiresPR != nil {
		where = append(where, `requires_pr=?`)
		args = append(args, boolInt(*filters.RequiresPR))
	} else if !eligibilityFilterProvided {
		where = append(where, `requires_pr=1`)
	}
	if filters.AnalyticsEnabled != nil {
		where = append(where, `analytics_enabled=?`)
		args = append(args, boolInt(*filters.AnalyticsEnabled))
	} else if !eligibilityFilterProvided {
		where = append(where, `analytics_enabled=1`)
	}
	return "WHERE " + strings.Join(where, " AND "), args
}

func addTaskRunAnalyticsInFilter(where *[]string, args *[]any, column string, values []string) {
	if len(values) == 0 {
		return
	}
	placeholders := make([]string, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, "?")
		*args = append(*args, value)
	}
	*where = append(*where, column+" IN ("+strings.Join(placeholders, ",")+")")
}

func addTaskRunAnalyticsOwnerFilter(where *[]string, args *[]any, values []string) {
	if len(values) == 0 {
		return
	}
	ownerIDPlaceholders := make([]string, 0, len(values))
	ownerDisplayPlaceholders := make([]string, 0, len(values))
	for _, value := range values {
		ownerIDPlaceholders = append(ownerIDPlaceholders, "?")
		*args = append(*args, value)
	}
	for _, value := range values {
		ownerDisplayPlaceholders = append(ownerDisplayPlaceholders, "?")
		*args = append(*args, value)
	}
	*where = append(*where, `(owner_id IN (`+strings.Join(ownerIDPlaceholders, ",")+`) OR owner_display_name IN (`+strings.Join(ownerDisplayPlaceholders, ",")+`))`)
}

func taskRunAnalyticsRunColumns() string {
	return `run_id, initial_attempt_id, current_attempt_id, status, phase, attempt_count,
		owner_type, workspace_name, workflow_name, factory_name, owner_id, owner_display_name,
		run_kind, integration, integration_workspace, issue_id, issue_created_at, claw_id, model, llm_key, repo,
		primary_pr_url, pr_count, open_pr_count, merged_pr_count, closed_pr_count, warning_types,
		failure_type, human_interaction_count, started_at, queued_at, provision_started_at,
		agent_started_at, pr_opened_at, ready_at, merged_at, finished_at, timeout_at, last_event_at,
		materialized_at, updated_at, analytics_enabled, requires_pr, excluded_reason,
		input_tokens, output_tokens, total_tokens, estimated_cost_usd, usage_updated_at, issue_title`
}

func scanTaskRunAnalyticsRuns(rows *sql.Rows) ([]taskRunAnalyticsRunView, error) {
	runs := []taskRunAnalyticsRunView{}
	for rows.Next() {
		var run taskRunAnalyticsRunView
		var warningsJSON string
		var analyticsEnabled, requiresPR int
		var inputTokens, outputTokens, totalTokens, usageUpdatedAt int64
		var estimatedCostUSD float64
		if err := rows.Scan(
			&run.RunID, &run.InitialAttemptID, &run.CurrentAttemptID, &run.Status, &run.Phase, &run.AttemptCount,
			&run.OwnerType, &run.WorkspaceName, &run.WorkflowName, &run.FactoryName, &run.OwnerID, &run.OwnerDisplayName,
			&run.RunKind, &run.Integration, &run.IntegrationWorkspace, &run.IssueID, &run.IssueCreatedAt, &run.ClawID, &run.Model, &run.LLMKey,
			&run.Repo, &run.PrimaryPRURL, &run.PRCount, &run.OpenPRCount, &run.MergedPRCount, &run.ClosedPRCount,
			&warningsJSON, &run.FailureType, &run.HumanInteractionCount, &run.StartedAt, &run.QueuedAt,
			&run.ProvisionStartedAt, &run.AgentStartedAt, &run.PROpenedAt, &run.ReadyAt, &run.MergedAt, &run.FinishedAt,
			&run.TimeoutAt, &run.LastEventAt, &run.MaterializedAt, &run.UpdatedAt, &analyticsEnabled, &requiresPR,
			&run.ExcludedReason, &inputTokens, &outputTokens, &totalTokens, &estimatedCostUSD, &usageUpdatedAt, &run.IssueTitle,
		); err != nil {
			return nil, err
		}
		run.AnalyticsEnabled = analyticsEnabled == 1
		run.RequiresPR = requiresPR == 1
		if usageUpdatedAt != 0 {
			run.InputTokens = &inputTokens
			run.OutputTokens = &outputTokens
			run.TotalTokens = &totalTokens
			run.EstimatedCostUsd = &estimatedCostUSD
		}
		run.WarningTypes = []string{}
		if warningsJSON != "" {
			if err := json.Unmarshal([]byte(warningsJSON), &run.WarningTypes); err != nil {
				log.Printf("[task-run-analytics] failed to unmarshal warning types for run %s: %v", run.RunID, err)
			}
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

const taskRunAnalyticsFilterValueLimit = 100

func splitTaskRunAnalyticsValues(q url.Values, keys ...string) ([]string, error) {
	seen := map[string]bool{}
	values := []string{}
	for _, key := range keys {
		for _, raw := range q[key] {
			for _, part := range strings.Split(raw, ",") {
				part, err := url.PathUnescape(part)
				if err != nil {
					return nil, fmt.Errorf("invalid value for filter %s: %w", key, err)
				}
				part = strings.TrimSpace(part)
				if part == "" || seen[part] {
					continue
				}
				seen[part] = true
				values = append(values, part)
				if len(values) > taskRunAnalyticsFilterValueLimit {
					return nil, fmt.Errorf("too many values for filter %s, max %d", keys[0], taskRunAnalyticsFilterValueLimit)
				}
			}
		}
	}
	return values, nil
}

func parseTaskRunAnalyticsTime(q url.Values, keys ...string) (int64, error) {
	for _, key := range keys {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			continue
		}
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return ms, nil
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return 0, fmt.Errorf("invalid %s", key)
		}
		return epochMillis(t), nil
	}
	return 0, nil
}

func parseOptionalTaskRunAnalyticsBool(q url.Values, keys ...string) (*bool, error) {
	for _, key := range keys {
		raw := strings.TrimSpace(q.Get(key))
		if raw == "" {
			continue
		}
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid %s", key)
		}
		return &parsed, nil
	}
	return nil, nil
}

func taskRunAnalyticsLimit(raw string) int {
	limit, err := strconv.Atoi(raw)
	if err != nil || limit <= 0 {
		return taskRunAnalyticsDefaultLimit
	}
	if limit > taskRunAnalyticsMaxLimit {
		return taskRunAnalyticsMaxLimit
	}
	return limit
}

func encodeTaskRunAnalyticsCursor(startedAt int64, runID string) string {
	if startedAt <= 0 || runID == "" {
		return ""
	}
	return strconv.FormatInt(startedAt, 10) + ":" + runID
}

func decodeTaskRunAnalyticsCursor(raw string) (int64, string, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, "", nil
	}
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, "", fmt.Errorf("invalid cursor")
	}
	startedAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || startedAt <= 0 {
		return 0, "", fmt.Errorf("invalid cursor")
	}
	return startedAt, parts[1], nil
}
