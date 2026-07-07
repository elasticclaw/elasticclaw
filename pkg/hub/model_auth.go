package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

type modelAuthLoginJob struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Profile   string `json:"profile"`
	Status    string `json:"status"`
	URL       string `json:"url,omitempty"`
	Code      string `json:"code,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"startedAt"`
	UpdatedAt string `json:"updatedAt"`
}

type cliAuthBundle struct {
	Files map[string]string `json:"files"`
}

var (
	modelAuthANSIRE        = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	modelAuthOSC8RE        = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	modelAuthURLRE         = regexp.MustCompile(`https?://[^\s"'\x00-\x1f\x7f]+`)
	modelAuthCodeRE        = regexp.MustCompile(`(?i:\b(?:device code|user code|verification code|code)\b)[^A-Za-z0-9]*([A-Za-z0-9][A-Za-z0-9-]{3,})\b`)
	modelAuthOneTimeCodeRE = regexp.MustCompile(`(?is)\bone-time code\b.*?\n\s*([A-Z0-9]{4,5}-[A-Z0-9]{4,5})\b`)
	modelAuth9DigitCodeRE  = regexp.MustCompile(`\b\d(?:[ -]?\d){8}\b`)
)

const (
	codexAuthIssuer   = "https://auth.openai.com"
	codexAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	grokAuthIssuer    = "https://auth.x.ai"
	grokAuthClientID  = "b1a00492-073a-47ea-816f-4c329264a828"
	grokAuthScope     = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"
)

