package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueryLinearIssuesPaginatesWithCursorVariable(t *testing.T) {
	var cursors []interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		cursors = append(cursors, request.Variables["after"])
		if request.Variables["after"] == nil {
			_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"1","identifier":"ELA-1","title":"one","state":{"name":"Todo"},"team":{"key":"ELA"}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"2","identifier":"ELA-2","title":"two","state":{"name":"Todo"},"team":{"key":"ELA"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`))
	}))
	defer server.Close()
	s := newFactoryTriggerTestServer(t)
	s.linearBaseURL = server.URL
	issues, err := s.queryLinearIssues("token", time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].Identifier != "ELA-1" || issues[1].Identifier != "ELA-2" {
		t.Fatalf("issues = %#v", issues)
	}
	if len(cursors) != 2 || cursors[0] != nil || cursors[1] != "cursor-1" {
		t.Fatalf("cursor variables = %#v", cursors)
	}
}

func TestLinearPollQueryUsesExpectedSince(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		query = request.Query
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`))
	}))
	defer server.Close()
	s := newFactoryTriggerTestServer(t)
	s.linearBaseURL = server.URL
	since := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second).Format(time.RFC3339)
	if _, err := s.queryLinearIssues("token", since); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, since) {
		t.Fatalf("query %q does not contain since %q", query, since)
	}
}
