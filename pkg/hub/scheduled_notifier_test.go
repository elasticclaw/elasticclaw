package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
		// does not exist that day, so the slot uses the normalized post-gap
		// instant (03:30 EDT) instead of skipping the day — skipping would make
		// a sun-only 02:30 schedule silently drop a full weekly cycle.
		{"dst spring forward uses the normalized post-gap instant", "2025-03-09T12:00:00Z", types.ScheduledNotificationConfig{At: "02:30", Timezone: "America/New_York"}, "2025-03-09T07:30:00Z"},
		{"dst spring forward on the only allowed weekday still yields that day", "2025-03-09T12:00:00Z", types.ScheduledNotificationConfig{At: "02:30", Timezone: "America/New_York", Weekdays: []string{"sun"}}, "2025-03-09T07:30:00Z"},
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
	if _, _, found, _ := s.scheduledLastFired("probe", testNotifierName); found {
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
	if _, _, found, _ := s.scheduledLastFired("probe", testNotifierName); found {
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
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))

	nowAt := time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC)
	s.scheduledNotifierTick(context.Background(), nowAt)
	if fake.count() != 0 {
		t.Fatalf("empty report sent %d messages, want 0", fake.count())
	}
	fired, _, found, err := s.scheduledLastFired("empty", testNotifierName)
	if err != nil || !found {
		t.Fatalf("scheduledLastFired: %v, found=%v", err, found)
	}
	if !fired.Equal(time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("fired slot = %v", fired)
	}

	// A second tick within the same slot must not send anything either.
	s.scheduledNotifierTick(context.Background(), nowAt.Add(time.Minute))
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
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))

	slotTime := time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC)
	s.scheduledNotifierTick(context.Background(), slotTime)
	if fake.count() != 1 {
		t.Fatalf("first tick sent %d messages, want 1", fake.count())
	}

	// Simulate a hub restart: build a fresh Server sharing the same DB so the
	// persisted state table is the only thing carried across (the notifier
	// cache and in-memory mutexes are legitimately empty after a restart).
	restarted := &Server{db: s.db, hubCfg: s.hubCfg, notifierSettingOverrides: s.notifierSettingOverrides}
	_ = db
	restarted.scheduledNotifierTick(context.Background(), slotTime.Add(20*time.Minute))
	if fake.count() != 1 {
		t.Fatalf("restart re-fired the same slot: got %d sends, want 1", fake.count())
	}

	// A later slot must fire again.
	nextDay := slotTime.AddDate(0, 0, 1)
	restarted.scheduledNotifierTick(context.Background(), nextDay)
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
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))

	slotTime := time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC)
	slot := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	s.scheduledNotifierTick(context.Background(), slotTime)
	if fake.count() != 2 {
		t.Fatalf("tick sent %d messages, want 2 (one failed, one ok)", fake.count())
	}
	if fired, _, _, _ := s.scheduledLastFired("partial", testNotifierName); fired.Equal(slot) {
		t.Fatal("failed notifier state was advanced despite a transient error")
	}
	if fired, _, _, _ := s.scheduledLastFired("partial", "second-notifier"); !fired.Equal(slot) {
		t.Fatalf("succeeding notifier state = %v, want the slot recorded", fired)
	}

	// Next tick, same slot: only the previously-failed destination retries.
	fake.setRespond(nil)
	s.scheduledNotifierTick(context.Background(), slotTime.Add(time.Minute))
	if fake.count() != 3 {
		t.Fatalf("retry tick sent %d messages, want 3 (1 retry)", fake.count())
	}
	if fired, _, _, _ := s.scheduledLastFired("partial", testNotifierName); !fired.Equal(slot) {
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
	s.scheduledNotifierTick(context.Background(), nowAt)
	if fake.count() != 0 {
		t.Fatalf("first sight of the schedule replayed a past slot: %d sends, want 0", fake.count())
	}
	fired, _, found, err := s.scheduledLastFired("fresh", testNotifierName)
	if err != nil || !found {
		t.Fatalf("scheduledLastFired: %v, found=%v — first sight must seed the state row", err, found)
	}
	if !fired.Equal(time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("seeded slot = %v, want today's 09:00", fired)
	}

	// The next occurrence fires normally.
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 7, 9, 0, 30, 0, time.UTC))
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
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("enabled slot sent %d messages, want 1", fake.count())
	}

	// Pause; the next day's slot passes silently but advances the state.
	disabled := false
	s.hubCfg.Notifications.Scheduled[0].Enabled = &disabled
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 7, 9, 30, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("paused slot sent %d messages, want still 1", fake.count())
	}

	// Re-enable after the slot: nothing to replay, next occurrence fires.
	enabled := true
	s.hubCfg.Notifications.Scheduled[0].Enabled = &enabled
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 7, 15, 0, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("re-enable replayed the slot that passed while paused: %d sends, want 1", fake.count())
	}
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 8, 9, 0, 30, 0, time.UTC))
	if fake.count() != 2 {
		t.Fatalf("next slot after re-enable sent %d messages, want 2", fake.count())
	}
}

