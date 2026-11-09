package workflowv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

var errDeferredEventGone = errors.New("deferred event already replayed")

// deferSubject is the queue key of a deferred observation; see EventInput.deferKey.
func (input EventInput) deferSubject() string {
	if input.deferKey != "" {
		return input.deferKey
	}
	return input.Kind
}

// deferEvent queues a listened observation recorded by a suspended run. Only
// the latest observation per subject is kept: an older one would fire a
// transition on state the provider has already moved past.
func deferEvent(ctx context.Context, tx *sql.Tx, runID string, input EventInput, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_deferred_events(run_id,defer_key,event_id,seq,created_at)
		VALUES(?,?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM workflow_v2_deferred_events WHERE run_id=?),?)
		ON CONFLICT(run_id,defer_key) DO UPDATE SET event_id=excluded.event_id,seq=excluded.seq,created_at=excluded.created_at`,
		runID, input.deferSubject(), input.ID, runID, now.UnixMilli())
	return err
}

// clearDeferredEvent drops the queued observation for a subject. A newer
// observation supersedes it even when the workflow does not listen on the newer
// kind, and a subject that no longer applies (PR head moved, PR closed) must
// not replay stale state on recovery.
func clearDeferredEvent(ctx context.Context, tx *sql.Tx, runID, key string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM workflow_v2_deferred_events WHERE run_id=? AND defer_key=?`, runID, key)
	return err
}

// ReplayDeferredEvents fires the transitions that suspended runs deferred, for
// every run that is active again. The transition that resumes a run replays
// its queue immediately; this sweep covers failures and recoveries outside the
// event path. Queues of finished runs are discarded.
func (s *Store) ReplayDeferredEvents(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM workflow_v2_deferred_events WHERE run_id IN
		(SELECT id FROM workflow_v2_runs WHERE status NOT IN ('active','suspended'))`); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT d.run_id FROM workflow_v2_deferred_events d
		JOIN workflow_v2_runs r ON r.id=d.run_id WHERE r.status='active' ORDER BY d.run_id`)
	if err != nil {
		return err
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var errs []error
	for _, runID := range runIDs {
		if err := s.replayDeferredEvents(ctx, runID); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", runID, err))
		}
	}
	return errors.Join(errs...)
}

// replayDeferredEvents re-applies each queued observation as a fresh event.
// Facts were already written when the observation was recorded, so the replay
// carries only the original payload for transition matching, with aggregates
// refreshed to the current delivery.
func (s *Store) replayDeferredEvents(ctx context.Context, runID string) error {
	type pending struct {
		key, eventID, kind, producer, provenanceJSON, payloadJSON string
	}
	rows, err := s.db.QueryContext(ctx, `SELECT d.defer_key,e.id,e.kind,e.producer,e.provenance_json,e.payload_json
		FROM workflow_v2_deferred_events d JOIN workflow_v2_events e ON e.id=d.event_id
		WHERE d.run_id=? ORDER BY d.seq`, runID)
	if err != nil {
		return err
	}
	var queue []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.key, &item.eventID, &item.kind, &item.producer, &item.provenanceJSON, &item.payloadJSON); err != nil {
			rows.Close()
			return err
		}
		queue = append(queue, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range queue {
		var payload map[string]interface{}
		if err := json.Unmarshal([]byte(item.payloadJSON), &payload); err != nil {
			return fmt.Errorf("decode deferred event %s payload: %w", item.eventID, err)
		}
		var provenance typesv2.EvidenceProvenance
		if err := json.Unmarshal([]byte(item.provenanceJSON), &provenance); err != nil {
			return fmt.Errorf("decode deferred event %s provenance: %w", item.eventID, err)
		}
		id := uuid.NewString()
		result, err := s.applyEventWithMutation(ctx, runID, EventInput{
			ID: id, MessageID: id, Kind: item.kind, Producer: Producer(item.producer), Provenance: provenance,
			Payload: payload, observation: true, deferKey: item.key, deferInvariant: true,
		}, func(ctx context.Context, tx *sql.Tx, input *EventInput) error {
			result, err := tx.ExecContext(ctx, `DELETE FROM workflow_v2_deferred_events
				WHERE run_id=? AND defer_key=? AND event_id=?`, runID, item.key, item.eventID)
			if err != nil {
				return err
			}
			if changed, err := result.RowsAffected(); err != nil {
				return err
			} else if changed != 1 {
				return errDeferredEventGone
			}
			if err := refreshDeferredDelivery(ctx, tx, runID, input); err != nil {
				return err
			}
			if err := refreshDeferredMergeability(ctx, tx, runID, input); err != nil {
				return err
			}
			return refreshDeferredReviewFeedback(ctx, tx, runID, input)
		})
		if errors.Is(err, errDeferredEventGone) {
			continue
		}
		if err != nil {
			return fmt.Errorf("replay deferred event %s: %w", item.eventID, err)
		}
		if result.Run.Status != RunActive {
			return nil
		}
	}
	return s.suspendOnBrokenInvariant(ctx, runID)
}

