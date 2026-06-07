package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
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
	TotalRuns         int                    `json:"totalRuns"`
	ByStatus          map[string]int         `json:"byStatus"`
	WarningBreakdown  map[string]int         `json:"warningBreakdown"`
	FailureBreakdown  map[string]int         `json:"failureBreakdown"`
	HumanInteractions int                    `json:"humanInteractions"`
	PRCounts          taskRunAnalyticsPRKPI  `json:"prCounts"`
	AppliedFilters    map[string]interface{} `json:"appliedFilters,omitempty"`
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
	Run taskRunAnalyticsRunView `json:"run"`
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
	MergedAt              int64    `json:"mergedAt"`
	FinishedAt            int64    `json:"finishedAt"`
	TimeoutAt             int64    `json:"timeoutAt"`
	LastEventAt           int64    `json:"lastEventAt"`
	MaterializedAt        int64    `json:"materializedAt"`
	UpdatedAt             int64    `json:"updatedAt"`
	AnalyticsEnabled      bool     `json:"analyticsEnabled"`
	RequiresPR            bool     `json:"requiresPr"`
	ExcludedReason        string   `json:"excludedReason"`
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
	jsonOK(w, taskRunAnalyticsRunsResponse{Runs: runs, NextCursor: nextCursor, Limit: limit})
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
	if len(parts) == 1 {
		run, found, err := s.readTaskRunAnalyticsRun(tenantID, runID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		if !found {
			jsonError(w, http.StatusNotFound, "not found")
			return
		}
		jsonOK(w, taskRunAnalyticsRunDetailResponse{Run: run})
		return
	}
	if len(parts) != 2 {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	if !s.taskRunAnalyticsRunExists(tenantID, runID) {
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
	filters := taskRunAnalyticsFilters{
		TenantID:    tenantFromCtx(r),
		Status:      splitTaskRunAnalyticsValues(q, "status"),
		Owner:       splitTaskRunAnalyticsValues(q, "owner", "owner_id", "ownerId", "owner_display_name", "ownerDisplayName"),
		OwnerType:   splitTaskRunAnalyticsValues(q, "owner_type", "ownerType"),
		Workspace:   splitTaskRunAnalyticsValues(q, "workspace", "workspace_name", "workspaceName"),
		Workflow:    splitTaskRunAnalyticsValues(q, "workflow", "workflow_name", "workflowName"),
		Factory:     splitTaskRunAnalyticsValues(q, "factory", "factory_name", "factoryName"),
		Integration: splitTaskRunAnalyticsValues(q, "integration"),
		Repo:        splitTaskRunAnalyticsValues(q, "repo"),
		Model:       splitTaskRunAnalyticsValues(q, "model"),
		WarningType: splitTaskRunAnalyticsValues(q, "warning_type", "warningType"),
		FailureType: splitTaskRunAnalyticsValues(q, "failure_type", "failureType"),
	}
	var err error
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

func (s *Server) readTaskRunAnalyticsFilterOptions(tenantID string) (taskRunAnalyticsFilterOptionsResponse, error) {
	response := taskRunAnalyticsFilterOptionsResponse{}
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
		run_kind, integration, integration_workspace, issue_id, claw_id, model, llm_key, repo,
		primary_pr_url, pr_count, open_pr_count, merged_pr_count, closed_pr_count, warning_types,
		failure_type, human_interaction_count, started_at, queued_at, provision_started_at,
		agent_started_at, pr_opened_at, merged_at, finished_at, timeout_at, last_event_at,
		materialized_at, updated_at, analytics_enabled, requires_pr, excluded_reason`
}

func scanTaskRunAnalyticsRuns(rows *sql.Rows) ([]taskRunAnalyticsRunView, error) {
	runs := []taskRunAnalyticsRunView{}
	for rows.Next() {
		var run taskRunAnalyticsRunView
		var warningsJSON string
		var analyticsEnabled, requiresPR int
		if err := rows.Scan(
			&run.RunID, &run.InitialAttemptID, &run.CurrentAttemptID, &run.Status, &run.Phase, &run.AttemptCount,
			&run.OwnerType, &run.WorkspaceName, &run.WorkflowName, &run.FactoryName, &run.OwnerID, &run.OwnerDisplayName,
			&run.RunKind, &run.Integration, &run.IntegrationWorkspace, &run.IssueID, &run.ClawID, &run.Model, &run.LLMKey,
			&run.Repo, &run.PrimaryPRURL, &run.PRCount, &run.OpenPRCount, &run.MergedPRCount, &run.ClosedPRCount,
			&warningsJSON, &run.FailureType, &run.HumanInteractionCount, &run.StartedAt, &run.QueuedAt,
			&run.ProvisionStartedAt, &run.AgentStartedAt, &run.PROpenedAt, &run.MergedAt, &run.FinishedAt,
			&run.TimeoutAt, &run.LastEventAt, &run.MaterializedAt, &run.UpdatedAt, &analyticsEnabled, &requiresPR,
			&run.ExcludedReason,
		); err != nil {
			return nil, err
		}
		run.AnalyticsEnabled = analyticsEnabled == 1
		run.RequiresPR = requiresPR == 1
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

func splitTaskRunAnalyticsValues(q url.Values, keys ...string) []string {
	seen := map[string]bool{}
	values := []string{}
	for _, key := range keys {
		for _, raw := range q[key] {
			for _, part := range strings.Split(raw, ",") {
				part = strings.TrimSpace(part)
				if part == "" || seen[part] {
					continue
				}
				seen[part] = true
				values = append(values, part)
			}
		}
	}
	return values
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
