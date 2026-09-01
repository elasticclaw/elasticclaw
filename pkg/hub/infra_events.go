package hub

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// infraEvent is the durable, fleet-wide counterpart to a task-run event.
// EventKey is supplied by the producer because it names the real-world edge
// and lets a retry after a crash safely repeat the insert.
type infraEvent struct {
	EventKey   string
	EventType  string
	Subject    string
	Detail     map[string]any
	OccurredAt time.Time
}

func (s *Server) recordInfraEvent(event infraEvent) error {
	if !types.IsInfraEventType(event.EventType) {
		return fmt.Errorf("unsupported infra event type %q", event.EventType)
	}
	detail, err := json.Marshal(event.Detail)
	if err != nil {
		return fmt.Errorf("marshal infra event detail: %w", err)
	}
	if event.OccurredAt.IsZero() {
		if s.nowFunc != nil {
			event.OccurredAt = s.nowFunc()
		} else {
			event.OccurredAt = now()
		}
	}
	_, err = s.db.Exec(`INSERT INTO infra_events(event_key, event_type, subject, detail, occurred_at)
		VALUES(?,?,?,?,?) ON CONFLICT(event_key) DO NOTHING`,
		event.EventKey, event.EventType, event.Subject, string(detail), epochMillis(event.OccurredAt))
	if err != nil {
		return fmt.Errorf("insert infra event: %w", err)
	}
	return nil
}