func (s *Server) handleModelAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Profile  string `json:"profile"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Profile = strings.TrimSpace(req.Profile)
	if req.Mode == "" {
		req.Mode = "device"
	}
	if req.Profile == "" {
		req.Profile = req.Provider + "-default"
	}
	if req.Provider != "codex" && req.Provider != "grok" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "provider must be codex or grok")
		return
	}
	if req.Mode != "device" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "only device login is supported")
		return
	}

	job := &modelAuthLoginJob{
		ID:        uuid.NewString(),
		Provider:  req.Provider,
		Profile:   req.Profile,
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.modelAuthJobsMu.Lock()
	if s.modelAuthJobs == nil {
		s.modelAuthJobs = map[string]*modelAuthLoginJob{}
	}
	s.modelAuthJobs[job.ID] = job
	snapshot := snapshotModelAuthLoginJob(job)
	s.modelAuthJobsMu.Unlock()

	go s.runModelAuthLoginJob(job)
	jsonOK(w, snapshot)
}

func (s *Server) handleModelAuthLoginStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	id := r.PathValue("id")
	s.modelAuthJobsMu.Lock()
	job := s.modelAuthJobs[id]
	snapshot := snapshotModelAuthLoginJob(job)
	s.modelAuthJobsMu.Unlock()
	if job == nil {
		writeErr(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	jsonOK(w, snapshot)
}

func snapshotModelAuthLoginJob(job *modelAuthLoginJob) *modelAuthLoginJob {
	if job == nil {
		return nil
	}
	snapshot := *job
	return &snapshot
}

func (s *Server) runModelAuthLoginJob(job *modelAuthLoginJob) {
	authDir, err := os.MkdirTemp("", "elasticclaw-model-auth-*")
	if err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	defer os.RemoveAll(authDir)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if err := s.runModelAuthDeviceLogin(ctx, job, authDir); err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	bundle, err := encodeCLIAuthBundle(authDir)
	if err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	if err := s.saveModelAuthProfile(job.Provider, job.Profile, "device", bundle); err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	s.finishModelAuthJob(job, "complete", bundle, nil)
}

func (s *Server) runModelAuthDeviceLogin(ctx context.Context, job *modelAuthLoginJob, authDir string) error {
	switch job.Provider {
	case "codex":
		return s.runCodexDeviceLogin(ctx, job, authDir)
	case "grok":
		return s.runGrokDeviceLogin(ctx, job, authDir)
	default:
		return fmt.Errorf("unsupported provider %q", job.Provider)
	}
}

func (s *Server) setModelAuthDevicePrompt(job *modelAuthLoginJob, authURL, code string) {
	s.modelAuthJobsMu.Lock()
	defer s.modelAuthJobsMu.Unlock()
	job.URL = authURL
	job.Code = code
	job.Output = strings.TrimSpace(authURL + "\n" + code)
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

type codexUserCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	UserCodeAlt  string `json:"usercode"`
	Interval     string `json:"interval"`
}

type codexDeviceTokenResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func (s *Server) runCodexDeviceLogin(ctx context.Context, job *modelAuthLoginJob, authDir string) error {
	client := http.DefaultClient
	var uc codexUserCodeResponse
	if err := postJSON(ctx, client, codexAuthIssuer+"/api/accounts/deviceauth/usercode", map[string]string{
		"client_id": codexAuthClientID,
	}, &uc); err != nil {
		return fmt.Errorf("request Codex device code: %w", err)
	}
	userCode := uc.UserCode
	if userCode == "" {
		userCode = uc.UserCodeAlt
	}
	interval := parsePositiveInt64(uc.Interval, 5)
	if uc.DeviceAuthID == "" || userCode == "" {
		return fmt.Errorf("Codex device code response missing device_auth_id or user_code")
	}
	s.setModelAuthDevicePrompt(job, codexAuthIssuer+"/codex/device", userCode)

	var code codexDeviceTokenResponse
	if err := pollCodexDeviceToken(ctx, client, uc.DeviceAuthID, userCode, time.Duration(interval)*time.Second, &code); err != nil {
		return err
	}
	if code.AuthorizationCode == "" || code.CodeVerifier == "" {
		return fmt.Errorf("Codex device token response missing authorization code or verifier")
	}

	var token oauthTokenResponse
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code.AuthorizationCode)
	form.Set("redirect_uri", codexAuthIssuer+"/deviceauth/callback")
	form.Set("client_id", codexAuthClientID)
	form.Set("code_verifier", code.CodeVerifier)
	if err := postForm(ctx, client, codexAuthIssuer+"/oauth/token", form, &token); err != nil {
		return fmt.Errorf("exchange Codex authorization code: %w", err)
	}
	if token.IDToken == "" || token.AccessToken == "" || token.RefreshToken == "" {
		return fmt.Errorf("Codex token response missing id, access, or refresh token")
	}
	return writeCodexAuthFiles(authDir, token)
}

func pollCodexDeviceToken(ctx context.Context, client *http.Client, deviceAuthID, userCode string, interval time.Duration, out *codexDeviceTokenResponse) error {
	deadline := time.NewTimer(15 * time.Minute)
	defer deadline.Stop()
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		var resp codexDeviceTokenResponse
		status, body, err := postJSONStatus(ctx, client, codexAuthIssuer+"/api/accounts/deviceauth/token", map[string]string{
			"device_auth_id": deviceAuthID,
			"user_code":      userCode,
		}, &resp)
		if err != nil {
			return fmt.Errorf("poll Codex device token: %w", err)
		}
		if status >= 200 && status < 300 {
			*out = resp
			return nil
		}
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return fmt.Errorf("Codex device auth failed with status %d: %s", status, strings.TrimSpace(string(body)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Codex device auth timed out after 15 minutes")
		case <-time.After(interval):
		}
	}
}

type grokDeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
	Error                   string `json:"error"`
	Description             string `json:"error_description"`
}

func (s *Server) runGrokDeviceLogin(ctx context.Context, job *modelAuthLoginJob, authDir string) error {
	client := http.DefaultClient
	var dc grokDeviceCodeResponse
	form := url.Values{}
	form.Set("client_id", grokAuthClientID)
	form.Set("scope", grokAuthScope)
	if err := postForm(ctx, client, grokAuthIssuer+"/oauth2/device/code", form, &dc); err != nil {
		return fmt.Errorf("request Grok device code: %w", err)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" {
		return fmt.Errorf("Grok device code response missing device_code or user_code")
	}
	authURL := dc.VerificationURIComplete
	if authURL == "" {
		authURL = dc.VerificationURI
	}
	s.setModelAuthDevicePrompt(job, authURL, dc.UserCode)

	var token oauthTokenResponse
	if err := pollGrokDeviceToken(ctx, client, dc.DeviceCode, time.Duration(dc.Interval)*time.Second, &token); err != nil {
		return err
	}
	if token.AccessToken == "" || token.RefreshToken == "" {
		return fmt.Errorf("Grok token response missing access or refresh token")
	}
	return writeGrokAuthFiles(ctx, client, authDir, token)
}

func pollGrokDeviceToken(ctx context.Context, client *http.Client, deviceCode string, interval time.Duration, out *oauthTokenResponse) error {
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		form := url.Values{}
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
		form.Set("device_code", deviceCode)
		form.Set("client_id", grokAuthClientID)
		var token oauthTokenResponse
		status, body, err := postFormStatus(ctx, client, grokAuthIssuer+"/oauth2/token", form, &token)
		if err != nil {
			return fmt.Errorf("poll Grok device token: %w", err)
		}
		if status >= 200 && status < 300 {
			*out = token
			return nil
		}
		if token.Error != "authorization_pending" && token.Error != "slow_down" {
			return fmt.Errorf("Grok device auth failed with status %d: %s", status, strings.TrimSpace(string(body)))
		}
		if token.Error == "slow_down" {
			interval += 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Grok device auth timed out after 30 minutes")
		case <-time.After(interval):
		}
	}
}

func writeCodexAuthFiles(root string, token oauthTokenResponse) error {
	authPath := filepath.Join(root, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0700); err != nil {
		return err
	}
	auth := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      token.IDToken,
			"access_token":  token.AccessToken,
			"refresh_token": token.RefreshToken,
			"account_id":    jwtStringClaim(token.IDToken, "https://api.openai.com/auth", "chatgpt_account_id"),
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339Nano),
	}
	return writePrettyJSON(authPath, auth, 0600)
}

func writeGrokAuthFiles(ctx context.Context, client *http.Client, root string, token oauthTokenResponse) error {
	userInfo, err := getGrokUserInfo(ctx, client, token.AccessToken)
	if err != nil {
		return err
	}
	return writeGrokAuthFilesFromUserInfo(root, token, userInfo, time.Now().UTC())
}

func writeGrokAuthFilesFromUserInfo(root string, token oauthTokenResponse, userInfo map[string]any, now time.Time) error {
	expiresAt := now.UTC().Add(time.Duration(token.ExpiresIn) * time.Second)
	if token.ExpiresIn <= 0 {
		expiresAt = now.UTC().Add(6 * time.Hour)
	}
	claims := jwtClaims(token.AccessToken)
	userID := firstNonEmptyString(stringFromMap(userInfo, "sub"), stringFromMap(claims, "sub"))
	principalID := firstNonEmptyString(stringFromMap(claims, "principal_id"), userID)
	principalType := firstNonEmptyString(stringFromMap(claims, "principal_type"), "User")
	entry := map[string]any{
		"key":                           token.AccessToken,
		"auth_mode":                     "oidc",
		"create_time":                   now.UTC().Format(time.RFC3339Nano),
		"user_id":                       userID,
		"email":                         stringFromMap(userInfo, "email"),
		"first_name":                    firstNonEmptyString(stringFromMap(userInfo, "given_name"), stringFromMap(userInfo, "name")),
		"profile_image_asset_id":        stringFromMap(userInfo, "picture"),
		"principal_type":                principalType,
		"principal_id":                  principalID,
		"team_id":                       stringFromMap(claims, "team_id"),
		"coding_data_retention_opt_out": false,
		"refresh_token":                 token.RefreshToken,
		"expires_at":                    expiresAt.Format(time.RFC3339Nano),
		"oidc_issuer":                   grokAuthIssuer,
		"oidc_client_id":                grokAuthClientID,
	}
	authPath := filepath.Join(root, ".grok", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0700); err != nil {
		return err
	}
	return writePrettyJSON(authPath, map[string]any{grokAuthIssuer + "::" + grokAuthClientID: entry}, 0600)
}

func getGrokUserInfo(ctx context.Context, client *http.Client, accessToken string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, grokAuthIssuer+"/oauth2/userinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Grok userinfo failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, body any, out any) error {
	status, respBody, err := postJSONStatus(ctx, client, endpoint, body, out)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("status %d: %s", status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func postJSONStatus(ctx context.Context, client *http.Client, endpoint string, body any, out any) (int, []byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(data)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, respBody, err
		}
	}
	return resp.StatusCode, respBody, nil
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, out any) error {
	status, respBody, err := postFormStatus(ctx, client, endpoint, form, out)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("status %d: %s", status, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func postFormStatus(ctx context.Context, client *http.Client, endpoint string, form url.Values, out any) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, respBody, err
		}
	}
	return resp.StatusCode, respBody, nil
}

func writePrettyJSON(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, mode)
}

func parsePositiveInt64(value string, fallback int64) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func jwtStringClaim(jwt string, objectClaim string, nestedKey string) string {
	claims := jwtClaims(jwt)
	if objectClaim == "" {
		return stringFromMap(claims, nestedKey)
	}
	nested, _ := claims[objectClaim].(map[string]any)
	return stringFromMap(nested, nestedKey)
}

func jwtClaims(jwt string) map[string]any {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(data, &claims); err != nil {
		return nil
	}
	return claims
}

func stringFromMap(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	switch value := values[key].(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	default:
		return ""
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *Server) captureModelAuthOutput(job *modelAuthLoginJob, r io.Reader, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.appendModelAuthOutput(job, string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) appendModelAuthOutput(job *modelAuthLoginJob, raw string) {
	chunk := normalizeModelAuthOutput(raw)
	s.modelAuthJobsMu.Lock()
	defer s.modelAuthJobsMu.Unlock()
	job.Output += chunk
	if match := modelAuthURLRE.FindString(raw); match != "" {
		updateModelAuthURL(job, match)
	}
	if match := modelAuthURLRE.FindString(job.Output); match != "" {
		updateModelAuthURL(job, match)
	}
	if code := extractModelAuthCode(job.Output); code != "" {
		job.Code = code
	}
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

func updateModelAuthURL(job *modelAuthLoginJob, url string) {
	url = strings.TrimRight(url, ".,)")
	if len(url) > len(job.URL) {
		job.URL = url
	}
}

func extractModelAuthCode(output string) string {
	if match := modelAuthOneTimeCodeRE.FindStringSubmatch(output); len(match) == 2 {
		return match[1]
	}
	matches := modelAuthCodeRE.FindAllStringSubmatch(output, -1)
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		code := strings.Trim(match[1], ".,)")
		if isValidModelAuthCode(code) {
			return code
		}
	}
	if match := modelAuth9DigitCodeRE.FindString(output); match != "" {
		return strings.NewReplacer(" ", "", "-", "").Replace(match)
	}
	return ""
}

func isValidModelAuthCode(code string) bool {
	if len(code) < 4 {
		return false
	}
	lower := strings.ToLower(code)
	switch lower {
	case "authorization", "authenticate", "authentication", "login", "device", "verify", "verification":
		return false
	}
	hasDigit := false
	hasHyphen := false
	hasUpper := false
	for _, r := range code {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '-':
			hasHyphen = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			// Lowercase-only English words are usually CLI prose, not device codes.
		default:
			return false
		}
	}
	return hasDigit || hasHyphen || hasUpper
}

func normalizeModelAuthOutput(line string) string {
	line = modelAuthOSC8RE.ReplaceAllString(line, "")
	line = modelAuthANSIRE.ReplaceAllString(line, "")
	var b strings.Builder
	b.Grow(len(line))
	for _, r := range line {
		if (r >= 0 && r < 32 && r != '\n' && r != '\t') || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Server) finishModelAuthJob(job *modelAuthLoginJob, status, _ string, err error) {
	s.modelAuthJobsMu.Lock()
	defer s.modelAuthJobsMu.Unlock()
	job.Status = status
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		job.Error = err.Error()
	}
}

func encodeCLIAuthBundle(root string) (string, error) {
	bundle := cliAuthBundle{Files: map[string]string{}}
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".npm" || name == ".cache" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > 2*1024*1024 {
			return nil
		}
		total += info.Size()
		if total > 10*1024*1024 {
			return fmt.Errorf("auth bundle exceeds 10MiB")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		bundle.Files[filepath.ToSlash(rel)] = base64.StdEncoding.EncodeToString(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func (s *Server) saveModelAuthProfile(provider, name, mode, authState string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	updatedCfg := *s.hubCfg
	profiles := make([]*types.ModelAuthProfileConfig, len(updatedCfg.ModelAuthProfiles))
	for i, profile := range updatedCfg.ModelAuthProfiles {
		if profile == nil {
			continue
		}
		copy := *profile
		profiles[i] = &copy
	}
	var found *types.ModelAuthProfileConfig
	for _, profile := range profiles {
		if profile != nil && profile.Name == name {
			found = profile
			break
		}
	}
	if found == nil {
		found = &types.ModelAuthProfileConfig{Name: name}
		profiles = append(profiles, found)
	}
	found.Provider = provider
	found.Mode = mode
	found.AuthState = authState
	found.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	updatedCfg.ModelAuthProfiles = profiles
	if err := config.SaveHubConfig(&updatedCfg); err != nil {
		return err
	}
	s.hubCfg = &updatedCfg
	return nil
}

func buildModelAuthEnv(cfg *types.HubConfig, selectedKeyName string) string {
	if cfg == nil {
		return ""
	}
	key := resolveActiveKey(cfg.LLMKeys, selectedKeyName)
	if key == nil || key.AuthProfile == "" {
		return ""
	}
	for _, profile := range cfg.ModelAuthProfiles {
		if profile == nil || profile.Name != key.AuthProfile || profile.AuthState == "" {
			continue
		}
		if profile.Provider != key.Provider {
			continue
		}
		return fmt.Sprintf("export ELASTICCLAW_MODEL_AUTH_PROVIDER=%q\nexport ELASTICCLAW_MODEL_AUTH_STATE=%q\n", profile.Provider, profile.AuthState)
	}
	return ""
}

func buildModelAuthRestoreShell(modelAuthEnv string) string {
	if strings.TrimSpace(modelAuthEnv) == "" {
		return ""
	}
	return modelAuthEnv + `
python3 <<'PYEOF'
import base64, json, os
state = os.environ.get('ELASTICCLAW_MODEL_AUTH_STATE', '')
if state:
    bundle = json.loads(base64.b64decode(state).decode())
    home = os.path.expanduser('~')
    for rel, encoded in bundle.get('files', {}).items():
        clean = os.path.normpath(rel)
        if clean == '.' or clean == '..' or clean.startswith('../') or os.path.isabs(clean):
            continue
        path = os.path.join(home, clean)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, 'wb') as f:
            f.write(base64.b64decode(encoded))
        os.chmod(path, 0o600)
PYEOF
`
}
