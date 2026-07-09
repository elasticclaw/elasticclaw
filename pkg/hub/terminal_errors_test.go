package hub

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newTerminalTestServer builds a minimal hub with one tenant so the
// terminal handler can run its auth and claw-lookup steps.
func newTerminalTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, _ = db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "tenant", "token", "claw-token", now())
	return &Server{db: db, hubCfg: &types.HubConfig{}}
}

// decodeAPIError parses the unified JSON error envelope written by
// httpserver.WriteDomainErr (injected into the claws service through Deps).
func decodeAPIError(t *testing.T, body []byte) (code, msg string) {
	t.Helper()
	var envelope struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("response body %q is not the JSON error envelope: %v", body, err)
	}
	return envelope.Code, envelope.Error
}

// TestHandleTerminalUnauthorizedEnvelope verifies that the terminal handler
// (moved into pkg/hub/claws) writes the domain-error envelope through the
// injected writer instead of importing httpserver directly.
func TestHandleTerminalUnauthorizedEnvelope(t *testing.T) {
	s := newTerminalTestServer(t)

	r := httptest.NewRequest("GET", "/api/terminal/some-claw", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	s.handleTerminal(w, r)

	if w.Code != 401 {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if code, _ := decodeAPIError(t, w.Body.Bytes()); code != "unauthorized" {
		t.Fatalf("code = %q, want %q", code, "unauthorized")
	}
}

func TestHandleTerminalClawNotFoundEnvelope(t *testing.T) {
	s := newTerminalTestServer(t)

	r := httptest.NewRequest("GET", "/api/terminal/missing-claw", nil)
	r.Header.Set("Authorization", "Bearer token")
	w := httptest.NewRecorder()
	s.handleTerminal(w, r)

	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if code, _ := decodeAPIError(t, w.Body.Bytes()); code != "not_found" {
		t.Fatalf("code = %q, want %q", code, "not_found")
	}
}
