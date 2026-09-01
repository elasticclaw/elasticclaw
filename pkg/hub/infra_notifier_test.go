package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// recordingNotifier captures every message it is asked to send, and can be
// told to fail (optionally with a classified error) the next N sends.
type recordingNotifier struct {
	sent    []notify.Message
	failN   int
	failErr error
}

func (n *recordingNotifier) Send(ctx context.Context, msg notify.Message) (string, error) {
	if n.failN > 0 {
		n.failN--
		if n.failErr != nil {
			return "", n.failErr
		}
		return "", errors.New("synthetic transient failure")
	}
	n.sent = append(n.sent, msg)
	return fmt.Sprintf("handle-%d", len(n.sent)), nil
}

func mustRecordInfraEvent(t *testing.T, s *Server, key, eventType string, detail map[string]any, at time.Time) {
	t.Helper()
	if err := s.recordInfraEvent(infraEvent{EventKey: key, EventType: eventType, Subject: key, Detail: detail, OccurredAt: at}); err != nil {
		t.Fatal(err)
	}
}

// Regression: a route with a non-empty Events allowlist must skip events not
// in that list (and still advance its watermark past them) while delivering
// the ones it does subscribe to.
func TestDeliverInfraRouteFiltersByEvent(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustRecordInfraEvent(t, s, "ev-down", "dependency_down", map[string]any{"name": "Anthropic"}, base)
	mustRecordInfraEvent(t, s, "ev-capped", "provider_limit_opened", map[string]any{"name": "OpenAI"}, base.Add(time.Second))

	n := &recordingNotifier{}
	route := types.InfraRoute{Via: "ops", Events: []string{"provider_limit_opened"}}
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(n.sent))
	}
	if n.sent[0].Subject != "OpenAI" {
		t.Fatalf("delivered subject = %q, want OpenAI", n.sent[0].Subject)
	}

	// A second tick with nothing new must not resend, proving the filtered
	// event advanced the watermark instead of blocking behind it.
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 1 {
		t.Fatalf("second tick sent %d messages, want still 1 (no resend, no unblocking gap)", len(n.sent))
	}
}

// An empty Events allowlist on a route means "all infra events".
func TestDeliverInfraRouteEmptyAllowlistDeliversEverything(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustRecordInfraEvent(t, s, "ev-down", "dependency_down", map[string]any{"name": "Anthropic"}, base)
	mustRecordInfraEvent(t, s, "ev-capped", "provider_limit_opened", map[string]any{"name": "OpenAI"}, base.Add(time.Second))

	n := &recordingNotifier{}
	route := types.InfraRoute{Via: "ops"}
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(n.sent))
	}
}

// Regression: dedupe is per (event, route) — two routes must each get their
// own copy of the same event, and neither route may be sent the same event
// twice across ticks.
func TestDeliverInfraRouteDedupePerRoute(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustRecordInfraEvent(t, s, "ev-down", "dependency_down", map[string]any{"name": "Anthropic"}, base)

	nOps, nSre := &recordingNotifier{}, &recordingNotifier{}
	route := types.InfraRoute{Via: "any"}
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, nOps); err != nil {
		t.Fatal(err)
	}
	if err := s.deliverInfraRoute(context.Background(), base, "sre", route, nSre); err != nil {
		t.Fatal(err)
	}
	if len(nOps.sent) != 1 || len(nSre.sent) != 1 {
		t.Fatalf("each route should get the event once: ops=%d sre=%d", len(nOps.sent), len(nSre.sent))
	}
	// Re-deliver "ops" — must not resend.
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, nOps); err != nil {
		t.Fatal(err)
	}
	if len(nOps.sent) != 1 {
		t.Fatalf("ops route resent an already-delivered event: sent=%d", len(nOps.sent))
	}
}

