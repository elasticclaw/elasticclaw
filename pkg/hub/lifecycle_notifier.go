package hub

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// The lifecycle notifier turns agent lifecycle events (agent started, PR
// opened, failures) into outbound notifications, delivered through the
// hub-level notifier named by notifications.lifecycle.via. All provider
// specifics (Block Kit, colours, pacing) live in pkg/hub/notify; this file
// owns event scanning, dedupe and the semantic
// message content.
//
// The dedupe/thread/state tables keep their historical slack_* names: they
// predate the provider abstraction and renaming SQLite tables buys nothing.
const (
	lifecycleDefaultPollInterval = 5 * time.Second
	lifecycleBatchSize           = 200

	// lifecycleStateWatermarkKey stores the task_run_events rowid the
	// notifier has processed up to. The cursor is insertion-ordered on
	// purpose: observed_at carries authoritative provider timestamps (a
	// pr_opened picked up by the 24h catch-up poller can be hours in the
	// past), so a timestamp watermark would silently skip backdated rows.
	lifecycleStateWatermarkKey = "watermark_rowid"

	// lifecycleSendWarningKey keys the log-once warning for
	// configuration-level send failures (bad token, missing channel).
	lifecycleSendWarningKey = "notify-send"

	notificationDeliveryStatusSent   = "sent"
	notificationDeliveryStatusFailed = "failed"
)

// lifecycleFailureEventTypes are the failure-shaped task run events we notify on.
var lifecycleFailureEventTypes = map[string]bool{
	taskRunEventAgentStopped:       true,
	"creation_failed":              true,
	taskRunFailureProvisionFailed:  true,
	taskRunFailureBootstrapFailed:  true,
	taskRunFailureTimeout:          true,
	"unknown_failure":              true,
	taskRunFailurePermissionOrAuth: true,
	taskRunFailureProviderLost:     true,
	taskRunEventDoneWithoutPR:      true,
}

// lifecycleSupportedEventTypes is everything the notifier can render.
func lifecycleSupportedEventTypes() map[string]bool {
	supported := make(map[string]bool, len(types.LifecycleEventTypes))
	for _, t := range types.LifecycleEventTypes {
		supported[t] = true
	}
	return supported
}

// notificationsConfig returns a copy of the current notifications config.
// Reading it fresh each tick means enabling/disabling notifications does not
// require a hub restart.
func (s *Server) notificationsConfig() *types.NotificationsConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hubCfg == nil || s.hubCfg.Notifications == nil {
		return nil
	}
	src := s.hubCfg.Notifications
	cfg := &types.NotificationsConfig{}
	if src.Notifiers != nil {
		cfg.Notifiers = make(map[string]types.NotifierConfig, len(src.Notifiers))
		for name, n := range src.Notifiers {
			cfg.Notifiers[name] = n
		}
	}
	if src.Lifecycle != nil {
		lc := *src.Lifecycle
		cfg.Lifecycle = &lc
	}
	return cfg
}

// hubSecretResolver resolves notifier secrets against the hub secrets map.
// Capturing the map (not the Server) is safe: hubCfg is never nil (NewServer
// coerces nil configs) and the secrets API is copy-on-write — every mutation
// builds a fresh map and swaps the hubCfg pointer under s.mu, so the captured
// map is an immutable snapshot.
func (s *Server) hubSecretResolver() notify.SecretResolver {
	s.mu.RLock()
	secrets := s.hubCfg.Secrets
	s.mu.RUnlock()
	return func(name string) (string, bool) {
		v, ok := secrets[name]
		return v, ok
	}
}

// notifierFor builds (or reuses) the Notifier for the named notifier config.
// Instances are cached per notifier name across ticks so every feature using
// the same named notifier shares one instance; each entry's key covers the
// config and a digest of the resolved secrets, so editing the config or
// rotating a token rebuilds that notifier on the next use. Reuse is an
// efficiency, never a correctness requirement: providers must keep pacing
// state outside the instance (the Slack limiter is keyed process-wide by
// channel), so a rebuild can never reset it.
func (s *Server) notifierFor(name string, nc types.NotifierConfig, secrets notify.SecretResolver) (notify.Notifier, error) {
	settings := s.notifierSettings(nc)
	key := notifierCacheKey(name, types.NotifierConfig{Type: nc.Type, Settings: settings}, secrets)
	s.notifierCacheMu.Lock()
	defer s.notifierCacheMu.Unlock()
	if c, ok := s.notifierCache[name]; ok && c.key == key {
		return c.notifier, nil
	}
	n, err := notify.New(nc.Type, settings, secrets)
	if err != nil {
		return nil, err
	}
	if s.notifierCache == nil {
		s.notifierCache = make(map[string]cachedNotifier)
	}
	s.notifierCache[name] = cachedNotifier{key: key, notifier: n}
	return n, nil
}

// notifierSettings is the notifier's configured settings with any test
// overrides applied. It always returns a copy so callers can never mutate the
// hub config's map.
func (s *Server) notifierSettings(nc types.NotifierConfig) map[string]any {
	settings := make(map[string]any, len(nc.Settings)+len(s.notifierSettingOverrides))
	for k, v := range nc.Settings {
		settings[k] = v
	}
	for k, v := range s.notifierSettingOverrides {
		settings[k] = v
	}
	return settings
}

func notifierCacheKey(name string, nc types.NotifierConfig, secrets notify.SecretResolver) string {
	settings, _ := json.Marshal(nc.Settings) // map keys marshal sorted
	key := name + "\x00" + nc.Type + "\x00" + string(settings)
	// Digest (never store) the resolved secret values so a rotation
	// invalidates the cached notifier.
	for _, settingKey := range notify.SecretSettings(nc.Type) {
		if secretName, ok := nc.Settings[settingKey].(string); ok {
			value, _ := secrets(secretName)
			sum := sha256.Sum256([]byte(value))
			key += "\x00" + hex.EncodeToString(sum[:])
		}
	}
	return key
}

// enabledLifecycleEventTypes maps the config toggles onto concrete event
// types. All categories default to enabled when the toggles block is absent.
func enabledLifecycleEventTypes(lc *types.LifecycleNotificationsConfig) map[string]bool {
	agentStarted, prOpened, failures, agentIdle := lifecycleClawKindsEnabled(lc)
	enabled := map[string]bool{}
	if agentStarted {
		enabled[taskRunEventAgentStarted] = true
	}
	if prOpened {
		enabled[taskRunEventPROpened] = true
	}
	if agentIdle {
		enabled[taskRunEventAgentIdle] = true
	}
	if failures {
		for t := range lifecycleFailureEventTypes {
			enabled[t] = true
		}
	}
	return enabled
}

func (s *Server) lifecyclePollInterval() time.Duration {
	cfg := s.notificationsConfig()
	if cfg != nil && cfg.Lifecycle != nil && cfg.Lifecycle.PollInterval != "" {
		if d, err := time.ParseDuration(cfg.Lifecycle.PollInterval); err == nil && d >= time.Second {
			return d
		}
	}
	return lifecycleDefaultPollInterval
}

// initLifecycleNotifierBaseline establishes the "everything before now is
// history" boundary synchronously. NewServer calls it BEFORE starting any
// event producer (PR watcher, cron scheduler, integration poller): the poll
// loop's first tick would otherwise set the baseline one poll interval after
// the producers began, silently dropping whatever they produced in that
// window. Only the first-ever enabled boot writes anything — afterwards the
// persisted cursors are authoritative — and the first-tick branches in the
// passes remain as a backstop for runtime enables.
func (s *Server) initLifecycleNotifierBaseline() {
	cfg := s.notificationsConfig()
	if cfg == nil || !cfg.Lifecycle.IsEnabled() {
		return
	}
	if _, found, err := s.notifierStateInt64(lifecycleStateWatermarkKey); err == nil && !found {
		if maxRow, err := s.lifecycleMaxEventRowID(); err == nil {
			s.setNotifierStateInt64(lifecycleStateWatermarkKey, maxRow)
		} else {
			log.Printf("[notify] init watermark baseline: %v", err)
		}
	}
	if _, found, err := s.notifierStateInt64(lifecycleStateClawBaselineKey); err == nil && !found {
		if err := s.seedLifecycleClawBaseline(); err == nil {
			s.setNotifierStateInt64(lifecycleStateClawBaselineKey, 1)
		} else {
			log.Printf("[notify] init claw baseline: %v", err)
		}
	}
	// Stamp the agent_idle baseline at boot on the first enabled run so idle
	// stretches that predate the feature (or this deploy) are parked, not
	// announced. Runtime enables are covered by the lazy stamp in
	// agentIdleBaseline.
	if _, found, err := s.notifierStateInt64(agentIdleBaselineKey); err == nil && !found {
		s.setNotifierStateInt64(agentIdleBaselineKey, epochMillis(now()))
	}
}

