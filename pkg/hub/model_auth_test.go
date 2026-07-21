package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func testGrokAuthState(t *testing.T, access, refresh string, expires time.Time) string {
	t.Helper()
	authData, err := json.Marshal(map[string]any{
		grokAuthIssuer + "::" + grokAuthClientID: map[string]any{
			"key":           access,
			"refresh_token": refresh,
			"expires_at":    expires.UTC().Format(time.RFC3339Nano),
			"preserved":     "metadata",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := json.Marshal(cliAuthBundle{Files: map[string]string{
		".grok/auth.json": base64.StdEncoding.EncodeToString(authData),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(bundleData)
}

func TestManagedGrokCredentialSerializesRefreshAndPersistsRotation(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	profileName := "grok-oauth"
	cfg := &types.HubConfig{
		ClawToken: "claw-token",
		LLMKeys: types.LLMKeysList{
			{Name: "grok", Provider: "grok", AuthProfile: profileName, Default: true},
		},
		ModelAuthProfiles: []*types.ModelAuthProfileConfig{
			{Name: profileName, Provider: "grok", Mode: "device", AuthState: testGrokAuthState(t, "old-access", "refresh-1", time.Now().Add(-time.Minute))},
		},
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,llm_key,status,created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		"claw-grok", "test-tenant-id", "grok claw", "grok", "connected"); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var refreshes []string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse refresh form: %v", err)
		}
		mu.Lock()
		refreshes = append(refreshes, r.Form.Get("refresh_token"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"refresh-2","expires_in":3600}`))
	}))
	defer tokenServer.Close()
	s.grokTokenEndpoint = tokenServer.URL

	ctx := context.WithValue(context.Background(), ctxTenantKey{}, "test-tenant-id")
	results := make(chan *managedModelAuthCredential, 2)
	errs := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			credential, err := s.managedGrokCredential(ctx, "claw-grok")
			results <- credential
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("managed credential: %v", err)
		}
		credential := <-results
		if credential.Access != "new-access" || credential.Provider != "xai" || credential.Expires <= time.Now().UnixMilli() {
			t.Fatalf("unexpected credential: %#v", credential)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(refreshes) != 1 || refreshes[0] != "refresh-1" {
		t.Fatalf("refresh calls = %#v, want one call with the original token", refreshes)
	}

	_, _, _, entry, _, err := decodeManagedGrokAuth(s.hubCfg.ModelAuthProfiles[0].AuthState)
	if err != nil {
		t.Fatal(err)
	}
	if got := stringFromMap(entry, "refresh_token"); got != "refresh-2" {
		t.Fatalf("persisted refresh token = %q, want refresh-2", got)
	}
	if got := stringFromMap(entry, "preserved"); got != "metadata" {
		t.Fatalf("unrelated Grok auth metadata was lost: %q", got)
	}
}

func TestManagedModelAuthCredentialRequiresClawAuth(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	req := httptest.NewRequest(http.MethodPost, "/api/internal/model-auth/credential", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/internal/model-auth/credential", nil)
	req.Header.Set("X-Claw-Token", "claw-token")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("authenticated status = %d, want %d for missing claw identity", rec.Code, http.StatusBadRequest)
	}
}

func TestCaptureModelAuthOutputStripsANSIFromURL(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("\x1b[32mhttps://auth.openai.com/codex/device_authorization\x1b[0m\n"), make(chan struct{}, 1))

	if job.URL != "https://auth.openai.com/codex/device_authorization" {
		t.Fatalf("URL = %q, want stripped device authorization URL", job.URL)
	}
	if strings.Contains(job.Output, "\x1b") || strings.Contains(job.Output, "[0m") {
		t.Fatalf("Output = %q, want ANSI escape sequences stripped", job.Output)
	}
}

func TestCaptureModelAuthOutputDoesNotTreatCodexURLAsCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Open https://auth.openai.com/codex/device_authorization\nCode: authorization\n"), make(chan struct{}, 1))

	if job.Code != "" {
		t.Fatalf("Code = %q, want no code extracted from codex URL", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsDeviceCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("User code: ABCD-EFGH\n"), make(chan struct{}, 1))

	if job.Code != "ABCD-EFGH" {
		t.Fatalf("Code = %q, want device code", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsCodexOneTimeCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}
	output := `Follow these steps to sign in with ChatGPT using device code authorization:

1. Open this link in your browser and sign in to your account
   https://auth.openai.com/codex/device

2. Enter this one-time code (expires in 15 minutes)
   2VX0-20MIV

Device codes are a common phishing target. Never share this code.
`

	s.captureModelAuthOutput(job, strings.NewReader(output), make(chan struct{}, 1))

	if job.Code != "2VX0-20MIV" {
		t.Fatalf("Code = %q, want Codex one-time code", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsNumericDeviceCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Authorization code: 123-456-789\n"), make(chan struct{}, 1))

	if job.Code != "123-456-789" {
		t.Fatalf("Code = %q, want numeric device code", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsStandaloneNineDigitCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Enter this authorization code:\n123 456 789\n"), make(chan struct{}, 1))

	if job.Code != "123456789" {
		t.Fatalf("Code = %q, want normalized 9 digit device code", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsUnlabeledNineDigitCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Open https://auth.openai.com/codex/device\n987654321\n"), make(chan struct{}, 1))

	if job.Code != "987654321" {
		t.Fatalf("Code = %q, want unlabeled 9 digit device code", job.Code)
	}
}

func TestAppendModelAuthOutputReplacesBadCodeWithRealCode(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.appendModelAuthOutput(job, "Code: authorization\n")
	s.appendModelAuthOutput(job, "Authorization code: 123-456-789\n")

	if job.Code != "123-456-789" {
		t.Fatalf("Code = %q, want real code after rejected prose token", job.Code)
	}
}

func TestCaptureModelAuthOutputExtractsURLWithoutNewline(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.captureModelAuthOutput(job, strings.NewReader("Open https://auth.openai.com/codex/device_authorization"), make(chan struct{}, 1))

	if job.URL != "https://auth.openai.com/codex/device_authorization" {
		t.Fatalf("URL = %q, want URL before process exits without newline", job.URL)
	}
}

func TestAppendModelAuthOutputExtractsSplitURL(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.appendModelAuthOutput(job, "Open https://auth.openai.com/codex/")
	s.appendModelAuthOutput(job, "device_authorization")

	if job.URL != "https://auth.openai.com/codex/device_authorization" {
		t.Fatalf("URL = %q, want URL reconstructed from streamed chunks", job.URL)
	}
}

func TestAppendModelAuthOutputExtractsOSCURL(t *testing.T) {
	s := &Server{}
	job := &modelAuthLoginJob{}

	s.appendModelAuthOutput(job, "\x1b]8;;https://auth.openai.com/codex/device_authorization\x07click here\x1b]8;;\x07")

	if job.URL != "https://auth.openai.com/codex/device_authorization" {
		t.Fatalf("URL = %q, want URL from terminal hyperlink", job.URL)
	}
	if strings.Contains(job.Output, "\x1b") {
		t.Fatalf("Output = %q, want terminal hyperlink escapes stripped", job.Output)
	}
}

func TestWriteCodexAuthFiles(t *testing.T) {
	root := t.TempDir()
	idToken := testJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-123",
		},
	})
	err := writeCodexAuthFiles(root, oauthTokenResponse{
		IDToken:      idToken,
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
	}
	readJSON(t, filepath.Join(root, ".codex", "auth.json"), &got)
	if got.AuthMode != "chatgpt" || got.Tokens.AccountID != "acct-123" {
		t.Fatalf("auth json = %#v", got)
	}
	if got.Tokens.IDToken != idToken || got.Tokens.AccessToken != "access-token" || got.Tokens.RefreshToken != "refresh-token" {
		t.Fatalf("tokens not persisted correctly: %#v", got.Tokens)
	}
}

func TestWriteGrokAuthFiles(t *testing.T) {
	root := t.TempDir()
	accessToken := testJWT(map[string]any{
		"sub":            "user-123",
		"principal_type": "User",
		"principal_id":   "principal-123",
		"team_id":        "team-123",
	})
	userInfo := map[string]any{
		"sub":        "user-123",
		"email":      "marc@example.com",
		"given_name": "Marc",
		"picture":    "users/avatar.webp",
	}

	err := writeGrokAuthFilesFromUserInfo(root, oauthTokenResponse{
		AccessToken:  accessToken,
		RefreshToken: "refresh-token",
		ExpiresIn:    3600,
	}, userInfo, time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]map[string]any
	readJSON(t, filepath.Join(root, ".grok", "auth.json"), &got)
	entry := got[grokAuthIssuer+"::"+grokAuthClientID]
	if entry["key"] != accessToken || entry["refresh_token"] != "refresh-token" {
		t.Fatalf("tokens not persisted correctly: %#v", entry)
	}
	if entry["auth_mode"] != "oidc" || entry["team_id"] != "team-123" || entry["email"] != "marc@example.com" {
		t.Fatalf("metadata not persisted correctly: %#v", entry)
	}
}

func readJSON(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func testJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]any{"alg": "none"})
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
