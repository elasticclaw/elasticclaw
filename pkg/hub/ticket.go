package hub

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Single-use auth tickets.
//
// Browsers cannot set the Authorization header on WebSocket upgrades or on
// resources loaded via <img src>. Instead of leaking the long-lived bearer
// token into query strings (access logs, proxy logs, URL history), the UI
// calls POST /api/auth/ticket (authenticated via header) to obtain an opaque
// single-use ticket with a short TTL, and passes it as ?ticket= on those
// endpoints only.

const authTicketTTL = 30 * time.Second

type authTicket struct {
	tenantID    string
	githubLogin string
	expiresAt   time.Time
}

// issueAuthTicket mints a single-use ticket bound to the given tenant/login.
func (s *Server) issueAuthTicket(tenantID, githubLogin string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	now := time.Now()
	s.ticketMu.Lock()
	if s.tickets == nil {
		s.tickets = make(map[string]authTicket)
	}
	// Opportunistically prune expired tickets so the map cannot grow unbounded.
	for k, t := range s.tickets {
		if now.After(t.expiresAt) {
			delete(s.tickets, k)
		}
	}
	s.tickets[id] = authTicket{tenantID: tenantID, githubLogin: githubLogin, expiresAt: now.Add(authTicketTTL)}
	s.ticketMu.Unlock()
	return id, nil
}

// redeemAuthTicket consumes a ticket. Each ticket is valid for exactly one
// redemption within its TTL.
func (s *Server) redeemAuthTicket(id string) (tenantID, githubLogin string, ok bool) {
	if id == "" {
		return "", "", false
	}
	s.ticketMu.Lock()
	t, found := s.tickets[id]
	if found {
		delete(s.tickets, id)
	}
	s.ticketMu.Unlock()
	if !found || time.Now().After(t.expiresAt) {
		return "", "", false
	}
	return t.tenantID, t.githubLogin, true
}

// handleAuthTicket issues a single-use ticket for WS/img endpoints.
// Route: POST /api/auth/ticket — authenticated via Authorization header only
// (no query fallback: a leaked query token must not be able to mint tickets).
func (s *Server) handleAuthTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tenantID, githubLogin, ok := s.resolveAuthToken(token)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ticket, err := s.issueAuthTicket(tenantID, githubLogin)
	if err != nil {
		http.Error(w, "ticket generation failed", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{
		"ticket":     ticket,
		"expires_in": int(authTicketTTL / time.Second),
	})
}