// startLifecycleNotifier launches the background loop that turns lifecycle
// events into notifications. The goroutine idles cheaply while notifications
// are disabled and picks the config up again on the next tick. It stops when
// stopLifecycleNotifier is called (graceful shutdown, before the DB closes).
func (s *Server) startLifecycleNotifier() {
	stop := make(chan struct{})
	done := make(chan struct{})
	s.lifecycleNotifierStop, s.lifecycleNotifierDone = stop, done
	s.safeGo("lifecycle notifier", func() {
		defer close(done)
		for {
			timer := time.NewTimer(s.lifecyclePollInterval())
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			// Run the tick inline (not via safeGo) so ticks never overlap:
			// the read-then-insert delivery dedupe is not safe under
			// concurrent ticks. A panic is contained to this iteration.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[notify] panic in lifecycle notifier tick: %v\n%s", r, debug.Stack())
					}
				}()
				s.lifecycleNotifierTick()
			}()
		}
	})
}

// stopLifecycleNotifier stops the poll loop and waits (bounded) for an
// in-flight tick to finish. Server.run calls it after the HTTP drain and
// BEFORE closing the database: a tick in flight at shutdown can complete an
// external Send and then fail the delivery-row insert against a closed DB —
// the in-memory retry stash dies with the process and the event would re-send
// after restart. Waiting lets the tick land its bookkeeping first; the timeout
// keeps a pathologically slow tick from wedging shutdown, accepting the
// (pre-existing) duplicate-send window in that case.
func (s *Server) stopLifecycleNotifier(timeout time.Duration) {
	if s.lifecycleNotifierStop == nil {
		return
	}
	close(s.lifecycleNotifierStop)
	s.lifecycleNotifierStop = nil
	select {
	case <-s.lifecycleNotifierDone:
	case <-time.After(timeout):
		log.Printf("[notify] lifecycle notifier tick still running after %v; shutting down anyway", timeout)
	}
}

type lifecycleEventRow struct {
	RowID       int64
	ID          string
	TenantID    string
	RunID       string
	EventType   string
	EventTime   int64
	ObservedAt  int64
	ActorLogin  string
	TargetURL   string
	TargetLabel string
	FailureType string
	Detail      map[string]any
}

// lifecycleRunContext is the task_run_summaries context used to enrich messages.
type lifecycleRunContext struct {
	RunID            string
	IssueID          string
	IssueTitle       string
	Repo             string
	PrimaryPRURL     string
	WorkflowName     string
	FactoryName      string
	ClawID           string
	OwnerDisplayName string
	Model            string
}

// lifecycleDelivery bundles what the two event passes need to deliver one
// message: the notifiers and lifecycle config.
type lifecycleDelivery struct {
	// notifier is kept for package-local test helpers; runtime uses routes.
	notifier notify.Notifier
	routes   []lifecycleRouteDelivery
	lc       *types.LifecycleNotificationsConfig
	// incomplete records that at least one CONFIGURED route could not be
	// built this tick (unresolvable secret mid-rotation, bad settings). Its
	// events are still owed to it, so nothing may write the shared legacy
	// fence row — that row marks an event handled for every route and would
	// turn a temporary outage into permanent per-route event loss.
	incomplete bool
	// paused collects routes whose send failed in a way that must not be
	// retried for the rest of this tick (a config error, or a transient error
	// under the retry policy). Parking the route instead of the whole pass is
	// what keeps one archived channel from blocking every healthy one; the
	// events it missed keep their missing delivery rows and are retried on a
	// later tick.
	paused map[string]bool
}

type lifecycleRouteDelivery struct {
	notifier string
	send     notify.Notifier
	events   map[string]bool
}

// effectiveRoutes is the route set to deliver through, translating the
// test-only single-notifier shape into one unnamed route.
func (d lifecycleDelivery) effectiveRoutes() []lifecycleRouteDelivery {
	if len(d.routes) != 0 {
		return d.routes
	}
	if d.notifier != nil {
		return []lifecycleRouteDelivery{{notifier: "", send: d.notifier}}
	}
	return nil
}

// singleRoute reports the legacy single-`via` shape: exactly one deliverable
// route and no configured route missing. It gates the legacy-table writes and
// the legacy (un-namespaced) state keys so pre-routing behaviour — and any
// external reader of slack_notification_deliveries — is bit-for-bit unchanged.
func (d lifecycleDelivery) singleRoute() bool {
	return !d.incomplete && len(d.effectiveRoutes()) == 1
}

func (d lifecycleDelivery) pauseRoute(notifier string) {
	if d.paused != nil {
		d.paused[notifier] = true
	}
}

func (d lifecycleDelivery) routePaused(notifier string) bool { return d.paused[notifier] }

// hasLiveRoutes reports whether any route is still deliverable this tick.
func (d lifecycleDelivery) hasLiveRoutes() bool {
	for _, route := range d.effectiveRoutes() {
		if !d.routePaused(route.notifier) {
			return true
		}
	}
	return false
}

func (s *Server) lifecycleNotifierTick() {
	// Drain delivery rows whose post-send write failed before anything can
	// re-select (and re-send) those events.
	s.flushPendingNotificationDeliveries()
	cfg := s.notificationsConfig()
	if cfg == nil || !cfg.Lifecycle.IsEnabled() {
		// Keep the cursors at the end of their streams while notifications
		// are off, so re-enabling behaves like a fresh enable instead of
		// flushing the backlog accumulated during the disabled window.
		s.parkLifecycleWatermark()
		s.parkLifecycleClawState()
		return
	}
	if err := types.ValidateNotificationsConfig(cfg); err != nil {
		s.logPollWarningOnce("notify-config", "[notify] invalid notifications config — notifications paused: %v", err)
		return
	}
	lc := cfg.Lifecycle
	var routes []lifecycleRouteDelivery
	incomplete := false
	for _, route := range lc.EffectiveRoutes() {
		via := strings.TrimSpace(route.Via)
		n, err := s.notifierFor(via, cfg.Notifiers[via], s.hubSecretResolver())
		if err != nil {
			// The route keeps its own cursor and gets no delivery rows written
			// on its behalf, so everything produced during the outage is still
			// pending for it and is delivered once the notifier builds again.
			incomplete = true
			s.logPollWarningOnce("notify-notifier:"+via, "[notify] notifier %q unavailable — its lifecycle notifications are held until it can be built: %v", via, err)
			continue
		}
		s.clearPollWarning("notify-notifier:" + via)
		events := map[string]bool{}
		for _, event := range route.Events {
			events[event] = true
		}
		routes = append(routes, lifecycleRouteDelivery{notifier: via, send: n, events: events})
	}
	if len(routes) == 0 {
		return
	}
	s.clearPollWarning("notify-config")

	d := lifecycleDelivery{routes: routes, lc: lc, incomplete: incomplete, paused: map[string]bool{}}
	// Two independent event sources share the notifier and dedupe table:
	// task-run events for claws that belong to a task run, and the claw pass
	// for ad-hoc claws (task_run_id=''). See lifecycle_claw_notifier.go for
	// the exclusivity rule that prevents double notifications.
	s.lifecycleTaskRunPass(d)
	s.lifecycleClawPass(d)
}

// lifecycleRouteWatermarkKey names one route's cursor over task_run_events.
// Routes advance independently on purpose: a shared cursor lets a healthy
// route drag it past events a broken route never received, and those events
// are then unreachable forever (the pass only ever selects rowid > cursor).
// The legacy single-route shape keeps the original un-suffixed key so
// pre-routing state — and anything reading it — is unchanged.
func lifecycleRouteWatermarkKey(notifier string) string {
	return lifecycleStateWatermarkKey + ":" + notifier
}

