package hub

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestFeatureFlagsResolveStagesAndIgnoreUnknownOverrides(t *testing.T) {
	setFeatureFlagRegistryForTest(t, []FeatureFlag{
		{Key: "default-off", Name: "Default off", DefaultStage: FeatureStageOff},
		{Key: "default-on", Name: "Default on", DefaultStage: FeatureStageOn},
	})
	s, db := NewTestServerWithConfig(t, nil, "", "", "")

	if got := s.effectiveFeatureStage(context.Background(), featureFlagRegistry[0]); got != FeatureStageOff {
		t.Fatalf("default stage = %q, want %q", got, FeatureStageOff)
	}
	if _, err := db.Exec(`INSERT INTO hub_feature_flags(key, stage, updated_at) VALUES(?, ?, ?)`, "default-off", FeatureStageBeta, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hub_feature_flags(key, stage, updated_at) VALUES(?, ?, ?)`, "removed-flag", FeatureStageOn, 1); err != nil {
		t.Fatal(err)
	}

	if got := s.effectiveFeatureStage(context.Background(), featureFlagRegistry[0]); got != FeatureStageBeta {
		t.Fatalf("overridden stage = %q, want %q", got, FeatureStageBeta)
	}
	if _, ok := findFeatureFlag("removed-flag"); ok {
		t.Fatal("database-only flag should not be present in the registry")
	}
}

func TestFeatureFlagsBetaTesterLookupIsCaseInsensitive(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	if _, err := db.Exec(`INSERT INTO hub_beta_testers(login, added_at) VALUES(?, ?)`, "octocat", 1); err != nil {
		t.Fatal(err)
	}

	for _, login := range []string{"octocat", "OctoCat", " @OCTOCAT "} {
		if !s.isBetaTester(context.Background(), login) {
			t.Errorf("isBetaTester(%q) = false, want true", login)
		}
	}
	if s.isBetaTester(context.Background(), "") {
		t.Fatal("empty login should not be a beta tester")
	}
}

func TestFeatureFlagsEnabledFeaturesForLogin(t *testing.T) {
	setFeatureFlagRegistryForTest(t, []FeatureFlag{
		{Key: "z-on", Name: "On", DefaultStage: FeatureStageOn},
		{Key: "a-beta", Name: "Beta", DefaultStage: FeatureStageBeta},
		{Key: "m-off", Name: "Off", DefaultStage: FeatureStageOff},
	})
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	if _, err := db.Exec(`INSERT INTO hub_beta_testers(login, added_at) VALUES(?, ?)`, "tester", 1); err != nil {
		t.Fatal(err)
	}

	if got, want := s.enabledFeaturesForLogin(context.Background(), "TESTER"), []string{"a-beta", "z-on"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tester features = %v, want %v", got, want)
	}
	if got, want := s.enabledFeaturesForLogin(context.Background(), "member"), []string{"z-on"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("non-tester features = %v, want %v", got, want)
	}
	if got, want := s.enabledFeaturesForLogin(context.Background(), ""), []string{"z-on"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("password-session features = %v, want %v", got, want)
	}

	betaRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	betaRequest = betaRequest.WithContext(context.WithValue(betaRequest.Context(), ctxGitHubLoginKey{}, "tester"))
	if !s.featureEnabled(betaRequest, "a-beta") {
		t.Fatal("beta flag should be enabled for a tester")
	}
	if s.featureEnabled(httptest.NewRequest(http.MethodGet, "/", nil), "a-beta") {
		t.Fatal("beta flag should be disabled for a password session")
	}
	if s.featureEnabled(betaRequest, "unknown") {
		t.Fatal("unknown flag should be disabled")
	}
}

func TestFeatureFlagRoutesRequireAdmin(t *testing.T) {
	s, _ := newFeatureFlagTestServer(t)
	nonAdmin := featureFlagSession(t, "member")
	admin := featureFlagSession(t, "admin")

	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/api/settings/feature-flags"},
		{method: http.MethodPut, path: "/api/settings/feature-flags/example", body: `{"stage":"on"}`},
		{method: http.MethodPost, path: "/api/settings/beta-testers", body: `{"login":"octocat"}`},
		{method: http.MethodDelete, path: "/api/settings/beta-testers/octocat"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if got := featureFlagRequest(t, s, tc.method, tc.path, tc.body, nonAdmin).Code; got != http.StatusForbidden {
				t.Fatalf("non-admin status = %d, want %d", got, http.StatusForbidden)
			}
			if got := featureFlagRequest(t, s, tc.method, tc.path, tc.body, admin).Code; got == http.StatusForbidden || got == http.StatusUnauthorized {
				t.Fatalf("admin status = %d, route did not pass admin auth", got)
			}
		})
	}
}

func TestFeatureFlagSettingsAPI(t *testing.T) {
	setFeatureFlagRegistryForTest(t, []FeatureFlag{
		{Key: "first", Name: "First flag", Description: "First description", DefaultStage: FeatureStageOff},
		{Key: "second", Name: "Second flag", Description: "Second description", DefaultStage: FeatureStageOn},
	})
	s, db := newFeatureFlagTestServer(t)
	admin := featureFlagSession(t, "admin")

	if _, err := db.Exec(`INSERT INTO hub_feature_flags(key, stage, updated_at) VALUES(?, ?, ?)`, "removed", FeatureStageOn, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hub_beta_testers(login, added_at, added_by) VALUES(?, ?, ?)`, "zeta", 1, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO hub_beta_testers(login, added_at, added_by) VALUES(?, ?, ?)`, "alpha", 2, "admin"); err != nil {
		t.Fatal(err)
	}

	rec := featureFlagRequest(t, s, http.MethodGet, "/api/settings/feature-flags", "", admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Flags       []featureFlagView `json:"flags"`
		BetaTesters []betaTesterView  `json:"beta_testers"`
	}
	decodeFeatureFlagResponse(t, rec, &listed)
	if got, want := []string{listed.Flags[0].Key, listed.Flags[1].Key}, []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("flag order = %v, want %v", got, want)
	}
	if listed.Flags[0].Stage != FeatureStageOff || listed.Flags[0].DefaultStage != FeatureStageOff {
		t.Fatalf("first flag = %#v", listed.Flags[0])
	}
	if got, want := []string{listed.BetaTesters[0].Login, listed.BetaTesters[1].Login}, []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tester order = %v, want %v", got, want)
	}

	rec = featureFlagRequest(t, s, http.MethodPut, "/api/settings/feature-flags/missing", `{"stage":"on"}`, admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown PUT status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	rec = featureFlagRequest(t, s, http.MethodPut, "/api/settings/feature-flags/first", `{"stage":"preview"}`, admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid-stage PUT status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	rec = featureFlagRequest(t, s, http.MethodPut, "/api/settings/feature-flags/first", `{"stage":"beta"}`, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var updated featureFlagView
	decodeFeatureFlagResponse(t, rec, &updated)
	if updated.Key != "first" || updated.Stage != FeatureStageBeta || updated.DefaultStage != FeatureStageOff {
		t.Fatalf("updated flag = %#v", updated)
	}
	var stage FeatureStage
	var updatedBy string
	if err := db.QueryRow(`SELECT stage, updated_by FROM hub_feature_flags WHERE key = ?`, "first").Scan(&stage, &updatedBy); err != nil {
		t.Fatal(err)
	}
	if stage != FeatureStageBeta || updatedBy != "admin" {
		t.Fatalf("stored override = (%q, %q), want (%q, admin)", stage, updatedBy, FeatureStageBeta)
	}
}

func TestBetaTesterSettingsAPIValidationAndIdempotency(t *testing.T) {
	s, db := newFeatureFlagTestServer(t)
	admin := featureFlagSession(t, "admin")
	otherAdmin := featureFlagSession(t, "other-admin")

	for _, login := range []string{"", "-octocat", "octocat-", "octo--cat", "octo_cat", strings.Repeat("a", 40)} {
		rec := featureFlagRequest(t, s, http.MethodPost, "/api/settings/beta-testers", `{"login":`+mustMarshalFeatureFlagTest(t, login)+`}`, admin)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST login %q status = %d, want %d", login, rec.Code, http.StatusBadRequest)
		}
	}

	rec := featureFlagRequest(t, s, http.MethodPost, "/api/settings/beta-testers", `{"login":" @Octo-Cat "}`, admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var created betaTesterView
	decodeFeatureFlagResponse(t, rec, &created)
	if created.Login != "octo-cat" || created.AddedBy != "admin" || created.AddedAt == 0 {
		t.Fatalf("created tester = %#v", created)
	}

	rec = featureFlagRequest(t, s, http.MethodPost, "/api/settings/beta-testers", `{"login":"OCTO-CAT"}`, otherAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent create status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var existing betaTesterView
	decodeFeatureFlagResponse(t, rec, &existing)
	if existing != created {
		t.Fatalf("idempotent create changed tester: got %#v, want %#v", existing, created)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_beta_testers WHERE login = ?`, "octo-cat").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("tester rows = %d, want 1", count)
	}

	rec = featureFlagRequest(t, s, http.MethodDelete, "/api/settings/beta-testers/octo--cat", "", admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid DELETE status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	for i := 0; i < 2; i++ {
		rec = featureFlagRequest(t, s, http.MethodDelete, "/api/settings/beta-testers/OCTO-CAT", "", admin)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("DELETE attempt %d status = %d, want %d", i+1, rec.Code, http.StatusNoContent)
		}
	}
}

