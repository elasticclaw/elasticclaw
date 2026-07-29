package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultSlackAPIBase = "https://slack.com/api"
	// slackMaxAttempts caps 429 retries within a single Send call.
	slackMaxAttempts = 4
	// slackMinSendInterval paces chat.postMessage: Slack allows roughly one
	// message per second per channel.
	slackMinSendInterval = time.Second
	// slackMaxBlockTextLength is Slack's limit for the text object inside a
	// section or context block. Exceeding it fails the whole message with
	// invalid_blocks/msg_too_long, so every block clamps its text: failure
	// reasons in Message.Body can be thousands of characters.
	slackMaxBlockTextLength = 3000
)

// Severity → attachment stripe colour. The palette is deliberately tiny —
// four colours, so a channel full of notifications reads as a system, not a
// rainbow. Exported so integration tests can assert the mapping; the hex
// values themselves must never be used outside this provider.
const (
	SlackColorInfo    = "#36C5F0" // blue — work started
	SlackColorSuccess = "#2EB67D" // green — a PR exists
	SlackColorError   = "#E01E5A" // red — hard failures (someone must look)
	SlackColorWarning = "#ECB22E" // amber — soft/ambiguous outcomes
)

var slackSeverityColors = map[Severity]string{
	SeverityInfo:    SlackColorInfo,
	SeveritySuccess: SlackColorSuccess,
	SeverityWarning: SlackColorWarning,
	SeverityError:   SlackColorError,
}

// slackChannelIDRegex matches Slack conversation IDs: public channels (C...),
// private groups (G...) and DMs (D...).
var slackChannelIDRegex = regexp.MustCompile(`^[CGD][A-Za-z0-9]+$`)

// slackAPIError is a logical Slack API failure (HTTP 200 with ok:false).
// Code carries the Slack error string, e.g. "channel_not_found".
type slackAPIError struct {
	Code string
}

func (e *slackAPIError) Error() string {
	return "slack API error: " + e.Code
}

// NotifyErrorClass classifies Slack API error codes for the caller's retry
// policy (see ErrorClass). Config-level codes (bad token, missing channel)
// fail every message alike; message-level codes (oversized payload) never
// succeed on retry; everything else is transient.
func (e *slackAPIError) NotifyErrorClass() ErrorClass {
	switch e.Code {
	case "invalid_auth", "account_inactive", "token_revoked",
		"channel_not_found", "not_in_channel", "is_archived":
		return ErrorConfig
	case "msg_too_long", "invalid_blocks", "invalid_blocks_format":
		return ErrorPermanent
	}
	return ErrorTransient
}

// slackConfigError is a pre-flight configuration failure detected before any
// HTTP call (e.g. no destination). Classified ErrorConfig so pollers pause
// instead of retrying forever.
type slackConfigError struct {
	msg string
}

func (e *slackConfigError) Error() string                { return e.msg }
func (e *slackConfigError) NotifyErrorClass() ErrorClass { return ErrorConfig }

// slackSendLimiter serializes sends and enforces a minimum interval between
// them so bursts of parallel callers cannot hammer chat.postMessage.
type slackSendLimiter struct {
	mu       sync.Mutex
	lastSend time.Time
}

// slackLimiters holds one send limiter per destination (api_base + channel),
// process-wide. Slack's chat.postMessage limit is per channel, so pacing must
// be keyed on the channel actually posted to — not on the notifier instance:
// instances are rebuilt on config edits and token rotations, two named
// notifiers can point at the same channel, and independent features (the
// lifecycle poller, the manual test endpoint, a future pipeline notify
// action) may each construct their own instance. The map is bounded by the
// number of distinct destinations the process ever posts to; entries are a
// few dozen bytes and are deliberately never evicted, so pacing state
// survives reconstruction. Keying includes api_base so tests pointing at
// separate fake servers never pace each other.
var (
	slackLimitersMu sync.Mutex
	slackLimiters   = map[string]*slackSendLimiter{}
)

