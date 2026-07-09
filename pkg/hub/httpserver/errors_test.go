package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// TestStatusForError covers the full error→status mapping table, including
// wrapped errors (errors.Is must see through fmt.Errorf %w chains) and the
// 500 fallback for unrecognized errors.
func TestStatusForError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"unauthorized", types.ErrUnauthorized, http.StatusUnauthorized, "unauthorized"},
		{"tenant mismatch", types.ErrTenantMismatch, http.StatusForbidden, "forbidden"},
		{"claw not found", types.ErrClawNotFound, http.StatusNotFound, "not_found"},
		{"workflow not found", types.ErrWorkflowNotFound, http.StatusNotFound, "not_found"},
		{"workspace not found", types.ErrWorkspaceNotFound, http.StatusNotFound, "not_found"},
		{"checkpoint not found", types.ErrCheckpointNotFound, http.StatusNotFound, "not_found"},
		{"checkpoint not ready", types.ErrCheckpointNotReady, http.StatusConflict, "conflict"},
		{"wrapped sentinel", fmt.Errorf("restore claw abc: %w", types.ErrCheckpointNotFound), http.StatusNotFound, "not_found"},
		{"double wrapped sentinel", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", types.ErrClawNotFound)), http.StatusNotFound, "not_found"},
		{"unknown error", errors.New("disk on fire"), http.StatusInternalServerError, "internal"},
		{"nil-safe unknown", fmt.Errorf("plain"), http.StatusInternalServerError, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code := StatusForError(tc.err)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("StatusForError(%v) = (%d, %q), want (%d, %q)", tc.err, status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

// TestWriteDomainErr verifies the envelope body: domain errors surface their
// message, unknown errors are replaced by a generic message so internals
// never leak into responses.
func TestWriteDomainErr(t *testing.T) {
	t.Run("domain error surfaces message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteDomainErr(rec, fmt.Errorf("claw %q: %w", "abc123", types.ErrClawNotFound))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		var body APIError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if body.Code != "not_found" || body.Error != `claw "abc123": claw not found` {
			t.Fatalf("body = %+v", body)
		}
	})
	t.Run("unknown error is masked", func(t *testing.T) {
		rec := httptest.NewRecorder()
		WriteDomainErr(rec, errors.New("SELECT failed: /var/lib/hub.db locked"))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		var body APIError
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if body.Code != "internal" || body.Error != "internal server error" {
			t.Fatalf("body = %+v", body)
		}
	})
}
