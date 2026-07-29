package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSlack captures chat.postMessage requests and lets tests script responses
// per call.
type fakeSlack struct {
	mu       sync.Mutex
	requests []fakeSlackRequest
	// respond decides the response for the nth request (1-based). Nil means
	// success with an incrementing ts.
	respond func(n int, w http.ResponseWriter)
	server  *httptest.Server
}

type fakeSlackRequest struct {
	Auth     string
	Channel  string
	Text     string
	ThreadTS string
	// Fallback, Color and Blocks come from the message's single colour-striped
	// attachment. For attachment-only messages the fallback — not top-level
	// text — is what drives push notifications and accessibility.
	Fallback string
	Color    string
	Blocks   []map[string]any
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" {
			t.Errorf("unexpected Slack path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body struct {
			Channel     string `json:"channel"`
			Text        string `json:"text"`
			ThreadTS    string `json:"thread_ts"`
			Attachments []struct {
				Fallback string           `json:"fallback"`
				Color    string           `json:"color"`
				Blocks   []map[string]any `json:"blocks"`
			} `json:"attachments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Slack request: %v", err)
		}
		req := fakeSlackRequest{
			Auth:     r.Header.Get("Authorization"),
			Channel:  body.Channel,
			Text:     body.Text,
			ThreadTS: body.ThreadTS,
		}
		if len(body.Attachments) > 1 {
			t.Errorf("message carries %d attachments, want at most 1", len(body.Attachments))
		}
		if len(body.Attachments) == 1 {
			req.Fallback = body.Attachments[0].Fallback
			req.Color = body.Attachments[0].Color
			req.Blocks = body.Attachments[0].Blocks
		}
		if len(body.Attachments) > 0 && body.Text != "" {
			// With an attachment present, top-level text renders as a visible
			// body line above it — the headline would appear twice.
			t.Errorf("attachment message carries top-level text %q, want empty", body.Text)
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		n := len(f.requests)
		respond := f.respond
		f.mu.Unlock()
		if respond != nil {
			respond(n, w)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"ts":"1000.%06d"}`, n)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSlack) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeSlack) request(i int) fakeSlackRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func (f *fakeSlack) setRespond(fn func(n int, w http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = fn
}

func testSecrets(name string) (string, bool) {
	if name == "slack_bot_token" {
		return "xoxb-test-token", true
	}
	return "", false
}

// newTestSlack builds the provider through the registry, so the tests exercise
// the same construction path the hub uses.
func newTestSlack(t *testing.T, baseURL string, extra map[string]any) Notifier {
	t.Helper()
	cfg := map[string]any{
		"token_secret":      "slack_bot_token",
		"channel":           "C123",
		"api_base":          baseURL,
		"min_send_interval": "1ns",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	n, err := New("slack", cfg, testSecrets)
	if err != nil {
		t.Fatalf("New(slack) error = %v", err)
	}
	return n
}

func TestSlackSendSuccess(t *testing.T) {
	fake := newFakeSlack(t)
	n := newTestSlack(t, fake.server.URL, nil)

	handle, err := n.Send(context.Background(), Message{
		Title:    "PR opened",
		Emoji:    ":tada:",
		Severity: SeveritySuccess,
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if handle == "" {
		t.Fatal("Send() returned an empty handle; threading depends on it")
	}
	req := fake.request(0)
	if req.Auth != "Bearer xoxb-test-token" {
		t.Fatalf("Authorization header = %q", req.Auth)
	}
	if req.Channel != "C123" {
		t.Fatalf("channel = %q, want the notifier's configured channel", req.Channel)
	}
	if req.Fallback == "" {
		t.Fatal("attachment fallback is empty; it is all the user sees in a push notification")
	}
	if req.Text != "" {
		t.Fatalf("top-level text = %q, want empty so the attachment is not preceded by a duplicate body line", req.Text)
	}
	if req.Color == "" {
		t.Fatal("attachment colour is empty; Severity must map to a stripe colour")
	}
	if len(req.Blocks) == 0 {
		t.Fatal("no blocks sent")
	}
	if req.ThreadTS != "" {
		t.Fatalf("thread_ts should be absent, got %q", req.ThreadTS)
	}
}

// A Message with only Text (the pipeline-action shape) must still send.
func TestSlackSendTextOnlyMessage(t *testing.T) {
	fake := newFakeSlack(t)
	n := newTestSlack(t, fake.server.URL, nil)

	if _, err := n.Send(context.Background(), Message{Text: "deploy finished"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	req := fake.request(0)
	if req.Fallback == "" && req.Text == "" {
		t.Fatal("text-only message carried neither fallback nor text")
	}
}

// Target overrides the notifier's default destination.
func TestSlackSendTargetOverridesChannel(t *testing.T) {
	fake := newFakeSlack(t)
	n := newTestSlack(t, fake.server.URL, nil)

	if _, err := n.Send(context.Background(), Message{Text: "hi", Target: "C999"}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := fake.request(0).Channel; got != "C999" {
		t.Fatalf("channel = %q, want the message target C999", got)
	}
}

// Threading rides through Options; a provider without threads can ignore it.
func TestSlackSendThreadsViaOptions(t *testing.T) {
	fake := newFakeSlack(t)
	n := newTestSlack(t, fake.server.URL, nil)

	_, err := n.Send(context.Background(), Message{
		Text:    "reply",
		Options: map[string]any{"thread_ts": "1717.42"},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := fake.request(0).ThreadTS; got != "1717.42" {
		t.Fatalf("thread_ts = %q, want 1717.42", got)
	}
}

func TestSlackOKFalseReturnsTypedError(t *testing.T) {
	fake := newFakeSlack(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		// Slack signals logical failures with HTTP 200 + ok:false.
		fmt.Fprint(w, `{"ok":false,"error":"channel_not_found"}`)
	})
	n := newTestSlack(t, fake.server.URL, nil)

	_, err := n.Send(context.Background(), Message{Text: "hi"})
	if err == nil {
		t.Fatal("Send() succeeded, want error")
	}
	var apiErr *slackAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != "channel_not_found" {
		t.Fatalf("error = %v, want slackAPIError channel_not_found", err)
	}
	if fake.count() != 1 {
		t.Fatalf("non-retryable ok:false error was retried: %d requests", fake.count())
	}
}

func TestSlackRetriesOn429(t *testing.T) {
	fake := newFakeSlack(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"ok":true,"ts":"1717.42"}`)
	})
	n := newTestSlack(t, fake.server.URL, nil)

	handle, err := n.Send(context.Background(), Message{Text: "hi"})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if handle != "1717.42" {
		t.Fatalf("handle = %q", handle)
	}
	if fake.count() != 2 {
		t.Fatalf("expected 2 requests (429 then success), got %d", fake.count())
	}
}

func TestSlack429GivesUpAfterMaxAttempts(t *testing.T) {
	fake := newFakeSlack(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	n := newTestSlack(t, fake.server.URL, nil)

	_, err := n.Send(context.Background(), Message{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error = %v, want rate limited", err)
	}
	if fake.count() != slackMaxAttempts {
		t.Fatalf("expected %d attempts, got %d", slackMaxAttempts, fake.count())
	}
}

// The classification drives the caller's retry policy: config errors pause
// delivery entirely, permanent errors burn the single event, transient errors
// are retried. Misclassifying a config error as permanent would silently drop
// every event while the token is bad.
func TestSlackErrorClassification(t *testing.T) {
	config := []string{"invalid_auth", "account_inactive", "token_revoked", "channel_not_found", "not_in_channel", "is_archived"}
	for _, code := range config {
		if got := Classify(&slackAPIError{Code: code}); got != ErrorConfig {
			t.Errorf("Classify(%s) = %v, want ErrorConfig", code, got)
		}
	}
	permanent := []string{"msg_too_long", "invalid_blocks", "invalid_blocks_format"}
	for _, code := range permanent {
		if got := Classify(&slackAPIError{Code: code}); got != ErrorPermanent {
			t.Errorf("Classify(%s) = %v, want ErrorPermanent", code, got)
		}
	}
	transient := []error{
		&slackAPIError{Code: "ratelimited"},
		&slackAPIError{Code: "internal_error"},
		errors.New("connection refused"),
		fmt.Errorf("slack HTTP 500: oops"),
	}
	for _, err := range transient {
		if got := Classify(err); got != ErrorTransient {
			t.Errorf("Classify(%v) = %v, want ErrorTransient", err, got)
		}
	}
}

// Regression: the send limiter must honor the caller's context. Before the fix
// a Send with an already-expired context still slept for the full queued
// pacing interval before doing anything.
func TestSlackCancelledContextAbortsBeforeQueueWait(t *testing.T) {
	fake := newFakeSlack(t)
	n := newTestSlack(t, fake.server.URL, map[string]any{"min_send_interval": "5s"})
	// A previous send just happened, so the next slot is 5s away.
	if _, err := n.Send(context.Background(), Message{Text: "first"}); err != nil {
		t.Fatalf("priming send error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := n.Send(ctx, Message{Text: "second"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled context still waited %v for the queue slot", elapsed)
	}
	if fake.count() != 1 {
		t.Fatalf("cancelled context still hit Slack: %d requests total, want just the priming one", fake.count())
	}
}

// A long Body must not push the message past Slack's 3000-char block limit,
// which Slack rejects outright as invalid_blocks.
func TestSlackClampsLongBody(t *testing.T) {
	fake := newFakeSlack(t)
	n := newTestSlack(t, fake.server.URL, nil)

	if _, err := n.Send(context.Background(), Message{
		Title: "Agent crashed during startup",
		Body:  strings.Repeat("x", 8000),
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	for i, block := range fake.request(0).Blocks {
		text, _ := block["text"].(map[string]any)
		if text == nil {
			continue
		}
		if s, _ := text["text"].(string); len([]rune(s)) > 3000 {
			t.Fatalf("block %d carries %d runes, want <= 3000", i, len([]rune(s)))
		}
	}
}

// Channel shape is a provider concern: a "#name" breaks when the channel is
// renamed, so construction must reject it rather than failing at send time.
func TestSlackConstructorValidatesConfig(t *testing.T) {
	tests := []struct {
		name   string
		cfg    map[string]any
		errMsg string
	}{
		{
			name:   "missing token_secret",
			cfg:    map[string]any{"channel": "C123"},
			errMsg: "token_secret",
		},
		{
			name:   "secret that does not resolve",
			cfg:    map[string]any{"token_secret": "nope", "channel": "C123"},
			errMsg: "not found",
		},
		{
			name:   "channel name instead of id",
			cfg:    map[string]any{"token_secret": "slack_bot_token", "channel": "#deploys"},
			errMsg: "use the channel ID",
		},
		{
			name:   "channel that is not an id",
			cfg:    map[string]any{"token_secret": "slack_bot_token", "channel": "deploys"},
			errMsg: "does not look like a Slack channel ID",
		},
		{
			name:   "bad min_send_interval",
			cfg:    map[string]any{"token_secret": "slack_bot_token", "channel": "C123", "min_send_interval": "soon"},
			errMsg: "min_send_interval",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New("slack", tt.cfg, testSecrets)
			if err == nil {
				t.Fatal("New(slack) succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.errMsg)
			}
		})
	}
	// Private groups and DMs are valid destinations too.
	for _, channel := range []string{"C0123ABCD", "G0123ABCD", "D0123ABCD"} {
		if _, err := New("slack", map[string]any{"token_secret": "slack_bot_token", "channel": channel}, testSecrets); err != nil {
			t.Errorf("channel %s rejected: %v", channel, err)
		}
	}
}

func TestRegistry(t *testing.T) {
	if !Supported("slack") {
		t.Fatal("slack should be a supported notifier type")
	}
	if Supported("carrier-pigeon") {
		t.Fatal("unknown type reported as supported")
	}
	if _, err := New("carrier-pigeon", nil, testSecrets); err == nil {
		t.Fatal("New() with an unknown type succeeded")
	} else if !strings.Contains(err.Error(), "slack") {
		t.Fatalf("error %q should list the supported types", err.Error())
	}
	// The doctor validates secret settings off this metadata without
	// constructing the notifier, so a new provider inherits doctor coverage.
	if got := SecretSettings("slack"); len(got) != 1 || got[0] != "token_secret" {
		t.Fatalf("SecretSettings(slack) = %v, want [token_secret]", got)
	}
	if got := SecretSettings("carrier-pigeon"); got != nil {
		t.Fatalf("SecretSettings(unknown) = %v, want nil", got)
	}
}

// dry_run returns RenderPayload's output, so it must be the same builder Send
// posts — otherwise a dry run stops predicting what really gets sent.
func TestSlackRenderPayloadMatchesSend(t *testing.T) {
	fake := newFakeSlack(t)
	n := newTestSlack(t, fake.server.URL, nil)
	renderer, ok := n.(PayloadRenderer)
	if !ok {
		t.Fatal("slack notifier must implement PayloadRenderer for dry runs")
	}
	msg := Message{Title: "Agent started", Emoji: ":rocket:", Severity: SeverityInfo}

	payload, err := renderer.RenderPayload(msg)
	if err != nil {
		t.Fatalf("RenderPayload() error = %v", err)
	}
	if _, err := n.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	rendered, err := json.Marshal(payload["attachments"])
	if err != nil {
		t.Fatalf("marshal rendered attachments: %v", err)
	}
	req := fake.request(0)
	sent, err := json.Marshal([]any{map[string]any{
		"fallback": req.Fallback,
		"color":    req.Color,
		"blocks":   req.Blocks,
	}})
	if err != nil {
		t.Fatalf("marshal sent attachments: %v", err)
	}
	if string(rendered) != string(sent) {
		t.Fatalf("dry-run payload differs from what Send posted:\n dry run: %s\n sent:    %s", rendered, sent)
	}
}
