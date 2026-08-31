package workflowv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

// MaterializeCommandTask completes an exec.run or dependency.update effect and
// queues its typed assignment on the run's active attempt. The actual execution
// happens in the bridge; the effect is considered successfully materialized once
// the assignment is durably queued, and completion is handled as a separate
// control event so the outcome is never auto-retried by the effect worker.
func (s *Store) MaterializeCommandTask(ctx context.Context, effectID, effectAttemptID, worker string) (typesv2.ControlEnvelope, error) {
	if s == nil || s.db == nil {
		return typesv2.ControlEnvelope{}, fmt.Errorf("workflow v2 store is not configured")
	}
	if strings.TrimSpace(effectID) == "" || strings.TrimSpace(effectAttemptID) == "" || strings.TrimSpace(worker) == "" {
		return typesv2.ControlEnvelope{}, fmt.Errorf("effect id, effect attempt id, and worker are required")
	}

	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return typesv2.ControlEnvelope{}, err
	}
	defer tx.Rollback()

	var runID, runAttemptID, state, kind, payloadJSON string
	var stateVersion uint64
	err = tx.QueryRowContext(ctx, `SELECT e.run_id,r.current_attempt_id,r.state,r.state_version,e.kind,e.payload_json
		FROM workflow_v2_effects e JOIN workflow_v2_runs r ON r.id=e.run_id
		JOIN workflow_v2_effect_attempts ea ON ea.effect_id=e.id
		WHERE e.id=? AND ea.id=? AND ea.status='running' AND e.status='running' AND e.lease_owner=? AND e.lease_expires_at>=?
		AND r.status='active'`, effectID, effectAttemptID, worker, now.UnixMilli()).Scan(
		&runID, &runAttemptID, &state, &stateVersion, &kind, &payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return typesv2.ControlEnvelope{}, fmt.Errorf("command effect is not actively leased")
	}
	if err != nil {
		return typesv2.ControlEnvelope{}, err
	}
	if runAttemptID == "" {
		return typesv2.ControlEnvelope{}, fmt.Errorf("run %s has no active attempt", runID)
	}
	var activeAttempt int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM workflow_v2_attempts WHERE id=? AND run_id=? AND status='active'`,
		runAttemptID, runID).Scan(&activeAttempt); err != nil {
		return typesv2.ControlEnvelope{}, fmt.Errorf("load active run attempt: %w", err)
	}

	var taskID = uuid.NewString()
	var assignmentKind typesv2.ControlMessageKind
	switch kind {
	case typesv2.EffectExecRun:
		assignmentKind = typesv2.MessageExecRunAssign
		var cfg typesv2.ExecRunConfig
		if err := json.Unmarshal([]byte(payloadJSON), &cfg); err != nil {
			return typesv2.ControlEnvelope{}, fmt.Errorf("decode exec.run effect: %w", err)
		}
		if strings.TrimSpace(cfg.Command) == "" {
			return typesv2.ControlEnvelope{}, fmt.Errorf("exec.run effect command is required")
		}
	case typesv2.EffectDependencyUpdate:
		assignmentKind = typesv2.MessageDependencyUpdateAssign
		var cfg typesv2.DependencyUpdateConfig
		if err := json.Unmarshal([]byte(payloadJSON), &cfg); err != nil {
			return typesv2.ControlEnvelope{}, fmt.Errorf("decode dependency.update effect: %w", err)
		}
		if len(cfg.Ecosystems) == 0 {
			return typesv2.ControlEnvelope{}, fmt.Errorf("dependency.update effect ecosystems are required")
		}
	default:
		return typesv2.ControlEnvelope{}, fmt.Errorf("effect %s has kind %q, want exec.run or dependency.update", effectID, kind)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return typesv2.ControlEnvelope{}, fmt.Errorf("decode command effect payload: %w", err)
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return typesv2.ControlEnvelope{}, err
	}

	envelope := typesv2.ControlEnvelope{
		ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: uuid.NewString(), Kind: assignmentKind,
		RunID: runID, AttemptID: runAttemptID, TaskID: taskID, ExpectedStateVersion: &stateVersion,
		CausationID: effectID, SentAt: now, Payload: payloadBytes,
	}
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionHubToClaw); err != nil {
		return typesv2.ControlEnvelope{}, err
	}
	envelopeJSON, _ := json.Marshal(envelope)
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_control_outbox(
		message_id,run_id,attempt_id,task_id,kind,envelope_json,status,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,'pending',0,?,?)`, envelope.MessageID, runID, runAttemptID, taskID,
		string(envelope.Kind), string(envelopeJSON), now.UnixMilli(), now.UnixMilli()); err != nil {
		return typesv2.ControlEnvelope{}, err
	}

	receiptJSON, _ := json.Marshal(map[string]interface{}{"task_id": taskID, "message_id": envelope.MessageID})
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effect_attempts SET status='succeeded',receipt_json=?,finished_at=?
		WHERE id=? AND effect_id=? AND status='running'`, string(receiptJSON), now.UnixMilli(), effectAttemptID, effectID); err != nil {
		return typesv2.ControlEnvelope{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET status='succeeded',lease_owner='',lease_expires_at=0,
		receipt_json=?,last_error='',updated_at=? WHERE id=? AND status='running' AND lease_owner=?`,
		string(receiptJSON), now.UnixMilli(), effectID, worker)
	if err != nil {
		return typesv2.ControlEnvelope{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return typesv2.ControlEnvelope{}, fmt.Errorf("command effect lease changed while materializing")
	}
	if err := tx.Commit(); err != nil {
		return typesv2.ControlEnvelope{}, err
	}
	return envelope, nil
}
