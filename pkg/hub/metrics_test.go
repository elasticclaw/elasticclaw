package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithMetricsRecordsRequests(t *testing.T) {
	s := &Server{metrics: newServerMetrics(nil)}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/claws/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/integrations/linear/webhook", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusBadRequest, "bad_signature", "bad signature")
	})
	mux.Handle("/metrics", s.metrics.handler())
	h := s.withMetrics(mux)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/claws/abc", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/claws/def", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/integrations/linear/webhook", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	// Requests are labeled by route pattern, not by raw path.
	if !strings.Contains(body, `elasticclaw_http_requests_total{method="GET",route="/api/claws/{id}",status="200"} 2`) {
		t.Fatalf("missing request counter for /api/claws/{id}:\n%s", body)
	}
	if strings.Contains(body, "/api/claws/abc") {
		t.Fatalf("raw path leaked into metrics labels:\n%s", body)
	}
	// Latency histogram exists for the route.
	if !strings.Contains(body, `elasticclaw_http_request_duration_seconds_count{method="GET",route="/api/claws/{id}"} 2`) {
		t.Fatalf("missing duration histogram:\n%s", body)
	}
	// A 4xx on a webhook route is counted per integration.
	if !strings.Contains(body, `elasticclaw_webhook_errors_total{integration="linear"} 1`) {
		t.Fatalf("missing webhook error counter:\n%s", body)
	}
}

func TestWithMetricsNilSafe(t *testing.T) {
	s := &Server{} // hand-built test servers have no metrics
	h := s.withMetrics(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	// Recording methods must also be no-ops on a nil receiver.
	s.metrics.wsMessage("in", "claw")
	s.metrics.webhookError("linear")
	s.metrics.observeRequest("/x", http.MethodGet, 200, 0)
}

func TestWebhookIntegration(t *testing.T) {
	cases := map[string]string{
		"/api/integrations/linear/webhook":            "linear",
		"/api/integrations/github-issues/webhook":     "github-issues",
		"/api/workspaces/{workspace}/webhooks/jira":   "jira",
		"/api/workspaces/{workspace}/webhooks/github": "github",
		"/api/claws":        "",
		"/metrics":          "",
		"unmatched":         "",
		"/api/integrations": "",
	}
	for route, want := range cases {
		if got := webhookIntegration(route); got != want {
			t.Errorf("webhookIntegration(%q) = %q, want %q", route, got, want)
		}
	}
}
