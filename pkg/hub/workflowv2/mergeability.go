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
	var previousHead, previousBase, previousState string
	err := s.db.QueryRowContext(ctx, `SELECT mergeable_head_sha,base_sha,mergeable_state
		FROM workflow_v2_delivery_prs WHERE id=? AND run_id=? AND active=1 AND registered_only=0`,
		target.PRID, target.RunID).Scan(&previousHead, &previousBase, &previousState)
	if err != nil {
		return EventResult{}, err
	}
	if previousHead == verified.HeadSHA && previousBase == verified.BaseSHA && previousState == state {
		return EventResult{}, nil
	}
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
	digest := sha256.Sum256([]byte(target.RunID + "\x00" + target.PRID + "\x00" + verified.HeadSHA + "\x00" + verified.BaseSHA + "\x00" + state))
	eventID := fmt.Sprintf("github:mergeability:%x", digest)
	// A tuple may recur after another state. Refresh facts in that case, but
	// never fire the same mergeability transition twice for the same tuple.
	var seen int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND id=?`,
		target.RunID, eventID).Scan(&seen); err != nil {
		return EventResult{}, err
	}
	if seen > 0 || kind == "pull_request.mergeability_updated" {
		eventID, kind = uuid.NewString(), "pull_request.mergeability_updated"
	}
	return s.applyEventWithMutation(ctx, target.RunID, EventInput{
		ID: eventID, MessageID: eventID, Kind: kind, AttemptID: target.AttemptID,
		Producer: ProducerSourceControl, Provenance: verified.Provenance, observation: true,
		deferKey: "mergeability:" + target.PRID,
	}, func(ctx context.Context, tx *sql.Tx, input *EventInput) error {
		result, err := tx.ExecContext(ctx, `UPDATE workflow_v2_delivery_prs
			SET mergeable_head_sha=?,base_sha=?,mergeable_state=?
			WHERE id=? AND run_id=? AND active=1 AND registered_only=0 AND state='open' AND current_head_sha=?
			AND mergeable_head_sha=? AND base_sha=? AND mergeable_state=?`,
			verified.HeadSHA, verified.BaseSHA, state, target.PRID, target.RunID, verified.HeadSHA,
			previousHead, previousBase, previousState)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return fmt.Errorf("delivery changed while reconciling mergeability")
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
