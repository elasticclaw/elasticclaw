package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWithRecoveryConvertsPanicTo500 verifies the recovery middleware catches a
// panic from a downstream handler and responds with a 500 JSON error envelope
// instead of crashing the process/goroutine.
func TestWithRecoveryConvertsPanicTo500(t *testing.T) {
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	rec := httptest.NewRecorder()

	// Should not panic out of this call — withRecovery must catch it.
	withRecovery(panicHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var body apiError
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if body.Error == "" {
		t.Fatalf("expected non-empty error message in envelope, got %+v", body)
	}
}

// TestWithRecoveryPassesThroughNormalRequests verifies the middleware is a
// no-op when the downstream handler does not panic.
func TestWithRecoveryPassesThroughNormalRequests(t *testing.T) {
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	})

	req := httptest.NewRequest(http.MethodGet, "/api/whatever", nil)
	rec := httptest.NewRecorder()

	withRecovery(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "ok")
	}
}

// TestUIHandlersReturnJSONErrorEnvelope spot-checks that migrated UI-facing
// handlers (claws, messages, settings, auth) respond with the standard
// {"error": "...", "code": "..."} envelope on error, instead of the old
// plain-text http.Error body.
func TestUIHandlersReturnJSONErrorEnvelope(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")

	cases := []struct {
		name       string
		method     string
		path       string
		handler    http.HandlerFunc
		wantStatus int
	}{
		{
			name:       "claws detail not found",
			method:     http.MethodGet,
			path:       "/api/claws/does-not-exist",
			handler:    s.handleClawDetail,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "messages missing claw id",
			method:     http.MethodGet,
			path:       "/api/messages/",
			handler:    s.handleMessages,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "settings method not allowed",
			method:     http.MethodDelete,
			path:       "/api/settings",
			handler:    s.handleSettings,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "web login wrong method",
			method:     http.MethodGet,
			path:       "/api/auth/login",
			handler:    s.handleWebLogin,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
			rec := httptest.NewRecorder()

			tc.handler(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json (body=%s)", ct, rec.Body.String())
			}
			var body apiError
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
			}
			if body.Error == "" {
				t.Fatalf("expected non-empty error message in envelope, got %+v", body)
			}
		})
	}
}
