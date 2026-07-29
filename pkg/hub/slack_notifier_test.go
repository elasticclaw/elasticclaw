package hub

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const slackTestChannel = "C0TEST1234"

func newSlackNotifierTestServer(t *testing.T, slackURL string, mutate func(*types.SlackNotificationsConfig)) (*Server, *sql.DB) {
	t.Helper()
	slackCfg := &types.SlackNotificationsConfig{
		Enabled:     true,
		BotTokenRef: "slack_bot_token",
		Channel:     slackTestChannel,
	}
	if mutate != nil {
		mutate(slackCfg)
	}
	cfg := &types.HubConfig{
		Token:         "test-token",
		Secrets:       map[string]string{"slack_bot_token": "xoxb-test-token"},
		Notifications: &types.NotificationsConfig{Slack: slackCfg},
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	s.slackBaseURL = slackURL
	s.slackSendInterval = time.Nanosecond
	return s, db
}

func insertSlackTestEvent(t *testing.T, db *sql.DB, id, runID, eventType string, observedAt int64, targetURL, failureType, detail string) {
	t.Helper()
	if detail == "" {
		detail = "{}"
	}
	_, err := db.Exec(`
		INSERT INTO task_run_events(
			id, tenant_id, run_id, attempt_id, event_key, source, event_type, event_time, observed_at,
			actor_type, actor_login, interaction_role, target_type, target_id, target_url,
			warning_type, failure_type, detail, created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "test-tenant-id", runID, "attempt-"+runID, "key-"+id, taskRunSourceHub, eventType,
		observedAt, observedAt, taskRunActorSystem, "", taskRunInteractionNeutral,
		"", "", targetURL, "", failureType, detail, observedAt,
	)
	if err != nil {
		t.Fatalf("insert slack test event %s: %v", id, err)
	}
}

// setSlackWatermark positions the rowid cursor; 0 means "process every event
// inserted from now on" for a test DB with no prior notifiable events.
func setSlackWatermark(t *testing.T, s *Server, rowid int64) {
	t.Helper()
	s.setSlackStateInt64(slackStateWatermarkKey, rowid)
}

func slackWatermark(t *testing.T, s *Server) (int64, bool) {
	t.Helper()
	wm, found, err := s.slackStateInt64(slackStateWatermarkKey)
	if err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	return wm, found
}

func slackDeliveryStatus(t *testing.T, db *sql.DB, eventID string) (string, bool) {
	t.Helper()
	var status string
	err := db.QueryRow(`SELECT status FROM slack_notification_deliveries WHERE event_id=?`, eventID).Scan(&status)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("query delivery %s: %v", eventID, err)
	}
	return status, true
}

func TestSlackNotifierDedupesOnCursorRescan(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", Model: "anthropic/claude", StartedAt: base - 1000,
		IssueTitle: "Fix login bug",
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	setSlackWatermark(t, s, 0)

	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("first tick sent %d messages, want 1", fake.count())
	}
	if status, ok := slackDeliveryStatus(t, db, "ev-started"); !ok || status != slackDeliveryStatusSent {
		t.Fatalf("delivery status = %q, %v", status, ok)
	}
	if wm, found := slackWatermark(t, s); !found || wm <= 0 {
		t.Fatalf("watermark = %d, %v, want advanced past the event", wm, found)
	}

	// Rewind the cursor so the event is re-scanned (as after a crash between
	// send and watermark advance); the delivery record must prevent a
	// duplicate send.
	setSlackWatermark(t, s, 0)
	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("re-scan caused a duplicate send: %d messages", fake.count())
	}

	req := fake.request(0)
	if req.Channel != slackTestChannel {
		t.Fatalf("channel = %q", req.Channel)
	}
	if !strings.Contains(req.Text, "Agent started") || !strings.Contains(req.Text, "Fix login bug") {
		t.Fatalf("fallback text = %q", req.Text)
	}
	if len(req.Blocks) == 0 {
		t.Fatal("message has no blocks")
	}
}

func TestSlackNotifierThreadsEventsByRun(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	insertSlackTestEvent(t, db, "ev-pr", "run-1", taskRunEventPROpened, base+20,
		"https://github.com/acme/app/pull/7", "", `{"repo":"acme/app","prNumber":7,"url":"https://github.com/acme/app/pull/7"}`)
	setSlackWatermark(t, s, 0)

	s.slackNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("sent %d messages, want 2", fake.count())
	}
	root := fake.request(0)
	reply := fake.request(1)
	if root.ThreadTS != "" {
		t.Fatalf("agent_started should be the thread root, got thread_ts %q", root.ThreadTS)
	}
	if reply.ThreadTS == "" {
		t.Fatal("pr_opened was not threaded under the run root")
	}
	var threadTS string
	if err := db.QueryRow(`SELECT thread_ts FROM slack_run_threads WHERE run_id='run-1'`).Scan(&threadTS); err != nil {
		t.Fatalf("thread root row: %v", err)
	}
	if reply.ThreadTS != threadTS {
		t.Fatalf("reply thread_ts %q != stored root %q", reply.ThreadTS, threadTS)
	}
	if !strings.Contains(reply.Text, "PR opened") || !strings.Contains(reply.Text, "acme/app#7") {
		t.Fatalf("pr_opened fallback = %q", reply.Text)
	}
}

func TestSlackNotifierDisabledEventToggle(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.SlackNotificationsConfig) {
		cfg.Events = &types.SlackEventToggles{AgentStarted: boolPtr(false)}
	})

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	insertSlackTestEvent(t, db, "ev-pr", "run-1", taskRunEventPROpened, base+20,
		"https://github.com/acme/app/pull/7", "", "")
	setSlackWatermark(t, s, 0)

	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("sent %d messages, want only pr_opened", fake.count())
	}
	req := fake.request(0)
	if !strings.Contains(req.Text, "PR opened") {
		t.Fatalf("unexpected message: %q", req.Text)
	}
	if req.ThreadTS != "" {
		t.Fatal("pr_opened without a prior root should post top-level")
	}
	// With no agent_started root, the pr_opened message becomes the run's root.
	var threadTS string
	if err := db.QueryRow(`SELECT thread_ts FROM slack_run_threads WHERE run_id='run-1'`).Scan(&threadTS); err != nil {
		t.Fatalf("thread root row: %v", err)
	}
	if _, delivered := slackDeliveryStatus(t, db, "ev-started"); delivered {
		t.Fatal("disabled agent_started event should not have a delivery row")
	}
}

func TestSlackNotifierFirstRunDoesNotReplayHistory(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	nowMs := epochMillis(now())
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-old", AttemptID: "attempt-run-old", ClawID: "claw-old", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: nowMs - 3600_000,
	})
	// Pre-existing history from before the feature was enabled.
	insertSlackTestEvent(t, db, "ev-old", "run-old", taskRunEventAgentStarted, nowMs-1800_000, "", "", "")

	// First tick initializes the cursor at the end of the stream and sends nothing.
	s.slackNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("first run replayed history: %d messages", fake.count())
	}
	if _, found := slackWatermark(t, s); !found {
		t.Fatal("first tick did not persist a watermark")
	}

	// A second tick must not pick up the pre-existing event either.
	s.slackNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("second tick replayed pre-existing rows: %d messages", fake.count())
	}

	// A genuinely new event is delivered.
	insertSlackTestEvent(t, db, "ev-new", "run-old", taskRunEventPROpened, nowMs+60_000,
		"https://github.com/acme/app/pull/9", "", "")
	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("new event after enable was not sent: %d messages", fake.count())
	}
}

func TestSlackNotifierPermanentFailureNotRetried(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		// A message-level permanent error: this payload will never send.
		fmt.Fprint(w, `{"ok":false,"error":"invalid_blocks"}`)
	})
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	setSlackWatermark(t, s, 0)

	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("sent %d requests, want 1", fake.count())
	}
	if status, ok := slackDeliveryStatus(t, db, "ev-started"); !ok || status != slackDeliveryStatusFailed {
		t.Fatalf("delivery status = %q, %v; want failed", status, ok)
	}

	// Permanent failures must not be retried on subsequent ticks.
	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("permanent failure was retried: %d requests", fake.count())
	}
}

func TestSlackNotifierTransientFailureRetriedNextTick(t *testing.T) {
	fake := newFakeSlackServer(t)
	fail := true
	fake.setRespond(func(n int, w http.ResponseWriter) {
		if fail {
			http.Error(w, "upstream hiccup", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"ok":true,"ts":"2000.1"}`)
	})
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	setSlackWatermark(t, s, 0)

	s.slackNotifierTick()
	if _, delivered := slackDeliveryStatus(t, db, "ev-started"); delivered {
		t.Fatal("transient failure must not record a delivery")
	}
	if wm, _ := slackWatermark(t, s); wm != 0 {
		t.Fatalf("watermark advanced past a transiently-failed event: %d", wm)
	}

	fail = false
	s.slackNotifierTick()
	if status, ok := slackDeliveryStatus(t, db, "ev-started"); !ok || status != slackDeliveryStatusSent {
		t.Fatalf("retry did not deliver: status = %q, %v", status, ok)
	}
}

