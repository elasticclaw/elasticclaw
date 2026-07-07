package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCORSMiddlewareEchoesAllowedOrigin(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{AllowedOrigins: []string{"http://localhost:3000"}}}
	handler := s.corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/claws", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "http://localhost:3000")
	}
	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want %q", got, "Origin")
	}
}

func TestCORSMiddlewareOmitsHeaderForUnlistedOrigin(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{AllowedOrigins: []string{"http://localhost:3000"}}}
	handler := s.corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/claws", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
	}
	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want %q", got, "Origin")
	}
}

func TestCORSMiddlewareAlwaysSetsVaryOrigin(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{}}
	handler := s.corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// No Origin header at all — still expect Vary: Origin.
	req := httptest.NewRequest(http.MethodGet, "/api/claws", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want %q", got, "Origin")
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestCORSMiddlewareDefaultsToHubOwnOrigin(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{URL: "http://localhost:8080"}}
	handler := s.corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/claws", nil)
	req.Header.Set("Origin", "http://localhost:8080")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:8080" {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, "http://localhost:8080")
	}
}
