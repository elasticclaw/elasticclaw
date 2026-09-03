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

// establishInfraRoute stamps the route's watermark at the current head of
// infra_events, the way NewServer's baseline (or the route's own first tick)
// does before any event under test is recorded.
func establishInfraRoute(t *testing.T, s *Server, via string) {
	t.Helper()
	if err := s.deliverInfraRoute(context.Background(), time.Unix(0, 0), via, types.InfraRoute{Via: via}, &recordingNotifier{}); err != nil {
		t.Fatal(err)
	}
}

// Regression: a route with a non-empty Events allowlist must skip events not
// in that list (and still advance its watermark past them) while delivering
// the ones it does subscribe to.
func TestDeliverInfraRouteFiltersByEvent(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	establishInfraRoute(t, s, "ops")
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
	establishInfraRoute(t, s, "ops")
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
	establishInfraRoute(t, s, "ops")
	establishInfraRoute(t, s, "sre")
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
	establishInfraRoute(t, s, "ops")
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
	establishInfraRoute(t, s, "ops")
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

// Regression: a route seen for the first time — added at runtime, or enabled
// months after the producers started recording — must baseline at the head of
// infra_events instead of replaying the whole history as fresh alerts.
func TestDeliverInfraRouteNewRouteDoesNotReplayHistory(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustRecordInfraEvent(t, s, "ev-old-down", "dependency_down", map[string]any{"name": "Anthropic"}, base)
	mustRecordInfraEvent(t, s, "ev-old-recovered", "dependency_recovered", map[string]any{"name": "Anthropic"}, base.Add(time.Second))

	n := &recordingNotifier{}
	route := types.InfraRoute{Via: "ops"}
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 0 {
		t.Fatalf("brand-new route replayed %d historical events", len(n.sent))
	}
	mustRecordInfraEvent(t, s, "ev-new", "provider_limit_opened", map[string]any{"name": "OpenAI"}, base.Add(time.Minute))
	if err := s.deliverInfraRoute(context.Background(), base.Add(time.Minute), "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 1 || n.sent[0].Subject != "OpenAI" {
		t.Fatalf("route missed the first post-baseline event: sent=%v", n.sent)
	}
}

// initInfraNotifierBaseline stamps every configured route synchronously at
// boot, so events produced before the notifier's first tick are history, not
// a backlog.
func TestInitInfraNotifierBaselineStampsConfiguredRoutes(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Infra: &types.InfraNotificationsConfig{Routes: []types.InfraRoute{{Via: "ops"}}},
	}}, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mustRecordInfraEvent(t, s, "ev-historical", "dependency_down", map[string]any{"name": "Anthropic"}, base)
	s.initInfraNotifierBaseline()
	watermark, found, err := s.notifierStateInt64(infraWatermarkKey("ops"))
	if err != nil || !found {
		t.Fatalf("baseline missing: found=%v err=%v", found, err)
	}
	maxRow, err := s.infraMaxEventRowID()
	if err != nil {
		t.Fatal(err)
	}
	if watermark != maxRow {
		t.Fatalf("baseline watermark = %d, want head %d", watermark, maxRow)
	}
}

