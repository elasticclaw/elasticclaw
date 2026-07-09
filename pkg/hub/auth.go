// Authentication middlewares, token/tenant resolution, and login/session handlers.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/logger"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ─── Auth ────────────────────────────────────────────────────────────────────

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Authorization header only. The deprecated ?token= query fallback was
		// removed (Phase 2.6): tokens in URLs leak into access logs, proxy logs,
		// and browser history. Endpoints that cannot set headers (WS upgrades,
		// <img src>) authenticate with a single-use ticket instead.
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		tenantID, githubLogin, ok := s.resolveAuthToken(token)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), ctxTenantKey{}, tenantID)
		ctx = logger.NewContext(ctx, logger.FromContext(ctx).With("tenant_id", tenantID))
		if githubLogin != "" {
			ctx = context.WithValue(ctx, ctxGitHubLoginKey{}, githubLogin)
		}
		r = r.WithContext(ctx)
		next(w, r)
	}
}

func (s *Server) withAdminForMethods(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	adminMethods := make(map[string]struct{}, len(methods))
	for _, method := range methods {
		adminMethods[method] = struct{}{}
	}
	authHandler := s.withAuth(next)
	adminHandler := s.withConfigMutationAuth(next)
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminMethods[r.Method]; ok {
			adminHandler(w, r)
			return
		}
		authHandler(w, r)
	}
}

func (s *Server) withConfigMutationAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Authorization header or web session header only. The deprecated
		// ?token= query fallback was removed (Phase 2.6).
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get(webSessionHeader)
		}
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}

		s.cfgMu.RLock()
		hubToken := s.hubCfg.Token
		var accessCfg *types.AccessConfig
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.cfgMu.RUnlock()

		if token == hubToken && hubToken != "" {
			next(w, r)
			return
		}

		sessionSecret := s.webSessionSecret()
		if sessionSecret != "" {
			if payload, ok := verifyGitHubSession(sessionSecret, token); ok {
				if !isAccessAdmin(accessCfg, payload.Login) {
					writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
					return
				}
				ctx := context.WithValue(r.Context(), ctxGitHubLoginKey{}, payload.Login)
				r = r.WithContext(ctx)
				next(w, r)
				return
			}
		}

		writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
	}
}

type ctxTenantKey struct{}

func tenantFromCtx(r *http.Request) string {
	v, _ := r.Context().Value(ctxTenantKey{}).(string)
	return v
}

// resolveAuthToken accepts either a tenant token (legacy/password auth)
// or a GitHub OAuth session token and returns the resolved tenant/login.
func (s *Server) resolveAuthToken(token string) (tenantID, githubLogin string, ok bool) {
	if token == "" {
		return "", "", false
	}
	// Accept the hub config token directly — this means a token change in hub.yaml
	// takes effect immediately without requiring a DB migration.
	s.cfgMu.RLock()
	hubCfgToken := s.hubCfg.Token
	s.cfgMu.RUnlock()
	if token == hubCfgToken && hubCfgToken != "" {
		if tid, err := s.githubTenantID(); err == nil {
			return tid, "", true
		}
	}
	if tenantID, err := s.tenantByToken(token); err == nil {
		return tenantID, "", true
	}
	sessionSecret := s.webSessionSecret()
	if sessionSecret == "" {
		return "", "", false
	}
	payload, valid := verifyGitHubSession(sessionSecret, token)
	if !valid {
		return "", "", false
	}
	tenantID, err := s.githubTenantID()
	if err != nil {
		return "", "", false
	}
	return tenantID, payload.Login, true
}

// githubTenantID resolves the tenant backing GitHub OAuth sessions.
func (s *Server) githubTenantID() (string, error) {
	s.cfgMu.RLock()
	hubToken := s.hubCfg.Token
	s.cfgMu.RUnlock()
	if hubToken != "" {
		if tenantID, err := s.tenantByToken(hubToken); err == nil {
			return tenantID, nil
		}
	}
	return s.st().Tenants().FirstID(s.base())
}

func (s *Server) tenantByToken(token string) (string, error) {
	return s.st().Tenants().IDByToken(s.base(), token)
}

func (s *Server) tenantByClawToken(token string) (string, error) {
	return s.st().Tenants().IDByClawToken(s.base(), token)
}

