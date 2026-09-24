package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// handleWorkflowV2Runs handles GET /api/v2/workspaces/{workspace}/workflows/{workflow}/runs
// and returns v2 workflow run history, one row per attempt.
func (s *Server) handleWorkflowV2Runs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	workspace := strings.TrimSpace(r.PathValue("workspace"))
	workflow := strings.TrimSpace(r.PathValue("workflow"))
	if workspace == "" || workflow == "" {
		jsonError(w, http.StatusBadRequest, "workspace and workflow are required")
		return
	}

	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}
	statusFilter := r.URL.Query().Get("status")

	var exists int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT 1 FROM workflow_v2_runs WHERE tenant_id=? AND workspace_name=? AND workflow_name=? LIMIT 1`,
		tenantFromCtx(r), workspace, workflow).Scan(&exists); err != nil && !errors.Is(err, sql.ErrNoRows) {
		jsonError(w, http.StatusInternalServerError, "list workflow v2 runs")
		return
	}

	rows, err := workflowv2.NewStore(s.db).ListRunAttempts(r.Context(), tenantFromCtx(r), workspace, workflow, limit, statusFilter)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "list workflow v2 runs")
		return
	}
	jsonOK(w, map[string]interface{}{
		"runs":  rows,
		"count": len(rows),
	})
}

// handleWorkflowV2RunAttempts handles GET /api/v2/workflow-runs/{runId}/attempts
// and returns all attempts for a v2 workflow run.
func (s *Server) handleWorkflowV2RunAttempts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	if runID == "" {
		jsonError(w, http.StatusBadRequest, "run id is required")
		return
	}

	var tenantID string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT tenant_id FROM workflow_v2_runs WHERE id=?`, runID).Scan(&tenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "workflow run not found")
			return
		}
		jsonError(w, http.StatusInternalServerError, "lookup workflow run")
		return
	}
	if tenantID != tenantFromCtx(r) {
		jsonError(w, http.StatusNotFound, "workflow run not found")
		return
	}

	attempts, err := workflowv2.NewStore(s.db).ListAttempts(r.Context(), runID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "list workflow v2 attempts")
		return
	}
	jsonOK(w, map[string]interface{}{
		"attempts": attempts,
		"count":    len(attempts),
	})
}

// handleWorkflowV2RunLogs handles GET /api/v2/workflow-runs/{runId}/logs
// and returns the run's complete record: the current attempt's activity
// messages merged with the run's lifecycle records (transitions, effects,
// agent tasks, exec outcomes).
func (s *Server) handleWorkflowV2RunLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	if runID == "" {
		jsonError(w, http.StatusBadRequest, "run id is required")
		return
	}
	clawID, ok := s.lookupV2RunClawID(r.Context(), w, r, runID)
	if !ok {
		return
	}
	s.queryWorkflowRunLogs(w, r, runID, clawID, "")
}

// handleWorkflowV2AttemptLogs handles GET /api/v2/workflow-runs/{runId}/attempts/{attemptId}/logs
// and returns the logs for the specified attempt: activity messages for its
// claw, plus the run's lifecycle records from that attempt's session (agent
// tasks are scoped by attempt; transitions, effects, and exec outcomes by the
// attempt's lifetime).
func (s *Server) handleWorkflowV2AttemptLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	runID := strings.TrimSpace(r.PathValue("runId"))
	attemptID := strings.TrimSpace(r.PathValue("attemptId"))
	if runID == "" || attemptID == "" {
		jsonError(w, http.StatusBadRequest, "run id and attempt id are required")
		return
	}
	clawID, ok := s.lookupV2AttemptClawID(r.Context(), w, r, runID, attemptID)
	if !ok {
		return
	}
	s.queryWorkflowRunLogs(w, r, runID, clawID, attemptID)
}

// lookupV2RunClawID returns the current attempt's claw_id for a v2 run, or
// writes an HTTP error and returns false if the run is not found or accessible.
func (s *Server) lookupV2RunClawID(ctx context.Context, w http.ResponseWriter, r *http.Request, runID string) (string, bool) {
	var clawID, tenantID string
	err := s.db.QueryRowContext(ctx, `SELECT r.tenant_id, a.claw_id
		FROM workflow_v2_runs r
		JOIN workflow_v2_attempts a ON a.id = r.current_attempt_id
		WHERE r.id=?`, runID).Scan(&tenantID, &clawID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "workflow run not found or has no current attempt")
		} else {
			jsonError(w, http.StatusInternalServerError, "lookup workflow run")
		}
		return "", false
	}
	if tenantID != tenantFromCtx(r) {
		jsonError(w, http.StatusNotFound, "workflow run not found")
		return "", false
	}
	return clawID, true
}

