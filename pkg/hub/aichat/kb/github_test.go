package kb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func fake(t *testing.T, handler http.HandlerFunc) *http.Client {
	t.Helper()
	return &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Errorf("write method %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer sentinel-secret" {
			t.Errorf("missing repository credential")
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w.Result(), nil
	})}
}
func TestGitHubKnowledgeReadsAndCachedHeadingIndex(t *testing.T) {
	var reads atomic.Int32
	commit := shaA
	client := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/docs":
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case "/repos/acme/docs/commits/main":
			fmt.Fprintf(w, `{"sha":%q}`, commit)
		case "/repos/acme/docs/git/trees/" + shaA, "/repos/acme/docs/git/trees/" + shaB:
			fmt.Fprint(w, `{"tree":[{"path":"README.md","type":"blob","mode":"100644","size":30},{"path":"guide/next.md","type":"blob","size":30},{"path":"secret.txt","type":"blob"},{"path":"link.md","type":"blob","mode":"120000"}]}`)
		case "/repos/acme/docs/contents/README.md", "/repos/acme/docs/contents/guide/next.md":
			reads.Add(1)
			if r.URL.Query().Get("ref") != commit {
				t.Errorf("read not pinned to commit")
			}
			content := base64.StdEncoding.EncodeToString([]byte("# Intro sentinel-secret\n\n## Adoption\n```\n# hidden\n```\n"))
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","size":60,"content":%q}`, content)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	})
	provider := New(config.KnowledgeBase{Repo: "acme/docs"}, t.Name(), func(string) string { return "sentinel-secret" }, client)
	provider.client.BaseURL = "https://" + strings.ToLower(t.Name()) + ".test"
	detail, err := provider.Health(context.Background())
	if err != nil || detail != "acme/docs · 2 pages" {
		t.Fatalf("health = %q %v", detail, err)
	}
	for _, tool := range provider.Tools() {
		args := map[string]string{"kb_list": `{"dir":"guide"}`, "kb_read": `{"path":"README.md"}`, "kb_search": `{"query":"Intro"}`}[tool.Definition().Name]
		result, err := tool.Run(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		if result.RowCount < 1 || result.Data == "" || tool.Provider() != "Knowledge base" {
			t.Fatalf("unexpected result %#v", result)
		}
		if strings.Contains(result.Data+result.Summary, "sentinel-secret") {
			t.Fatal("secret leaked")
		}
	}
	before := reads.Load()
	matches, err := provider.Search(context.Background(), "Adoption")
	if err != nil || len(matches) != 2 {
		t.Fatalf("search = %#v %v", matches, err)
	}
	if reads.Load() != before {
		t.Fatal("heading cache did not avoid file reads")
	}
	commit = shaB
	if _, err := provider.Search(context.Background(), "Adoption"); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != before+2 {
		t.Fatal("new commit reused old index")
	}
	matches, err = provider.Search(context.Background(), "hidden")
	if err != nil || len(matches) != 0 {
		t.Fatalf("indexed fenced code: %#v %v", matches, err)
	}
}
func TestKnowledgeArgumentValidationAndAuth(t *testing.T) {
	requests := 0
	client := fake(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(401)
		fmt.Fprint(w, "sentinel-secret denied")
	})
	provider := New(config.KnowledgeBase{Repo: "acme/docs", Branch: "main"}, "https://github.test", func(string) string { return "sentinel-secret" }, client)
	for _, path := range []string{"../README.md", "a/../README.md", "/README.md", "a\\README.md", "a\x00.md", "./README.md", "config.yaml"} {
		if _, err := provider.Read(context.Background(), path); err == nil {
			t.Errorf("accepted %q", path)
		}
	}
	for _, tool := range provider.Tools() {
		if _, err := tool.Run(context.Background(), json.RawMessage(`{"unknown":true}`)); err == nil {
			t.Errorf("%s accepted unknown args", tool.Definition().Name)
		}
	}
	if requests != 0 {
		t.Fatal("invalid args reached GitHub")
	}
	for _, tool := range provider.Tools() {
		args := map[string]string{"kb_list": `{}`, "kb_read": `{"path":"README.md"}`, "kb_search": `{"query":"hello"}`}[tool.Definition().Name]
		result, err := tool.Run(context.Background(), json.RawMessage(args))
		if err == nil {
			t.Fatal("expected auth failure")
		}
		if strings.Contains(fmt.Sprint(result, err), "sentinel-secret") {
			t.Fatal("credential leaked on error")
		}
	}
	if _, err := provider.Health(context.Background()); err == nil {
		t.Fatal("auth failure reported healthy")
	}
}
func TestKnowledgeIndexBoundsAndCancellation(t *testing.T) {
	reads := 0
	client := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			fmt.Fprintf(w, `{"sha":%q}`, shaA)
		case strings.Contains(r.URL.Path, "/git/trees/"):
			entries := make([]map[string]any, 300)
			for i := range entries {
				entries[i] = map[string]any{"path": fmt.Sprintf("%d.md", i), "type": "blob"}
			}
			json.NewEncoder(w).Encode(map[string]any{"tree": entries})
		case strings.Contains(r.URL.Path, "/contents/"):
			reads++
			fmt.Fprint(w, `{"type":"file","encoding":"base64","content":"IyBoZWxsbw=="}`)
		default:
			t.Fatalf("unexpected %s", r.URL)
		}
	})
	provider := New(config.KnowledgeBase{Repo: "acme/bounded", Branch: "main"}, "https://github.test", func(string) string { return "sentinel-secret" }, client)
	matches, err := provider.Search(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if reads != 250 || len(matches) != 100 {
		t.Fatalf("bounds reads=%d matches=%d", reads, len(matches))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Read(ctx, "README.md"); err == nil {
		t.Fatal("canceled read succeeded")
	}
}
