package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestScheduledNotificationSlot(t *testing.T) {
	tests := []struct {
		name     string
		now      string
		schedule types.ScheduledNotificationConfig
		want     string
	}{
		{"utc today", "2025-01-06T10:30:00Z", types.ScheduledNotificationConfig{At: "09:00"}, "2025-01-06T09:00:00Z"},
		{"not yet due", "2025-01-06T08:30:00Z", types.ScheduledNotificationConfig{At: "09:00"}, "2025-01-05T09:00:00Z"},
		{"weekday filter", "2025-01-06T10:30:00Z", types.ScheduledNotificationConfig{At: "09:00", Weekdays: []string{"fri"}}, "2025-01-03T09:00:00Z"},
		{"timezone", "2025-01-06T14:30:00Z", types.ScheduledNotificationConfig{At: "09:00", Timezone: "America/New_York"}, "2025-01-06T14:00:00Z"},
		// 2025-03-09 is the US spring-forward DST transition: 02:30 America/New_York
		// does not exist that day, so the slot must skip it and fall back to the
		// previous day's 02:30 EST rather than silently normalizing to 03:30.
		{"dst spring forward skips nonexistent local time", "2025-03-09T12:00:00Z", types.ScheduledNotificationConfig{At: "02:30", Timezone: "America/New_York"}, "2025-03-08T07:30:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nowAt, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := scheduledNotificationSlot(nowAt, tt.schedule)
			if !ok || got.UTC().Format(time.RFC3339) != tt.want {
				t.Fatalf("slot = %v, %v; want %s", got, ok, tt.want)
			}
		})
	}
}

// newScheduledNotifierTestServer wires a hub whose scheduled notifier posts to
// the given httptest Slack server, mirroring newSlackNotifierTestServer.
func newScheduledNotifierTestServer(t *testing.T, slackURL string, schedules []types.ScheduledNotificationConfig) (*Server, *sql.DB) {
	t.Helper()
	cfg := &types.HubConfig{
		Token:   "test-token",
		Secrets: map[string]string{testNotifierToken: "xoxb-test-token"},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				testNotifierName: {Type: "slack", Settings: map[string]any{
					"token_secret": testNotifierToken,
					"channel":      slackTestChannel,
				}},
				"second-notifier": {Type: "slack", Settings: map[string]any{
					"token_secret": testNotifierToken,
					"channel":      "C0SECOND12",
				}},
			},
			Scheduled: schedules,
		},
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	s.notifierSettingOverrides = map[string]any{
		"api_base":          slackURL,
		"min_send_interval": time.Nanosecond.String(),
	}
	return s, db
}

// ── Manual trigger endpoint ───────────────────────────────────────────────────

