package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	slackDefaultPollInterval = 5 * time.Second
	slackBatchSize           = 200

	// slackStateWatermarkKey stores the task_run_events rowid the notifier has
	// processed up to. The cursor is insertion-ordered on purpose: observed_at
	// carries authoritative provider timestamps (a pr_opened picked up by the
	// 24h catch-up poller can be hours in the past), so a timestamp watermark
	// would silently skip backdated rows.
	slackStateWatermarkKey = "watermark_rowid"

	// slackSendWarningKey keys the log-once warning for configuration-level
	// Slack send failures (bad token, missing channel).
	slackSendWarningKey = "slack-send"

	slackDeliveryStatusSent   = "sent"
	slackDeliveryStatusFailed = "failed"
)

// slackFailureEventTypes are the failure-shaped task run events we notify on.
var slackFailureEventTypes = map[string]bool{
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

// slackSupportedEventTypes is everything the notifier can render.
func slackSupportedEventTypes() map[string]bool {
	supported := map[string]bool{
		taskRunEventAgentStarted: true,
		taskRunEventPROpened:     true,
	}
	for t := range slackFailureEventTypes {
		supported[t] = true
	}
	return supported
}

// slackNotificationsConfig returns a copy of the current Slack config plus the
// resolved bot token. Reading it fresh each tick means enabling/disabling
// Slack does not require a hub restart.
func (s *Server) slackNotificationsConfig() (*types.SlackNotificationsConfig, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hubCfg == nil || s.hubCfg.Notifications == nil || s.hubCfg.Notifications.Slack == nil {
		return nil, ""
	}
	cfg := *s.hubCfg.Notifications.Slack
	token := ""
	if cfg.BotTokenRef != "" {
		token = s.hubCfg.Secrets[cfg.BotTokenRef]
	}
	return &cfg, token
}

// enabledSlackEventTypes maps the config toggles onto concrete event types.
// All categories default to enabled when the toggles block is absent.
func enabledSlackEventTypes(cfg *types.SlackNotificationsConfig) map[string]bool {
	agentStarted, prOpened, failures := slackClawKindsEnabled(cfg)
	enabled := map[string]bool{}
	if agentStarted {
		enabled[taskRunEventAgentStarted] = true
	}
	if prOpened {
		enabled[taskRunEventPROpened] = true
	}
	if failures {
		for t := range slackFailureEventTypes {
			enabled[t] = true
		}
	}
	return enabled
}

func slackThreadByRun(cfg *types.SlackNotificationsConfig) bool {
	return cfg == nil || cfg.ThreadByRun == nil || *cfg.ThreadByRun
}

func (s *Server) slackPollInterval() time.Duration {
	cfg, _ := s.slackNotificationsConfig()
	if cfg != nil && cfg.PollInterval != "" {
		if d, err := time.ParseDuration(cfg.PollInterval); err == nil && d >= time.Second {
			return d
		}
	}
	return slackDefaultPollInterval
}

// newSlackClient builds a client sharing the server-wide send limiter so the
// poller and the manual test endpoint cannot exceed Slack's per-channel pace.
func (s *Server) newSlackClient(token string) *slackClient {
	return &slackClient{
		token:           token,
		baseURL:         s.slackBaseURL,
		limiter:         &s.slackLimiter,
		minSendInterval: s.slackSendInterval,
	}
}

// startSlackNotifier launches the background loop that turns task run events
// into Slack messages. The goroutine idles cheaply while Slack is disabled and
// picks the config up again on the next tick when it is enabled.
func (s *Server) startSlackNotifier() {
	s.safeGo("slack notifier", func() {
		for {
			time.Sleep(s.slackPollInterval())
			// Run the tick inline (not via safeGo) so ticks never overlap:
			// the read-then-insert delivery dedupe is not safe under
			// concurrent ticks. A panic is contained to this iteration.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[slack] panic in notifier tick: %v\n%s", r, debug.Stack())
					}
				}()
				s.slackNotifierTick()
			}()
		}
	})
}

