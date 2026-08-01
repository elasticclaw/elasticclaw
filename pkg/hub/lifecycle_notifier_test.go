package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	slackTestChannel  = "C0TEST1234"
	testNotifierName  = "test-notifier"
	testNotifierToken = "slack_bot_token"
)

// newSlackNotifierTestServer builds a hub whose lifecycle notifier posts to the
// given httptest Slack server. The provider is reached the same way production
// reaches it — a named hub notifier plus lifecycle.via — with api_base and
// pacing injected through notifierSettingOverrides, the only test seam.
func newSlackNotifierTestServer(t *testing.T, slackURL string, mutate func(*types.LifecycleNotificationsConfig)) (*Server, *sql.DB) {
	t.Helper()
	lc := &types.LifecycleNotificationsConfig{Via: testNotifierName}
	if mutate != nil {
		mutate(lc)
	}
	cfg := &types.HubConfig{
		Token:   "test-token",
		Secrets: map[string]string{testNotifierToken: "xoxb-test-token"},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				testNotifierName: {Type: "slack", Settings: map[string]any{
					"token_secret": testNotifierToken,
					"channel":      slackTestChannel,
				}},
			},
			Lifecycle: lc,
		},
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	s.notifierSettingOverrides = map[string]any{
		"api_base":          slackURL,
		"min_send_interval": time.Nanosecond.String(),
	}
	return s, db
}

// testLifecycleDelivery builds the delivery bundle exactly as
// lifecycleNotifierTick does, so tests that drive a single pass or a single
// send exercise the real notifier instance (and therefore the real pacing and
// destination bookkeeping) instead of a hand-rolled client.
func testLifecycleDelivery(t *testing.T, s *Server) lifecycleDelivery {
	t.Helper()
	cfg := s.notificationsConfig()
	if cfg == nil || cfg.Lifecycle == nil {
		t.Fatal("test server has no lifecycle notifications config")
	}
	nc, ok := cfg.Notifiers[cfg.Lifecycle.Via]
	if !ok {
		t.Fatalf("lifecycle.via %q does not name a configured notifier", cfg.Lifecycle.Via)
	}
	notifier, err := s.notifierFor(cfg.Lifecycle.Via, nc, s.hubSecretResolver())
	if err != nil {
		t.Fatalf("build notifier: %v", err)
	}
	return lifecycleDelivery{notifier: notifier, lc: cfg.Lifecycle, destination: notifierDestination(notifier)}
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
	s.setNotifierStateInt64(lifecycleStateWatermarkKey, rowid)
}

func slackWatermark(t *testing.T, s *Server) (int64, bool) {
	t.Helper()
	wm, found, err := s.notifierStateInt64(lifecycleStateWatermarkKey)
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

	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("first tick sent %d messages, want 1", fake.count())
	}
	if status, ok := slackDeliveryStatus(t, db, "ev-started"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("delivery status = %q, %v", status, ok)
	}
	if wm, found := slackWatermark(t, s); !found || wm <= 0 {
		t.Fatalf("watermark = %d, %v, want advanced past the event", wm, found)
	}

	// Rewind the cursor so the event is re-scanned (as after a crash between
	// send and watermark advance); the delivery record must prevent a
	// duplicate send.
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("re-scan caused a duplicate send: %d messages", fake.count())
	}

	req := fake.request(0)
	if req.Channel != slackTestChannel {
		t.Fatalf("channel = %q", req.Channel)
	}
	if !strings.Contains(req.Fallback, "Agent started") || !strings.Contains(req.Fallback, "Fix login bug") {
		t.Fatalf("fallback text = %q", req.Fallback)
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

	s.lifecycleNotifierTick()
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
	if !strings.Contains(reply.Fallback, "PR opened") || !strings.Contains(reply.Fallback, "acme/app#7") {
		t.Fatalf("pr_opened fallback = %q", reply.Fallback)
	}
}

