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
	if requests.Load() != 2 {
		t.Fatalf("health not cached: %d", requests.Load())
	}
	now = now.Add(11 * time.Second)
	_, _, _ = deps.checkSources(context.Background(), "ws", cfg, true)
	if requests.Load() != 4 {
		t.Fatal("refresh did not run live checks")
	}
	if cfg.Sources[0].Status != "" || len(cfg.Sources[0].Events) != 0 {
		t.Fatal("configuration mutated")
	}
}
