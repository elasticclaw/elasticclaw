package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// decodeAPIError asserts the response is the unified JSON error envelope and
// returns it.
func decodeAPIError(t *testing.T, rec *httptest.ResponseRecorder) apiError {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (body: %q)", ct, rec.Body.String())
	}
	var envelope apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body is not valid JSON: %v (body: %q)", err, rec.Body.String())
	}
	return envelope
}

// Phase 0.2: auth middleware wrapping UI routes must return the JSON error
// envelope instead of plain-text http.Error.
func TestAuthMiddlewareReturnsJSONEnvelope(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "hub-token",
		Auth: &types.AuthConfig{
			SessionSecret: "session-secret",
			Access:        &types.AccessConfig{Admins: []string{"admin-user"}},
		},
	}, "", "", "")

	nonAdminSession, err := signGitHubSession("session-secret", "regular-user", "", "")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		middleware func(http.HandlerFunc) http.HandlerFunc
		token      string
		wantStatus int
		wantCode   string
	}{
		{"withAuth missing token", s.withAuth, "", http.StatusUnauthorized, "unauthorized"},
		{"withAuth invalid token", s.withAuth, "bogus", http.StatusUnauthorized, "unauthorized"},
		{"withWebAuth missing token", s.withWebAuth, "", http.StatusUnauthorized, "unauthorized"},
		{"withWebAuth invalid token", s.withWebAuth, "bogus", http.StatusUnauthorized, "unauthorized"},
		{"withWebAdminAuth invalid token", s.withWebAdminAuth, "bogus", http.StatusUnauthorized, "unauthorized"},
		{"withWebAdminAuth non-admin session", s.withWebAdminAuth, nonAdminSession, http.StatusForbidden, "forbidden"},
		{"withConfigMutationAuth missing token", s.withConfigMutationAuth, "", http.StatusUnauthorized, "unauthorized"},
		{"withConfigMutationAuth non-admin session", s.withConfigMutationAuth, nonAdminSession, http.StatusForbidden, "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			tc.middleware(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("handler should not be called")
			})(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			envelope := decodeAPIError(t, rec)
			if envelope.Code != tc.wantCode {
				t.Fatalf("envelope = %+v, want code %q", envelope, tc.wantCode)
			}
		})
	}
}

// Phase 0.2: the model-auth login status poll endpoint must return the JSON
// envelope for a missing job (the frontend polls this URL).
func TestModelAuthLoginStatusMissingJobReturnsEnvelope(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "hub-token"}, "", "", "")

	req := httptest.NewRequest(http.MethodGet, "/api/settings/model-auth/login/does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer hub-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %q)", rec.Code, rec.Body.String())
	}
	envelope := decodeAPIError(t, rec)
	if envelope.Code != "not_found" {
		t.Fatalf("envelope = %+v, want code not_found", envelope)
	}
}

// Phase 0.2: canViewMessages error paths (missing claw, OAuth session without
// tag access) must return the JSON envelope on the timeline/activity routes.
func TestMessageTimelineAccessErrorsReturnEnvelope(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token: "hub-token",
		Auth: &types.AuthConfig{
			SessionSecret: "session-secret",
			Access: &types.AccessConfig{
				Admins:           []string{"admin-user"},
				ViewRequiresTags: []string{"user:{user}"},
			},
		},
	}, "", "", "")

	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, tags, created_at) VALUES(?,?,?,?,datetime('now'))`,
		"claw-restricted", "test-tenant-id", "restricted claw", `["user:someone-else"]`,
	)
	if err != nil {
		t.Fatal(err)
	}

	session, err := signGitHubSession("session-secret", "regular-user", "", "")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		url        string
		wantStatus int
		wantCode   string
	}{
		{"forbidden timeline", "/api/messages/claw-restricted/timeline", http.StatusForbidden, "forbidden"},
		{"forbidden activity", "/api/messages/claw-restricted/activity", http.StatusForbidden, "forbidden"},
		{"missing claw timeline", "/api/messages/no-such-claw/timeline", http.StatusNotFound, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			req.Header.Set("Authorization", "Bearer "+session)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			envelope := decodeAPIError(t, rec)
			if envelope.Code != tc.wantCode {
				t.Fatalf("envelope = %+v, want code %q", envelope, tc.wantCode)
			}
		})
	}
}