func slackLimiterFor(apiBase, channel string) *slackSendLimiter {
	key := apiBase + "\x00" + channel
	slackLimitersMu.Lock()
	defer slackLimitersMu.Unlock()
	l, ok := slackLimiters[key]
	if !ok {
		l = &slackSendLimiter{}
		slackLimiters[key] = l
	}
	return l
}

// wait blocks until at least minInterval has passed since the previous send,
// then claims the send slot. Sends are serialized by the mutex. A cancelled
// or expired context aborts the wait immediately (releasing the mutex) so
// callers with a deadline are not pinned behind a long send queue.
func (l *slackSendLimiter) wait(ctx context.Context, minInterval time.Duration) error {
	if l == nil || minInterval <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if wait := minInterval - time.Since(l.lastSend); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	l.lastSend = time.Now()
	return nil
}

// slackNotifier posts to one Slack channel via chat.postMessage. Send pacing
// lives in the process-wide per-channel slackLimiters registry, not in the
// instance, so rebuilding a notifier never resets it.
type slackNotifier struct {
	token           string
	channel         string
	apiBase         string
	client          *http.Client
	minSendInterval time.Duration
}

func newSlack(cfg map[string]any, secrets SecretResolver) (Notifier, error) {
	secretName := stringOption(cfg, "token_secret")
	if secretName == "" {
		return nil, fmt.Errorf("slack notifier requires token_secret (the name of a hub secret holding the bot token)")
	}
	token, ok := secrets(secretName)
	if !ok || token == "" {
		return nil, fmt.Errorf("slack notifier secret %q not found in hub secrets", secretName)
	}
	channel := strings.TrimSpace(stringOption(cfg, "channel"))
	if strings.HasPrefix(channel, "#") {
		return nil, fmt.Errorf("slack notifier channel %q is a channel name; use the channel ID instead (e.g. C0123ABCD) — names break when the channel is renamed", channel)
	}
	if channel != "" && !slackChannelIDRegex.MatchString(channel) {
		return nil, fmt.Errorf("slack notifier channel %q does not look like a Slack channel ID (expected C/G/D followed by alphanumerics, e.g. C0123ABCD)", channel)
	}
	apiBase := stringOption(cfg, "api_base")
	if apiBase == "" {
		apiBase = defaultSlackAPIBase
	}
	interval := slackMinSendInterval
	if raw := stringOption(cfg, "min_send_interval"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("slack notifier min_send_interval %q: %w", raw, err)
		}
		interval = d
	}
	return &slackNotifier{
		token:           token,
		channel:         channel,
		apiBase:         apiBase,
		client:          &http.Client{Timeout: 15 * time.Second},
		minSendInterval: interval,
	}, nil
}

// Destination names the configured channel for the caller's bookkeeping
// (see notify.DestinationReporter).
func (s *slackNotifier) Destination() string { return s.channel }

// destinationFor resolves the channel a message will post to.
func (s *slackNotifier) destinationFor(msg Message) string {
	if msg.Target != "" {
		return msg.Target
	}
	return s.channel
}

