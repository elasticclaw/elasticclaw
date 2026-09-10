package workflowv2

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// RunAttemptHistory is one row of v2 workflow history. It joins a run with a
// single attempt because each attempt has its own claw_id and therefore its own
// agent logs. Multiple attempts for the same run appear as multiple rows.
type RunAttemptHistory struct {
	RunID             string               `json:"run_id"`
	AttemptID         string               `json:"attempt_id"`
	AttemptNumber     int                  `json:"attempt_number"`
	TenantID          string               `json:"tenant_id"`
	WorkspaceName     string               `json:"workspace_name"`
	WorkflowName      string               `json:"workflow_name"`
	State             string               `json:"state"`
	DisplayPhase      typesv2.DisplayPhase `json:"display_phase"`
	RunStatus         RunStatus            `json:"run_status"`
	WaitingReason     string               `json:"waiting_reason,omitempty"`
	AttemptStatus     string               `json:"attempt_status"`
	TriggerType       string               `json:"trigger_type"`
	ClawID            string               `json:"claw_id,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	StartedAt         time.Time            `json:"started_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
	FinishedAt        *time.Time           `json:"finished_at,omitempty"`
	AttemptFinishedAt *time.Time           `json:"attempt_finished_at,omitempty"`
}

// ListRunAttempts returns run history for a given workflow, one row per attempt.
func (s *Store) ListRunAttempts(ctx context.Context, tenantID, workspaceName, workflowName string, limit int, statusFilter string) ([]RunAttemptHistory, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	args := []interface{}{tenantID, workspaceName, workflowName}
	statusClause := ""
	if strings.TrimSpace(statusFilter) != "" {
		statusClause = " AND r.status = ?"
		args = append(args, statusFilter)
	}
	query := fmt.Sprintf(`SELECT r.id,a.id,a.number,r.tenant_id,r.workspace_name,r.workflow_name,
		r.state,r.display_phase,r.status,r.waiting_reason,a.status,r.trigger_type,
		a.claw_id,r.created_at,a.started_at,r.updated_at,r.finished_at,a.finished_at
		FROM workflow_v2_runs r
		JOIN workflow_v2_attempts a ON a.run_id = r.id
		WHERE r.tenant_id=? AND r.workspace_name=? AND r.workflow_name=?%s
		ORDER BY r.updated_at DESC, r.id DESC, a.number DESC
		LIMIT ?`, statusClause)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflow v2 run attempts: %w", err)
	}
	defer rows.Close()
	var result []RunAttemptHistory
	for rows.Next() {
		row, err := scanRunAttemptHistory(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow v2 run attempts: %w", err)
	}
	return result, nil
}

// ListAttempts returns all attempts for a run, ordered by attempt number.
func (s *Store) ListAttempts(ctx context.Context, runID string) ([]Attempt, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,run_id,claw_id,number,status,started_at,heartbeat_at,finished_at
		FROM workflow_v2_attempts WHERE run_id=? ORDER BY number ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list workflow v2 attempts: %w", err)
	}
	defer rows.Close()
	var result []Attempt
	for rows.Next() {
		var a Attempt
		var started, heartbeat, finished int64
		if err := rows.Scan(&a.ID, &a.RunID, &a.ClawID, &a.Number, &a.Status, &started, &heartbeat, &finished); err != nil {
			return nil, fmt.Errorf("scan workflow v2 attempt: %w", err)
		}
		a.StartedAt = time.UnixMilli(started).UTC()
		if heartbeat > 0 {
			a.HeartbeatAt = time.UnixMilli(heartbeat).UTC()
		}
		if finished > 0 {
			a.FinishedAt = time.UnixMilli(finished).UTC()
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow v2 attempts: %w", err)
	}
	return result, nil
}

// GetAttempt returns a single attempt by ID.
func (s *Store) GetAttempt(ctx context.Context, attemptID string) (Attempt, error) {
	if s == nil || s.db == nil {
		return Attempt{}, fmt.Errorf("workflow v2 store is not configured")
	}
	var a Attempt
	var started, heartbeat, finished int64
	err := s.db.QueryRowContext(ctx, `SELECT id,run_id,claw_id,number,status,started_at,heartbeat_at,finished_at
		FROM workflow_v2_attempts WHERE id=?`, attemptID).Scan(
		&a.ID, &a.RunID, &a.ClawID, &a.Number, &a.Status, &started, &heartbeat, &finished)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Attempt{}, err
		}
		return Attempt{}, fmt.Errorf("get workflow v2 attempt: %w", err)
	}
	a.StartedAt = time.UnixMilli(started).UTC()
	if heartbeat > 0 {
		a.HeartbeatAt = time.UnixMilli(heartbeat).UTC()
	}
	if finished > 0 {
		a.FinishedAt = time.UnixMilli(finished).UTC()
	}
	return a, nil
}

func scanRunAttemptHistory(row scanner) (RunAttemptHistory, error) {
	var h RunAttemptHistory
	var phase, runStatus, attemptStatus, triggerType string
	var created, started, updated, runFinished, attemptFinished int64
	err := row.Scan(&h.RunID, &h.AttemptID, &h.AttemptNumber, &h.TenantID, &h.WorkspaceName, &h.WorkflowName,
		&h.State, &phase, &runStatus, &h.WaitingReason, &attemptStatus, &triggerType,
		&h.ClawID, &created, &started, &updated, &runFinished, &attemptFinished)
	if err != nil {
		return RunAttemptHistory{}, fmt.Errorf("scan workflow v2 run attempt history: %w", err)
	}
	if phase != "" {
		h.DisplayPhase = typesv2.DisplayPhase(phase)
	}
	if runStatus != "" {
		h.RunStatus = RunStatus(runStatus)
	}
	h.AttemptStatus = attemptStatus
	h.TriggerType = triggerType
	h.CreatedAt = time.UnixMilli(created).UTC()
	h.StartedAt = time.UnixMilli(started).UTC()
	h.UpdatedAt = time.UnixMilli(updated).UTC()
	if runFinished > 0 {
		t := time.UnixMilli(runFinished).UTC()
		h.FinishedAt = &t
	}
	if attemptFinished > 0 {
		t := time.UnixMilli(attemptFinished).UTC()
		h.AttemptFinishedAt = &t
	}
	return h, nil
}
