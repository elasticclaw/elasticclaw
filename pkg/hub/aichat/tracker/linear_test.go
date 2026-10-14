package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

const secret = "sentinel-linear-private-key"

func TestLinearFixedQueriesAndResults(t *testing.T) {
	l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != secret {
			t.Error("request/auth")
		}
		var body struct {
			Query     string
			Variables map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch body.Query {
		case searchQuery:
			if body.Variables["query"] != "mutation { malicious }" || body.Variables["limit"] != float64(1) {
				t.Errorf("variables %+v", body.Variables)
			}
			encoded, _ := json.Marshal(body.Variables["filter"])
			if !strings.Contains(string(encoded), "Product") || !strings.Contains(string(encoded), "team") {
				t.Errorf("missing configured team %s", encoded)
			}
			fmt.Fprintf(w, `{"data":{"searchIssues":{"nodes":[{"identifier":"PRD-1","title":%q,"description":%q,"url":"https://linear.app/PRD-1"},{"identifier":"PRD-2"}]}}}`, secret, strings.Repeat("a", 9000))
		case getQuery:
			if body.Variables["id"] != "PRD-1" {
				t.Error("id")
			}
			fmt.Fprintf(w, `{"data":{"issue":{"identifier":"PRD-1","title":%q,"description":%q,"state":{"name":"Todo"},"assignee":{"name":"Alice"},"labels":{"nodes":[{"name":"bug"}]},"url":"https://linear.app/PRD-1"}}}`, secret, strings.Repeat("a", 9000))
		default:
			t.Errorf("unrecognized query: %s", body.Query)
		}
	}))
	for i, args := range []string{`{"query":"mutation { malicious }","limit":1}`, `{"id":"PRD-1"}`} {
		value, err := l.Tools()[i].Run(context.Background(), []byte(args))
		if err != nil || value.RowCount != 1 || strings.Contains(fmt.Sprint(value, err), secret) {
			t.Fatalf("result %+v %v", value, err)
		}
		var rows []issue
		if json.Unmarshal([]byte(value.Data), &rows) != nil || len(rows) != 1 || len(rows[0].Description) > 8020 || !strings.HasSuffix(rows[0].Description, "[truncated]") {
			t.Fatalf("bad description %s", value.Data)
		}
		if i == 1 && (rows[0].State.Name != "Todo" || rows[0].Assignee.Name != "Alice" || len(rows[0].Labels.Nodes) != 1) {
			t.Errorf("missing issue fields %+v", rows)
		}
	}
}
func TestLinearRejectsInvalidArguments(t *testing.T) {
	calls := 0
	l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	for _, args := range []string{`{"id":"../foo"}`, `{"id":"PRD-1","query":"mutation {}"}`, `{"id":"PRD-0"}`, `{"id":"PRD-1"} {}`} {
		if _, err := l.Get(context.Background(), []byte(args)); err == nil {
			t.Errorf("accepted %s", args)
		}
	}
	for _, args := range []string{`{"query":""}`, `{"query":"x","limit":101}`, `{"query":"x","limit":-1}`, `{"query":"x","graphql":"mutation {}"}`} {
		if _, err := l.Search(context.Background(), []byte(args)); err == nil {
			t.Errorf("accepted %s", args)
		}
	}
	if calls != 0 {
		t.Errorf("invalid requests sent: %d", calls)
	}
}
func TestLinearHealth(t *testing.T) {
	l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string
			Variables map[string]any
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Query != healthQuery {
			t.Errorf("query %s", body.Query)
		}
		fmt.Fprintf(w, `{"data":{"teams":{"nodes":[{"id":"team","name":%q}]}}}`, "Product "+secret)
	}))
	detail, err := l.Health(context.Background())
	if err != nil || !strings.Contains(detail, "Team Product") || strings.Contains(detail, secret) {
		t.Fatalf("health %s %v", detail, err)
	}
}
func TestLinearGenericErrorsAndCancellation(t *testing.T) {
	for _, code := range []int{200, 401} {
		l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			fmt.Fprintf(w, `{"errors":[{"message":%q}]}`, secret)
		}))
		for i, args := range []string{`{"query":"search"}`, `{"id":"PRD-1"}`} {
			value, err := l.Tools()[i].Run(context.Background(), []byte(args))
			if err == nil || strings.Contains(fmt.Sprint(value, err), secret) {
				t.Errorf("leaking error %+v %v", value, err)
			}
		}
		if _, err := l.Health(context.Background()); err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("health err %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := NewLinear("Product", secret, &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })})
	if _, err := l.Get(ctx, []byte(`{"id":"PRD-1"}`)); err != context.Canceled {
		t.Errorf("context %v", err)
	}
}
func TestLinearTeamFilters(t *testing.T) {
	for _, team := range []string{"ENG", "Design", "ab12cd34-0000-1111-2222-ab12cd345678"} {
		l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) {
			var body struct{ Variables map[string]any }
			_ = json.NewDecoder(r.Body).Decode(&body)
			filter, _ := json.Marshal(body.Variables["filter"])
			if !strings.Contains(string(filter), team) {
				t.Errorf("team missing %s", filter)
			}
			if uuid.MatchString(team) && !strings.Contains(string(filter), `"id"`) {
				t.Error("UUID not filtered by id")
			}
			fmt.Fprint(w, `{"data":{"searchIssues":{"nodes":[]}}}`)
		}))
		raw, _ := json.Marshal(map[string]any{"query": "foo", "team": team})
		if _, err := l.Search(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
	}
}
