package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
	err = tx.QueryRow(`
		SELECT COALESCE(ft.claw_id,''), ft.status, COALESCE(c.status,'')
		  FROM factory_triggers ft
		  LEFT JOIN claws c ON c.id = ft.claw_id
		 WHERE ft.factory_name=? AND ft.integration=? AND ft.trigger_key=?`,
		factoryName, integration, triggerKey,
	).Scan(&clawID, &triggerStatus, &clawStatus)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, fmt.Errorf("factory trigger disappeared while claiming")
		}
		return false, err
	}

	if clawID != "" && clawStatus != "" && clawStatus != "deleted" {
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

	if clawID != "" && clawStatus != "" && clawStatus != "deleted" {
		_, _ = tx.Exec(`UPDATE claws SET status='deleted' WHERE id=?`, clawID)
		if s.cronScheduler != nil {
			s.cronScheduler.finishRunByClawID(clawID, "canceled", "factory trigger reclaimed")
		}
	}
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
		   SET status='failed', updated_at=?
		 WHERE factory_name=? AND integration=? AND trigger_key=? AND claw_id=''`,
		now(), factoryName, integration, triggerKey,
	)
}