func (s *Server) lifecycleWatermarkKeyFor(d lifecycleDelivery, notifier string) string {
	if notifier == "" || d.singleRoute() {
		return lifecycleStateWatermarkKey
	}
	return lifecycleRouteWatermarkKey(notifier)
}

// lifecycleRouteWatermark reads one route's cursor, falling back to the shared
// cursor the first time a route runs. Without the fallback, migrating a
// single-`via` config to routes would look like a first run and park the
// pending backlog; the shared cursor is kept as a floor (see
// advanceLifecycleWatermarkFloor) so the fallback stays close to "now" and a
// route added years later does not replay the archive.
func (s *Server) lifecycleRouteWatermark(key string) (int64, bool, error) {
	value, found, err := s.notifierStateInt64(key)
	if err != nil || found || key == lifecycleStateWatermarkKey {
		return value, found, err
	}
	return s.notifierStateInt64(lifecycleStateWatermarkKey)
}

// lifecycleRouteCursor pairs a route with its own position in the event stream.
type lifecycleRouteCursor struct {
	route     lifecycleRouteDelivery
	stateKey  string
	watermark int64
}

// lifecycleTaskRunPass scans task_run_events behind each route's rowid
// watermark and delivers the enabled event types.
func (s *Server) lifecycleTaskRunPass(d lifecycleDelivery) {
	var cursors []lifecycleRouteCursor
	lowest := int64(-1)
	for _, route := range d.effectiveRoutes() {
		key := s.lifecycleWatermarkKeyFor(d, route.notifier)
		watermark, found, err := s.lifecycleRouteWatermark(key)
		if err != nil {
			// A read failure must not be mistaken for a first run: resetting
			// the cursor here would silently skip the pending backlog.
			log.Printf("[notify] read watermark %s: %v", key, err)
			continue
		}
		if !found {
			// First run for this route: start at the current end of the event
			// stream so enabling it does not replay history.
			maxRow, err := s.lifecycleMaxEventRowID()
			if err != nil {
				log.Printf("[notify] read max event rowid: %v", err)
				continue
			}
			s.setNotifierStateInt64(key, maxRow)
			continue
		}
		cursors = append(cursors, lifecycleRouteCursor{route: route, stateKey: key, watermark: watermark})
		if lowest < 0 || watermark < lowest {
			lowest = watermark
		}
	}
	if len(cursors) == 0 {
		return
	}

	enabled := enabledLifecycleEventTypes(d.lc)
	if len(enabled) == 0 {
		// Every event category is toggled off: keep the cursors moving so
		// re-enabling a category does not flush the skipped backlog.
		s.parkLifecycleWatermark()
		return
	}
	// Park the muted categories too: a watermark only advances when an enabled
	// event is handled, so without this, muted-type events above the cursor
	// would be replayed the moment their toggle is re-enabled — or silently
	// dropped if a later enabled event happened to advance the watermark past
	// them first. Seeding "skipped" delivery rows (the claw pass's parking
	// mechanism) makes the outcome deterministic: whatever happened while a
	// category was muted stays muted. Seeding from the lowest cursor covers
	// every route in one statement.
	s.skipLifecycleMutedEvents(lowest, enabled)
	floor := int64(-1)
	for _, cursor := range cursors {
		handled := s.lifecycleTaskRunRoutePass(d, cursor, enabled)
		if floor < 0 || handled < floor {
			floor = handled
		}
	}
	if !d.singleRoute() && len(cursors) == len(d.effectiveRoutes()) {
		s.advanceLifecycleWatermarkFloor(d, floor)
	}
}

// advanceLifecycleWatermarkFloor keeps the shared cursor at the slowest route's
// position. It is what a route added later inherits, so leaving it frozen where
// the config first became multi-route would flood the new channel with the
// whole backlog since then. It is deliberately NOT advanced while a configured
// route could not be built this tick: that route has no cursor of its own yet,
// so the floor is the only thing standing between it and permanent event loss.
func (s *Server) advanceLifecycleWatermarkFloor(d lifecycleDelivery, floor int64) {
	if d.incomplete || floor <= 0 {
		return
	}
	shared, found, err := s.notifierStateInt64(lifecycleStateWatermarkKey)
	if err != nil || (found && shared >= floor) {
		return
	}
	s.setNotifierStateInt64(lifecycleStateWatermarkKey, floor)
}

// lifecycleTaskRunRoutePass delivers the pending task-run events for one route
// and advances that route's cursor only over what it actually handled. It
// returns the route's resulting cursor position.
func (s *Server) lifecycleTaskRunRoutePass(d lifecycleDelivery, cursor lifecycleRouteCursor, enabled map[string]bool) int64 {
	events, err := s.selectLifecycleCandidateEvents(cursor.watermark, enabled)
	if err != nil {
		log.Printf("[notify] select candidate events: %v", err)
		return cursor.watermark
	}
	maxHandled := cursor.watermark
	for _, ev := range events {
		if d.routePaused(cursor.route.notifier) {
			break
		}
		if len(cursor.route.events) != 0 && !cursor.route.events[ev.EventType] {
			// Not this route's event type: handled by definition.
			maxHandled = ev.RowID
			continue
		}
		msg := buildLifecycleMessage(ev, s.lifecycleRunContextFor(ev.RunID))
		if err := s.postLifecycleEventRoute(d, cursor.route, msg, ev.RunID, ev.ID); err != nil {
			handled, _ := s.handleLifecycleSendError(err, "event "+ev.ID+" ("+ev.EventType+")", ev.ID, ev.RunID, cursor.route.notifier, d.singleRoute())
			if handled {
				maxHandled = ev.RowID
				continue
			}
			// Park the destination, not the cursor: the event stays pending
			// for this route only.
			d.pauseRoute(cursor.route.notifier)
			break
		}
		maxHandled = ev.RowID
	}
	if maxHandled > cursor.watermark {
		s.setNotifierStateInt64(cursor.stateKey, maxHandled)
	}
	return maxHandled
}

// lifecycleMaxTransientFailures bounds consecutive transient failures for
// one delivery key. Unclassified errors default to transient, so a
// permanently-undeliverable message that a provider failed to classify would
// otherwise be re-sent every tick forever, blocking every notification behind
// it. At the default 5s poll interval the cap allows ~5 minutes of retries —
// enough to ride out real outages of that order, after which the one message
// is recorded failed and delivery moves on (a longer outage burns at most one
// event per cap window, never the backlog).
const lifecycleMaxTransientFailures = 60

// lifecycleSendWarningKeyFor namespaces the log-once send warning per route so
// one broken destination cannot swallow another's first failure.
func lifecycleSendWarningKeyFor(notifier string) string {
	if notifier == "" {
		return lifecycleSendWarningKey
	}
	return lifecycleSendWarningKey + ":" + notifier
}

