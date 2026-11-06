package hub

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func newAPITokenTestServer(t *testing.T) *Server {
	t.Helper()
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "hub-token",
		Auth: &types.AuthConfig{
			SessionSecret: "api-token-test-secret",
			Access:        &types.AccessConfig{Admins: []string{"Alice", "bob"}},
		},
	}, "", "", "")
	return s
}

func apiTokenRequest(t *testing.T, s *Server, login, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	session := "hub-token"
	if login != "" {
		var err error
		session, err = signGitHubSession("api-token-test-secret", login, "", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+session)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

type createdAPIToken struct {
	apiTokenView
	Token string `json:"token"`
}

func createAPITokenForTest(t *testing.T, s *Server, login, body string) createdAPIToken {
	t.Helper()
	w := apiTokenRequest(t, s, login, http.MethodPost, "/api/settings/api-tokens", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("creation response must not be cached")
	}
	var item createdAPIToken
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func listAPITokensForTest(t *testing.T, s *Server, login string) []apiTokenView {
	t.Helper()
	w := apiTokenRequest(t, s, login, http.MethodGet, "/api/settings/api-tokens", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Tokens []apiTokenView `json:"tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Tokens == nil || strings.Contains(w.Body.String(), `"token":`) || strings.Contains(w.Body.String(), "token_hash") {
		t.Fatalf("invalid list response: %s", w.Body.String())
	}
	return response.Tokens
}

func TestAPITokensCreateListRevoke(t *testing.T) {
	s := newAPITokenTestServer(t)
	if items := listAPITokensForTest(t, s, "alice"); len(items) != 0 {
		t.Fatalf("initial tokens = %v", items)
	}
	item := createAPITokenForTest(t, s, "ALICE", `{"name":"  Claude Code  ","expires_in_days":30}`)
	id, err := hex.DecodeString(item.ID)
	if err != nil || len(id) != 16 {
		t.Fatalf("invalid ID: %q", item.ID)
	}
	secret, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(item.Token, "ecf_"))
	if err != nil || len(secret) != 32 || !strings.HasPrefix(item.Token, "ecf_") {
		t.Fatal("invalid token format")
	}
	if item.Name != "Claude Code" || item.Prefix != item.Token[:12] || item.Status != "active" || item.CreatedAt.IsZero() || item.LastUsedAt != nil || item.RevokedAt != nil {
		t.Fatalf("unexpected item: %+v", item.apiTokenView)
	}
	if item.ExpiresAt == nil || item.ExpiresAt.Sub(item.CreatedAt) != 30*24*time.Hour {
		t.Fatal("incorrect expiration")
	}
	var owner, hash, prefix string
	if err := s.db.QueryRow(`SELECT owner_login, token_hash, token_prefix FROM api_tokens WHERE id = ?`, item.ID).Scan(&owner, &hash, &prefix); err != nil {
		t.Fatal(err)
	}
	if owner != "ALICE" || hash != apiTokenHash(item.Token) || prefix != item.Prefix || strings.Contains(hash, item.Token) {
		t.Fatal("unexpected stored token")
	}
	newest := createAPITokenForTest(t, s, "alice", `{"name":"Never expires","expires_in_days":null}`)
	// Ensure ordering is verified independently of wall-clock timing.
	if _, err := s.db.Exec(`UPDATE api_tokens SET created_at = created_at - 10 WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	items := listAPITokensForTest(t, s, "aLiCe")
	if len(items) != 2 || items[0].ID != newest.ID || items[0].ExpiresAt != nil || items[1].ID != item.ID {
		t.Fatalf("unexpected list: %+v", items)
	}
	var revokedAt time.Time
	for i := 0; i < 2; i++ {
		w := apiTokenRequest(t, s, "alice", http.MethodDelete, "/api/settings/api-tokens/"+item.ID, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("revoke status = %d", w.Code)
		}
		items = listAPITokensForTest(t, s, "alice")
		if len(items) != 2 || items[1].Status != "revoked" || items[1].RevokedAt == nil {
			t.Fatal("revoked token should remain listed")
		}
		if i == 1 && !revokedAt.Equal(*items[1].RevokedAt) {
			t.Fatal("idempotent revoke changed timestamp")
		}
		revokedAt = *items[1].RevokedAt
	}
}

func TestAPITokensOwnerIsolation(t *testing.T) {
	s := newAPITokenTestServer(t)
	alice := createAPITokenForTest(t, s, "alice", `{"name":"Alice"}`)
	bob := createAPITokenForTest(t, s, "bob", `{"name":"Bob"}`)
	for login, own := range map[string]createdAPIToken{"alice": alice, "bob": bob} {
		items := listAPITokensForTest(t, s, login)
		if len(items) != 1 || items[0].ID != own.ID {
			t.Fatalf("%s can see another owner's tokens", login)
		}
	}
	for _, id := range []string{alice.ID, "missing"} {
		w := apiTokenRequest(t, s, "bob", http.MethodDelete, "/api/settings/api-tokens/"+id, "")
		if w.Code != http.StatusNotFound || w.Body.String() != "{\"error\":\"token not found\"}\n" {
			t.Fatalf("non-owned revoke: %d %s", w.Code, w.Body.String())
		}
	}
	if _, err := s.authenticateAPIToken(context.Background(), alice.Token); err != nil {
		t.Fatal("another admin revoked Alice's token")
	}
}

func TestAPITokensValidationAndAuthorization(t *testing.T) {
	s := newAPITokenTestServer(t)
	for _, expires := range []string{"0", "1", "-7", "365", "30.5", "30.0", `"30"`, "true", "[]", "{}"} {
		w := apiTokenRequest(t, s, "alice", http.MethodPost, "/api/settings/api-tokens", `{"name":"test","expires_in_days":`+expires+`}`)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expires %s: status %d", expires, w.Code)
		}
	}
	for _, name := range []string{"", "   ", strings.Repeat("é", 101)} {
		body, _ := json.Marshal(map[string]any{"name": name})
		w := apiTokenRequest(t, s, "alice", http.MethodPost, "/api/settings/api-tokens", string(body))
		if w.Code != http.StatusBadRequest {
			t.Errorf("invalid name status %d", w.Code)
		}
	}
	for _, days := range []string{"7", "30", "90", "180", "null"} {
		createAPITokenForTest(t, s, "alice", `{"name":"test","expires_in_days":`+days+`}`)
	}
	body, _ := json.Marshal(map[string]any{"name": strings.Repeat("é", 100)})
	createAPITokenForTest(t, s, "alice", string(body))
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/settings/api-tokens", ""},
		{http.MethodPost, "/api/settings/api-tokens", `{"name":"test"}`},
		{http.MethodDelete, "/api/settings/api-tokens/missing", ""},
	} {
		for _, login := range []string{"", "non-admin"} {
			w := apiTokenRequest(t, s, login, tc.method, tc.path, tc.body)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s login %q: status %d", tc.method, tc.path, login, w.Code)
			}
			if login == "" && w.Body.String() != "{\"error\":\"API tokens require GitHub sign-in\"}\n" {
				t.Errorf("password session body: %s", w.Body.String())
			}
		}
	}
}

func TestAuthenticateAPITokenRejections(t *testing.T) {
	for _, scenario := range []string{"revoked", "expired", "removed-admin", "malformed", "unknown", "no-prefix"} {
		t.Run(scenario, func(t *testing.T) {
			s := newAPITokenTestServer(t)
			item := createAPITokenForTest(t, s, "alice", `{"name":"test"}`)
			raw := item.Token
			switch scenario {
			case "revoked":
				w := apiTokenRequest(t, s, "alice", http.MethodDelete, "/api/settings/api-tokens/"+item.ID, "")
				if w.Code != http.StatusNoContent {
					t.Fatal(w.Code)
				}
			case "expired":
				if _, err := s.db.Exec(`UPDATE api_tokens SET expires_at = ? WHERE id = ?`, now().Unix(), item.ID); err != nil {
					t.Fatal(err)
				}
				if items := listAPITokensForTest(t, s, "alice"); items[0].Status != "expired" {
					t.Fatal("expected expired status")
				}
			case "removed-admin":
				s.mu.Lock()
				s.hubCfg.Auth.Access.Admins = []string{"bob"}
				s.mu.Unlock()
			case "malformed":
				raw = "ecf_invalid!"
			case "unknown":
				raw = "ecf_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			case "no-prefix":
				raw = strings.TrimPrefix(raw, "ecf_")
			}
			identity, err := s.authenticateAPIToken(context.Background(), raw)
			if err != errInvalidAPIToken || identity != (apiTokenIdentity{}) {
				t.Fatalf("rejection = %+v, %v", identity, err)
			}
			var used sql.NullInt64
			if err := s.db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id = ?`, item.ID).Scan(&used); err != nil {
				t.Fatal(err)
			}
			if used.Valid {
				t.Fatal("rejected token recorded usage")
			}
		})
	}
}

func TestAuthenticateAPITokenLastUsedThrottling(t *testing.T) {
	s := newAPITokenTestServer(t)
	item := createAPITokenForTest(t, s, "alice", `{"name":"test"}`)
	authenticate := func() {
		t.Helper()
		identity, err := s.authenticateAPIToken(context.Background(), item.Token)
		if err != nil || identity.TokenID != item.ID || identity.OwnerLogin != "alice" {
			t.Fatalf("identity = %+v, error = %v", identity, err)
		}
	}
	authenticate()
	var first int64
	if err := s.db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id = ?`, item.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first < now().Unix()-5 {
		t.Fatal("first use was not recorded")
	}
	// Count actual writes, even if they would set the same timestamp.
	if _, err := s.db.Exec(`CREATE TABLE api_token_write_count (n INTEGER); INSERT INTO api_token_write_count VALUES (0);
		CREATE TRIGGER count_api_token_writes AFTER UPDATE OF last_used_at ON api_tokens BEGIN UPDATE api_token_write_count SET n = n + 1; END;`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		authenticate()
	}
	var writes int
	if err := s.db.QueryRow(`SELECT n FROM api_token_write_count`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("recent usage caused %d writes", writes)
	}
	if _, err := s.db.Exec(`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, now().Unix()-60, item.ID); err != nil {
		t.Fatal(err)
	}
	authenticate()
	if err := s.db.QueryRow(`SELECT n FROM api_token_write_count`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Fatalf("old usage was not updated: %d writes", writes)
	}
	if err := s.db.QueryRow(`SELECT last_used_at FROM api_tokens WHERE id = ?`, item.ID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first < now().Unix()-5 {
		t.Fatal("last use was not refreshed")
	}
}

func TestAPITokensExistingDatabaseMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	// Model a database from before this feature while preserving existing data.
	if _, err := db.Exec(`DROP TABLE api_tokens; INSERT INTO hub_beta_testers(login, added_at) VALUES ('existing', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM api_tokens`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_beta_testers WHERE login = 'existing'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("existing data: count %d, error %v", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_index_list('api_tokens') WHERE name = 'idx_api_tokens_owner_login'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("owner index: count %d, error %v", count, err)
	}
}