func TestSlackNotifierDisabledEventToggle(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.LifecycleNotificationsConfig) {
		cfg.Events = &types.LifecycleEventToggles{AgentStarted: boolPtr(false)}
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

	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("sent %d messages, want only pr_opened", fake.count())
	}
	req := fake.request(0)
	if !strings.Contains(req.Fallback, "PR opened") {
		t.Fatalf("unexpected message: %q", req.Fallback)
	}
	if req.ThreadTS != "" {
		t.Fatal("pr_opened without a prior root should post top-level")
	}
	// With no agent_started root, the pr_opened message becomes the run's root.
	var threadTS string
	if err := db.QueryRow(`SELECT thread_ts FROM slack_run_threads WHERE run_id='run-1'`).Scan(&threadTS); err != nil {
		t.Fatalf("thread root row: %v", err)
	}
	// The muted event is parked as skipped so a later re-enable can never
	// replay it (see TestSlackNotifierMutedCategoryNotReplayedOnReenable).
	if status, ok := slackDeliveryStatus(t, db, "ev-started"); !ok || status != notificationDeliveryStatusSkipped {
		t.Fatalf("disabled agent_started delivery = %q, %v; want parked as skipped", status, ok)
	}
}

// The no-replay invariant for the task-run pass (referenced by
// TestSlackNotifierDisabledEventToggle): an event parked as "skipped" while
// its category was muted stays muted after the category is re-enabled and the
// notifier keeps ticking — without the skipLifecycleMutedEvents parking, the
// watermark would still sit before the muted event and the re-enable would
// flush it into the channel.
func TestSlackNotifierMutedCategoryNotReplayedOnReenable(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.LifecycleNotificationsConfig) {
		cfg.Events = &types.LifecycleEventToggles{AgentStarted: boolPtr(false)}
	})

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-muted", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	setSlackWatermark(t, s, 0)

	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("muted category sent %d messages", fake.count())
	}
	if status, ok := slackDeliveryStatus(t, db, "ev-muted"); !ok || status != notificationDeliveryStatusSkipped {
		t.Fatalf("muted event delivery = %q, %v; want parked as skipped", status, ok)
	}

	// Re-enable the category and tick again: the parked event must stay muted.
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Events.AgentStarted = boolPtr(true)
	s.mu.Unlock()
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("re-enabling the category replayed the muted event: %d messages", fake.count())
	}

	// A genuinely new event of the re-enabled category is delivered.
	insertSlackTestEvent(t, db, "ev-new", "run-1", taskRunEventAgentStarted, base+20, "", "", "")
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("new event after re-enable sent %d messages, want 1", fake.count())
	}
}

// Regression: ValidateNotificationsConfig resolves lifecycle.via after
// strings.TrimSpace, so a hub.yaml with via: "test-notifier " passes
// validation — the runtime lookups must trim the same way, or they resolve a
// zero NotifierConfig and notifications silently stop while everything
// reports green.
func TestSlackNotifierTrimsLifecycleVia(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.LifecycleNotificationsConfig) {
		cfg.Via = testNotifierName + " "
	})

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", Repo: "acme/app", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	setSlackWatermark(t, s, 0)

	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("padded via delivered %d messages, want 1 (lookup did not trim)", fake.count())
	}

	// The manual test endpoint is the other runtime consumer of via.
	rr := postSlackTest(t, s, `{"event_type":"agent_started"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("test endpoint with padded via: status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// Regression coverage for the pending-delivery stash — the only guard against
// duplicate external sends when the delivery-row INSERT fails. A claw-pass
// event is used because it has no rowid cursor: the delivery row is its ONLY
// dedupe, so a forgotten failed write meant a duplicate Slack message every
// tick until the DB accepted the insert.
func TestSlackNotifierPendingDeliveryStashPreventsDuplicateSends(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)

	// Make ONLY the delivery-row insert fail, exactly like a locked/failing
	// DB at the moment of the write; selects and every other table keep
	// working so the tick still scans, sends and bookkeeps normally.
	if _, err := db.Exec(`CREATE TRIGGER fail_delivery_insert BEFORE INSERT ON slack_notification_deliveries
		BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}

	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("first tick sent %d messages, want 1", fake.count())
	}
	if _, ok := slackDeliveryStatus(t, db, lifecycleClawStartedKey("claw-adhoc")); ok {
		t.Fatal("delivery row landed despite the failing trigger; the test is not exercising the stash")
	}

	// (a) While the write keeps failing, the stash must prevent a re-send even
	// though the SQL anti-join still selects the claw.
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("failed delivery write caused a duplicate external send: %d messages", fake.count())
	}

	// (b) Once the DB accepts writes again, the stash drains into a real row
	// and nothing is re-sent.
	if _, err := db.Exec(`DROP TRIGGER fail_delivery_insert`); err != nil {
		t.Fatalf("drop failing trigger: %v", err)
	}
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("stash drain re-sent the message: %d messages", fake.count())
	}
	if status, ok := slackDeliveryStatus(t, db, lifecycleClawStartedKey("claw-adhoc")); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("drained delivery = %q, %v; want sent", status, ok)
	}
	if len(s.lifecyclePendingDeliveries) != 0 {
		t.Fatalf("stash not drained: %d entries left", len(s.lifecyclePendingDeliveries))
	}
}