// lookupV2AttemptClawID returns the claw_id for a specific v2 attempt, or
// writes an HTTP error and returns false if not found or accessible.
func (s *Server) lookupV2AttemptClawID(ctx context.Context, w http.ResponseWriter, r *http.Request, runID, attemptID string) (string, bool) {
	var clawID, tenantID string
	err := s.db.QueryRowContext(ctx, `SELECT r.tenant_id, a.claw_id
		FROM workflow_v2_runs r
		JOIN workflow_v2_attempts a ON a.run_id = r.id
		WHERE r.id=? AND a.id=?`, runID, attemptID).Scan(&tenantID, &clawID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			jsonError(w, http.StatusNotFound, "workflow attempt not found")
		} else {
			jsonError(w, http.StatusInternalServerError, "lookup workflow attempt")
		}
		return "", false
	}
	if tenantID != tenantFromCtx(r) {
		jsonError(w, http.StatusNotFound, "workflow attempt not found")
		return "", false
	}
	return clawID, true
}

// queryWorkflowV2TransitionsForClaw returns synthetic state-transition messages
// for any v2 workflow run whose attempt uses the given claw. The messages have
// role "state" so they can be rendered in the main chat timeline alongside
// user/claw messages.
func (s *Server) queryWorkflowV2TransitionsForClaw(ctx context.Context, tenantID, clawID string) ([]types.HubMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.from_state, t.to_state, t.created_at
		FROM workflow_v2_transitions t
		JOIN workflow_v2_attempts a ON a.run_id = t.run_id
		JOIN workflow_v2_runs r ON r.id = a.run_id
		WHERE a.claw_id = ? AND r.tenant_id = ?
		ORDER BY t.created_at ASC`, clawID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []types.HubMessage
	for rows.Next() {
		var id, fromState, toState string
		var createdAtMs int64
		if err := rows.Scan(&id, &fromState, &toState, &createdAtMs); err != nil {
			return nil, err
		}
		msgs = append(msgs, types.HubMessage{
			ID:        "transition-" + id,
			ClawID:    clawID,
			TenantID:  tenantID,
			Role:      "state",
			Content:   fmt.Sprintf("Entered state: %s (from %s)", toState, fromState),
			Format:    "workflow:state",
			CreatedAt: time.UnixMilli(createdAtMs).UTC(),
		})
	}
	return msgs, rows.Err()
}

// queryWorkflowRunLogs returns agent activity messages merged with workflow state
// transition entries for a v2 run. This lets the UI logs view show when the run
// entered each state, not just agent.task messages.
func (s *Server) queryWorkflowRunLogs(w http.ResponseWriter, r *http.Request, runID, clawID, attemptID string) {
	if !s.canViewMessages(w, r, tenantFromCtx(r), clawID) {
		return
	}

	// Attempt-scoped views only include lifecycle records from that attempt's
	// session; run-level views include the run's complete record.
	var window *attemptWindow
	if attemptID != "" {
		resolved, err := s.lookupV2AttemptWindow(r.Context(), runID, attemptID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "lookup workflow attempt window")
			return
		}
		window = &resolved
	}

	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	before := r.URL.Query().Get("before")
	limit := parsePositiveLimit(r, 200, 500)
	order := strings.ToLower(r.URL.Query().Get("order"))
	if order != "desc" {
		order = "asc"
	}

	fromParsed, ok := requireTimeCursor(w, from)
	if !ok {
		return
	}
	toParsed, ok := requireTimeCursor(w, to)
	if !ok {
		return
	}
	beforeParsed, ok := requireTimeCursor(w, before)
	if !ok {
		return
	}
	beforeID := strings.TrimSpace(r.URL.Query().Get("before_id"))
	cursor := logCursor{from: fromParsed, to: toParsed, before: beforeParsed, beforeID: beforeID}

	query := `SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), COALESCE(user_login,''), created_at
		FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role = 'activity'`
	args := []interface{}{clawID, tenantFromCtx(r)}
	if window != nil {
		// A retried attempt may reuse the same claw, so the activity stream is
		// scoped to the attempt's lifetime as well, not just the claw.
		query += ` AND created_at >= ?`
		args = append(args, time.UnixMilli(window.start).UTC())
		if window.end > 0 {
			query += ` AND created_at <= ?`
			args = append(args, time.UnixMilli(window.end).UTC())
		}
	}
	if fromParsed != nil {
		query += ` AND created_at > ?`
		args = append(args, *fromParsed)
	}
	if toParsed != nil {
		query += ` AND created_at < ?`
		args = append(args, *toParsed)
	}
	if beforeParsed != nil {
		if beforeID != "" {
			// Compound cursor: (created_at, id) is a total order, so a page
			// boundary that lands inside a same-millisecond group does not
			// skip the group's remaining rows on the next page.
			query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
			args = append(args, *beforeParsed, *beforeParsed, beforeID)
		} else {
			query += ` AND created_at < ?`
			args = append(args, *beforeParsed)
		}
	}
	if order == "desc" {
		query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	} else {
		query += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch activity logs")
		return
	}
	defer rows.Close()
	msgs, err := scanHubMessages(rows)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch activity logs")
		return
	}
	if msgs == nil {
		msgs = []types.HubMessage{}
	}

	// Merge in state transition markers so the UI can show when each state was entered.
	transRows, err := s.db.QueryContext(r.Context(), `
		SELECT id, from_state, to_state, created_at
		FROM workflow_v2_transitions
		WHERE run_id = ?
		ORDER BY created_at ASC`, runID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch state transitions")
		return
	}
	defer transRows.Close()
	for transRows.Next() {
		var id, fromState, toState string
		var createdAtMs int64
		if err := transRows.Scan(&id, &fromState, &toState, &createdAtMs); err != nil {
			jsonError(w, http.StatusInternalServerError, "fetch state transitions")
			return
		}
		createdAt := time.UnixMilli(createdAtMs).UTC()
		if !lifecycleVisible(window, cursor, createdAt, "transition-"+id) {
			continue
		}
		msgs = append(msgs, types.HubMessage{
			ID:        "transition-" + id,
			ClawID:    clawID,
			TenantID:  tenantFromCtx(r),
			Role:      "state",
			Content:   fmt.Sprintf("Entered state: %s (from %s)", toState, fromState),
			Format:    "workflow:state",
			CreatedAt: createdAt,
		})
	}
	if err := transRows.Err(); err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch state transitions")
		return
	}

	// Merge effect and agent-task lifecycle lines (including exec.run
	// stdout/stderr receipts) so the log timeline explains what the workflow
	// did, not just which states it passed through. Lifecycle lines honor the
	// same cursor predicates as the activity rows so the merged page is a
	// stable slice of one timeline.
	tenantID := tenantFromCtx(r)
	msgs, err = s.appendWorkflowV2EffectLogs(r.Context(), msgs, runID, clawID, tenantID, window, cursor)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch effect lifecycle")
		return
	}
	msgs, err = s.appendWorkflowV2AgentTaskLogs(r.Context(), msgs, runID, clawID, tenantID, attemptID, cursor)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch agent task lifecycle")
		return
	}
	msgs, err = s.appendWorkflowV2ExecOutcomeLogs(r.Context(), msgs, runID, clawID, tenantID, window, cursor)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch exec outcomes")
		return
	}

	if order == "desc" {
		sort.Slice(msgs, func(i, j int) bool {
			return msgs[i].CreatedAt.After(msgs[j].CreatedAt) ||
				(msgs[i].CreatedAt.Equal(msgs[j].CreatedAt) && msgs[i].ID > msgs[j].ID)
		})
	} else {
		sort.Slice(msgs, func(i, j int) bool {
			return msgs[i].CreatedAt.Before(msgs[j].CreatedAt) ||
				(msgs[i].CreatedAt.Equal(msgs[j].CreatedAt) && msgs[i].ID < msgs[j].ID)
		})
	}
	if len(msgs) > limit {
		msgs = msgs[:limit]
	}

	jsonOK(w, msgs)
}

// requireTimeCursor parses a non-empty cursor and returns a 400 response if it
// cannot be parsed as an RFC3339/RFC3339Nano time. An empty cursor returns
// (nil, true).
func requireTimeCursor(w http.ResponseWriter, raw string) (*time.Time, bool) {
	if raw == "" {
		return nil, true
	}
	parsed := parseTimeCursor(raw)
	if parsed == nil {
		jsonError(w, http.StatusBadRequest, "invalid cursor: "+raw)
		return nil, false
	}
	return parsed, true
}

// attemptWindow bounds an attempt-scoped logs view to one run attempt's
// lifetime, in unix milliseconds. End is zero while the attempt is still
// active (open-ended). Effects and exec outcomes have no attempt column —
// they are dispatched to and executed by a specific attempt's bridge, so the
// window attributes them to the session that was alive at their timestamp.
type attemptWindow struct {
	start int64
	end   int64
}

func (w attemptWindow) contains(ts int64) bool {
	if ts < w.start {
		return false
	}
	return w.end == 0 || ts <= w.end
}

// logCursor mirrors the activity query's from/to/before predicates so merged
// lifecycle records paginate on the same timeline as activity rows instead of
// repeating on every page. from and to are exclusive window bounds; before is
// the page cursor: a strict upper bound on created_at, optionally compound
// with beforeID so a page boundary inside a same-millisecond group cannot
// skip the group's remaining rows.
type logCursor struct {
	from     *time.Time
	to       *time.Time
	before   *time.Time
	beforeID string
}

// excludes reports whether a merged row (ts, id) falls outside the cursor.
// For the compound before cursor, "strictly older than the cursor row" is
// ts < before OR (ts == before AND id < beforeID) in the (created_at, id)
// total order — matching the SQL predicate exactly.
func (c logCursor) excludes(ts time.Time, id string) bool {
	if c.from != nil && !ts.After(*c.from) {
		return true
	}
	if c.to != nil && !ts.Before(*c.to) {
		return true
	}
	if c.before != nil && !ts.Before(*c.before) {
		if c.beforeID == "" || !ts.Equal(*c.before) {
			return true
		}
		return id >= c.beforeID
	}
	return false
}

// lifecycleVisible reports whether a merged lifecycle line (ts, id) falls
// inside the attempt window and the pagination cursor.
func lifecycleVisible(window *attemptWindow, cursor logCursor, ts time.Time, id string) bool {
	if window != nil && !window.contains(ts.UnixMilli()) {
		return false
	}
	return !cursor.excludes(ts, id)
}

// lookupV2AttemptWindow returns the [started_at, finished_at] lifetime of a
// v2 run attempt. finished_at is zero while the attempt is still active.
func (s *Server) lookupV2AttemptWindow(ctx context.Context, runID, attemptID string) (attemptWindow, error) {
	var window attemptWindow
	err := s.db.QueryRowContext(ctx, `SELECT started_at, finished_at FROM workflow_v2_attempts WHERE id=? AND run_id=?`,
		attemptID, runID).Scan(&window.start, &window.end)
	if err != nil {
		return attemptWindow{}, err
	}
	return window, nil
}

// appendWorkflowV2EffectLogs merges one log line per effect lifecycle point:
// planned effects that were never claimed, each attempt start, and each attempt
// finish with its receipt (exec.run stdout/stderr, exit code, dependency update
// results) embedded as a structured WorkflowEffectEvent. When window is
// non-nil only lifecycle points inside that run-attempt's session are merged;
// lines outside the pagination cursor are skipped.
func (s *Server) appendWorkflowV2EffectLogs(ctx context.Context, msgs []types.HubMessage,
	runID, clawID, tenantID string, window *attemptWindow, cursor logCursor) ([]types.HubMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id, e.kind, e.definition_path, e.payload_json, e.status, e.attempt_count, e.created_at,
			a.number, a.status, a.started_at, a.finished_at, a.receipt_json, a.error
		FROM workflow_v2_effects e
		LEFT JOIN workflow_v2_effect_attempts a ON a.effect_id=e.id
		WHERE e.run_id=? ORDER BY e.created_at, e.id, a.number`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var effectID, kind, definitionPath, payloadJSON, effectStatus string
		var attemptCount, effectCreated int64
		var attemptNumber sql.NullInt64
		var attemptStatus sql.NullString
		var attemptStarted, attemptFinished sql.NullInt64
		var receiptJSON, attemptError sql.NullString
		if err := rows.Scan(&effectID, &kind, &definitionPath, &payloadJSON, &effectStatus, &attemptCount, &effectCreated,
			&attemptNumber, &attemptStatus, &attemptStarted, &attemptFinished, &receiptJSON, &attemptError); err != nil {
			return nil, err
		}
		command := effectCommand(payloadJSON)
		if !attemptNumber.Valid {
			if attemptCount == 0 && lifecycleVisible(window, cursor, time.UnixMilli(effectCreated).UTC(), "effect-"+effectID) {
				msgs, err = appendWorkflowEffectLogMessage(msgs, "effect-"+effectID, clawID, tenantID,
					fmt.Sprintf("%s effect planned (waiting for worker)", kind),
					types.WorkflowEffectEvent{Kind: kind, Phase: types.WorkflowEffectPhasePlanned, Status: effectStatus, DefinitionPath: definitionPath, Command: command},
					time.UnixMilli(effectCreated))
				if err != nil {
					return nil, err
				}
			}
			continue
		}
		attempt := int(attemptNumber.Int64)
		startID := fmt.Sprintf("effect-%s-attempt-%d-start", effectID, attempt)
		if lifecycleVisible(window, cursor, time.UnixMilli(attemptStarted.Int64).UTC(), startID) {
			msgs, err = appendWorkflowEffectLogMessage(msgs, startID, clawID, tenantID,
				fmt.Sprintf("%s effect started (attempt %d)", kind, attempt),
				types.WorkflowEffectEvent{Kind: kind, Phase: types.WorkflowEffectPhaseStarted, Status: "running", Attempt: attempt,
					DefinitionPath: definitionPath, Command: command},
				time.UnixMilli(attemptStarted.Int64))
			if err != nil {
				return nil, err
			}
		}
		if attemptFinished.Int64 <= 0 {
			continue
		}
		finishID := fmt.Sprintf("effect-%s-attempt-%d-finish", effectID, attempt)
		if !lifecycleVisible(window, cursor, time.UnixMilli(attemptFinished.Int64).UTC(), finishID) {
			continue
		}
		if dispatchedEffectKinds[kind] && attemptStatus.String == string(workflowv2.EffectSucceeded) {
			// A succeeded attempt for these kinds only records that a durable
			// task was dispatched (receipt: task_id, message_id). The
			// authoritative completion arrives separately — as the projected
			// exec outcome event or the agent task lifecycle — so this line is
			// labeled "dispatched" and never rendered as a second completion.
			msgs, err = appendWorkflowEffectLogMessage(msgs, finishID, clawID, tenantID,
				fmt.Sprintf("%s effect dispatched (attempt %d)", kind, attempt),
				types.WorkflowEffectEvent{Kind: kind, Phase: types.WorkflowEffectPhaseDispatched, Status: types.WorkflowEffectPhaseDispatched, Attempt: attempt,
					DefinitionPath: definitionPath, TaskID: receiptTaskID(receiptJSON.String)},
				time.UnixMilli(attemptFinished.Int64))
			if err != nil {
				return nil, err
			}
			continue
		}
		event := types.WorkflowEffectEvent{Kind: kind, Phase: types.WorkflowEffectPhaseFinished, Status: attemptStatus.String, Attempt: attempt,
			DefinitionPath: definitionPath, Command: command}
		content := fmt.Sprintf("%s effect %s (attempt %d)", kind, attemptStatus.String, attempt)
		receiptError := applyReceiptToEffectEvent(receiptJSON.String, &event)
		if event.Succeeded != nil && *event.Succeeded {
			content = fmt.Sprintf("%s effect succeeded (attempt %d)", kind, attempt)
			if event.ExitCode != nil {
				content = fmt.Sprintf("%s effect succeeded (attempt %d, exit code %d)", kind, attempt, *event.ExitCode)
			}
		} else if receiptError != "" {
			content += ": " + receiptError
		} else if attemptError.String != "" {
			content += ": " + attemptError.String
		}
		msgs, err = appendWorkflowEffectLogMessage(msgs, fmt.Sprintf("effect-%s-attempt-%d-finish", effectID, attempt), clawID, tenantID,
			content, event, time.UnixMilli(attemptFinished.Int64))
		if err != nil {
			return nil, err
		}
	}
	return msgs, rows.Err()
}

