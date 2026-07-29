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
	agentStarted, prOpened, failures := true, true, true
	if cfg != nil && cfg.Events != nil {
		if cfg.Events.AgentStarted != nil {
			agentStarted = *cfg.Events.AgentStarted
		}
		if cfg.Events.PROpened != nil {
			prOpened = *cfg.Events.PROpened
		}
		if cfg.Events.Failures != nil {
			failures = *cfg.Events.Failures
		}
	}
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
		// Keep the cursor at the end of the stream while Slack is off, so
		// re-enabling behaves like a fresh enable instead of flushing the
		// backlog accumulated during the disabled window.
		s.parkSlackWatermark()
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

	client := s.newSlackClient(token)
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

// sendSlackEvent renders one event, posts it (threaded per run when
// configured) and records the delivery.
func (s *Server) sendSlackEvent(client *slackClient, cfg *types.SlackNotificationsConfig, ev slackEventRow) error {
	runCtx := s.slackRunContextFor(ev.RunID)
	blocks, fallback := buildSlackMessage(ev, runCtx)

	threadTS := ""
	haveRoot := false
	if slackThreadByRun(cfg) {
		var rootChannel, rootTS string
		err := s.db.QueryRow(`SELECT channel, thread_ts FROM slack_run_threads WHERE run_id=?`, ev.RunID).Scan(&rootChannel, &rootTS)
		if err == nil && rootChannel == cfg.Channel {
			threadTS = rootTS
			haveRoot = true
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ts, err := client.postMessage(ctx, cfg.Channel, threadTS, blocks, fallback)
	if err != nil {
		return err
	}
	// The first message for a run becomes the thread root — whatever event it
	// is (e.g. pr_opened when agent_started is disabled).
	if slackThreadByRun(cfg) && !haveRoot && ts != "" {
		_, dbErr := s.db.Exec(`
			INSERT INTO slack_run_threads(run_id, tenant_id, channel, thread_ts, created_at)
			VALUES(?,?,?,?,?)
			ON CONFLICT(run_id) DO UPDATE SET channel=excluded.channel, thread_ts=excluded.thread_ts`,
			ev.RunID, ev.TenantID, cfg.Channel, ts, epochMillis(now()))
		if dbErr != nil {
			log.Printf("[slack] record thread root for run %s: %v", ev.RunID, dbErr)
		}
	}
	s.recordSlackDelivery(ev.ID, ev.RunID, ts, slackDeliveryStatusSent)
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

// buildSlackMessage renders one event as Block Kit blocks plus the required
// plain-text notification fallback. It must never include tokens or secrets.
func buildSlackMessage(ev slackEventRow, runCtx slackRunContext) ([]any, string) {
	switch ev.EventType {
	case taskRunEventAgentStarted:
		return buildSlackAgentStarted(ev, runCtx)
	case taskRunEventPROpened:
		return buildSlackPROpened(ev, runCtx)
	default:
		return buildSlackFailure(ev, runCtx)
	}
}

func buildSlackAgentStarted(ev slackEventRow, runCtx slackRunContext) ([]any, string) {
	subject := slackIssueRef(runCtx)
	if subject == "" {
		subject = "task"
	}
	headline := ":hammer_and_wrench: *Agent started* — "
	if ev.TargetURL != "" {
		headline += slackLink(ev.TargetURL, subject)
	} else {
		headline += slackEscape(subject)
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
	blocks := []any{slackSectionBlock(headline)}
	if len(meta) > 0 {
		blocks = append(blocks, slackContextBlock([]string{strings.Join(meta, " · ")}))
	}
	fallback := "Agent started: " + subject
	if runCtx.Repo != "" {
		fallback += " (" + runCtx.Repo + ")"
	}
	return blocks, fallback
}

func buildSlackPROpened(ev slackEventRow, runCtx slackRunContext) ([]any, string) {
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
	headline := ":tada: *PR opened* — " + slackLink(prURL, prLabel)
	if subject := slackIssueRef(runCtx); subject != "" {
		headline += "\n" + slackEscape(subject)
	}
	var meta []string
	if owner := slackOwnerLabel(runCtx); owner != "" {
		meta = append(meta, slackEscape(owner))
	}
	if runCtx.ClawID != "" {
		meta = append(meta, "claw `"+slackEscape(shortID(runCtx.ClawID))+"`")
	}
	blocks := []any{slackSectionBlock(headline)}
	if len(meta) > 0 {
		blocks = append(blocks, slackContextBlock([]string{strings.Join(meta, " · ")}))
	}
	fallback := "PR opened: " + prLabel
	if prURL != "" {
		fallback += " " + prURL
	}
	return blocks, fallback
}

func buildSlackFailure(ev slackEventRow, runCtx slackRunContext) ([]any, string) {
	failureType := firstNonEmpty(ev.FailureType, ev.EventType)
	reason := detailString(ev.Detail, "reason", "error")
	headline := ":warning: *Run failed* — `" + slackEscape(failureType) + "`"
	if subject := slackIssueRef(runCtx); subject != "" {
		headline += "\n" + slackEscape(subject)
	}
	if reason != "" {
		headline += "\n_" + slackEscape(reason) + "_"
	}
	var meta []string
	if runCtx.Repo != "" {
		meta = append(meta, "repo `"+slackEscape(runCtx.Repo)+"`")
	}
	if owner := slackOwnerLabel(runCtx); owner != "" {
		meta = append(meta, slackEscape(owner))
	}
	if runCtx.ClawID != "" {
		meta = append(meta, "claw `"+slackEscape(shortID(runCtx.ClawID))+"`")
	}
	blocks := []any{slackSectionBlock(headline)}
	if len(meta) > 0 {
		blocks = append(blocks, slackContextBlock([]string{strings.Join(meta, " · ")}))
	}
	fallback := "Run failed (" + failureType + ")"
	if subject := slackIssueRef(runCtx); subject != "" {
		fallback += ": " + subject
	}
	return blocks, fallback
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

	ev := slackEventRow{EventType: body.EventType}
	var runCtx slackRunContext
	if body.RunID != "" {
		runCtx = s.slackRunContextFor(body.RunID)
		if runCtx.IssueID == "" && runCtx.IssueTitle == "" && runCtx.Repo == "" && runCtx.ClawID == "" {
			jsonError(w, http.StatusNotFound, "run "+strconv.Quote(body.RunID)+" not found in task_run_summaries")
			return
		}
		if body.EventType == taskRunEventPROpened {
			ev.TargetURL = runCtx.PrimaryPRURL
		}
	} else {
		runCtx, ev = sampleSlackContext(body.EventType)
	}

	blocks, fallback := buildSlackMessage(ev, runCtx)
	payload := map[string]any{
		"channel": cfg.Channel,
		"text":    fallback,
		"blocks":  blocks,
	}
	if body.DryRun {
		jsonOK(w, map[string]any{"dry_run": true, "payload": payload})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// Always post top-level: a test send must not create or reuse a run thread.
	ts, err := s.newSlackClient(token).postMessage(ctx, cfg.Channel, "", blocks, fallback)
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