// handleLifecycleSendError applies the shared send-failure policy: config
// errors pause THIS ROUTE (log-once), permanent errors are recorded as
// failed under deliveryKey (and count as handled), transient errors stop the
// route for this tick — until the same delivery key has failed
// lifecycleMaxTransientFailures times in a row, at which point it is recorded
// failed so it cannot wedge the cursor forever. Returns handled=true when the
// caller may move past the event; stop is informational for symmetry.
func (s *Server) handleLifecycleSendError(err error, what, deliveryKey, runKey, notifier string, singleRoute bool) (handled, stop bool) {
	switch notify.Classify(err) {
	case notify.ErrorConfig:
		// Bad token / archived channel fails every message on this route, not
		// this one message. Pause the route (leaving its cursor) instead of
		// burning events as failed, so delivery resumes once the config is
		// fixed — and pause only the route, so a single archived secondary
		// channel cannot mute every healthy destination.
		s.logPollWarningOnce(lifecycleSendWarningKeyFor(notifier), "[notify] delivery through %q paused until the notifier config is fixed: %v", notifier, err)
		return false, true
	case notify.ErrorPermanent:
		// Never succeeds on retry — record it so we stop trying.
		log.Printf("[notify] permanent failure for %s: %v", what, err)
		s.recordNotificationDeliveryV2(deliveryKey, notifier, runKey, "", notificationDeliveryStatusFailed)
		if singleRoute {
			s.recordNotificationDelivery(deliveryKey, runKey, "", notificationDeliveryStatusFailed)
		}
		return true, false
	default:
		// The streak is persisted (not an in-memory counter) because the cap
		// exists precisely for crashloop/deploy conditions: a hub restarting
		// more often than the cap window would reset an in-memory counter on
		// every boot, so the poisoned event would never be burned and the
		// cursor would stay wedged for as long as the restarts continued.
		stateKey := lifecycleTransientFailureStateKey(deliveryKey, notifier)
		if singleRoute {
			// Legacy single-`via` key shape: no per-notifier component, so
			// pre-routing streak state (and its corruption-recovery
			// semantics) keeps working unchanged.
			stateKey = lifecycleTransientFailureStateKey(deliveryKey)
		}
		count, _, stateErr := s.notifierStateInt64(stateKey)
		if stateErr != nil {
			// Unreadable streak state: retry without counting rather than
			// risk resetting (or double-counting) the streak.
			log.Printf("[notify] read transient-failure streak for %s: %v", what, stateErr)
			return false, true
		}
		count++
		if count >= lifecycleMaxTransientFailures {
			s.clearNotifierState(stateKey)
			log.Printf("[notify] giving up on %s after %d consecutive transient failures: %v", what, count, err)
			s.recordNotificationDeliveryV2(deliveryKey, notifier, runKey, "", notificationDeliveryStatusFailed)
			if singleRoute {
				s.recordNotificationDelivery(deliveryKey, runKey, "", notificationDeliveryStatusFailed)
			}
			return true, false
		}
		s.setNotifierStateInt64(stateKey, count)
		// Transient: leave the event (and the cursor) for the next tick.
		log.Printf("[notify] transient failure for %s, will retry: %v", what, err)
		return false, true
	}
}

// lifecycleTransientFailureStateKey keys one delivery key's consecutive
// transient-failure streak in slack_notifier_state. Rows exist only while a
// streak is live: they are cleared on success or when the cap burns the event.
func lifecycleTransientFailureStateKey(deliveryKey string, notifier ...string) string {
	if len(notifier) == 0 {
		return "transient_failures:" + deliveryKey
	} // legacy test/state compatibility
	return "transient_failures:" + notifier[0] + ":" + deliveryKey
}

// skipLifecycleMutedEvents seeds "skipped" delivery rows for events of the
// currently muted categories sitting past the cursor, so a later re-enable
// behaves like a fresh enable instead of flushing the muted window. Idempotent
// (ON CONFLICT DO NOTHING): events that were genuinely delivered keep their
// "sent" rows.
func (s *Server) skipLifecycleMutedEvents(afterRowID int64, enabled map[string]bool) {
	var muted []string
	for t := range lifecycleSupportedEventTypes() {
		if !enabled[t] {
			muted = append(muted, t)
		}
	}
	if len(muted) == 0 {
		return
	}
	sort.Strings(muted)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(muted)), ",")
	args := make([]any, 0, len(muted)+2)
	args = append(args, epochMillis(now()), notificationDeliveryStatusSkipped, afterRowID)
	for _, t := range muted {
		args = append(args, t)
	}
	if _, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
		SELECT e.id, e.run_id, ?, '', ?
		  FROM task_run_events e
		 WHERE e.rowid > ? AND e.event_type IN (`+placeholders+`)
		ON CONFLICT(event_id) DO NOTHING`, args...); err != nil {
		log.Printf("[notify] seed skipped muted-event deliveries: %v", err)
	}
}

// parkLifecycleWatermark advances every route cursor to the end of the event
// stream without sending anything. Used while notifications (or every event
// category) are disabled so a later re-enable does not replay the disabled
// window. Parking is driven off the state table rather than the config so it
// covers routes that have since been removed or renamed.
func (s *Server) parkLifecycleWatermark() {
	_, found, err := s.notifierStateInt64(lifecycleStateWatermarkKey)
	if err != nil {
		return
	}
	maxRow, err := s.lifecycleMaxEventRowID()
	if err != nil {
		log.Printf("[notify] park watermark: %v", err)
		return
	}
	if !found {
		// Never enabled: park only the per-route cursors, if any exist.
		var routeCursors int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM slack_notifier_state WHERE key LIKE ?`, lifecycleRouteWatermarkKey("%")).Scan(&routeCursors); err != nil || routeCursors == 0 {
			return
		}
	}
	if _, err := s.db.Exec(`
		UPDATE slack_notifier_state SET value = ?
		 WHERE (key = ? OR key LIKE ?) AND CAST(value AS INTEGER) < ?`,
		strconv.FormatInt(maxRow, 10), lifecycleStateWatermarkKey, lifecycleRouteWatermarkKey("%"), maxRow); err != nil {
		log.Printf("[notify] park watermark: %v", err)
	}
}

func (s *Server) lifecycleMaxEventRowID() (int64, error) {
	var maxRow int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM task_run_events`).Scan(&maxRow)
	return maxRow, err
}

// postLifecycleEvent renders one event, posts it top-level through every
// configured route and records the deliveries under deliveryKey. It returns a
// lifecycleRouteSendErrors carrying one entry per route that failed (nil when
// every route is done), so no route's failure can be swallowed by another's.
// The slack_run_threads table remains for legacy data, but lifecycle
// notifications no longer read or write it.
func (s *Server) postLifecycleEvent(d lifecycleDelivery, ev lifecycleEventRow, runCtx lifecycleRunContext, runKey, deliveryKey string) error {
	msg := buildLifecycleMessage(ev, runCtx)
	var errs lifecycleRouteSendErrors
	// complete tracks whether every route this event is owed to has reached a
	// terminal outcome. Only then may the legacy fence row be written.
	complete := !d.incomplete
	applicable, sentAny := 0, false
	for _, route := range d.effectiveRoutes() {
		if len(route.events) != 0 && !route.events[ev.EventType] {
			continue
		}
		applicable++
		if d.routePaused(route.notifier) {
			complete = false
			continue
		}
		sent, err := s.postLifecycleRoute(d, route, msg, runKey, deliveryKey)
		if err != nil {
			errs = append(errs, lifecycleRouteSendError{notifier: route.notifier, err: err})
			complete = false
			continue
		}
		sentAny = sentAny || sent
	}
	// The claw pass has no cursor: a handled event that ends up with no row in
	// the legacy table is re-selected on every tick forever and eventually
	// consumes the whole LIMIT, silently starving newer claws. The v2 rows are
	// per-route and the pass cannot know the route set in SQL, so once every
	// route is done a single legacy row fences the event for all of them.
	// Skipped when one route already wrote it (the legacy single-`via` shape),
	// so the real handle/status is never overwritten by a fence.
	if complete && !(d.singleRoute() && applicable > 0) {
		status := notificationDeliveryStatusSkipped
		if sentAny {
			status = notificationDeliveryStatusSent
		}
		s.recordNotificationDelivery(deliveryKey, runKey, "", status)
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// postLifecycleRoute delivers one already-rendered message through one route
// and records its delivery row. sent reports whether an external Send actually
// happened (false when the route was already deduped).
func (s *Server) postLifecycleRoute(d lifecycleDelivery, route lifecycleRouteDelivery, msg notify.Message, runKey, deliveryKey string) (sent bool, err error) {
	delivered, err := s.lifecycleRouteDelivered(deliveryKey, route.notifier)
	if err != nil {
		// Unclassified, therefore transient: the route retries and its cursor
		// stays put. Guessing "not delivered" here would re-send externally on
		// every tick for as long as the database stayed unhappy.
		return false, fmt.Errorf("check delivery record for %s via %s: %w", deliveryKey, route.notifier, err)
	}
	if delivered {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	handle, err := route.send.Send(ctx, msg)
	cancel()
	if err != nil {
		// Permanent failures are terminal for this route. Record them here so
		// a second broken route cannot be lost when the caller moves on after
		// handling the first error.
		if notify.Classify(err) == notify.ErrorPermanent {
			s.recordNotificationDeliveryV2(deliveryKey, route.notifier, runKey, "", notificationDeliveryStatusFailed)
			if d.singleRoute() {
				s.recordNotificationDelivery(deliveryKey, runKey, "", notificationDeliveryStatusFailed)
			}
		}
		return false, err
	}
	s.recordNotificationDeliveryV2(deliveryKey, route.notifier, runKey, handle, notificationDeliveryStatusSent)
	if d.singleRoute() {
		// Exactly one configured route is the legacy single-`via` shape:
		// keep writing the legacy table too so its dedupe/read behavior
		// (and any external readers) stay identical to pre-routing.
		s.recordNotificationDelivery(deliveryKey, runKey, handle, notificationDeliveryStatusSent)
		s.clearNotifierState(lifecycleTransientFailureStateKey(deliveryKey))
	}
	s.clearNotifierState(lifecycleTransientFailureStateKey(deliveryKey, route.notifier))
	s.clearPollWarning(lifecycleSendWarningKeyFor(route.notifier))
	return true, nil
}

// postLifecycleEventRoute is the single-route entry point used by the
// per-route task-run pass.
func (s *Server) postLifecycleEventRoute(d lifecycleDelivery, route lifecycleRouteDelivery, msg notify.Message, runKey, deliveryKey string) error {
	_, err := s.postLifecycleRoute(d, route, msg, runKey, deliveryKey)
	return err
}

type lifecycleRouteSendError struct {
	notifier string
	err      error
}

func (e lifecycleRouteSendError) Error() string { return e.err.Error() }

// lifecycleRouteSendErrors carries every failing route of one fan-out. Callers
// must apply the send-failure policy to each entry: classifying only the first
// one lets a later route's transient/config failure be mistaken for handled,
// which advances the cursor past an event that route never received.
type lifecycleRouteSendErrors []lifecycleRouteSendError

func (e lifecycleRouteSendErrors) Error() string {
	parts := make([]string, 0, len(e))
	for _, routeErr := range e {
		parts = append(parts, routeErr.notifier+": "+routeErr.err.Error())
	}
	return strings.Join(parts, "; ")
}

// lifecycleRouteDelivered reports whether this (event, route) pair already has
// a delivery record, counting rows still stashed in memory after a failed
// write. A query error is returned, never swallowed: reporting "not delivered"
// on a sick database turns into a duplicate external send every tick.
func (s *Server) lifecycleRouteDelivered(eventID, notifier string) (bool, error) {
	if s.lifecycleDeliveryPending(eventID, notifier) {
		return true, nil
	}
	var n int
	// The legacy read fallback is deliberately retained instead of a migration:
	// it applies to every configured route, including routes added after upgrade.
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM slack_notification_deliveries WHERE event_id=?) OR EXISTS(SELECT 1 FROM slack_notification_deliveries_v2 WHERE event_id=? AND notifier=?)`, eventID, eventID, notifier).Scan(&n); err != nil {
		return false, err
	}
	return n != 0, nil
}

