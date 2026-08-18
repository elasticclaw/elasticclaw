package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var errFactoryTriggerAlreadyClaimed = errors.New("factory trigger already claimed")

func isFactoryTriggerAlreadyClaimed(err error) bool {
	return errors.Is(err, errFactoryTriggerAlreadyClaimed)
}

func factoryTriggerKey(integration, externalID string) string {
	return integration + ":" + externalID
}

func activeTriggerStatus(status string) bool {
	switch status {
	case "claimed", "creating", "active":
		return true
	default:
		return false
	}
}

func triggerPayloadJSON(payload any) string {
	if payload == nil {
		return "{}"
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	const maxTriggerPayload = 16 * 1024
	if len(b) > maxTriggerPayload {
		return "{}"
	}
	return string(b)
}

// factoryTriggerBackoff is the wait a trigger owes after n failed attempts:
// min(30s * 2^n, 30m). Both the creation-failure path and the dead-claw path
// climb this same ladder, so a trigger that keeps failing keeps backing off
// whether it dies before or after its claw exists.
func factoryTriggerBackoff(retryCount int) time.Duration {
	backoff := 30 * time.Second
	for i := 0; i < retryCount && backoff < 30*time.Minute; i++ {
		backoff *= 2
	}
	if backoff > 30*time.Minute {
		backoff = 30 * time.Minute
	}
	return backoff
}

// triggerSourceSkipsBackoff reports whether a source may retry without waiting.
// The backoff exists to break a tight loop, and only the poller can loop: it
// re-sees the same trigger every tick and would recreate the claw forever. A
// plain "webhook" source (the external integration) is edge-triggered — one
// delivery per external event — so it cannot spin on its own, and delaying it
// would only make a deliberate re-fire look ignored. Note this is a narrow
// exemption: integration webhooks label themselves "<integration> webhook" and
// do back off, because their events can repeat on their own (label churn,
// comment edits).
func triggerSourceSkipsBackoff(source string) bool {
	return source == "webhook"
}

func (s *Server) claimFactoryTrigger(factoryName, integration, triggerKey, source string, payload any) (bool, error) {
	if factoryName == "" || integration == "" || triggerKey == "" {
		return false, fmt.Errorf("invalid factory trigger claim")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := now()
	payloadJSON := triggerPayloadJSON(payload)
	res, err := tx.Exec(`
		INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, status, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		uuid.New().String(), factoryName, integration, triggerKey, source, payloadJSON, "claimed", now, now, now, now,
	)
	if err != nil {
		return false, err
	}
	if rows, _ := res.RowsAffected(); rows > 0 {
		return true, tx.Commit()
	}

	var clawID, triggerStatus, clawStatus string
	var retryCount int
	var updatedAt time.Time
	var clawAgeSeconds sql.NullInt64
	err = tx.QueryRow(`
		SELECT COALESCE(ft.claw_id,''), ft.status, COALESCE(c.status,''), ft.retry_count, ft.updated_at,
		       CAST(strftime('%s','now') AS INTEGER) - CAST(strftime('%s', c.created_at) AS INTEGER)
		  FROM factory_triggers ft
		  LEFT JOIN claws c ON c.id = ft.claw_id
		 WHERE ft.factory_name=? AND ft.integration=? AND ft.trigger_key=?`,
		factoryName, integration, triggerKey,
	).Scan(&clawID, &triggerStatus, &clawStatus, &retryCount, &updatedAt, &clawAgeSeconds)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, fmt.Errorf("factory trigger disappeared while claiming")
		}
		return false, err
	}

	if clawID != "" && clawStatus != "" && clawStatus != "deleted" && clawStatus != "error" {
		_, err = tx.Exec(`
			UPDATE factory_triggers
			   SET trigger_source=?, trigger_payload=?, last_seen_at=?, updated_at=?
			 WHERE factory_name=? AND integration=? AND trigger_key=?`,
			source, payloadJSON, now, now, factoryName, integration, triggerKey,
		)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if clawID == "" && activeTriggerStatus(triggerStatus) {
		_, err = tx.Exec(`
			UPDATE factory_triggers
			   SET trigger_source=?, trigger_payload=?, last_seen_at=?, updated_at=?
			 WHERE factory_name=? AND integration=? AND trigger_key=?`,
			source, payloadJSON, now, now, factoryName, integration, triggerKey,
		)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if clawID == "" && triggerStatus == "failed" && !triggerSourceSkipsBackoff(source) {
		if now.Before(updatedAt.Add(factoryTriggerBackoff(retryCount))) {
			_, err = tx.Exec(`UPDATE factory_triggers SET last_seen_at=? WHERE factory_name=? AND integration=? AND trigger_key=?`, now, factoryName, integration, triggerKey)
			if err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
	}

	// bornDeadClawMax separates a claw that never got going from one that did its
	// job and was cleaned up. Both end up 'deleted', so status alone cannot tell
	// them apart, and charging the long-lived one a retry would make every
	// SUCCESSFUL ticket pay a backoff the next time its trigger fires — with the
	// counter ratcheting, because nothing ever resets it. The measured churn is
	// claws dying within ~3 minutes of bootstrap, so the cutoff sits well above
	// that and well below any real run.
	const bornDeadClawMax = 10 * 60 // seconds

	clawDiedYoung := clawAgeSeconds.Valid && clawAgeSeconds.Int64 <= bornDeadClawMax
	if clawID != "" && (clawStatus == "error" || clawStatus == "deleted") && clawDiedYoung {
		// The claw this trigger already paid for died young. Charge it as a failed
		// attempt instead of dropping into the reclaim below, which reclaims with
		// no wait and no retry_count bump. The failed-trigger guard above cannot
		// cover this case: a claw that dies *after* creation still has its id on
		// the trigger, so the guard's clawID=="" never holds, and every poll tick
		// re-created an identical claw the moment the previous one errored. A
		// deterministic failure (a build gate that always fails, a bootstrap that
		// always crashes) then burns several claws per ticket instead of one.
		// Recording the failure here routes the next tick through the backoff
		// above, so the retry ladder is the same one a creation failure climbs.
		_, err = tx.Exec(`
			UPDATE factory_triggers
			   SET claw_id='', status='failed', retry_count=retry_count+1, last_seen_at=?, updated_at=?
			 WHERE factory_name=? AND integration=? AND trigger_key=?`,
			now, now, factoryName, integration, triggerKey,
		)
		if err != nil {
			return false, err
		}
		if !triggerSourceSkipsBackoff(source) {
			return false, tx.Commit()
		}
		// Exempt sources still get charged the retry, but reclaim immediately —
		// see triggerSourceSkipsBackoff.
	}

	// Reclaim path: reached when the trigger has no claw and is not actively
	// being processed, when its claw row is simply gone from the database, or
	// when an exempt source is retrying a dead claw. No cleanup of the old claw is
	// needed here — a claw reaches 'error' via stopAgentTerminalWithReason, which
	// already calls finishRunByClawID, and the boot reaper sweeps any still-'running'
	// run whose claw is 'error'/'deleted'/missing. (A prior cleanup block here was
	// dead code: its guard duplicated the "already claimed" block above, which
	// returns first.)
	_, err = tx.Exec(`
		UPDATE factory_triggers
		   SET trigger_source=?, trigger_payload=?, claw_id='', status='claimed', last_seen_at=?, updated_at=?
		 WHERE factory_name=? AND integration=? AND trigger_key=?`,
		source, payloadJSON, now, now, factoryName, integration, triggerKey,
	)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Server) completeFactoryTrigger(factoryName, integration, triggerKey, clawID string) error {
	if clawID == "" {
		return fmt.Errorf("complete factory trigger: missing claw id")
	}
	// retry_count is deliberately not reset here. Completing a claim only means a
	// claw was created, not that the work succeeded, and the dead-claw path in
	// claimFactoryTrigger runs after this: zeroing here would pin every retry at
	// the first rung (create → die → 30s → create → die → 30s …) and give back
	// the tight loop the backoff is meant to break. The counter is per trigger
	// key, so it stays bounded to one ticket, capped at 30m; a human who wants an
	// immediate retry has the exempt-source path, not a counter reset.
	res, err := s.db.Exec(`
		UPDATE factory_triggers
		   SET claw_id=?, status='active', updated_at=?
		 WHERE factory_name=? AND integration=? AND trigger_key=?`,
		clawID, now(), factoryName, integration, triggerKey,
	)
	if err != nil {
		return err
	}
	rowsAffected, _ := res.RowsAffected()
	if rowsAffected == 0 {
		return fmt.Errorf("factory trigger claim not found")
	}
	return nil
}

func (s *Server) failFactoryTrigger(factoryName, integration, triggerKey string) {
	_, _ = s.db.Exec(`
		UPDATE factory_triggers
		   SET status='failed', retry_count=retry_count+1, updated_at=?
		 WHERE factory_name=? AND integration=? AND trigger_key=? AND claw_id=''`,
		now(), factoryName, integration, triggerKey,
	)
}