// Regression: an unreadable transient-failure streak (a corrupted value; in
// production also SQLITE_BUSY) must retry WITHOUT counting — a refactor that
// fell through to count++ would re-arm the cap on every failed read, so the
// poisoned event would never be burned and the cursor would stay wedged, the
// exact crashloop the persisted streak exists to prevent.
func TestSlackNotifierUnreadableStreakStateRetriesWithoutBurning(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		http.Error(w, "upstream hiccup", http.StatusInternalServerError)
	})
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-poison", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	setSlackWatermark(t, s, 0)
	streakKey := lifecycleTransientFailureStateKey("ev-poison")
	if _, err := db.Exec(`INSERT INTO slack_notifier_state(key, value) VALUES(?, 'not-a-number')`, streakKey); err != nil {
		t.Fatalf("corrupt streak state: %v", err)
	}

	// Even past the cap's worth of ticks, the event must be neither burned as
	// failed nor have its streak overwritten while the state is unreadable.
	for i := 0; i < lifecycleMaxTransientFailures+5; i++ {
		s.lifecycleNotifierTick()
	}
	if status, ok := slackDeliveryStatus(t, db, "ev-poison"); ok {
		t.Fatalf("unreadable streak state burned the event as %q", status)
	}
	var raw string
	if err := db.QueryRow(`SELECT value FROM slack_notifier_state WHERE key=?`, streakKey).Scan(&raw); err != nil {
		t.Fatalf("read streak state: %v", err)
	}
	if raw != "not-a-number" {
		t.Fatalf("unreadable streak state was overwritten with %q", raw)
	}
	if wm, _ := slackWatermark(t, s); wm != 0 {
		t.Fatalf("watermark advanced past the event: %d", wm)
	}

	// Once the state is readable again the event is delivered normally.
	if _, err := db.Exec(`DELETE FROM slack_notifier_state WHERE key=?`, streakKey); err != nil {
		t.Fatalf("clear streak state: %v", err)
	}
	fake.setRespond(nil)
	s.lifecycleNotifierTick()
	if status, ok := slackDeliveryStatus(t, db, "ev-poison"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("recovered event delivery = %q, %v; want sent", status, ok)
	}
}

