package hub

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
