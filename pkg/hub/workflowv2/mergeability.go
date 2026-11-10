package workflowv2

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

// ReconcileMergeability records a known GitHub observation. Unknown observations
// leave the last known state intact and are retried by the next reconcile cycle.
func (s *Store) ReconcileMergeability(ctx context.Context, target DeliveryTarget,
	verified typesv2.VerifiedPullRequest) (EventResult, error) {
	state := verified.MergeableState
	if verified.State != "open" || verified.BaseSHA == "" || state == "" || state == "unknown" {
		return EventResult{}, nil
	}
	if verified.Repository != target.Repository || verified.Number != target.Number || verified.HeadSHA != target.HeadSHA {
		return EventResult{}, fmt.Errorf("mergeability observation does not match delivery target")
	}
	// Webhook and reconciler fetches race. The CAS below only guards writes
	// after the local read, so an observation fetched before the persisted one
	// must be dropped explicitly or it would overwrite newer state.
	observedAt := verified.Provenance.ObservedAt
	if observedAt.IsZero() {
		observedAt = verified.VerifiedAt
	}
	var observed int64
	if !observedAt.IsZero() {
		observed = observedAt.UnixMilli()
	}
	var previousHead, previousBase, previousState string
	var previousObserved int64
	err := s.db.QueryRowContext(ctx, `SELECT mergeable_head_sha,base_sha,mergeable_state,mergeable_observed_at
		FROM workflow_v2_delivery_prs WHERE id=? AND run_id=? AND active=1 AND registered_only=0`,
		target.PRID, target.RunID).Scan(&previousHead, &previousBase, &previousState, &previousObserved)
	if err != nil {
		return EventResult{}, err
	}
	if observed < previousObserved {
		return EventResult{}, nil
	}
	sameTuple := previousHead == verified.HeadSHA && previousBase == verified.BaseSHA && previousState == state
	kind := "pull_request.mergeability_updated"
	switch state {
	case "dirty":
		kind = "pull_request.conflicted"
	case "behind":
		kind = "pull_request.behind"
	default:
		if previousState == "dirty" || previousState == "behind" {
			kind = "pull_request.mergeable"
		}
	}
	if sameTuple && kind == "pull_request.mergeability_updated" {
		// A confirmed clean tuple can only owe a dropped pull_request.mergeable.
		kind = "pull_request.mergeable"
	}
	digest := sha256.Sum256([]byte(target.RunID + "\x00" + target.PRID + "\x00" + verified.HeadSHA + "\x00" + verified.BaseSHA + "\x00" + state))
	eventID := fmt.Sprintf("github:mergeability:%x", digest)
	// A tuple may recur after another state. Refresh facts in that case, but
	// never fire the same mergeability transition twice for the same tuple.
	// An observation a suspended run deferred only counts while it is still
	// queued or once replayed: if superseded or cleared by a push, its
	// transition never ran. The replay refreshes the payload to the current
	// tuple, so it is tracked by a receipt on the queued event.
	var recorded, fired, observedTuple int
	if err := s.db.QueryRowContext(ctx, `WITH tuple AS (SELECT e.id,
			EXISTS(SELECT 1 FROM workflow_v2_deferred_events d WHERE d.run_id=e.run_id AND d.event_id=e.id)
			OR NOT EXISTS(SELECT 1 FROM workflow_v2_event_receipts r WHERE r.run_id=e.run_id AND r.event_id=e.id
				AND r.reason=?)
			OR EXISTS(SELECT 1 FROM workflow_v2_event_receipts r WHERE r.run_id=e.run_id AND r.event_id=e.id
				AND r.reason=?) AS fired
		FROM workflow_v2_events e WHERE e.run_id=? AND e.kind=?
			AND json_extract(e.payload_json,'$.pull_request.id')=?
			AND json_extract(e.payload_json,'$.pull_request.head_sha')=?
			AND json_extract(e.payload_json,'$.pull_request.base_sha')=?
			AND json_extract(e.payload_json,'$.pull_request.mergeable_state')=?)
		SELECT (SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND id=?),
			(SELECT COUNT(*) FROM tuple WHERE fired), (SELECT COUNT(*) FROM tuple)`,
		deferredUntilRecoveryReason, replayedDeferredReason, target.RunID, kind, target.PRID, verified.HeadSHA, verified.BaseSHA, state,
		target.RunID, eventID).Scan(&recorded, &fired, &observedTuple); err != nil {
		return EventResult{}, err
	}
	// Confirming the persisted tuple fires nothing, unless a suspended run
	// deferred its transition and then dropped it (a close/reopen or a push
	// back to a head whose other observation stayed unknown leaves the tuple
	// in place). It only advances the watermark so an older observation of a
	// different tuple cannot land afterwards.
	if sameTuple && (fired > 0 || observedTuple == 0) {
		_, err := s.db.ExecContext(ctx, `UPDATE workflow_v2_delivery_prs SET mergeable_observed_at=?
			WHERE id=? AND run_id=? AND mergeable_observed_at<?`, observed, target.PRID, target.RunID, observed)
		return EventResult{}, err
	}
	if fired > 0 || kind == "pull_request.mergeability_updated" {
		eventID, kind = uuid.NewString(), "pull_request.mergeability_updated"
	} else if recorded > 0 {
		eventID = uuid.NewString()
	}
	deferKey := mergeabilityDeferKey(target.PRID)
	if kind == "pull_request.mergeability_updated" {
		deferKey = mergeabilityUpdatedDeferKey(target.PRID)
	}
	return s.applyEventWithMutation(ctx, target.RunID, EventInput{
		ID: eventID, MessageID: eventID, Kind: kind, AttemptID: target.AttemptID,
		Producer: ProducerSourceControl, Provenance: verified.Provenance, observation: true,
		deferKey: deferKey,
	}, func(ctx context.Context, tx *sql.Tx, input *EventInput) error {
		result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_delivery_prs
			SET mergeable_head_sha=?,base_sha=?,mergeable_state=?,mergeable_observed_at=?
			WHERE id=? AND run_id=? AND active=1 AND registered_only=0 AND state='open' AND current_head_sha=?
			AND mergeable_head_sha=? AND base_sha=? AND mergeable_state=? AND mergeable_observed_at<=?`,
			verified.HeadSHA, verified.BaseSHA, state, observed, target.PRID, target.RunID, verified.HeadSHA,
			previousHead, previousBase, previousState, observed)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return fmt.Errorf("delivery changed while reconciling mergeability")
		}
		if err := supersedeDeferredMergeability(ctx, tx, target.RunID, target.PRID, kind, state); err != nil {
			return err
		}
		summary, err := deliveryFactsFrom(ctx, tx, target.RunID)
		if err != nil {
			return err
		}
		input.Payload = map[string]interface{}{"pull_request": map[string]interface{}{
			"id": target.PRID, "repository": target.Repository, "number": target.Number,
			"url": verified.URL, "state": verified.State,
			"head_sha": verified.HeadSHA, "base_sha": verified.BaseSHA,
			"mergeable_state": state, "previous_mergeable_state": previousState,
		}}
		input.Facts = map[string]interface{}{
			"pull_request.id": target.PRID, "pull_request.repository": target.Repository,
			"pull_request.number": target.Number, "pull_request.url": verified.URL,
			"pull_request.head_sha": verified.HeadSHA, "pull_request.state": verified.State,
			"pull_request.head_changed": false, "pull_request.mergeable_state": state,
			"pull_request.base_sha":   verified.BaseSHA,
			"delivery.any_conflicted": summary["any_conflicted"], "delivery.any_behind": summary["any_behind"],
		}
		return nil
	})
}

// supersedeDeferredMergeability keeps the queues of a suspended run consistent
// with a new mergeability observation. A conflicted, behind or mergeable event
// supersedes a queued pull_request.mergeability_updated. A
// pull_request.mergeability_updated never fires those transitions, so it only
// drops a queued one whose state class the PR has left: a PR that went from
// clean to blocked is still mergeable, and the reconciler will not emit
// pull_request.mergeable again for it.
func supersedeDeferredMergeability(ctx context.Context, tx *sql.Tx, runID, prID, kind, state string) error {
	if kind != "pull_request.mergeability_updated" {
		return clearDeferredEvent(ctx, tx, runID, mergeabilityUpdatedDeferKey(prID))
	}
	current := "pull_request.mergeable"
	switch state {
	case "dirty":
		current = "pull_request.conflicted"
	case "behind":
		current = "pull_request.behind"
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM workflow_v2_deferred_events WHERE run_id=? AND defer_key=?
		AND event_id IN (SELECT id FROM workflow_v2_events WHERE run_id=? AND kind<>?)`,
		runID, mergeabilityDeferKey(prID), runID, current)
	return err
}
