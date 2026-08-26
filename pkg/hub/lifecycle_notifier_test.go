package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// newSlackNotifierRoutesTestServer configures named Slack destinations against
// one capture server; distinct channels make route assertions unambiguous.
func newSlackNotifierRoutesTestServer(t *testing.T, slackURL string, routes []types.LifecycleRoute) (*Server, *sql.DB) {
	t.Helper()
	cfg := &types.HubConfig{
		Token: "test-token", Secrets: map[string]string{testNotifierToken: "xoxb-test-token"},
		Notifications: &types.NotificationsConfig{Notifiers: map[string]types.NotifierConfig{}, Lifecycle: &types.LifecycleNotificationsConfig{Routes: routes}},
	}
	for i, route := range routes {
		cfg.Notifications.Notifiers[route.Via] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
			"token_secret": testNotifierToken, "channel": fmt.Sprintf("C0ROUTE%04d", i+1),
		}}
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	s.notifierSettingOverrides = map[string]any{"api_base": slackURL, "min_send_interval": time.Nanosecond.String()}
	return s, db
}

// testLifecycleDelivery builds the delivery bundle exactly as
// lifecycleNotifierTick does, so tests that drive a single pass or a single
// send exercise the real notifier instance (and therefore the real pacing)
// instead of a hand-rolled client.
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
	return lifecycleDelivery{notifier: notifier, lc: cfg.Lifecycle, paused: map[string]bool{}}
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

func routeDeliveryStatus(t *testing.T, db *sql.DB, eventID, notifier string) (string, bool) {
	t.Helper()
	var status string
	err := db.QueryRow(`SELECT status FROM slack_notification_deliveries_v2 WHERE event_id=? AND notifier=?`, eventID, notifier).Scan(&status)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("query route delivery %s via %s: %v", eventID, notifier, err)
	}
	return status, true
}

func insertLifecycleRouteEvent(t *testing.T, db *sql.DB, id, eventType string) {
	t.Helper()
	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{RunID: "route-run", AttemptID: "attempt-route-run", ClawID: "route-claw", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Factory: "bugfix", StartedAt: base - 1000})
	insertSlackTestEvent(t, db, id, "route-run", eventType, base+10, "", "", "")
}

func TestLifecycleRoutesFanoutAndFiltering(t *testing.T) {
	fake := newFakeSlackServer(t)
	routes := []types.LifecycleRoute{{Via: "all"}, {Via: "started", Events: []string{taskRunEventAgentStarted}}}
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, routes)
	insertLifecycleRouteEvent(t, db, "route-started", taskRunEventAgentStarted)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("fan-out sent %d messages, want 2", fake.count())
	}
	if fake.request(0).Channel == fake.request(1).Channel {
		t.Fatalf("fan-out used one channel %q twice", fake.request(0).Channel)
	}
	if _, ok := routeDeliveryStatus(t, db, "route-started", "all"); !ok {
		t.Fatal("empty allow-list route did not receive event")
	}
	if _, ok := routeDeliveryStatus(t, db, "route-started", "started"); !ok {
		t.Fatal("matching allow-list route did not receive event")
	}

	insertSlackTestEvent(t, db, "route-pr", "route-run", taskRunEventPROpened, 1760000000020, "", "", "")
	s.lifecycleNotifierTick()
	if fake.count() != 3 {
		t.Fatalf("filtered event sent %d messages, want 3", fake.count())
	}
	if _, ok := routeDeliveryStatus(t, db, "route-pr", "all"); !ok {
		t.Fatal("empty allow-list route did not receive all event types")
	}
	if _, ok := routeDeliveryStatus(t, db, "route-pr", "started"); ok {
		t.Fatal("non-matching allow-list route received pr_opened")
	}
}

func TestLifecycleRoutesLegacyDeliveryAndUpgradeFence(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	insertLifecycleRouteEvent(t, db, "legacy-event", taskRunEventAgentStarted)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("legacy via sent %d messages, want 1", fake.count())
	}

	// A pre-v2 delivery is a global fence, including for routes added later.
	if _, err := db.Exec(`INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status) VALUES(?,?,?,?,?)`, "upgraded-event", "route-run", 1, "", notificationDeliveryStatusSent); err != nil {
		t.Fatalf("seed legacy delivery: %v", err)
	}
	insertSlackTestEvent(t, db, "upgraded-event", "route-run", taskRunEventPROpened, 1760000000030, "", "", "")
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Via = ""
	s.hubCfg.Notifications.Lifecycle.Routes = []types.LifecycleRoute{{Via: testNotifierName}, {Via: "new-route"}}
	s.hubCfg.Notifications.Notifiers["new-route"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{"token_secret": testNotifierToken, "channel": "C0NEWROUTE"}}
	s.mu.Unlock()
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("legacy delivery was resent after upgrade: %d sends", fake.count())
	}
}

func TestLifecycleRoutesErrorDoesNotBlockOtherRouteAndRetriesPerRoute(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		if n == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"ok":true,"ts":"1.000001"}`)
	})
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "broken"}, {Via: "healthy"}})
	insertLifecycleRouteEvent(t, db, "route-error", taskRunEventAgentStarted)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("erroring route blocked fan-out: got %d sends, want 2", fake.count())
	}
	if status, ok := routeDeliveryStatus(t, db, "route-error", "healthy"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("healthy route = %q, %v; want sent", status, ok)
	}
	s.handleLifecycleSendError(fmt.Errorf("temporary"), "event", "retry-event", "run", "broken", false)
	s.handleLifecycleSendError(fmt.Errorf("temporary"), "event", "retry-event", "run", "broken", false)
	s.handleLifecycleSendError(fmt.Errorf("temporary"), "event", "retry-event", "run", "healthy", false)
	var count int64
	if err := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM slack_notifier_state WHERE key=?`, lifecycleTransientFailureStateKey("retry-event", "broken")).Scan(&count); err != nil || count != 2 {
		t.Fatalf("broken retry count = %d, err %v; want 2", count, err)
	}
	if err := db.QueryRow(`SELECT CAST(value AS INTEGER) FROM slack_notifier_state WHERE key=?`, lifecycleTransientFailureStateKey("retry-event", "healthy")).Scan(&count); err != nil || count != 1 {
		t.Fatalf("healthy retry count = %d, err %v; want independent 1", count, err)
	}
}

// respondByChannel scripts the fake Slack server per destination channel,
// which is how route-level failures are simulated (each route posts to its own
// channel; see newSlackNotifierRoutesTestServer).
func respondByChannel(fake *fakeSlackServer, fn func(channel string, w http.ResponseWriter)) {
	fake.setRespond(func(n int, w http.ResponseWriter) { fn(fake.request(n-1).Channel, w) })
}

func slackOK(w http.ResponseWriter) { fmt.Fprint(w, `{"ok":true,"ts":"1000.000001"}`) }

