package repos

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func fake(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	return &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("write method %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer sentinel-secret" {
			t.Errorf("wrong auth")
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Result(), nil
	})}
}
func TestRepositoryReadsAndPullRequestResolution(t *testing.T) {
	pulls := 0
	branch := 0
	client := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/app":
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case "/repos/acme/app/commits/main", "/repos/acme/app/commits/feature/work":
			branch++
			fmt.Fprintf(w, `{"sha":%q}`, commit)
		case "/repos/acme/app/pulls/42":
			pulls++
			fmt.Fprintf(w, `{"head":{"sha":%q}}`, commit)
		case "/repos/acme/app/git/trees/" + commit:
			fmt.Fprint(w, `{"tree":[{"path":"src/app.go","type":"blob"},{"path":"sentinel-secret.md","type":"blob"}]}`)
		case "/repos/acme/app/contents/src/app.go":
			if r.URL.Query().Get("ref") != commit {
				t.Error("PR read not pinned to head SHA")
			}
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte("source sentinel-secret")))
		case "/search/code":
			if r.URL.Query().Get("q") != "repo:acme/app handler" {
				t.Errorf("unscoped query: %s", r.URL)
			}
			fmt.Fprint(w, `{"items":[{"path":"src/app.go","html_url":"https://github.com/acme/app/sentinel-secret","repository":{"full_name":"acme/app"}},{"path":"private.go","repository":{"full_name":"other/private"}}]}`)
		default:
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(404)
		}
	})
	provider := New([]string{"acme/app"}, "https://github.test", func(repo string) string {
		if repo != "acme/app" {
			t.Fatalf("unexpected token scope %s", repo)
		}
		return "sentinel-secret"
	}, client)
	detail, err := provider.Health(context.Background())
	if err != nil || detail != "acme/app · default branch" {
		t.Fatalf("health = %q %v", detail, err)
	}
	for _, tool := range provider.Tools() {
		args := map[string]string{"repo_tree": `{"repo":"acme/app","ref":"feature/work"}`, "repo_read": `{"repo":"acme/app","pr":42,"path":"src/app.go"}`, "repo_search": `{"repo":"acme/app","query":"handler"}`}[tool.Definition().Name]
		result, err := tool.Run(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		if result.RowCount < 1 || strings.Contains(result.Summary+result.Data, "sentinel-secret") || strings.Contains(result.Data, "private.go") {
			t.Fatalf("unsafe result %#v", result)
		}
		if tool.Provider() != "Repositories" {
			t.Fatal("provider display name")
		}
	}
	if pulls != 1 || branch != 1 {
		t.Fatalf("resolution pulls=%d branch=%d", pulls, branch)
	}
}
func TestRepositoryRejectsUnsafeArgumentsBeforeRequest(t *testing.T) {
	requests := 0
	provider := New([]string{"acme/app"}, "https://github.test", func(string) string { return "sentinel-secret" }, fake(t, func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(500) }))
	cases := map[string][]string{
		"repo_tree":   {`{"repo":"other/private"}`, `{"repo":"acme/app","dir":"../"}`, `{"repo":"acme/app","pr":1,"ref":"main"}`, `{"repo":"acme/app","pr":-1}`, `{"repo":"acme/app","ref":"../main"}`},
		"repo_read":   {`{"repo":"acme/app","path":"a/../secret"}`, `{"repo":"acme/app","path":"/etc/passwd"}`, `{"repo":"acme/app","path":"a\\b"}`, `{"repo":"acme/app","path":"a\u0000b"}`},
		"repo_search": {`{"repo":"other/private","query":"hello"}`, `{"repo":"acme/app","query":"repo:other/private"}`, `{"repo":"acme/app","query":"hello OR secret"}`, `{"repo":"acme/app","query":"hello","ref":"other"}`, `{"repo":"acme/app","query":"hello","limit":101}`},
	}
	for _, tool := range provider.Tools() {
		for _, args := range append(cases[tool.Definition().Name], `{"unknown":true}`) {
			if _, err := tool.Run(context.Background(), json.RawMessage(args)); err == nil {
				t.Errorf("accepted %s: %s", tool.Definition().Name, args)
			}
		}
	}
	if requests != 0 {
		t.Fatalf("%d invalid requests reached GitHub", requests)
	}
}
func TestRepositoryAuthFailureIsGeneric(t *testing.T) {
	provider := New([]string{"acme/app"}, "https://github.test", func(string) string { return "sentinel-secret" }, fake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, "sentinel-secret provider details")
	}))
	if _, err := provider.Health(context.Background()); err == nil || strings.Contains(err.Error(), "sentinel-secret") {
		t.Fatalf("health error: %v", err)
	}
	for _, tool := range provider.Tools() {
		args := map[string]string{"repo_tree": `{"repo":"acme/app"}`, "repo_read": `{"repo":"acme/app","path":"README.md"}`, "repo_search": `{"repo":"acme/app","query":"hello"}`}[tool.Definition().Name]
		result, err := tool.Run(context.Background(), json.RawMessage(args))
		if err == nil || strings.Contains(fmt.Sprint(result, err), "sentinel-secret") {
			t.Fatalf("%s result: %#v %v", tool.Definition().Name, result, err)
		}
	}
}