// recordNotificationDeliveryV2 persists one per-route delivery row, stashing a
// failed write for retry exactly like the legacy table (see
// recordNotificationDelivery): the row is this route's only dedupe for
// claw-pass events, so logging and forgetting means a duplicate send per tick
// until the database recovers.
func (s *Server) recordNotificationDeliveryV2(eventID, notifier, runID, messageHandle, status string) {
	if err := s.execNotificationDeliveryV2(eventID, notifier, runID, messageHandle, status); err != nil {
		log.Printf("[notify] record route delivery for event %s via %s (will retry): %v", eventID, notifier, err)
		s.stashPendingDelivery(pendingDeliveryKey{eventID: eventID, notifier: notifier},
			pendingNotificationDelivery{runID: runID, messageHandle: messageHandle, status: status})
	}
}

func (s *Server) execNotificationDeliveryV2(eventID, notifier, runID, messageHandle, status string) error {
	_, err := s.db.Exec(`INSERT INTO slack_notification_deliveries_v2(event_id, notifier, run_id, delivered_at, message_ts, status) VALUES(?,?,?,?,?,?) ON CONFLICT(event_id, notifier) DO NOTHING`, eventID, notifier, runID, epochMillis(now()), messageHandle, status)
	return err
}

// pendingNotificationDelivery is a delivery whose post-send bookkeeping write
// failed; it is retried by flushPendingNotificationDeliveries.
type pendingNotificationDelivery struct {
	runID         string
	messageHandle string
	status        string
}

// lifecycleLegacyDeliveryNotifier keys the legacy (un-routed) delivery table in
// the pending stash. The sentinel cannot collide with a notifier name: config
// validation rejects the empty name and no name may contain a NUL.
const lifecycleLegacyDeliveryNotifier = "\x00legacy"

// pendingDeliveryKey identifies one stashed delivery row.
type pendingDeliveryKey struct {
	eventID  string
	notifier string
}

func (s *Server) stashPendingDelivery(key pendingDeliveryKey, p pendingNotificationDelivery) {
	if s.lifecyclePendingDeliveries == nil {
		s.lifecyclePendingDeliveries = make(map[pendingDeliveryKey]pendingNotificationDelivery)
	}
	s.lifecyclePendingDeliveries[key] = p
}

// recordNotificationDelivery persists one delivery row. A failed write is
// stashed in memory and retried on later ticks; while stashed, the delivery
// key still counts as handled (see lifecycleDeliveryPending) so the message
// cannot be re-sent externally. This matters most for claw-state events,
// which have no rowid cursor: the delivery row is their only dedupe, so
// logging and forgetting a failed write meant a duplicate send every tick
// until the DB accepted the insert.
func (s *Server) recordNotificationDelivery(eventID, runID, messageHandle, status string) {
	if err := s.execNotificationDelivery(eventID, runID, messageHandle, status); err != nil {
		log.Printf("[notify] record delivery for event %s (will retry): %v", eventID, err)
		s.stashPendingDelivery(pendingDeliveryKey{eventID: eventID, notifier: lifecycleLegacyDeliveryNotifier},
			pendingNotificationDelivery{runID: runID, messageHandle: messageHandle, status: status})
	}
}

