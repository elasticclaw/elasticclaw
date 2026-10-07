package aichat

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/analytics"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/kb"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/repos"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tracker"
)

const healthTimeout = 5 * time.Second
const healthTTL = 5 * time.Minute
const refreshInterval = 10 * time.Second

type healthResult struct {
	detail  string
	events  []string
	err     error
	checked time.Time
}
type healthEntry struct {
	result  healthResult
	started time.Time
	done    chan struct{}
}
type sourceHealth struct {
	mu      sync.Mutex
	entries map[string]*healthEntry
	pending map[string]*candidatePending
	now     func() time.Time
}

func newSourceHealth() *sourceHealth {
	return &sourceHealth{entries: make(map[string]*healthEntry), pending: make(map[string]*candidatePending), now: time.Now}
}

// One in-flight request per source also limits simultaneous refreshes and turns.
func (h *sourceHealth) check(ctx context.Context, key string, refresh bool, check func(context.Context) healthResult) healthResult {
	h.mu.Lock()
	now := h.now()
	if entry := h.entries[key]; entry != nil {
		if entry.done != nil {
			done := entry.done
			h.mu.Unlock()
			select {
			case <-done:
				return entry.result
			case <-ctx.Done():
				return healthResult{err: ctx.Err(), checked: now}
			}
		}
		age := now.Sub(entry.started)
		if age < refreshInterval || (!refresh && age < healthTTL) {
			result := entry.result
			h.mu.Unlock()
			return result
		}
	}
	// Bound cache size when workspaces or source configurations change frequently.
	if len(h.entries) >= 256 {
		for k, e := range h.entries {
			if e.done == nil {
				delete(h.entries, k)
				if len(h.entries) < 256 {
					break
				}
			}
		}
	}
	entry := &healthEntry{started: now, done: make(chan struct{})}
	h.entries[key] = entry
	h.mu.Unlock()
	result := check(ctx)
	result.checked = now
	h.mu.Lock()
	entry.result = result
	close(entry.done)
	entry.done = nil
	h.mu.Unlock()
	return result
}

type sourceCandidate struct {
	tools   []tools.Tool
	secrets []string
	detail  string
	err     error
	check   func(context.Context) healthResult
}

func (d Deps) candidate(workspace string, cfg *config.Config, source config.Source) sourceCandidate {
	c := sourceCandidate{}
	secret := func(name string) string {
		value, ok := "", false
		if d.Secret != nil {
			value, ok = d.Secret(workspace, name)
		}
		if !ok || value == "" {
			c.err = fmt.Errorf("Secret %s is not set.", name)
		}
		c.secrets = append(c.secrets, value)
		return value
	}
	githubToken := func(repo string) string {
		value := ""
		if d.GitHubToken != nil {
			value = d.GitHubToken(repo)
		}
		if value == "" {
			c.err = errors.New("GitHub read access is not configured.")
		}
		c.secrets = append(c.secrets, value)
		return value
	}
	switch source.Kind {
	case "knowledge_base":
		if cfg.KnowledgeBase == nil {
			break
		}
		v := *cfg.KnowledgeBase
		c.detail = v.Repo
		token := githubToken(v.Repo)
		provider := kb.New(v, d.GitHubAPI, func(string) string { return token }, d.HTTPClient)
		c.tools = provider.Tools()
		c.check = func(ctx context.Context) healthResult {
			detail, err := provider.Health(ctx)
			return healthResult{detail: detail, err: err}
		}
	case "repositories":
		c.detail = strings.Join(cfg.Repositories, ", ") + " · default branch"
		tokens := make(map[string]string)
		for _, repo := range cfg.Repositories {
			tokens[repo] = githubToken(repo)
		}
		provider := repos.New(cfg.Repositories, d.GitHubAPI, func(repo string) string { return tokens[repo] }, d.HTTPClient)
		c.tools = provider.Tools()
		c.check = func(ctx context.Context) healthResult {
			detail, err := provider.Health(ctx)
			return healthResult{detail: detail, err: err}
		}
	case "posthog":
		if cfg.PostHog == nil {
			break
		}
		v := *cfg.PostHog
		c.detail = "Project " + v.ProjectID + " · events and saved insights"
		provider := analytics.NewPostHog(v, secret(v.APIKey), d.HTTPClient)
		c.tools = provider.Tools()
		c.check = func(ctx context.Context) healthResult {
			detail, events, err := provider.Health(ctx)
			return healthResult{detail: detail, events: events, err: err}
		}
	case "datadog":
		if cfg.Datadog == nil {
			break
		}
		v := *cfg.Datadog
		c.detail = "Metrics and logs · env " + v.Env
		provider := analytics.NewDatadog(v, secret(v.APIKey), secret(v.AppKey), d.HTTPClient)
		c.tools = provider.Tools()
		c.check = func(ctx context.Context) healthResult {
			detail, err := provider.Health(ctx)
			return healthResult{detail: detail, err: err}
		}
	case "issue_tracker":
		if cfg.IssueTracker == nil {
			break
		}
		v := *cfg.IssueTracker
		c.detail = "Team " + v.DefaultFields.Team + " · reads issues to avoid duplicates"
		token, ok := "", false
		if d.LinearToken != nil {
			token, ok = d.LinearToken(workspace)
		}
		if !ok || token == "" {
			c.err = errors.New("The Linear tracker token is not configured.")
		}
		c.secrets = append(c.secrets, token)
		provider := tracker.NewLinear(v.DefaultFields.Team, token, d.HTTPClient)
		c.tools = provider.Tools()
		c.check = func(ctx context.Context) healthResult {
			detail, err := provider.Health(ctx)
			return healthResult{detail: detail, err: err}
		}
	}
	return c
}