// Regression: the claw pass has NO cursor — the delivery-row anti-join alone
// decides what is new. With more than one route no row was written for the
// handled claw, so it was re-selected on every tick forever and eventually ate
// the whole batch LIMIT, silently starving newer claws of notifications.
func TestLifecycleRoutesFanoutFencesClawEvents(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "primary"}, {Via: "secondary"}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)

	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("fan-out sent %d messages, want 2", fake.count())
	}
	for _, notifier := range []string{"primary", "secondary"} {
		claws, err := s.selectLifecycleClawStateCandidates(lifecycleClawKindStarted, notifier)
		if err != nil || len(claws) != 0 {
			t.Fatalf("handled claw still selected as a candidate for %s: %d (err %v)", notifier, len(claws), err)
		}
	}
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("handled claw was re-sent: %d messages", fake.count())
	}
}

// Regression: an event type a route does not accept must never be scanned for
// that route, or those claws sit in its oldest-first batch forever.
func TestLifecycleRoutesFenceClawEventNoRouteAccepts(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL,
		[]types.LifecycleRoute{{Via: "prs-only", Events: []string{taskRunEventPROpened}}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)

	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("agent_started reached a pr_opened-only route: %d messages", fake.count())
	}
	route := lifecycleRouteDelivery{notifier: "prs-only", events: map[string]bool{taskRunEventPROpened: true}}
	if lifecycleRouteAccepts(route, taskRunEventAgentStarted) {
		t.Fatal("a pr_opened-only route must not scan agent_started claws; they would consume its batch forever")
	}
}

// Regression: in the legacy single-`via` shape, a claw event whose route row
// exists without the legacy row — the state a crash between the two writes
// leaves, or a multi-route era where only this route ever delivered — was
// re-selected on every tick forever: the dedupe read saw the route row and sent
// nothing, so no legacy row was ever written and the event kept one slot of the
// oldest-first batch for good.
func TestLifecycleClawEventWithOnlyRouteDeliveryIsNotReselected(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	if _, err := db.Exec(`INSERT INTO slack_notification_deliveries_v2(event_id, notifier, run_id, delivered_at, message_ts, status) VALUES(?,?,?,?,?,?)`,
		lifecycleClawStartedKey("claw-adhoc"), testNotifierName, lifecycleClawRunKey("claw-adhoc"), 1, "", notificationDeliveryStatusSent); err != nil {
		t.Fatalf("seed route delivery: %v", err)
	}

	for i := 0; i < 3; i++ {
		s.lifecycleNotifierTick()
	}
	if fake.count() != 0 {
		t.Fatalf("an already-delivered claw event was re-sent %d times", fake.count())
	}
	claws, err := s.selectLifecycleClawStateCandidates(lifecycleClawKindStarted, testNotifierName)
	if err != nil || len(claws) != 0 {
		t.Fatalf("delivered claw is still a candidate: %d (err %v)", len(claws), err)
	}
}

// Regression: claws delivered to a healthy route kept no delivery row while
// another route was broken, so they stayed in the shared, oldest-first
// `LIMIT 200` candidate set forever and eventually locked newer claws out of
// every route — the cross-route muting the per-route parking exists to prevent.
// ErrorConfig (is_archived) is never burned by the transient cap, so the state
// persisted for as long as the channel stayed archived.
func TestLifecycleClawBrokenRouteDoesNotStarveHealthyRoute(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "archived"}, {Via: "healthy"}})
	respondByChannel(fake, func(channel string, w http.ResponseWriter) {
		if channel == "C0ROUTE0001" { // the "archived" route
			fmt.Fprint(w, `{"ok":false,"error":"is_archived"}`)
			return
		}
		slackOK(w)
	})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	for i := 0; i < 3; i++ {
		insertSlackTestClaw(t, db, fmt.Sprintf("claw-%d", i), "connected", 1, "", oldEnough)
	}

	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()

	healthy, err := s.selectLifecycleClawStateCandidates(lifecycleClawKindStarted, "healthy")
	if err != nil || len(healthy) != 0 {
		t.Fatalf("claws already delivered to the healthy route still occupy its batch: %d (err %v)", len(healthy), err)
	}
	archived, err := s.selectLifecycleClawStateCandidates(lifecycleClawKindStarted, "archived")
	if err != nil || len(archived) != 3 {
		t.Fatalf("the broken route lost its backlog: %d candidates (err %v)", len(archived), err)
	}
}

// Regression: a configured route that cannot be built at all (its token secret
// no longer exists) marked every claw event incomplete — even event types its
// own allow-list does not cover — so the routes that DID build never got a
// fence row and replayed their candidate set on every tick until it filled the
// batch. Pre-routing this was a loud total outage; here it was silent.
func TestLifecycleClawUnbuildableRouteDoesNotStarveBuiltRoutes(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{
		{Via: "primary", Events: []string{taskRunEventAgentStarted}},
		{Via: "secondary", Events: []string{taskRunEventPROpened}},
	})
	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers["secondary"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": "secret-that-no-longer-exists", "channel": "C0SECOND",
	}}
	s.mu.Unlock()
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)

	for i := 0; i < 3; i++ {
		s.lifecycleNotifierTick()
	}
	if fake.count() != 1 {
		t.Fatalf("agent_started sent %d times; an unbuildable route must not make a built one replay", fake.count())
	}
	claws, err := s.selectLifecycleClawStateCandidates(lifecycleClawKindStarted, "primary")
	if err != nil || len(claws) != 0 {
		t.Fatalf("an unbuildable route kept a delivered claw in the built route's batch: %d (err %v)", len(claws), err)
	}
}

// A route added to an existing config must not replay every currently
// connected claw into its new channel: the claw pass has no cursor, so the
// route needs its own baseline the way a new task-run route inherits a cursor.
func TestLifecycleClawRouteAddedLaterDoesNotReplayCurrentClaws(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "primary"}, {Via: "secondary"}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("fan-out sent %d messages, want 2", fake.count())
	}

	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers["late"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": testNotifierToken, "channel": "C0LATEROUTE",
	}}
	s.hubCfg.Notifications.Lifecycle.Routes = append(s.hubCfg.Notifications.Lifecycle.Routes, types.LifecycleRoute{Via: "late"})
	s.mu.Unlock()

	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("a route added later replayed the current claw list: %d messages, want 2", fake.count())
	}
}

// Regression: Slack classifies is_archived/channel_not_found/not_in_channel as
// ErrorConfig, and the config branch used to pause the shared cursor for every
// route. One archived secondary channel therefore muted every healthy channel
// indefinitely — no transient cap applies to ErrorConfig.
func TestLifecycleRoutesConfigErrorParksOnlyTheBrokenRoute(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "archived"}, {Via: "healthy"}})
	respondByChannel(fake, func(channel string, w http.ResponseWriter) {
		if channel == "C0ROUTE0001" { // the "archived" route
			fmt.Fprint(w, `{"ok":false,"error":"is_archived"}`)
			return
		}
		slackOK(w)
	})
	insertLifecycleRouteEvent(t, db, "cfg-first", taskRunEventAgentStarted)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()

	insertSlackTestEvent(t, db, "cfg-second", "route-run", taskRunEventAgentStarted, 1760000000030, "", "", "")
	s.lifecycleNotifierTick()

	if status, ok := routeDeliveryStatus(t, db, "cfg-second", "healthy"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("a broken route blocked the healthy one: cfg-second via healthy = %q, %v", status, ok)
	}
	if _, ok := routeDeliveryStatus(t, db, "cfg-first", "archived"); ok {
		t.Fatal("a config-failed route must not record a delivery — its events stay pending until the config is fixed")
	}
}

