package hub

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestHandleClawPreviewReadyRequiresMatchingBridgeIdentity(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "user-token",
		ClawToken: "claw-token",
	}, "", "", "")
	s.previewDetachScheduleOverride = func(string) {}
	var tenantID string
	if err := db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, preview_port, preview_url, created_at) VALUES(?,?,?,?,?,?,?)`,
		"preview-claw", tenantID, "preview", "connected", 3000, "https://preview.example", time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	request := func(bridgeClawID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/claws/preview-claw/preview/ready", nil)
		req.Header.Set("X-Claw-Token", "claw-token")
		req.Header.Set("X-ElasticClaw-Claw-ID", bridgeClawID)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	if rr := request("different-claw"); rr.Code != http.StatusForbidden {
		t.Fatalf("identity mismatch status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr := request("preview-claw"); rr.Code != http.StatusOK {
		t.Fatalf("ready status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var ready bool
	if err := db.QueryRow(`SELECT preview_ready FROM claws WHERE id='preview-claw'`).Scan(&ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if !ready {
		t.Fatal("preview_ready was not persisted")
	}
	var linkMessage, format string
	if err := db.QueryRow(
		`SELECT content, format FROM messages WHERE claw_id='preview-claw' AND role='hub' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&linkMessage, &format); err != nil {
		t.Fatalf("read preview link message: %v", err)
	}
	if linkMessage != "Preview ready: [Open QA preview](https://preview.example/)" || format != "markdown" {
		t.Fatalf("preview link message = %q (%q)", linkMessage, format)
	}
}

func TestHandleClawPreviewReadyUsesVerifiedSameOriginRoute(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "user-token",
		ClawToken: "claw-token",
	}, "", "", "")
	s.previewDetachScheduleOverride = func(string) {}
	var tenantID string
	if err := db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, preview_port, preview_url, created_at) VALUES(?,?,?,?,?,?,?)`,
		"preview-route", tenantID, "preview", "connected", 3000,
		"https://preview.example/?signed=token", time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	request := func(payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(
			http.MethodPost,
			"/api/claws/preview-route/preview/ready",
			bytes.NewBufferString(payload),
		)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Claw-Token", "claw-token")
		req.Header.Set("X-ElasticClaw-Claw-ID", "preview-route")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	if rr := request(`{"path":"https://evil.example/setup"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("external route status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if rr := request(`{"path":"/setup?task=123"}`); rr.Code != http.StatusOK {
		t.Fatalf("same-origin route status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var previewURL string
	if err := db.QueryRow(`SELECT preview_url FROM claws WHERE id='preview-route'`).Scan(&previewURL); err != nil {
		t.Fatalf("read preview URL: %v", err)
	}
	if previewURL != "https://preview.example/setup?signed=token&task=123" {
		t.Fatalf("preview URL = %q", previewURL)
	}
}

func TestDetachPreviewAgentStopsAgentAndRetainsSandbox(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	var tenantID string
	if err := db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	_, err := db.Exec(
		`INSERT INTO claws(
			id, tenant_id, name, provider, provider_id, status,
			preview_port, preview_url, preview_ready, preview_ttl_seconds, created_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		"preview-detach", tenantID, "preview", "docker", "container-123", "connected",
		3000, "http://127.0.0.1:45678/setup", 1, 60, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	var stopped bool
	s.previewStopOverride = func(_ context.Context, provider, providerID, clawID string) error {
		stopped = provider == "docker" && providerID == "container-123" && clawID == "preview-detach"
		return nil
	}
	before := time.Now().UnixMilli()
	if err := s.detachPreviewAgent(context.Background(), "preview-detach"); err != nil {
		t.Fatalf("detach preview: %v", err)
	}

	var status, providerID string
	var expiresAt int64
	if err := db.QueryRow(
		`SELECT status, provider_id, preview_expires_at FROM claws WHERE id='preview-detach'`,
	).Scan(&status, &providerID, &expiresAt); err != nil {
		t.Fatalf("read detached preview: %v", err)
	}
	if !stopped {
		t.Fatal("agent processes were not stopped")
	}
	if status != "preview" {
		t.Fatalf("status = %q, want preview", status)
	}
	if providerID != "container-123" {
		t.Fatalf("provider_id = %q; sandbox should be retained", providerID)
	}
	if expiresAt < before+59_000 || expiresAt > before+61_000 {
		t.Fatalf("preview_expires_at = %d, want about one minute after %d", expiresAt, before)
	}
}

func TestDetachedPreviewCannotMintGitHubToken(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		ClawToken: "claw-token",
	}, "", "", "")
	var tenantID string
	if err := db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, github_repos, created_at)
		 VALUES(?,?,?,?,?,?)`,
		"preview-no-token", tenantID, "preview", "preview", "[]", time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/github/token/preview-no-token?claw_token=claw-token",
		nil,
	)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusGone {
		t.Fatalf("status = %d, body = %s; want 410", rr.Code, rr.Body.String())
	}
}

func TestHandleClawPreviewStartUsesClawProvider(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "user-token",
		ClawToken: "claw-token",
	}, "", "", "")
	var tenantID string
	if err := db.QueryRow(`SELECT id FROM tenants LIMIT 1`).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, provider, provider_id, status, preview_port, preview_url, preview_ready, created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"preview-claw", tenantID, "preview", "docker", "container-123", "connected", 3000, "http://127.0.0.1:45678", 1, time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	var gotProvider, gotProviderID, gotCwd, gotCommand string
	s.previewStartOverride = func(_ context.Context, provider, providerID, cwd, command string) error {
		gotProvider, gotProviderID, gotCwd, gotCommand = provider, providerID, cwd, command
		return nil
	}
	body := bytes.NewBufferString(`{"cwd":"/workspace/repo","command":"npm run dev -- --host 0.0.0.0 --port 3000"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/claws/preview-claw/preview/start", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claw-Token", "claw-token")
	req.Header.Set("X-ElasticClaw-Claw-ID", "preview-claw")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if gotProvider != "docker" || gotProviderID != "container-123" ||
		gotCwd != "/workspace/repo" || gotCommand != "npm run dev -- --host 0.0.0.0 --port 3000" {
		t.Fatalf("unexpected preview start target: %q %q %q %q", gotProvider, gotProviderID, gotCwd, gotCommand)
	}
	var ready bool
	if err := db.QueryRow(`SELECT preview_ready FROM claws WHERE id='preview-claw'`).Scan(&ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if ready {
		t.Fatal("starting a preview must reset preview_ready")
	}
}