// Regression: a transport-level Send failure (connection refused — no HTTP
// status at all, unlike the 5xx path) must classify transient: the event is
// left for the next tick, not burned, and delivers once the endpoint is back.
func TestSlackNotifierTransportFailureRetriedNextTick(t *testing.T) {
	fake := newFakeSlackServer(t)
	deadURL := fake.server.URL
	fake.server.Close() // connection refused from here on
	s, db := newSlackNotifierTestServer(t, deadURL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	setSlackWatermark(t, s, 0)

	s.lifecycleNotifierTick()
	if _, delivered := slackDeliveryStatus(t, db, "ev-started"); delivered {
		t.Fatal("transport failure must not record a delivery (neither sent nor failed)")
	}
	if wm, _ := slackWatermark(t, s); wm != 0 {
		t.Fatalf("watermark advanced past a transport-failed event: %d", wm)
	}

	// Endpoint comes back (a fresh fake; the settings override rebuilds the
	// notifier because the cache key digests the settings): delivered.
	fake2 := newFakeSlackServer(t)
	s.notifierSettingOverrides["api_base"] = fake2.server.URL
	s.lifecycleNotifierTick()
	if status, ok := slackDeliveryStatus(t, db, "ev-started"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("retry after transport recovery did not deliver: %q, %v", status, ok)
	}
	if fake2.count() != 1 {
		t.Fatalf("recovered endpoint received %d messages, want 1", fake2.count())
	}
}

// The lifecycle notifier loop must stop when told to (graceful shutdown stops
// it before closing the DB — see Server.run): after stopLifecycleNotifier
// returns, no further tick may fire, so no send can straddle the DB close.
func TestLifecycleNotifierLoopStopsOnShutdown(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.LifecycleNotificationsConfig) {
		cfg.PollInterval = "1s"
	})
	s.startLifecycleNotifier()
	s.stopLifecycleNotifier(5 * time.Second)
	select {
	case <-s.lifecycleNotifierDone:
	default:
		t.Fatal("stopLifecycleNotifier returned before the loop exited")
	}

	// An event landing after the stop is never picked up by the (stopped)
	// background loop, even well past the poll interval.
	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	setSlackWatermark(t, s, 0)
	time.Sleep(1500 * time.Millisecond)
	if fake.count() != 0 {
		t.Fatalf("stopped notifier loop still sent %d messages", fake.count())
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
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("first run replayed history: %d messages", fake.count())
	}
	if _, found := slackWatermark(t, s); !found {
		t.Fatal("first tick did not persist a watermark")
	}

	// A second tick must not pick up the pre-existing event either.
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("second tick replayed pre-existing rows: %d messages", fake.count())
	}

	// A genuinely new event is delivered.
	insertSlackTestEvent(t, db, "ev-new", "run-old", taskRunEventPROpened, nowMs+60_000,
		"https://github.com/acme/app/pull/9", "", "")
	s.lifecycleNotifierTick()
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

	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("sent %d requests, want 1", fake.count())
	}
	if status, ok := slackDeliveryStatus(t, db, "ev-started"); !ok || status != notificationDeliveryStatusFailed {
		t.Fatalf("delivery status = %q, %v; want failed", status, ok)
	}

	// Permanent failures must not be retried on subsequent ticks.
	s.lifecycleNotifierTick()
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

	s.lifecycleNotifierTick()
	if _, delivered := slackDeliveryStatus(t, db, "ev-started"); delivered {
		t.Fatal("transient failure must not record a delivery")
	}
	if wm, _ := slackWatermark(t, s); wm != 0 {
		t.Fatalf("watermark advanced past a transiently-failed event: %d", wm)
	}

	fail = false
	s.lifecycleNotifierTick()
	if status, ok := slackDeliveryStatus(t, db, "ev-started"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("retry did not deliver: status = %q, %v", status, ok)
	}
}

func TestSlackNotifierNoopWhenDisabled(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, func(cfg *types.LifecycleNotificationsConfig) {
		cfg.Enabled = boolPtr(false)
	})

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-started", "run-1", taskRunEventAgentStarted, base+10, "", "", "")

	s.lifecycleNotifierTick()
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
	s.hubCfg.Notifications.Lifecycle.Enabled = &enabled
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
	total := lifecycleBatchSize + 50
	for i := 0; i < total; i++ {
		insertSlackTestEvent(t, db, fmt.Sprintf("ev-%03d", i), "run-1", taskRunEventAgentStarted, base+int64(i), "", "", "")
	}
	setSlackWatermark(t, s, 0)

	s.lifecycleNotifierTick()
	if fake.count() != lifecycleBatchSize {
		t.Fatalf("first tick sent %d messages, want %d", fake.count(), lifecycleBatchSize)
	}
	s.lifecycleNotifierTick()
	if fake.count() != total {
		t.Fatalf("second tick left the cursor wedged: %d messages sent, want %d", fake.count(), total)
	}

	// Events created after the burst must still be delivered.
	insertSlackTestEvent(t, db, "ev-later", "run-1", taskRunEventAgentStarted, base+3600_000, "", "", "")
	s.lifecycleNotifierTick()
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
	s.lifecycleNotifierTick()

	// A pr_opened recorded now but carrying the PR's real (3h old) creation time.
	insertSlackTestEvent(t, db, "ev-backdated", "run-1", taskRunEventPROpened, nowMs-3*3600_000,
		"https://github.com/acme/app/pull/7", "", "")
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("backdated event was not delivered: %d messages", fake.count())
	}
	if status, ok := slackDeliveryStatus(t, db, "ev-backdated"); !ok || status != notificationDeliveryStatusSent {
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

	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
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
	s.lifecycleNotifierTick()
	for i := 0; i < 3; i++ {
		if status, ok := slackDeliveryStatus(t, db, fmt.Sprintf("ev-%d", i)); !ok || status != notificationDeliveryStatusSent {
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
	if _, err := db.Exec(`INSERT INTO slack_notifier_state(key, value) VALUES(?, 'not-a-number')`, lifecycleStateWatermarkKey); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}

	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("tick with unreadable state sent %d messages", fake.count())
	}
	var raw string
	if err := db.QueryRow(`SELECT value FROM slack_notifier_state WHERE key=?`, lifecycleStateWatermarkKey).Scan(&raw); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if raw != "not-a-number" {
		t.Fatalf("unreadable state was overwritten with %q — the backlog would be skipped", raw)
	}

	// Once the state is readable again, the backlog is still there.
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()
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
	s.lifecycleNotifierTick()

	// Operator mutes Slack; events accumulate while it is off.
	setSlackEnabled(t, s, false)
	for i := 0; i < 3; i++ {
		insertSlackTestEvent(t, db, fmt.Sprintf("ev-muted-%d", i), "run-1", taskRunEventAgentStarted, base+int64(i), "", "", "")
		s.lifecycleNotifierTick()
	}

	// Re-enabling must not dump the muted window's backlog.
	setSlackEnabled(t, s, true)
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("re-enable flushed %d stale messages from the disabled window", fake.count())
	}

	// New events after re-enable are delivered normally.
	insertSlackTestEvent(t, db, "ev-after", "run-1", taskRunEventAgentStarted, base+100, "", "", "")
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("event after re-enable was not delivered: %d messages", fake.count())
	}
}