// RenderPayload builds the exact chat.postMessage body for a Message. It is
// the single source of truth for the wire shape: Send and the dry-run test
// endpoint both use it, so a dry run always returns precisely what a real
// send would post.
func (s *slackNotifier) RenderPayload(msg Message) (map[string]any, error) {
	channel := s.destinationFor(msg)
	if channel == "" {
		return nil, &slackConfigError{"slack notifier has no channel configured and the message sets no target"}
	}

	// Provider passthrough first; the core fields computed below always win.
	payload := make(map[string]any, len(msg.Options)+3)
	for k, v := range msg.Options {
		payload[k] = v
	}
	payload["channel"] = channel
	if msg.Thread != "" {
		payload["thread_ts"] = msg.Thread
	}

	if !msg.Semantic() {
		// Plain tier: Text only (the pipeline notify action path). Text is
		// plain text, never raw mrkdwn: it is template-rendered from untrusted
		// data (issue titles, command stdout), so the mrkdwn control
		// characters are escaped like every other rendering path — otherwise a
		// crafted issue title could smuggle a <!channel> broadcast or a
		// spoofed <url|label> link into the channel. Formatting that survives
		// escaping (*bold*, _italic_) still renders.
		payload["text"] = slackEscape(msg.Text)
		return payload, nil
	}

	// Semantic tier: Block Kit blocks wrapped in a single colour-striped
	// attachment. Blocks live inside the attachment (not top-level) so the
	// message carries the coloured left border. For an attachment-only
	// message the top-level "text" is NOT a hidden fallback — Slack renders
	// it as a visible line above the attachment, duplicating the headline —
	// so the notification/accessibility summary goes in the attachment's
	// "fallback" field instead, which Slack uses for push notifications when
	// no top-level text is present.
	delete(payload, "text")
	blocks := slackRenderBlocks(msg)
	if len(blocks) == 0 {
		// Degenerate semantic message with nothing renderable (e.g. only a
		// Severity set): fall back to the plain tier rather than posting an
		// attachment Slack will reject as empty.
		payload["text"] = slackEscape(msg.Text)
		return payload, nil
	}
	attachment := map[string]any{
		"fallback": slackFallbackText(msg),
		"blocks":   blocks,
	}
	if color, ok := slackSeverityColors[msg.Severity]; ok {
		attachment["color"] = color
	}
	payload["attachments"] = []any{attachment}
	return payload, nil
}

