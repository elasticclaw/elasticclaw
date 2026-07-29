package hub

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

// fakeSlackServer captures chat.postMessage requests and lets tests script
// responses per call.
type fakeSlackServer struct {
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
	// Fallback, Color and Blocks come from the message's single
	// colour-striped attachment (the only place blocks are sent). For
	// attachment-only messages the fallback — not top-level text — is what
	// drives push notifications and accessibility.
	Fallback string
	Color    string
	Blocks   []map[string]any
}

func newFakeSlackServer(t *testing.T) *fakeSlackServer {
	t.Helper()
	f := &fakeSlackServer{}
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

func (f *fakeSlackServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeSlackServer) request(i int) fakeSlackRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func (f *fakeSlackServer) setRespond(fn func(n int, w http.ResponseWriter)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = fn
}

func newTestSlackClient(baseURL string) *slackClient {
	return &slackClient{
		token:           "xoxb-test-token",
		baseURL:         baseURL,
		minSendInterval: time.Nanosecond,
	}
}

func TestSlackClientPostMessageSuccess(t *testing.T) {
	fake := newFakeSlackServer(t)
	client := newTestSlackClient(fake.server.URL)

	msg := slackMessage{
		fallback: "fallback text",
		color:    "#2EB67D",
		blocks:   []any{map[string]any{"type": "section"}},
	}
	ts, err := client.postMessage(context.Background(), "C123", "", msg)
	if err != nil {
		t.Fatalf("postMessage() error = %v", err)
	}
	if ts == "" {
		t.Fatal("postMessage() returned empty ts")
	}
	req := fake.request(0)
	if req.Auth != "Bearer xoxb-test-token" {
		t.Fatalf("Authorization header = %q", req.Auth)
	}
	if req.Channel != "C123" || req.Fallback != "fallback text" || len(req.Blocks) != 1 {
		t.Fatalf("unexpected request payload: %#v", req)
	}
	if req.Text != "" {
		t.Fatalf("top-level text = %q, want empty so the attachment is not preceded by a duplicate body line", req.Text)
	}
	if req.Color != "#2EB67D" {
		t.Fatalf("attachment color = %q, want the stripe colour passed in", req.Color)
	}
	if req.ThreadTS != "" {
		t.Fatalf("thread_ts should be absent, got %q", req.ThreadTS)
	}
}

func TestSlackClientOKFalseReturnsTypedError(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		// Slack signals logical failures with HTTP 200 + ok:false.
		fmt.Fprint(w, `{"ok":false,"error":"channel_not_found"}`)
	})
	client := newTestSlackClient(fake.server.URL)

	_, err := client.postMessage(context.Background(), "C123", "", slackMessage{fallback: "fallback"})
	if err == nil {
		t.Fatal("postMessage() succeeded, want error")
	}
	var apiErr *slackAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != "channel_not_found" {
		t.Fatalf("error = %v, want slackAPIError channel_not_found", err)
	}
	if fake.count() != 1 {
		t.Fatalf("permanent ok:false error was retried: %d requests", fake.count())
	}
}

func TestSlackClientRetriesOn429(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"ok":true,"ts":"1717.42"}`)
	})
	client := newTestSlackClient(fake.server.URL)

	ts, err := client.postMessage(context.Background(), "C123", "", slackMessage{fallback: "fallback"})
	if err != nil {
		t.Fatalf("postMessage() error = %v", err)
	}
	if ts != "1717.42" {
		t.Fatalf("ts = %q", ts)
	}
	if fake.count() != 2 {
		t.Fatalf("expected 2 requests (429 then success), got %d", fake.count())
	}
}

func TestSlackClient429GivesUpAfterMaxAttempts(t *testing.T) {
	fake := newFakeSlackServer(t)
	fake.setRespond(func(n int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	client := newTestSlackClient(fake.server.URL)

	_, err := client.postMessage(context.Background(), "C123", "", slackMessage{fallback: "fallback"})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error = %v, want rate limited", err)
	}
	if fake.count() != slackMaxAttempts {
		t.Fatalf("expected %d attempts, got %d", slackMaxAttempts, fake.count())
	}
}

func TestSlackErrorClassification(t *testing.T) {
	// Configuration-level failures affect every message: the notifier pauses
	// on them instead of recording individual events as failed.
	config := []string{"invalid_auth", "account_inactive", "token_revoked", "channel_not_found", "not_in_channel", "is_archived"}
	for _, code := range config {
		err := &slackAPIError{Code: code}
		if !isSlackConfigError(err) {
			t.Errorf("%s should be a config error", code)
		}
		if isPermanentSlackError(err) {
			t.Errorf("%s must not be message-permanent — that would burn the event", code)
		}
	}
	// Message-level failures never succeed on retry for that payload.
	permanent := []string{"msg_too_long", "invalid_blocks", "invalid_blocks_format"}
	for _, code := range permanent {
		err := &slackAPIError{Code: code}
		if !isPermanentSlackError(err) {
			t.Errorf("%s should be permanent", code)
		}
		if isSlackConfigError(err) {
			t.Errorf("%s should not be a config error", code)
		}
	}
	transient := []error{
		&slackAPIError{Code: "ratelimited"},
		&slackAPIError{Code: "internal_error"},
		errors.New("connection refused"),
		fmt.Errorf("slack HTTP 500: oops"),
	}
	for _, err := range transient {
		if isPermanentSlackError(err) || isSlackConfigError(err) {
			t.Errorf("%v should be transient", err)
		}
	}
}

// Regression: the send limiter must honor the caller's context. Before the
// fix, a postMessage with an already-expired context still slept for the full
// queued pacing interval before doing anything.
func TestSlackClientCancelledContextAbortsBeforeQueueWait(t *testing.T) {
	fake := newFakeSlackServer(t)
	client := newTestSlackClient(fake.server.URL)
	// A previous send just happened, so the next slot is 5s away.
	client.limiter = &slackSendLimiter{lastSend: time.Now()}
	client.minSendInterval = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := client.postMessage(ctx, "C123", "", slackMessage{fallback: "fallback"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled context still waited %v for the queue slot", elapsed)
	}
	if fake.count() != 0 {
		t.Fatalf("cancelled context still hit Slack %d times", fake.count())
	}
}
