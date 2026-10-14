package aichat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
)

type sourceTransport func(*http.Request) (*http.Response, error)

func (f sourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func sourceClient(handler http.HandlerFunc) *http.Client {
	return &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		handler(rec, r)
		return rec.Result(), nil
	})}
}

func TestHealthCacheAndRefreshThrottle(t *testing.T) {
	cache := newSourceHealth()
	now := time.Now()
	cache.now = func() time.Time { return now }
	calls := 0
	check := func(context.Context) healthResult { calls++; return healthResult{detail: "connected"} }
	ctx := context.Background()
	for _, refresh := range []bool{false, true, false, true} {
		cache.check(ctx, "workspace/source/config", refresh, check)
	}
	if calls != 1 {
		t.Fatalf("refresh bypassed throttle: %d", calls)
	}
	now = now.Add(11 * time.Second)
	cache.check(ctx, "workspace/source/config", false, check)
	if calls != 1 {
		t.Fatal("fresh cache missed")
	}
	cache.check(ctx, "workspace/source/config", true, check)
	if calls != 2 {
		t.Fatal("refresh did not bypass cache")
	}
	now = now.Add(healthTTL)
	cache.check(ctx, "workspace/source/config", false, check)
	cache.check(ctx, "other/source/config", false, check)
	cache.check(ctx, "workspace/source/changed", false, check)
	if calls != 5 {
		t.Fatalf("expiry/workspace/config isolation: %d", calls)
	}
}

func TestHealthConcurrentRefreshCoalesces(t *testing.T) {
	cache := newSourceHealth()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	check := func(context.Context) healthResult {
		calls.Add(1)
		close(started)
		<-release
		return healthResult{detail: "ready"}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); cache.check(context.Background(), "source", true, check) }()
	<-started
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := cache.check(context.Background(), "source", true, check)
			if r.detail != "ready" {
				t.Error(r)
			}
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("live checks = %d", calls.Load())
	}
}

func TestSourceHealthMissingSecretsAndAuth(t *testing.T) {
	cfg := &config.Config{Datadog: &config.Datadog{Site: "datadoghq.com", Env: "prod", APIKey: "DATADOG_API_KEY", AppKey: "DATADOG_APP_KEY"}, Sources: []config.Source{{Kind: "datadog", Name: "Datadog"}}}
	deps := Deps{health: newSourceHealth(), Secret: func(_ string, name string) (string, bool) { return "", false }}
	got, reads, _ := deps.checkSources(context.Background(), "ws", cfg, false)
	if len(reads) != 0 || got[0].Status != "unreachable" || !strings.Contains(got[0].Error, "Secret DATADOG_") || got[0].Access != "read" {
		t.Fatalf("missing: %+v", got)
	}
	deps.Secret = func(string, string) (string, bool) { return "sentinel-secret", true }
	deps.HTTPClient = sourceClient(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401); fmt.Fprint(w, "sentinel-secret") })
	got, reads, _ = deps.checkSources(context.Background(), "ws", cfg, true)
	if len(reads) != 0 || got[0].Status != "unreachable" || got[0].Error != "The API key was rejected." || got[0].CheckedAt == "" {
		t.Fatalf("auth: %+v", got)
	}
	data, _ := json.Marshal(got)
	if strings.Contains(string(data), "sentinel-secret") {
		t.Fatal("secret exposed")
	}
}

