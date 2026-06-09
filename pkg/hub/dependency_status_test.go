package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type stubDependencyChecker struct {
	status DependencyStatus
	err    error
}

func (c stubDependencyChecker) CheckDependencyStatus(context.Context, dependencyStatusTarget) (DependencyStatus, error) {
	return c.status, c.err
}

type blockingDependencyChecker struct {
	calls   atomic.Int32
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (c *blockingDependencyChecker) CheckDependencyStatus(ctx context.Context, target dependencyStatusTarget) (DependencyStatus, error) {
	c.calls.Add(1)
	c.once.Do(func() { close(c.started) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return DependencyStatus{}, ctx.Err()
	}
	return DependencyStatus{
		ID:     target.ID,
		Name:   target.Name,
		Kind:   target.Kind,
		Status: dependencyStatusDowntime,
	}, nil
}

func TestDependencyStatusDiscoversConfiguredDependencies(t *testing.T) {
	service := newDependencyStatusService(&types.HubConfig{
		Providers: map[string]types.ProviderConfig{
			"primary":   {Type: "replicated", Token: "token"},
			"secondary": {Type: "replicated", Token: "other-token"},
			"local":     {Type: "docker"},
		},
		LLMKeys: types.LLMKeysList{
			{Name: "anthropic-main", Provider: "anthropic", APIKey: "sk-test"},
			{Name: "anthropic-backup", Provider: "anthropic", APIKey: "sk-test-2"},
			{Name: "openai", Provider: "openai", APIKey: "sk-test"},
		},
		GitHubApps: []*types.GitHubAppConfig{{AppID: 123}},
		Integrations: &types.IntegrationsConfig{
			Linear:       []*types.LinearIntegrationConfig{{Workspace: "eng", Token: "lin"}},
			Shortcut:     []*types.ShortcutIntegrationConfig{{Workspace: "product", Token: "sc"}},
			GitHubIssues: []*types.GitHubIssuesIntegrationConfig{{Workspace: "org", Token: "ghp"}},
		},
	})

	targets := service.discoverTargets()
	got := make([]string, 0, len(targets))
	for _, target := range targets {
		got = append(got, target.ID)
	}
	want := []string{
		"issue_tracker:github",
		"issue_tracker:linear",
		"issue_tracker:shortcut",
		"model:anthropic",
		"model:openai",
		"sandbox:replicated",
	}
	if len(got) != len(want) {
		t.Fatalf("targets = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets = %#v, want %#v", got, want)
		}
	}
}

func TestDependencyStatusFailureIsUnknownAndNotDowntime(t *testing.T) {
	service := newDependencyStatusService(&types.HubConfig{
		LLMKeys: types.LLMKeysList{{Name: "anthropic", Provider: "anthropic", APIKey: "sk-test"}},
	})
	service.checkers["model:anthropic"] = stubDependencyChecker{err: errors.New("status page unavailable")}

	resp := service.snapshot(context.Background())
	if resp.DowntimeCount != 0 {
		t.Fatalf("DowntimeCount = %d, want 0", resp.DowntimeCount)
	}
	if len(resp.Dependencies) != 1 {
		t.Fatalf("dependencies len = %d, want 1", len(resp.Dependencies))
	}
	dep := resp.Dependencies[0]
	if dep.Status != dependencyStatusUnknown {
		t.Fatalf("status = %q, want %q", dep.Status, dependencyStatusUnknown)
	}
	if dep.Name != "Anthropic" {
		t.Fatalf("name = %q, want Anthropic", dep.Name)
	}
}

func TestDependencyStatusKeepsLastDowntimeSnapshotWhenRefreshFails(t *testing.T) {
	service := newDependencyStatusService(&types.HubConfig{
		LLMKeys: types.LLMKeysList{{Name: "anthropic", Provider: "anthropic", APIKey: "sk-test"}},
	})
	service.cacheTTL = time.Nanosecond
	service.checkers["model:anthropic"] = stubDependencyChecker{status: DependencyStatus{
		ID:     "model:anthropic",
		Name:   "Anthropic",
		Kind:   dependencyKindModel,
		Status: dependencyStatusDowntime,
	}}

	first := service.snapshot(context.Background())
	if first.DowntimeCount != 1 {
		t.Fatalf("first DowntimeCount = %d, want 1", first.DowntimeCount)
	}

	time.Sleep(time.Millisecond)
	service.checkers["model:anthropic"] = stubDependencyChecker{err: errors.New("network down")}
	second := service.snapshot(context.Background())
	if second.DowntimeCount != 1 {
		t.Fatalf("second DowntimeCount = %d, want cached 1", second.DowntimeCount)
	}
	if second.Dependencies[0].Status != dependencyStatusDowntime {
		t.Fatalf("status = %q, want cached downtime", second.Dependencies[0].Status)
	}
}

func TestDependencyStatusCoalescesConcurrentRefreshes(t *testing.T) {
	service := newDependencyStatusService(&types.HubConfig{
		LLMKeys: types.LLMKeysList{{Name: "anthropic", Provider: "anthropic", APIKey: "sk-test"}},
	})
	checker := &blockingDependencyChecker{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service.checkers["model:anthropic"] = checker

	const callers = 20
	start := make(chan struct{})
	results := make(chan DependencyStatusResponse, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- service.snapshot(context.Background())
		}()
	}

	close(start)
	select {
	case <-checker.started:
	case <-time.After(time.Second):
		t.Fatal("checker was not called")
	}
	time.Sleep(25 * time.Millisecond)
	close(checker.release)
	wg.Wait()
	close(results)

	if got := checker.calls.Load(); got != 1 {
		t.Fatalf("checker calls = %d, want 1", got)
	}
	for resp := range results {
		if resp.DowntimeCount != 1 {
			t.Fatalf("DowntimeCount = %d, want 1", resp.DowntimeCount)
		}
		if len(resp.Dependencies) != 1 || resp.Dependencies[0].Status != dependencyStatusDowntime {
			t.Fatalf("dependencies = %#v, want one downtime dependency", resp.Dependencies)
		}
	}
}

func TestDependencyStatusEndpointRequiresAuthAndReturnsSnapshot(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{
		Token:   "test-token",
		LLMKeys: types.LLMKeysList{{Name: "anthropic", Provider: "anthropic", APIKey: "sk-test"}},
	}, "", "", "")
	s.dependencyStatus = newDependencyStatusService(s.hubCfg)
	s.dependencyStatus.checkers["model:anthropic"] = stubDependencyChecker{status: DependencyStatus{
		ID:     "model:anthropic",
		Name:   "Anthropic",
		Kind:   dependencyKindModel,
		Status: dependencyStatusDowntime,
	}}

	unauthorized := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/dependencies/status", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/dependencies/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	s.Handler().ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, want 200: %s", authorized.Code, authorized.Body.String())
	}

	var resp DependencyStatusResponse
	if err := json.NewDecoder(authorized.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.DowntimeCount != 1 {
		t.Fatalf("DowntimeCount = %d, want 1", resp.DowntimeCount)
	}
	if len(resp.Dependencies) != 1 || resp.Dependencies[0].Name != "Anthropic" {
		t.Fatalf("dependencies = %#v, want Anthropic", resp.Dependencies)
	}
}