// Editing a schedule's slot definition (at/timezone/weekdays) must not fire
// an off-schedule send against the pre-edit state: the state row is reseeded
// like a first sight and the edited schedule delivers from its next slot on.
func TestScheduledNotifierEditReseedsSlotState(t *testing.T) {
	registerScheduledReport("edit-reseed-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "edited", Report: "edit-reseed-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	// Arm and fire once under the original definition.
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("original slot sent %d messages, want 1", fake.count())
	}

	// Edit the slot: the next tick sees a digest mismatch and reseeds instead
	// of treating today's new 10:00 slot as pending against the old history.
	s.hubCfg.Notifications.Scheduled[0].At = "10:00"
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 10, 30, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("edit fired an off-schedule send: %d sends, want still 1", fake.count())
	}
	fired, _, found, err := s.scheduledLastFired("edited", testNotifierName)
	if err != nil || !found {
		t.Fatalf("scheduledLastFired: %v, found=%v — the edit must reseed the state row", err, found)
	}
	if !fired.Equal(time.Date(2025, 1, 6, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("reseeded slot = %v, want today's 10:00", fired)
	}

	// The edited schedule's next occurrence fires normally.
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 7, 10, 0, 30, 0, time.UTC))
	if fake.count() != 2 {
		t.Fatalf("next slot after the edit sent %d messages, want 2", fake.count())
	}
}

// A destination whose sends keep failing transiently retries once per tick,
// but only up to scheduledMaxTransientFailures: then the slot is advanced
// with a permanent-failure log so the identical report is not re-posted every
// minute for the rest of the day.
func TestScheduledNotifierTransientFailureCapAdvancesSlot(t *testing.T) {
	registerScheduledReport("retry-cap-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		http.Error(w, "temporary", http.StatusInternalServerError)
	})
	schedule := types.ScheduledNotificationConfig{ID: "capped", Report: "retry-cap-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))
	slot := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	for i := 0; i < scheduledMaxTransientFailures; i++ {
		s.scheduledNotifierTick(context.Background(), slot.Add(time.Duration(30+i)*time.Minute))
		fired, _, _, _ := s.scheduledLastFired("capped", testNotifierName)
		if advanced := fired.Equal(slot); advanced != (i == scheduledMaxTransientFailures-1) {
			t.Fatalf("after failure %d state advanced=%v, want advance only on the %dth", i+1, advanced, scheduledMaxTransientFailures)
		}
	}
	if fake.count() != scheduledMaxTransientFailures {
		t.Fatalf("capped destination attempted %d sends, want %d", fake.count(), scheduledMaxTransientFailures)
	}

	// The slot is burned: later ticks in the same slot retry nothing more.
	s.scheduledNotifierTick(context.Background(), slot.Add(3*time.Hour))
	if fake.count() != scheduledMaxTransientFailures {
		t.Fatalf("burned slot still retried: %d sends", fake.count())
	}

	// The next slot starts with a fresh retry budget and can succeed.
	fake.setRespond(nil)
	s.scheduledNotifierTick(context.Background(), slot.AddDate(0, 0, 1).Add(30*time.Minute))
	if fake.count() != scheduledMaxTransientFailures+1 {
		t.Fatalf("next slot sent %d messages total, want %d", fake.count(), scheduledMaxTransientFailures+1)
	}
}