// Regression: a message that keeps failing with an unclassified (therefore
// transient) error must not wedge the route's cursor forever — after the cap
// it is recorded failed and the queue behind it is free to flow.
func TestDeliverInfraRouteTransientFailureCapUnwedgesRoute(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustRecordInfraEvent(t, s, "ev-poison", "dependency_down", map[string]any{"name": "Anthropic"}, base)
	mustRecordInfraEvent(t, s, "ev-behind", "dependency_recovered", map[string]any{"name": "Anthropic"}, base.Add(time.Second))

	n := &recordingNotifier{failN: infraMaxTransientFailures}
	route := types.InfraRoute{Via: "ops"}
	for i := 0; i < infraMaxTransientFailures-1; i++ {
		if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
			t.Fatal(err)
		}
	}
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM infra_notification_deliveries WHERE notifier='ops'`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatalf("poisoned event was recorded before the transient-failure cap was reached")
	}

	// The cap-th consecutive failure records the poisoned event failed and
	// unblocks the event behind it.
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM infra_notification_deliveries WHERE notifier='ops' AND event_rowid=(SELECT rowid FROM infra_events WHERE event_key='ev-poison')`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != notificationDeliveryStatusFailed {
		t.Fatalf("poisoned delivery status = %q, want failed", status)
	}
	if len(n.sent) != 1 || n.sent[0].Subject != "Anthropic" {
		t.Fatalf("event behind the poisoned one was not delivered: sent=%v", n.sent)
	}
}

// A notify.ErrorConfig failure pauses the whole route (the config is broken,
// every send would fail alike) rather than burning through the transient cap
// per event.
func TestDeliverInfraRouteConfigErrorPausesRoute(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustRecordInfraEvent(t, s, "ev-down", "dependency_down", map[string]any{"name": "Anthropic"}, base)

	n := &recordingNotifier{failN: 1, failErr: notify.ConfigError(errors.New("bad token"))}
	route := types.InfraRoute{Via: "ops"}
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM infra_notification_deliveries`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("config error must not record a delivery outcome, got %d rows", count)
	}
}

// buildInfraMessage must render an actionable next step for both the
// dependency and provider-cap event families.
func TestBuildInfraMessageActionable(t *testing.T) {
	cases := []struct {
		eventType string
		wantTitle string
		wantBody  string
	}{
		{"dependency_down", "Dependency is down", "status page"},
		{"dependency_degraded", "Dependency is degraded", "status page"},
		{"dependency_recovered", "Dependency recovered", "recovered"},
		{"provider_limit_opened", "Provider account is capped", "billing console"},
		{"provider_limit_exhausted", "Provider cap needs a human", "billing console"},
		{"provider_limit_released", "Provider cap lifted", "recovered"},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			e := infraEventRow{
				EventType:  tc.eventType,
				Subject:    "sample",
				Detail:     map[string]any{"name": "Anthropic"},
				OccurredAt: time.Now(),
			}
			msg := buildInfraMessage(e, time.Now())
			if msg.Title != tc.wantTitle {
				t.Fatalf("title = %q, want %q", msg.Title, tc.wantTitle)
			}
			if !strings.Contains(strings.ToLower(msg.Body), tc.wantBody) {
				t.Fatalf("body = %q, want it to mention %q", msg.Body, tc.wantBody)
			}
		})
	}
}

// buildInfraMessage must never render the raw provider key: only the masked
// key id the event producer already stored ever reaches the detail map, so
// the rendered message must reflect exactly what was recorded and nothing
// derived from a raw secret.
func TestBuildInfraMessageRendersOnlyMaskedKeyID(t *testing.T) {
	const maskedID = "sk-...ab12"
	e := infraEventRow{
		EventType:  "provider_limit_opened",
		Subject:    "sample",
		Detail:     map[string]any{"name": "OpenAI", "key_id": maskedID, "message": "account capped"},
		OccurredAt: time.Now(),
	}
	msg := buildInfraMessage(e, time.Now())
	all := msg.Title + msg.Body + msg.Subject
	for _, f := range msg.Fields {
		all += f.Label + f.Value
	}
	if !strings.Contains(all, "capped") {
		t.Fatalf("rendered message dropped the provider's own detail message: %q", all)
	}
}

func TestBuildInfraMessageUnknownEventTypeDoesNotPanic(t *testing.T) {
	msg := buildInfraMessage(infraEventRow{EventType: "something_new", Subject: "x"}, time.Now())
	if msg.Title == "" {
		t.Fatal("unknown event type produced an empty title")
	}
}
