package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"golang.org/x/sync/singleflight"
)

const (
	dependencyKindModel        = "model"
	dependencyKindSandbox      = "sandbox"
	dependencyKindIssueTracker = "issue_tracker"

	dependencyStatusOperational = "operational"
	dependencyStatusDegraded    = "degraded"
	dependencyStatusDowntime    = "downtime"
	dependencyStatusUnknown     = "unknown"
)

type DependencyStatus struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Message   string    `json:"message,omitempty"`
	Source    string    `json:"source,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

type DependencyStatusResponse struct {
	Dependencies  []DependencyStatus `json:"dependencies"`
	DowntimeCount int                `json:"downtimeCount"`
	CheckedAt     time.Time          `json:"checkedAt"`
}

type dependencyStatusTarget struct {
	ID     string
	Name   string
	Kind   string
	Source string
}

type dependencyStatusChecker interface {
	CheckDependencyStatus(context.Context, dependencyStatusTarget) (DependencyStatus, error)
}

type dependencyStatusTargetProvider func() []dependencyStatusTarget

type dependencyStatusService struct {
	mu             sync.Mutex
	hubCfg         *types.HubConfig
	checkers       map[string]dependencyStatusChecker
	targetProvider dependencyStatusTargetProvider
	cache          *DependencyStatusResponse
	cacheTTL       time.Duration
	refresh        singleflight.Group
}

func newDependencyStatusService(cfg *types.HubConfig) *dependencyStatusService {
	if cfg == nil {
		cfg = &types.HubConfig{}
	}
	checker := newStatusPageDependencyChecker(http.DefaultClient)
	return &dependencyStatusService{
		hubCfg: cfg,
		checkers: map[string]dependencyStatusChecker{
			"model:anthropic":          checker.withURL("https://status.anthropic.com/api/v2/status.json"),
			"model:openai":             checker.withURL("https://status.openai.com/api/v2/status.json"),
			"model:fireworks":          checker.withURL("https://status.fireworks.ai/api/v2/status.json"),
			"sandbox:daytona":          checker.withURL("https://status.daytona.io/api/v2/status.json"),
			"sandbox:replicated":       checker.withURL("https://status.replicated.com/api/v2/status.json"),
			"issue_tracker:linear":     checker.withURL("https://status.linear.app/api/v2/status.json"),
			"issue_tracker:shortcut":   checker.withURL("https://status.shortcut.com/api/v2/status.json"),
			"issue_tracker:github":     checker.withURL("https://www.githubstatus.com/api/v2/status.json"),
			"issue_tracker:github-app": checker.withURL("https://www.githubstatus.com/api/v2/status.json"),
		},
		cacheTTL: 5 * time.Minute,
	}
}

func (s *dependencyStatusService) snapshot(ctx context.Context) DependencyStatusResponse {
	if resp, ok := s.freshSnapshot(); ok {
		return resp
	}

	value, _, _ := s.refresh.Do("snapshot", func() (interface{}, error) {
		if resp, ok := s.freshSnapshot(); ok {
			return resp, nil
		}

		return s.refreshSnapshot(ctx), nil
	})
	if resp, ok := value.(DependencyStatusResponse); ok {
		return cloneDependencyStatusResponse(resp)
	}

	return DependencyStatusResponse{Dependencies: []DependencyStatus{}, CheckedAt: time.Now().UTC()}
}

func (s *dependencyStatusService) freshSnapshot() (DependencyStatusResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil && time.Since(s.cache.CheckedAt) < s.cacheTTL {
		resp := cloneDependencyStatusResponse(*s.cache)
		return resp, true
	}
	return DependencyStatusResponse{}, false
}

func (s *dependencyStatusService) cachedSnapshot() *DependencyStatusResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache != nil {
		copy := cloneDependencyStatusResponse(*s.cache)
		return &copy
	}
	return nil
}

func (s *dependencyStatusService) refreshSnapshot(ctx context.Context) DependencyStatusResponse {
	cached := s.cachedSnapshot()
	targets := s.targets()
	if len(targets) == 0 {
		resp := DependencyStatusResponse{Dependencies: []DependencyStatus{}, CheckedAt: time.Now().UTC()}
		s.storeSnapshot(resp)
		return resp
	}

	dependencies, anyCheckerSuccess := s.checkTargets(ctx, targets)
	resp := buildDependencyStatusResponse(dependencies)
	if !anyCheckerSuccess && cached != nil {
		return cloneDependencyStatusResponse(*cached)
	}
	s.storeSnapshot(resp)
	return resp
}

func (s *dependencyStatusService) targets() []dependencyStatusTarget {
	if s.targetProvider != nil {
		return append([]dependencyStatusTarget(nil), s.targetProvider()...)
	}
	return s.discoverTargets()
}

func (s *dependencyStatusService) storeSnapshot(resp DependencyStatusResponse) {
	s.mu.Lock()
	copy := cloneDependencyStatusResponse(resp)
	s.cache = &copy
	s.mu.Unlock()
}

func (s *dependencyStatusService) discoverTargets() []dependencyStatusTarget {
	seen := map[string]dependencyStatusTarget{}
	add := func(id, name, kind string) {
		if id == "" || name == "" || kind == "" {
			return
		}
		if _, ok := seen[id]; !ok {
			seen[id] = dependencyStatusTarget{ID: id, Name: name, Kind: kind}
		}
	}

	if s.hubCfg != nil {
		for _, key := range s.hubCfg.LLMKeys {
			if key == nil {
				continue
			}
			if id, name, ok := modelDependency(strings.TrimSpace(key.Provider)); ok {
				add(id, name, dependencyKindModel)
			}
		}
		if provider := providerFromModel(s.hubCfg.DefaultModel); provider != "" {
			if id, name, ok := modelDependency(provider); ok {
				add(id, name, dependencyKindModel)
			}
		}
		for name, cfg := range s.hubCfg.Providers {
			providerType := strings.TrimSpace(cfg.Type)
			if providerType == "" {
				providerType = name
			}
			if id, depName, ok := sandboxDependency(providerType); ok {
				add(id, depName, dependencyKindSandbox)
			}
		}
		if len(s.hubCfg.GitHubApps) > 0 {
			add("issue_tracker:github", "GitHub", dependencyKindIssueTracker)
		}
		if s.hubCfg.Integrations != nil {
			if len(s.hubCfg.Integrations.Linear) > 0 {
				add("issue_tracker:linear", "Linear", dependencyKindIssueTracker)
			}
			if len(s.hubCfg.Integrations.Shortcut) > 0 {
				add("issue_tracker:shortcut", "Shortcut", dependencyKindIssueTracker)
			}
			if len(s.hubCfg.Integrations.GitHubIssues) > 0 {
				add("issue_tracker:github", "GitHub", dependencyKindIssueTracker)
			}
		}
	}

	for _, workspace := range externalWorkspaceNames() {
		trackers, err := loadWorkspaceIssueTrackers(workspace)
		if err != nil {
			continue
		}
		if len(trackers.Linear) > 0 {
			add("issue_tracker:linear", "Linear", dependencyKindIssueTracker)
		}
		if len(trackers.Shortcut) > 0 {
			add("issue_tracker:shortcut", "Shortcut", dependencyKindIssueTracker)
		}
		if len(trackers.GitHubIssues) > 0 {
			add("issue_tracker:github", "GitHub", dependencyKindIssueTracker)
		}
		if apps, err := loadWorkspaceGitHubApps(workspace); err == nil && len(apps) > 0 {
			add("issue_tracker:github", "GitHub", dependencyKindIssueTracker)
		}
	}

	targets := make([]dependencyStatusTarget, 0, len(seen))
	for _, target := range seen {
		targets = append(targets, target)
	}
	sortDependencyTargets(targets)
	return targets
}

func (s *dependencyStatusService) checkTargets(ctx context.Context, targets []dependencyStatusTarget) ([]DependencyStatus, bool) {
	type result struct {
		index  int
		status DependencyStatus
		ok     bool
	}
	results := make([]DependencyStatus, len(targets))
	ch := make(chan result, len(targets))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup

	for i, target := range targets {
		wg.Add(1)
		go func(i int, target dependencyStatusTarget) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			checker, ok := s.checkers[target.ID]
			if !ok {
				ch <- result{index: i, status: unknownDependencyStatus(target, "no status checker configured"), ok: false}
				return
			}
			checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			status, err := checker.CheckDependencyStatus(checkCtx, target)
			if err != nil {
				ch <- result{index: i, status: unknownDependencyStatus(target, err.Error()), ok: false}
				return
			}
			status = normalizeDependencyStatus(target, status)
			ch <- result{index: i, status: status, ok: true}
		}(i, target)
	}

	wg.Wait()
	close(ch)

	anySuccess := false
	for res := range ch {
		results[res.index] = res.status
		if res.ok {
			anySuccess = true
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Kind != results[j].Kind {
			return results[i].Kind < results[j].Kind
		}
		return results[i].Name < results[j].Name
	})
	return results, anySuccess
}

func (s *Server) handleDependencyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.RLock()
	service := s.dependencyStatus
	cfg := s.hubCfg
	s.mu.RUnlock()
	if service == nil {
		service = newDependencyStatusService(cfg)
		s.mu.Lock()
		if s.dependencyStatus == nil {
			s.dependencyStatus = service
		} else {
			service = s.dependencyStatus
		}
		s.mu.Unlock()
	}
	jsonOK(w, service.snapshot(r.Context()))
}

func buildDependencyStatusResponse(dependencies []DependencyStatus) DependencyStatusResponse {
	now := time.Now().UTC()
	out := make([]DependencyStatus, len(dependencies))
	copy(out, dependencies)
	count := 0
	for i := range out {
		if out[i].CheckedAt.IsZero() {
			out[i].CheckedAt = now
		}
		if out[i].Status == dependencyStatusDowntime {
			count++
		}
	}
	return DependencyStatusResponse{Dependencies: out, DowntimeCount: count, CheckedAt: now}
}

func cloneDependencyStatusResponse(resp DependencyStatusResponse) DependencyStatusResponse {
	resp.Dependencies = append([]DependencyStatus(nil), resp.Dependencies...)
	return resp
}

func normalizeDependencyStatus(target dependencyStatusTarget, status DependencyStatus) DependencyStatus {
	if status.ID == "" {
		status.ID = target.ID
	}
	if status.Name == "" {
		status.Name = target.Name
	}
	if status.Kind == "" {
		status.Kind = target.Kind
	}
	if status.Status == "" {
		status.Status = dependencyStatusUnknown
	}
	if status.CheckedAt.IsZero() {
		status.CheckedAt = time.Now().UTC()
	}
	return status
}

func unknownDependencyStatus(target dependencyStatusTarget, message string) DependencyStatus {
	return DependencyStatus{
		ID:        target.ID,
		Name:      target.Name,
		Kind:      target.Kind,
		Status:    dependencyStatusUnknown,
		Message:   message,
		CheckedAt: time.Now().UTC(),
	}
}

func sortDependencyTargets(targets []dependencyStatusTarget) {
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Kind != targets[j].Kind {
			return targets[i].Kind < targets[j].Kind
		}
		if targets[i].Name != targets[j].Name {
			return targets[i].Name < targets[j].Name
		}
		return targets[i].ID < targets[j].ID
	})
}

func modelDependency(provider string) (id, name string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		return "model:anthropic", "Anthropic", true
	case "openai", "codex":
		return "model:openai", "OpenAI", true
	case "fireworks":
		return "model:fireworks", "Fireworks", true
	default:
		return "", "", false
	}
}

func sandboxDependency(provider string) (id, name string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "daytona":
		return "sandbox:daytona", "Daytona", true
	case "replicated":
		return "sandbox:replicated", "Replicated", true
	default:
		return "", "", false
	}
}

func providerFromModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if idx := strings.Index(model, "/"); idx > 0 {
		return model[:idx]
	}
	return ""
}

func externalWorkspaceNames() []string {
	workspaces, err := loadExternalWorkspaces()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(workspaces))
	for _, workspace := range workspaces {
		if workspace != nil && workspace.Name != "" {
			names = append(names, workspace.Name)
		}
	}
	sort.Strings(names)
	return names
}

type statusPageDependencyChecker struct {
	client *http.Client
	url    string
}

func newStatusPageDependencyChecker(client *http.Client) statusPageDependencyChecker {
	if client == nil {
		client = http.DefaultClient
	}
	return statusPageDependencyChecker{client: client}
}

func (c statusPageDependencyChecker) withURL(url string) statusPageDependencyChecker {
	c.url = url
	return c
}

func (c statusPageDependencyChecker) CheckDependencyStatus(ctx context.Context, target dependencyStatusTarget) (DependencyStatus, error) {
	if c.url == "" {
		return DependencyStatus{}, fmt.Errorf("status URL not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return DependencyStatus{}, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return DependencyStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DependencyStatus{}, fmt.Errorf("status page returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Status struct {
			Indicator   string `json:"indicator"`
			Description string `json:"description"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return DependencyStatus{}, err
	}
	status := statusPageIndicatorToDependencyStatus(payload.Status.Indicator)
	return DependencyStatus{
		ID:        target.ID,
		Name:      target.Name,
		Kind:      target.Kind,
		Status:    status,
		Message:   payload.Status.Description,
		Source:    c.url,
		CheckedAt: time.Now().UTC(),
	}, nil
}

func statusPageIndicatorToDependencyStatus(indicator string) string {
	switch strings.ToLower(strings.TrimSpace(indicator)) {
	case "none":
		return dependencyStatusOperational
	case "minor":
		return dependencyStatusDegraded
	case "major", "critical":
		return dependencyStatusDowntime
	default:
		return dependencyStatusUnknown
	}
}