// hasDeferredEvents reports whether a run has queued observations to replay.
func hasDeferredEvents(ctx context.Context, tx *sql.Tx, runID string) (bool, error) {
	var queued bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_v2_deferred_events WHERE run_id=?)`, runID).Scan(&queued)
	return queued, err
}

// suspendOnBrokenInvariant runs the invariant check that a replay postponed:
// once the queue drained, an active run whose state invariant no longer holds
// is suspended exactly as a live event would have left it.
func (s *Store) suspendOnBrokenInvariant(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, workflowYAML, err := getRunForUpdate(ctx, tx, runID)
	if err != nil || stored.Status != RunActive {
		return err
	}
	workflow, err := typesv2.ParseAndValidateWorkflow([]byte(workflowYAML))
	if err != nil {
		return fmt.Errorf("load pinned workflow: %w", err)
	}
	invariant := workflow.Workflow.States[stored.State].Invariant
	if len(invariant) == 0 {
		return nil
	}
	facts, err := loadFacts(ctx, tx, runID)
	if err != nil {
		return err
	}
	if valid, err := typesv2.MatchPredicate(invariant, facts); err != nil || valid {
		return err
	}
	reason := fmt.Sprintf("state invariant no longer holds for %s after replaying deferred events", stored.State)
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='suspended',waiting_reason=?,updated_at=?
		WHERE id=? AND state_version=? AND status='active'`, reason, s.now().UTC().UnixMilli(), runID, stored.StateVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// refreshDeferredDelivery replaces the queued delivery summary with the current
// one. Payload values override facts in guard evaluation, and a sibling PR may
// have changed state after the observation was queued.
func refreshDeferredDelivery(ctx context.Context, tx *sql.Tx, runID string, input *EventInput) error {
	if _, ok := input.Payload["delivery"]; !ok {
		return nil
	}
	summary, err := deliveryFactsFrom(ctx, tx, runID)
	if err != nil {
		return err
	}
	input.Payload["delivery"] = summary
	return nil
}

// refreshDeferredMergeability replaces the queued mergeability of a PR with the
// persisted one while the PR is still on the queued head. A queued
// pull_request.mergeable survives a later clean -> blocked change; replaying
// its queue-time state would override the current fact in guard evaluation.
func refreshDeferredMergeability(ctx context.Context, tx *sql.Tx, runID string, input *EventInput) error {
	pullRequest, ok := input.Payload["pull_request"].(map[string]interface{})
	if !ok {
		return nil
	}
	prID, _ := pullRequest["id"].(string)
	headSHA, _ := pullRequest["head_sha"].(string)
	if _, ok := pullRequest["mergeable_state"]; !ok || prID == "" || headSHA == "" {
		return nil
	}
	var baseSHA, mergeableState string
	err := tx.QueryRowContext(ctx, `SELECT base_sha,mergeable_state FROM workflow_v2_delivery_prs
		WHERE id=? AND run_id=? AND current_head_sha=? AND mergeable_head_sha=?`,
		prID, runID, headSHA, headSHA).Scan(&baseSHA, &mergeableState)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	pullRequest["base_sha"], pullRequest["mergeable_state"] = baseSHA, mergeableState
	return nil
}

// refreshDeferredReviewFeedback replaces the queued feedback content with the
// current review.feedback fact when both describe the same provider object. An
// edit made while suspended is recorded as review.feedback.reconciled, which
// does not supersede the queued received event; replaying the original payload
// would override the refreshed fact in guard evaluation.
func refreshDeferredReviewFeedback(ctx context.Context, tx *sql.Tx, runID string, input *EventInput) error {
	if input.Kind != "review.feedback.received" && input.Kind != "review.feedback.reconciled" {
		return nil
	}
	review, ok := input.Payload["review"].(map[string]interface{})
	if !ok {
		return nil
	}
	queued, _ := review["feedback"].(map[string]interface{})
	var valueJSON, provenanceJSON string
	err := tx.QueryRowContext(ctx, `SELECT value_json,provenance_json FROM workflow_v2_facts
		WHERE run_id=? AND fact_key='review.feedback'`, runID).Scan(&valueJSON, &provenanceJSON)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var current map[string]interface{}
	if err := json.Unmarshal([]byte(valueJSON), &current); err != nil {
		return fmt.Errorf("decode current review feedback: %w", err)
	}
	var currentProvenance typesv2.EvidenceProvenance
	if err := json.Unmarshal([]byte(provenanceJSON), &currentProvenance); err != nil {
		return fmt.Errorf("decode current review feedback provenance: %w", err)
	}
	queuedID, currentID := input.Provenance.ExternalID, currentProvenance.ExternalID
	if queuedID == "" || current["kind"] != queued["kind"] ||
		!(reviewFeedbackExternalIDsMatch(currentID, queuedID) || reviewFeedbackExternalIDsMatch(queuedID, currentID)) {
		return nil
	}
	review["feedback"] = current
	return nil
}
