package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubFetchUserUsesBearerAuthorization(t *testing.T) {
	const accessToken = "oauth-access-token"

	var gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/user" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"login":"octocat","name":"Octo Cat","avatar_url":"https://example.com/avatar.png"}`))
	}))
	t.Cleanup(api.Close)

	s, _ := NewTestServerWithConfig(t, nil, api.URL, "", "")
	user, err := s.githubFetchUser(context.Background(), accessToken)
	if err != nil {
		t.Fatalf("githubFetchUser returned error: %v", err)
	}
	if user.Login != "octocat" {
		t.Fatalf("login = %q, want octocat", user.Login)
	}
	if gotAuth != "Bearer "+accessToken {
		t.Fatalf("Authorization header = %q, want Bearer token", gotAuth)
	}
}

func TestGitHubFetchUserUnauthorizedErrorDoesNotLeakToken(t *testing.T) {
	const accessToken = "oauth-access-token"

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "token "+accessToken+" rejected", http.StatusUnauthorized)
	}))
	t.Cleanup(api.Close)

	s, _ := NewTestServerWithConfig(t, nil, api.URL, "", "")
	_, err := s.githubFetchUser(context.Background(), accessToken)
	if err == nil {
		t.Fatal("githubFetchUser returned nil error, want unauthorized error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401 Unauthorized") {
		t.Fatalf("error = %q, want 401 Unauthorized context", msg)
	}
	if strings.Contains(msg, accessToken) {
		t.Fatalf("error leaked access token: %q", msg)
	}
}
