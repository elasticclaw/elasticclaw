// Single-use auth tickets for endpoints that cannot send an Authorization
// header from the browser: WebSocket upgrades (/api/ws, /api/terminal/{id})
// and resources loaded via <img src> (/api/files/view/...).
//
// Flow (phase 0.3 of the re-arch plan, prerequisite for the phase 2.6
// removal of the deprecated ?token= query fallback): the frontend calls
// POST /api/auth/ticket with its normal Authorization header, receives an
// opaque ticket with a short TTL that is redeemable exactly once, and
// appends it as ?ticket= to the browser-only URL. Tickets never carry the
// long-lived token, so a leaked URL (access logs, proxy logs, browser
// history) is worthless after redemption or expiry.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
)

// authTicketTTL is how long an issued ticket stays redeemable. Long enough
// for the browser to turn around and open the WS / load the image, short
// enough that a logged URL is stale almost immediately.
const authTicketTTL = 30 * time.Second

type authTicket struct {
	tenantID    string
	githubLogin string
	expiresAt   time.Time
}

// authTicketStore is an in-memory single-use ticket registry. The zero
// value is ready to use (hand-built test servers need no wiring); tickets
// are process-local by design — a hub restart invalidates them, and the
// frontend simply requests a new one.
type authTicketStore struct {
	mu      sync.Mutex
	tickets map[string]authTicket
}

func (st *authTicketStore) issue(tenantID, githubLogin string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	ticket := hex.EncodeToString(buf)
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.tickets == nil {
		st.tickets = make(map[string]authTicket)
	}
	// Lazy purge keeps the map from growing with never-redeemed tickets
	// without needing a background goroutine.
	for k, v := range st.tickets {
		if now.After(v.expiresAt) {
			delete(st.tickets, k)
		}
	}
	st.tickets[ticket] = authTicket{tenantID: tenantID, githubLogin: githubLogin, expiresAt: now.Add(authTicketTTL)}
	return ticket, nil
}

// redeem consumes the ticket: a second redemption (or an expired ticket)
// fails.
func (st *authTicketStore) redeem(ticket string) (tenantID, githubLogin string, ok bool) {
	if ticket == "" {
		return "", "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	entry, found := st.tickets[ticket]
	if !found {
		return "", "", false
	}
	delete(st.tickets, ticket)
	if time.Now().After(entry.expiresAt) {
		return "", "", false
	}
	return entry.tenantID, entry.githubLogin, true
}

// handleAuthTicket issues a single-use ticket for the authenticated caller.
// Mounted behind withAuth, so the tenant (and GitHub login, when present)
// come from the request context.
func (s *Server) handleAuthTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	githubLogin, _ := r.Context().Value(ctxGitHubLoginKey{}).(string)
	ticket, err := s.authTickets.issue(tenantFromCtx(r), githubLogin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "failed to issue ticket")
		return
	}
	jsonOK(w, map[string]interface{}{
		"ticket":     ticket,
		"expires_in": int(authTicketTTL.Seconds()),
	})
}

// redeemAuthTicket exposes ticket redemption to subpackages (the claws
// terminal handler authenticates on its own, outside withAuth).
func (s *Server) redeemAuthTicket(ticket string) (tenantID string, ok bool) {
	tenantID, _, ok = s.authTickets.redeem(ticket)
	return tenantID, ok
}

// withAuthOrTicket authenticates like withAuth but additionally accepts a
// single-use ?ticket= query parameter. It guards only the browser-only
// endpoints that cannot set headers (WS upgrade, <img src>); everything
// else keeps the header-only withAuth.
func (s *Server) withAuthOrTicket(next http.HandlerFunc) http.HandlerFunc {
	authed := s.withAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		ticket := r.URL.Query().Get("ticket")
		if ticket == "" {
			authed(w, r)
			return
		}
		tenantID, githubLogin, ok := s.authTickets.redeem(ticket)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired ticket")
			return
		}
		ctx := context.WithValue(r.Context(), ctxTenantKey{}, tenantID)
		ctx = logger.NewContext(ctx, logger.FromContext(ctx).With("tenant_id", tenantID))
		if githubLogin != "" {
			ctx = context.WithValue(ctx, ctxGitHubLoginKey{}, githubLogin)
		}
		next(w, r.WithContext(ctx))
	}
}