// Dedupe rows of schedules (or destinations) that leave the config are
// pruned, mirroring pruneLifecycleRouteState: a schedule id deleted and later
// re-created must behave like a brand-new schedule, and orphaned rows must
// not accumulate in slack_notifier_state.
func TestScheduledNotifierPrunesRemovedScheduleState(t *testing.T) {
	registerScheduledReport("prune-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "pruned", Report: "prune-report", Via: []string{testNotifierName}, At: "09:00"}
	s, db := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 15, 0, 0, 0, time.UTC))
	if _, _, found, _ := s.scheduledLastFired("pruned", testNotifierName); !found {
		t.Fatal("first sight did not seed the state row")
	}
	// A row in a superseded key format is orphaned by definition and pruned too.
	if _, err := db.Exec(`INSERT INTO slack_notifier_state(key, value) VALUES(?,?)`,
		"scheduled:last_fired:pruned:"+testNotifierName, "2025-01-06T09:00:00Z"); err != nil {
		t.Fatal(err)
	}

	s.hubCfg.Notifications.Scheduled = nil
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 15, 1, 0, 0, time.UTC))
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM slack_notifier_state WHERE key LIKE 'scheduled:last_fired:%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d scheduled state rows survive the schedule's removal, want 0", n)
	}

	// Re-created under the same id: first sight seeds, nothing replays.
	s.hubCfg.Notifications.Scheduled = []types.ScheduledNotificationConfig{schedule}
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 15, 2, 0, 0, time.UTC))
	if fake.count() != 0 {
		t.Fatalf("re-created schedule replayed a stale slot: %d sends, want 0", fake.count())
	}
}

// A deterministic panic in one schedule's report builder must be contained to
// that schedule: the tick-level recover alone unwinds the whole loop and
// permanently starves every schedule after the panicking one in config order.
// The panicking schedule's slot is burned so it does not also log a stack
// trace every minute.
func TestScheduledNotifierPanickingBuilderDoesNotStarveLaterSchedules(t *testing.T) {
	panics := 0
	registerScheduledReport("panicking-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		panics++
		panic("builder bug")
	})
	registerScheduledReport("healthy-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedules := []types.ScheduledNotificationConfig{
		{ID: "bad", Report: "panicking-report", Via: []string{testNotifierName}, At: "09:00"},
		{ID: "good", Report: "healthy-report", Via: []string{testNotifierName}, At: "09:00"},
	}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, schedules)

	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("schedule after the panicking one sent %d messages, want 1", fake.count())
	}
	slot := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	if fired, _, _, _ := s.scheduledLastFired("bad", testNotifierName); !fired.Equal(slot) {
		t.Fatalf("panicking schedule's slot = %v, want it burned to %v", fired, slot)
	}

	// The burned slot stops the minutely re-panic for the rest of the day.
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 9, 31, 0, 0, time.UTC))
	if panics != 1 {
		t.Fatalf("builder panicked %d times, want 1 (slot burned after the first)", panics)
	}
}