func healthError(err error) string {
	var status *readhttp.StatusError
	if errors.As(err, &status) {
		if status.Code == 401 || status.Code == 403 {
			return "The API key was rejected."
		}
		return fmt.Sprintf("The source returned HTTP %d.", status.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "The source did not respond in time."
	}
	return "Unable to reach this source."
}

func (d Deps) checkSources(ctx context.Context, workspace string, cfg *config.Config, refresh bool) ([]config.Source, []tools.Tool, []string) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	cache := d.health
	if cache == nil {
		cache = newSourceHealth()
	}
	sources := append([]config.Source{}, cfg.Sources...)
	candidates := make([]sourceCandidate, len(sources))
	var wg sync.WaitGroup
	for i := range sources {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := &sources[i]
			s.Access = "read"
			if s.Status == "invalid" {
				return
			}
			raw, _ := json.Marshal(sourceConfiguration(cfg, s.Kind))
			key := fmt.Sprintf("%s\x00%s\x00%x", workspace, s.Kind, sha256.Sum256(raw))
			source := *s
			c := cache.prepare(ctx, key, func() sourceCandidate { return d.candidate(workspace, cfg, source) })
			candidates[i] = c
			s.Detail = c.detail
			s.Status = "unreachable"
			if c.err != nil {
				s.Error = c.err.Error()
				if errors.Is(c.err, context.DeadlineExceeded) || errors.Is(c.err, context.Canceled) {
					s.Error = healthError(c.err)
				}
				s.CheckedAt = time.Now().UTC().Format(time.RFC3339)
				return
			}
			if c.check == nil {
				s.Error = "This source is not supported."
				return
			}
			result := cache.check(ctx, key, refresh, c.check)
			s.CheckedAt = result.checked.UTC().Format(time.RFC3339)
			if result.err != nil {
				s.Error = healthError(result.err)
				return
			}
			s.Status = "connected"
			s.Error = ""
			s.Events = append([]string(nil), result.events...)
			if result.detail != "" {
				s.Detail = result.detail
			}
		}(i)
	}
	wg.Wait()
	var secrets []string
	for _, c := range candidates {
		secrets = append(secrets, c.secrets...)
	}
	var reads []tools.Tool
	for i := range sources {
		s := &sources[i]
		s.Name = readhttp.Redact(s.Name, secrets)
		s.Detail = strings.Join(strings.Fields(readhttp.Redact(s.Detail, secrets)), " ")
		s.Error = strings.Join(strings.Fields(readhttp.Redact(s.Error, secrets)), " ")
		for j := range s.Events {
			s.Events[j] = readhttp.Redact(s.Events[j], secrets)
		}
		if s.Status == "connected" {
			reads = append(reads, candidates[i].tools...)
		}
	}
	return sources, reads, secrets
}

func (d Deps) prepareReads(ctx context.Context, workspace string, cfg *config.Config) (*tools.ReadRegistry, *config.Config, []string) {
	sources, reads, secrets := d.checkSources(ctx, workspace, cfg, false)
	copy := *cfg
	copy.Sources = sources
	registry, _ := tools.NewReadRegistry(reads...)
	return registry, &copy, secrets
}

func sourceConfiguration(cfg *config.Config, kind string) any {
	switch kind {
	case "knowledge_base":
		return cfg.KnowledgeBase
	case "repositories":
		return cfg.Repositories
	case "posthog":
		return cfg.PostHog
	case "datadog":
		return cfg.Datadog
	case "issue_tracker":
		return cfg.IssueTracker
	}
	return nil
}

type candidatePending struct {
	done   chan struct{}
	result sourceCandidate
}

// Hub credential callbacks predate context support. Coalesce them and bound the
// number of blocked callbacks, while allowing each caller's deadline to expire.
func (h *sourceHealth) prepare(ctx context.Context, key string, build func() sourceCandidate) sourceCandidate {
	h.mu.Lock()
	pending := h.pending[key]
	if pending == nil {
		if len(h.pending) >= 256 {
			h.mu.Unlock()
			return sourceCandidate{err: errors.New("Source credentials are temporarily unavailable.")}
		}
		pending = &candidatePending{done: make(chan struct{})}
		h.pending[key] = pending
		go func() {
			result := build()
			h.mu.Lock()
			pending.result = result
			close(pending.done)
			delete(h.pending, key)
			h.mu.Unlock()
		}()
	}
	h.mu.Unlock()
	select {
	case <-ctx.Done():
		return sourceCandidate{err: ctx.Err()}
	case <-pending.done:
		return pending.result
	}
}
