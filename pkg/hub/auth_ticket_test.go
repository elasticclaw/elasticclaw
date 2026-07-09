package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestAuthTicketStoreSingleUseAndExpiry(t *testing.T) {
	var st authTicketStore

	ticket, err := st.issue("tenant-a", "octocat")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	tenantID, login, ok := st.redeem(ticket)
	if !ok || tenantID != "tenant-a" || login != "octocat" {
		t.Fatalf("redeem = (%q, %q, %v), want (tenant-a, octocat, true)", tenantID, login, ok)
	}
	if _, _, ok := st.redeem(ticket); ok {
		t.Fatal("second redemption must fail (single-use)")
	}
	if _, _, ok := st.redeem("no-such-ticket"); ok {
		t.Fatal("unknown ticket must fail")
	}

	expired, err := st.issue("tenant-b", "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	st.mu.Lock()
	entry := st.tickets[expired]
	entry.expiresAt = time.Now().Add(-time.Second)
	st.tickets[expired] = entry
	st.mu.Unlock()
	if _, _, ok := st.redeem(expired); ok {
		t.Fatal("expired ticket must fail")
	}
}

func TestAuthTicketEndToEndAuthorizesBrowserOnlyEndpoint(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")

	// 1. Issue a ticket through the authenticated handler.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/ticket", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.withAuth(s.handleAuthTicket)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("issue ticket status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode ticket response: %v", err)
	}
	if resp.Ticket == "" || resp.ExpiresIn <= 0 {
		t.Fatalf("unexpected ticket response: %+v", resp)
	}

	// 2. Redeem it on a ticket-enabled endpoint: the tenant must land in ctx.
	var gotTenant string
	handler := s.withAuthOrTicket(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = tenantFromCtx(r)
		w.WriteHeader(http.StatusNoContent)
	})
	wsReq := httptest.NewRequest(http.MethodGet, "/api/ws?ticket="+resp.Ticket, nil)
	wsRec := httptest.NewRecorder()
	handler(wsRec, wsReq)
	if wsRec.Code != http.StatusNoContent {
		t.Fatalf("ticket request status = %d: %s", wsRec.Code, wsRec.Body.String())
	}
	if gotTenant != "test-tenant-id" {
		t.Fatalf("tenant from ticket = %q, want test-tenant-id", gotTenant)
	}

	// 3. Same ticket again: rejected (single-use).
	wsReq = httptest.NewRequest(http.MethodGet, "/api/ws?ticket="+resp.Ticket, nil)
	wsRec = httptest.NewRecorder()
	handler(wsRec, wsReq)
	if wsRec.Code != http.StatusUnauthorized {
		t.Fatalf("reused ticket status = %d, want %d", wsRec.Code, http.StatusUnauthorized)
	}

	// 4. Without a ticket the middleware behaves like withAuth: header works,
	// nothing at all does not.
	wsReq = httptest.NewRequest(http.MethodGet, "/api/ws", nil)
	wsReq.Header.Set("Authorization", "Bearer test-token")
	wsRec = httptest.NewRecorder()
	handler(wsRec, wsReq)
	if wsRec.Code != http.StatusNoContent {
		t.Fatalf("header auth status = %d, want %d", wsRec.Code, http.StatusNoContent)
	}
	wsReq = httptest.NewRequest(http.MethodGet, "/api/ws", nil)
	wsRec = httptest.NewRecorder()
	handler(wsRec, wsReq)
	if wsRec.Code != http.StatusUnauthorized {
		t.Fatalf("no-credential status = %d, want %d", wsRec.Code, http.StatusUnauthorized)
	}

	// 5. The deprecated ?token= stays rejected even on ticket endpoints.
	wsReq = httptest.NewRequest(http.MethodGet, "/api/ws?token=test-token", nil)
	wsRec = httptest.NewRecorder()
	handler(wsRec, wsReq)
	if wsRec.Code != http.StatusUnauthorized {
		t.Fatalf("query token status = %d, want %d", wsRec.Code, http.StatusUnauthorized)
	}
}

func TestTerminalAcceptsTicketAuth(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")

	ticket, err := s.authTickets.issue("test-tenant-id", "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// A valid ticket passes auth and reaches claw resolution (404: the claw
	// does not exist), instead of failing with 401.
	req := httptest.NewRequest(http.MethodGet, "/api/terminal/no-such-claw?ticket="+ticket, nil)
	rec := httptest.NewRecorder()
	s.handleTerminal(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("valid ticket rejected with 401: %s", rec.Body.String())
	}

	// A reused or bogus ticket is a 401.
	req = httptest.NewRequest(http.MethodGet, "/api/terminal/no-such-claw?ticket="+ticket, nil)
	rec = httptest.NewRecorder()
	s.handleTerminal(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused ticket status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
