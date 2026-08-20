package hub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const taskRunAnalyticsTicketCacheTTL = 5 * time.Minute
const taskRunAnalyticsTicketCacheMaxEntries = 256
const taskRunAnalyticsTicketMaxPageDepth = 100
const taskRunAnalyticsTicketMetadataRefreshLimit = 16
const taskRunAnalyticsTicketMetadataColdLimit = 48
const taskRunAnalyticsTicketMetadataRetryBackoff = 2 * time.Minute
const taskRunAnalyticsTicketMetadataFailureThreshold = 3

var errInvalidTaskRunAnalyticsTicketKey = errors.New("invalid ticket key")

type taskRunAnalyticsTicketTotalCacheEntry struct {
	total     int
	expiresAt time.Time
}

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
	TicketKey      string                             `json:"ticketKey"`
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
	"ci_succeeded": "Checks passed", "ci_failed": "Blocked: tests would not pass", "human_review_comment": "Human reviewed", "human_dashboard_message": "Human stepped in",
	"pr_merged": "Shipped", "pr_closed_unmerged": "Attempt discarded", "provision_failed": "Blocked: no sandbox available",
	"context_exhausted": "Blocked: agent ran out of context",
	// Currently unreachable pending a hub-native retry event type.
	"attempt_retried": "Retried",
}

var taskRunAnalyticsStoryKinds = map[string]string{
	"pr_merged": "good", "ci_succeeded": "good", "ci_failed": "bad", "provision_failed": "bad",
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
	if key := r.URL.Query().Get("key"); key != "" {
		ticket, err := s.readTaskRunAnalyticsTicketByKey(r.Context(), filters, key)
		if err != nil {
			if errors.Is(err, errInvalidTaskRunAnalyticsTicketKey) {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		if ticket == nil {
			jsonError(w, http.StatusNotFound, "ticket not found")
			return
		}
		jsonOK(w, struct {
			Ticket taskRunAnalyticsTicketView `json:"ticket"`
		}{Ticket: *ticket})
		return
	}
	cursorAt, cursorIssueID, cursorDepth, err := s.decodeTaskRunAnalyticsTicketCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := taskRunAnalyticsLimit(r.URL.Query().Get("limit"))
	groups, total, hasNextPage, lastGroup, err := s.readTaskRunAnalyticsTicketGroupsPage(filters, cursorAt, cursorIssueID, limit)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	nextCursor := ""
	if hasNextPage {
		if cursorDepth+1 < taskRunAnalyticsTicketMaxPageDepth {
			nextCursor = s.encodeTaskRunAnalyticsTicketCursor(lastGroup.cursorAt(), lastGroup.key, cursorDepth+1)
		}
	}
	groups = nonEmptyTaskRunAnalyticsTicketGroups(groups)

	// Keep these reads page-batched: query count must not grow with ticket count.
	runIDs, issueIDs := []string{}, []string{}
	for _, group := range groups {
		for _, run := range group.runs {
			runIDs = append(runIDs, run.RunID)
		}
		issueIDs = append(issueIDs, group.key)
	}
	attemptsByRun, err := s.readTaskRunAnalyticsAttemptsForRuns(filters.TenantID, runIDs)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	prsByRun, err := s.readTaskRunAnalyticsPRsForRuns(filters.TenantID, runIDs)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	eventsByRun, err := s.readTaskRunAnalyticsTicketTimingForRuns(filters.TenantID, runIDs)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	metadataByIssue, err := s.readTaskRunAnalyticsTicketMetadataPage(r.Context(), filters.TenantID, issueIDs)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "db error")
		return
	}
	tickets := make([]taskRunAnalyticsTicketView, 0, len(groups))
	for _, group := range groups {
		ticket, err := s.buildTaskRunAnalyticsTicket(filters.TenantID, group.issueID, group.runs, attemptsByRun, prsByRun, eventsByRun, metadataByIssue[group.key], false)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "db error")
			return
		}
		ticket.TicketKey = group.key
		// The caption intentionally uses this cached total; accepted departures allow its 5-minute staleness.
		ticket.Ask, ticket.Story = "", []taskRunAnalyticsTicketStoryEntry{}
		tickets = append(tickets, ticket)
	}
	jsonOK(w, taskRunAnalyticsTicketsResponse{Tickets: tickets, NextCursor: nextCursor, Limit: limit, Total: total})
}