// Regression: failure reasons can be up to 6000 chars (failureSummaryInputLimit)
// but a Slack section text object caps at 3000 — an oversized block fails the
// whole message with invalid_blocks and the alert is dropped.
// The clamp itself lives in the provider (see TestSlackClampsLongBody in
// pkg/hub/notify); this asserts the end-to-end path, i.e. that a real oversized
// failure reason travels through buildLifecycleMessage into a payload Slack
// will actually accept.
func TestSlackFailureMessageClampsLongReason(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)
	d := testLifecycleDelivery(t, s)

	msg := buildLifecycleMessage(lifecycleEventRow{
		EventType:   taskRunFailureBootstrapFailed,
		FailureType: taskRunFailureBootstrapFailed,
		Detail:      map[string]any{"reason": strings.Repeat("x", 6000)},
	}, lifecycleRunContext{IssueID: "ISSUE-1", IssueTitle: "Broken build"})

	renderer, ok := d.notifier.(notify.PayloadRenderer)
	if !ok {
		t.Fatal("notifier does not implement PayloadRenderer")
	}
	payload, err := renderer.RenderPayload(msg)
	if err != nil {
		t.Fatalf("RenderPayload() error = %v", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var decoded struct {
		Attachments []struct {
			Blocks []map[string]any `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(decoded.Attachments) != 1 {
		t.Fatalf("payload carries %d attachments, want 1", len(decoded.Attachments))
	}
	const slackMaxBlockTextLength = 3000
	for i, block := range decoded.Attachments[0].Blocks {
		if text, ok := block["text"].(map[string]any); ok {
			if got, _ := text["text"].(string); len([]rune(got)) > slackMaxBlockTextLength {
				t.Fatalf("block %d text is %d runes, exceeds Slack's %d limit", i, len([]rune(got)), slackMaxBlockTextLength)
			}
		}
		if elements, ok := block["elements"].([]any); ok {
			for j, el := range elements {
				elText, _ := el.(map[string]any)["text"].(string)
				if len([]rune(elText)) > slackMaxBlockTextLength {
					t.Fatalf("block %d element %d is %d runes, exceeds Slack's %d limit", i, j, len([]rune(elText)), slackMaxBlockTextLength)
				}
			}
		}
	}
}

// The bug this design fixes: nine failure types all rendered as the identical
// ":warning: Run failed" shape and were indistinguishable in the channel.
// Every supported event type must render with its own (emoji, title) identity,
// a stripe colour from the palette, a human title (no raw snake_case in the
// headline) and a non-empty notification fallback.
func TestSlackEventTypesRenderDistinctly(t *testing.T) {
	palette := map[string]bool{
		notify.SlackColorInfo:    true,
		notify.SlackColorSuccess: true,
		notify.SlackColorError:   true,
		notify.SlackColorWarning: true,
	}
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)
	renderer, ok := testLifecycleDelivery(t, s).notifier.(notify.PayloadRenderer)
	if !ok {
		t.Fatal("notifier does not implement PayloadRenderer")
	}

	seen := map[string]string{} // (emoji, title) -> event type
	seenEmoji := map[string]string{}
	for eventType := range lifecycleSupportedEventTypes() {
		runCtx, ev := sampleLifecycleContext(eventType)
		msg := buildLifecycleMessage(ev, runCtx)
		style := lifecycleEventStyleFor(ev)
		blocks, color, fallback := renderSlackParts(t, renderer, msg)

		// Identity: no two event types may share the same (emoji, title) pair,
		// and each keeps its own emoji so messages read distinctly at a glance.
		pair := style.emoji + " " + style.title
		if prev, dup := seen[pair]; dup {
			t.Errorf("%s and %s render the same (emoji, title) pair %q", prev, eventType, pair)
		}
		seen[pair] = eventType
		if prev, dup := seenEmoji[style.emoji]; dup {
			t.Errorf("%s and %s share the emoji %s", prev, eventType, style.emoji)
		}
		seenEmoji[style.emoji] = eventType

		// Human title: never a raw snake_case identifier in the headline.
		if strings.Contains(style.title, "_") || style.title == eventType {
			t.Errorf("%s title %q is not human-readable", eventType, style.title)
		}

		// Colour stripe from the small palette.
		if !palette[color] {
			t.Errorf("%s color = %q, want one of the palette colours", eventType, color)
		}

		// Headline block leads with "<emoji> *<title>*".
		if len(blocks) == 0 {
			t.Fatalf("%s rendered no blocks", eventType)
		}
		headText, _ := blocks[0]["text"].(map[string]any)["text"].(string)
		if !strings.HasPrefix(headText, style.emoji+" *"+style.title+"*") {
			t.Errorf("%s headline = %q, want it to start with %q", eventType, headText, style.emoji+" *"+style.title+"*")
		}

		// Notification fallback leads with the discriminator — emoji then
		// title — so a truncated lock-screen line still identifies the event,
		// and never carries the raw snake_case type.
		if !strings.HasPrefix(fallback, style.emoji+" "+style.title) {
			t.Errorf("%s fallback = %q, want it to start with %q", eventType, fallback, style.emoji+" "+style.title)
		}
		if strings.Contains(fallback, "("+eventType+")") {
			t.Errorf("%s fallback = %q carries the raw type in machine form", eventType, fallback)
		}
	}
}

// ── Manual trigger endpoint ───────────────────────────────────────────────────

func postSlackTest(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/test", strings.NewReader(body))
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
			Channel     string `json:"channel"`
			Text        string `json:"text"`
			Attachments []struct {
				Fallback string           `json:"fallback"`
				Color    string           `json:"color"`
				Blocks   []map[string]any `json:"blocks"`
			} `json:"attachments"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.DryRun || resp.Payload.Channel != slackTestChannel {
		t.Fatalf("unexpected dry-run response: %s", rr.Body.String())
	}
	// The dry-run payload must be the real wire shape: blocks inside a single
	// colour-striped attachment carrying the notification fallback, and NO
	// top-level text — with an attachment present it would render as a
	// visible duplicate of the headline, not as a hidden fallback.
	if len(resp.Payload.Attachments) != 1 {
		t.Fatalf("dry-run payload has %d attachments, want 1: %s", len(resp.Payload.Attachments), rr.Body.String())
	}
	if resp.Payload.Attachments[0].Color == "" || len(resp.Payload.Attachments[0].Blocks) == 0 {
		t.Fatalf("dry-run attachment missing color or blocks: %s", rr.Body.String())
	}
	if resp.Payload.Text != "" {
		t.Fatalf("dry-run payload has top-level text %q, want the notification summary in the attachment fallback only", resp.Payload.Text)
	}
	if resp.Payload.Attachments[0].Fallback == "" {
		t.Fatal("dry-run attachment has no plain-text fallback")
	}
	if !strings.Contains(resp.Payload.Attachments[0].Fallback, "SAMPLE-123") {
		t.Fatalf("synthetic sample should be clearly marked, got %q", resp.Payload.Attachments[0].Fallback)
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
		OK bool `json:"ok"`
		// message_id, not ts: the response is provider-neutral now, since the
		// handle a provider returns is not necessarily a Slack timestamp.
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.MessageID == "" {
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

// renderSlackParts renders a semantic Message through the Slack provider and
// pulls out the attachment's blocks, colour and fallback — the three things the
// design contract is expressed in.
func renderSlackParts(t *testing.T, renderer notify.PayloadRenderer, msg notify.Message) ([]map[string]any, string, string) {
	t.Helper()
	payload, err := renderer.RenderPayload(msg)
	if err != nil {
		t.Fatalf("RenderPayload() error = %v", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var decoded struct {
		Attachments []struct {
			Fallback string           `json:"fallback"`
			Color    string           `json:"color"`
			Blocks   []map[string]any `json:"blocks"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(decoded.Attachments) != 1 {
		t.Fatalf("payload carries %d attachments, want 1", len(decoded.Attachments))
	}
	a := decoded.Attachments[0]
	return a.Blocks, a.Color, a.Fallback
}

// Regression: the notifier cache must hold one instance per notifier name.
// The old single-slot cache thrashed as soon as two features alternated
// between two named notifiers — every call rebuilt the instance, and any
// instance-local provider state was reset on each rebuild.
func TestNotifierCacheHoldsOneInstancePerName(t *testing.T) {
	fake := newFakeSlackServer(t)
	cfg := &types.HubConfig{
		Token:   "test-token",
		Secrets: map[string]string{testNotifierToken: "xoxb-test-token"},
		Notifications: &types.NotificationsConfig{
			Notifiers: map[string]types.NotifierConfig{
				"notifier-a": {Type: "slack", Settings: map[string]any{
					"token_secret": testNotifierToken, "channel": "C0000AAAA",
				}},
				"notifier-b": {Type: "slack", Settings: map[string]any{
					"token_secret": testNotifierToken, "channel": "C0000BBBB",
				}},
			},
			Lifecycle: &types.LifecycleNotificationsConfig{Via: "notifier-a"},
		},
	}
	s, _ := NewTestServerWithConfig(t, cfg, "", "", "")
	s.notifierSettingOverrides = map[string]any{
		"api_base":          fake.server.URL,
		"min_send_interval": time.Nanosecond.String(),
	}

	secrets := s.hubSecretResolver()
	get := func(name string) notify.Notifier {
		t.Helper()
		nc := s.notificationsConfig().Notifiers[name]
		n, err := s.notifierFor(name, nc, secrets)
		if err != nil {
			t.Fatalf("notifierFor(%s): %v", name, err)
		}
		return n
	}

	a1, b1 := get("notifier-a"), get("notifier-b")
	a2, b2 := get("notifier-a"), get("notifier-b")
	if a1 != a2 {
		t.Fatal("alternating between two notifiers rebuilt notifier-a instead of reusing the cached instance")
	}
	if b1 != b2 {
		t.Fatal("alternating between two notifiers rebuilt notifier-b instead of reusing the cached instance")
	}
	if a1 == b1 {
		t.Fatal("distinct notifier names returned the same instance")
	}
}

// Regression: the dry-run preview and the real send must be built from the
// SAME settings. Before the fix the dry-run notifier was constructed from the
// raw config settings while the real send used the merged (override-applied)
// settings, so the preview could report a different destination than the one
// actually posted to.
func TestSlackTestEndpointDryRunMatchesRealSend(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierTestServer(t, fake.server.URL, nil)
	// Divergence probe: an override changes the effective channel away from
	// the configured one. Both paths must see it.
	s.notifierSettingOverrides["channel"] = "C9OVERRIDE"

	dry := postSlackTest(t, s, `{"event_type":"agent_started","dry_run":true}`)
	if dry.Code != http.StatusOK {
		t.Fatalf("dry run status = %d, body = %s", dry.Code, dry.Body.String())
	}
	var dryResp struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(dry.Body.Bytes(), &dryResp); err != nil {
		t.Fatalf("decode dry-run response: %v", err)
	}
	var dryPayload struct {
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(dryResp.Payload, &dryPayload); err != nil {
		t.Fatalf("decode dry-run payload: %v", err)
	}
	if dryPayload.Channel != "C9OVERRIDE" {
		t.Fatalf("dry-run payload channel = %q, want the effective channel C9OVERRIDE", dryPayload.Channel)
	}

	real := postSlackTest(t, s, `{"event_type":"agent_started"}`)
	if real.Code != http.StatusOK {
		t.Fatalf("real send status = %d, body = %s", real.Code, real.Body.String())
	}
	var realResp struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(real.Body.Bytes(), &realResp); err != nil {
		t.Fatalf("decode real-send response: %v", err)
	}
	if string(dryResp.Payload) != string(realResp.Payload) {
		t.Fatalf("dry-run payload differs from the real send payload:\n dry:  %s\n real: %s", dryResp.Payload, realResp.Payload)
	}
	if fake.count() != 1 {
		t.Fatalf("expected exactly one Slack call, got %d", fake.count())
	}
	if got := fake.request(0).Channel; got != "C9OVERRIDE" {
		t.Fatalf("real send posted to %q, want C9OVERRIDE", got)
	}
}

// Regression: a message that keeps failing with an unclassified (therefore
// transient) error must not wedge the cursor forever — after the cap it is
// recorded failed and everything behind it flows again.
func TestSlackNotifierTransientFailureCapUnwedgesCursor(t *testing.T) {
	fake := newFakeSlackServer(t)
	broken := true
	fake.setRespond(func(n int, w http.ResponseWriter) {
		if broken {
			http.Error(w, "upstream hiccup", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"ts":"4000.%06d"}`, n)
	})
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	insertSlackTestEvent(t, db, "ev-poison", "run-1", taskRunEventAgentStarted, base+10, "", "", "")
	insertSlackTestEvent(t, db, "ev-behind", "run-1", taskRunEventAgentStarted, base+20, "", "", "")
	setSlackWatermark(t, s, 0)

	for i := 0; i < lifecycleMaxTransientFailures-1; i++ {
		s.lifecycleNotifierTick()
	}
	if _, delivered := slackDeliveryStatus(t, db, "ev-poison"); delivered {
		t.Fatal("event was recorded before the transient-failure cap was reached")
	}

	// The cap-th consecutive failure records the event failed and unblocks
	// the queue behind it.
	s.lifecycleNotifierTick()
	if status, ok := slackDeliveryStatus(t, db, "ev-poison"); !ok || status != notificationDeliveryStatusFailed {
		t.Fatalf("poisoned delivery status = %q, %v; want failed after %d consecutive transient failures", status, ok, lifecycleMaxTransientFailures)
	}

	broken = false
	s.lifecycleNotifierTick()
	if status, ok := slackDeliveryStatus(t, db, "ev-behind"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("event behind the poisoned one was not delivered: status = %q, %v", status, ok)
	}
}

// stubNotifier is a minimal provider without DestinationReporter: it returns
// handles (so it LOOKS threadable to the old handle-based inference) but
// cannot name its destination.
type stubNotifier struct{ sends int }

func (s *stubNotifier) Send(ctx context.Context, msg notify.Message) (string, error) {
	s.sends++
	if msg.Thread != "" {
		return "", fmt.Errorf("stub provider does not thread, got thread %q", msg.Thread)
	}
	return fmt.Sprintf("handle-%d", s.sends), nil
}

// Regression: an unknown destination ("" — the provider does not implement
// DestinationReporter) must disable thread bookkeeping entirely, not act as a
// match-everything wildcard. Before the fix the root row was recorded with
// channel=” and every later event matched it, replying into threads that may
// not exist wherever the notifier now points.
func TestLifecycleUnknownDestinationDisablesThreading(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	stub := &stubNotifier{}
	if got := notifierDestination(stub); got != "" {
		t.Fatalf("notifierDestination(stub) = %q, want empty", got)
	}
	d := lifecycleDelivery{notifier: stub, lc: s.notificationsConfig().Lifecycle, destination: notifierDestination(stub)}

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	ev := lifecycleEventRow{ID: "ev-1", RunID: "run-1", EventType: taskRunEventAgentStarted}
	if err := s.postLifecycleEvent(d, ev, s.lifecycleRunContextFor("run-1"), "run-1", "ev-1", "test-tenant-id"); err != nil {
		t.Fatalf("first post: %v", err)
	}
	// The second event must post top-level (the stub errors on any Thread) and
	// no root row may be recorded for the unknown destination.
	ev2 := lifecycleEventRow{ID: "ev-2", RunID: "run-1", EventType: taskRunEventPROpened}
	if err := s.postLifecycleEvent(d, ev2, s.lifecycleRunContextFor("run-1"), "run-1", "ev-2", "test-tenant-id"); err != nil {
		t.Fatalf("second post: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_run_threads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unknown destination recorded %d thread roots (err %v), want 0", n, err)
	}
}