// appendWorkflowV2ExecOutcomeLogs merges command-outcome lines from accepted
// exec.run/dependency.update completion events. The projected
// exec.last_run.* / exec.dependency_update.* event facts carry the receipt
// (stdout/stderr/exit code), so each execution's output appears in the
// timeline even though effect receipts only record the assignment. When
// window is non-nil only outcomes received inside that run-attempt's session
// are merged; lines outside the pagination cursor are skipped.
func (s *Server) appendWorkflowV2ExecOutcomeLogs(ctx context.Context, msgs []types.HubMessage,
	runID, clawID, tenantID string, window *attemptWindow, cursor logCursor) ([]types.HubMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, facts_json, received_at FROM workflow_v2_events
		WHERE run_id=? AND disposition='accepted' AND kind IN (
			'exec.run.completed','exec.run.failed','dependency.update.completed','dependency.update.failed')
		ORDER BY received_at, id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var eventID, kind, factsJSON string
		var received int64
		if err := rows.Scan(&eventID, &kind, &factsJSON, &received); err != nil {
			return nil, err
		}
		if !lifecycleVisible(window, cursor, time.UnixMilli(received).UTC(), "event-"+eventID) {
			continue
		}
		event, content, ok := execOutcomeEvent(kind, factsJSON)
		if !ok {
			continue
		}
		msgs, err = appendWorkflowEffectLogMessage(msgs, "event-"+eventID, clawID, tenantID, content, event, time.UnixMilli(received))
		if err != nil {
			return nil, err
		}
	}
	return msgs, rows.Err()
}