// Send posts the message and returns the Slack message ts as the handle.
// Slack signals logical errors as HTTP 200 with ok:false, which surfaces as
// *slackAPIError (classified via NotifyErrorClass). HTTP 429 is retried
// honoring Retry-After, up to slackMaxAttempts total attempts; other HTTP
// failures return immediately as transient errors for the caller to retry.
func (s *slackNotifier) Send(ctx context.Context, msg Message) (string, error) {
	payload, err := s.RenderPayload(msg)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal slack message: %w", err)
	}

	// Pacing is keyed on the channel actually posted to: Slack rate-limits
	// chat.postMessage per channel, and the limiter registry is process-wide
	// so every instance targeting the same channel shares one pace.
	limiter := slackLimiterFor(s.apiBase, s.destinationFor(msg))
	for attempt := 1; ; attempt++ {
		if err := limiter.wait(ctx, s.minSendInterval); err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.apiBase+"/chat.postMessage", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+s.token)

		resp, err := s.client.Do(req)
		if err != nil {
			return "", err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			if attempt >= slackMaxAttempts {
				return "", fmt.Errorf("slack rate limited after %d attempts", attempt)
			}
			delay := slackRetryAfter(resp.Header.Get("Retry-After"), attempt)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return "", fmt.Errorf("slack HTTP %d: %s", resp.StatusCode, truncateForLog(string(respBody), 300))
		}

		var result struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			TS    string `json:"ts"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return "", fmt.Errorf("parse slack response: %w", err)
		}
		if !result.OK {
			return "", &slackAPIError{Code: result.Error}
		}
		return result.TS, nil
	}
}

// slackRetryAfter parses a Retry-After header (seconds); when absent or
// malformed it falls back to a small exponential backoff.
func slackRetryAfter(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return time.Duration(attempt) * time.Second
}

// ── Semantic rendering ────────────────────────────────────────────────────────

// slackRenderBlocks lays out the semantic fields as Block Kit blocks. The
// layout is shared by every message — headline, then subject/link, then the
// long body as a blockquote, then dim metadata — so a channel of
// notifications reads consistently. A message without a Title (a plain-text
// body that still set severity, a link or fields) leads with its Text
// unbolded, so no populated field is ever dropped.
func slackRenderBlocks(msg Message) []any {
	var lines []string
	switch {
	case msg.Title != "":
		headline := "*" + msg.Title + "*"
		if msg.Emoji != "" {
			headline = msg.Emoji + " " + headline
		}
		lines = append(lines, headline)
	case msg.Text != "":
		line := slackEscape(msg.Text)
		if msg.Emoji != "" {
			line = msg.Emoji + " " + line
		}
		lines = append(lines, line)
	}

	subjectConsumed := false
	switch {
	case msg.Link.URL != "":
		label := msg.Link.Label
		if label == "" {
			label = msg.Subject
			subjectConsumed = label != ""
		}
		if label == "" {
			label = msg.Link.URL
		}
		lines = append(lines, slackMrkdwnLink(msg.Link.URL, label))
	case msg.Link.Label != "":
		// A link with no URL still names its subject (e.g. a PR label whose
		// URL is unknown); render the label as plain text.
		lines = append(lines, slackEscape(msg.Link.Label))
	}
	if msg.Subject != "" && !subjectConsumed {
		lines = append(lines, slackEscape(msg.Subject))
	}

	var blocks []any
	if len(lines) > 0 {
		blocks = append(blocks, slackSectionBlock(strings.Join(lines, "\n")))
	}
	// The body gets its own section so long diagnostics never truncate the
	// headline or subject, and each block clamps independently.
	if msg.Body != "" {
		blocks = append(blocks, slackSectionBlock(slackBlockquote(msg.Body)))
	}
	if parts := slackRenderFields(msg.Fields); parts != "" {
		blocks = append(blocks, slackContextBlock(parts))
	}
	return blocks
}

// slackRenderFields joins metadata fields into one dim context line:
// labelled machine values as "label `value`", label-less identifiers as
// "`value`", and plain values escaped as-is, separated by middle dots.
func slackRenderFields(fields []Field) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Value == "" {
			continue
		}
		value := slackEscape(f.Value)
		if f.Code {
			value = "`" + value + "`"
		}
		if f.Label != "" {
			value = slackEscape(f.Label) + " " + value
		}
		parts = append(parts, value)
	}
	return strings.Join(parts, " · ")
}

// slackFallbackText builds the plain-text notification line — on a phone
// lock screen it is 100% of what the user sees. The emoji leads (shortcodes
// render in Slack push notifications) so the discriminator survives
// truncation, and every message keeps the same slot order:
//
//	<emoji> <title> — <summary parts joined by middle dots>
//
// with empty parts dropped, so a slot never silently changes meaning. iOS
// truncates at roughly two lines, so callers must front-load what matters in
// Message.Summary and keep parts short.
func slackFallbackText(msg Message) string {
	out := msg.Title
	if out == "" {
		// Title-less rich messages (plain text plus semantic decorations)
		// lead with their text, mirroring the block layout.
		out = msg.Text
	}
	if msg.Emoji != "" {
		out = msg.Emoji + " " + out
	}
	var kept []string
	for _, p := range msg.Summary {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) > 0 {
		out += " — " + strings.Join(kept, " · ")
	}
	return out
}

// ── mrkdwn helpers ────────────────────────────────────────────────────────────

// slackEscape escapes the three characters Slack mrkdwn treats specially.
func slackEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func slackMrkdwnLink(url, label string) string {
	if url == "" {
		return slackEscape(label)
	}
	if label == "" {
		label = url
	}
	return "<" + url + "|" + slackEscape(label) + ">"
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

func slackClampBlockText(s string) string {
	const marker = "… [truncated]"
	if utf8.RuneCountInString(s) <= slackMaxBlockTextLength {
		return s
	}
	return truncateRunes(s, slackMaxBlockTextLength-utf8.RuneCountInString(marker)) + marker
}

func slackSectionBlock(mrkdwn string) map[string]any {
	return map[string]any{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": slackClampBlockText(mrkdwn)},
	}
}

func slackContextBlock(mrkdwn string) map[string]any {
	return map[string]any{
		"type":     "context",
		"elements": []any{map[string]any{"type": "mrkdwn", "text": slackClampBlockText(mrkdwn)}},
	}
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
