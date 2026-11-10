package workflowv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

var (
	errDeferredEventGone  = errors.New("deferred event already replayed")
	errReplayHalted       = errors.New("run no longer replays deferred events")
	errDeferredEventStale = errors.New("deferred event describes a superseded pull request head")
)

// Held runs stay suspended while their deferred queue replays with the
// invariant check postponed. Effects only run for active runs, so nothing
// scheduled meanwhile executes before settleReplayedInvariant judges the
// invariant of the state the run ends in. replayPendingReason holds a state
// whose entry was verified; replayPendingEntryReason holds an entry whose
// invariant was never verified, so its entry effects must not run if the run
// leaves the state or the invariant fails (see cancelHeldEntryEffects).
const (
	replayPendingReason      = "replaying deferred events before judging the state invariant"
	replayPendingEntryReason = "replaying deferred events before judging the entered state invariant"
)

// heldForReplay reports whether a replay holds the run suspended.
func (run Run) heldForReplay() bool {
	return run.Status == RunSuspended &&
		(run.WaitingReason == replayPendingReason || run.WaitingReason == replayPendingEntryReason)
}

// heldEntry reports whether the run is held in a state whose entry invariant
// was never verified.
func (run Run) heldEntry() bool {
	return run.Status == RunSuspended && run.WaitingReason == replayPendingEntryReason
}

// replaying reports whether a run may replay its deferred queue: active, or
// held suspended by a replay that postponed its invariant check.
func (run Run) replaying() bool {
	return run.Status == RunActive || run.heldForReplay()
}

// cancelHeldEntryEffects cancels the effects that the unverified entry into
// the run's current state scheduled. The run leaves that entry (another
// transition, or a suspension because its invariant does not hold), so a live
// event would never have scheduled them; releasing them later would run the
// entry work of a state the run is no longer in.
func cancelHeldEntryEffects(ctx context.Context, tx *sql.Tx, run Run, now time.Time) error {
	var transitionID, eventID string
	err := tx.QueryRowContext(ctx, `SELECT id,event_id FROM workflow_v2_transitions WHERE run_id=? AND to_version=?`,
		run.ID, run.StateVersion).Scan(&transitionID, &eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET status='cancelled',last_error=?,updated_at=?
		WHERE run_id=? AND status IN ('planned','retryable_failed')
		AND ((origin_type='transition' AND origin_id=?) OR (origin_type='event_clause' AND origin_id=?))`,
		fmt.Sprintf("state %s was left before its entry invariant was verified", run.State), now.UnixMilli(),
		run.ID, transitionID, eventID)
	return err
}

// postponeClauseEffects parks the clause effects an event planned while a
// replay postponed a broken invariant. A live event would have been rejected
// before planning them, so they wait for settleReplayedInvariant.
func postponeClauseEffects(ctx context.Context, tx *sql.Tx, runID, eventID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET postponed=1
		WHERE run_id=? AND origin_type='event_clause' AND origin_id=? AND status='planned'`, runID, eventID)
	return err
}

// releasePostponedEffects lets the effect worker claim parked clause effects
// once the replay verified the invariant of the state they were planned in.
func releasePostponedEffects(ctx context.Context, tx *sql.Tx, runID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET postponed=0,updated_at=? WHERE run_id=? AND postponed=1`,
		now.UnixMilli(), runID)
	return err
}

// cancelPostponedEffects cancels parked clause effects when the run suspends
// or leaves the state they were planned in before its invariant was verified.
func cancelPostponedEffects(ctx context.Context, tx *sql.Tx, run Run, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE workflow_v2_effects SET status='cancelled',postponed=0,last_error=?,updated_at=?
		WHERE run_id=? AND postponed=1 AND status IN ('planned','retryable_failed')`,
		fmt.Sprintf("state %s was left or suspended before the replay verified its invariant", run.State), now.UnixMilli(), run.ID)
	return err
}