// execOutcomeEvent builds the structured log event for a command completion
// event from its projected exec.* facts.
func execOutcomeEvent(kind, factsJSON string) (types.WorkflowEffectEvent, string, bool) {
	var facts map[string]interface{}
	if err := json.Unmarshal([]byte(factsJSON), &facts); err != nil {
		return types.WorkflowEffectEvent{}, "", false
	}
	effectKind := "exec.run"
	factPrefix := "exec.last_run."
	if strings.HasPrefix(kind, "dependency.update.") {
		effectKind = "dependency.update"
		factPrefix = "exec.dependency_update."
	}
	factString := func(key string) string {
		value, _ := facts[factPrefix+key].(string)
		return value
	}
	event := types.WorkflowEffectEvent{Kind: effectKind, Phase: types.WorkflowEffectPhaseFinished}
	if succeeded, ok := facts[factPrefix+"succeeded"].(bool); ok {
		event.Succeeded = &succeeded
	}
	exitCode := -1
	hasExitCode := false
	if value, ok := facts[factPrefix+"exit_code"].(float64); ok {
		exitCode, hasExitCode = int(value), true
		event.ExitCode = &exitCode
	}
	event.Stdout = factString("stdout")
	event.Stderr = factString("stderr")
	event.Error = factString("error")

	// The event kind is authoritative: the bridge emits .completed or .failed
	// based on the outcome. The projected succeeded fact, when present,
	// overrides for emitters that set one but not the other.
	failed := strings.HasSuffix(kind, ".failed")
	if event.Succeeded != nil {
		failed = !*event.Succeeded
	}
	status := "succeeded"
	content := effectKind + " completed"
	if failed {
		status = "failed"
		content = effectKind + " failed"
	}
	if hasExitCode {
		content = fmt.Sprintf("%s (exit code %d)", content, exitCode)
	}
	if failed && event.Error != "" {
		content += ": " + event.Error
	}
	event.Status = status
	return event, content, true
}

