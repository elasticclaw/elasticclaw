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

func TestQueryLinearIssuesReturnsErrorAtPaginationCap(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		_, _ = w.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"1","identifier":"ELA-1","title":"one","state":{"name":"Todo"},"team":{"key":"ELA"}}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`))
	}))
	defer server.Close()
	s := newFactoryTriggerTestServer(t)
	s.linearBaseURL = server.URL
	issues, err := s.queryLinearIssues("token", time.Now().UTC().Format(time.RFC3339))
	if err == nil {
		t.Fatal("expected error when pagination cap is hit with more pages remaining")
	}
	if pages != 20 || len(issues) != 20 {
		t.Fatalf("pages = %d, issues = %d, want 20/20", pages, len(issues))
	}
}

func TestQueryLinearIssuesReturnsGraphQLErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"rate limited"}],"data":null}`))
	}))
	defer server.Close()
	s := newFactoryTriggerTestServer(t)
	s.linearBaseURL = server.URL
	if _, err := s.queryLinearIssues("token", time.Now().UTC().Format(time.RFC3339)); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v, want GraphQL error", err)
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
