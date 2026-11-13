package hub

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// appSlugRetryAfter spaces out GET /app lookups after one failed, so a
// misconfigured App does not cost a request on every bot check.
const appSlugRetryAfter = 5 * time.Minute

type appSlugEntry struct {
	slug     string
	failedAt time.Time
}

// ownAppSlugs returns the slug of every configured GitHub App. A configured
// App URL wins; otherwise the slug is resolved from GET /app and cached per
// app id (managed github_apps.yaml files often carry only app_id). It may call
// GitHub, so it must not be called with s.mu held.
func (s *Server) ownAppSlugs() []string {
	apps := s.githubAppConfigsForTokens()
	var slugs []string
	named := map[int64]bool{}
	for _, app := range apps {
		if slug := appSlugFromURL(app.URL); slug != "" {
			slugs = append(slugs, slug)
			named[app.AppID] = true
		}
	}
	unnamed := map[int64][]*types.GitHubAppConfig{}
	var ids []int64
	for _, app := range apps {
		if named[app.AppID] {
			continue
		}
		if _, seen := unnamed[app.AppID]; !seen {
			ids = append(ids, app.AppID)
		}
		unnamed[app.AppID] = append(unnamed[app.AppID], app)
	}
	for _, id := range ids {
		if slug := s.resolveAppSlug(id, unnamed[id]); slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

// appSlugFromURL extracts the slug from https://github.com/apps/<slug>,
// https://github.com/settings/apps/<slug>, or an org settings URL.
func appSlugFromURL(appURL string) string {
	u := strings.TrimSuffix(appURL, "/")
	return u[strings.LastIndex(u, "/")+1:]
}

// resolveAppSlug returns the cached slug for an App, looking it up through
// each of its configs in turn (the same app id may be configured with an
// unusable key in one place and a valid one in another).
func (s *Server) resolveAppSlug(appID int64, apps []*types.GitHubAppConfig) string {
	s.appSlugMu.Lock()
	entry, ok := s.appSlugs[appID]
	s.appSlugMu.Unlock()
	if ok && (entry.slug != "" || time.Since(entry.failedAt) < appSlugRetryAfter) {
		return entry.slug
	}
	var slug string
	var lastErr error
	for _, app := range apps {
		provider, err := NewGitHubTokenProvider(app)
		if err != nil {
			lastErr = err
			continue
		}
		if s.githubBaseURL != "" {
			provider.apiBaseURL = s.githubBaseURL
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		slug, lastErr = provider.AppSlug(ctx)
		cancel()
		if lastErr == nil {
			break
		}
	}
	entry = appSlugEntry{slug: slug}
	if slug == "" {
		log.Printf("[github] resolve slug for app %d: %v (retrying in %s)", appID, lastErr, appSlugRetryAfter)
		entry.failedAt = time.Now()
	}
	s.appSlugMu.Lock()
	if s.appSlugs == nil {
		s.appSlugs = map[int64]appSlugEntry{}
	}
	s.appSlugs[appID] = entry
	s.appSlugMu.Unlock()
	return slug
}