func TestRunnerOmitsUnreachableSource(t *testing.T) {
	store := testStore(t)
	thread := testThread(t, store)
	message, err := store.BeginTurn(context.Background(), thread, "Question", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Datadog: &config.Datadog{Site: "datadoghq.com", Env: "prod", APIKey: "API", AppKey: "APP"}, Sources: []config.Source{{Kind: "datadog", Name: "Datadog"}}}
	deps := Deps{health: newSourceHealth()}
	runner := Runner{Store: store, PrepareReads: deps.prepareReads}
	provider := fakeProvider{stream: func(_ context.Context, req llm.Request, _ func(string) error) (llm.Response, error) {
		if len(req.Tools) != 0 {
			t.Fatalf("unreachable source advertised: %+v", req.Tools)
		}
		if !strings.Contains(req.System, "Datadog is unreachable this turn; tell the user answers skip it") || strings.Contains(req.System, "unchecked") {
			t.Fatal(req.System)
		}
		return llm.Response{}, nil
	}}
	if err := runner.Run(context.Background(), thread, message, cfg, provider, func(string, any) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestHealthWaiterHonorsCancellation(t *testing.T) {
	cache := newSourceHealth()
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.check(context.Background(), "source", false, func(context.Context) healthResult { close(started); <-release; return healthResult{} })
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := cache.check(ctx, "source", false, func(context.Context) healthResult { t.Error("duplicate check"); return healthResult{} })
	close(release)
	<-done
	if result.err != context.Canceled {
		t.Fatalf("cancellation: %v", result.err)
	}
}

func TestBlockedCredentialsHonorHealthDeadline(t *testing.T) {
	cache := newSourceHealth()
	release := make(chan struct{})
	finished := make(chan struct{})
	var calls atomic.Int32
	deps := Deps{health: cache, GitHubToken: func(string) string { calls.Add(1); <-release; defer close(finished); return "token" }}
	cfg := &config.Config{Repositories: []string{"acme/app"}, Sources: []config.Source{{Kind: "repositories", Name: "Repositories"}}}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			sources, reads, _ := deps.checkSources(ctx, "ws", cfg, false)
			if len(reads) != 0 || sources[0].Status != "unreachable" || sources[0].Error != "The source did not respond in time." {
				t.Errorf("timeout %+v", sources)
			}
		}()
	}
	wg.Wait()
	close(release)
	<-finished
	if calls.Load() != 1 {
		t.Fatalf("credential callbacks not coalesced: %d", calls.Load())
	}
}

func TestHealthyPostHogCachesEventsAndRefreshes(t *testing.T) {
	cache := newSourceHealth()
	now := time.Now()
	cache.now = func() time.Time { return now }
	var requests atomic.Int32
	deps := Deps{health: cache, Secret: func(string, string) (string, bool) { return "secret", true }, HTTPClient: sourceClient(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.Contains(r.URL.Path, "event_definitions") {
			fmt.Fprint(w, `{"results":[{"name":"meal_swapped"}]}`)
		} else {
			fmt.Fprint(w, `{"name":"Web app"}`)
		}
	})}
	cfg := &config.Config{PostHog: &config.PostHog{Host: "https://posthog.test", ProjectID: "42", APIKey: "PH"}, Sources: []config.Source{{Kind: "posthog", Name: "PostHog"}}}
	for range 2 {
		registry, prepared, _ := deps.prepareReads(context.Background(), "ws", cfg)
		if len(registry.Definitions()) != 2 || prepared.Sources[0].Status != "connected" || !strings.Contains(prompt(prepared, "explore_idea"), "meal_swapped") {
			t.Fatalf("prepared %+v", prepared)
		}
	}
	if requests.Load() != 3 {
		t.Fatalf("health not cached: %d", requests.Load())
	}
	now = now.Add(11 * time.Second)
	_, _, _ = deps.checkSources(context.Background(), "ws", cfg, true)
	if requests.Load() != 6 {
		t.Fatal("refresh did not run live checks")
	}
	if cfg.Sources[0].Status != "" || len(cfg.Sources[0].Events) != 0 {
		t.Fatal("configuration mutated")
	}
}

