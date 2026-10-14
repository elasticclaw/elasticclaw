package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestReadRejectsUnsafeProviderFiles(t *testing.T) {
	for _, response := range []string{
		`{"type":"symlink","encoding":"base64","content":"aGVsbG8="}`,
		`{"type":"file","target":"other","encoding":"base64","content":"aGVsbG8="}`,
		`{"type":"file","submodule_git_url":"https://other","encoding":"base64","content":"aGVsbG8="}`,
		`{"type":"file","size":9999999,"encoding":"base64","content":"aGVsbG8="}`,
		`{"type":"file","encoding":"base64","content":"broken"}`,
		fmt.Sprintf(`{"type":"file","encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte{0, 255})),
	} {
		client := Client{BaseURL: "https://github.test", Token: func(string) string { return "secret" }, HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			fmt.Fprint(w, response)
			return w.Result(), nil
		})}}
		if _, err := client.Read(context.Background(), "acme/app", "file", "main"); err == nil {
			t.Errorf("accepted unsafe response %s", response)
		}
	}
}
func TestTreeTruncationAndMissingCredentials(t *testing.T) {
	client := Client{BaseURL: "https://github.test", Token: func(string) string { return "secret" }, HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		fmt.Fprint(w, `{"truncated":true,"tree":[]}`)
		return w.Result(), nil
	})}}
	if _, err := client.Tree(context.Background(), "acme/app", strings.Repeat("a", 40)); err == nil {
		t.Fatal("silently accepted partial tree")
	}
	client.Token = nil
	if _, err := client.DefaultBranch(context.Background(), "acme/app"); err == nil {
		t.Fatal("missing credentials accepted")
	}
}
