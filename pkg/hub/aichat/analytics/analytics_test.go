package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fake(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Result(), nil
	})}
}

const secret = "sentinel-private-provider-key"

func TestPostHogQueryAndInsights(t *testing.T) {
	client := fake(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("missing auth")
		}
		switch r.URL.Path {
		case "/api/projects/42/query/":
			if r.Method != http.MethodPost {
				t.Error("query method")
			}
			var body struct{ Query struct{ Kind, Query string } }
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Query.Kind != "HogQLQuery" || body.Query.Query != "SELECT event FROM events" {
				t.Errorf("body: %+v", body)
			}
			rows := make([][]string, 600)
			for i := range rows {
				rows[i] = []string{secret}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"columns": []string{"event"}, "results": rows})
		case "/api/projects/42/insights/":
			if r.Method != http.MethodGet || r.URL.Query().Get("search") != "signup" || r.URL.Query().Get("saved") != "true" {
				t.Error("insight search")
			}
			fmt.Fprintf(w, `{"results":[{"name":%q}]}`, secret)
		case "/api/projects/42/insights/123/":
			fmt.Fprintf(w, `{"id":123,"result":[%q]}`, secret)
		default:
			t.Errorf("unexpected path %s", r.URL)
			w.WriteHeader(404)
		}
	})
	p := NewPostHog(config.PostHog{Host: "https://posthog.example", ProjectID: "42"}, secret, client)
	for _, test := range []struct {
		index int
		args  string
		rows  int
	}{{0, `{"query":"SELECT event FROM events","limit":999}`, 500}, {1, `{"search":"signup"}`, 1}, {1, `{"id":"123"}`, 1}} {
		value, err := p.Tools()[test.index].Run(context.Background(), json.RawMessage(test.args))
		if err != nil || value.RowCount != test.rows || strings.Contains(fmt.Sprint(value, err), secret) {
			t.Fatalf("result %+v error %v", value, err)
		}
	}
}
func TestPostHogRejectsWritesAndMalformedArguments(t *testing.T) {
	calls := 0
	p := NewPostHog(config.PostHog{Host: "https://posthog.example", ProjectID: "42"}, secret, fake(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	for _, query := range []string{"DELETE FROM events", "SELECT 1; DROP TABLE events", "WITH x AS (DELETE FROM events) SELECT * FROM x", "SELECT 1 INTO OUTFILE 'x'", "WITH x AS (SELECT 1) INSERT INTO events SELECT * FROM x", "SELECT 1 -- hi", "SELECT 'unterminated", "'ignored' SELECT 1"} {
		args, _ := json.Marshal(map[string]any{"query": query})
		if _, err := p.Query(context.Background(), args); err == nil {
			t.Errorf("accepted %q", query)
		}
	}
	for _, args := range []string{`{"query":"SELECT 1","extra":true}`, `{"id":"../secrets"}`, `{"search":"x","id":"12"}`} {
		var err error
		if strings.Contains(args, "query") {
			_, err = p.Query(context.Background(), []byte(args))
		} else {
			_, err = p.Insights(context.Background(), []byte(args))
		}
		if err == nil {
			t.Errorf("accepted %s", args)
		}
	}
	if calls != 0 {
		t.Fatalf("sent %d invalid requests", calls)
	}
	for _, query := range []string{"SELECT event FROM events", "WITH x AS (SELECT 1) SELECT * FROM x", "SELECT 'delete; -- update'", "SELECT properties.format, count() FROM events GROUP BY 1", "SELECT * FROM events WHERE properties.system = 'ios'", "SELECT properties . system FROM events", "SELECT properties.v2system FROM events", "SELECT `format`, \"system\" FROM events"} {
		if !readQuery(query) {
			t.Errorf("rejected read %q", query)
		}
	}
}
func TestPostHogHealth(t *testing.T) {
	p := NewPostHog(config.PostHog{Host: "https://posthog.example", ProjectID: "42"}, secret, fake(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/projects/42/query/" {
			fmt.Fprint(w, `{"results":[[1]]}`)
			return
		}
		if r.URL.Path == "/api/projects/42/" {
			fmt.Fprintf(w, `{"name":%q}`, "Demo "+secret)
			return
		}
		if r.URL.Path != "/api/projects/42/event_definitions/" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("path %s", r.URL)
		}
		events := make([]map[string]string, 60)
		for i := range events {
			events[i] = map[string]string{"name": fmt.Sprint("event", i)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": events})
	}))
	detail, events, err := p.Health(context.Background())
	if err != nil || len(events) != 50 || strings.Contains(detail, secret) || !strings.Contains(detail, "Demo") {
		t.Fatalf("health %s %v %v", detail, events, err)
	}
}

const period = `"from":"2026-10-01T00:00:00Z","to":"2026-10-02T00:00:00Z"`

func TestDatadogScopeAndCaps(t *testing.T) {
	d := NewDatadog(config.Datadog{Site: "datadoghq.com", Env: "prod"}, secret, "app-"+secret, fake(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("DD-API-KEY") != secret || r.Header.Get("DD-APPLICATION-KEY") != "app-"+secret {
			t.Error("auth")
		}
		switch r.URL.Path {
		case "/api/v1/query":
			if r.Method != http.MethodGet || r.URL.Query().Get("query") != "avg:system.cpu{env:prod,service:api} by {host}" {
				t.Errorf("metric query %s", r.URL)
			}
			fmt.Fprintf(w, `{"status":"ok","series":[{"metric":%q}]}`, secret)
		case "/api/v2/logs/events/search":
			var body struct {
				Filter struct{ Query string }
				Page   struct{ Limit int }
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Method != http.MethodPost || body.Filter.Query != "env:prod (status:error OR service:web)" || body.Page.Limit != 500 {
				t.Errorf("logs %+v", body)
			}
			rows := make([]map[string]string, 600)
			for i := range rows {
				rows[i] = map[string]string{"message": secret}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
		default:
			t.Errorf("path %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	for _, test := range []struct {
		index int
		args  string
		rows  int
	}{{0, `{` + period + `,"aggregation":"avg","metric":"system.cpu","tags":{"service":"api"},"groupBy":["host"]}`, 1}, {1, `{` + period + `,"query":"status:error OR service:web","limit":1000}`, 500}} {
		value, err := d.Tools()[test.index].Run(context.Background(), []byte(test.args))
		if err != nil || value.RowCount != test.rows || strings.Contains(fmt.Sprint(value, err), secret) {
			t.Fatalf("result %+v, err %v", value, err)
		}
	}
}
func TestDatadogRejectsScopeEscapes(t *testing.T) {
	calls := 0
	d := NewDatadog(config.Datadog{Site: "datadoghq.com", Env: "prod"}, secret, secret, fake(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	for _, extra := range []string{`"tags":{"env":"staging"}`, `"tags":{"service":"api} OR env:staging"}`, `"groupBy":["host} + avg:other{*"]`, `"metric":"cpu{*}"`, `"query":"avg:cpu{*}"`} {
		args := `{` + period + `,"aggregation":"avg","metric":"system.cpu",` + extra + `}`
		if _, err := d.Metrics(context.Background(), []byte(args)); err == nil {
			t.Errorf("accepted %s", args)
		}
	}
	for _, query := range []string{"env:staging", "ENV :staging", ") OR * (", `service:"x`, "foo\\) OR *", "@env:staging"} {
		args := map[string]any{"from": "2026-10-01T00:00:00Z", "to": "2026-10-02T00:00:00Z", "query": query}
		raw, _ := json.Marshal(args)
		if _, err := d.Logs(context.Background(), raw); err == nil {
			t.Errorf("accepted %q", query)
		}
	}
	if _, err := d.Logs(context.Background(), []byte(`{"from":"2026-09-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}`)); err == nil {
		t.Error("accepted long window")
	}
	if calls != 0 {
		t.Errorf("sent %d invalid requests", calls)
	}
}
func TestAnalyticsAuthFailuresAndCancellation(t *testing.T) {
	rejected := fake(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); fmt.Fprint(w, secret) })
	p := NewPostHog(config.PostHog{Host: "https://posthog.example", ProjectID: "42"}, secret, rejected)
	d := NewDatadog(config.Datadog{Site: "datadoghq.com", Env: "prod"}, secret, secret, rejected)
	for _, source := range []Source{p, d} {
		for _, tool := range source.Tools() {
			args := `{}`
			switch tool.Definition().Name {
			case "posthog_query":
				args = `{"query":"SELECT 1"}`
			case "datadog_metrics":
				args = `{` + period + `,"metric":"cpu","aggregation":"avg"}`
			case "datadog_logs":
				args = `{` + period + `}`
			}
			value, err := tool.Run(context.Background(), []byte(args))
			if err == nil || strings.Contains(fmt.Sprint(value, err), secret) || !strings.Contains(err.Error(), "401") {
				t.Errorf("auth result %+v %v", value, err)
			}
		}
	}
	if _, _, err := p.Health(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
		t.Error("PostHog health accepted auth error")
	}
	if _, err := d.Health(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
		t.Error("Datadog health accepted auth error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Client.HTTP = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	if _, err := p.Query(ctx, []byte(`{"query":"SELECT 1"}`)); err != context.Canceled {
		t.Errorf("context: %v", err)
	}
}
func TestDatadogHealthValidatesBothKeys(t *testing.T) {
	calls := 0
	d := NewDatadog(config.Datadog{Site: "datadoghq.com", Env: "prod"}, secret, secret, fake(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.URL.Path {
		case "/api/v1/validate":
			fmt.Fprint(w, `{"valid":true}`)
		case "/api/v2/validate_keys":
			fmt.Fprint(w, `{"status":"ok"}`)
		default:
			t.Error(r.URL.Path)
		}
	}))
	detail, err := d.Health(context.Background())
	if err != nil || calls != 2 || detail != "Metrics and logs · env prod" {
		t.Fatalf("health %s %v calls %d", detail, err, calls)
	}
}

func TestPostHogQueryRejectsProviderError(t *testing.T) {
	p := NewPostHog(config.PostHog{Host: "https://posthog.example", ProjectID: "42"}, secret, fake(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintf(w, `{"error":%q}`, secret) }))
	value, err := p.Query(context.Background(), []byte(`{"query":"SELECT 1"}`))
	if err == nil || strings.Contains(fmt.Sprint(value, err), secret) {
		t.Fatalf("provider error: %+v %v", value, err)
	}
}