func TestHealthLeaderCancellationDoesNotPoisonCache(t *testing.T) {
	cache := newSourceHealth()
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan healthResult, 1)
	go func() {
		finished <- cache.check(ctx, "source", false, func(checkCtx context.Context) healthResult {
			close(started)
			select {
			case <-release:
				return healthResult{detail: "connected", err: checkCtx.Err()}
			case <-checkCtx.Done():
				return healthResult{err: checkCtx.Err()}
			}
		})
	}()
	<-started
	cancel()
	select {
	case result := <-finished:
		if result.err != context.Canceled {
			t.Errorf("leader cancellation: %v", result.err)
		}
	case <-time.After(time.Second):
		t.Error("leader did not honor cancellation")
	}
	close(release)
	for range 2 {
		result := cache.check(context.Background(), "source", false, func(context.Context) healthResult {
			t.Error("shared check was discarded")
			return healthResult{}
		})
		if result.err != nil || result.detail != "connected" {
			t.Fatalf("poisoned cache: %+v", result)
		}
	}
}

func TestPostHogConnectedWithoutMetadataPermission(t *testing.T) {
	deps := Deps{health: newSourceHealth(), Secret: func(string, string) (string, bool) { return "secret", true }, HTTPClient: sourceClient(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects/42/query/":
			if r.Method != http.MethodPost {
				t.Error("expected query POST")
			}
			fmt.Fprint(w, `{"results":[[1]]}`)
		case "/api/projects/42/insights/":
			fmt.Fprint(w, `{"results":[]}`)
		case "/api/projects/42/", "/api/projects/42/event_definitions/":
			w.WriteHeader(http.StatusForbidden)
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	})}
	cfg := &config.Config{PostHog: &config.PostHog{Host: "https://posthog.test", ProjectID: "42", APIKey: "PH"}, Sources: []config.Source{{Kind: "posthog", Name: "PostHog"}}}
	sources, reads, _ := deps.checkSources(context.Background(), "ws", cfg, false)
	if sources[0].Status != "connected" || len(reads) != 2 || sources[0].Detail != "Project 42 · events and saved insights" {
		t.Fatalf("health: %+v, tools: %d", sources, len(reads))
	}
	for _, tool := range reads {
		args := `{}`
		if tool.Definition().Name == "posthog_query" {
			args = `{"query":"SELECT 1"}`
		}
		if _, err := tool.Run(context.Background(), json.RawMessage(args)); err != nil {
			t.Fatalf("%s: %v", tool.Definition().Name, err)
		}
	}
}

func TestLargeKnowledgeTreeKeepsHealthAndReadAvailable(t *testing.T) {
	var trees atomic.Int32
	deps := Deps{health: newSourceHealth(), GitHubAPI: "https://github.test", GitHubToken: func(string) string { return "secret" }, HTTPClient: sourceClient(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/commits/"):
			fmt.Fprint(w, `{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
		case strings.Contains(r.URL.Path, "/contents/README.md"):
			fmt.Fprint(w, `{"type":"file","encoding":"base64","content":"IyBoZWxsbw=="}`)
		case strings.Contains(r.URL.Path, "/git/trees/"):
			trees.Add(1)
			entries := make([]map[string]any, 5001)
			for i := range entries {
				entries[i] = map[string]any{"path": fmt.Sprintf("%d.txt", i), "type": "blob"}
			}
			json.NewEncoder(w).Encode(map[string]any{"tree": entries})
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	})}
	cfg := &config.Config{KnowledgeBase: &config.KnowledgeBase{Repo: "acme/large", Branch: "main", Entry: "README.md"}, Sources: []config.Source{{Kind: "knowledge_base", Name: "Knowledge base"}}}
	sources, reads, _ := deps.checkSources(context.Background(), "ws", cfg, false)
	if sources[0].Status != "connected" || len(reads) != 3 || trees.Load() != 0 {
		t.Fatalf("health: %+v tools=%d trees=%d", sources, len(reads), trees.Load())
	}
	for _, tool := range reads {
		name := tool.Definition().Name
		args := map[string]string{"kb_read": `{"path":"README.md"}`, "kb_list": `{}`, "kb_search": `{"query":"hello"}`}[name]
		result, err := tool.Run(context.Background(), json.RawMessage(args))
		if name == "kb_read" {
			if err != nil || result.Data != "# hello" {
				t.Fatalf("read: %+v %v", result, err)
			}
		} else if err == nil {
			t.Errorf("%s ignored enumeration limit", name)
		}
	}
}
