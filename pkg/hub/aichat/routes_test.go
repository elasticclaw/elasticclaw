package aichat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSources(t *testing.T) {
	root := t.TempDir()
	managedDir := func(name string) string { return filepath.Join(root, name, ".elasticclaw-managed") }
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(managedDir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(managedDir(name), "ai_chat.yaml"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("configured", "repositories: [org/repo]\nposthog: {host: invalid}")
	write("broken", "about: [")
	deps := Deps{
		WebAuth:     func(next http.HandlerFunc) http.HandlerFunc { return next },
		WithFeature: func(_ string, next http.HandlerFunc) http.HandlerFunc { return next },
		Workspaces:  func() ([]string, error) { return []string{"missing", "configured", "broken"}, nil },
		ManagedDir: func(name string) string {
			if name != "missing" && name != "configured" && name != "broken" {
				t.Fatalf("untrusted workspace passed to resolver: %q", name)
			}
			return managedDir(name)
		},
	}
	handler := Routes(deps)
	for _, tc := range []struct {
		query, workspace string
		configured       bool
		sourceCount      int
	}{
		{"", "broken", false, 0},
		{"?workspace=configured", "configured", true, 2},
		{"?workspace=broken", "broken", false, 0},
		{"?workspace=missing", "missing", false, 0},
		{"?workspace=unknown", "unknown", false, 0},
		{"?workspace=..%2Fprivate", "../private", false, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/ai-chat/sources"+tc.query, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			var response SourcesResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Configured != tc.configured || response.Workspace != tc.workspace || len(response.Sources) != tc.sourceCount || response.Sources == nil || !reflect.DeepEqual(response.Workspaces, []string{"broken", "configured"}) {
				t.Fatalf("response = %#v", response)
			}
			if tc.configured && (response.Sources[0].Status != "unchecked" || response.Sources[1].Status != "invalid" || response.Sources[1].Error == "") {
				t.Fatalf("sources = %#v", response.Sources)
			}
		})
	}
	// The next request sees edits and newly removed files without restarting.
	write("configured", "about: Updated")
	if err := os.Remove(filepath.Join(managedDir("broken"), "ai_chat.yaml")); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/ai-chat/sources", nil))
	var response SourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Configured || response.Workspace != "configured" || len(response.Workspaces) != 1 || len(response.Sources) != 0 {
		t.Fatalf("reloaded = %#v", response)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/api/ai-chat/sources", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}
	deps.Workspaces = func() ([]string, error) { return nil, errors.New("internal path") }
	rec = httptest.NewRecorder()
	Routes(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/ai-chat/sources", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("error status = %d", rec.Code)
	}
	deps.Workspaces = func() ([]string, error) { return nil, nil }
	rec = httptest.NewRecorder()
	Routes(deps).ServeHTTP(rec, httptest.NewRequest("GET", "/api/ai-chat/sources", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Configured || response.Workspaces == nil || response.Sources == nil {
		t.Fatalf("empty = %#v", response)
	}
}

func TestOwnerLogin(t *testing.T) {
	for input, want := range map[string]string{"OctoCat": "octocat", "": "admin", "  TESTER ": "tester"} {
		if got := OwnerLogin(input); got != want {
			t.Errorf("OwnerLogin(%q) = %q, want %q", input, got, want)
		}
	}
}