func (s *Server) execNotificationDelivery(eventID, runID, messageHandle, status string) error {
	_, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
		VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`,
		eventID, runID, epochMillis(now()), messageHandle, status)
	return err
}

// lifecycleDeliveryPending reports whether the event was sent but its
// delivery row is still awaiting persistence — either the legacy row (a fence
// for every route) or this route's own v2 row.
func (s *Server) lifecycleDeliveryPending(eventID, notifier string) bool {
	if _, ok := s.lifecyclePendingDeliveries[pendingDeliveryKey{eventID: eventID, notifier: lifecycleLegacyDeliveryNotifier}]; ok {
		return true
	}
	_, ok := s.lifecyclePendingDeliveries[pendingDeliveryKey{eventID: eventID, notifier: notifier}]
	return ok
}

// flushPendingNotificationDeliveries retries delivery rows whose insert
// failed after a successful send. Runs at the top of every tick so the stash
// drains as soon as the DB accepts writes again.
//
// Known window: the stash only closes the dedupe gap within one process
// lifetime. A crash between a successful Send and the insert (or flush)
// landing loses the stash, and for claw-pass events — whose delivery row is
// their only dedupe — the SQL anti-join re-selects the claw on the next boot
// and the message is sent again. That is inherent to at-least-once delivery
// without a provider-side idempotency key (chat.postMessage has none); a
// distributed transaction is deliberately out of scope. The window is kept
// small: the insert runs immediately after Send, and graceful shutdown stops
// the loop and drains in-flight ticks before the DB closes
// (stopLifecycleNotifier), leaving only a hard crash in the microseconds
// between Send and insert exposed.
func (s *Server) flushPendingNotificationDeliveries() {
	for key, p := range s.lifecyclePendingDeliveries {
		var err error
		if key.notifier == lifecycleLegacyDeliveryNotifier {
			err = s.execNotificationDelivery(key.eventID, p.runID, p.messageHandle, p.status)
		} else {
			err = s.execNotificationDeliveryV2(key.eventID, key.notifier, p.runID, p.messageHandle, p.status)
		}
		if err != nil {
			log.Printf("[notify] retry delivery record for event %s: %v", key.eventID, err)
			return // DB still unhappy; keep the stash for the next tick
		}
		delete(s.lifecyclePendingDeliveries, key)
	}
}

// selectLifecycleCandidateEvents returns undelivered events after the rowid
// cursor, in insertion order. The dedupe happens in SQL on purpose: filtering
// delivered rows in Go would let them consume the LIMIT, and a burst larger
// than the batch size would then re-select the same delivered rows forever
// and permanently wedge the cursor.
func (s *Server) selectLifecycleCandidateEvents(afterRowID int64, enabled map[string]bool) ([]lifecycleEventRow, error) {
	if len(enabled) == 0 {
		return nil, nil
	}
	typeList := make([]string, 0, len(enabled))
	for t := range enabled {
		typeList = append(typeList, t)
	}
	sort.Strings(typeList)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(typeList)), ",")
	args := make([]any, 0, len(typeList)+1)
	args = append(args, afterRowID)
	for _, t := range typeList {
		args = append(args, t)
	}

	rows, err := s.db.Query(`
		SELECT rowid, id, tenant_id, run_id, event_type, event_time, observed_at,
		       actor_login, target_url, target_label, failure_type, detail
		  FROM task_run_events
		 WHERE rowid > ? AND event_type IN (`+placeholders+`)
		   AND NOT EXISTS (
			SELECT 1 FROM slack_notification_deliveries d WHERE d.event_id = task_run_events.id
		   )
		 ORDER BY rowid
		 LIMIT `+strconv.Itoa(lifecycleBatchSize), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []lifecycleEventRow
	for rows.Next() {
		var ev lifecycleEventRow
		var detail string
		if err := rows.Scan(&ev.RowID, &ev.ID, &ev.TenantID, &ev.RunID, &ev.EventType, &ev.EventTime, &ev.ObservedAt,
			&ev.ActorLogin, &ev.TargetURL, &ev.TargetLabel, &ev.FailureType, &detail); err != nil {
			return nil, err
		}
		if detail != "" {
			_ = json.Unmarshal([]byte(detail), &ev.Detail)
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

func (s *Server) lifecycleRunContextFor(runID string) lifecycleRunContext {
	runCtx := lifecycleRunContext{RunID: runID}
	err := s.db.QueryRow(`
		SELECT issue_id, issue_title, repo, primary_pr_url, workflow_name,
		       factory_name, claw_id, owner_display_name, model
		  FROM task_run_summaries WHERE run_id=?`, runID).Scan(
		&runCtx.IssueID, &runCtx.IssueTitle, &runCtx.Repo, &runCtx.PrimaryPRURL,
		&runCtx.WorkflowName, &runCtx.FactoryName, &runCtx.ClawID,
		&runCtx.OwnerDisplayName, &runCtx.Model)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("[notify] load run context for %s: %v", runID, err)
	}
	return runCtx
}

// notifierStateInt64 reads one notifier state value. found is false only when
// the row genuinely does not exist; any other failure (a locked or closed DB,
// a corrupted value) is returned as an error so callers never mistake a
// transient read failure for a first run and reset the cursor.
func (s *Server) notifierStateInt64(key string) (value int64, found bool, err error) {
	var raw string
	err = s.db.QueryRow(`SELECT value FROM slack_notifier_state WHERE key=?`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse state %s value %q: %w", key, raw, err)
	}
	return n, true, nil
}

func (s *Server) setNotifierStateInt64(key string, value int64) {
	if _, err := s.db.Exec(`
		INSERT INTO slack_notifier_state(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, strconv.FormatInt(value, 10)); err != nil {
		log.Printf("[notify] persist state %s: %v", key, err)
	}
}

func (s *Server) clearNotifierState(key string) {
	if _, err := s.db.Exec(`DELETE FROM slack_notifier_state WHERE key=?`, key); err != nil {
		log.Printf("[notify] clear state %s: %v", key, err)
	}
}

// ── Message content ───────────────────────────────────────────────────────────

// lifecycleIssueRef renders "ISSUE-1 — Title" (whichever parts exist).
func lifecycleIssueRef(runCtx lifecycleRunContext) string {
	switch {
	case runCtx.IssueID != "" && runCtx.IssueTitle != "":
		return runCtx.IssueID + " — " + runCtx.IssueTitle
	case runCtx.IssueTitle != "":
		return runCtx.IssueTitle
	case runCtx.IssueID != "":
		return runCtx.IssueID
	default:
		return ""
	}
}

func lifecycleOwnerLabel(runCtx lifecycleRunContext) string {
	switch {
	case runCtx.WorkflowName != "":
		return "workflow " + runCtx.WorkflowName
	case runCtx.FactoryName != "":
		return "factory " + runCtx.FactoryName
	case runCtx.OwnerDisplayName != "":
		return runCtx.OwnerDisplayName
	default:
		return ""
	}
}

func detailString(detail map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := detail[key]; ok {
			if str, ok := v.(string); ok && strings.TrimSpace(str) != "" {
				return str
			}
		}
	}
	return ""
}

// ── Per-event look ──
//
// Every event type gets its own (emoji, title, severity) so a channel full
// of notifications is scannable at a glance. Severity maps to provider
// emphasis (a colour stripe on Slack; see notify's severity palette):
//
//	info     work started
//	success  a PR exists
//	error    hard failures (someone must look)
//	warning  soft/ambiguous outcomes (ended, but maybe fine)
type lifecycleEventStyle struct {
	emoji    string // emoji shortcode hint, e.g. ":rocket:"
	title    string // human headline — never a raw snake_case identifier
	severity notify.Severity
}

// lifecycleEventStyles maps every supported event/failure type to its look.
//
// Titles hold one grammatical subject — the agent — so the set reads as one
// system, and every failure headline says in plain words what went wrong to
// a reader who has never seen this codebase ("Couldn't get a machine", not
// "provision_failed"). "Agent died" deliberately does not rhyme with "Agent
// started": the two highest-volume events must not differ by two letters.
//
// Emoji are chosen to stay recognisable at Slack's 16px inline size on both
// themes: saturated single-shape glyphs, no two alike in shape and colour,
// and nothing near-black or near-white (those vanish on one of the two
// themes — :stop_button: was invisible on dark mode). The raw snake_case
// type still appears as dim metadata in failure messages.
var lifecycleEventStyles = map[string]lifecycleEventStyle{
	taskRunEventAgentStarted:       {":rocket:", "Agent started", notify.SeverityInfo},
	taskRunEventPROpened:           {":tada:", "PR opened", notify.SeveritySuccess},
	taskRunEventAgentStopped:       {":skull:", "Agent died", notify.SeverityError},
	"creation_failed":              {":no_entry_sign:", "Couldn't create the agent", notify.SeverityError},
	taskRunFailureProvisionFailed:  {":construction:", "Couldn't get a machine", notify.SeverityError},
	taskRunFailureBootstrapFailed:  {":boom:", "Agent crashed during startup", notify.SeverityError},
	taskRunFailurePermissionOrAuth: {":lock:", "Agent was denied access", notify.SeverityError},
	taskRunFailureProviderLost:     {":satellite_antenna:", "Lost contact with the provider", notify.SeverityError},
	taskRunFailureTimeout:          {":hourglass_flowing_sand:", "Agent ran out of time", notify.SeverityWarning},
	taskRunEventAgentIdle:          {":zzz:", "Agent stalled", notify.SeverityWarning},
	taskRunEventDoneWithoutPR:      {":mailbox_with_no_mail:", "Agent finished without a PR", notify.SeverityWarning},
	"unknown_failure":              {":question:", "Agent failed", notify.SeverityError},
}

// lifecycleEventStyleFor resolves the style for an event. Failure events key
// on the failure type (the event type for synthetic rows); anything unmapped
// still gets a readable humanized headline, never a raw snake_case string.
func lifecycleEventStyleFor(ev lifecycleEventRow) lifecycleEventStyle {
	key := ev.EventType
	if key != taskRunEventAgentStarted && key != taskRunEventPROpened && key != taskRunEventAgentIdle {
		key = firstNonEmpty(ev.FailureType, ev.EventType)
	}
	if key == taskRunFailureUnknown {
		key = "unknown_failure"
	}
	if style, ok := lifecycleEventStyles[key]; ok {
		return style
	}
	return lifecycleEventStyle{":question:", humanizeEventType(key), notify.SeverityError}
}

