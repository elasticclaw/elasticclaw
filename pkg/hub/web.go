package hub

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

const sessionCookieName = "elasticclaw_session"

// registerWebHandlers sets up auth endpoints and static file serving.
// staticFS is the embedded web/out directory (or nil to skip static serving).
func (s *Server) registerWebHandlers(staticFS fs.FS) {
	// Auth endpoints — no cookie required
	s.mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)

	// Hub config — requires auth, returns token to browser
	s.mux.HandleFunc("GET /api/hub-config", s.webAuthMiddleware(s.handleHubConfig))

	if staticFS == nil {
		return
	}

	fileServer := http.FileServer(http.FS(staticFS))

	// Serve static Next.js export — auth-gated
	s.mux.HandleFunc("/", s.webAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		fileServer.ServeHTTP(w, r)
	}))
}

// webAuthMiddleware checks the session cookie. Login page and static assets bypass auth.
func (s *Server) webAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Always pass through login page and Next.js build assets
		if path == "/login" || path == "/login/" ||
			strings.HasPrefix(path, "/_next/") ||
			strings.HasSuffix(path, ".png") ||
			strings.HasSuffix(path, ".svg") ||
			strings.HasSuffix(path, ".ico") {
			next(w, r)
			return
		}

		uiToken := s.hubCfg.UIToken
		if uiToken == "" {
			uiToken = "admin"
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value != uiToken {
			if isWebAPIPath(path) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login/?next="+r.URL.RequestURI(), http.StatusFound)
			return
		}

		next(w, r)
	}
}

func isWebAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/hub/")
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	uiToken := s.hubCfg.UIToken
	if uiToken == "" {
		uiToken = "admin"
	}

	if body.Password != uiToken {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"Invalid password"}`))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    uiToken,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 30,
		Secure:   r.TLS != nil,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:    sessionCookieName,
		Value:   "",
		Path:    "/",
		MaxAge:  -1,
		Expires: time.Unix(0, 0),
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (s *Server) handleHubConfig(w http.ResponseWriter, r *http.Request) {
	token := s.hubCfg.Token
	hubURL := s.hubCfg.URL
	if hubURL == "" {
		hubURL = "http://localhost:18788"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token":  token,
		"hubUrl": hubURL,
	})
}
