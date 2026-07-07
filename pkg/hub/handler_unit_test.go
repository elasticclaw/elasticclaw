package hub

// Direct unit tests for the main HTTP handlers, using the in-memory SQLite
// store from NewTestServerWithConfig. No Daytona/Docker or network access is
// required (re-architecture plan item 3.7).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func linearSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func withTenant(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxTenantKey{}, "test-tenant-id"))
}

func TestHandleCreateClawValidation(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Providers: map[string]types.ProviderConfig{"mock": {}},
	}, "", "", "")

	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "invalid json", body: "{not json", want: http.StatusBadRequest},
		{name: "missing name", body: `{"provider":"mock"}`, want: http.StatusBadRequest},
		{name: "missing provider", body: `{"name":"claw-a"}`, want: http.StatusBadRequest},
		{name: "unconfigured provider", body: `{"name":"claw-a","provider":"daytona"}`, want: http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := withTenant(httptest.NewRequest(http.MethodPost, "/api/claws", strings.NewReader(tt.body)))
			rec := httptest.NewRecorder()
			s.handleClaws(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestHandleCreateClawPersistsAndReturnsAccepted(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Providers: map[string]types.ProviderConfig{"mock": {}},
	}, "", "", "")

	body := `{"name":"claw-a","provider":"mock","template_name":"tpl"}`
	req := withTenant(httptest.NewRequest(http.MethodPost, "/api/claws", strings.NewReader(body)))
	rec := httptest.NewRecorder()
	s.handleClaws(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	var claw types.Claw
	if err := json.NewDecoder(rec.Body).Decode(&claw); err != nil {
		t.Fatal(err)
	}
	if claw.ID == "" || claw.Name != "claw-a" || claw.Status != "provisioning" {
		t.Fatalf("unexpected response claw: %+v", claw)
	}

	// The row is pre-registered synchronously; provisioning itself is async
	// (and fails for the unroutable "mock" provider), so only assert the
	// stable columns.
	var name, provider string
	if err := db.QueryRow(`SELECT name, provider FROM claws WHERE id=? AND tenant_id=?`, claw.ID, "test-tenant-id").Scan(&name, &provider); err != nil {
		t.Fatalf("claw row not persisted: %v", err)
	}
	if name != "claw-a" || provider != "mock" {
		t.Fatalf("persisted row = (%q, %q), want (claw-a, mock)", name, provider)
	}

	// Wait for the async provisioning goroutine to fail and mark the claw as
	// error, so it does not touch the test DB after cleanup.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		_ = db.QueryRow(`SELECT status FROM claws WHERE id=?`, claw.ID).Scan(&status)
		if status == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("claw status = %q, expected async provisioning failure to set error", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandleMessagesPostPersistsUserMessage(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at, status) VALUES(?,?,?,?,datetime('now'),?)`,
		"claw-1", "test-tenant-id", "claw 1", `[]`, "connected",
	); err != nil {
		t.Fatal(err)
	}

	req := withTenant(httptest.NewRequest(http.MethodPost, "/api/messages/claw-1", strings.NewReader(`{"content":"hello agent"}`)))
	rec := httptest.NewRecorder()
	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var msg types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	if msg.Role != "user" || msg.Content != "hello agent" || msg.ClawID != "claw-1" {
		t.Fatalf("unexpected response message: %+v", msg)
	}

	var content, role string
	if err := db.QueryRow(`SELECT content, role FROM messages WHERE id=?`, msg.ID).Scan(&content, &role); err != nil {
		t.Fatalf("message row not persisted: %v", err)
	}
	if content != "hello agent" || role != "user" {
		t.Fatalf("persisted message = (%q, %q)", content, role)
	}

	t.Run("empty content is rejected", func(t *testing.T) {
		req := withTenant(httptest.NewRequest(http.MethodPost, "/api/messages/claw-1", strings.NewReader(`{"content":""}`)))
		rec := httptest.NewRecorder()
		s.handleMessages(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

func TestHandleClawsListMarksStaleConnectedAsOffline(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	seed := []struct{ id, status string }{
		{"claw-stale", "connected"}, // no live WS conn: must be reported offline
		{"claw-prov", "provisioning"},
		{"claw-err", "error"},
	}
	for _, c := range seed {
		if _, err := db.Exec(
			`INSERT INTO claws(id, tenant_id, name, tags, created_at, status) VALUES(?,?,?,?,datetime('now'),?)`,
			c.id, "test-tenant-id", c.id, `[]`, c.status,
		); err != nil {
			t.Fatal(err)
		}
	}

	req := withTenant(httptest.NewRequest(http.MethodGet, "/api/claws", nil))
	rec := httptest.NewRecorder()
	s.handleClaws(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var claws []types.Claw
	if err := json.NewDecoder(rec.Body).Decode(&claws); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range claws {
		got[c.ID] = string(c.Status)
	}
	want := map[string]string{
		"claw-stale": "offline",
		"claw-prov":  "provisioning",
		"claw-err":   "error",
	}
	for id, status := range want {
		if got[id] != status {
			t.Fatalf("claw %s status = %q, want %q (all: %v)", id, got[id], status, got)
		}
	}
}

func TestSettingsGetMasksLLMKeySecrets(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	const secret = "sk-super-secret-value"
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		LLMKeys: types.LLMKeysList{
			{Name: "main", Provider: "anthropic", APIKey: secret, Default: true},
		},
	}, "", "", "")

	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()
	s.handleSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("settings response leaks the raw LLM API key")
	}
	var view SettingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.LLMKeys) != 1 || !view.LLMKeys[0].KeySet {
		t.Fatalf("expected one configured LLM key, got %+v", view.LLMKeys)
	}
}

func TestSettingsPatchUpsertsLLMKeyAndPersists(t *testing.T) {
	cfgPath := t.TempDir() + "/hub.yaml"
	t.Setenv("ELASTICCLAW_HUB_CONFIG", cfgPath)
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")

	body := `{"llmKeys":[{"name":"main","provider":"anthropic","apiKey":"sk-new-key","default":true}]}`
	req := httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// In-memory config updated
	if len(s.hubCfg.LLMKeys) != 1 || s.hubCfg.LLMKeys[0].Name != "main" ||
		s.hubCfg.LLMKeys[0].APIKey != "sk-new-key" || !s.hubCfg.LLMKeys[0].Default {
		t.Fatalf("in-memory LLM keys = %+v", s.hubCfg.LLMKeys)
	}

	// Persisted to hub.yaml before being applied
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("hub.yaml not written: %v", err)
	}
	if !strings.Contains(string(data), "sk-new-key") {
		t.Fatalf("hub.yaml does not contain the new key:\n%s", data)
	}

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, "/api/settings", nil)
		rec := httptest.NewRecorder()
		s.handleSettings(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})
}

func TestHandleLinearWebhook(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	const secret = "linear-hmac-secret"
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Integrations: &types.IntegrationsConfig{
			Linear: []*types.LinearIntegrationConfig{
				{Workspace: "acme", Token: "lin_api_x", WebhookSecret: secret},
			},
		},
	}, "", "", "")

	post := func(body []byte, sig string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/integrations/linear/webhook", strings.NewReader(string(body)))
		if sig != "" {
			req.Header.Set("Linear-Signature", sig)
		}
		rec := httptest.NewRecorder()
		s.handleLinearWebhook(rec, req)
		return rec
	}

	t.Run("method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/integrations/linear/webhook", nil)
		rec := httptest.NewRecorder()
		s.handleLinearWebhook(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})

	body := []byte(`{"action":"update","type":"Comment","data":{"id":"1"}}`)

	t.Run("missing signature is rejected", func(t *testing.T) {
		if rec := post(body, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong signature is rejected", func(t *testing.T) {
		if rec := post(body, linearSignature("other-secret", body)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("valid signature non-issue event is acknowledged", func(t *testing.T) {
		if rec := post(body, linearSignature(secret, body)); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("invalid payload with valid signature is rejected", func(t *testing.T) {
		bad := []byte(`{not json`)
		if rec := post(bad, linearSignature(secret, bad)); rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}