// The test endpoint builds the report through the same registry the scheduler
// uses, so what an operator previews is what the next due slot posts. A dry
// run must render the real wire payload without touching Slack, and must not
// advance the scheduler's dedupe state — suppressing the delivery it tests.
func TestScheduledReportTestEndpointDryRunRendersWithoutSending(t *testing.T) {
	registerScheduledReport("endpoint-test-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "Pull requests waiting", Body: "3 open"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "probe", Report: "endpoint-test-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	rr := postSlackTest(t, s, `{"report":"endpoint-test-report","via":"`+testNotifierName+`","dry_run":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		DryRun  bool   `json:"dry_run"`
		Report  string `json:"report"`
		Via     string `json:"via"`
		Payload struct {
			Channel     string `json:"channel"`
			Attachments []struct {
				Fallback string           `json:"fallback"`
				Blocks   []map[string]any `json:"blocks"`
			} `json:"attachments"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.DryRun || resp.Report != "endpoint-test-report" || resp.Via != testNotifierName {
		t.Fatalf("unexpected dry-run response: %s", rr.Body.String())
	}
	if resp.Payload.Channel != slackTestChannel || len(resp.Payload.Attachments) != 1 || len(resp.Payload.Attachments[0].Blocks) == 0 {
		t.Fatalf("dry run did not render the real wire payload: %s", rr.Body.String())
	}
	if !strings.Contains(resp.Payload.Attachments[0].Fallback, "Pull requests waiting") {
		t.Fatalf("payload does not carry the report title: %s", rr.Body.String())
	}
	if fake.count() != 0 {
		t.Fatalf("dry run posted %d messages to Slack, want 0", fake.count())
	}
	if _, found, _ := s.scheduledLastFired("probe", testNotifierName); found {
		t.Fatal("dry run advanced the scheduler's dedupe state")
	}

	// A real send posts, and still leaves the schedule's own slot untouched.
	rr = postSlackTest(t, s, `{"report":"endpoint-test-report","via":"`+testNotifierName+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("real send status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if fake.count() != 1 {
		t.Fatalf("real send posted %d messages, want 1", fake.count())
	}
	if _, found, _ := s.scheduledLastFired("probe", testNotifierName); found {
		t.Fatal("test send advanced the scheduler's dedupe state")
	}
}

// A report with nothing to say is a normal outcome, not a failure: the
// endpoint answers 200 with empty:true so the screen can say "nothing to
// report" instead of painting a red error.
func TestScheduledReportTestEndpointEmptyReportReportsItWithoutSending(t *testing.T) {
	registerScheduledReport("endpoint-empty-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return nil, false, nil
	})
	fake := newFakeSlackServer(t)
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, nil)

	rr := postSlackTest(t, s, `{"report":"endpoint-empty-report","via":"`+testNotifierName+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK    bool `json:"ok"`
		Empty bool `json:"empty"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || !resp.Empty {
		t.Fatalf("empty report response = %s", rr.Body.String())
	}
	if fake.count() != 0 {
		t.Fatalf("empty report posted %d messages, want 0", fake.count())
	}
}

func TestScheduledReportTestEndpointRejectsUnknownReportAndVia(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, nil)

	rr := postSlackTest(t, s, `{"report":"no-such-report","via":"`+testNotifierName+`"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown report = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	// The 400 has to list what IS available, or the operator has nowhere to go.
	if !strings.Contains(rr.Body.String(), "supported reports") {
		t.Fatalf("unknown-report error does not list the supported names: %s", rr.Body.String())
	}

	registerScheduledReport("endpoint-via-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report"}, true, nil
	})
	rr = postSlackTest(t, s, `{"report":"endpoint-via-report","via":"nope"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown via = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	rr = postSlackTest(t, s, `{"report":"endpoint-via-report"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing via = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	if fake.count() != 0 {
		t.Fatalf("a rejected probe still posted %d messages", fake.count())
	}
}

func TestScheduledNotifierEmptyReportMarksFiredWithoutSending(t *testing.T) {
	registerScheduledReport("empty-test-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return nil, false, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "empty", Report: "empty-test-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	// Arm the schedule with a pre-slot tick, as the minutely production loop
	// would: the first tick a schedule is ever seen on only seeds its state.
	s.scheduledNotifierTick(time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))

	nowAt := time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC)
	s.scheduledNotifierTick(nowAt)
	if fake.count() != 0 {
		t.Fatalf("empty report sent %d messages, want 0", fake.count())
	}
	fired, found, err := s.scheduledLastFired("empty", testNotifierName)
	if err != nil || !found {
		t.Fatalf("scheduledLastFired: %v, found=%v", err, found)
	}
	if !fired.Equal(time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("fired slot = %v", fired)
	}

	// A second tick within the same slot must not send anything either.
	s.scheduledNotifierTick(nowAt.Add(time.Minute))
	if fake.count() != 0 {
		t.Fatalf("re-tick within same slot sent %d messages, want 0", fake.count())
	}
}

func TestScheduledNotifierDedupeAcrossSimulatedRestart(t *testing.T) {
	registerScheduledReport("dedupe-test-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "dedupe", Report: "dedupe-test-report", Via: []string{testNotifierName}, At: "09:00"}
	s, db := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	// Arm the schedule with a pre-slot tick (production ticks every minute,
	// so a schedule created before its slot is always seen before it).
	s.scheduledNotifierTick(time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))

	slotTime := time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC)
	s.scheduledNotifierTick(slotTime)
	if fake.count() != 1 {
		t.Fatalf("first tick sent %d messages, want 1", fake.count())
	}

	// Simulate a hub restart: build a fresh Server sharing the same DB so the
	// persisted state table is the only thing carried across (the notifier
	// cache and in-memory mutexes are legitimately empty after a restart).
	restarted := &Server{db: s.db, hubCfg: s.hubCfg, notifierSettingOverrides: s.notifierSettingOverrides}
	_ = db
	restarted.scheduledNotifierTick(slotTime.Add(20 * time.Minute))
	if fake.count() != 1 {
		t.Fatalf("restart re-fired the same slot: got %d sends, want 1", fake.count())
	}

	// A later slot must fire again.
	nextDay := slotTime.AddDate(0, 0, 1)
	restarted.scheduledNotifierTick(nextDay)
	if fake.count() != 2 {
		t.Fatalf("next day's slot sent %d messages, want 2", fake.count())
	}
}

func TestScheduledNotifierPerNotifierPartialFailureRetries(t *testing.T) {
	registerScheduledReport("partial-fail-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		if n == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"ok":true,"ts":"1.000001"}`))
	})
	schedule := types.ScheduledNotificationConfig{
		ID:     "partial",
		Report: "partial-fail-report",
		Via:    []string{testNotifierName, "second-notifier"},
		At:     "09:00",
	}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	// Arm both destinations before the slot: the first tick a schedule is
	// ever seen on only seeds its state, posting nothing to Slack.
	s.scheduledNotifierTick(time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))

	slotTime := time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC)
	slot := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	s.scheduledNotifierTick(slotTime)
	if fake.count() != 2 {
		t.Fatalf("tick sent %d messages, want 2 (one failed, one ok)", fake.count())
	}
	if fired, _, _ := s.scheduledLastFired("partial", testNotifierName); fired.Equal(slot) {
		t.Fatal("failed notifier state was advanced despite a transient error")
	}
	if fired, _, _ := s.scheduledLastFired("partial", "second-notifier"); !fired.Equal(slot) {
		t.Fatalf("succeeding notifier state = %v, want the slot recorded", fired)
	}

	// Next tick, same slot: only the previously-failed destination retries.
	fake.setRespond(nil)
	s.scheduledNotifierTick(slotTime.Add(time.Minute))
	if fake.count() != 3 {
		t.Fatalf("retry tick sent %d messages, want 3 (1 retry)", fake.count())
	}
	if fired, _, _ := s.scheduledLastFired("partial", testNotifierName); !fired.Equal(slot) {
		t.Fatalf("retried notifier state = %v, want the slot recorded after success", fired)
	}
}

