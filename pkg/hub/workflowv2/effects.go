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

type EffectStatus string

const (
	EffectPlanned         EffectStatus = "planned"
	EffectRunning         EffectStatus = "running"
	EffectSucceeded       EffectStatus = "succeeded"
	EffectRetryableFailed EffectStatus = "retryable_failed"
	EffectPermanentFailed EffectStatus = "permanent_failed"
	EffectUnknown         EffectStatus = "unknown"
	EffectCancelled       EffectStatus = "cancelled"
)

type Effect struct {
	ID             string                 `json:"id"`
	RunID          string                 `json:"run_id"`
	EffectKey      string                 `json:"effect_key"`
	Kind           string                 `json:"kind"`
	Payload        map[string]interface{} `json:"payload"`
	Status         EffectStatus           `json:"status"`
	AttemptCount   int                    `json:"attempt_count"`
	LeaseOwner     string                 `json:"lease_owner,omitempty"`
	LeaseExpiresAt time.Time              `json:"lease_expires_at,omitempty"`
	NextAttemptAt  time.Time              `json:"next_attempt_at,omitempty"`
	LastError      string                 `json:"last_error,omitempty"`
}

type EffectClaim struct {
	Effect    Effect `json:"effect"`
	AttemptID string `json:"attempt_id"`
}

// ClaimEffect leases one ready effect. Expired running effects are first moved
// to unknown and are deliberately not retried: a reconciler must determine
// whether the external side effect happened before it is safe to continue.
func (s *Store) ClaimEffect(ctx context.Context, worker string, lease time.Duration) (*EffectClaim, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("workflow v2 store is not configured")
	}
	worker = strings.TrimSpace(worker)
	if worker == "" {
		return nil, fmt.Errorf("worker is required")
	}
	if lease <= 0 {
		return nil, fmt.Errorf("lease duration must be positive")
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := expireEffectLeases(ctx, tx, now); err != nil {
		return nil, err
	}
	row := tx.QueryRowContext(ctx, `SELECT e.id,e.run_id,e.effect_key,e.kind,e.payload_json,e.status,e.attempt_count,e.next_attempt_at,e.last_error
		FROM workflow_v2_effects e JOIN workflow_v2_runs r ON r.id=e.run_id
		WHERE e.status IN ('planned','retryable_failed') AND e.next_attempt_at<=? AND r.status='active'
		ORDER BY e.next_attempt_at,e.created_at,e.id LIMIT 1`, now.UnixMilli())
	var effect Effect
	var payloadJSON string
	var status string
	var nextAttempt int64
	if err := row.Scan(&effect.ID, &effect.RunID, &effect.EffectKey, &effect.Kind, &payloadJSON,
		&status, &effect.AttemptCount, &nextAttempt, &effect.LastError); errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(payloadJSON), &effect.Payload); err != nil {
		return nil, fmt.Errorf("decode effect %s payload: %w", effect.ID, err)
	}

	expires := now.Add(lease)
	result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET status='running',attempt_count=attempt_count+1,
		lease_owner=?,lease_expires_at=?,updated_at=?
		WHERE id=? AND status=? AND attempt_count=? AND next_attempt_at<=?`,
		worker, expires.UnixMilli(), now.UnixMilli(), effect.ID, status, effect.AttemptCount, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, nil
	}
	effect.Status = EffectRunning
	effect.AttemptCount++
	effect.LeaseOwner = worker
	effect.LeaseExpiresAt = expires
	if nextAttempt > 0 {
		effect.NextAttemptAt = time.UnixMilli(nextAttempt).UTC()
	}
	attemptID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_effect_attempts(
		id,effect_id,number,status,request_json,started_at) VALUES(?,?,?,?,?,?)`,
		attemptID, effect.ID, effect.AttemptCount, string(EffectRunning), payloadJSON, now.UnixMilli()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &EffectClaim{Effect: effect, AttemptID: attemptID}, nil
}

type CompleteEffectRequest struct {
	EffectID   string
	AttemptID  string
	Worker     string
	Status     EffectStatus
	Receipt    map[string]interface{}
	Error      string
	RetryAfter time.Duration
}