// An invalid scheduled block (here: two schedules sharing an id, which would
// map to one dedupe row and mutually reseed each other forever) pauses the
// tick with a warning instead of running live, mirroring the lifecycle tick's
// ValidateNotificationsConfig gate.
func TestScheduledNotifierInvalidConfigPausesDeliveries(t *testing.T) {
	registerScheduledReport("invalid-config-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedules := []types.ScheduledNotificationConfig{
		{ID: "daily", Report: "invalid-config-report", Via: []string{testNotifierName}, At: "09:00"},
		{ID: "daily", Report: "invalid-config-report", Via: []string{testNotifierName}, At: "17:00"},
	}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, schedules)

	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))
	if _, _, found, _ := s.scheduledLastFired("daily", testNotifierName); found {
		t.Fatal("invalid config still seeded dedupe state")
	}
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC))
	if fake.count() != 0 {
		t.Fatalf("invalid config still delivered %d messages, want 0", fake.count())
	}

	// Repairing the config resumes the tick (the first sight seeds, as ever).
	s.hubCfg.Notifications.Scheduled[1].ID = "evening"
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 9, 31, 0, 0, time.UTC))
	if _, _, found, _ := s.scheduledLastFired("daily", testNotifierName); !found {
		t.Fatal("repaired config did not resume seeding")
	}
}

// A via with surrounding whitespace resolves and keys state by its trimmed
// name, matching the lifecycle runtime and doctor: the raw name would miss the
// notifier map and burn one slot per day as "permanently failed" while doctor
// reports the schedule green.
func TestScheduledNotifierTrimsViaWhitespace(t *testing.T) {
	registerScheduledReport("trim-via-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "trimmed", Report: "trim-via-report", Via: []string{"  " + testNotifierName + " "}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))
	if _, _, found, _ := s.scheduledLastFired("trimmed", testNotifierName); !found {
		t.Fatal("state row is not keyed by the trimmed via")
	}
	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC))
	if fake.count() != 1 {
		t.Fatalf("untrimmed via sent %d messages, want 1", fake.count())
	}
}

// A notifier that cannot be constructed at the slot minute (a secret briefly
// absent during rotation) retries on the transient budget instead of burning
// the slot on the first miss, mirroring the lifecycle notifier's
// hold-until-buildable — but still bounded by the cap.
func TestScheduledNotifierConstructionFailureRetriesInsteadOfBurningSlot(t *testing.T) {
	registerScheduledReport("construction-fail-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return &notify.Message{Title: "report", Body: "body"}, true, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "rotating", Report: "construction-fail-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))

	// The bot-token secret vanishes mid-rotation: construction fails.
	secrets := s.hubCfg.Secrets
	s.hubCfg.Secrets = map[string]string{}
	slot := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	s.scheduledNotifierTick(context.Background(), slot.Add(30*time.Minute))
	if fake.count() != 0 {
		t.Fatalf("unbuildable notifier sent %d messages, want 0", fake.count())
	}
	if fired, _, _, _ := s.scheduledLastFired("rotating", testNotifierName); fired.Equal(slot) {
		t.Fatal("construction failure burned the slot instead of retrying")
	}

	// The secret is restored one minute later — well inside the retry budget.
	s.hubCfg.Secrets = secrets
	s.scheduledNotifierTick(context.Background(), slot.Add(31*time.Minute))
	if fake.count() != 1 {
		t.Fatalf("restored notifier sent %d messages, want 1", fake.count())
	}
	if fired, _, _, _ := s.scheduledLastFired("rotating", testNotifierName); !fired.Equal(slot) {
		t.Fatalf("state = %v, want the slot recorded after the retried send", fired)
	}
}