func TestFeatureFlagsWebMeFields(t *testing.T) {
	setFeatureFlagRegistryForTest(t, []FeatureFlag{
		{Key: "beta-feature", DefaultStage: FeatureStageBeta},
		{Key: "on-feature", DefaultStage: FeatureStageOn},
		{Key: "off-feature", DefaultStage: FeatureStageOff},
	})
	s, db := newFeatureFlagTestServer(t)
	if _, err := db.Exec(`INSERT INTO hub_beta_testers(login, added_at) VALUES(?, ?)`, "tester", 1); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		token      string
		wantTester bool
		want       []string
	}{
		{name: "tester", token: featureFlagSession(t, "tester"), wantTester: true, want: []string{"beta-feature", "on-feature"}},
		{name: "non-tester", token: featureFlagSession(t, "member"), want: []string{"on-feature"}},
		{name: "password", token: "hub-token", want: []string{"on-feature"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := featureFlagRequest(t, s, http.MethodGet, "/api/auth/me", "", tc.token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			var me struct {
				IsBetaTester bool     `json:"is_beta_tester"`
				Features     []string `json:"features"`
			}
			decodeFeatureFlagResponse(t, rec, &me)
			if me.IsBetaTester != tc.wantTester || !reflect.DeepEqual(me.Features, tc.want) {
				t.Fatalf("me fields = (tester=%t, features=%v), want (tester=%t, features=%v)", me.IsBetaTester, me.Features, tc.wantTester, tc.want)
			}
			if me.Features == nil {
				t.Fatal("features must not be null")
			}
		})
	}
}

func newFeatureFlagTestServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	return NewTestServerWithConfig(t, &types.HubConfig{
		Token: "hub-token",
		Auth: &types.AuthConfig{
			SessionSecret: "feature-flag-test-secret",
			Access:        &types.AccessConfig{Admins: []string{"admin", "other-admin"}},
		},
	}, "", "", "")
}

func setFeatureFlagRegistryForTest(t *testing.T, registry []FeatureFlag) {
	t.Helper()
	original := featureFlagRegistry
	featureFlagRegistry = registry
	t.Cleanup(func() { featureFlagRegistry = original })
}

func featureFlagSession(t *testing.T, login string) string {
	t.Helper()
	session, err := signGitHubSession("feature-flag-test-secret", login, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func featureFlagRequest(t *testing.T, s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeFeatureFlagResponse(t *testing.T, rec *httptest.ResponseRecorder, target interface{}) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
}

func mustMarshalFeatureFlagTest(t *testing.T, value string) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
