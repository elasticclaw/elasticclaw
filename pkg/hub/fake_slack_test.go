package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