// humanizeEventType turns an unmapped snake_case type into a headline-ready
// label ("manual_stop_before_delivery" → "Manual stop before delivery").
func humanizeEventType(t string) string {
	words := strings.ReplaceAll(t, "_", " ")
	if words == "" {
		return "Agent failed"
	}
	r := []rune(words)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// compressSummaryReason compresses a failure reason onto one short line for
// the message summary (which drives push-notification text): the reason is
// the part that tells the reader whether to get out of bed, but diagnostics
// can be 6000 chars of multi-line output.
func compressSummaryReason(reason string) string {
	const maxRunes = 140
	reason = strings.Join(strings.Fields(reason), " ")
	if runeLen(reason) > maxRunes {
		return truncateRunes(reason, maxRunes-1) + "…"
	}
	return reason
}

// buildLifecycleMessage renders one event as a semantic notify.Message —
// headline, severity, subject, long detail, dim metadata and a one-line
// summary — leaving all wire formatting to the provider. Content layout is
// shared by all event types so the destination reads consistently. It must
// never include tokens or secrets.
func buildLifecycleMessage(ev lifecycleEventRow, runCtx lifecycleRunContext) notify.Message {
	switch ev.EventType {
	case taskRunEventAgentStarted:
		return buildAgentStartedMessage(ev, runCtx)
	case taskRunEventPROpened:
		return buildPROpenedMessage(ev, runCtx)
	case taskRunEventAgentIdle:
		return buildAgentIdleMessage(ev, runCtx)
	default:
		return buildFailureMessage(ev, runCtx)
	}
}

// lifecycleMetaFields builds the dim metadata trail shared by the event
// shapes. Empty values are dropped by the provider.
func lifecycleMetaFields(runCtx lifecycleRunContext, withRepo, withModel bool) []notify.Field {
	var fields []notify.Field
	if withRepo && runCtx.Repo != "" {
		fields = append(fields, notify.Field{Label: "repo", Value: runCtx.Repo, Code: true})
	}
	if owner := lifecycleOwnerLabel(runCtx); owner != "" {
		fields = append(fields, notify.Field{Value: owner})
	}
	if withModel && runCtx.Model != "" {
		fields = append(fields, notify.Field{Label: "model", Value: runCtx.Model, Code: true})
	}
	if runCtx.ClawID != "" {
		fields = append(fields, notify.Field{Label: "claw", Value: shortID(runCtx.ClawID), Code: true})
	}
	return fields
}

func buildAgentStartedMessage(ev lifecycleEventRow, runCtx lifecycleRunContext) notify.Message {
	style := lifecycleEventStyleFor(ev)
	subject := lifecycleIssueRef(runCtx)
	if subject == "" {
		subject = "task"
	}
	return notify.Message{
		Title:    style.title,
		Emoji:    style.emoji,
		Severity: style.severity,
		Subject:  subject,
		// Label left empty: the link decorates the subject.
		Link:    notify.Link{URL: ev.TargetURL},
		Fields:  lifecycleMetaFields(runCtx, true, true),
		Summary: []string{runCtx.Repo, subject},
	}
}

func buildPROpenedMessage(ev lifecycleEventRow, runCtx lifecycleRunContext) notify.Message {
	style := lifecycleEventStyleFor(ev)
	prURL := firstNonEmpty(ev.TargetURL, runCtx.PrimaryPRURL)
	prLabel := ev.TargetLabel
	if prLabel == "" {
		repo := detailString(ev.Detail, "repo")
		if repo == "" {
			repo = runCtx.Repo
		}
		if num, ok := ev.Detail["prNumber"].(float64); ok && repo != "" {
			prLabel = fmt.Sprintf("%s#%d", repo, int(num))
		}
	}
	if prLabel == "" {
		prLabel = firstNonEmpty(prURL, "pull request")
	}
	// The PR label (repo#number) fills the summary's "where" slot: it
	// already names the repo, so a separate repo part would only repeat it.
	return notify.Message{
		Title:    style.title,
		Emoji:    style.emoji,
		Severity: style.severity,
		Subject:  lifecycleIssueRef(runCtx),
		Link:     notify.Link{URL: prURL, Label: prLabel},
		Fields:   lifecycleMetaFields(runCtx, false, false),
		Summary:  []string{prLabel, lifecycleIssueRef(runCtx)},
	}
}

// buildAgentIdleMessage renders the soft "agent stopped making progress"
// alert: how long the agent has been idle and what it was working on. It is a
// warning, not a failure — the agent is alive, just not moving.
func buildAgentIdleMessage(ev lifecycleEventRow, runCtx lifecycleRunContext) notify.Message {
	style := lifecycleEventStyleFor(ev)
	subject := lifecycleIssueRef(runCtx)
	if subject == "" {
		subject = "task"
	}
	// A bridge-error pause is carried by this event (see notifyBridgeErrorPause)
	// and renders from the real error text. "Agent stalled" would send an
	// operator to read the agent's chat; "no space left on device" sends them to
	// the disk, which is the whole point of NEXT-725 — the error existed only
	// inside the claw's chat and never reached a human.
	if bridgeError := detailString(ev.Detail, "bridgeError"); bridgeError != "" {
		turns := "Consecutive turns"
		if n := intFromDetail(ev.Detail, "bridgeErrorTurns", "bridge_error_turns"); n > 0 {
			turns = fmt.Sprintf("%d consecutive turns", n)
		}
		body := fmt.Sprintf("%s never reached the agent: claw-bridge returned a transport error instead. Automatic continuation is paused until someone sends a message.\n\n%s", turns, bridgeError)
		return notify.Message{
			Title:    style.title,
			Emoji:    style.emoji,
			Severity: style.severity,
			Subject:  subject,
			Body:     body,
			Fields:   lifecycleMetaFields(runCtx, true, false),
			Summary:  []string{runCtx.Repo, subject, "bridge error: " + compressSummaryReason(bridgeError)},
		}
	}
	idleLabel := agentIdleDurationLabel(ev.Detail)
	body := "No agent activity"
	if idleLabel != "" {
		body += " for " + idleLabel
	}
	body += ": the agent is connected but has not run a turn."
	if paused, ok := ev.Detail["noProgressPaused"].(bool); ok && paused {
		body += " Automatic continuation is paused after repeated turns without progress — it is waiting for a message."
	}
	summaryIdle := "idle"
	if idleLabel != "" {
		summaryIdle = "idle for " + idleLabel
	}
	return notify.Message{
		Title:    style.title,
		Emoji:    style.emoji,
		Severity: style.severity,
		Subject:  subject,
		Body:     body,
		Fields:   lifecycleMetaFields(runCtx, true, false),
		Summary:  []string{runCtx.Repo, subject, summaryIdle},
	}
}

func buildFailureMessage(ev lifecycleEventRow, runCtx lifecycleRunContext) notify.Message {
	style := lifecycleEventStyleFor(ev)
	failureType := firstNonEmpty(ev.FailureType, ev.EventType)
	reason := detailString(ev.Detail, "reason", "error")
	// The raw type stays available as dim metadata for operators who grep;
	// it leads the trail so it is always visible.
	fields := append([]notify.Field{{Value: failureType, Code: true}}, lifecycleMetaFields(runCtx, true, false)...)
	// Failure summaries keep the issue reference short (id when there is
	// one) so the reason — the part that says how bad it is — fits before a
	// phone lock screen truncates. The raw failure type is deliberately
	// absent: it duplicates the human title in machine form.
	return notify.Message{
		Title:    style.title,
		Emoji:    style.emoji,
		Severity: style.severity,
		Subject:  lifecycleIssueRef(runCtx),
		Body:     reason,
		Fields:   fields,
		Summary: []string{
			runCtx.Repo,
			firstNonEmpty(runCtx.IssueID, runCtx.IssueTitle),
			compressSummaryReason(reason),
		},
	}
}

// ── Manual trigger endpoint ───────────────────────────────────────────────────

// handleNotificationTest lets an admin probe the notification pipeline:
// dry_run returns the provider-rendered payload without sending; a real run
// posts through the configured lifecycle notifier. It never writes delivery
// or thread state — it is a side-effect-free probe apart from the message
// itself.
func (s *Server) handleNotificationTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		EventType string `json:"event_type"`
		RunID     string `json:"run_id"`
		ClawID    string `json:"claw_id"`
		// Via names which configured notifier to probe. Empty means the first
		// effective route — lifecycle.via for a legacy single-channel config.
		Via    string `json:"via"`
		DryRun bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	supported := lifecycleSupportedEventTypes()
	if !supported[body.EventType] {
		valid := make([]string, 0, len(supported))
		for t := range supported {
			valid = append(valid, t)
		}
		sort.Strings(valid)
		jsonError(w, http.StatusBadRequest, "invalid event_type "+strconv.Quote(body.EventType)+"; valid values: "+strings.Join(valid, ", "))
		return
	}
	cfg := s.notificationsConfig()
	if cfg == nil || !cfg.Lifecycle.IsEnabled() {
		jsonError(w, http.StatusBadRequest, "lifecycle notifications are not configured or not enabled (set notifications.lifecycle in hub.yaml)")
		return
	}
	if err := types.ValidateNotificationsConfig(cfg); err != nil {
		jsonError(w, http.StatusBadRequest, "notifications config invalid: "+err.Error())
		return
	}
	// Resolve which destination to probe. A routes-only config has no
	// lifecycle.via at all, so defaulting to it would 400 every test send the
	// Notifier UI makes. Trimmed to match ValidateNotificationsConfig (see
	// lifecycleNotifierTick).
	via := strings.TrimSpace(body.Via)
	if via == "" {
		routes := cfg.Lifecycle.EffectiveRoutes()
		if len(routes) == 0 { // unreachable: validated above while enabled
			jsonError(w, http.StatusBadRequest, "notifications.lifecycle has no configured route to test")
			return
		}
		via = strings.TrimSpace(routes[0].Via)
	}
	nc, ok := cfg.Notifiers[via]
	if !ok {
		jsonError(w, http.StatusBadRequest, "via "+strconv.Quote(via)+" does not name a configured notifier (defined: "+configuredNotifierNames(cfg.Notifiers)+")")
		return
	}

	if body.RunID != "" && body.ClawID != "" {
		jsonError(w, http.StatusBadRequest, "run_id and claw_id are mutually exclusive")
		return
	}

	ev := lifecycleEventRow{EventType: body.EventType}
	var runCtx lifecycleRunContext
	switch {
	case body.RunID != "":
		runCtx = s.lifecycleRunContextFor(body.RunID)
		if runCtx.IssueID == "" && runCtx.IssueTitle == "" && runCtx.Repo == "" && runCtx.ClawID == "" {
			jsonError(w, http.StatusNotFound, "run "+strconv.Quote(body.RunID)+" not found in task_run_summaries")
			return
		}
		if body.EventType == taskRunEventPROpened {
			ev.TargetURL = runCtx.PrimaryPRURL
		}
	case body.ClawID != "":
		// Claw-sourced variant: render from real claws-table context, the same
		// context the claw pass uses for ad-hoc claws.
		claw, ok, err := s.lifecycleClawByID(body.ClawID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "load claw: "+err.Error())
			return
		}
		if !ok {
			jsonError(w, http.StatusNotFound, "claw "+strconv.Quote(body.ClawID)+" not found")
			return
		}
		runCtx = lifecycleClawRunContext(claw)
		switch body.EventType {
		case taskRunEventPROpened:
			var repo, prURL string
			var prNumber int
			err := s.db.QueryRow(`SELECT repo, pr_number, pr_url FROM claw_prs WHERE claw_id=? ORDER BY rowid DESC LIMIT 1`,
				body.ClawID).Scan(&repo, &prNumber, &prURL)
			if err == nil {
				ev.TargetURL = prURL
				ev.TargetLabel = fmt.Sprintf("%s#%d", repo, prNumber)
				runCtx.Repo = repo
			}
		case taskRunEventAgentStarted:
			// no target — renders without a link, like real claw starts
		case taskRunEventAgentIdle:
			// Render from the claw's real idle latch when it has one; a claw
			// that is not currently latched gets the synthetic sample value.
			var idleSince int64
			_ = s.db.QueryRow(`SELECT idle_since FROM claws WHERE id=?`, body.ClawID).Scan(&idleSince)
			minutes := 9
			if idleSince > 0 {
				if m := int(now().Sub(time.UnixMilli(idleSince)).Minutes()); m > 0 {
					minutes = m
				}
			}
			ev.Detail = map[string]any{"idleSince": idleSince, "idleMinutes": minutes}
		default:
			ev.FailureType = body.EventType
			if claw.BootstrapDiagnostic != "" {
				ev.Detail = map[string]any{"reason": claw.BootstrapDiagnostic}
			}
		}
	default:
		runCtx, ev = sampleLifecycleContext(body.EventType)
	}

	msg := buildLifecycleMessage(ev, runCtx)
	secrets := s.hubSecretResolver()

	if body.DryRun {
		// A dry run must work even while the secret is missing (the operator
		// may be iterating on formatting before wiring credentials), so
		// unresolvable secrets fall back to a placeholder. The payload never
		// contains the secret, so the preview is unaffected. The throwaway
		// notifier instance is deliberately not cached, but it is built from
		// s.notifierSettings(nc) — the exact settings a real send uses — so
		// the preview can never diverge from what a real send would post.
		dryResolver := func(name string) (string, bool) {
			if v, ok := secrets(name); ok && v != "" {
				return v, true
			}
			return "dry-run-placeholder", true
		}
		n, err := notify.New(nc.Type, s.notifierSettings(nc), dryResolver)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "notifier "+strconv.Quote(via)+" invalid: "+err.Error())
			return
		}
		payload, err := renderNotifierPayload(n, msg)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "render payload: "+err.Error())
			return
		}
		jsonOK(w, map[string]any{"dry_run": true, "via": via, "payload": payload})
		return
	}

	n, err := s.notifierFor(via, nc, secrets)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "notifier "+strconv.Quote(via)+" unavailable: "+err.Error())
		return
	}
	payload, err := renderNotifierPayload(n, msg)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "render payload: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Always post top-level: a test send must not create or reuse a thread.
	handle, err := n.Send(ctx, msg)
	if err != nil {
		// Surface the provider error so scope/channel problems are
		// debuggable. Providers redact anything derived from a response body
		// (see the Slack notifier's redactSecrets) before it reaches an
		// error, so this can never leak the token.
		jsonError(w, http.StatusBadGateway, "notification send failed: "+err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "message_id": handle, "via": via, "payload": payload})
}

