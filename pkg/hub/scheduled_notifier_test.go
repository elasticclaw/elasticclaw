package hub

import (
	"context"
	"database/sql"
	"net/http"
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

func TestScheduledNotifierEmptyReportMarksFiredWithoutSending(t *testing.T) {
	registerScheduledReport("empty-test-report", func(context.Context, *Server) (*notify.Message, bool, error) {
		return nil, false, nil
	})
	fake := newFakeSlackServer(t)
	schedule := types.ScheduledNotificationConfig{ID: "empty", Report: "empty-test-report", Via: []string{testNotifierName}, At: "09:00"}
	s, _ := newScheduledNotifierTestServer(t, fake.server.URL, []types.ScheduledNotificationConfig{schedule})

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

	slotTime := time.Date(2025, 1, 6, 9, 30, 0, 0, time.UTC)
	s.scheduledNotifierTick(slotTime)
	if fake.count() != 2 {
		t.Fatalf("tick sent %d messages, want 2 (one failed, one ok)", fake.count())
	}
	if _, found, _ := s.scheduledLastFired("partial", testNotifierName); found {
		t.Fatal("failed notifier state was advanced despite a transient error")
	}
	if _, found, _ := s.scheduledLastFired("partial", "second-notifier"); !found {
		t.Fatal("succeeding notifier state was not recorded")
	}

	// Next tick, same slot: only the previously-failed destination retries.
	fake.setRespond(nil)
	s.scheduledNotifierTick(slotTime.Add(time.Minute))
	if fake.count() != 3 {
		t.Fatalf("retry tick sent %d messages, want 3 (1 retry)", fake.count())
	}
	if _, found, _ := s.scheduledLastFired("partial", testNotifierName); !found {
		t.Fatal("retried notifier state was not recorded after success")
	}
}
