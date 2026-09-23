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
// and returns agent activity logs for the run's current attempt.
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
	s.queryWorkflowRunLogs(w, r, runID, clawID)
}

// handleWorkflowV2AttemptLogs handles GET /api/v2/workflow-runs/{runId}/attempts/{attemptId}/logs
// and returns agent activity logs and state transitions for the specified attempt.
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
	s.queryWorkflowRunLogs(w, r, runID, clawID)
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

// queryActivityMessages returns activity messages for a claw, applying the same
// filtering and authorization used by /api/messages/{clawID}/activity.
func (s *Server) queryActivityMessages(w http.ResponseWriter, r *http.Request, clawID string) {
	if !s.canViewMessages(w, r, tenantFromCtx(r), clawID) {
		return
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

	query := `SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), COALESCE(user_login,''), created_at
		FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role = 'activity'`
	args := []interface{}{clawID, tenantFromCtx(r)}
	if fromParsed != nil {
		query += ` AND created_at > ?`
		args = append(args, *fromParsed)
	}
	if toParsed != nil {
		query += ` AND created_at < ?`
		args = append(args, *toParsed)
	}
	if beforeParsed != nil {
		query += ` AND created_at < ?`
		args = append(args, *beforeParsed)
	}
	if order == "desc" {
		query += ` ORDER BY created_at DESC LIMIT ?`
	} else {
		query += ` ORDER BY created_at ASC LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
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
	jsonOK(w, msgs)
}

// queryWorkflowRunLogs returns agent activity messages merged with workflow state
// transition entries for a v2 run. This lets the UI logs view show when the run
// entered each state, not just agent.task messages.
func (s *Server) queryWorkflowRunLogs(w http.ResponseWriter, r *http.Request, runID, clawID string) {
	if !s.canViewMessages(w, r, tenantFromCtx(r), clawID) {
		return
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

	query := `SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), COALESCE(user_login,''), created_at
		FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role = 'activity'`
	args := []interface{}{clawID, tenantFromCtx(r)}
	if fromParsed != nil {
		query += ` AND created_at > ?`
		args = append(args, *fromParsed)
	}
	if toParsed != nil {
		query += ` AND created_at < ?`
		args = append(args, *toParsed)
	}
	if beforeParsed != nil {
		query += ` AND created_at < ?`
		args = append(args, *beforeParsed)
	}
	if order == "desc" {
		query += ` ORDER BY created_at DESC LIMIT ?`
	} else {
		query += ` ORDER BY created_at ASC LIMIT ?`
	}
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
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
	transRows, err := s.db.Query(`
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
		msgs = append(msgs, types.HubMessage{
			ID:        "transition-" + id,
			ClawID:    clawID,
			TenantID:  tenantFromCtx(r),
			Role:      "state",
			Content:   fmt.Sprintf("Entered state: %s (from %s)", toState, fromState),
			Format:    "workflow:state",
			CreatedAt: time.UnixMilli(createdAtMs).UTC(),
		})
	}
	if err := transRows.Err(); err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch state transitions")
		return
	}

	// Merge effect and agent-task lifecycle lines (including exec.run
	// stdout/stderr receipts) so the log timeline explains what the workflow
	// did, not just which states it passed through.
	tenantID := tenantFromCtx(r)
	msgs, err = s.appendWorkflowV2EffectLogs(msgs, runID, clawID, tenantID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch effect lifecycle")
		return
	}
	msgs, err = s.appendWorkflowV2AgentTaskLogs(msgs, runID, clawID, tenantID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch agent task lifecycle")
		return
	}
	msgs, err = s.appendWorkflowV2ExecOutcomeLogs(msgs, runID, clawID, tenantID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "fetch exec outcomes")
		return
	}

	if order == "desc" {
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].CreatedAt.After(msgs[j].CreatedAt) })
	} else {
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].CreatedAt.Before(msgs[j].CreatedAt) })
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

// appendWorkflowV2EffectLogs merges one log line per effect lifecycle point:
// planned effects that were never claimed, each attempt start, and each attempt
// finish with its receipt (exec.run stdout/stderr, exit code, dependency update
// results) embedded as a structured WorkflowEffectEvent.
func (s *Server) appendWorkflowV2EffectLogs(msgs []types.HubMessage, runID, clawID, tenantID string) ([]types.HubMessage, error) {
	rows, err := s.db.Query(`
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
			if attemptCount == 0 {
				msgs, err = appendWorkflowEffectLogMessage(msgs, "effect-"+effectID, clawID, tenantID,
					fmt.Sprintf("%s effect planned (waiting for worker)", kind),
					types.WorkflowEffectEvent{Kind: kind, Phase: "planned", Status: effectStatus, DefinitionPath: definitionPath, Command: command},
					time.UnixMilli(effectCreated))
				if err != nil {
					return nil, err
				}
			}
			continue
		}
		attempt := int(attemptNumber.Int64)
		msgs, err = appendWorkflowEffectLogMessage(msgs, fmt.Sprintf("effect-%s-attempt-%d-start", effectID, attempt), clawID, tenantID,
			fmt.Sprintf("%s effect started (attempt %d)", kind, attempt),
			types.WorkflowEffectEvent{Kind: kind, Phase: "started", Status: "running", Attempt: attempt,
				DefinitionPath: definitionPath, Command: command},
			time.UnixMilli(attemptStarted.Int64))
		if err != nil {
			return nil, err
		}
		if attemptFinished.Int64 <= 0 {
			continue
		}
		event := types.WorkflowEffectEvent{Kind: kind, Phase: "finished", Status: attemptStatus.String, Attempt: attempt,
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
// timeline even though effect receipts only record the assignment.
func (s *Server) appendWorkflowV2ExecOutcomeLogs(msgs []types.HubMessage, runID, clawID, tenantID string) ([]types.HubMessage, error) {
	rows, err := s.db.Query(`
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
	event := types.WorkflowEffectEvent{Kind: effectKind, Phase: "finished"}
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

	failed := event.Succeeded == nil || !*event.Succeeded
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
// terminal outcome with its reason.
func (s *Server) appendWorkflowV2AgentTaskLogs(msgs []types.HubMessage, runID, clawID, tenantID string) ([]types.HubMessage, error) {
	rows, err := s.db.Query(`
		SELECT id, status, instructions, terminal_reason, created_at, finished_at
		FROM workflow_v2_agent_tasks WHERE run_id=? ORDER BY created_at, id`, runID)
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
		msgs, err = appendWorkflowEffectLogMessage(msgs, "agent-task-"+taskID+"-assigned", clawID, tenantID,
			"agent task assigned",
			types.WorkflowEffectEvent{Kind: "agent.task", Phase: "assigned", Status: status, Instructions: instructions},
			time.UnixMilli(created))
		if err != nil {
			return nil, err
		}
		if finished <= 0 {
			continue
		}
		content := "agent task " + status
		if terminalReason != "" {
			content += ": " + terminalReason
		}
		msgs, err = appendWorkflowEffectLogMessage(msgs, "agent-task-"+taskID+"-finish", clawID, tenantID,
			content,
			types.WorkflowEffectEvent{Kind: "agent.task", Phase: "finished", Status: status, TerminalReason: terminalReason},
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