type ResolveUnknownEffectRequest struct {
	EffectID   string
	Status     EffectStatus
	Receipt    map[string]interface{}
	Error      string
	RetryAfter time.Duration
}

// CompleteEffect records the durable outcome of the currently leased attempt.
func (s *Store) CompleteEffect(ctx context.Context, req CompleteEffectRequest) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	switch req.Status {
	case EffectSucceeded, EffectRetryableFailed, EffectPermanentFailed, EffectUnknown, EffectCancelled:
	default:
		return fmt.Errorf("invalid effect completion status %q", req.Status)
	}
	if strings.TrimSpace(req.EffectID) == "" || strings.TrimSpace(req.AttemptID) == "" || strings.TrimSpace(req.Worker) == "" {
		return fmt.Errorf("effect id, attempt id, and worker are required")
	}
	receiptJSON, err := marshalObject(req.Receipt)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	nextAttempt := int64(0)
	if req.Status == EffectRetryableFailed {
		if req.RetryAfter < 0 {
			return fmt.Errorf("retry delay cannot be negative")
		}
		nextAttempt = now.Add(req.RetryAfter).UnixMilli()
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attemptNumber int
	err = tx.QueryRowContext(ctx, `SELECT a.number FROM workflow_v2_effect_attempts a
		JOIN workflow_v2_effects e ON e.id=a.effect_id
		WHERE a.id=? AND a.effect_id=? AND a.status='running' AND e.status='running' AND e.lease_owner=? AND e.lease_expires_at>=?`,
		req.AttemptID, req.EffectID, req.Worker, now.UnixMilli()).Scan(&attemptNumber)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("effect attempt is not actively leased by worker")
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effect_attempts SET status=?,receipt_json=?,error=?,finished_at=?
		WHERE id=? AND effect_id=? AND number=?`, string(req.Status), receiptJSON, req.Error, now.UnixMilli(),
		req.AttemptID, req.EffectID, attemptNumber); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET status=?,lease_owner='',lease_expires_at=0,
		next_attempt_at=?,receipt_json=?,last_error=?,updated_at=? WHERE id=? AND status='running' AND lease_owner=?`,
		string(req.Status), nextAttempt, receiptJSON, req.Error, now.UnixMilli(), req.EffectID, req.Worker)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("effect lease changed while completing attempt")
	}
	return tx.Commit()
}

// ResolveUnknownEffect records the result of authoritative reconciliation
// after a worker lease expired. Retrying is opt-in so an uncertain external
// write can never be replayed merely because time passed.
func (s *Store) ResolveUnknownEffect(ctx context.Context, req ResolveUnknownEffectRequest) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	switch req.Status {
	case EffectSucceeded, EffectRetryableFailed, EffectPermanentFailed, EffectCancelled:
	default:
		return fmt.Errorf("invalid reconciled effect status %q", req.Status)
	}
	if strings.TrimSpace(req.EffectID) == "" {
		return fmt.Errorf("effect id is required")
	}
	if req.RetryAfter < 0 {
		return fmt.Errorf("retry delay cannot be negative")
	}
	receiptJSON, err := marshalObject(req.Receipt)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	nextAttempt := int64(0)
	if req.Status == EffectRetryableFailed {
		nextAttempt = now.Add(req.RetryAfter).UnixMilli()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_effects SET status=?,next_attempt_at=?,
		receipt_json=?,last_error=?,updated_at=? WHERE id=? AND status='unknown'`,
		string(req.Status), nextAttempt, receiptJSON, req.Error, now.UnixMilli(), req.EffectID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("effect is not awaiting reconciliation")
	}
	return nil
}

func expireEffectLeases(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effect_attempts SET status='unknown',
		error='worker lease expired; outcome requires reconciliation',finished_at=?
		WHERE status='running' AND effect_id IN (
			SELECT id FROM workflow_v2_effects WHERE status='running' AND lease_expires_at<?
		)`, now.UnixMilli(), now.UnixMilli()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET status='unknown',lease_owner='',lease_expires_at=0,
		last_error='worker lease expired; outcome requires reconciliation',updated_at=?
		WHERE status='running' AND lease_expires_at<?`, now.UnixMilli(), now.UnixMilli())
	return err
}