// Regression: postLifecycleEvent propagated only the FIRST route error, so a
// permanent failure on an earlier route made the caller treat the event as
// handled and advance past it — the later route's transient failure was
// discarded and its copy of the event was lost forever.
func TestLifecycleRoutesLaterRouteFailureIsNotSwallowed(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "permanent"}, {Via: "flaky"}})
	var flakyDown atomic.Bool
	flakyDown.Store(true)
	respondByChannel(fake, func(channel string, w http.ResponseWriter) {
		if channel == "C0ROUTE0001" { // the "permanent" route
			fmt.Fprint(w, `{"ok":false,"error":"msg_too_long"}`)
			return
		}
		if flakyDown.Load() {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		slackOK(w)
	})
	insertLifecycleRouteEvent(t, db, "mixed-event", taskRunEventAgentStarted)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()

	if status, ok := routeDeliveryStatus(t, db, "mixed-event", "permanent"); !ok || status != notificationDeliveryStatusFailed {
		t.Fatalf("permanent route delivery = %q, %v; want failed", status, ok)
	}
	if _, ok := routeDeliveryStatus(t, db, "mixed-event", "flaky"); ok {
		t.Fatal("a transient failure must not record a delivery row")
	}

	flakyDown.Store(false)
	s.lifecycleNotifierTick()
	if status, ok := routeDeliveryStatus(t, db, "mixed-event", "flaky"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("event lost for the route that failed after a permanent one: %q, %v", status, ok)
	}
}

// Regression: a route whose notifier cannot be built (unresolvable secret
// mid-rotation) used to be skipped while the healthy routes advanced the
// SHARED watermark, so everything produced during the outage was permanently
// lost for that route. Pre-routing behaviour paused instead and never lost an
// event; per-route cursors restore that.
func TestLifecycleRoutesUnbuildableRouteKeepsItsBacklog(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "healthy"}, {Via: "rotating"}})
	setNotifierSecret := func(secret string) {
		s.mu.Lock()
		s.hubCfg.Notifications.Notifiers["rotating"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
			"token_secret": secret, "channel": "C0ROTATING",
		}}
		s.mu.Unlock()
	}
	setNotifierSecret("secret-being-rotated") // not in hubCfg.Secrets

	insertLifecycleRouteEvent(t, db, "outage-event", taskRunEventAgentStarted)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("healthy route sent %d messages during the outage, want 1", fake.count())
	}
	if _, ok := routeDeliveryStatus(t, db, "outage-event", "rotating"); ok {
		t.Fatal("an unbuildable route must not be recorded as delivered")
	}

	setNotifierSecret(testNotifierToken)
	s.lifecycleNotifierTick()
	if status, ok := routeDeliveryStatus(t, db, "outage-event", "rotating"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("event produced during the outage was lost for the recovered route: %q, %v", status, ok)
	}
	if fake.count() != 2 {
		t.Fatalf("sent %d messages, want 2 (one per route)", fake.count())
	}
}

// Per-route cursors must not turn into a history replay: a route added to an
// already-routed config inherits the shared cursor, so that cursor has to keep
// tracking the slowest route instead of freezing where routing began.
func TestLifecycleRouteAddedLaterDoesNotReplayHistory(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "primary"}, {Via: "secondary"}})
	insertLifecycleRouteEvent(t, db, "history-1", taskRunEventAgentStarted)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()
	insertSlackTestEvent(t, db, "history-2", "route-run", taskRunEventAgentStarted, 1760000000030, "", "", "")
	s.lifecycleNotifierTick()
	if fake.count() != 4 {
		t.Fatalf("two events over two routes sent %d messages, want 4", fake.count())
	}

	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers["late"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": testNotifierToken, "channel": "C0LATEROUTE",
	}}
	s.hubCfg.Notifications.Lifecycle.Routes = append(s.hubCfg.Notifications.Lifecycle.Routes, types.LifecycleRoute{Via: "late"})
	s.mu.Unlock()

	s.lifecycleNotifierTick()
	if fake.count() != 4 {
		t.Fatalf("a route added later replayed the backlog: %d messages, want 4", fake.count())
	}
}