func TestSlackNotifierNoopWhenDisabled(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.SlackNotificationsConfig) {
		cfg.Enabled = false
	})

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")

	s.slackNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("disabled notifier sent %d messages", fake.count())
	}
	if _, found := slackWatermark(t, s); found {
		t.Fatal("disabled notifier should not initialize a watermark")
	}
}

func setSlackEnabled(t *testing.T, s *Server, enabled bool) {
	t.Helper()
	s.mu.Lock()
	s.hubCfg.Notifications.Slack.Enabled = enabled
	s.mu.Unlock()
}

// Regression: a burst larger than the batch size must not wedge the cursor.
// With the old observed_at watermark, the second tick re-selected the same
// (already delivered) 200 rows, the watermark never advanced and every later
// event was silently dropped forever.
func TestSlackNotifierBurstLargerThanBatchDoesNotWedge(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: base - 1000,
	})
	total := slackBatchSize + 50
	for i := 0; i < total; i++ {
		insertSlackTestEvent(t, db, fmt.Sprintf("ev-%03d", i), "run-1", taskRunEventAgentStarted, base+int64(i), "", "", "")
	}
	setSlackWatermark(t, s, 0)

	s.slackNotifierTick()
	if fake.count() != slackBatchSize {
		t.Fatalf("first tick sent %d messages, want %d", fake.count(), slackBatchSize)
	}
	s.slackNotifierTick()
	if fake.count() != total {
		t.Fatalf("second tick left the cursor wedged: %d messages sent, want %d", fake.count(), total)
	}

	// Events created after the burst must still be delivered.
	insertSlackTestEvent(t, db, "ev-later", "run-1", taskRunEventAgentStarted, base+3600_000, "", "", "")
	s.slackNotifierTick()
	if fake.count() != total+1 {
		t.Fatalf("event after the burst was not delivered: %d messages", fake.count())
	}
}

