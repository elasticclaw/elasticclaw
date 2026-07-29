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
// owns event scanning, dedupe, threading bookkeeping and the semantic
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
	supported := map[string]bool{
		taskRunEventAgentStarted: true,
		taskRunEventPROpened:     true,
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
	return cfg
}

// hubSecretResolver resolves notifier secrets against the hub secrets map.
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
// The instance is cached across ticks so provider-internal pacing state (the
// Slack send limiter) is shared by the lifecycle poller and the manual test
// endpoint; the cache key covers the config and a digest of the resolved
// secrets, so editing the config or rotating a token rebuilds the notifier
// on the next use.
func (s *Server) notifierFor(name string, nc types.NotifierConfig, secrets notify.SecretResolver) (notify.Notifier, error) {
	settings := s.notifierSettings(nc)
	key := notifierCacheKey(name, types.NotifierConfig{Type: nc.Type, Settings: settings}, secrets)
	s.notifierCacheMu.Lock()
	defer s.notifierCacheMu.Unlock()
	if s.notifierCache != nil && s.notifierCacheKey == key {
		return s.notifierCache, nil
	}
	n, err := notify.New(nc.Type, settings, secrets)
	if err != nil {
		return nil, err
	}
	s.notifierCache = n
	s.notifierCacheKey = key
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

// notifierDestination is the notifier's default destination setting, used
// only as thread-root bookkeeping: a thread root recorded for one
// destination must not be reused after the operator points the notifier at
// another. Providers without a channel setting return "" and always match.
func notifierDestination(nc types.NotifierConfig) string {
	dest, _ := nc.Settings["channel"].(string)
	return dest
}

// enabledLifecycleEventTypes maps the config toggles onto concrete event
// types. All categories default to enabled when the toggles block is absent.
func enabledLifecycleEventTypes(lc *types.LifecycleNotificationsConfig) map[string]bool {
	agentStarted, prOpened, failures := lifecycleClawKindsEnabled(lc)
	enabled := map[string]bool{}
	if agentStarted {
		enabled[taskRunEventAgentStarted] = true
	}
	if prOpened {
		enabled[taskRunEventPROpened] = true
	}
	if failures {
		for t := range lifecycleFailureEventTypes {
			enabled[t] = true
		}
	}
	return enabled
}

func lifecycleThreadByRun(lc *types.LifecycleNotificationsConfig) bool {
	return lc == nil || lc.ThreadByRun == nil || *lc.ThreadByRun
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

// startLifecycleNotifier launches the background loop that turns lifecycle
// events into notifications. The goroutine idles cheaply while notifications
// are disabled and picks the config up again on the next tick.
func (s *Server) startLifecycleNotifier() {
	s.safeGo("lifecycle notifier", func() {
		for {
			time.Sleep(s.lifecyclePollInterval())
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
// message: the notifier, the lifecycle config, and the thread-root
// destination bookkeeping value.
type lifecycleDelivery struct {
	notifier    notify.Notifier
	lc          *types.LifecycleNotificationsConfig
	destination string
}

func (s *Server) lifecycleNotifierTick() {
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
	nc := cfg.Notifiers[lc.Via] // exists: validated above
	notifier, err := s.notifierFor(lc.Via, nc, s.hubSecretResolver())
	if err != nil {
		// Unknown provider type, unresolvable secret, bad provider settings:
		// pause (leaving all cursors) until the operator fixes the config.
		s.logPollWarningOnce("notify-notifier", "[notify] notifier %q unavailable — notifications paused: %v", lc.Via, err)
		return
	}
	s.clearPollWarning("notify-config")
	s.clearPollWarning("notify-notifier")

	d := lifecycleDelivery{notifier: notifier, lc: lc, destination: notifierDestination(nc)}
	// Two independent event sources share the notifier, dedupe table and
	// thread table: task-run events for claws that belong to a task run, and
	// the claw pass for ad-hoc claws (task_run_id=''). See
	// lifecycle_claw_notifier.go for the exclusivity rule that prevents
	// double notifications.
	s.lifecycleTaskRunPass(d)
	s.lifecycleClawPass(d)
}

// lifecycleTaskRunPass scans task_run_events behind the rowid watermark and
// delivers the enabled event types.
func (s *Server) lifecycleTaskRunPass(d lifecycleDelivery) {
	watermark, found, err := s.notifierStateInt64(lifecycleStateWatermarkKey)
	if err != nil {
		// A read failure must not be mistaken for a first run: resetting the
		// cursor here would silently skip the pending backlog.
		log.Printf("[notify] read watermark: %v", err)
		return
	}
	if !found {
		// First run: start at the current end of the event stream so enabling
		// the feature does not replay history.
		maxRow, err := s.lifecycleMaxEventRowID()
		if err != nil {
			log.Printf("[notify] read max event rowid: %v", err)
			return
		}
		s.setNotifierStateInt64(lifecycleStateWatermarkKey, maxRow)
		return
	}

	enabled := enabledLifecycleEventTypes(d.lc)
	if len(enabled) == 0 {
		// Every event category is toggled off: keep the cursor moving so
		// re-enabling a category does not flush the skipped backlog.
		s.parkLifecycleWatermark()
		return
	}
	events, err := s.selectLifecycleCandidateEvents(watermark, enabled)
	if err != nil {
		log.Printf("[notify] select candidate events: %v", err)
		return
	}
	if len(events) == 0 {
		return
	}

	maxHandled := watermark
	for _, ev := range events {
		if err := s.sendLifecycleEvent(d, ev); err != nil {
			if handled, stop := s.handleLifecycleSendError(err, "event "+ev.ID+" ("+ev.EventType+")", ev.ID, ev.RunID); handled {
				maxHandled = ev.RowID
				continue
			} else if stop {
				break
			}
			break
		}
		s.clearPollWarning(lifecycleSendWarningKey)
		maxHandled = ev.RowID
	}
	if maxHandled > watermark {
		s.setNotifierStateInt64(lifecycleStateWatermarkKey, maxHandled)
	}
}

// handleLifecycleSendError applies the shared send-failure policy: config
// errors pause everything (log-once), permanent errors are recorded as
// failed under deliveryKey (and count as handled), transient errors stop the
// pass for this tick. Returns handled=true when the caller may move past the
// event; stop is informational for symmetry.
func (s *Server) handleLifecycleSendError(err error, what, deliveryKey, runKey string) (handled, stop bool) {
	switch notify.Classify(err) {
	case notify.ErrorConfig:
		// Bad token / missing channel fails every message, not this one.
		// Pause (leave the cursors) instead of burning events as failed, so
		// delivery resumes once the config is fixed.
		s.logPollWarningOnce(lifecycleSendWarningKey, "[notify] delivery paused until the notifier config is fixed: %v", err)
		return false, true
	case notify.ErrorPermanent:
		// Never succeeds on retry — record it so we stop trying.
		log.Printf("[notify] permanent failure for %s: %v", what, err)
		s.recordNotificationDelivery(deliveryKey, runKey, "", notificationDeliveryStatusFailed)
		return true, false
	default:
		// Transient: leave the event (and the cursor) for the next tick.
		log.Printf("[notify] transient failure for %s, will retry: %v", what, err)
		return false, true
	}
}

// parkLifecycleWatermark advances the cursor to the end of the event stream
// without sending anything. Used while notifications (or every event
// category) are disabled so a later re-enable does not replay the disabled
// window.
func (s *Server) parkLifecycleWatermark() {
	watermark, found, err := s.notifierStateInt64(lifecycleStateWatermarkKey)
	if err != nil || !found {
		// Never enabled (or state unreadable): nothing to park.
		return
	}
	maxRow, err := s.lifecycleMaxEventRowID()
	if err != nil {
		log.Printf("[notify] park watermark: %v", err)
		return
	}
	if maxRow > watermark {
		s.setNotifierStateInt64(lifecycleStateWatermarkKey, maxRow)
	}
}

func (s *Server) lifecycleMaxEventRowID() (int64, error) {
	var maxRow int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM task_run_events`).Scan(&maxRow)
	return maxRow, err
}

// sendLifecycleEvent renders one task-run event, posts it (threaded per run
// when configured) and records the delivery.
func (s *Server) sendLifecycleEvent(d lifecycleDelivery, ev lifecycleEventRow) error {
	runCtx := s.lifecycleRunContextFor(ev.RunID)
	return s.postLifecycleEvent(d, ev, runCtx, ev.RunID, ev.ID, ev.TenantID)
}

// postLifecycleEvent renders one event, posts it (threaded per threadKey
// when configured) and records the delivery under deliveryKey. Task-run
// events use (run_id, event id); claw-pass events use namespaced synthetic
// keys ("claw:<id>", "claw:<id>:<kind>") so the two sources share the thread
// and dedupe tables without ever colliding.
//
// Threading is provider-specific and the Notifier interface has no thread
// concept: the thread root travels in Options["thread_ts"], which providers
// without threading ignore, and the handle Send returns records the root. A
// provider that returns no handle simply never threads.
func (s *Server) postLifecycleEvent(d lifecycleDelivery, ev lifecycleEventRow, runCtx lifecycleRunContext, threadKey, deliveryKey, tenantID string) error {
	msg := buildLifecycleMessage(ev, runCtx)

	threadTS := ""
	haveRoot := false
	if lifecycleThreadByRun(d.lc) {
		var rootDest, rootTS string
		err := s.db.QueryRow(`SELECT channel, thread_ts FROM slack_run_threads WHERE run_id=?`, threadKey).Scan(&rootDest, &rootTS)
		if err == nil && rootDest == d.destination {
			threadTS = rootTS
			haveRoot = true
		}
	}
	if threadTS != "" {
		msg.Options = map[string]any{"thread_ts": threadTS}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handle, err := d.notifier.Send(ctx, msg)
	if err != nil {
		return err
	}
	// The first message for a thread key becomes the thread root — whatever
	// event it is (e.g. pr_opened when agent_started is disabled).
	if lifecycleThreadByRun(d.lc) && !haveRoot && handle != "" {
		_, dbErr := s.db.Exec(`
			INSERT INTO slack_run_threads(run_id, tenant_id, channel, thread_ts, created_at)
			VALUES(?,?,?,?,?)
			ON CONFLICT(run_id) DO UPDATE SET channel=excluded.channel, thread_ts=excluded.thread_ts`,
			threadKey, tenantID, d.destination, handle, epochMillis(now()))
		if dbErr != nil {
			log.Printf("[notify] record thread root for %s: %v", threadKey, dbErr)
		}
	}
	s.recordNotificationDelivery(deliveryKey, threadKey, handle, notificationDeliveryStatusSent)
	return nil
}

func (s *Server) recordNotificationDelivery(eventID, runID, messageHandle, status string) {
	if _, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
		VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`,
		eventID, runID, epochMillis(now()), messageHandle, status); err != nil {
		log.Printf("[notify] record delivery for event %s: %v", eventID, err)
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
	taskRunEventDoneWithoutPR:      {":mailbox_with_no_mail:", "Agent finished without a PR", notify.SeverityWarning},
	"unknown_failure":              {":question:", "Agent failed", notify.SeverityError},
}

// lifecycleEventStyleFor resolves the style for an event. Failure events key
// on the failure type (the event type for synthetic rows); anything unmapped
// still gets a readable humanized headline, never a raw snake_case string.
func lifecycleEventStyleFor(ev lifecycleEventRow) lifecycleEventStyle {
	key := ev.EventType
	if key != taskRunEventAgentStarted && key != taskRunEventPROpened {
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
		DryRun    bool   `json:"dry_run"`
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
	via := cfg.Lifecycle.Via
	nc := cfg.Notifiers[via] // exists: validated above

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
		// notifier instance is deliberately not cached.
		dryResolver := func(name string) (string, bool) {
			if v, ok := secrets(name); ok && v != "" {
				return v, true
			}
			return "dry-run-placeholder", true
		}
		n, err := notify.New(nc.Type, nc.Settings, dryResolver)
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
		// Surface the provider error verbatim so scope/channel problems are
		// debuggable. Errors never contain the token.
		jsonError(w, http.StatusBadGateway, "notification send failed: "+err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "message_id": handle, "via": via, "payload": payload})
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
	default:
		ev.FailureType = eventType
		ev.Detail = map[string]any{"reason": "synthetic sample failure (test send)"}
	}
	return runCtx, ev
}