// holdForReplay suspends an active run whose current invariant a replay found
// broken, until settleReplayedInvariant judges it.
func holdForReplay(ctx context.Context, tx *sql.Tx, run Run, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='suspended',waiting_reason=?,updated_at=?
		WHERE id=? AND state_version=? AND status='active'`, replayPendingReason, now.UnixMilli(), run.ID, run.StateVersion)
	return err
}

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
// every run that is active again, and settles runs a failed replay left held.
// The transition that resumes a run replays its queue immediately; this sweep
// covers failures and recoveries outside the event path. Queues of finished
// runs are discarded.
func (s *Store) ReplayDeferredEvents(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("workflow v2 store is not configured")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM workflow_v2_deferred_events WHERE run_id IN
		(SELECT id FROM workflow_v2_runs WHERE status NOT IN ('active','suspended'))`); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT d.run_id FROM workflow_v2_deferred_events d
		JOIN workflow_v2_runs r ON r.id=d.run_id WHERE r.status='active'
		UNION SELECT id FROM workflow_v2_runs WHERE status='suspended' AND waiting_reason IN (?,?) ORDER BY 1`,
		replayPendingReason, replayPendingEntryReason)
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
			// A replay must not resume a run another event suspended or finished.
			run, err := scanRun(tx.QueryRowContext(ctx, `SELECT id,tenant_id,workspace_name,workflow_name,
				workspace_revision,workflow_revision,state,display_phase,state_version,status,waiting_reason,
				current_attempt_id,current_task_id,context_bundle_id,task_run_id,trigger_type,created_at,updated_at,finished_at
				FROM workflow_v2_runs WHERE id=?`, runID))
			if err != nil {
				return err
			}
			if !run.replaying() {
				return errReplayHalted
			}
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
			if stale, err := deferredMergeabilityHeadMoved(ctx, tx, runID, item.key, input); err != nil {
				return err
			} else if stale {
				return errDeferredEventStale
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
		if errors.Is(err, errDeferredEventStale) {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM workflow_v2_deferred_events
				WHERE run_id=? AND defer_key=? AND event_id=?`, runID, item.key, item.eventID); err != nil {
				return err
			}
			continue
		}
		if errors.Is(err, errReplayHalted) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("replay deferred event %s: %w", item.eventID, err)
		}
		if !result.Run.replaying() {
			return nil
		}
	}
	return s.settleReplayedInvariant(ctx, runID)
}

// hasDeferredEvents reports whether a run has queued observations to replay.
func hasDeferredEvents(ctx context.Context, tx *sql.Tx, runID string) (bool, error) {
	var queued bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_v2_deferred_events WHERE run_id=?)`, runID).Scan(&queued)
	return queued, err
}

// settleReplayedInvariant runs the invariant check that a replay postponed:
// once the queue drained, a run whose state invariant holds is released (held
// runs become active), and one whose invariant no longer holds is suspended
// exactly as a live event would have left it. Observations queued during the
// replay keep the run held for the next sweep.
func (s *Store) settleReplayedInvariant(ctx context.Context, runID string) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, workflowYAML, err := getRunForUpdate(ctx, tx, runID)
	if err != nil || !stored.replaying() {
		return err
	}
	workflow, err := typesv2.ParseAndValidateWorkflow([]byte(workflowYAML))
	if err != nil {
		return fmt.Errorf("load pinned workflow: %w", err)
	}
	facts, err := loadFacts(ctx, tx, runID)
	if err != nil {
		return err
	}
	valid, err := typesv2.MatchPredicate(workflow.Workflow.States[stored.State].Invariant, facts)
	if err != nil {
		return err
	}
	queued, err := hasDeferredEvents(ctx, tx, runID)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	switch {
	case valid:
		if err := releasePostponedEffects(ctx, tx, runID, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='active',waiting_reason='',updated_at=?
			WHERE id=? AND state_version=? AND status='suspended' AND waiting_reason IN (?,?)`,
			now.UnixMilli(), runID, stored.StateVersion, replayPendingReason, replayPendingEntryReason)
	case queued:
		err = holdForReplay(ctx, tx, stored, now)
	default:
		if stored.heldEntry() {
			if err := cancelHeldEntryEffects(ctx, tx, stored, now); err != nil {
				return err
			}
		}
		if err := cancelPostponedEffects(ctx, tx, stored, now); err != nil {
			return err
		}
		reason := fmt.Sprintf("state invariant no longer holds for %s after replaying deferred events", stored.State)
		_, err = tx.ExecContext(ctx, `UPDATE workflow_v2_runs SET status='suspended',waiting_reason=?,updated_at=?
			WHERE id=? AND state_version=? AND (status='active' OR (status='suspended' AND waiting_reason IN (?,?)))`,
			reason, now.UnixMilli(), runID, stored.StateVersion, replayPendingReason, replayPendingEntryReason)
	}
	if err != nil {
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

// deferredMergeabilityHeadMoved reports whether a queued mergeability
// observation describes a PR head that is no longer current. Every path that
// moves a head clears these queues, but a stale conflict replayed against a
// new head would ask the agent to resolve a conflict that no longer exists.
func deferredMergeabilityHeadMoved(ctx context.Context, tx *sql.Tx, runID, key string, input *EventInput) (bool, error) {
	if !strings.HasPrefix(key, mergeabilityDeferKey("")) {
		return false, nil
	}
	pullRequest, _ := input.Payload["pull_request"].(map[string]interface{})
	prID, _ := pullRequest["id"].(string)
	headSHA, _ := pullRequest["head_sha"].(string)
	if prID == "" || headSHA == "" {
		return false, nil
	}
	var currentHead string
	err := tx.QueryRowContext(ctx, `SELECT current_head_sha FROM workflow_v2_delivery_prs WHERE id=? AND run_id=?`,
		prID, runID).Scan(&currentHead)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil && currentHead != headSHA, err
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