// configuredNotifierNames lists the configured notifier names for error text.
func configuredNotifierNames(notifiers map[string]types.NotifierConfig) string {
	if len(notifiers) == 0 {
		return "none"
	}
	names := make([]string, 0, len(notifiers))
	for name := range notifiers {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// renderNotifierPayload returns the provider-rendered wire payload when the
// provider supports previews, and a generic view of the message otherwise.
func renderNotifierPayload(n notify.Notifier, msg notify.Message) (any, error) {
	if r, ok := n.(notify.PayloadRenderer); ok {
		return r.RenderPayload(msg)
	}
	return map[string]any{"message": msg}, nil
}

// sampleLifecycleContext builds clearly-marked synthetic data for test sends
// without a run_id.
func sampleLifecycleContext(eventType string) (lifecycleRunContext, lifecycleEventRow) {
	runCtx := lifecycleRunContext{
		RunID:      "sample-run",
		IssueID:    "SAMPLE-123",
		IssueTitle: "Sample issue for notification test",
		Repo:       "example/repo",
		ClawID:     "sample-claw",
		Model:      "sample/model",
	}
	ev := lifecycleEventRow{EventType: eventType}
	switch eventType {
	case taskRunEventPROpened:
		ev.TargetURL = "https://github.com/example/repo/pull/123"
		ev.TargetLabel = "example/repo#123"
	case taskRunEventAgentStarted:
		// no target — renders without a link, like most real starts
	case taskRunEventAgentIdle:
		ev.Detail = map[string]any{"idleMinutes": 9}
	default:
		ev.FailureType = eventType
		ev.Detail = map[string]any{"reason": "synthetic sample failure (test send)"}
	}
	return runCtx, ev
}