// readTaskRunAnalyticsTicketTimingForRuns keeps list-row timing accurate without
// materializing the per-event story used only by the ticket detail panel.
func (s *Server) readTaskRunAnalyticsTicketTimingForRuns(tenantID string, runIDs []string) (map[string][]taskRunAnalyticsEventView, error) {
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
			chunk, err := s.readTaskRunAnalyticsTicketTimingForRuns(tenantID, runIDs[start:end])
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
	rows, err := s.db.Query(`SELECT run_id, MAX(event_time), MIN(CASE WHEN event_type='pr_merged' THEN event_time END) FROM task_run_events WHERE tenant_id=? AND run_id IN (`+placeholders+`) GROUP BY run_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var lastActivity int64
		var firstMergedAt *int64
		if err := rows.Scan(&runID, &lastActivity, &firstMergedAt); err != nil {
			return nil, err
		}
		eventsByRun[runID] = append(eventsByRun[runID], taskRunAnalyticsEventView{EventTime: lastActivity})
		if firstMergedAt != nil {
			eventsByRun[runID] = append(eventsByRun[runID], taskRunAnalyticsEventView{EventType: "pr_merged", EventTime: *firstMergedAt})
		}
	}
	return eventsByRun, rows.Err()
}

// readTaskRunAnalyticsTicketGroupsPage groups by issue_id and orders by a
// computed aggregate that idx_task_run_summaries_ticket_page cannot serve
// because issue_id sits behind started_at in that index. Each page therefore
// re-aggregates and re-sorts the whole filtered window; the HAVING cursor only
// trims output, not aggregation work, making this O(window) per page. That is
// acceptable at current ticket volumes, but the page-depth cap is a stopgap
// until a materialized per-issue ordering column can provide the real fix.
func (s *Server) readTaskRunAnalyticsTicketGroupsPage(filters taskRunAnalyticsFilters, cursorAt int64, cursorIssueID string, limit int) ([]taskRunAnalyticsTicketGroup, int, bool, taskRunAnalyticsTicketGroup, error) {
	where, args := taskRunAnalyticsSummaryWhere(filters)
	total, err := s.readTaskRunAnalyticsTicketTotal(where, args)
	if err != nil {
		return nil, 0, false, taskRunAnalyticsTicketGroup{}, err
	}
	orderAt := `MAX(COALESCE(NULLIF(MIN(NULLIF(issue_created_at,0)),0), MIN(started_at), 1), 1)`
	groupWhere := where + ` AND issue_id != ''`
	groupArgs := append([]any{}, args...)
	if cursorAt > 0 && cursorIssueID != "" {
		groupWhere += ` GROUP BY ` + taskRunAnalyticsTicketKeySQL + ` HAVING (` + orderAt + ` < ? OR (` + orderAt + ` = ? AND ` + taskRunAnalyticsTicketKeySQL + ` < ?))`
		groupArgs = append(groupArgs, cursorAt, cursorAt, cursorIssueID)
	} else {
		groupWhere += ` GROUP BY ` + taskRunAnalyticsTicketKeySQL
	}
	groupArgs = append(groupArgs, limit+1)
	rows, err := s.db.Query(`SELECT `+taskRunAnalyticsTicketKeySQL+`, `+orderAt+` FROM task_run_summaries `+groupWhere+` ORDER BY 2 DESC, 1 DESC LIMIT ?`, groupArgs...)
	if err != nil {
		return nil, 0, false, taskRunAnalyticsTicketGroup{}, err
	}
	defer rows.Close()
	ids := []string{}
	cursorGroups := []taskRunAnalyticsTicketGroup{}
	for rows.Next() {
		var id string
		var ignored int64
		if err := rows.Scan(&id, &ignored); err != nil {
			return nil, 0, false, taskRunAnalyticsTicketGroup{}, err
		}
		ids = append(ids, id)
		cursorGroups = append(cursorGroups, taskRunAnalyticsTicketGroup{key: id, reportedAt: ignored})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, taskRunAnalyticsTicketGroup{}, err
	}
	if len(ids) == 0 {
		return []taskRunAnalyticsTicketGroup{}, total, false, taskRunAnalyticsTicketGroup{}, nil
	}
	hasNextPage := len(ids) > limit
	hydratedIDs := ids
	if hasNextPage {
		hydratedIDs = ids[:limit]
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(hydratedIDs)), ",")
	runWhere := where + ` AND ` + taskRunAnalyticsTicketKeySQL + ` IN (` + ph + `)`
	runArgs := append([]any{}, args...)
	for _, id := range hydratedIDs {
		runArgs = append(runArgs, id)
	}
	runRows, err := s.db.Query(`SELECT `+taskRunAnalyticsRunColumns()+` FROM task_run_summaries `+runWhere, runArgs...)
	if err != nil {
		return nil, 0, false, taskRunAnalyticsTicketGroup{}, err
	}
	defer runRows.Close()
	runs, err := scanTaskRunAnalyticsRuns(runRows)
	if err != nil {
		return nil, 0, false, taskRunAnalyticsTicketGroup{}, err
	}
	return taskRunAnalyticsTicketGroupsFromHydration(hydratedIDs, runs), total, hasNextPage, cursorGroups[len(hydratedIDs)-1], nil
}

func randomTicketCursorKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("generate ticket cursor key: " + err.Error())
	}
	return key
}

func (s *Server) loadTaskRunAnalyticsTicketCursorKey() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS hub_settings (key TEXT PRIMARY KEY, value BLOB NOT NULL)`); err != nil {
		return err
	}
	var key []byte
	err := s.db.QueryRow(`SELECT value FROM hub_settings WHERE key=?`, "task_run_analytics_ticket_cursor_key").Scan(&key)
	if err == nil && len(key) >= 32 {
		s.ticketCursorKey = key
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	key = randomTicketCursorKey()
	if err == nil {
		log.Printf("[task-run-analytics] replaced invalid ticket cursor key")
	}
	if _, err := s.db.Exec(`INSERT INTO hub_settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "task_run_analytics_ticket_cursor_key", key); err != nil {
		return err
	}
	s.ticketCursorKey = key
	return nil
}

func (s *Server) readTaskRunAnalyticsTicketByKey(ctx context.Context, filters taskRunAnalyticsFilters, key string) (*taskRunAnalyticsTicketView, error) {
	parts := strings.Split(key, taskRunAnalyticsTicketKeySeparator)
	if len(parts) != 3 || parts[2] == "" {
		return nil, errInvalidTaskRunAnalyticsTicketKey
	}
	where, args := taskRunAnalyticsSummaryWhere(filters)
	runWhere := where + ` AND integration = ? AND integration_workspace = ? AND issue_id = ?`
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskRunAnalyticsRunColumns()+` FROM task_run_summaries `+runWhere, append(args, parts[0], parts[1], parts[2])...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs, err := scanTaskRunAnalyticsRuns(rows)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	runIDs := make([]string, 0, len(runs))
	for _, run := range runs {
		runIDs = append(runIDs, run.RunID)
	}
	attempts, err := s.readTaskRunAnalyticsAttemptsForRuns(filters.TenantID, runIDs)
	if err != nil {
		return nil, err
	}
	prs, err := s.readTaskRunAnalyticsPRsForRuns(filters.TenantID, runIDs)
	if err != nil {
		return nil, err
	}
	events, err := s.readTaskRunAnalyticsEventsForRunsForTickets(filters.TenantID, runIDs)
	if err != nil {
		return nil, err
	}
	metadata, err := s.readTaskRunAnalyticsTicketMetadataPage(ctx, filters.TenantID, []string{key})
	if err != nil {
		return nil, err
	}
	ticket, err := s.buildTaskRunAnalyticsTicket(filters.TenantID, parts[2], runs, attempts, prs, events, metadata[key], true)
	if err != nil {
		return nil, err
	}
	ticket.TicketKey = key
	return &ticket, nil
}

// Ticket cursor depth gates an expensive aggregate, so it is MACed instead of
// trusting a client-rewritable page counter.
func (s *Server) encodeTaskRunAnalyticsTicketCursor(cursorAt int64, issueID string, depth int) string {
	if cursorAt <= 0 || issueID == "" || depth <= 0 {
		return ""
	}
	payload := strconv.Itoa(depth) + ":" + strconv.FormatInt(cursorAt, 10) + ":" + base64.RawURLEncoding.EncodeToString([]byte(issueID))
	mac := hmac.New(sha256.New, s.ticketCursorKey)
	_, _ = mac.Write([]byte(payload))
	return "ticket:" + payload + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) decodeTaskRunAnalyticsTicketCursor(raw string) (int64, string, int, error) {
	if !strings.HasPrefix(raw, "ticket:") {
		// Pre-signature cursors are deliberately restarted rather than trusted:
		// their client-controlled depth could bypass the aggregate page cap.
		return 0, "", 0, nil
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 5 || parts[3] == "" || parts[4] == "" {
		return 0, "", 0, fmt.Errorf("invalid cursor")
	}
	depth, err := strconv.Atoi(parts[1])
	if err != nil || depth <= 0 || depth >= taskRunAnalyticsTicketMaxPageDepth {
		return 0, "", 0, fmt.Errorf("invalid cursor")
	}
	cursorAt, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || cursorAt <= 0 {
		return 0, "", 0, fmt.Errorf("invalid cursor")
	}
	issueID, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(issueID) == 0 {
		return 0, "", 0, fmt.Errorf("invalid cursor")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return 0, "", 0, fmt.Errorf("invalid cursor")
	}
	mac := hmac.New(sha256.New, s.ticketCursorKey)
	_, _ = mac.Write([]byte(strings.Join(parts[1:4], ":")))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		// A restart may rotate a legacy key; restart safely rather than reject pagination.
		return 0, "", 0, nil
	}
	return cursorAt, string(issueID), depth, nil
}

// quantizeTaskRunAnalyticsTicketArgs snaps int64 args (the started_at window
// bounds — the only int64s the analytics where-builder produces) down to the
// cache TTL. The quantized args feed BOTH the cache key and the COUNT query, so a
// cached total always describes exactly the window its key names.
func quantizeTaskRunAnalyticsTicketArgs(args []any) []any {
	quantized := append([]any(nil), args...)
	for i, arg := range quantized {
		if value, ok := arg.(int64); ok {
			quantized[i] = value / int64(taskRunAnalyticsTicketCacheTTL/time.Millisecond) * int64(taskRunAnalyticsTicketCacheTTL/time.Millisecond)
		}
	}
	return quantized
}

func taskRunAnalyticsTicketCacheKey(where string, args []any) string {
	return fmt.Sprintf("%s|%#v", where, args)
}

func (s *Server) readTaskRunAnalyticsTicketTotal(where string, args []any) (int, error) {
	args = quantizeTaskRunAnalyticsTicketArgs(args)
	key := taskRunAnalyticsTicketCacheKey(where, args)
	now := time.Now()
	s.ticketAnalyticsCacheMu.Lock()
	if entry, ok := s.ticketAnalyticsTotalCache[key]; ok && now.Before(entry.expiresAt) {
		s.ticketAnalyticsCacheMu.Unlock()
		return entry.total, nil
	}
	s.ticketAnalyticsCacheMu.Unlock()

	result, err, _ := s.ticketAnalyticsTotalGroup.Do(key, func() (any, error) {
		now := time.Now()
		s.ticketAnalyticsCacheMu.Lock()
		if entry, ok := s.ticketAnalyticsTotalCache[key]; ok && now.Before(entry.expiresAt) {
			s.ticketAnalyticsCacheMu.Unlock()
			return entry.total, nil
		}
		s.ticketAnalyticsCacheMu.Unlock()

		var total int
		if err := s.db.QueryRow(`SELECT COUNT(DISTINCT `+taskRunAnalyticsTicketKeySQL+`) FROM task_run_summaries `+where+` AND issue_id != ''`, args...).Scan(&total); err != nil {
			return 0, err
		}
		s.ticketAnalyticsCacheMu.Lock()
		if s.ticketAnalyticsTotalCache == nil {
			s.ticketAnalyticsTotalCache = map[string]taskRunAnalyticsTicketTotalCacheEntry{}
		}
		pruneTaskRunAnalyticsTicketTotalCache(s.ticketAnalyticsTotalCache, now)
		if len(s.ticketAnalyticsTotalCache) >= taskRunAnalyticsTicketCacheMaxEntries {
			evictOldestTaskRunAnalyticsTicketTotalCacheEntry(s.ticketAnalyticsTotalCache)
		}
		// Ticket rows are always freshly queried. Total is only a UI caption;
		// the TTL bounds staleness from the quantized window edges.
		s.ticketAnalyticsTotalCache[key] = taskRunAnalyticsTicketTotalCacheEntry{total: total, expiresAt: now.Add(taskRunAnalyticsTicketCacheTTL)}
		s.ticketAnalyticsTotalQueries++
		s.ticketAnalyticsCacheMu.Unlock()
		return total, nil
	})
	if err != nil {
		return 0, err
	}
	return result.(int), nil
}

func pruneTaskRunAnalyticsTicketTotalCache(cache map[string]taskRunAnalyticsTicketTotalCacheEntry, now time.Time) {
	for key, entry := range cache {
		if !now.Before(entry.expiresAt) {
			delete(cache, key)
		}
	}
}

func evictOldestTaskRunAnalyticsTicketTotalCacheEntry(cache map[string]taskRunAnalyticsTicketTotalCacheEntry) {
	var oldestKey string
	var oldestExpiry time.Time
	for key, entry := range cache {
		if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
			oldestKey, oldestExpiry = key, entry.expiresAt
		}
	}
	delete(cache, oldestKey)
}

func taskRunAnalyticsTicketGroupsFromHydration(ids []string, runs []taskRunAnalyticsRunView) []taskRunAnalyticsTicketGroup {
	byID := map[string][]taskRunAnalyticsRunView{}
	for _, run := range runs {
		key := taskRunAnalyticsTicketKey(run.Integration, run.IntegrationWorkspace, run.IssueID)
		byID[key] = append(byID[key], run)
	}
	groups := make([]taskRunAnalyticsTicketGroup, 0, len(ids))
	for _, id := range ids {
		rs := byID[id]
		sort.Slice(rs, func(i, j int) bool { return rs[i].StartedAt < rs[j].StartedAt })
		var reported int64
		for _, run := range rs {
			if run.IssueCreatedAt > 0 && (reported == 0 || run.IssueCreatedAt < reported) {
				reported = run.IssueCreatedAt
			}
		}
		if group, ok := taskRunAnalyticsTicketGroupWithRuns(id, rs, reported); ok {
			groups = append(groups, group)
		}
	}
	return groups
}

type taskRunAnalyticsTicketGroup struct {
	key        string
	issueID    string
	runs       []taskRunAnalyticsRunView
	reportedAt int64
}

const taskRunAnalyticsTicketKeySeparator = "\x1f"
const taskRunAnalyticsTicketKeySQL = "integration || char(31) || integration_workspace || char(31) || issue_id"

func taskRunAnalyticsTicketKey(integration, workspace, issueID string) string {
	return integration + taskRunAnalyticsTicketKeySeparator + workspace + taskRunAnalyticsTicketKeySeparator + issueID
}

func nonEmptyTaskRunAnalyticsTicketGroups(groups []taskRunAnalyticsTicketGroup) []taskRunAnalyticsTicketGroup {
	filtered := make([]taskRunAnalyticsTicketGroup, 0, len(groups))
	for _, group := range groups {
		if len(group.runs) == 0 {
			continue
		}
		filtered = append(filtered, group)
	}
	return filtered
}

func taskRunAnalyticsTicketGroupWithRuns(issueID string, runs []taskRunAnalyticsRunView, reportedAt int64) (taskRunAnalyticsTicketGroup, bool) {
	if len(runs) == 0 {
		return taskRunAnalyticsTicketGroup{}, false
	}
	return taskRunAnalyticsTicketGroup{key: issueID, issueID: runs[0].IssueID, runs: runs, reportedAt: reportedAt}, true
}

func (group taskRunAnalyticsTicketGroup) cursorAt() int64 {
	if group.reportedAt > 0 {
		return group.reportedAt
	}
	if len(group.runs) > 0 && group.runs[0].StartedAt > 0 {
		return group.runs[0].StartedAt
	}
	return 1
}

func (s *Server) buildTaskRunAnalyticsTicket(tenantID, issueID string, runs []taskRunAnalyticsRunView, attemptsByRun map[string][]taskRunAnalyticsAttemptView, prsByRun map[string][]taskRunAnalyticsPRView, eventsByRun map[string][]taskRunAnalyticsEventView, metadata taskRunAnalyticsTicketMetadata, includeStory bool) (taskRunAnalyticsTicketView, error) {
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt < runs[j].StartedAt })
	ticket := taskRunAnalyticsTicketView{TicketKey: taskRunAnalyticsTicketKey(runs[0].Integration, runs[0].IntegrationWorkspace, issueID), IssueID: issueID, IssueTitle: runs[0].IssueTitle, Source: runs[0].Integration, Repo: runs[0].Repo, WorkflowName: runs[0].WorkflowName, WorkspaceName: runs[0].WorkspaceName, RunIDs: []string{}, Runs: []taskRunAnalyticsTicketRunSummary{}, PRs: []taskRunAnalyticsTicketPRView{}, Story: []taskRunAnalyticsTicketStoryEntry{}}
	events := []taskRunAnalyticsTicketStoryEntry{}
	for _, run := range runs {
		attempts := attemptsByRun[run.RunID]
		prs := prsByRun[run.RunID]
		runEvents := eventsByRun[run.RunID]
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
			if includeStory {
				events = append(events, ticketStoryEntry(event, run.RunID))
			} else {
				events = append(events, taskRunAnalyticsTicketStoryEntry{EventType: event.EventType, Time: event.EventTime, RunID: run.RunID})
			}
		}
	}
	ticket.RunCount = len(runs)
	ticket.Status = deriveTaskRunAnalyticsTicketStatus(ticket.Runs, ticket.PRs)
	// Resolve fallback reported time before deriving ticket timing metrics.
	s.applyOrScheduleTaskRunAnalyticsTicketMetadata(tenantID, &ticket, runs[0], metadata)
	sort.Slice(events, func(i, j int) bool {
		if events[i].Time != events[j].Time {
			return events[i].Time < events[j].Time
		}
		if events[i].RunID != events[j].RunID {
			return events[i].RunID < events[j].RunID
		}
		return events[i].ID < events[j].ID
	})
	if includeStory {
		ticket.Story = collapseTaskRunAnalyticsTicketStory(events)
	}
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

func (s *Server) enrichTaskRunAnalyticsTicket(ticket *taskRunAnalyticsTicketView, run taskRunAnalyticsRunView) bool {
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
					return true
				} else {
					log.Printf("[task-run-analytics] Linear ticket enrichment failed for %s: %v", ticket.IssueID, err)
				}
			}
		}
	}
	if ticket.Source == "github-issues" {
		return s.enrichTaskRunAnalyticsGitHubTicket(ticket, run)
	}
	return false
}

// readOrScheduleTaskRunAnalyticsTicketMetadata keeps tracker HTTP out of the request path.
type taskRunAnalyticsTicketMetadata struct {
	requester, requesterRole, team, priority, ask string
	reportedAt, updatedAt, lastAttemptAt          int64
	consecutiveFailures                           int
}

func (s *Server) readTaskRunAnalyticsTicketMetadataPage(ctx context.Context, tenantID string, issueIDs []string) (map[string]taskRunAnalyticsTicketMetadata, error) {
	result := map[string]taskRunAnalyticsTicketMetadata{}
	for start := 0; start < len(issueIDs); start += 500 {
		end := start + 500
		if end > len(issueIDs) {
			end = len(issueIDs)
		}
		args := []any{tenantID}
		terms := make([]string, 0, end-start)
		for _, id := range issueIDs[start:end] {
			parts := strings.Split(id, taskRunAnalyticsTicketKeySeparator)
			if len(parts) != 3 {
				continue
			}
			terms = append(terms, "(integration=? AND integration_workspace=? AND issue_id=?)")
			args = append(args, parts[0], parts[1], parts[2])
		}
		if len(terms) == 0 {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT integration || char(31) || integration_workspace || char(31) || issue_id, requester, requester_role, team, priority, ask, reported_at, updated_at, last_attempt_at, consecutive_failures FROM ticket_metadata WHERE tenant_id=? AND (`+strings.Join(terms, " OR ")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var m taskRunAnalyticsTicketMetadata
			if err := rows.Scan(&id, &m.requester, &m.requesterRole, &m.team, &m.priority, &m.ask, &m.reportedAt, &m.updatedAt, &m.lastAttemptAt, &m.consecutiveFailures); err != nil {
				rows.Close()
				return nil, err
			}
			result[id] = m
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return result, nil
}

func (s *Server) applyOrScheduleTaskRunAnalyticsTicketMetadata(tenantID string, ticket *taskRunAnalyticsTicketView, run taskRunAnalyticsRunView, metadata taskRunAnalyticsTicketMetadata) {
	if metadata.updatedAt > 0 {
		ticket.Requester, ticket.RequesterRole, ticket.Team, ticket.Priority, ticket.Ask = metadata.requester, metadata.requesterRole, metadata.team, metadata.priority, metadata.ask
		if metadata.reportedAt != 0 {
			ticket.ReportedAt = metadata.reportedAt
		}
		if time.Since(time.UnixMilli(metadata.updatedAt)) < 15*time.Minute {
			return
		}
		// Terminal tickets do not change tracker metadata in a way the panel needs.
		if ticket.Status == "delivered" || ticket.Status == "failed" {
			return
		}
		if ticket.Source == "github-issues" && !defaultGitHubClient.allowLowPriority() {
			return
		}
	}
	if metadata.consecutiveFailures >= taskRunAnalyticsTicketMetadataFailureThreshold && metadata.lastAttemptAt > 0 && time.Since(time.UnixMilli(metadata.lastAttemptAt)) < 15*time.Minute {
		return
	}
	if metadata.lastAttemptAt > 0 && time.Since(time.UnixMilli(metadata.lastAttemptAt)) < taskRunAnalyticsTicketMetadataRetryBackoff {
		return
	}
	metadataKey := taskRunAnalyticsTicketKey(run.Integration, run.IntegrationWorkspace, ticket.IssueID)
	key := tenantID + ":" + metadataKey
	if _, loaded := s.ticketMetadataInflight.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	select {
	case s.ticketMetadataEnrichment <- struct{}{}:
	default:
		s.ticketMetadataInflight.Delete(key)
		return
	}
	if metadata.updatedAt > 0 {
		if !s.allowTaskRunAnalyticsTicketMetadataRefresh() {
			s.ticketMetadataInflight.Delete(key)
			<-s.ticketMetadataEnrichment
			return
		}
	} else if !s.allowTaskRunAnalyticsTicketMetadataColdEnrichment() {
		s.ticketMetadataInflight.Delete(key)
		<-s.ticketMetadataEnrichment
		return
	}
	go func() {
		defer s.ticketMetadataInflight.Delete(key)
		defer func() { <-s.ticketMetadataEnrichment }()
		metadata := taskRunAnalyticsTicketView{IssueID: ticket.IssueID, IssueTitle: ticket.IssueTitle, Source: ticket.Source, ReportedAt: ticket.ReportedAt}
		succeeded := s.enrichTaskRunAnalyticsTicket(&metadata, run)
		// Failed attempts retry quickly at first, then settle into the freshness window.
		attemptedAt := now().UnixMilli()
		updatedAt := int64(0)
		if succeeded {
			updatedAt = attemptedAt
		}
		consecutiveFailures := 0
		if !succeeded {
			consecutiveFailures = 1
		}
		if _, err := s.db.Exec(`INSERT INTO ticket_metadata(tenant_id,integration,integration_workspace,issue_id,requester,requester_role,team,priority,ask,reported_at,updated_at,last_attempt_at,consecutive_failures) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,integration,integration_workspace,issue_id) DO UPDATE SET requester=CASE WHEN excluded.requester != '' THEN excluded.requester ELSE ticket_metadata.requester END, requester_role=CASE WHEN excluded.requester_role != '' THEN excluded.requester_role ELSE ticket_metadata.requester_role END, team=CASE WHEN excluded.team != '' THEN excluded.team ELSE ticket_metadata.team END, priority=CASE WHEN excluded.priority != '' THEN excluded.priority ELSE ticket_metadata.priority END, ask=CASE WHEN excluded.ask != '' THEN excluded.ask ELSE ticket_metadata.ask END, reported_at=CASE WHEN excluded.reported_at != 0 THEN excluded.reported_at ELSE ticket_metadata.reported_at END, updated_at=CASE WHEN excluded.updated_at != 0 THEN excluded.updated_at ELSE ticket_metadata.updated_at END, last_attempt_at=excluded.last_attempt_at, consecutive_failures=CASE WHEN excluded.updated_at != 0 THEN 0 ELSE ticket_metadata.consecutive_failures+1 END`, tenantID, run.Integration, run.IntegrationWorkspace, metadata.IssueID, metadata.Requester, metadata.RequesterRole, metadata.Team, metadata.Priority, metadata.Ask, metadata.ReportedAt, updatedAt, attemptedAt, consecutiveFailures); err != nil {
			log.Printf("[task-run-analytics] write ticket metadata %s: %v", metadata.IssueID, err)
		}
	}()
}

func (s *Server) allowTaskRunAnalyticsTicketMetadataColdEnrichment() bool {
	// Cold lookups deserve a larger independent budget, but must not consume the refresh budget or burst the pool.
	s.ticketMetadataRefreshMu.Lock()
	defer s.ticketMetadataRefreshMu.Unlock()
	n := time.Now()
	if s.ticketMetadataColdAt.IsZero() || n.Sub(s.ticketMetadataColdAt) >= 15*time.Minute {
		s.ticketMetadataColdAt, s.ticketMetadataColdEnrichments = n, 0
	}
	if s.ticketMetadataColdEnrichments >= taskRunAnalyticsTicketMetadataColdLimit {
		return false
	}
	s.ticketMetadataColdEnrichments++
	return true
}

func (s *Server) allowTaskRunAnalyticsTicketMetadataRefresh() bool {
	s.ticketMetadataRefreshMu.Lock()
	defer s.ticketMetadataRefreshMu.Unlock()
	now := time.Now()
	if s.ticketMetadataRefreshAt.IsZero() || now.Sub(s.ticketMetadataRefreshAt) >= 15*time.Minute {
		s.ticketMetadataRefreshAt, s.ticketMetadataRefreshes = now, 0
	}
	if s.ticketMetadataRefreshes >= taskRunAnalyticsTicketMetadataRefreshLimit {
		return false
	}
	s.ticketMetadataRefreshes++
	return true
}

func (s *Server) enrichTaskRunAnalyticsGitHubTicket(ticket *taskRunAnalyticsTicketView, run taskRunAnalyticsRunView) bool {
	parts := strings.Split(ticket.IssueID, "/")
	if len(parts) != 3 {
		return false
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil {
		return false
	}
	_, workflow, ok, err := s.resolveWorkflowConfig(run.WorkspaceName, run.WorkflowName)
	if err != nil || !ok {
		return false
	}
	token := s.resolveGitHubIssuesTokenForWorkflow(run.WorkspaceName, workflow)
	if token == "" {
		return false
	}
	base := s.githubBaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	issue, err := s.queryGitHubIssueContext(ctx, parts[0]+"/"+parts[1], token, base, number)
	if err != nil {
		log.Printf("[task-run-analytics] GitHub ticket enrichment failed for %s: %v", ticket.IssueID, err)
		return false
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
	return true
}

func parseTicketTimestamp(value string) (int64, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, fmt.Errorf("parse timestamp: %w", err)
	}
	return parsed.UnixMilli(), nil
}