// ─── REST handlers ────────────────────────────────────────────────────────────

// ─── Web UI auth (for embedded/static web UI) ───────────────────────────────────
// These endpoints validate the UI password (ui_password) and return a session token
// the browser stores and sends as Authorization: Bearer <token>.

const webSessionHeader = "X-Elasticclaw-Session"

func (s *Server) resolveUIPassword() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.hubCfg.UIPassword != "" {
		return s.hubCfg.UIPassword
	}
	return "admin"
}

func (s *Server) withWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get(webSessionHeader)
		}
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		s.cfgMu.RLock()
		hubToken := s.hubCfg.Token
		s.cfgMu.RUnlock()
		sessionSecret := s.webSessionSecret()

		// Accept shared hub token (existing behavior)
		if token == hubToken {
			next(w, r)
			return
		}

		// Try GitHub OAuth session token
		if sessionSecret != "" {
			if payload, ok := verifyGitHubSession(sessionSecret, token); ok {
				ctx := context.WithValue(r.Context(), ctxGitHubLoginKey{}, payload.Login)
				ctx = context.WithValue(ctx, ctxGitHubSessionPayloadKey{}, payload)
				r = r.WithContext(ctx)
				next(w, r)
				return
			}
		}

		writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
	}
}

func isAccessAdmin(cfg *types.AccessConfig, login string) bool {
	if login == "" || cfg == nil {
		return false
	}
	for _, admin := range cfg.Admins {
		if strings.EqualFold(admin, login) {
			return true
		}
	}
	return false
}

func (s *Server) withWebAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get(webSessionHeader)
		}
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		s.cfgMu.RLock()
		hubToken := s.hubCfg.Token
		var accessCfg *types.AccessConfig
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.cfgMu.RUnlock()
		sessionSecret := s.webSessionSecret()

		// Password-authenticated sessions keep existing admin access.
		if token == hubToken {
			next(w, r)
			return
		}

		if sessionSecret != "" {
			if payload, ok := verifyGitHubSession(sessionSecret, token); ok {
				if !isAccessAdmin(accessCfg, payload.Login) {
					writeErr(w, http.StatusForbidden, "forbidden", "forbidden")
					return
				}
				ctx := context.WithValue(r.Context(), ctxGitHubLoginKey{}, payload.Login)
				r = r.WithContext(ctx)
				next(w, r)
				return
			}
		}

		writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
	}
}

func (s *Server) handleWebLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	s.cfgMu.RLock()
	disablePassword := s.hubCfg.Auth != nil && s.hubCfg.Auth.DisablePasswordAuth
	s.cfgMu.RUnlock()
	if disablePassword {
		writeErr(w, http.StatusForbidden, "forbidden", "password login disabled")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	if body.Password != s.resolveUIPassword() {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid password")
		return
	}
	s.cfgMu.RLock()
	hubToken := s.hubCfg.Token
	s.cfgMu.RUnlock()
	jsonOK(w, map[string]interface{}{
		"ok":       true,
		"hubToken": hubToken, // hub API token — browser uses for all hub calls
	})
}

func (s *Server) handleWebLogout(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]bool{"ok": true})
}

func (s *Server) handleWebMe(w http.ResponseWriter, r *http.Request) {
	if payload := githubSessionPayloadFromContext(r.Context()); payload != nil {
		s.cfgMu.RLock()
		var accessCfg *types.AccessConfig
		if s.hubCfg.Auth != nil {
			accessCfg = s.hubCfg.Auth.Access
		}
		s.cfgMu.RUnlock()
		jsonOK(w, map[string]interface{}{
			"login":       payload.Login,
			"name":        payload.Name,
			"avatar_url":  payload.AvatarURL,
			"auth_method": "github",
			"is_admin":    isAccessAdmin(accessCfg, payload.Login),
		})
		return
	}
	jsonOK(w, map[string]interface{}{"auth_method": "password", "is_admin": true})
}

