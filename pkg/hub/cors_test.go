package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func newCORSHandler(allowed map[string]struct{}) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return corsMiddleware(next, allowed)
}

func doCORSRequest(t *testing.T, h http.Handler, method, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/claws", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCORSMiddlewareAllowedOrigin(t *testing.T) {
	h := newCORSHandler(map[string]struct{}{"http://localhost:3000": {}})

	rec := doCORSRequest(t, h, http.MethodGet, "http://localhost:3000")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "http://localhost:3000")
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want %q", got, "Origin")
	}
	if rec.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Error("expected Access-Control-Allow-Headers on allowed origin")
	}
}

func TestCORSMiddlewareDisallowedOrigin(t *testing.T) {
	h := newCORSHandler(map[string]struct{}{"http://localhost:3000": {}})

	rec := doCORSRequest(t, h, http.MethodGet, "https://evil.example.com")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for unlisted origin", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "" {
		t.Errorf("Access-Control-Allow-Headers = %q, want empty for unlisted origin", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want %q even when origin is unlisted", got, "Origin")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (request still served; browser enforces CORS)", rec.Code, http.StatusOK)
	}
}

func TestCORSMiddlewareNoOriginHeader(t *testing.T) {
	h := newCORSHandler(map[string]struct{}{"http://localhost:3000": {}})

	rec := doCORSRequest(t, h, http.MethodGet, "")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty when no Origin header", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want %q", got, "Origin")
	}
}

func TestCORSMiddlewarePreflight(t *testing.T) {
	h := newCORSHandler(map[string]struct{}{"http://localhost:3000": {}})

	rec := doCORSRequest(t, h, http.MethodOptions, "http://localhost:3000")
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "http://localhost:3000")
	}
	if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("expected Access-Control-Allow-Methods on preflight from allowed origin")
	}

	rec = doCORSRequest(t, h, http.MethodOptions, "https://evil.example.com")
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty for unlisted origin", got)
	}
}

func TestCORSMiddlewareOriginMatchIsCaseInsensitive(t *testing.T) {
	h := newCORSHandler(map[string]struct{}{"http://localhost:3000": {}})

	rec := doCORSRequest(t, h, http.MethodGet, "HTTP://LOCALHOST:3000")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "HTTP://LOCALHOST:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want request origin echoed", got)
	}
}

func TestAllowedCORSOriginsFromConfig(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{
		URL:            "http://localhost:8080",
		AllowedOrigins: []string{"http://localhost:3000/", "https://Hub.Example.com", "not a url"},
	}}
	got := s.allowedCORSOrigins()
	for _, want := range []string{"http://localhost:3000", "https://hub.example.com"} {
		if _, ok := got[want]; !ok {
			t.Errorf("allowedCORSOrigins missing %q (got %v)", want, got)
		}
	}
	if _, ok := got["http://localhost:8080"]; ok {
		t.Error("hub URL must not be implicitly allowed when allowed_origins is set")
	}
	if len(got) != 2 {
		t.Errorf("allowedCORSOrigins has %d entries, want 2 (invalid entries dropped): %v", len(got), got)
	}
}

func TestAllowedCORSOriginsDefaultsToHubOrigin(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{
		URL:       "http://localhost:8080/some/path",
		PublicURL: "https://hub.example.com",
	}}
	got := s.allowedCORSOrigins()
	for _, want := range []string{"http://localhost:8080", "https://hub.example.com"} {
		if _, ok := got[want]; !ok {
			t.Errorf("allowedCORSOrigins missing default %q (got %v)", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("allowedCORSOrigins has %d entries, want 2: %v", len(got), got)
	}
}