type slackEventRow struct {
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

// slackRunContext is the task_run_summaries context used to enrich messages.
type slackRunContext struct {
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

func (s *Server) slackNotifierTick() {
	cfg, token := s.slackNotificationsConfig()
	if cfg == nil || !cfg.Enabled {
		// Keep the cursors at the end of their streams while Slack is off, so
		// re-enabling behaves like a fresh enable instead of flushing the
		// backlog accumulated during the disabled window.
		s.parkSlackWatermark()
		s.parkSlackClawState()
		return
	}
	if err := types.ValidateSlackNotificationsConfig(cfg); err != nil {
		s.logPollWarningOnce("slack-config", "[slack] invalid config — notifications paused: %v", err)
		return
	}
	if token == "" {
		s.logPollWarningOnce("slack-token", "[slack] bot_token_ref %q not found in hub secrets — notifications paused", cfg.BotTokenRef)
		return
	}
	s.clearPollWarning("slack-config")
	s.clearPollWarning("slack-token")

	client := s.newSlackClient(token)
	// Two independent event sources share the client, dedupe table and thread
	// table: task-run events for claws that belong to a task run, and the claw
	// pass for ad-hoc claws (task_run_id=''). See slack_claw_notifier.go for
	// the exclusivity rule that prevents double notifications.
	s.slackTaskRunPass(client, cfg)
	s.slackClawPass(client, cfg)
}

// slackTaskRunPass scans task_run_events behind the rowid watermark and
// delivers the enabled event types.
func (s *Server) slackTaskRunPass(client *slackClient, cfg *types.SlackNotificationsConfig) {
	watermark, found, err := s.slackStateInt64(slackStateWatermarkKey)
	if err != nil {
		// A read failure must not be mistaken for a first run: resetting the
		// cursor here would silently skip the pending backlog.
		log.Printf("[slack] read watermark: %v", err)
		return
	}
	if !found {
		// First run: start at the current end of the event stream so enabling
		// the feature does not replay history.
		maxRow, err := s.slackMaxEventRowID()
		if err != nil {
			log.Printf("[slack] read max event rowid: %v", err)
			return
		}
		s.setSlackStateInt64(slackStateWatermarkKey, maxRow)
		return
	}

	enabled := enabledSlackEventTypes(cfg)
	if len(enabled) == 0 {
		// Every event category is toggled off: keep the cursor moving so
		// re-enabling a category does not flush the skipped backlog.
		s.parkSlackWatermark()
		return
	}
	events, err := s.selectSlackCandidateEvents(watermark, enabled)
	if err != nil {
		log.Printf("[slack] select candidate events: %v", err)
		return
	}
	if len(events) == 0 {
		return
	}

	maxHandled := watermark
	for _, ev := range events {
		if err := s.sendSlackEvent(client, cfg, ev); err != nil {
			if isSlackConfigError(err) {
				// Bad token / missing channel fails every message, not this
				// one. Pause (leave the watermark) instead of burning events
				// as failed, so delivery resumes once the config is fixed.
				s.logPollWarningOnce(slackSendWarningKey, "[slack] delivery paused until the Slack config is fixed: %v", err)
				break
			}
			if isPermanentSlackError(err) {
				// Never succeeds on retry — record it so we stop trying.
				log.Printf("[slack] permanent failure for event %s (%s): %v", ev.ID, ev.EventType, err)
				s.recordSlackDelivery(ev.ID, ev.RunID, "", slackDeliveryStatusFailed)
				maxHandled = ev.RowID
				continue
			}
			// Transient: leave the event (and the watermark) for the next tick.
			log.Printf("[slack] transient failure for event %s (%s), will retry: %v", ev.ID, ev.EventType, err)
			break
		}
		s.clearPollWarning(slackSendWarningKey)
		maxHandled = ev.RowID
	}
	if maxHandled > watermark {
		s.setSlackStateInt64(slackStateWatermarkKey, maxHandled)
	}
}

// parkSlackWatermark advances the cursor to the end of the event stream
// without sending anything. Used while Slack (or every event category) is
// disabled so a later re-enable does not replay the disabled window.
func (s *Server) parkSlackWatermark() {
	watermark, found, err := s.slackStateInt64(slackStateWatermarkKey)
	if err != nil || !found {
		// Never enabled (or state unreadable): nothing to park.
		return
	}
	maxRow, err := s.slackMaxEventRowID()
	if err != nil {
		log.Printf("[slack] park watermark: %v", err)
		return
	}
	if maxRow > watermark {
		s.setSlackStateInt64(slackStateWatermarkKey, maxRow)
	}
}

func (s *Server) slackMaxEventRowID() (int64, error) {
	var maxRow int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM task_run_events`).Scan(&maxRow)
	return maxRow, err
}

// sendSlackEvent renders one task-run event, posts it (threaded per run when
// configured) and records the delivery.
func (s *Server) sendSlackEvent(client *slackClient, cfg *types.SlackNotificationsConfig, ev slackEventRow) error {
	runCtx := s.slackRunContextFor(ev.RunID)
	return s.postSlackEvent(client, cfg, ev, runCtx, ev.RunID, ev.ID, ev.TenantID)
}

// postSlackEvent renders one event, posts it (threaded per threadKey when
// configured) and records the delivery under deliveryKey. Task-run events use
// (run_id, event id); claw-pass events use namespaced synthetic keys
// ("claw:<id>", "claw:<id>:<kind>") so the two sources share the thread and
// dedupe tables without ever colliding.
func (s *Server) postSlackEvent(client *slackClient, cfg *types.SlackNotificationsConfig, ev slackEventRow, runCtx slackRunContext, threadKey, deliveryKey, tenantID string) error {
	msg := buildSlackMessage(ev, runCtx)

	threadTS := ""
	haveRoot := false
	if slackThreadByRun(cfg) {
		var rootChannel, rootTS string
		err := s.db.QueryRow(`SELECT channel, thread_ts FROM slack_run_threads WHERE run_id=?`, threadKey).Scan(&rootChannel, &rootTS)
		if err == nil && rootChannel == cfg.Channel {
			threadTS = rootTS
			haveRoot = true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts, err := client.postMessage(ctx, cfg.Channel, threadTS, msg)
	if err != nil {
		return err
	}
	// The first message for a thread key becomes the thread root — whatever
	// event it is (e.g. pr_opened when agent_started is disabled).
	if slackThreadByRun(cfg) && !haveRoot && ts != "" {
		_, dbErr := s.db.Exec(`
			INSERT INTO slack_run_threads(run_id, tenant_id, channel, thread_ts, created_at)
			VALUES(?,?,?,?,?)
			ON CONFLICT(run_id) DO UPDATE SET channel=excluded.channel, thread_ts=excluded.thread_ts`,
			threadKey, tenantID, cfg.Channel, ts, epochMillis(now()))
		if dbErr != nil {
			log.Printf("[slack] record thread root for %s: %v", threadKey, dbErr)
		}
	}
	s.recordSlackDelivery(deliveryKey, threadKey, ts, slackDeliveryStatusSent)
	return nil
}

func (s *Server) recordSlackDelivery(eventID, runID, messageTS, status string) {
	if _, err := s.db.Exec(`
		INSERT INTO slack_notification_deliveries(event_id, run_id, delivered_at, message_ts, status)
		VALUES(?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`,
		eventID, runID, epochMillis(now()), messageTS, status); err != nil {
		log.Printf("[slack] record delivery for event %s: %v", eventID, err)
	}
}

// selectSlackCandidateEvents returns undelivered events after the rowid
// cursor, in insertion order. The dedupe happens in SQL on purpose: filtering
// delivered rows in Go would let them consume the LIMIT, and a burst larger
// than the batch size would then re-select the same delivered rows forever
// and permanently wedge the cursor.
func (s *Server) selectSlackCandidateEvents(afterRowID int64, enabled map[string]bool) ([]slackEventRow, error) {
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
		 LIMIT `+strconv.Itoa(slackBatchSize), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []slackEventRow
	for rows.Next() {
		var ev slackEventRow
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

func (s *Server) slackRunContextFor(runID string) slackRunContext {
	runCtx := slackRunContext{RunID: runID}
	err := s.db.QueryRow(`
		SELECT issue_id, issue_title, repo, primary_pr_url, workflow_name,
		       factory_name, claw_id, owner_display_name, model
		  FROM task_run_summaries WHERE run_id=?`, runID).Scan(
		&runCtx.IssueID, &runCtx.IssueTitle, &runCtx.Repo, &runCtx.PrimaryPRURL,
		&runCtx.WorkflowName, &runCtx.FactoryName, &runCtx.ClawID,
		&runCtx.OwnerDisplayName, &runCtx.Model)
	if err != nil && err != sql.ErrNoRows {
		log.Printf("[slack] load run context for %s: %v", runID, err)
	}
	return runCtx
}

// slackStateInt64 reads one notifier state value. found is false only when
// the row genuinely does not exist; any other failure (a locked or closed DB,
// a corrupted value) is returned as an error so callers never mistake a
// transient read failure for a first run and reset the cursor.
func (s *Server) slackStateInt64(key string) (value int64, found bool, err error) {
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

func (s *Server) setSlackStateInt64(key string, value int64) {
	if _, err := s.db.Exec(`
		INSERT INTO slack_notifier_state(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		key, strconv.FormatInt(value, 10)); err != nil {
		log.Printf("[slack] persist state %s: %v", key, err)
	}
}

// ── Message rendering ─────────────────────────────────────────────────────────

// slackEscape escapes the three characters Slack mrkdwn treats specially.
func slackEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func slackLink(url, label string) string {
	if url == "" {
		return slackEscape(label)
	}
	if label == "" {
		label = url
	}
	return "<" + url + "|" + slackEscape(label) + ">"
}

// slackIssueRef renders "ISSUE-1 — Title" (whichever parts exist).
func slackIssueRef(runCtx slackRunContext) string {
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

func slackOwnerLabel(runCtx slackRunContext) string {
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

// slackMaxBlockTextLength is Slack's limit for the text object inside a
// section or context block. Exceeding it fails the whole message with
// invalid_blocks/msg_too_long, so every block clamps its text: failure
// reasons from event detail can be up to 6000 chars (failureSummaryInputLimit).
const slackMaxBlockTextLength = 3000

func slackClampBlockText(s string) string {
	const marker = "… [truncated]"
	if runeLen(s) <= slackMaxBlockTextLength {
		return s
	}
	return truncateRunes(s, slackMaxBlockTextLength-runeLen(marker)) + marker
}

func slackSectionBlock(mrkdwn string) map[string]any {
	return map[string]any{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": slackClampBlockText(mrkdwn)},
	}
}

func slackContextBlock(parts []string) map[string]any {
	elements := make([]any, 0, len(parts))
	for _, p := range parts {
		elements = append(elements, map[string]any{"type": "mrkdwn", "text": slackClampBlockText(p)})
	}
	return map[string]any{"type": "context", "elements": elements}
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
// Every event type gets its own (emoji, title, colour) so a channel full of
// notifications is scannable at a glance. The palette is deliberately tiny —
// four colours, so the channel reads as a system, not a rainbow:
//
//	blue   work started
//	green  a PR exists
//	red    hard failures (someone must look)
//	amber  soft/ambiguous outcomes (ended, but maybe fine)
const (
	slackColorStarted = "#36C5F0" // blue
	slackColorSuccess = "#2EB67D" // green
	slackColorFailure = "#E01E5A" // red
	slackColorWarning = "#ECB22E" // amber
)

// slackEventStyle drives one event type's rendering: the headline is always
// "<emoji> *<title>*" and the attachment stripe uses the colour.
type slackEventStyle struct {
	emoji string // Slack emoji shortcode, e.g. ":rocket:"
	title string // human headline — never a raw snake_case identifier
	color string // attachment stripe
}

// slackEventStyles maps every supported event/failure type to its look.
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
var slackEventStyles = map[string]slackEventStyle{
	taskRunEventAgentStarted:       {":rocket:", "Agent started", slackColorStarted},
	taskRunEventPROpened:           {":tada:", "PR opened", slackColorSuccess},
	taskRunEventAgentStopped:       {":skull:", "Agent died", slackColorFailure},
	"creation_failed":              {":no_entry_sign:", "Couldn't create the agent", slackColorFailure},
	taskRunFailureProvisionFailed:  {":construction:", "Couldn't get a machine", slackColorFailure},
	taskRunFailureBootstrapFailed:  {":boom:", "Agent crashed during startup", slackColorFailure},
	taskRunFailurePermissionOrAuth: {":lock:", "Agent was denied access", slackColorFailure},
	taskRunFailureProviderLost:     {":satellite_antenna:", "Lost contact with the provider", slackColorFailure},
	taskRunFailureTimeout:          {":hourglass_flowing_sand:", "Agent ran out of time", slackColorWarning},
	taskRunEventDoneWithoutPR:      {":mailbox_with_no_mail:", "Agent finished without a PR", slackColorWarning},
	"unknown_failure":              {":question:", "Agent failed", slackColorFailure},
}

// slackEventStyleFor resolves the style for an event. Failure events key on
// the failure type (the event type for synthetic rows); anything unmapped
// still gets a readable humanized headline, never a raw snake_case string.
func slackEventStyleFor(ev slackEventRow) slackEventStyle {
	key := ev.EventType
	if key != taskRunEventAgentStarted && key != taskRunEventPROpened {
		key = firstNonEmpty(ev.FailureType, ev.EventType)
	}
	if key == taskRunFailureUnknown {
		key = "unknown_failure"
	}
	if style, ok := slackEventStyles[key]; ok {
		return style
	}
	return slackEventStyle{":question:", slackHumanizeType(key), slackColorFailure}
}

// slackHumanizeType turns an unmapped snake_case type into a headline-ready
// label ("manual_stop_before_delivery" → "Manual stop before delivery").
func slackHumanizeType(t string) string {
	words := strings.ReplaceAll(t, "_", " ")
	if words == "" {
		return "Agent failed"
	}
	r := []rune(words)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// slackBlockquote renders text as a mrkdwn blockquote (every line prefixed),
// used for failure reasons so error text reads as quoted output rather than
// competing with the headline.
func slackBlockquote(s string) string {
	lines := strings.Split(slackEscape(s), "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}

// slackHeadline renders the standard first line: "<emoji> *<title>*".
func slackHeadline(style slackEventStyle) string {
	return style.emoji + " *" + style.title + "*"
}

// slackFallbackText builds the plain-text notification line — on a phone lock
// screen it is 100% of what the user sees. The emoji leads (shortcodes render
// in Slack push notifications) so the discriminator survives truncation, and
// every message shape keeps the same slot order:
//
//	<emoji> <title> — <where> · <what> · <why>
//
// with empty parts dropped, so a slot never silently changes meaning between
// shapes. iOS truncates at roughly two lines, so callers must front-load what
// matters and keep parts short.
func slackFallbackText(style slackEventStyle, parts ...string) string {
	out := style.emoji + " " + style.title
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) > 0 {
		out += " — " + strings.Join(kept, " · ")
	}
	return out
}

// slackFallbackReason compresses a failure reason onto one short line for the
// notification text: the reason is the part that tells the reader whether to
// get out of bed, but diagnostics can be 6000 chars of multi-line output.
func slackFallbackReason(reason string) string {
	const maxRunes = 140
	reason = strings.Join(strings.Fields(reason), " ")
	if runeLen(reason) > maxRunes {
		return truncateRunes(reason, maxRunes-1) + "…"
	}
	return reason
}

// buildSlackMessage renders one event as a colour-striped Block Kit message
// plus the required plain-text notification fallback. Layout is shared by all
// event types — headline, then subject, then dim metadata — so the channel
// reads consistently. It must never include tokens or secrets.
func buildSlackMessage(ev slackEventRow, runCtx slackRunContext) slackMessage {
	switch ev.EventType {
	case taskRunEventAgentStarted:
		return buildSlackAgentStarted(ev, runCtx)
	case taskRunEventPROpened:
		return buildSlackPROpened(ev, runCtx)
	default:
		return buildSlackFailure(ev, runCtx)
	}
}

func buildSlackAgentStarted(ev slackEventRow, runCtx slackRunContext) slackMessage {
	style := slackEventStyleFor(ev)
	subject := slackIssueRef(runCtx)
	if subject == "" {
		subject = "task"
	}
	body := slackHeadline(style) + "\n"
	if ev.TargetURL != "" {
		body += slackLink(ev.TargetURL, subject)
	} else {
		body += slackEscape(subject)
	}
	var meta []string
	if runCtx.Repo != "" {
		meta = append(meta, "repo `"+slackEscape(runCtx.Repo)+"`")
	}
	if owner := slackOwnerLabel(runCtx); owner != "" {
		meta = append(meta, slackEscape(owner))
	}
	if runCtx.Model != "" {
		meta = append(meta, "model `"+slackEscape(runCtx.Model)+"`")
	}
	if runCtx.ClawID != "" {
		meta = append(meta, "claw `"+slackEscape(shortID(runCtx.ClawID))+"`")
	}
	blocks := []any{slackSectionBlock(body)}
	if len(meta) > 0 {
		blocks = append(blocks, slackContextBlock([]string{strings.Join(meta, " · ")}))
	}
	fallback := slackFallbackText(style, runCtx.Repo, subject)
	return slackMessage{fallback: fallback, color: style.color, blocks: blocks}
}

func buildSlackPROpened(ev slackEventRow, runCtx slackRunContext) slackMessage {
	style := slackEventStyleFor(ev)
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
	body := slackHeadline(style) + "\n" + slackLink(prURL, prLabel)
	if subject := slackIssueRef(runCtx); subject != "" {
		body += "\n" + slackEscape(subject)
	}
	var meta []string
	if owner := slackOwnerLabel(runCtx); owner != "" {
		meta = append(meta, slackEscape(owner))
	}
	if runCtx.ClawID != "" {
		meta = append(meta, "claw `"+slackEscape(shortID(runCtx.ClawID))+"`")
	}
	blocks := []any{slackSectionBlock(body)}
	if len(meta) > 0 {
		blocks = append(blocks, slackContextBlock([]string{strings.Join(meta, " · ")}))
	}
	// The PR label (repo#number) fills the "where" slot: it already names the
	// repo, so a separate repo part would only repeat it.
	fallback := slackFallbackText(style, prLabel, slackIssueRef(runCtx))
	return slackMessage{fallback: fallback, color: style.color, blocks: blocks}
}

func buildSlackFailure(ev slackEventRow, runCtx slackRunContext) slackMessage {
	style := slackEventStyleFor(ev)
	failureType := firstNonEmpty(ev.FailureType, ev.EventType)
	reason := detailString(ev.Detail, "reason", "error")
	body := slackHeadline(style)
	if subject := slackIssueRef(runCtx); subject != "" {
		body += "\n" + slackEscape(subject)
	}
	blocks := []any{slackSectionBlock(body)}
	// The reason gets its own section so long diagnostics never truncate the
	// headline or subject, and each block clamps independently.
	if reason != "" {
		blocks = append(blocks, slackSectionBlock(slackBlockquote(reason)))
	}
	// The raw type stays available as dim metadata for operators who grep.
	meta := []string{"`" + slackEscape(failureType) + "`"}
	if runCtx.Repo != "" {
		meta = append(meta, "repo `"+slackEscape(runCtx.Repo)+"`")
	}
	if owner := slackOwnerLabel(runCtx); owner != "" {
		meta = append(meta, slackEscape(owner))
	}
	if runCtx.ClawID != "" {
		meta = append(meta, "claw `"+slackEscape(shortID(runCtx.ClawID))+"`")
	}
	blocks = append(blocks, slackContextBlock([]string{strings.Join(meta, " · ")}))
	// Failure notifications keep the issue reference short (id when there is
	// one) so the reason — the part that says how bad it is — fits before
	// the lock screen truncates. The raw failure type is deliberately absent
	// here: it duplicates the human title in machine form.
	fallback := slackFallbackText(style,
		runCtx.Repo,
		firstNonEmpty(runCtx.IssueID, runCtx.IssueTitle),
		slackFallbackReason(reason))
	return slackMessage{fallback: fallback, color: style.color, blocks: blocks}
}

// ── Manual trigger endpoint ───────────────────────────────────────────────────

// handleSlackNotificationTest lets an admin probe the Slack integration:
// dry_run returns the rendered Block Kit payload without calling Slack; a real
// run posts to the configured channel. It never writes delivery or thread
// state — it is a side-effect-free probe apart from the Slack message itself.
func (s *Server) handleSlackNotificationTest(w http.ResponseWriter, r *http.Request) {
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
	supported := slackSupportedEventTypes()
	if !supported[body.EventType] {
		valid := make([]string, 0, len(supported))
		for t := range supported {
			valid = append(valid, t)
		}
		sort.Strings(valid)
		jsonError(w, http.StatusBadRequest, "invalid event_type "+strconv.Quote(body.EventType)+"; valid values: "+strings.Join(valid, ", "))
		return
	}
	cfg, token := s.slackNotificationsConfig()
	if cfg == nil || !cfg.Enabled {
		jsonError(w, http.StatusBadRequest, "Slack notifications are not configured or not enabled (set notifications.slack in hub.yaml)")
		return
	}
	if err := types.ValidateSlackNotificationsConfig(cfg); err != nil {
		jsonError(w, http.StatusBadRequest, "Slack config invalid: "+err.Error())
		return
	}
	if token == "" && !body.DryRun {
		jsonError(w, http.StatusBadRequest, "Slack bot token secret "+strconv.Quote(cfg.BotTokenRef)+" not found in hub secrets")
		return
	}

	if body.RunID != "" && body.ClawID != "" {
		jsonError(w, http.StatusBadRequest, "run_id and claw_id are mutually exclusive")
		return
	}

	ev := slackEventRow{EventType: body.EventType}
	var runCtx slackRunContext
	switch {
	case body.RunID != "":
		runCtx = s.slackRunContextFor(body.RunID)
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
		claw, ok, err := s.slackClawByID(body.ClawID)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "load claw: "+err.Error())
			return
		}
		if !ok {
			jsonError(w, http.StatusNotFound, "claw "+strconv.Quote(body.ClawID)+" not found")
			return
		}
		runCtx = slackClawRunContext(claw)
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
		runCtx, ev = sampleSlackContext(body.EventType)
	}

	msg := buildSlackMessage(ev, runCtx)
	// The payload comes from the same builder postMessage uses, so dry_run
	// returns exactly what a real send would post (attachment wrapper included).
	payload := msg.payload(cfg.Channel, "")
	if body.DryRun {
		jsonOK(w, map[string]any{"dry_run": true, "payload": payload})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Always post top-level: a test send must not create or reuse a run thread.
	ts, err := s.newSlackClient(token).postMessage(ctx, cfg.Channel, "", msg)
	if err != nil {
		// Surface the Slack error verbatim so scope/channel problems are
		// debuggable. Errors never contain the token.
		jsonError(w, http.StatusBadGateway, "Slack post failed: "+err.Error())
		return
	}
	jsonOK(w, map[string]any{"ok": true, "ts": ts, "payload": payload})
}

// sampleSlackContext builds clearly-marked synthetic data for test sends
// without a run_id.
func sampleSlackContext(eventType string) (slackRunContext, slackEventRow) {
	runCtx := slackRunContext{
		RunID:      "sample-run",
		IssueID:    "SAMPLE-123",
		IssueTitle: "Sample issue for Slack notification test",
		Repo:       "example/repo",
		ClawID:     "sample-claw",
		Model:      "sample/model",
	}
	ev := slackEventRow{EventType: eventType}
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