// appendWorkflowV2AgentTaskLogs merges agent-task lifecycle lines: the task
// assignment (with the instructions the workflow gave the agent) and the
// terminal outcome with its reason. Agent tasks carry an attempt_id, so when
// attemptID is non-empty the lines are scoped to that exact attempt; lines
// outside the pagination cursor are skipped.
func (s *Server) appendWorkflowV2AgentTaskLogs(ctx context.Context, msgs []types.HubMessage,
	runID, clawID, tenantID, attemptID string, cursor logCursor) ([]types.HubMessage, error) {
	query := `
		SELECT id, status, instructions, terminal_reason, created_at, finished_at
		FROM workflow_v2_agent_tasks WHERE run_id=?`
	args := []interface{}{runID}
	if attemptID != "" {
		query += ` AND attempt_id=?`
		args = append(args, attemptID)
	}
	query += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, status, instructions, terminalReason string
		var created, finished int64
		if err := rows.Scan(&taskID, &status, &instructions, &terminalReason, &created, &finished); err != nil {
			return nil, err
		}
		assignedID := "agent-task-" + taskID + "-assigned"
		if !cursor.excludes(time.UnixMilli(created).UTC(), assignedID) {
			// The assigned line carries the assignment-phase status; the task's
			// terminal status belongs to the finish line, so one failed task
			// never renders as two identical red "failed" lines.
			msgs, err = appendWorkflowEffectLogMessage(msgs, assignedID, clawID, tenantID,
				"agent task assigned",
				types.WorkflowEffectEvent{Kind: "agent.task", Phase: types.WorkflowEffectPhaseAssigned, Status: types.WorkflowEffectPhaseAssigned, Instructions: instructions},
				time.UnixMilli(created))
			if err != nil {
				return nil, err
			}
		}
		if finished <= 0 {
			continue
		}
		finishID := "agent-task-" + taskID + "-finish"
		if cursor.excludes(time.UnixMilli(finished).UTC(), finishID) {
			continue
		}
		content := "agent task " + status
		if terminalReason != "" {
			content += ": " + terminalReason
		}
		msgs, err = appendWorkflowEffectLogMessage(msgs, finishID, clawID, tenantID,
			content,
			types.WorkflowEffectEvent{Kind: "agent.task", Phase: types.WorkflowEffectPhaseFinished, Status: status, TerminalReason: terminalReason},
			time.UnixMilli(finished))
		if err != nil {
			return nil, err
		}
	}
	return msgs, rows.Err()
}