// Regression: a delivery whose bookkeeping write fails after a successful
// send must be fenced in memory — never re-sent — and its row landed once the
// database accepts writes again.
func TestDeliverInfraRoutePendingDeliveryPreventsResend(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	establishInfraRoute(t, s, "ops")
	mustRecordInfraEvent(t, s, "ev-down", "dependency_down", map[string]any{"name": "Anthropic"}, base)
	var rowid int64
	if err := s.db.QueryRow(`SELECT rowid FROM infra_events WHERE event_key='ev-down'`).Scan(&rowid); err != nil {
		t.Fatal(err)
	}
	// Block only the bookkeeping insert; reads and the send stay healthy.
	if _, err := s.db.Exec(`CREATE TRIGGER block_infra_delivery BEFORE INSERT ON infra_notification_deliveries BEGIN SELECT RAISE(ABORT, 'synthetic bookkeeping failure'); END`); err != nil {
		t.Fatal(err)
	}
	n := &recordingNotifier{}
	route := types.InfraRoute{Via: "ops"}
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(n.sent))
	}
	if !s.infraDeliveryPending(rowid, "ops") {
		t.Fatal("failed bookkeeping write was not stashed as pending")
	}
	// Even with the cursor rewound (as if the watermark write had failed
	// too), the stash alone must fence the resend.
	s.setNotifierStateInt64(infraWatermarkKey("ops"), 0)
	if err := s.deliverInfraRoute(context.Background(), base, "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 1 {
		t.Fatalf("stashed delivery was re-sent: sent=%d", len(n.sent))
	}
	// Once the DB accepts writes again the flush lands the row and drains the stash.
	if _, err := s.db.Exec(`DROP TRIGGER block_infra_delivery`); err != nil {
		t.Fatal(err)
	}
	s.flushPendingInfraDeliveries()
	if s.infraDeliveryPending(rowid, "ops") {
		t.Fatal("stash did not drain after the database recovered")
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM infra_notification_deliveries WHERE event_rowid=? AND notifier='ops'`, rowid).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != notificationDeliveryStatusSent {
		t.Fatalf("flushed delivery status = %q, want sent", status)
	}
}

// Regression: the producer stores the parked-claw count under "parked_claws";
// the renderer must read the same key, and must render both the Go int the
// test-send path stores and the float64 a JSON round-trip produces.
func TestBuildInfraMessageRendersParkedClaws(t *testing.T) {
	for name, count := range map[string]any{"int": int(4), "float64": float64(4)} {
		t.Run(name, func(t *testing.T) {
			e := infraEventRow{
				EventType:  "provider_limit_opened",
				Subject:    "openai",
				Detail:     map[string]any{"provider": "openai", "key_id": "key_abc123", "parked_claws": count, "deadline": "2026-09-02 00:00 UTC"},
				OccurredAt: time.Now(),
			}
			msg := buildInfraMessage(e, time.Now())
			got := ""
			for _, f := range msg.Fields {
				if f.Label == "Claws parked" {
					got = f.Value
				}
			}
			if got != "4" {
				t.Fatalf("Claws parked field = %q, want \"4\" (fields: %+v)", got, msg.Fields)
			}
		})
	}
}

// Regression: a route removed from notifications.infra.routes and later
// re-added under the same via must be re-baselined at the head of the stream,
// not resume from the cursor it left behind and replay the absence window.
func TestPruneInfraRouteStateReAddedRouteDoesNotReplayAbsenceWindow(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	establishInfraRoute(t, s, "ops")
	s.setNotifierStateInt64(infraFailureKey(1, "ops"), 3)

	// Route removed: the absence window records an incident start to finish.
	s.pruneInfraRouteState(map[string]bool{"eng": true})
	mustRecordInfraEvent(t, s, "ev-absent-down", "dependency_down", map[string]any{"name": "Anthropic"}, base)
	mustRecordInfraEvent(t, s, "ev-absent-recovered", "dependency_recovered", map[string]any{"name": "Anthropic"}, base.Add(time.Minute))
	if _, found, _ := s.notifierStateInt64(infraFailureKey(1, "ops")); found {
		t.Fatal("transient-failure counter of the removed route survived the prune")
	}

	// Route re-added.
	n := &recordingNotifier{}
	route := types.InfraRoute{Via: "ops"}
	if err := s.deliverInfraRoute(context.Background(), base.Add(2*time.Minute), "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 0 {
		t.Fatalf("re-added route replayed %d events from its absence window", len(n.sent))
	}
	mustRecordInfraEvent(t, s, "ev-live", "provider_limit_opened", map[string]any{"name": "OpenAI"}, base.Add(3*time.Minute))
	if err := s.deliverInfraRoute(context.Background(), base.Add(3*time.Minute), "ops", route, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 1 || n.sent[0].Subject != "OpenAI" {
		t.Fatalf("re-added route missed its first live event: sent=%v", n.sent)
	}
	// A configured route keeps its cursor across ticks.
	s.pruneInfraRouteState(map[string]bool{"ops": true})
	if _, found, _ := s.notifierStateInt64(infraWatermarkKey("ops")); !found {
		t.Fatal("prune dropped the cursor of a configured route")
	}
}

// Regression: disabling infrastructure alerts and re-enabling them a week
// later must not flush the disabled window into the channel as live outages.
func TestInfraNotifierTickDisabledWindowIsNotReplayed(t *testing.T) {
	disabled := false
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Notifications: &types.NotificationsConfig{
		Notifiers: map[string]types.NotifierConfig{"ops": {Type: "slack", Settings: map[string]any{"channel": "C0123ABCD", "token_secret": "slack_token"}}},
		Infra:     &types.InfraNotificationsConfig{Enabled: &disabled, Routes: []types.InfraRoute{{Via: "ops"}}},
	}}, "", "", "")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	establishInfraRoute(t, s, "ops")

	s.infraNotifierTick(context.Background(), base)
	mustRecordInfraEvent(t, s, "ev-while-off", "dependency_down", map[string]any{"name": "Anthropic"}, base.Add(time.Minute))
	s.infraNotifierTick(context.Background(), base.Add(2*time.Minute))

	// Re-enabled: the route's first delivery must see nothing from the window.
	n := &recordingNotifier{}
	if err := s.deliverInfraRoute(context.Background(), base.Add(3*time.Minute), "ops", types.InfraRoute{Via: "ops"}, n); err != nil {
		t.Fatal(err)
	}
	if len(n.sent) != 0 {
		t.Fatalf("re-enabled route replayed %d events from the disabled window", len(n.sent))
	}
}

// provider_limit_exhausted is never auto-released, so its message must send
// the operator to clear the block rather than promise a reset that will not
// come; provider_limit_released describes claws that resumed, not parked.
func TestBuildInfraMessageProviderLimitFieldsMatchTheLatch(t *testing.T) {
	detail := map[string]any{"name": "Anthropic", "parked_claws": 4, "deadline": "2026-09-02 00:45 UTC", "retry_count": 3}
	labels := func(msg notify.Message) map[string]string {
		out := map[string]string{}
		for _, f := range msg.Fields {
			out[f.Label] = f.Value
		}
		return out
	}

	exhausted := buildInfraMessage(infraEventRow{EventType: "provider_limit_exhausted", Subject: "anthropic", Detail: detail, OccurredAt: time.Now()}, time.Now())
	if strings.Contains(strings.ToLower(exhausted.Body), "wait until") {
		t.Fatalf("exhausted body still tells the operator to wait: %q", exhausted.Body)
	}
	if !strings.Contains(strings.ToLower(exhausted.Body), "clear the block") {
		t.Fatalf("exhausted body does not name the required action: %q", exhausted.Body)
	}
	if got := labels(exhausted); got["Deadline"] != "" || got["Last retry"] != "2026-09-02 00:45 UTC" {
		t.Fatalf("exhausted fields = %v, want the deadline relabelled as the last retry", got)
	}

	released := buildInfraMessage(infraEventRow{EventType: "provider_limit_released", Subject: "anthropic", Detail: detail, OccurredAt: time.Now()}, time.Now())
	if got := labels(released); got["Claws parked"] != "" || got["Deadline"] != "" || got["Claws resumed"] != "4" {
		t.Fatalf("released fields = %v, want resumed claws and no deadline", got)
	}

	retry := buildInfraMessage(infraEventRow{EventType: "provider_limit_opened", Subject: "anthropic", Detail: map[string]any{"name": "Anthropic", "retry_count": 2, "deadline": "2026-09-02 00:30 UTC"}, OccurredAt: time.Now()}, time.Now())
	if got := labels(retry); got["Retry"] != "2" {
		t.Fatalf("re-latch fields = %v, want the retry number", got)
	}
	if !strings.Contains(strings.ToLower(retry.Body), "still") {
		t.Fatalf("re-latch body reads like a first sighting: %q", retry.Body)
	}
}
