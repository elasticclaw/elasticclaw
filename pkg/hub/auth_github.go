package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ─── GitHub OAuth session token ───────────────────────────────────────────────

const githubSessionExpiry = 7 * 24 * time.Hour

// githubSessionPayload is the signed payload stored in OAuth session tokens.
type githubSessionPayload struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	Exp       int64  `json:"exp"`
}

// signGitHubSession creates a signed session token for a GitHub user.
// Format: base64url(payload).base64url(hmac-sha256)
func signGitHubSession(secret, login, name, avatarURL string) (string, error) {
	payload := githubSessionPayload{
		Login:     login,
		Name:      name,
		AvatarURL: avatarURL,
		Exp:       time.Now().Add(githubSessionExpiry).Unix(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return encoded + "." + sig, nil
}

// verifyGitHubSession validates the token and returns the payload if valid and unexpired.
func verifyGitHubSession(secret, token string) (*githubSessionPayload, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, false
	}
	encoded, sig := parts[0], parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(encoded))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return nil, false
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	var p githubSessionPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, false
	}
	if time.Now().Unix() > p.Exp {
		return nil, false
	}
	return &p, true
}

// ─── Context key for GitHub login ────────────────────────────────────────────

type ctxGitHubLoginKey struct{}

// githubLoginFromContext returns the GitHub login attached to the context, if any.
func githubLoginFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxGitHubLoginKey{}).(string)
	return v
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// ghBaseURL returns the GitHub API base URL (overridable for tests).
func (s *Server) ghBaseURL() string {
	if s.githubBaseURL != "" {
		return s.githubBaseURL
	}
	return "https://api.github.com"
}

// webSessionSecret returns the HMAC key used to sign/verify web session tokens.
func (s *Server) webSessionSecret() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.hubCfg.Token != "" {
		return s.hubCfg.Token
	}
	if s.hubCfg.Auth != nil && s.hubCfg.Auth.GitHubOAuth != nil {
		return s.hubCfg.Auth.GitHubOAuth.ClientSecret
	}
	return ""
}

// ─── Public endpoints ────────────────────────────────────────────────────────

