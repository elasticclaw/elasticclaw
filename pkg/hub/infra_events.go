package hub

import (
	"encoding/json"
	"fmt"
	"regexp"
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

// infraEventSecretPatterns match credential-shaped substrings a message
// quoted from outside — a provider's 429 body, a vendor status page — may
// carry into infra_events and from there into every routed channel. The
// redaction lives here, at the one boundary every producer writes through,
// rather than in the producer that first needed it. Over-redaction is the
// safe failure mode: nothing in an outage or billing-cap message needs a
// 28-char opaque string to be actionable.
var infraEventSecretPatterns = []*regexp.Regexp{
	// Provider API keys: sk-..., sk-ant-..., sk-proj-... and friends.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
	// An echoed Authorization header: whatever follows "Bearer" on the same
	// line is the credential, however short or oddly spelled, quotes
	// included. Only horizontal whitespace may separate the two — a header
	// block's next line is a different header, not the token.
	regexp.MustCompile(`(?i)\bbearer[ \t]+(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s;,'"]+)`),
	// Any long opaque run that reads as a credential, not prose.
	regexp.MustCompile(`[A-Za-z0-9_-]{28,}`),
}

func redactInfraEventMessage(message string) string {
	for _, pattern := range infraEventSecretPatterns {
		message = pattern.ReplaceAllString(message, "[redacted]")
	}
	return message
}

func (s *Server) recordInfraEvent(event infraEvent) error {
	if !types.IsInfraEventType(event.EventType) {
		return fmt.Errorf("unsupported infra event type %q", event.EventType)
	}
	// The message is the one detail field quoted from outside the hub, and
	// buildInfraMessage forwards it verbatim; redact it for every producer
	// here rather than trusting each to remember. Copied, not mutated: the
	// caller's map is its own.
	if message, ok := event.Detail["message"].(string); ok {
		copied := make(map[string]any, len(event.Detail))
		for key, value := range event.Detail {
			copied[key] = value
		}
		copied["message"] = redactInfraEventMessage(message)
		event.Detail = copied
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
