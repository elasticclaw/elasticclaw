package workflowv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CronRunHistory is one row of cron history for a v2 workflow, matching the
// shape and semantics of v1's workflow_runs table.
type CronRunHistory struct {
	ID            string                 `json:"id"`
	TenantID      string                 `json:"tenant_id"`
	WorkspaceName string                 `json:"workspace_name"`
	WorkflowName  string                 `json:"workflow_name"`
	TriggerType   string                 `json:"trigger_type"`
	Status        string                 `json:"status"`
	Result        string                 `json:"result"`
	ClawID        string                 `json:"claw_id"`
	V2RunID       string                 `json:"v2_run_id"`
	RunContext    map[string]interface{} `json:"run_context"`
	CreatedAt     time.Time              `json:"created_at"`
	FinishedAt    *time.Time             `json:"finished_at,omitempty"`
}

// RecordCronRunStarted inserts a new cron history row in the 'running' state.
// It returns the generated cron run ID.
func (s *Store) RecordCronRunStarted(ctx context.Context, tenantID, workspaceName, workflowName, triggerType string, runContext map[string]interface{}) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("workflow v2 store is not configured")
	}
	id := uuid.NewString()
	now := s.now().UTC().UnixMilli()
	runContextJSON, err := marshalRunContext(runContext)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workflow_v2_cron_runs(id, tenant_id, workspace_name, workflow_name, trigger_type, status, run_context, created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		id, tenantID, workspaceName, workflowName, triggerType, "running", runContextJSON, now)
	if err != nil {
		return "", fmt.Errorf("record cron run started: %w", err)
	}
	return id, nil
}

// RecordCronRunSkipped inserts a cron history row for a skipped tick.
func (s *Store) RecordCronRunSkipped(ctx context.Context, tenantID, workspaceName, workflowName, triggerType string, runContext map[string]interface{}) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("workflow v2 store is not configured")
	}
	id := uuid.NewString()
	now := s.now().UTC().UnixMilli()
	runContextJSON, err := marshalRunContext(runContext)
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workflow_v2_cron_runs(id, tenant_id, workspace_name, workflow_name, trigger_type, status, result, run_context, created_at, finished_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, tenantID, workspaceName, workflowName, triggerType, "skipped", "skipped", runContextJSON, now, now)
	if err != nil {
		return "", fmt.Errorf("record cron run skipped: %w", err)
	}
	return id, nil
}

// UpdateCronRunForV2Run links a cron history row to its v2 run and claw.
func (s *Store) UpdateCronRunForV2Run(ctx context.Context, cronRunID, v2RunID, clawID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(cronRunID) == "" {
		return fmt.Errorf("cron run id is required")
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_v2_cron_runs SET v2_run_id=?, claw_id=?, updated_at=?
		WHERE id=?`, v2RunID, clawID, s.now().UTC().UnixMilli(), cronRunID)
	if err != nil {
		return fmt.Errorf("update cron run: %w", err)
	}
	return nil
}

// FinishCronRunByV2RunID marks a cron history row as finished based on the v2
// run's terminal status. It is a no-op if no matching row exists.
func (s *Store) FinishCronRunByV2RunID(ctx context.Context, v2RunID string, runStatus RunStatus) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(v2RunID) == "" {
		return fmt.Errorf("v2 run id is required")
	}
	status, result := cronHistoryStatus(runStatus)
	now := s.now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_v2_cron_runs SET status=?, result=?, finished_at=?
		WHERE v2_run_id=? AND status='running'`, status, result, now, v2RunID)
	if err != nil {
		return fmt.Errorf("finish cron run: %w", err)
	}
	return nil
}