// handleGitHubClientID returns just the OAuth client_id (public, no secret).
// The frontend needs this to build the GitHub authorize URL client-side.
func (s *Server) handleGitHubClientID(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.hubCfg.Auth
	s.mu.RUnlock()
	if cfg == nil || cfg.GitHubOAuth == nil || cfg.GitHubOAuth.ClientID == "" {
		http.Error(w, "GitHub OAuth not configured", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonOK(w, map[string]string{"client_id": cfg.GitHubOAuth.ClientID})
}

// ─── GitHub OAuth exchange ────────────────────────────────────────────────────
//
// The frontend handles the GitHub OAuth redirect entirely:
//   1. Frontend generates state, redirects user to GitHub with redirect_uri=<frontend>/login
//   2. GitHub sends code back to <frontend>/login?code=...&state=...
//   3. Frontend validates state, then POSTs {code, redirect_uri} to this endpoint
//   4. Hub exchanges code with GitHub (holds client_secret), validates allowlist,
//      and returns a signed session token directly — no Go redirects ever appear in the browser.

// handleGitHubOAuthExchange accepts {code, redirect_uri} from the frontend,
// exchanges it with GitHub, validates allowlist, and returns a session token.
func (s *Server) handleGitHubOAuthExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	cfg := s.hubCfg.Auth
	s.mu.RUnlock()

	if cfg == nil || cfg.GitHubOAuth == nil {
		http.Error(w, "GitHub OAuth not configured", http.StatusNotFound)
		return
	}
	oauthCfg := cfg.GitHubOAuth
	secret := s.webSessionSecret()
	if secret == "" {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var body struct {
		Code        string `json:"code"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Exchange code for GitHub access token
	accessToken, err := s.githubExchangeCode(r.Context(), oauthCfg.ClientID, oauthCfg.ClientSecret, body.Code, body.RedirectURI)
	if err != nil {
		log.Printf("[github-oauth] token exchange error: %v", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}

	// Fetch user info
	ghUser, err := s.githubFetchUser(r.Context(), accessToken)
	if err != nil {
		log.Printf("[github-oauth] fetch user error: %v", err)
		http.Error(w, "fetch user failed", http.StatusBadGateway)
		return
	}

	// Check allowlist
	allowed, err := s.githubCheckAllowlist(r.Context(), oauthCfg, accessToken, ghUser.Login)
	if err != nil {
		log.Printf("[github-oauth] allowlist check error: %v", err)
		http.Error(w, "allowlist check failed", http.StatusBadGateway)
		return
	}
	if !allowed {
		log.Printf("[github-oauth] denied: user %s not in allowlist", ghUser.Login)
		http.Error(w, "access denied", http.StatusForbidden)
		return
	}

	// Issue signed session token
	sessionToken, err := signGitHubSession(secret, ghUser.Login, ghUser.Name, ghUser.AvatarURL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Printf("[github-oauth] login: %s (%s)", ghUser.Login, ghUser.Name)

	w.Header().Set("Cache-Control", "no-store")
	jsonOK(w, map[string]string{"github_token": sessionToken})
}

// ─── GitHub API helpers ───────────────────────────────────────────────────────

type githubUser struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

func (s *Server) githubExchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (string, error) {
	body := url.Values{}
	body.Set("client_id", clientID)
	body.Set("client_secret", clientSecret)
	body.Set("code", code)
	if redirectURI != "" {
		body.Set("redirect_uri", redirectURI)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("oauth error: %s: %s", result.Error, result.ErrorDesc)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("empty access token")
	}
	return result.AccessToken, nil
}

func (s *Server) githubFetchUser(ctx context.Context, accessToken string) (*githubUser, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.ghBaseURL()+"/user", nil)
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch user: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch user: status %d", resp.StatusCode)
	}
	var u githubUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("decode user: %w", err)
	}
	return &u, nil
}

func (s *Server) githubCheckAllowlist(ctx context.Context, cfg *types.GitHubOAuthConfig, accessToken, login string) (bool, error) {
	// If no allowlist is configured, allow any authenticated GitHub user.
	if len(cfg.AllowedUsers) == 0 && len(cfg.AllowedOrgs) == 0 && len(cfg.AllowedTeams) == 0 {
		return true, nil
	}

	// Check allowed users
	for _, u := range cfg.AllowedUsers {
		if strings.EqualFold(u, login) {
			return true, nil
		}
	}

	// Check allowed orgs
	if len(cfg.AllowedOrgs) > 0 {
		orgs, err := s.githubFetchUserOrgs(ctx, accessToken)
		if err != nil {
			return false, err
		}
		orgSet := make(map[string]bool, len(orgs))
		for _, o := range orgs {
			orgSet[strings.ToLower(o)] = true
		}
		for _, allowedOrg := range cfg.AllowedOrgs {
			if orgSet[strings.ToLower(allowedOrg)] {
				return true, nil
			}
		}
	}

	// Check allowed teams ("org/team" format)
	for _, teamSpec := range cfg.AllowedTeams {
		parts := strings.SplitN(teamSpec, "/", 2)
		if len(parts) != 2 {
			continue
		}
		org, team := parts[0], parts[1]
		member, err := s.githubCheckTeamMembership(ctx, accessToken, org, team, login)
		if err != nil {
			log.Printf("[github-oauth] team check %s/%s: %v", org, team, err)
			continue
		}
		if member {
			return true, nil
		}
	}

	return false, nil
}

func (s *Server) githubFetchUserOrgs(ctx context.Context, accessToken string) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.ghBaseURL()+"/user/orgs", nil)
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch orgs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch orgs: status %d: %s", resp.StatusCode, string(body))
	}
	var orgs []struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&orgs); err != nil {
		return nil, fmt.Errorf("decode orgs: %w", err)
	}
	names := make([]string, len(orgs))
	for i, o := range orgs {
		names[i] = o.Login
	}
	return names, nil
}

func (s *Server) githubCheckTeamMembership(ctx context.Context, accessToken, org, team, login string) (bool, error) {
	apiURL := fmt.Sprintf("%s/orgs/%s/teams/%s/members/%s",
		s.ghBaseURL(),
		url.PathEscape(org),
		url.PathEscape(team),
		url.PathEscape(login),
	)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	req.Header.Set("Authorization", "token "+accessToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("team membership check: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent, nil
}
