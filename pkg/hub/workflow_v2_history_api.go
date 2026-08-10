package hub

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

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
	s.queryActivityMessages(w, r, clawID)
}

// handleWorkflowV2AttemptLogs handles GET /api/v2/workflow-runs/{runId}/attempts/{attemptId}/logs
// and returns agent activity logs for the specified attempt.
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
	s.queryActivityMessages(w, r, clawID)
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

	query := `SELECT id, claw_id, tenant_id, role, content, COALESCE(format,''), created_at
		FROM messages
		WHERE claw_id = ? AND tenant_id = ? AND role = 'activity'`
	args := []interface{}{clawID, tenantFromCtx(r)}
	if from != "" {
		query += ` AND created_at > ?`
		if parsed := parseTimeCursor(from); parsed != nil {
			args = append(args, *parsed)
		} else {
			args = append(args, from)
		}
	}
	if to != "" {
		query += ` AND created_at < ?`
		if parsed := parseTimeCursor(to); parsed != nil {
			args = append(args, *parsed)
		} else {
			args = append(args, to)
		}
	}
	if before != "" {
		query += ` AND created_at < ?`
		if parsed := parseTimeCursor(before); parsed != nil {
			args = append(args, *parsed)
		} else {
			args = append(args, before)
		}
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