// Regression: observed_at carries authoritative provider timestamps (e.g. a
// pr_opened found by the 24h catch-up poller carries the PR's creation time),
// so an event inserted now with an observed_at hours in the past must still
// be delivered. The old observed_at watermark dropped it silently.
func TestSlackNotifierDeliversBackdatedObservedAt(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	nowMs := epochMillis(now())
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: nowMs - 1000,
	})
	// First tick initializes the cursor at the end of the stream.
	s.slackNotifierTick()

	// A pr_opened recorded now but carrying the PR's real (3h old) creation time.
	insertSlackTestEvent(t, db, "ev-backdated", "run-1", taskRunEventPROpened, nowMs-3*3600_000,
		"https://github.com/acme/app/pull/7", "", "")
	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("backdated event was not delivered: %d messages", fake.count())
	}
	if status, ok := slackDeliveryStatus(t, db, "ev-backdated"); !ok || status != slackDeliveryStatusSent {
		t.Fatalf("delivery status = %q, %v", status, ok)
	}
}

// Regression: a configuration-level Slack failure (rotated token, bot kicked
// from the channel) must pause delivery, not burn each event as failed —
// otherwise every notification generated before the operator fixes the
// config is permanently lost.
func TestSlackNotifierConfigErrorPausesAndResumes(t *testing.T) {
	fake := newFakeSlackServer(t)
	broken := true
	fake.setRespond(func(n int, w http.ResponseWriter) {
		if broken {
			fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"ts":"3000.%06d"}`, n)
	})
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	for i := 0; i < 3; i++ {
		insertSlackTestEvent(t, db, fmt.Sprintf("ev-%d", i), "run-1", taskRunEventAgentStarted, base+int64(i), "", "", "")
	}
	setSlackWatermark(t, s, 0)

	s.slackNotifierTick()
	s.slackNotifierTick()
	for i := 0; i < 3; i++ {
		if _, delivered := slackDeliveryStatus(t, db, fmt.Sprintf("ev-%d", i)); delivered {
			t.Fatalf("config-level failure burned event ev-%d as delivered", i)
		}
	}
	if wm, _ := slackWatermark(t, s); wm != 0 {
		t.Fatalf("watermark advanced past events failed by a config error: %d", wm)
	}

	// Operator fixes the token: everything is delivered.
	broken = false
	s.slackNotifierTick()
	for i := 0; i < 3; i++ {
		if status, ok := slackDeliveryStatus(t, db, fmt.Sprintf("ev-%d", i)); !ok || status != slackDeliveryStatusSent {
			t.Fatalf("event ev-%d not delivered after the config was fixed: %q, %v", i, status, ok)
		}
	}
}

// Regression: an unreadable watermark (here: a corrupted value; in production
// also SQLITE_BUSY) must not be treated as a first run — resetting the cursor
// would silently skip the pending backlog.
func TestSlackNotifierUnreadableStateDoesNotResetCursor(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	if _, err := db.Exec(`INSERT INTO slack_notifier_state(key, value) VALUES(?, 'not-a-number')`, slackStateWatermarkKey); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}

	s.slackNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("tick with unreadable state sent %d messages", fake.count())
	}
	var raw string
	if err := db.QueryRow(`SELECT value FROM slack_notifier_state WHERE key=?`, slackStateWatermarkKey).Scan(&raw); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if raw != "not-a-number" {
		t.Fatalf("unreadable state was overwritten with %q — the backlog would be skipped", raw)
	}

	// Once the state is readable again, the backlog is still there.
	setSlackWatermark(t, s, 0)
	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("backlog lost after state recovery: %d messages", fake.count())
	}
}

// Regression: while Slack is disabled the cursor must keep moving, so
// re-enabling behaves like a fresh enable instead of flushing the backlog
// accumulated during the disabled window into the channel at once.
func TestSlackNotifierParksCursorWhileDisabled(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	// First tick initializes the cursor.
	s.slackNotifierTick()

	// Operator mutes Slack; events accumulate while it is off.
	setSlackEnabled(t, s, false)
	for i := 0; i < 3; i++ {
		insertSlackTestEvent(t, db, fmt.Sprintf("ev-muted-%d", i), "run-1", taskRunEventAgentStarted, base+int64(i), "", "", "")
		s.slackNotifierTick()
	}

	// Re-enabling must not dump the muted window's backlog.
	setSlackEnabled(t, s, true)
	s.slackNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("re-enable flushed %d stale messages from the disabled window", fake.count())
	}

	// New events after re-enable are delivered normally.
	insertSlackTestEvent(t, db, "ev-after", "run-1", taskRunEventAgentStarted, base+100, "", "", "")
	s.slackNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("event after re-enable was not delivered: %d messages", fake.count())
	}
}

// Regression: failure reasons can be up to 6000 chars (failureSummaryInputLimit)
// but a Slack section text object caps at 3000 — an oversized block fails the
// whole message with invalid_blocks and the alert is dropped.
func TestSlackFailureMessageClampsLongReason(t *testing.T) {
	ev := slackEventRow{
		EventType:   taskRunFailureBootstrapFailed,
		FailureType: taskRunFailureBootstrapFailed,
		Detail:      map[string]any{"reason": strings.Repeat("x", 6000)},
	}
	blocks, _ := buildSlackMessage(ev, slackRunContext{IssueID: "ISSUE-1", IssueTitle: "Broken build"})
	for i, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			t.Fatalf("block %d has unexpected shape %T", i, b)
		}
		if text, ok := block["text"].(map[string]any); ok {
			if got := text["text"].(string); len([]rune(got)) > slackMaxBlockTextLength {
				t.Fatalf("block %d text is %d runes, exceeds Slack's %d limit", i, len([]rune(got)), slackMaxBlockTextLength)
			}
		}
		if elements, ok := block["elements"].([]any); ok {
			for j, el := range elements {
				elText := el.(map[string]any)["text"].(string)
				if len([]rune(elText)) > slackMaxBlockTextLength {
					t.Fatalf("block %d element %d is %d runes, exceeds Slack's %d limit", i, j, len([]rune(elText)), slackMaxBlockTextLength)
				}
			}
		}
	}
}

// ── Manual trigger endpoint ───────────────────────────────────────────────────

func postSlackTest(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/slack/test", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestSlackTestEndpointDryRunReturnsPayloadWithoutCallingSlack(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)

	rr := postSlackTest(t, s, `{"event_type":"agent_started","dry_run":true}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		DryRun  bool `json:"dry_run"`
		Payload struct {
			Channel string           `json:"channel"`
			Text    string           `json:"text"`
			Blocks  []map[string]any `json:"blocks"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.DryRun || resp.Payload.Channel != slackTestChannel || len(resp.Payload.Blocks) == 0 {
		t.Fatalf("unexpected dry-run response: %s", rr.Body.String())
	}
	if !strings.Contains(resp.Payload.Text, "SAMPLE-123") {
		t.Fatalf("synthetic sample should be clearly marked, got %q", resp.Payload.Text)
	}
	if fake.count() != 0 {
		t.Fatalf("dry_run hit Slack %d times", fake.count())
	}
}

func TestSlackTestEndpointInvalidEventType(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)

	rr := postSlackTest(t, s, `{"event_type":"nonsense"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), taskRunEventAgentStarted) {
		t.Fatalf("400 should list valid event types, got %s", rr.Body.String())
	}
}

func TestSlackTestEndpointNotConfigured(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	rr := postSlackTest(t, s, `{"event_type":"agent_started"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not configured") {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}
}

func TestSlackTestEndpointRealSendLeavesNoState(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	rr := postSlackTest(t, s, `{"event_type":"pr_opened"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK bool   `json:"ok"`
		TS string `json:"ts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.TS == "" {
		t.Fatalf("unexpected response: %s", rr.Body.String())
	}
	if fake.count() != 1 {
		t.Fatalf("expected exactly one Slack call, got %d", fake.count())
	}
	if req := fake.request(0); req.ThreadTS != "" {
		t.Fatal("test sends must post top-level, never into a run thread")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_notification_deliveries`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("test send wrote %d delivery rows (err %v)", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_run_threads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("test send wrote %d thread rows (err %v)", n, err)
	}
}

func TestSlackTestEndpointSurfacesSlackError(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		fmt.Fprint(w, `{"ok":false,"error":"not_in_channel"}`)
	})
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)

	rr := postSlackTest(t, s, `{"event_type":"agent_started"}`)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "not_in_channel") {
		t.Fatalf("Slack error not surfaced: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "xoxb-") {
		t.Fatalf("response leaked the bot token: %s", rr.Body.String())
	}
}
