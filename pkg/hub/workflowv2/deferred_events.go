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

// deferEvent queues a listened observation recorded by a suspended run. Only
// the latest observation per subject is kept: an older one would fire a
// transition on state the provider has already moved past.
func deferEvent(ctx context.Context, tx *sql.Tx, runID string, input EventInput, now time.Time) error {
	key := input.deferKey
	if key == "" {
		key = input.Kind
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO workflow_v2_deferred_events(run_id,defer_key,event_id,seq,created_at)
		VALUES(?,?,?,(SELECT COALESCE(MAX(seq),0)+1 FROM workflow_v2_deferred_events WHERE run_id=?),?)
		ON CONFLICT(run_id,defer_key) DO UPDATE SET event_id=excluded.event_id,seq=excluded.seq,created_at=excluded.created_at`,
		runID, key, input.ID, runID, now.UnixMilli())
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
// carries only the original payload for transition matching.
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
			Payload: payload, observation: true, deferKey: item.key,
		}, func(ctx context.Context, tx *sql.Tx, _ *EventInput) error {
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
			return nil
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
	return nil
}