// handleAuthConfig returns public auth config (no auth required).
func (s *Server) handleAuthConfig(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.RLock()
	githubOAuthEnabled := s.hubCfg.Auth != nil && s.hubCfg.Auth.GitHubOAuth != nil && s.hubCfg.Auth.GitHubOAuth.ClientID != ""
	passwordAuthEnabled := s.hubCfg.Token != "" && !(s.hubCfg.Auth != nil && s.hubCfg.Auth.DisablePasswordAuth)
	s.cfgMu.RUnlock()
	jsonOK(w, map[string]bool{
		"github_oauth_enabled":  githubOAuthEnabled,
		"password_auth_enabled": passwordAuthEnabled,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid request")
		return
	}
	tenantID, err := s.tenantByToken(body.Token)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid token")
		return
	}
	jsonOK(w, map[string]string{"tenant_id": tenantID, "token": body.Token})
}

// ─── GitHub Token Endpoint ────────────────────────────────────────────────────

// handleGitHubToken mints a fresh GitHub installation token for the claw.
// Auth: ?claw_token= query param (the claw's hub token, same as registration).
// URL: GET /api/github/token/:clawId
// Used by the git credential helper on the VM.
func (s *Server) handleGitHubToken(w http.ResponseWriter, r *http.Request) {
	clawToken := r.URL.Query().Get("claw_token")
	if clawToken == "" {
		clawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	// Single-tenant: validate against hub's claw_token directly
	s.cfgMu.RLock()
	hubClawToken := s.hubCfg.ClawToken
	s.cfgMu.RUnlock()
	if clawToken != hubClawToken {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
		return
	}

	clawID := strings.TrimPrefix(r.URL.Path, "/api/github/token/")
	if clawID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "missing claw id")
		return
	}

	workspaceName, reposJSON, err := s.st().Claws().WorkspaceRepos(r.Context(), clawID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "claw not found")
		return
	}

	var repos []RepoAccess
	if reposJSON != "" && reposJSON != "[]" {
		// Support both old (capitalized) and new (lowercase) JSON key formats.
		// Old format: [{"Repo":"owner/repo","Permissions":"write"}]
		// New format: [{"repo":"owner/repo","permissions":"write"}]
		var rawRepos []struct {
			Repo        string `json:"repo"`        // new format
			RepoOld     string `json:"Repo"`        // old format (no json tags)
			Permissions string `json:"permissions"` // new format
			PermsOld    string `json:"Permissions"` // old format
		}
		if err := json.Unmarshal([]byte(reposJSON), &rawRepos); err == nil {
			for _, r := range rawRepos {
				repo := r.Repo
				if repo == "" {
					repo = r.RepoOld // fall back to old capitalized key
				}
				perm := r.Permissions
				if perm == "" {
					perm = r.PermsOld
				}
				if perm == "" {
					perm = "read"
				}
				if repo != "" {
					repos = append(repos, RepoAccess{Repo: repo, Permissions: perm})
				}
			}
		}
	}

	// Try each configured GitHub App in order; use the first that finds an installation
	s.cfgMu.RLock()
	githubApps := append([]*types.GitHubAppConfig(nil), s.hubCfg.GitHubApps...)
	s.cfgMu.RUnlock()
	if workspaceApps, err := loadWorkspaceGitHubAppConfigs(workspaceName); err == nil && len(workspaceApps) > 0 {
		githubApps = append(workspaceApps, githubApps...)
	}
	if len(githubApps) == 0 {
		writeErr(w, http.StatusNotImplemented, "not_implemented", "no github apps configured")
		return
	}
	for i, appCfg := range githubApps {
		provider, err := NewGitHubTokenProvider(appCfg)
		if err != nil {
			logfCtx(r.Context(), "github app[%d] (app_id=%d url=%s) config error: %v", i, appCfg.AppID, appCfg.URL, err)
			continue
		}
		token, expiresAt, err := provider.InstallationToken(r.Context(), 0, repos)
		if err != nil {
			// Debug-level only — expected when multiple apps configured and only one matches
			logfCtx(r.Context(), "[github] app[%d] app_id=%d: no match for repos (trying next): %v", i, appCfg.AppID, err)
			continue
		}
		logfCtx(r.Context(), "github token issued via app_id=%d for claw %s", appCfg.AppID, clawID[:8])
		jsonOK(w, map[string]interface{}{
			"token":      token,
			"expires_at": expiresAt,
		})
		return
	}

	logfCtx(r.Context(), "no github app found with installation for repos %v (claw %s)", repos, clawID[:8])
	writeErr(w, http.StatusNotFound, "not_found", "no github installation found for the requested repos")
}
