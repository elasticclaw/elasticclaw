package kb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	provider := New(config.KnowledgeBase{Repo: "acme/docs", Entry: "README.md"}, t.Name(), func(string) string { return "sentinel-secret" }, client)
	provider.client.BaseURL = "https://" + strings.ToLower(t.Name()) + ".test"
	detail, err := provider.Health(context.Background())
	if err != nil || detail != "acme/docs · README.md" {
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
	provider := New(config.KnowledgeBase{Repo: "acme/docs", Branch: "main", Entry: "README.md"}, "https://github.test", func(string) string { return "sentinel-secret" }, client)
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
	var reads atomic.Int32
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
			reads.Add(1)
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
	if reads.Load() != 250 || len(matches) != 100 {
		t.Fatalf("bounds reads=%d matches=%d", reads.Load(), len(matches))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Read(ctx, "README.md"); err == nil {
		t.Fatal("canceled read succeeded")
	}
}

func TestKnowledgeIndexConcurrentFetchAndResume(t *testing.T) {
	var reads atomic.Int32
	var active atomic.Int32
	var peak atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, indexWorkers)
	release := make(chan struct{})
	client := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			fmt.Fprintf(w, `{"sha":%q}`, shaA)
		case strings.Contains(r.URL.Path, "/git/trees/"):
			entries := make([]map[string]any, 2*indexWorkers)
			for i := range entries {
				entries[i] = map[string]any{"path": fmt.Sprintf("%d.md", i), "type": "blob"}
			}
			json.NewEncoder(w).Encode(map[string]any{"tree": entries})
		case strings.Contains(r.URL.Path, "/contents/"):
			n := reads.Add(1)
			current := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); current > old; old = peak.Load() {
				if peak.CompareAndSwap(old, current) {
					break
				}
			}
			if n <= indexWorkers {
				started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			} else if n <= 2*indexWorkers && ctx.Err() == nil {
				cancel()
			}
			fmt.Fprint(w, `{"type":"file","encoding":"base64","content":"IyBoZWxsbw=="}`)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
	provider := New(config.KnowledgeBase{Repo: "acme/resume", Branch: "main"}, "https://github.test", func(string) string { return "sentinel-secret" }, client)
	finished := make(chan error, 1)
	go func() { _, err := provider.Search(ctx, "hello"); finished <- err }()
	for range indexWorkers {
		select {
		case <-started:
		case <-time.After(time.Second):
			cancel()
			<-finished
			t.Fatal("file reads were not concurrent")
		}
	}
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	before := reads.Load()
	matches, err := provider.Search(context.Background(), "hello")
	if err != nil || len(matches) != 2*indexWorkers || reads.Load()-before != indexWorkers {
		t.Fatalf("resume: matches=%d reads=%d err=%v", len(matches), reads.Load()-before, err)
	}
	if peak.Load() != indexWorkers {
		t.Fatalf("concurrency = %d", peak.Load())
	}
}

func TestKnowledgeIndexReusesCommitAcrossCredentials(t *testing.T) {
	var reads atomic.Int32
	client := fake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			fmt.Fprintf(w, `{"sha":%q}`, shaA)
		case strings.Contains(r.URL.Path, "/git/trees/"):
			fmt.Fprint(w, `{"tree":[{"path":"next-secret.md","type":"blob"}]}`)
		case strings.Contains(r.URL.Path, "/contents/"):
			reads.Add(1)
			content := base64.StdEncoding.EncodeToString([]byte("# hello sentinel-secret next-secret"))
			fmt.Fprintf(w, `{"type":"file","encoding":"base64","content":%q}`, content)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})
	provider := New(config.KnowledgeBase{Repo: "acme/rotation", Branch: "main"}, "https://github.test", func(string) string { return "sentinel-secret" }, client)
	if _, err := provider.Search(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	provider.client.Token = func(string) string { return "next-secret" }
	original := client.Transport
	client.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer next-secret" {
			t.Error("credential not rotated")
		}
		r.Header.Set("Authorization", "Bearer sentinel-secret")
		return original.RoundTrip(r)
	})
	matches, err := provider.Search(context.Background(), "hello")
	if err != nil || len(matches) != 1 || reads.Load() != 1 {
		t.Fatalf("cache reused: matches=%v reads=%d err=%v", matches, reads.Load(), err)
	}
	if strings.Contains(fmt.Sprint(matches), "sentinel-secret") || strings.Contains(fmt.Sprint(matches), "next-secret") {
		t.Fatal("credential leaked from cached index")
	}
}
