package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func newTicketTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "tenant", "secret-token", "claw-token", now()); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Server{db: db, hubCfg: &types.HubConfig{}}
}

func TestAuthTicketSingleUse(t *testing.T) {
	s := newTicketTestServer(t)
	ticket, err := s.issueAuthTicket("tenant", "octocat")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	tenantID, login, ok := s.redeemAuthTicket(ticket)
	if !ok || tenantID != "tenant" || login != "octocat" {
		t.Fatalf("redeem: got (%q,%q,%v)", tenantID, login, ok)
	}
	if _, _, ok := s.redeemAuthTicket(ticket); ok {
		t.Fatal("ticket redeemed twice")
	}
}

func TestAuthTicketExpiry(t *testing.T) {
	s := newTicketTestServer(t)
	ticket, err := s.issueAuthTicket("tenant", "")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	s.ticketMu.Lock()
	tk := s.tickets[ticket]
	tk.expiresAt = time.Now().Add(-time.Second)
	s.tickets[ticket] = tk
	s.ticketMu.Unlock()
	if _, _, ok := s.redeemAuthTicket(ticket); ok {
		t.Fatal("expired ticket accepted")
	}
}

func TestHandleAuthTicketRequiresHeaderAuth(t *testing.T) {
	s := newTicketTestServer(t)

	// No auth header → 401.
	rec := httptest.NewRecorder()
	s.handleAuthTicket(rec, httptest.NewRequest(http.MethodPost, "/api/auth/ticket", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no header: got %d, want 401", rec.Code)
	}

	// Query token must not mint tickets.
	rec = httptest.NewRecorder()
	s.handleAuthTicket(rec, httptest.NewRequest(http.MethodPost, "/api/auth/ticket?token=secret-token", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token: got %d, want 401", rec.Code)
	}

	// Header auth mints a ticket redeemable by withAuth via ?ticket=.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/ticket", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	s.handleAuthTicket(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("header auth: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Ticket == "" || resp.ExpiresIn != 30 {
		t.Fatalf("unexpected response: %+v", resp)
	}

	var gotTenant string
	protected := s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = tenantFromCtx(r)
		w.WriteHeader(http.StatusOK)
	})

	rec = httptest.NewRecorder()
	protected(rec, httptest.NewRequest(http.MethodGet, "/api/ws?ticket="+resp.Ticket, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ticket auth: got %d, want 200", rec.Code)
	}
	if gotTenant != "tenant" {
		t.Fatalf("tenant from ctx: got %q", gotTenant)
	}

	// Second use of the same ticket must fail.
	rec = httptest.NewRecorder()
	protected(rec, httptest.NewRequest(http.MethodGet, "/api/ws?ticket="+resp.Ticket, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ticket reuse: got %d, want 401", rec.Code)
	}
}

// Tickets exist for WS upgrades and <img src> loads only. Any other withAuth
// route must reject ticket auth so a leaked single-use ticket cannot reach
// generic (including mutating) REST endpoints.
func TestTicketAuthRestrictedToTicketCapableRoutes(t *testing.T) {
	s := newTicketTestServer(t)
	protected := s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	allowed := []string{"/api/ws", "/api/files/view/claw-1"}
	for _, path := range allowed {
		ticket, err := s.issueAuthTicket("tenant", "")
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		rec := httptest.NewRecorder()
		protected(rec, httptest.NewRequest(http.MethodGet, path+"?ticket="+ticket, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", path, rec.Code)
		}
	}

	denied := []string{"/api/claws", "/api/claws/claw-1", "/api/messages/claw-1", "/api/workspaces"}
	for _, path := range denied {
		ticket, err := s.issueAuthTicket("tenant", "")
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		rec := httptest.NewRecorder()
		protected(rec, httptest.NewRequest(http.MethodGet, path+"?ticket="+ticket, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401 (ticket must not authenticate generic REST routes)", path, rec.Code)
		}
		// The ignored ticket must not have been consumed by the rejected request.
		if _, _, ok := s.redeemAuthTicket(ticket); !ok {
			t.Fatalf("%s: ticket was consumed by a route that must ignore tickets", path)
		}
	}
}

// Terminal access requires canModifyClaw for GitHub OAuth users, not just
// tenant ownership: a user whose tag filters hide a claw must not be able to
// open an SSH terminal to it by guessing the claw ID.
func TestHandleTerminalEnforcesClawAccess(t *testing.T) {
	s := newTicketTestServer(t)
	s.hubCfg.Auth = &types.AuthConfig{Access: &types.AccessConfig{
		Admins:           []string{"admin-user"},
		ViewRequiresTags: []string{"owner={user}"},
	}}
	if _, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, created_at, tags) VALUES(?,?,?,?,?)`,
		"claw-1", "tenant", "claw-1", now(), `["owner=alice"]`,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	terminal := func(login string) *httptest.ResponseRecorder {
		t.Helper()
		ticket, err := s.issueAuthTicket("tenant", login)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		rec := httptest.NewRecorder()
		s.handleTerminal(rec, httptest.NewRequest(http.MethodGet, "/api/terminal/claw-1?ticket="+ticket, nil))
		return rec
	}

	// A tenant user whose tags do not match the claw is forbidden.
	if rec := terminal("mallory"); rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized login: got %d, want 403", rec.Code)
	}

	// A user matching the tag filter passes the access check and reaches the
	// SSH availability check (503: the test claw has no SSH endpoint).
	if rec := terminal("alice"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("tagged login: got %d, want 503 (past access check)", rec.Code)
	}

	// Admins bypass tag checks.
	if rec := terminal("admin-user"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("admin login: got %d, want 503 (past access check)", rec.Code)
	}

	// Token-based auth (no GitHub login) keeps full tenant access.
	if rec := terminal(""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("token auth: got %d, want 503 (past access check)", rec.Code)
	}
}

func TestWithAuthDeprecatedQueryTokenStillWorks(t *testing.T) {
	s := newTicketTestServer(t)
	protected := s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	protected(rec, httptest.NewRequest(http.MethodGet, "/api/claws?token=secret-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("deprecated query token: got %d, want 200", rec.Code)
	}
}