// A persistently erroring report builder gets the same bounded retry budget as
// a failing send: without it, fired never advances and the builder re-runs and
// logs the identical line every minute for the life of the process.
func TestScheduledNotifierBuildFailureRetriesThenBurnsSlot(t *testing.T) {
	builds := 0
	registerScheduledReport("build-fail-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		builds++
		return nil, false, errors.New("tenants table is empty")
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "broken-build", Report: "build-fail-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

	s.scheduledNotifierTick(context.Background(), time.Date(2025, 1, 6, 8, 30, 0, 0, time.UTC))
	slot := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	for i := 0; i < scheduledMaxTransientFailures; i++ {
		s.scheduledNotifierTick(context.Background(), slot.Add(time.Duration(30+i)*time.Minute))
		fired, _, _, _ := s.scheduledLastFired("broken-build", testNotifierName)
		if advanced := fired.Equal(slot); advanced != (i == scheduledMaxTransientFailures-1) {
			t.Fatalf("after build failure %d state advanced=%v, want advance only on the %dth", i+1, advanced, scheduledMaxTransientFailures)
		}
	}
	if builds != scheduledMaxTransientFailures {
		t.Fatalf("builder ran %d times, want %d", builds, scheduledMaxTransientFailures)
	}

	// The slot is burned: later ticks in the same slot rebuild nothing.
	s.scheduledNotifierTick(context.Background(), slot.Add(3*time.Hour))
	if builds != scheduledMaxTransientFailures {
		t.Fatalf("burned slot still rebuilt: %d builds", builds)
	}
	if fake.count() != 0 {
		t.Fatalf("failing builds still sent %d messages", fake.count())
	}
}

// Semantically identical slot definitions must produce identical digests:
// normalizeScheduledTimes zero-pads a hand-written "9:00" on any settings
// save, and the edit dialog seeds an absent timezone as "UTC" — neither
// rewrite is an edit, and reading one back as a digest mismatch would reseed
// the row and silently skip a pending or mid-retry slot.
func TestScheduledSlotDigestNormalizesEquivalentRepresentations(t *testing.T) {
	base := scheduledSlotDigest(types.ScheduledNotificationConfig{At: "09:00", Timezone: "UTC"})
	if got := scheduledSlotDigest(types.ScheduledNotificationConfig{At: "9:00"}); got != base {
		t.Fatalf("unpadded at / empty timezone digest = %q, want %q", got, base)
	}
	if scheduledSlotDigest(types.ScheduledNotificationConfig{At: "10:00", Timezone: "UTC"}) == base {
		t.Fatal("a real at edit must change the digest")
	}
	if scheduledSlotDigest(types.ScheduledNotificationConfig{At: "09:00", Timezone: "America/Sao_Paulo"}) == base {
		t.Fatal("a real timezone edit must change the digest")
	}
	if scheduledSlotDigest(types.ScheduledNotificationConfig{At: "09:00", Timezone: "UTC", Weekdays: []string{"mon"}}) == base {
		t.Fatal("a weekday edit must change the digest")
	}
}

// pruneScheduledState clears the in-memory failure streaks of removed pairs
// too, mirroring clearLifecycleTransientFailures: a pair removed mid-streak
// would otherwise leak its entry for the process lifetime and hand a same-slot
// re-add a retry budget already spent.
func TestPruneScheduledStateClearsRemovedStreaks(t *testing.T) {
	fake := newFakeSlackServer(t)
	kept := types.ScheduledNotificationConfig{ID: "kept", Report: "prune-streak-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{kept})

	slot := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	s.noteScheduledTransientFailure("kept", testNotifierName, slot)
	s.noteScheduledTransientFailure("kept", scheduledBuildFailureVia, slot)
	s.noteScheduledTransientFailure("gone", testNotifierName, slot)

	s.pruneScheduledState([]types.ScheduledNotificationConfig{kept})
	if _, ok := s.scheduledTransientFailures[scheduledStateKey("gone", testNotifierName)]; ok {
		t.Fatal("removed pair's streak survived the prune")
	}
	if _, ok := s.scheduledTransientFailures[scheduledStateKey("kept", testNotifierName)]; !ok {
		t.Fatal("configured pair's send streak was pruned")
	}
	if _, ok := s.scheduledTransientFailures[scheduledStateKey("kept", scheduledBuildFailureVia)]; !ok {
		t.Fatal("configured schedule's build streak was pruned")
	}
}
