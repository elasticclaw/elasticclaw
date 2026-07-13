package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func noSecrets(name string) (string, bool) { return "", false }

func secretsWith(m map[string]string) SecretResolver {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestNewUnknownTypeReturnsError(t *testing.T) {
	_, err := New("carrier-pigeon", map[string]any{}, noSecrets)
	if err == nil {
		t.Fatal("expected error for unknown notifier type")
	}
	if !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Fatalf("error should name the unknown type, got: %v", err)
	}
}

func TestNewSlackMissingTokenSecret(t *testing.T) {
	_, err := New("slack", map[string]any{
		"token_secret": "slack-bot-token",
		"channel":      "#eng",
	}, noSecrets)
	if err == nil {
		t.Fatal("expected error when token secret is not resolvable")
	}
	if !strings.Contains(err.Error(), "slack-bot-token") {
		t.Fatalf("error should name the missing secret, got: %v", err)
	}
}

func TestNewSlackRequiresTokenSecretKey(t *testing.T) {
	_, err := New("slack", map[string]any{"channel": "#eng"}, noSecrets)
	if err == nil {
		t.Fatal("expected error when token_secret is not configured")
	}
	if !strings.Contains(err.Error(), "token_secret") {
		t.Fatalf("error should name the missing key, got: %v", err)
	}
}

type slackCall struct {
	authorization string
	body          map[string]any
}

func newSlackTestServer(t *testing.T, calls *[]slackCall, responses []func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.postMessage" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		*calls = append(*calls, slackCall{
			authorization: r.Header.Get("Authorization"),
			body:          body,
		})
		idx := len(*calls) - 1
		if idx >= len(responses) {
			idx = len(responses) - 1
		}
		responses[idx](w)
	}))
}

func okResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func newSlackNotifier(t *testing.T, serverURL string, cfg map[string]any) Notifier {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["token_secret"] = "slack-bot-token"
	cfg["api_base"] = serverURL
	n, err := New("slack", cfg, secretsWith(map[string]string{"slack-bot-token": "xoxb-test-token"}))
	if err != nil {
		t.Fatalf("New(slack): %v", err)
	}
	return n
}

func TestSlackSendPostsChatMessage(t *testing.T) {
	var calls []slackCall
	srv := newSlackTestServer(t, &calls, []func(http.ResponseWriter){okResponse})
	defer srv.Close()

	n := newSlackNotifier(t, srv.URL, map[string]any{"channel": "#eng-releases"})
	err := n.Send(context.Background(), Message{Text: "PR merged"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if got := calls[0].authorization; got != "Bearer xoxb-test-token" {
		t.Errorf("Authorization = %q, want Bearer token", got)
	}
	if got := calls[0].body["channel"]; got != "#eng-releases" {
		t.Errorf("channel = %v, want #eng-releases", got)
	}
	if got := calls[0].body["text"]; got != "PR merged" {
		t.Errorf("text = %v, want PR merged", got)
	}
}

func TestSlackSendTargetOverridesChannel(t *testing.T) {
	var calls []slackCall
	srv := newSlackTestServer(t, &calls, []func(http.ResponseWriter){okResponse})
	defer srv.Close()

	n := newSlackNotifier(t, srv.URL, map[string]any{"channel": "#default"})
	if err := n.Send(context.Background(), Message{Text: "hi", Target: "#override"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := calls[0].body["channel"]; got != "#override" {
		t.Errorf("channel = %v, want #override", got)
	}
}

func TestSlackSendNoChannelConfigured(t *testing.T) {
	var calls []slackCall
	srv := newSlackTestServer(t, &calls, []func(http.ResponseWriter){okResponse})
	defer srv.Close()

	n := newSlackNotifier(t, srv.URL, nil)
	err := n.Send(context.Background(), Message{Text: "hi"})
	if err == nil {
		t.Fatal("expected error when neither channel nor target is set")
	}
	if len(calls) != 0 {
		t.Fatalf("expected no API calls, got %d", len(calls))
	}
}

func TestSlackSendAPIErrorInBody(t *testing.T) {
	var calls []slackCall
	srv := newSlackTestServer(t, &calls, []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}})
	defer srv.Close()

	n := newSlackNotifier(t, srv.URL, map[string]any{"channel": "#missing"})
	err := n.Send(context.Background(), Message{Text: "hi"})
	if err == nil {
		t.Fatal("expected error when Slack responds ok:false")
	}
	if !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("error should include Slack error code, got: %v", err)
	}
}

func TestSlackSendRetriesOn429(t *testing.T) {
	var calls []slackCall
	srv := newSlackTestServer(t, &calls, []func(http.ResponseWriter){
		func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		okResponse,
	})
	defer srv.Close()

	n := newSlackNotifier(t, srv.URL, map[string]any{"channel": "#eng"})
	if err := n.Send(context.Background(), Message{Text: "hi"}); err != nil {
		t.Fatalf("Send should succeed after retry: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (retry after 429), got %d", len(calls))
	}
}

func TestSlackSendServerErrorNoRetrySuccess(t *testing.T) {
	var calls []slackCall
	srv := newSlackTestServer(t, &calls, []func(http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
	})
	defer srv.Close()

	n := newSlackNotifier(t, srv.URL, map[string]any{"channel": "#eng"})
	err := n.Send(context.Background(), Message{Text: "hi"})
	if err == nil {
		t.Fatal("expected error when server keeps failing")
	}
	if len(calls) != 2 {
		t.Fatalf("expected exactly 2 attempts (1 retry), got %d", len(calls))
	}
}

func TestSlackOptionsPassthrough(t *testing.T) {
	var calls []slackCall
	srv := newSlackTestServer(t, &calls, []func(http.ResponseWriter){okResponse})
	defer srv.Close()

	n := newSlackNotifier(t, srv.URL, map[string]any{"channel": "#eng"})
	err := n.Send(context.Background(), Message{
		Text:    "hi",
		Options: map[string]any{"unfurl_links": false, "thread_ts": "123.456"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := calls[0].body["thread_ts"]; got != "123.456" {
		t.Errorf("thread_ts = %v, want 123.456", got)
	}
	if got, ok := calls[0].body["unfurl_links"].(bool); !ok || got {
		t.Errorf("unfurl_links = %v, want false", calls[0].body["unfurl_links"])
	}
	// Options must not override the core fields.
	if got := calls[0].body["text"]; got != "hi" {
		t.Errorf("text = %v, want hi", got)
	}
}