// FinishCronRunByClawID marks a cron history row as finished by claw ID. It is
// a no-op if no matching row exists.
func (s *Store) FinishCronRunByClawID(ctx context.Context, clawID string, runStatus RunStatus) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(clawID) == "" {
		return fmt.Errorf("claw id is required")
	}
	status, result := cronHistoryStatus(runStatus)
	now := s.now().UTC().UnixMilli()
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_v2_cron_runs SET status=?, result=?, finished_at=?
		WHERE claw_id=? AND status='running'`, status, result, now, clawID)
	if err != nil {
		return fmt.Errorf("finish cron run by claw: %w", err)
	}
	return nil
}

// FailCronRun marks a running cron history row as failed and records the
// failure reason in its run_context.
func (s *Store) FailCronRun(ctx context.Context, cronRunID, reason string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(cronRunID) == "" {
		return fmt.Errorf("cron run id is required")
	}
	now := s.now().UTC().UnixMilli()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("fail cron run: %w", err)
	}
	defer tx.Rollback()
	var runContextJSON string
	if err := tx.QueryRowContext(ctx, `SELECT run_context FROM workflow_v2_cron_runs WHERE id=? AND status='running'`, cronRunID).Scan(&runContextJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tx.Commit()
		}
		return fmt.Errorf("fail cron run: %w", err)
	}
	runContext, err := unmarshalRunContext(runContextJSON)
	if err != nil {
		return fmt.Errorf("fail cron run: %w", err)
	}
	runContext["failure_reason"] = reason
	updatedRunContextJSON, err := marshalRunContext(runContext)
	if err != nil {
		return fmt.Errorf("fail cron run: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_v2_cron_runs SET status='failed', result='failure', finished_at=?, run_context=?
		WHERE id=? AND status='running'`, now, updatedRunContextJSON, cronRunID)
	if err != nil {
		return fmt.Errorf("fail cron run: %w", err)
	}
	return tx.Commit()
}

// ListCronRunHistory returns cron history rows for a workflow, newest first.
func (s *Store) ListCronRunHistory(ctx context.Context, tenantID, workspaceName, workflowName string, limit int) ([]CronRunHistory, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, tenant_id, workspace_name, workflow_name, trigger_type, status, result, claw_id, v2_run_id, run_context, created_at, finished_at
		FROM workflow_v2_cron_runs
		WHERE tenant_id=? AND workspace_name=? AND workflow_name=?
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, tenantID, workspaceName, workflowName, limit)
	if err != nil {
		return nil, fmt.Errorf("list cron run history: %w", err)
	}
	defer rows.Close()
	var result []CronRunHistory
	for rows.Next() {
		h, err := scanCronRunHistory(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list cron run history: %w", err)
	}
	return result, nil
}

// GetCronRun returns a single cron history row by ID, or nil if not found.
func (s *Store) GetCronRun(ctx context.Context, tenantID, workspaceName, workflowName, cronRunID string) (*CronRunHistory, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, workspace_name, workflow_name, trigger_type, status, result, claw_id, v2_run_id, run_context, created_at, finished_at
		FROM workflow_v2_cron_runs
		WHERE tenant_id=? AND workspace_name=? AND workflow_name=? AND id=?`, tenantID, workspaceName, workflowName, cronRunID)
	h, err := scanCronRunHistory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get cron run: %w", err)
	}
	return &h, nil
}

func scanCronRunHistory(row scanner) (CronRunHistory, error) {
	var h CronRunHistory
	var runContextJSON string
	var created, finished int64
	err := row.Scan(&h.ID, &h.TenantID, &h.WorkspaceName, &h.WorkflowName, &h.TriggerType, &h.Status, &h.Result,
		&h.ClawID, &h.V2RunID, &runContextJSON, &created, &finished)
	if err != nil {
		return CronRunHistory{}, err
	}
	if runContextJSON != "" && runContextJSON != "{}" {
		_ = json.Unmarshal([]byte(runContextJSON), &h.RunContext)
	}
	h.CreatedAt = time.UnixMilli(created).UTC()
	if finished > 0 {
		t := time.UnixMilli(finished).UTC()
		h.FinishedAt = &t
	}
	return h, nil
}

func cronHistoryStatus(runStatus RunStatus) (status, result string) {
	switch runStatus {
	case RunCompleted:
		return "completed", "success"
	case RunCancelled:
		return "canceled", "canceled"
	default:
		return "failed", "failure"
	}
}

func marshalRunContext(runContext map[string]interface{}) (string, error) {
	if runContext == nil {
		return "{}", nil
	}
	b, err := json.Marshal(runContext)
	if err != nil {
		return "{}", fmt.Errorf("marshal run context: %w", err)
	}
	return string(b), nil
}

func unmarshalRunContext(runContextJSON string) (map[string]interface{}, error) {
	if strings.TrimSpace(runContextJSON) == "" || runContextJSON == "{}" {
		return map[string]interface{}{}, nil
	}
	var runContext map[string]interface{}
	if err := json.Unmarshal([]byte(runContextJSON), &runContext); err != nil {
		return nil, fmt.Errorf("unmarshal run context: %w", err)
	}
	if runContext == nil {
		return map[string]interface{}{}, nil
	}
	return runContext, nil
}
