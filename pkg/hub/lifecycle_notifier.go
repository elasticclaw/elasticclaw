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

	// lifecycleStateRoutedKey records that the per-route state scheme has been
	// live at least once. The per-route keys alone cannot answer that question:
	// pruneLifecycleRouteState deletes the cursors of routes that are no longer
	// configured, so replacing every route in one save erases the evidence in
	// the same tick the newcomers are seeded — and they would then inherit the
	// (long frozen) shared floor instead of the stream head. Nothing deletes
	// this key: once routing is live it stays live for the hub's lifetime.
	lifecycleStateRoutedKey = "routes_live"

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

// lifecycleSupportedEventTypes is everything the notifier can render: the
// wire types a route's allow-list may name (types.LifecycleEventTypes) plus
// the concrete failure kinds, which never appear as task_run_events.event_type
// but do carry their own rendering (see lifecycleEventStyles). Callers that
// mean the narrower route vocabulary use types.IsLifecycleEventType instead.
func lifecycleSupportedEventTypes() map[string]bool {
	supported := make(map[string]bool, len(types.LifecycleEventTypes)+len(lifecycleFailureEventTypes))
	for _, t := range types.LifecycleEventTypes {
		supported[t] = true
	}
	for t := range lifecycleFailureEventTypes {
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
	if src.Scheduled != nil {
		cfg.Scheduled = append([]types.ScheduledNotificationConfig(nil), src.Scheduled...)
		for i := range cfg.Scheduled {
			cfg.Scheduled[i].Via = append([]string(nil), src.Scheduled[i].Via...)
			cfg.Scheduled[i].Weekdays = append([]string(nil), src.Scheduled[i].Weekdays...)
		}
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
	agentStarted, prOpened, failures, agentIdle, stageStalled := lifecycleClawKindsEnabled(lc)
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
	if stageStalled {
		enabled[taskRunEventStageStalled] = true
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
	if _, found, err := s.notifierStateInt64(lifecycleStateClawBaselineKey); err == nil {
		if !found {
			if err := s.seedLifecycleClawBaseline(); err == nil {
				s.setNotifierStateInt64(lifecycleStateClawBaselineKey, 1)
				// The shared seeding just recorded the current claw state for
				// every route, so none of them needs its own seeding pass. The
				// in-tick first-enable branch stamps the same keys, but it can
				// never run once this path created the shared flag — leaving
				// every configured route to be treated as "newly added" on its
				// first tick and have its pending claw events buried as
				// "skipped" (a route whose notifier cannot even be built at
				// boot is exactly the case the tick promises is "still pending
				// for it").
				s.stampLifecycleClawRouteBaselines(cfg.Lifecycle)
			} else {
				log.Printf("[notify] init claw baseline: %v", err)
			}
		} else if routed, err := s.lifecycleRouteStateExists(lifecycleClawRouteBaselinePrefix); err == nil && !routed {
			// The shared flag predates the per-route scheme: this is the first
			// boot after a single-`via` config was migrated to routes. Every
			// claw the incumbent already handled is fenced by the legacy
			// delivery table (lifecycleRouteDelivered reads it for every
			// route), so stamping without seeding is enough — and it is what
			// keeps the incumbent's undelivered backlog (claws that connected
			// while its token secret was broken) from being buried the moment
			// a second route makes it look newly added.
			//
			// Only the INCUMBENT may be stamped. The stamp is also what
			// lifecycleSingleViaIncumbent reads, so stamping every configured
			// route would present a channel added in the same maintenance
			// window as the legacy incumbent, hand it the incumbent's frozen
			// shared cursor and replay the whole stalled backlog into it.
			//
			// An absent stamp is NOT proof the scheme never went live:
			// pruneLifecycleRouteState deletes the stamps of routes that left
			// the config and deliberately keeps the routes_live latch for
			// exactly this reason. Consult the latch first, or a restart taken
			// while a config was collapsed onto one brand-new route would
			// present that route as the legacy incumbent.
			var via string
			live, liveErr := s.lifecycleRoutingSchemeLive()
			if liveErr != nil {
				log.Printf("[notify] read routing scheme state: %v", liveErr)
			} else if !live {
				via = lifecycleLegacyIncumbentVia(cfg.Lifecycle)
			}
			if via != "" {
				s.setNotifierStateInt64(lifecycleClawRouteBaselineKey(via), 1)
			} else if liveErr == nil {
				// Either the scheme has already been live, or the config
				// carries several routes and no longer says which one was the
				// single `via`: no route can be verified as the incumbent.
				// Latch the scheme instead: every route
				// starts at the stream head and ensureLifecycleClawRouteBaselines
				// seeds its claw fence, which loses the incumbent's undelivered
				// backlog but never floods a brand-new channel with it.
				s.setNotifierStateInt64(lifecycleStateRoutedKey, 1)
			}
		}
	}
	// Give the CONFIGURED routes their own state here too, for the same reason
	// the shared baseline is written synchronously: a route added while the hub
	// was down (a `via` migrated to routes in hub.yaml) would otherwise be
	// baselined by the first poll tick, one poll interval after the producers
	// started, and silently miss whatever they produced in that window. Both
	// helpers are idempotent, so the tick keeps covering runtime saves.
	if err := types.ValidateLifecycleNotificationsConfig(cfg); err == nil {
		s.pruneLifecycleRouteState(cfg.Lifecycle)
		s.ensureLifecycleRouteWatermarks(cfg.Lifecycle)
		s.ensureLifecycleClawRouteBaselines(cfg.Lifecycle)
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
	// events are still owed to it, so the shared task-run watermark floor must
	// not advance past them. It deliberately does NOT hold back the routes that
	// did build: both passes dedupe per route, so an indefinitely broken
	// channel cannot wedge a healthy one's delivery.
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
	// Only the config this tick consumes is judged (notifiers plus the
	// lifecycle block): a defect in the scheduled block pauses scheduled
	// reports, never lifecycle alerts.
	if err := types.ValidateLifecycleNotificationsConfig(cfg); err != nil {
		s.logPollWarningOnce("notify-config", "[notify] invalid notifications config — lifecycle notifications paused: %v", err)
		return
	}
	lc := cfg.Lifecycle
	// Both run off the CONFIGURED routes, before any notifier is built, so a
	// route that cannot be built this tick is still treated as configured.
	s.pruneLifecycleRouteState(lc)
	s.ensureLifecycleRouteWatermarks(lc)
	s.ensureLifecycleClawRouteBaselines(lc)
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
	if notifier == "" {
		return lifecycleStateWatermarkKey
	}
	key := lifecycleRouteWatermarkKey(notifier)
	if !d.singleRoute() {
		return key
	}
	// Collapsing a multi-route config back to one route must not silently
	// re-point the survivor at the shared key: that floor is pinned to the
	// SLOWEST route of the multi-route era, and the events above it carry no
	// legacy delivery row (multi-route writes v2 rows only, keyed by the OTHER
	// notifiers), so the survivor would re-send every one of them. A route only
	// has a cursor of its own because it was configured alongside a sibling, so
	// keeping it whenever it exists is exactly the "no silent re-point" rule.
	// A read error deliberately keeps the per-route key too: the pass's own read
	// then fails and the route waits, rather than replaying from the floor.
	if _, found, err := s.notifierStateInt64(key); err != nil || found {
		return key
	}
	// The genuine legacy incumbent — the route that has already been running as
	// the single `via` — reads the shared key: that IS its position, and its
	// history lives in the legacy delivery table.
	if s.lifecycleSingleViaIncumbent(notifier) {
		return lifecycleStateWatermarkKey
	}
	// Anything else arriving here is a NEWCOMER that happens to be alone:
	// collapsing a multi-route config to ONE brand-new route deletes the old
	// cursors (pruneLifecycleRouteState) and leaves none of its own, so without
	// this it would adopt the floor frozen at the stalled route's position and
	// replay the whole multi-route-era backlog — none of which carries a legacy
	// row, because that era wrote v2 rows keyed by the OLD notifiers only. Once
	// the per-route scheme has been live, start it at the head like any other
	// newcomer; the pass's first-run backstop materialises the cursor there.
	if live, err := s.lifecycleRoutingSchemeLive(); err != nil || live {
		return key
	}
	return lifecycleStateWatermarkKey
}

// lifecycleSingleViaIncumbent reports whether a route has already been
// delivering as the legacy single-`via` incumbent. The single-route shape
// stamps `claw_baseline_done:<via>` without seeding for exactly this purpose
// (ensureLifecycleClawRouteBaseline), so the stamp — with no per-route cursor
// beside it — is what tells the incumbent apart from a route that only just
// appeared in config. An unreadable state is not an incumbent: the callers'
// fallbacks (per-route key, stream head) are the safe side of that answer.
// lifecycleLegacyIncumbentVia names the route that has been delivering as the
// legacy single `via`, or "" when the config no longer says which one it was.
// It is only meaningful against state written before the per-route scheme
// existed, where the incumbent is not recorded anywhere else.
func lifecycleLegacyIncumbentVia(lc *types.LifecycleNotificationsConfig) string {
	routes := lc.EffectiveRoutes()
	if len(routes) == 1 {
		return strings.TrimSpace(routes[0].Via)
	}
	// A hand-written config can still carry the legacy `via` alongside routes
	// (the settings screen clears it, hub.yaml does not have to); it names the
	// incumbent as long as it is one of the routes.
	via := strings.TrimSpace(lc.Via)
	for _, route := range routes {
		if via != "" && strings.TrimSpace(route.Via) == via {
			return via
		}
	}
	return ""
}

// lifecycleRoutesNeedOwnState reports whether the configured routes must be
// given per-route state (cursor and claw fence) at CONFIG time rather than at
// the first tick their notifier happens to build. Multi-route configs always
// do. A LONE route does too once the per-route scheme has gone live and that
// route is not the legacy incumbent: collapsing a config onto one brand-new
// channel leaves it as the hub's only route, and materialising its state only
// on recovery would bury everything produced while its notifier could not be
// built — the very window each tick logs as "held until it can be built".
// The genuine legacy single-`via` shape keeps using the shared state and gets
// no per-route keys, so pre-routing behaviour is byte-for-byte unchanged.
func (s *Server) lifecycleRoutesNeedOwnState(routes []types.LifecycleRoute) bool {
	if len(routes) >= 2 {
		return true
	}
	if len(routes) == 0 {
		return false
	}
	via := strings.TrimSpace(routes[0].Via)
	if via == "" || s.lifecycleSingleViaIncumbent(via) {
		return false
	}
	live, err := s.lifecycleRoutingSchemeLive()
	return err == nil && live
}

func (s *Server) lifecycleSingleViaIncumbent(via string) bool {
	if via == "" {
		return false
	}
	_, found, err := s.notifierStateInt64(lifecycleClawRouteBaselineKey(via))
	return err == nil && found
}

// ensureLifecycleRouteWatermarks gives every CONFIGURED route a cursor of its
// own before any delivery runs — including a route whose notifier cannot be
// built this tick, which would otherwise still have none when it recovers.
// Materialising the cursor here, rather than falling back lazily inside the
// pass, is what lets a route added later be told apart from one that has simply
// never delivered anything yet: the newcomer has no cursor because it was not
// configured, and must start at the stream head.
//
// The routes present when a single-`via` config is migrated to routes inherit
// the shared cursor — the incumbent's real position — so the migration does not
// look like a first run and park the pending backlog. Once ANY route keeps a
// cursor the per-route scheme is live, and a newcomer starts at the head
// instead: from then on the shared key is a floor pinned to the SLOWEST route
// (advanceLifecycleWatermarkFloor bails entirely while a configured route
// cannot be built), so inheriting it would flood the new channel with
// everything that piled up behind a paused or unbuildable sibling.
func (s *Server) ensureLifecycleRouteWatermarks(lc *types.LifecycleNotificationsConfig) {
	routes := lc.EffectiveRoutes()
	if !s.lifecycleRoutesNeedOwnState(routes) {
		// The legacy single-route shape keeps using the shared key
		// (lifecycleWatermarkKeyFor), so it needs no cursor of its own.
		return
	}
	routed, err := s.lifecycleRoutingSchemeLive()
	if err != nil {
		// Unreadable state must not be mistaken for "nothing routed yet": that
		// would hand a route added later the (possibly long-frozen) shared floor.
		log.Printf("[notify] read route watermark state: %v", err)
		return
	}
	shared, sharedFound, err := s.notifierStateInt64(lifecycleStateWatermarkKey)
	if err != nil {
		log.Printf("[notify] read watermark floor: %v", err)
		return
	}
	head, err := s.lifecycleMaxEventRowID()
	if err != nil {
		log.Printf("[notify] read max event rowid: %v", err)
		return
	}
	if !sharedFound {
		// A config that is multi-route from its very first enable never gets
		// the floor written by advanceLifecycleWatermarkFloor while a sibling
		// route cannot be built (it bails on d.incomplete). Freeze it here so
		// the floor exists from the start.
		s.setNotifierStateInt64(lifecycleStateWatermarkKey, head)
	}
	// At the migration tick only the incumbent may inherit the shared cursor.
	// The shared key is the incumbent's real position, but for the channel being
	// ADDED — the whole reason the migration runs through the settings screen —
	// it is a cursor frozen wherever the incumbent stalled, and the events above
	// it carry no delivery row of any kind that could fence them, so the
	// newcomer would replay the incumbent's entire backlog. A config that was
	// multi-route from its very first enable has no incumbent to tell apart, and
	// there its routes do all share one starting position.
	incumbents := map[string]bool{}
	for _, route := range routes {
		if via := strings.TrimSpace(route.Via); via != "" && s.lifecycleSingleViaIncumbent(via) {
			incumbents[via] = true
		}
	}
	for _, route := range routes {
		via := strings.TrimSpace(route.Via)
		if via == "" {
			continue
		}
		key := lifecycleRouteWatermarkKey(via)
		if _, found, err := s.notifierStateInt64(key); err != nil || found {
			continue
		}
		if sharedFound && incumbents[via] {
			// The incumbent is read off the SHARED key precisely because it
			// never got one of its own (lifecycleWatermarkKeyFor), so that is
			// its real position no matter how the routes_live latch came to be
			// set — a restricted single route latches it on its very first tick
			// (lifecycleClawRouteSkip) without ever leaving the shared key.
			// Stamping the head here instead would drop everything the
			// incumbent had not yet delivered, silently and for good.
			s.setNotifierStateInt64(key, shared)
			continue
		}
		if !routed && sharedFound && len(incumbents) == 0 {
			s.setNotifierStateInt64(key, shared)
			continue
		}
		s.setNotifierStateInt64(key, head)
	}
	if !routed {
		// Latch the scheme so a later save that replaces every route at once
		// cannot make the newcomers look like a fresh migration.
		s.setNotifierStateInt64(lifecycleStateRoutedKey, 1)
	}
}

// lifecycleRouteWatermarksMaterialised reports whether every configured route
// already carries a cursor of its own. It is the guard for writing per-route
// claw baselines: a route stamped as baselined while it has no cursor reads as
// the legacy incumbent (lifecycleSingleViaIncumbent) and would then be handed
// the shared floor and replay everything piled up behind it.
func (s *Server) lifecycleRouteWatermarksMaterialised(routes []types.LifecycleRoute) bool {
	for _, route := range routes {
		via := strings.TrimSpace(route.Via)
		if via == "" {
			continue
		}
		if _, found, err := s.notifierStateInt64(lifecycleRouteWatermarkKey(via)); err != nil || !found {
			return false
		}
	}
	return true
}

// lifecycleRoutingSchemeLive reports whether the per-route state scheme has
// already gone live for this hub. It is how both passes tell a legacy
// single-`via` migration (nothing per-route recorded yet, so the shared state is
// the routes' shared history) from routes genuinely added later (the scheme is
// already live, so a newcomer must start clean). The persisted latch is the
// authority; the per-route cursors are the fallback for hubs that went
// multi-route before the latch existed.
func (s *Server) lifecycleRoutingSchemeLive() (bool, error) {
	if _, found, err := s.notifierStateInt64(lifecycleStateRoutedKey); err != nil {
		return false, err
	} else if found {
		return true, nil
	}
	return s.lifecycleRouteStateExists(lifecycleStateWatermarkKey + ":")
}

// lifecycleRouteStateExists reports whether any per-route state key with the
// given prefix is persisted. It is how both passes tell a legacy migration
// (nothing per-route recorded yet, so the shared state is the routes' shared
// history) from a route genuinely added later (the per-route scheme is already
// live, so the newcomer must start clean).
func (s *Server) lifecycleRouteStateExists(prefix string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM slack_notifier_state WHERE key LIKE ?)`, prefix+"%").Scan(&n); err != nil {
		return false, err
	}
	return n != 0, nil
}

// pruneLifecycleRouteState drops the cursor and claw baseline of notifiers that
// are no longer routed. Nothing else clears them while lifecycle alerts stay
// enabled (parkLifecycleWatermark only runs while they are off, and
// advanceLifecycleWatermarkFloor only touches the shared key), so a route that
// is removed and later re-added under the same name would resume from its stale
// cursor and flush the entire removal window into its channel — the events of
// that window carry no v2 delivery row for it, and no legacy row either
// whenever two or more routes remained. Dropping the state makes a re-added
// route behave exactly like a newly added one.
func (s *Server) pruneLifecycleRouteState(lc *types.LifecycleNotificationsConfig) {
	configured := map[string]bool{}
	for _, route := range lc.EffectiveRoutes() {
		if via := strings.TrimSpace(route.Via); via != "" {
			configured[via] = true
		}
	}
	rows, err := s.db.Query(`SELECT key FROM slack_notifier_state WHERE key LIKE ? OR key LIKE ?`,
		lifecycleRouteWatermarkKey("%"), lifecycleClawRouteBaselinePrefix+"%")
	if err != nil {
		log.Printf("[notify] list route state: %v", err)
		return
	}
	var stale []string
	gone := map[string]bool{}
	schemeLive := false
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			log.Printf("[notify] list route state: %v", err)
			rows.Close()
			return
		}
		if strings.HasPrefix(key, lifecycleStateWatermarkKey+":") {
			schemeLive = true
		}
		notifier := key[strings.Index(key, ":")+1:]
		if !configured[notifier] {
			stale = append(stale, key)
			gone[notifier] = true
			// Deleting ANY per-route stamp counts as evidence too: a hub that
			// never went past the legacy single-`via` shape records its
			// incumbent only as a claw baseline stamp, so replacing that route
			// with an all-new set would leave nothing per-route behind and let
			// ensureLifecycleRouteWatermarks read "nothing routed yet" and hand
			// each newcomer the incumbent's stalled shared cursor.
			schemeLive = true
		}
	}
	rows.Close()
	if schemeLive {
		// Latch before deleting: a save that replaces EVERY route removes the
		// only per-route cursors, and ensureLifecycleRouteWatermarks (which runs
		// straight after) would then read "nothing routed yet" and hand the
		// brand-new channels the stale shared floor. This covers hubs that went
		// multi-route before the latch existed.
		if _, found, err := s.notifierStateInt64(lifecycleStateRoutedKey); err == nil && !found {
			s.setNotifierStateInt64(lifecycleStateRoutedKey, 1)
		}
	}
	for _, key := range stale {
		s.clearNotifierState(key)
	}
	s.clearLifecycleTransientFailures(gone)
}

// clearLifecycleTransientFailures drops the transient-failure streaks of
// notifiers that are no longer routed. A streak row exists only while a streak
// is live — a successful send clears its own key, the cap clears the key it
// burns — so a route removed mid-streak leaves its counters behind with nothing
// left to clear them. Re-adding the route under the same name would then hand
// it a retry budget already spent for every delivery key that recurs (a
// reopened PR re-uses claw:<id>:pr:<url>), burning the notification on the
// first transient blip instead of after lifecycleMaxTransientFailures.
func (s *Server) clearLifecycleTransientFailures(notifiers map[string]bool) {
	if len(notifiers) == 0 {
		return
	}
	prefix := lifecycleTransientFailureStateKey("")
	rows, err := s.db.Query(`SELECT key FROM slack_notifier_state WHERE key LIKE ?`, prefix+"%")
	if err != nil {
		log.Printf("[notify] list transient-failure state: %v", err)
		return
	}
	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			log.Printf("[notify] list transient-failure state: %v", err)
			rows.Close()
			return
		}
		// Only the per-notifier shape is namespaced; the legacy key is
		// "transient_failures:<deliveryKey>" and belongs to no notifier.
		rest := key[len(prefix):]
		sep := strings.Index(rest, ":")
		if sep < 0 || !notifiers[rest[:sep]] {
			continue
		}
		stale = append(stale, key)
	}
	rows.Close()
	for _, key := range stale {
		s.clearNotifierState(key)
	}
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
		watermark, found, err := s.notifierStateInt64(key)
		if err != nil {
			// A read failure must not be mistaken for a first run: resetting
			// the cursor here would silently skip the pending backlog.
			log.Printf("[notify] read watermark %s: %v", key, err)
			continue
		}
		if !found {
			if key != lifecycleStateWatermarkKey {
				// A per-route cursor belongs to ensureLifecycleRouteWatermarks:
				// it is the only place that knows whether this route inherits
				// the incumbent's shared position or starts at the head. Its
				// absence here means that write did not land (a transient
				// state-read failure earlier in this same tick), so stamping the
				// head would materialise the cursor from the WRONG source and
				// bury the route's pending backlog for good — ensure sees the
				// cursor as found on every later tick and never revisits it.
				// Wait for the next tick, which retries both in order (the same
				// guard ensureLifecycleClawRouteBaselines uses).
				continue
			}
			// First run for this cursor: start at the current end of the event
			// stream so enabling it does not replay history. This is the
			// backstop for the legacy shared key, which no ensure pass writes.
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
		if !lifecycleRouteAccepts(cursor.route, ev.EventType) {
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
		priorKey := lifecycleTransientFailureStateKey(deliveryKey)
		if singleRoute {
			// Legacy single-`via` key shape: no per-notifier component, so
			// pre-routing streak state (and its corruption-recovery
			// semantics) keeps working unchanged.
			stateKey, priorKey = priorKey, stateKey
		}
		count, found, stateErr := s.notifierStateInt64(stateKey)
		if stateErr != nil {
			// Unreadable streak state: retry without counting rather than
			// risk resetting (or double-counting) the streak.
			log.Printf("[notify] read transient-failure streak for %s: %v", what, stateErr)
			return false, true
		}
		if !found {
			// Adding or removing a sibling route flips this notifier between
			// the legacy and per-notifier key shapes. Carry a live streak
			// across the move: re-keying it would hand a route that has
			// nearly exhausted its retry budget a fresh one, and the cap
			// exists to stop a poisoned event wedging the cursor forever.
			if prior, priorFound, priorErr := s.notifierStateInt64(priorKey); priorErr == nil && priorFound {
				count = prior
				s.clearNotifierState(priorKey)
			}
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

// postLifecycleEventRoute is the single-route entry point both passes use.
// Every event is delivered per route — the task-run pass behind that route's
// cursor, the claw pass behind that route's delivery rows — so no route's
// failure can be swallowed by, or hold back, another's.
func (s *Server) postLifecycleEventRoute(d lifecycleDelivery, route lifecycleRouteDelivery, msg notify.Message, runKey, deliveryKey string) error {
	_, err := s.postLifecycleRoute(d, route, msg, runKey, deliveryKey)
	return err
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
	taskRunEventStageStalled:       {":warning:", "Pipeline stage stalled", notify.SeverityWarning},
	taskRunEventDoneWithoutPR:      {":mailbox_with_no_mail:", "Agent finished without a PR", notify.SeverityWarning},
	"unknown_failure":              {":question:", "Agent failed", notify.SeverityError},
}

// lifecycleEventStyleFor resolves the style for an event. Failure events key
// on the failure type (the event type for synthetic rows); anything unmapped
// still gets a readable humanized headline, never a raw snake_case string.
func lifecycleEventStyleFor(ev lifecycleEventRow) lifecycleEventStyle {
	key := ev.EventType
	if key != taskRunEventAgentStarted && key != taskRunEventPROpened && key != taskRunEventAgentIdle && key != taskRunEventStageStalled {
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
	case taskRunEventStageStalled:
		return buildStageStalledMessage(ev, runCtx)
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

func buildStageStalledMessage(ev lifecycleEventRow, runCtx lifecycleRunContext) notify.Message {
	style := lifecycleEventStyleFor(ev)
	subject := lifecycleIssueRef(runCtx)
	if subject == "" {
		subject = "task"
	}
	stage := detailString(ev.Detail, "stage")
	if stage == "" {
		stage = "current stage"
	}
	stageAge, progressAge := stageProgressDurationLabel(ev.Detail, "stageAgeMinutes"), stageProgressDurationLabel(ev.Detail, "lastProgressMinutes")
	body := fmt.Sprintf("Stage %q has not made meaningful progress", stage)
	if progressAge != "" {
		body += " for " + progressAge
	}
	if stageAge != "" {
		body += " (stage age: " + stageAge + ")"
	}
	body += "."
	return notify.Message{Title: style.title, Emoji: style.emoji, Severity: style.severity, Subject: subject, Body: body, Fields: lifecycleMetaFields(runCtx, true, false), Summary: []string{runCtx.Repo, subject, "stage stalled: " + stage}}
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
		// Report probes a scheduled report instead of a lifecycle event. It is
		// mutually exclusive with event_type and takes the scheduled-report
		// branch below, which shares only the notifier resolution and the
		// dry-run rendering with the lifecycle path.
		Report string `json:"report"`
		// Via names which configured notifier to probe. Empty means the first
		// effective route — lifecycle.via for a legacy single-channel config.
		Via    string `json:"via"`
		DryRun bool   `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if report := strings.TrimSpace(body.Report); report != "" {
		if strings.TrimSpace(body.EventType) != "" {
			jsonError(w, http.StatusBadRequest, "report and event_type are mutually exclusive")
			return
		}
		s.handleScheduledReportTest(w, r, report, strings.TrimSpace(body.Via), body.DryRun)
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
	if err := types.ValidateLifecycleNotificationsConfig(cfg); err != nil {
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

// handleScheduledReportTest probes one scheduled report on demand: it builds
// the report through the same registry the scheduler uses, so what the
// operator previews or receives is byte-identical to what the next due slot
// would post. It never touches the scheduler's dedupe state — a test send must
// not suppress the real delivery it is testing.
//
// A report with nothing to say answers 200 with empty:true rather than an
// error: "no pull requests are waiting" is a legitimate, and common, result.
func (s *Server) handleScheduledReportTest(w http.ResponseWriter, r *http.Request, report, via string, dryRun bool) {
	builder, ok := scheduledReport(report)
	if !ok {
		jsonError(w, http.StatusBadRequest, "unknown report "+strconv.Quote(report)+"; supported reports: "+scheduledReportNamesText())
		return
	}
	cfg := s.notificationsConfig()
	if cfg == nil {
		jsonError(w, http.StatusBadRequest, "no notifiers are configured (set notifications.notifiers in hub.yaml)")
		return
	}
	// Unlike the lifecycle probe there is no default destination to fall back
	// to: a schedule names its own notifiers and nothing else in the config
	// implies one.
	if via == "" {
		jsonError(w, http.StatusBadRequest, "via is required: name the notifier to send the report through (defined: "+configuredNotifierNames(cfg.Notifiers)+")")
		return
	}
	nc, ok := cfg.Notifiers[via]
	if !ok {
		jsonError(w, http.StatusBadRequest, "via "+strconv.Quote(via)+" does not name a configured notifier (defined: "+configuredNotifierNames(cfg.Notifiers)+")")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	msg, hasReport, err := builder(ctx, s)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "build report "+strconv.Quote(report)+": "+err.Error())
		return
	}
	if !hasReport {
		jsonOK(w, map[string]any{"ok": true, "empty": true, "report": report, "via": via, "dry_run": dryRun})
		return
	}

	secrets := s.hubSecretResolver()
	if dryRun {
		// Same fallback the lifecycle dry run uses: a preview must work while
		// the token secret is still missing, and the payload never carries it.
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
		payload, err := renderNotifierPayload(n, *msg)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "render payload: "+err.Error())
			return
		}
		jsonOK(w, map[string]any{"dry_run": true, "report": report, "via": via, "payload": payload})
		return
	}

	n, err := s.notifierFor(via, nc, secrets)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "notifier "+strconv.Quote(via)+" unavailable: "+err.Error())
		return
	}
	payload, err := renderNotifierPayload(n, *msg)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "render payload: "+err.Error())
		return
	}
	handle, err := n.Send(ctx, *msg)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "notification send failed: "+err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "message_id": handle, "report": report, "via": via, "payload": payload})
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