func appendWorkflowEffectLogMessage(msgs []types.HubMessage, id, clawID, tenantID, content string,
	event types.WorkflowEffectEvent, at time.Time) ([]types.HubMessage, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return append(msgs, types.HubMessage{
		ID: id, ClawID: clawID, TenantID: tenantID, Role: "effect",
		Content: content, Format: types.WorkflowEffectFormatPrefix + string(payload), CreatedAt: at.UTC(),
	}), nil
}

// effectCommand extracts the shell command from an exec.run effect payload so
// log consumers can see exactly what ran.
func effectCommand(payloadJSON string) string {
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return ""
	}
	command, _ := payload["command"].(string)
	return command
}

// dispatchedEffectKinds are effect kinds whose succeeded attempt only records
// that a durable task was handed off: exec.run and dependency.update complete
// via a separate control event, and agent.task via the task lifecycle. Their
// success lines are labeled "dispatched" (with the receipt's task_id) so one
// execution never renders as two completions.
var dispatchedEffectKinds = map[string]bool{
	typesv2.EffectExecRun:          true,
	typesv2.EffectDependencyUpdate: true,
	"agent.task":                   true,
}

// receiptTaskID extracts the durable task id from a dispatched effect's
// assignment receipt for correlation with the authoritative outcome lines.
func receiptTaskID(receiptJSON string) string {
	var receipt map[string]interface{}
	if err := json.Unmarshal([]byte(receiptJSON), &receipt); err != nil {
		return ""
	}
	taskID, _ := receipt["task_id"].(string)
	return taskID
}

// applyReceiptToEffectEvent copies known receipt fields (exec.run and
// dependency.update receipts) onto the structured log event and returns the
// receipt's human-readable error, if any.
func applyReceiptToEffectEvent(receiptJSON string, event *types.WorkflowEffectEvent) string {
	var receipt map[string]interface{}
	if err := json.Unmarshal([]byte(receiptJSON), &receipt); err != nil {
		return ""
	}
	if value, ok := receipt["stdout"].(string); ok {
		event.Stdout = value
	}
	if value, ok := receipt["stderr"].(string); ok {
		event.Stderr = value
	}
	if value, ok := receipt["succeeded"].(bool); ok {
		event.Succeeded = &value
	}
	if value, ok := receipt["exit_code"].(float64); ok {
		exitCode := int(value)
		event.ExitCode = &exitCode
	}
	errorMsg, _ := receipt["error"].(string)
	return errorMsg
}