// A schedule seen for the first time AFTER its slot passed must wait for the
// next occurrence: the slot search reaches up to 8 days back, so a newly
// created (or newly re-routed) schedule would otherwise replay a stale slot
// the moment it is saved.
func TestScheduledNotifierNewScheduleDoesNotReplayPastSlot(t *testing.T) {
	registerScheduledReport("no-replay-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "fresh", Report: "no-replay-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	// First tick, hours after today's slot: seed, don't send.
	nowAt := time.Date(2025, 1, 6, 15, 0, 0, 0, time.UTC)
	s.scheduledNotifierTick(nowAt)
	if fake.count() != 0 {
		t.Fatalf("first sight of the schedule replayed a past slot: %d sends, want 0", fake.count())
	}
	fired, found, err := s.scheduledLastFired("fresh", testNotifierName)
	if err != nil || !found {
		t.Fatalf("scheduledLastFired: %v, found=%v — first sight must seed the state row", err, found)
	}
	if !fired.Equal(time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("seeded slot = %v, want today's 09:00", fired)
	}

	// The next occurrence fires normally.
	s.scheduledNotifierTick(time.Date(2025, 1, 7, 9, 0, 30, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("next day's slot sent %d messages, want 1", fake.count())
	}
}

// Slots that pass while a schedule is paused are skipped, not queued: the
// re-enable must wait for its next occurrence instead of replaying whatever
// slot last passed during the pause.
func TestScheduledNotifierPausedScheduleSkipsSlotsUntilReEnabled(t *testing.T) {
	registerScheduledReport("pause-skip-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "paused", Report: "pause-skip-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	// Arm and fire once while enabled.
	s.scheduledNotifierTick(time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))
	s.scheduledNotifierTick(time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("enabled slot sent %d messages, want 1", fake.count())
	}

	// Pause; the next day's slot passes silently but advances the state.
	disabled := false
	s.hubCfg.Notifications.Scheduled[0].Enabled = &disabled
	s.scheduledNotifierTick(time.Date(2025, 1, 7, 9, 30, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("paused slot sent %d messages, want still 1", fake.count())
	}

	// Re-enable after the slot: nothing to replay, next occurrence fires.
	enabled := true
	s.hubCfg.Notifications.Scheduled[0].Enabled = &enabled
	s.scheduledNotifierTick(time.Date(2025, 1, 7, 15, 0, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("re-enable replayed the slot that passed while paused: %d sends, want 1", fake.count())
	}
	s.scheduledNotifierTick(time.Date(2025, 1, 8, 9, 0, 30, 0, time.UTC))
	if fake.count() != 2 {
		t.Fatalf("next slot after re-enable sent %d messages, want 2", fake.count())
	}
}