// Regression: the pending stash only covered the legacy table, so a v2 write
// that failed after a successful Send was logged and forgotten. The next tick
// re-selected the event (the claw pass has no cursor) and re-sent it
// externally — every tick, until the database recovered.
func TestLifecycleStashCoversRouteDeliveryWrites(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)

	// A locked database rejects writes to BOTH delivery tables.
	for _, table := range []string{"slack_notification_deliveries", "slack_notification_deliveries_v2"} {
		if _, err := db.Exec(`CREATE TRIGGER fail_` + table + ` BEFORE INSERT ON ` + table +
			` BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
			t.Fatalf("create failing trigger on %s: %v", table, err)
		}
	}

	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("first tick sent %d messages, want 1", fake.count())
	}
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("failed delivery writes caused a duplicate external send: %d messages", fake.count())
	}

	for _, table := range []string{"slack_notification_deliveries", "slack_notification_deliveries_v2"} {
		if _, err := db.Exec(`DROP TRIGGER fail_` + table); err != nil {
			t.Fatalf("drop trigger on %s: %v", table, err)
		}
	}
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("stash drain re-sent the message: %d messages", fake.count())
	}
	if status, ok := routeDeliveryStatus(t, db, lifecycleClawStartedKey("claw-adhoc"), testNotifierName); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("drained route delivery = %q, %v; want sent", status, ok)
	}
	if len(s.lifecyclePendingDeliveries) != 0 {
		t.Fatalf("stash not drained: %d entries left", len(s.lifecyclePendingDeliveries))
	}
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

func TestSlackNotifierPostsEventsTopLevel(t *testing.T) {
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
	if root.ThreadTS != "" || reply.ThreadTS != "" {
		t.Fatalf("lifecycle messages must be top-level: root=%q reply=%q", root.ThreadTS, reply.ThreadTS)
	}
	var threadRows int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_run_threads`).Scan(&threadRows); err != nil || threadRows != 0 {
		t.Fatalf("lifecycle notifier wrote %d thread rows (err %v), want 0", threadRows, err)
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

// Regression: the endpoint resolved the notifier exclusively from
// lifecycle.via. A routes-only config (everything the Notifier UI saves) has
// an empty via, so the UI's own "Send test" button 400'd on every config it
// produced — while already sending the notifier name in the request body.
func TestSlackTestEndpointHonoursRequestedRoute(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, _ := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "primary"}, {Via: "secondary"}})

	rr := postSlackTest(t, s, `{"event_type":"agent_started"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("routes-only default send = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := fake.request(0).Channel; got != "C0ROUTE0001" {
		t.Fatalf("default test send went to %q, want the first route's channel", got)
	}

	rr = postSlackTest(t, s, `{"event_type":"agent_started","via":"secondary"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("explicit via send = %d, body %s", rr.Code, rr.Body.String())
	}
	if got := fake.request(1).Channel; got != "C0ROUTE0002" {
		t.Fatalf("explicit via send went to %q, want the secondary route's channel", got)
	}

	rr = postSlackTest(t, s, `{"event_type":"agent_started","via":"nope"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown via = %d, want 400; body %s", rr.Code, rr.Body.String())
	}
	if fake.count() != 2 {
		t.Fatalf("unknown via still sent a message: %d requests", fake.count())
	}
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

// stubNotifier is a minimal provider that records top-level sends.
type stubNotifier struct{ sends int }

func (s *stubNotifier) Send(ctx context.Context, msg notify.Message) (string, error) {
	s.sends++
	if msg.Thread != "" {
		return "", fmt.Errorf("stub provider does not thread, got thread %q", msg.Thread)
	}
	return fmt.Sprintf("handle-%d", s.sends), nil
}

func TestLifecyclePostDoesNotWriteLegacyThreadRows(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)

	stub := &stubNotifier{}
	d := lifecycleDelivery{notifier: stub, lc: s.notificationsConfig().Lifecycle}

	base := int64(1760000000000)
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-1", AttemptID: "attempt-run-1", ClawID: "claw-1", TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory,
		Factory: "bugfix", StartedAt: base - 1000,
	})
	route := d.effectiveRoutes()[0]
	ev := lifecycleEventRow{ID: "ev-1", RunID: "run-1", EventType: taskRunEventAgentStarted}
	msg := buildLifecycleMessage(ev, s.lifecycleRunContextFor("run-1"))
	if err := s.postLifecycleEventRoute(d, route, msg, "run-1", "ev-1"); err != nil {
		t.Fatalf("first post: %v", err)
	}
	// The second event must stay top-level and no legacy root row may be written.
	ev2 := lifecycleEventRow{ID: "ev-2", RunID: "run-1", EventType: taskRunEventPROpened}
	msg2 := buildLifecycleMessage(ev2, s.lifecycleRunContextFor("run-1"))
	if err := s.postLifecycleEventRoute(d, route, msg2, "run-1", "ev-2"); err != nil {
		t.Fatalf("second post: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM slack_run_threads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("lifecycle post recorded %d thread roots (err %v), want 0", n, err)
	}
}

// Regression: a config that is multi-route from its very first runtime enable
// never got a shared watermark floor — advanceLifecycleWatermarkFloor bails
// while any configured route is unbuildable, and nothing else wrote the key.
// The broken route therefore first-ran at the CURRENT stream head once its
// secret was fixed, losing every event produced during the outage: exactly the
// loss the floor exists to prevent. The older backlog test hid this by
// pre-seeding the shared key.
func TestLifecycleRoutesFirstEnableCreatesWatermarkFloor(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "healthy"}, {Via: "rotating"}})
	setNotifierSecret := func(secret string) {
		s.mu.Lock()
		s.hubCfg.Notifications.Notifiers["rotating"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
			"token_secret": secret, "channel": "C0ROTATING",
		}}
		s.mu.Unlock()
	}
	setNotifierSecret("secret-being-rotated") // not in hubCfg.Secrets

	// No cursor of any kind persisted yet: what a runtime enable through the
	// settings screen looks like on a hub that booted with notifications off.
	s.lifecycleNotifierTick()
	if _, found := slackWatermark(t, s); !found {
		t.Fatal("no shared watermark floor was created, so the unbuildable route will first-run at the stream head")
	}

	insertLifecycleRouteEvent(t, db, "outage-event", taskRunEventAgentStarted)
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("healthy route sent %d messages during the outage, want 1", fake.count())
	}

	setNotifierSecret(testNotifierToken)
	s.lifecycleNotifierTick()
	if status, ok := routeDeliveryStatus(t, db, "outage-event", "rotating"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("event produced during the outage was lost for the recovered route: %q, %v", status, ok)
	}
}

// Regression: migrating a legacy single-`via` config to routes presented the
// incumbent as a newly added route, so its per-route baseline seeded the
// current claw state as "skipped" — burying the claw alerts that were still
// pending because the channel had been unreachable. Nothing ever delivered
// them, on any route.
func TestLifecycleClawLegacyViaMigrationKeepsIncumbentBacklog(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	// The legacy shape needs no per-route key, so stamp only the shared
	// baseline: "enabled, with no history".
	s.setNotifierStateInt64(lifecycleStateClawBaselineKey, 1)
	setSlackWatermark(t, s, 0)

	// The channel is down while the claw comes up: its agent_started is owed
	// but nothing is recorded for it.
	var down atomic.Bool
	down.Store(true)
	fake.setRespond(func(_ int, w http.ResponseWriter) {
		if down.Load() {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		slackOK(w)
	})
	insertSlackTestClaw(t, db, "claw-pending", "connected", 1, "", oldEnough)
	s.lifecycleNotifierTick()
	if _, ok := slackDeliveryStatus(t, db, lifecycleClawStartedKey("claw-pending")); ok {
		t.Fatal("a transient failure must not record a delivery row")
	}

	// The operator adds a second channel through the settings screen: the
	// PATCH writes routes and clears the legacy via.
	down.Store(false)
	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers["secondary"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": testNotifierToken, "channel": "C0SECONDARY",
	}}
	s.hubCfg.Notifications.Lifecycle.Via = ""
	s.hubCfg.Notifications.Lifecycle.Routes = []types.LifecycleRoute{{Via: testNotifierName}, {Via: "secondary"}}
	s.mu.Unlock()

	s.lifecycleNotifierTick() // baselines the genuinely new route
	s.lifecycleNotifierTick()
	if status, ok := routeDeliveryStatus(t, db, lifecycleClawStartedKey("claw-pending"), testNotifierName); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("the incumbent route's pending claw alert was buried by the migration: %q, %v", status, ok)
	}
}

// Regression: a claw-pass kind a route's allow-list rejected was neither
// scanned nor parked, and the claw pass has no cursor — so adding that event
// type to the route later replayed every claw still connected and every PR
// still open since the route's baseline.
func TestLifecycleClawRouteAllowListAdditionDoesNotReplay(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{
		{Via: "primary", Events: []string{taskRunEventAgentStarted}},
		{Via: "secondary"},
	})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	insertSlackTestClawPR(t, db, "pr-1", "claw-adhoc", "acme/api", 7, "https://github.com/acme/api/pull/7")

	s.lifecycleNotifierTick()
	delivered := fake.count()
	if delivered == 0 {
		t.Fatal("first tick delivered nothing")
	}
	if _, ok := routeDeliveryStatus(t, db, lifecycleClawPRKey("claw-adhoc", "https://github.com/acme/api/pull/7"), "primary"); !ok {
		t.Fatal("a kind the route's allow-list rejects must still be parked for that route")
	}

	// A month later the operator checks "PR opened" for primary.
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Routes[0].Events = []string{taskRunEventAgentStarted, taskRunEventPROpened}
	s.mu.Unlock()

	s.lifecycleNotifierTick()
	if fake.count() != delivered {
		t.Fatalf("widening a route's allow-list replayed history: %d messages, want %d", fake.count(), delivered)
	}
}

// Regression: per-route state survived the route's removal from an enabled
// config (nothing parks it while alerts stay on), so re-adding the channel
// resumed from its stale cursor and flushed the whole removal window — and its
// stale claw baseline kept the claw pass from re-seeding, replaying every
// still-connected ad-hoc claw on top.
func TestLifecycleRemovedRouteStateIsDroppedSoReAddDoesNotReplay(t *testing.T) {
	fake := newFakeSlackServer(t)
	routes := []types.LifecycleRoute{{Via: "primary"}, {Via: "secondary"}, {Via: "third"}}
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, routes)
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	insertLifecycleRouteEvent(t, db, "before-removal", taskRunEventAgentStarted)
	s.lifecycleNotifierTick()
	if fake.count() != 3 {
		t.Fatalf("three routes sent %d messages, want 3", fake.count())
	}

	// The operator un-routes "third". Two routes remain, so nothing is written
	// to the legacy table that could fence the removal window later.
	setRoutes := func(routes ...types.LifecycleRoute) {
		s.mu.Lock()
		s.hubCfg.Notifications.Lifecycle.Routes = routes
		s.mu.Unlock()
	}
	setRoutes(types.LifecycleRoute{Via: "primary"}, types.LifecycleRoute{Via: "secondary"})
	s.lifecycleNotifierTick()
	for _, key := range []string{lifecycleRouteWatermarkKey("third"), lifecycleClawRouteBaselineKey("third")} {
		if _, found, err := s.notifierStateInt64(key); err != nil || found {
			t.Fatalf("state %q survived the route's removal (found=%v, err=%v)", key, found, err)
		}
	}

	insertSlackTestEvent(t, db, "during-removal", "route-run", taskRunEventAgentStarted, 1760000000030, "", "", "")
	insertSlackTestClaw(t, db, "claw-during-removal", "connected", 1, "", oldEnough)
	s.lifecycleNotifierTick()
	sent := fake.count()

	// Re-adding it must behave exactly like a newly added route.
	setRoutes(types.LifecycleRoute{Via: "primary"}, types.LifecycleRoute{Via: "secondary"}, types.LifecycleRoute{Via: "third"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != sent {
		t.Fatalf("a re-added route replayed the removal window: %d messages, want %d", fake.count(), sent)
	}
}

// Regression: a new route inherited the shared floor, which
// advanceLifecycleWatermarkFloor freezes while any configured route cannot be
// built. Adding a channel while a sibling was broken therefore flooded it with
// the broken route's entire backlog.
func TestLifecycleRouteAddedWhileSiblingBrokenStartsAtHead(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "healthy"}, {Via: "broken"}})
	setSlackWatermark(t, s, 0)
	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers["broken"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": "secret-that-does-not-exist", "channel": "C0BROKEN",
	}}
	s.mu.Unlock()

	// The shared floor cannot advance past the broken route, so it stays at 0
	// while the healthy route works through the backlog.
	insertLifecycleRouteEvent(t, db, "backlog-0", taskRunEventAgentStarted)
	for i, id := range []string{"backlog-1", "backlog-2", "backlog-3"} {
		insertSlackTestEvent(t, db, id, "route-run", taskRunEventAgentStarted, 1760000000030+int64(i), "", "", "")
	}
	s.lifecycleNotifierTick()
	sent := fake.count()
	if sent == 0 {
		t.Fatal("healthy route delivered nothing")
	}
	if floor, _ := slackWatermark(t, s); floor != 0 {
		t.Fatalf("shared floor advanced past the broken route: %d, want 0", floor)
	}

	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers["late"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": testNotifierToken, "channel": "C0LATEROUTE",
	}}
	s.hubCfg.Notifications.Lifecycle.Routes = append(s.hubCfg.Notifications.Lifecycle.Routes, types.LifecycleRoute{Via: "late"})
	s.mu.Unlock()

	s.lifecycleNotifierTick()
	if fake.count() != sent {
		t.Fatalf("a route added behind a broken sibling replayed its backlog: %d messages, want %d", fake.count(), sent)
	}
}

// Regression: initLifecycleNotifierBaseline created the shared claw baseline
// itself, which makes the in-tick first-enable branch unreachable — so no route
// was ever stamped at boot. A route that could not be built at boot was then
// treated as newly added when it recovered, and the claw events it was owed in
// the meantime were seeded as "skipped".
func TestLifecycleBootBaselineStampsEveryConfiguredRoute(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "healthy"}, {Via: "rotating"}})
	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers["rotating"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": "secret-being-rotated", "channel": "C0ROTATING",
	}}
	s.mu.Unlock()

	s.initLifecycleNotifierBaseline()
	for _, via := range []string{"healthy", "rotating"} {
		if _, found, err := s.notifierStateInt64(lifecycleClawRouteBaselineKey(via)); err != nil || !found {
			t.Fatalf("boot baseline did not stamp route %q (found=%v, err=%v)", via, found, err)
		}
	}

	// An ad-hoc claw connects while the rotating route is unavailable.
	insertSlackTestClaw(t, db, "claw-during-outage", "connected", 1, "", oldEnough)
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("healthy route sent %d messages during the outage, want 1", fake.count())
	}

	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers["rotating"] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": testNotifierToken, "channel": "C0ROTATING",
	}}
	s.mu.Unlock()
	s.lifecycleNotifierTick()
	if status, ok := routeDeliveryStatus(t, db, lifecycleClawStartedKey("claw-during-outage"), "rotating"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("claw event owed to the recovered route was buried: %q, %v", status, ok)
	}
}

// setLifecycleRoutes rewrites the configured routes the way a settings PATCH
// does, so a test can replace, collapse or extend the route set mid-run.
func setLifecycleRoutes(s *Server, routes ...types.LifecycleRoute) {
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Routes = routes
	s.mu.Unlock()
}

// addSlackTestNotifier registers a Slack destination; an empty tokenSecret name
// that resolves to nothing is how a test makes a route unbuildable.
func addSlackTestNotifier(s *Server, name, tokenSecret, channel string) {
	s.mu.Lock()
	s.hubCfg.Notifications.Notifiers[name] = types.NotifierConfig{Type: "slack", Settings: map[string]any{
		"token_secret": tokenSecret, "channel": channel,
	}}
	s.mu.Unlock()
}

// Regression: pruneLifecycleRouteState runs before ensureLifecycleRouteWatermarks,
// so replacing EVERY route in one save deleted the only per-route cursors and
// made the newcomers look like a legacy migration — they inherited the shared
// floor, frozen behind the paused sibling, and replayed the whole backlog.
func TestLifecycleReplacingEveryRouteStartsNewcomersAtHead(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "alerts-a"}, {Via: "alerts-b"}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	addSlackTestNotifier(s, "alerts-b", "secret-that-does-not-exist", "C0BROKEN")

	// alerts-b never builds, so the shared floor stays pinned at 0 while
	// alerts-a works through the backlog.
	insertLifecycleRouteEvent(t, db, "backlog-0", taskRunEventAgentStarted)
	for i, id := range []string{"backlog-1", "backlog-2", "backlog-3"} {
		insertSlackTestEvent(t, db, id, "route-run", taskRunEventAgentStarted, 1760000000030+int64(i), "", "", "")
	}
	s.lifecycleNotifierTick()
	sent := fake.count()
	if sent == 0 {
		t.Fatal("healthy route delivered nothing")
	}
	if floor, _ := slackWatermark(t, s); floor != 0 {
		t.Fatalf("shared floor advanced past the unbuildable route: %d, want 0", floor)
	}

	// The operator gives up and swaps in two brand-new channels in one save.
	addSlackTestNotifier(s, "alerts-c", testNotifierToken, "C0NEWC")
	addSlackTestNotifier(s, "alerts-d", testNotifierToken, "C0NEWD")
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "alerts-c"}, types.LifecycleRoute{Via: "alerts-d"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != sent {
		t.Fatalf("replacing every route flooded the new channels: %d messages, want %d", fake.count(), sent)
	}
}

// Regression: collapsing a multi-route config back to one route made
// lifecycleWatermarkKeyFor return the shared key, silently re-pointing the
// survivor from its own cursor to the floor its slowest sibling froze.
func TestLifecycleCollapseToOneRouteKeepsItsOwnCursor(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "alerts-a"}, {Via: "alerts-b"}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	addSlackTestNotifier(s, "alerts-b", "secret-that-does-not-exist", "C0BROKEN")

	insertLifecycleRouteEvent(t, db, "backlog-0", taskRunEventAgentStarted)
	for i, id := range []string{"backlog-1", "backlog-2"} {
		insertSlackTestEvent(t, db, id, "route-run", taskRunEventAgentStarted, 1760000000030+int64(i), "", "", "")
	}
	s.lifecycleNotifierTick()
	if fake.count() == 0 {
		t.Fatal("healthy route delivered nothing")
	}

	// A fresh channel is routed while the sibling is still broken, so it
	// correctly starts at the stream head rather than at the frozen floor.
	addSlackTestNotifier(s, "alerts-c", testNotifierToken, "C0NEWC")
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "alerts-a"}, types.LifecycleRoute{Via: "alerts-b"}, types.LifecycleRoute{Via: "alerts-c"})
	s.lifecycleNotifierTick()
	sent := fake.count()

	// The decommissioned channels are dropped; alerts-c is now the only route,
	// and it must keep reading its own cursor.
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "alerts-c"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != sent {
		t.Fatalf("the surviving route replayed from the shared floor: %d messages, want %d", fake.count(), sent)
	}
}

// Regression: collapsing to a single route with a NEW notifier name took the
// singleRoute branch, which stamps the claw baseline without seeding on the
// assumption that the legacy delivery table already fences the route. The
// multi-route era wrote v2 rows for the OLD names only, so the new channel got
// a one-time flood of every still-connected claw.
func TestLifecycleCollapseToNewSingleRouteSeedsClawBaseline(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "alerts-a"}, {Via: "alerts-b"}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)

	insertSlackTestClaw(t, db, "claw-multi-route-era", "connected", 1, "", oldEnough)
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("multi-route era sent %d messages, want one per route", fake.count())
	}

	addSlackTestNotifier(s, "alerts-c", testNotifierToken, "C0NEWC")
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "alerts-c"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != 2 {
		t.Fatalf("the collapsed-to route replayed the claw backlog: %d messages, want 2", fake.count())
	}
	if status, ok := routeDeliveryStatus(t, db, lifecycleClawStartedKey("claw-multi-route-era"), "alerts-c"); !ok || status != notificationDeliveryStatusSkipped {
		t.Fatalf("claw baseline for the collapsed-to route = %q (%v), want a skipped fence", status, ok)
	}
}

// Regression: a route added while its notifier could not be built had its claw
// baseline seeded at the first SUCCESSFUL build, burying the claw events it
// accrued during the outage — even though ensureLifecycleRouteWatermarks had
// already materialised its task-run cursor at add time and those events are
// delivered on recovery.
func TestLifecycleClawBaselineFencesAtConfigTimeNotAtRecovery(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "eng"}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()

	// A second route is added with a typo'd secret name: configured, unbuildable.
	addSlackTestNotifier(s, "oncall", "secret-that-does-not-exist", "C0ONCALL")
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "eng"}, types.LifecycleRoute{Via: "oncall"})
	s.lifecycleNotifierTick()

	// The claw connects AFTER oncall was configured, so it is owed to oncall.
	insertSlackTestClaw(t, db, "claw-during-outage", "connected", 1, "", oldEnough)
	s.lifecycleNotifierTick()
	if fake.count() != 1 {
		t.Fatalf("eng sent %d messages during the outage, want 1", fake.count())
	}

	addSlackTestNotifier(s, "oncall", testNotifierToken, "C0ONCALL")
	s.lifecycleNotifierTick()
	if status, ok := routeDeliveryStatus(t, db, lifecycleClawStartedKey("claw-during-outage"), "oncall"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("claw event owed to the recovered route was buried: %q, %v", status, ok)
	}
}

// Regression: replacing a multi-route config with exactly ONE brand-new route
// left that route without a cursor (ensureLifecycleRouteWatermarks returns at
// len(routes) < 2), so lifecycleWatermarkKeyFor's single-route fallback handed
// it the shared floor — pinned to the stalled route of the multi-route era —
// and it re-sent the whole backlog. The two-newcomers case was already guarded.
func TestLifecycleCollapseToOneNewRouteStartsAtHead(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "alerts-a"}, {Via: "alerts-b"}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	addSlackTestNotifier(s, "alerts-b", "secret-that-does-not-exist", "C0BROKEN")

	insertLifecycleRouteEvent(t, db, "backlog-0", taskRunEventAgentStarted)
	for i, id := range []string{"backlog-1", "backlog-2", "backlog-3"} {
		insertSlackTestEvent(t, db, id, "route-run", taskRunEventAgentStarted, 1760000000030+int64(i), "", "", "")
	}
	s.lifecycleNotifierTick()
	sent := fake.count()
	if sent == 0 {
		t.Fatal("healthy route delivered nothing")
	}
	if floor, _ := slackWatermark(t, s); floor != 0 {
		t.Fatalf("shared floor advanced past the unbuildable route: %d, want 0", floor)
	}

	// One brand-new channel replaces BOTH in a single save.
	addSlackTestNotifier(s, "alerts-c", testNotifierToken, "C0NEWC")
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "alerts-c"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != sent {
		t.Fatalf("collapsing to one new route replayed the multi-route backlog: %d messages, want %d", fake.count(), sent)
	}
}

// Regression: on a hub that never had more than one route, swapping a
// RESTRICTED route's notifier for a new name stamped the newcomer's claw
// baseline without seeding — the "genuine legacy incumbent" shortcut — even
// though the allow-list parking of the previous era wrote v2 rows keyed by the
// OLD name only. Every still-connected ad-hoc claw was replayed into the new
// channel.
func TestLifecycleRestrictedSingleRouteSwapSeedsClawBaseline(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{
		{Via: "slack", Events: []string{taskRunEventPROpened}},
	})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)

	// The claw connects while "slack" is the only route. Its agent_started is
	// rejected by the route's allow-list, so it is parked as a v2 row keyed by
	// "slack" — nothing lands in the legacy delivery table for it.
	insertSlackTestClaw(t, db, "claw-adhoc", "connected", 1, "", oldEnough)
	s.lifecycleNotifierTick()
	sent := fake.count()
	if _, ok := routeDeliveryStatus(t, db, lifecycleClawStartedKey("claw-adhoc"), "slack"); !ok {
		t.Fatal("a kind the route's allow-list rejects must be parked for that route")
	}

	// The operator swaps the channel: still exactly one route, new name.
	addSlackTestNotifier(s, "ops", testNotifierToken, "C0OPS")
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "ops"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != sent {
		t.Fatalf("swapping a restricted single route replayed the claw backlog: %d messages, want %d", fake.count(), sent)
	}
	if status, ok := routeDeliveryStatus(t, db, lifecycleClawStartedKey("claw-adhoc"), "ops"); !ok || status != notificationDeliveryStatusSkipped {
		t.Fatalf("claw baseline for the swapped-in route = %q (%v), want a skipped fence", status, ok)
	}
}

// Regression: at the legacy `via` → routes migration EVERY route inherited the
// incumbent's shared cursor, so the channel the operator was adding — the whole
// reason the migration runs — replayed the incumbent's stalled backlog. Only
// the incumbent may inherit it; the newcomer starts at the stream head.
func TestLifecycleLegacyViaMigrationStartsTheNewRouteAtHead(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick() // records the incumbent; nothing pending yet

	// The incumbent's channel goes down, so its cursor stays frozen while the
	// event stream grows.
	var down atomic.Bool
	down.Store(true)
	fake.setRespond(func(_ int, w http.ResponseWriter) {
		if down.Load() {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		slackOK(w)
	})
	insertLifecycleRouteEvent(t, db, "stalled-0", taskRunEventAgentStarted)
	for i, id := range []string{"stalled-1", "stalled-2"} {
		insertSlackTestEvent(t, db, id, "route-run", taskRunEventAgentStarted, 1760000000030+int64(i), "", "", "")
	}
	s.lifecycleNotifierTick()
	if wm, _ := slackWatermark(t, s); wm != 0 {
		t.Fatalf("the incumbent's cursor advanced past the failed sends: %d, want 0", wm)
	}

	// The operator adds a second channel through the settings screen: the PATCH
	// writes routes and clears the legacy via.
	down.Store(false)
	addSlackTestNotifier(s, "oncall", testNotifierToken, "C0ONCALL")
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Via = ""
	s.mu.Unlock()
	setLifecycleRoutes(s, types.LifecycleRoute{Via: testNotifierName}, types.LifecycleRoute{Via: "oncall"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()

	for _, id := range []string{"stalled-0", "stalled-1", "stalled-2"} {
		if status, ok := routeDeliveryStatus(t, db, id, "oncall"); ok && status == notificationDeliveryStatusSent {
			t.Fatalf("the newly added channel replayed the incumbent's stalled backlog (%s)", id)
		}
		if status, ok := routeDeliveryStatus(t, db, id, testNotifierName); !ok || status != notificationDeliveryStatusSent {
			t.Fatalf("the incumbent lost its pending backlog at the migration: %s = %q (%v)", id, status, ok)
		}
	}
}

// Regression: replacing the legacy single-`via` incumbent with an all-new route
// set in one save deleted the incumbent's claw baseline stamp — the ONLY
// per-route state a hub that never went multi-route has — so
// ensureLifecycleRouteWatermarks read "nothing routed yet" and seeded every
// brand-new channel with the incumbent's stalled shared cursor, replaying its
// whole undelivered backlog into each of them.
func TestLifecycleReplacingTheLegacyIncumbentStartsNewcomersAtHead(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierTestServer(t, fake.server.URL, nil)
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick() // records the incumbent; nothing pending yet

	// The incumbent's channel is archived, so its cursor stays frozen while the
	// event stream grows.
	var down atomic.Bool
	down.Store(true)
	fake.setRespond(func(_ int, w http.ResponseWriter) {
		if down.Load() {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		slackOK(w)
	})
	insertLifecycleRouteEvent(t, db, "stalled-0", taskRunEventAgentStarted)
	for i, id := range []string{"stalled-1", "stalled-2"} {
		insertSlackTestEvent(t, db, id, "route-run", taskRunEventAgentStarted, 1760000000030+int64(i), "", "", "")
	}
	s.lifecycleNotifierTick()
	if wm, _ := slackWatermark(t, s); wm != 0 {
		t.Fatalf("the incumbent's cursor advanced past the failed sends: %d, want 0", wm)
	}

	// The operator replaces the channel outright: one save drops the incumbent
	// and routes two brand-new channels.
	down.Store(false)
	addSlackTestNotifier(s, "alerts-b", testNotifierToken, "C0ALERTSB")
	addSlackTestNotifier(s, "alerts-c", testNotifierToken, "C0ALERTSC")
	s.mu.Lock()
	s.hubCfg.Notifications.Lifecycle.Via = ""
	s.mu.Unlock()
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "alerts-b"}, types.LifecycleRoute{Via: "alerts-c"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()

	for _, via := range []string{"alerts-b", "alerts-c"} {
		for _, id := range []string{"stalled-0", "stalled-1", "stalled-2"} {
			if status, ok := routeDeliveryStatus(t, db, id, via); ok && status == notificationDeliveryStatusSent {
				t.Fatalf("replacing the incumbent replayed its stalled backlog into %q (%s)", via, id)
			}
		}
	}
}

// Regression: the first boot of the upgraded binary on a DB left by a legacy
// single-`via` era stamped a claw baseline for EVERY configured route. The
// stamp is what lifecycleSingleViaIncumbent reads, so a channel added in the
// same maintenance window was reported as the legacy incumbent, inherited its
// frozen shared cursor and replayed the stalled backlog into a channel that
// should have started clean.
func TestLifecycleLegacyBootWithRoutesStartsEveryRouteAtHead(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL,
		[]types.LifecycleRoute{{Via: "incumbent"}, {Via: "oncall"}})
	// Exactly the state the OLD binary leaves: a shared cursor frozen where the
	// single `via` stalled plus the shared claw baseline, nothing per-route.
	s.setNotifierStateInt64(lifecycleStateClawBaselineKey, 1)
	setSlackWatermark(t, s, 0)
	insertLifecycleRouteEvent(t, db, "stalled-0", taskRunEventAgentStarted)
	for i, id := range []string{"stalled-1", "stalled-2"} {
		insertSlackTestEvent(t, db, id, "route-run", taskRunEventAgentStarted, 1760000000030+int64(i), "", "", "")
	}
	insertSlackTestClaw(t, db, "claw-during-outage", "connected", 1, "", oldEnough)

	// The operator upgrades the binary and saves the routes in one window.
	s.initLifecycleNotifierBaseline()
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()

	if fake.count() != 0 {
		t.Fatalf("the upgrade replayed the legacy backlog: %d messages, want 0", fake.count())
	}
	for _, via := range []string{"incumbent", "oncall"} {
		for _, id := range []string{"stalled-0", "stalled-1", "stalled-2", lifecycleClawStartedKey("claw-during-outage")} {
			if status, ok := routeDeliveryStatus(t, db, id, via); ok && status == notificationDeliveryStatusSent {
				t.Fatalf("%s was sent to %q on the first boot after the upgrade", id, via)
			}
		}
	}
}

// Regression: the boot-time migration branch stamped the single configured
// route as the legacy incumbent whenever no claw_baseline_done key was left,
// ignoring the routes_live latch pruneLifecycleRouteState deliberately keeps
// when it drops the stamps of the routes a save replaced. A hub restarted in
// that window presented a brand-new channel as the incumbent, handed it the
// frozen shared cursor and replayed the whole multi-route-era backlog into it.
func TestLifecycleBootBaselineHonoursTheRoutesLiveLatch(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "alerts-c"}})
	// The hub has been running (shared claw flag) and the per-route scheme has
	// been live, but the save that collapsed onto "alerts-c" left no per-route
	// claw stamp behind.
	s.setNotifierStateInt64(lifecycleStateClawBaselineKey, 1)
	s.setNotifierStateInt64(lifecycleStateRoutedKey, 1)
	setSlackWatermark(t, s, 0)
	insertLifecycleRouteEvent(t, db, "backlog-0", taskRunEventAgentStarted)
	insertSlackTestClaw(t, db, "claw-multi-route-era", "connected", 1, "", oldEnough)

	s.initLifecycleNotifierBaseline()
	if s.lifecycleSingleViaIncumbent("alerts-c") {
		t.Fatal("a brand-new route was stamped as the legacy single-via incumbent at boot")
	}

	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("the collapsed-to route replayed the backlog after a restart: %d messages, want 0", fake.count())
	}
}

// Regression: collapsing onto ONE brand-new route whose notifier could not be
// built dropped everything produced during the outage — both ensure passes
// bailed at len(routes) < 2, so the route's cursor and claw fence were
// materialised at the first SUCCESSFUL build instead of at add time, past the
// window every tick logs as "held until it can be built".
func TestLifecycleCollapseToOneUnbuildableRouteFencesAtConfigTime(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{{Via: "alerts-a"}, {Via: "alerts-b"}})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	s.lifecycleNotifierTick()

	// One brand-new channel replaces both, with a typo'd secret name: it is
	// configured, unbuildable, and the hub's ONLY route.
	addSlackTestNotifier(s, "alerts-c", "secret-that-does-not-exist", "C0NEWC")
	setLifecycleRoutes(s, types.LifecycleRoute{Via: "alerts-c"})
	s.lifecycleNotifierTick()

	// Produced during the outage, so owed to alerts-c.
	insertLifecycleRouteEvent(t, db, "during-outage", taskRunEventAgentStarted)
	insertSlackTestClaw(t, db, "claw-during-outage", "connected", 1, "", oldEnough)
	s.lifecycleNotifierTick()
	if fake.count() != 0 {
		t.Fatalf("an unbuildable route sent %d messages", fake.count())
	}

	addSlackTestNotifier(s, "alerts-c", testNotifierToken, "C0NEWC")
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()
	if status, ok := routeDeliveryStatus(t, db, "during-outage", "alerts-c"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("the task-run event produced during the outage was never delivered: %q (%v)", status, ok)
	}
	if status, ok := routeDeliveryStatus(t, db, lifecycleClawStartedKey("claw-during-outage"), "alerts-c"); !ok || status != notificationDeliveryStatusSent {
		t.Fatalf("the claw that connected during the outage was buried: %q (%v)", status, ok)
	}
}

// Regression: a lone route with a restricted allow-list latches routes_live
// (parking a rejected kind writes per-route rows) while still reading the
// SHARED cursor. ensureLifecycleRouteWatermarks gated the "inherit the shared
// position" branch on that latch, so adding a second channel later stamped the
// incumbent's brand-new per-route cursor at the stream head and permanently
// dropped everything it had not yet delivered.
func TestLifecycleRestrictedIncumbentKeepsItsBacklogWhenARouteIsAdded(t *testing.T) {
	fake := newFakeSlackServer(t)
	s, db := newSlackNotifierRoutesTestServer(t, fake.server.URL, []types.LifecycleRoute{
		{Via: "slack", Events: []string{taskRunEventPROpened}},
	})
	setLifecycleClawBaseline(t, s)
	setSlackWatermark(t, s, 0)
	// Parking the kinds the allow-list rejects latches routes_live on the very
	// first tick, without the route ever leaving the shared key.
	s.lifecycleNotifierTick()
	if live, err := s.lifecycleRoutingSchemeLive(); err != nil || !live {
		t.Fatalf("a restricted route must latch the routing scheme: %v, %v", live, err)
	}

	// The channel goes down, so the shared cursor freezes while pr_opened
	// events pile up.
	var down atomic.Bool
	down.Store(true)
	fake.setRespond(func(_ int, w http.ResponseWriter) {
		if down.Load() {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		slackOK(w)
	})
	insertLifecycleRouteEvent(t, db, "pending-0", taskRunEventPROpened)
	for i, id := range []string{"pending-1", "pending-2"} {
		insertSlackTestEvent(t, db, id, "route-run", taskRunEventPROpened, 1760000000030+int64(i), "", "", "")
	}
	s.lifecycleNotifierTick()
	if wm, _ := slackWatermark(t, s); wm != 0 {
		t.Fatalf("the incumbent's cursor advanced past the failed sends: %d, want 0", wm)
	}

	// The operator adds a second channel through the settings screen.
	down.Store(false)
	addSlackTestNotifier(s, "ops", testNotifierToken, "C0OPS")
	setLifecycleRoutes(s,
		types.LifecycleRoute{Via: "slack", Events: []string{taskRunEventPROpened}},
		types.LifecycleRoute{Via: "ops"})
	s.lifecycleNotifierTick()
	s.lifecycleNotifierTick()

	for _, id := range []string{"pending-0", "pending-1", "pending-2"} {
		if status, ok := routeDeliveryStatus(t, db, id, "slack"); !ok || status != notificationDeliveryStatusSent {
			t.Fatalf("the incumbent lost its pending backlog when a route was added: %s = %q (%v)", id, status, ok)
		}
		if status, ok := routeDeliveryStatus(t, db, id, "ops"); ok && status == notificationDeliveryStatusSent {
			t.Fatalf("the newly added channel replayed the incumbent's backlog (%s)", id)
		}
	}
}
