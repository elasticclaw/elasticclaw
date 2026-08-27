package hub

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// githubRateLimitReserve is the slice of the installation quota the background
// pollers refuse to spend, so interactive and agent-initiated calls keep working
// even when the watcher is busy. Override with ELASTICCLAW_GITHUB_RATE_RESERVE.
var githubRateLimitReserve = githubEnvInt("ELASTICCLAW_GITHUB_RATE_RESERVE", 500)

// githubETagCacheMax bounds the conditional-request cache so a long-lived hub
// cannot grow it without limit.
const githubETagCacheMax = 2048

// githubRateLimitFallbackWait is used when GitHub reports a rate limit without
// a usable reset hint.
const githubRateLimitFallbackWait = time.Minute

// githubRateLimitMaxWait clamps how long a single response may pause every
// GitHub call in the hub. Legitimate values never exceed the hourly window, so
// a malformed or hostile Retry-After/X-RateLimit-Reset cannot blind the hub
// until the process restarts.
const githubRateLimitMaxWait = time.Hour

func githubEnvInt(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v >= 0 {
		return v
	}
	return def
}

type githubETagEntry struct {
	etag     string
	body     []byte
	lastUsed time.Time
}

// githubClient is the shared HTTP client for GitHub REST calls. It tracks the
// rate-limit headers GitHub returns on every response and caches ETags so
// unchanged resources come back as 304s, which do not consume quota.
type githubClient struct {
	httpClient *http.Client

	mu    sync.Mutex
	etags map[string]*githubETagEntry

	limit     int
	remaining int
	reset     time.Time
	haveLimit bool
	// blockedUntil is set when GitHub reports the quota exhausted (403/429 with
	// no remaining budget, or a Retry-After). Requests short-circuit until it
	// passes instead of hammering an exhausted quota every tick.
	blockedUntil time.Time
	lastBlockLog time.Time
}

func newGitHubClient() *githubClient {
	return &githubClient{httpClient: http.DefaultClient, etags: map[string]*githubETagEntry{}}
}

// defaultGitHubClient backs the githubAPI* helpers, so every GitHub consumer in
// the hub shares one rate-limit budget and one ETag cache.
var defaultGitHubClient = newGitHubClient()

type githubResponse struct {
	StatusCode int
	Body       []byte
	Header     http.Header
	// NotModified reports that GitHub answered 304 and Body came from the cache.
	NotModified bool
}

// rateLimited mirrors the pre-existing semantics: GitHub says the quota is gone.
func (r *githubResponse) rateLimited() bool {
	return r.Header.Get("X-RateLimit-Remaining") == "0"
}

// get performs a conditional GET. A cached ETag is replayed as If-None-Match,
// and a 304 returns the cached body without spending quota.
func (c *githubClient) get(url, token string) (*githubResponse, error) {
	return c.doGet(url, token, true)
}

func (c *githubClient) getContext(ctx context.Context, url, token string) (*githubResponse, error) {
	return c.doGetContext(ctx, url, token, true)
}

// do sends a GitHub request through the shared rate-limit gate. Callers build
// the request when they need a non-GET method or custom body.
func (c *githubClient) do(req *http.Request) (*githubResponse, error) {
	if until, blocked := c.blockedUntilTime(); blocked {
		return nil, &githubAPIError{StatusCode: http.StatusForbidden, RateLimited: true,
			Body: fmt.Sprintf("github rate limit exhausted; not retrying until %s", until.UTC().Format(time.RFC3339))}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github API response read error: %w", err)
	}
	c.observe(resp.StatusCode, resp.Header, body)
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: string(body), RateLimited: githubIsRateLimited(resp.StatusCode, resp.Header, body)}
	}
	return &githubResponse{StatusCode: resp.StatusCode, Body: body, Header: resp.Header}, nil
}

func (c *githubClient) doGet(url, token string, conditional bool) (*githubResponse, error) {
	return c.doGetContext(context.Background(), url, token, conditional)
}

func (c *githubClient) doGetContext(ctx context.Context, url, token string, conditional bool) (*githubResponse, error) {
	if until, blocked := c.blockedUntilTime(); blocked {
		return nil, &githubAPIError{
			StatusCode:  http.StatusForbidden,
			RateLimited: true,
			Body: fmt.Sprintf("github rate limit exhausted; not retrying until %s",
				until.UTC().Format(time.RFC3339)),
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if conditional {
		if etag := c.cachedETag(url); etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github API response read error: %w", err)
	}
	c.observe(resp.StatusCode, resp.Header, body)

	if resp.StatusCode == http.StatusNotModified {
		if cached, ok := c.cachedBody(url); ok {
			return &githubResponse{StatusCode: http.StatusOK, Body: cached, Header: resp.Header, NotModified: true}, nil
		}
		if !conditional {
			// We sent no validator, so a 304 is a protocol violation and there is
			// nothing to replay. Surface it instead of retrying forever.
			return nil, &githubAPIError{StatusCode: resp.StatusCode, Body: "github returned 304 without a conditional request"}
		}
		// The entry was evicted after the validator was sent; ask again unconditionally.
		c.forget(url)
		return c.doGetContext(ctx, url, token, false)
	}
	if resp.StatusCode == http.StatusOK {
		c.store(url, resp.Header.Get("ETag"), body)
	}
	return &githubResponse{StatusCode: resp.StatusCode, Body: body, Header: resp.Header}, nil
}

// observe records the rate-limit headers GitHub sends on every response and
// arms the backoff when the quota is exhausted.
func (c *githubClient) observe(status int, h http.Header, body []byte) {
	limit, hasLimit := githubHeaderInt(h, "X-RateLimit-Limit")
	remaining, hasRemaining := githubHeaderInt(h, "X-RateLimit-Remaining")
	var reset time.Time
	if secs, ok := githubHeaderInt(h, "X-RateLimit-Reset"); ok {
		reset = time.Unix(int64(secs), 0)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if hasLimit {
		c.limit = limit
	}
	if hasRemaining {
		c.remaining = remaining
		c.haveLimit = true
	}
	if !reset.IsZero() {
		c.reset = reset
	}

	if !githubIsRateLimited(status, h, body) {
		return
	}
	now := time.Now()
	until := now.Add(githubRateLimitFallbackWait)
	if retry, ok := githubHeaderInt(h, "Retry-After"); ok && retry > 0 {
		until = now.Add(time.Duration(retry) * time.Second)
	} else if !reset.IsZero() && reset.After(now) {
		until = reset
	}
	if max := now.Add(githubRateLimitMaxWait); until.After(max) {
		until = max
	}
	if until.After(c.blockedUntil) {
		c.blockedUntil = until
		if time.Since(c.lastBlockLog) >= time.Minute {
			c.lastBlockLog = time.Now()
			log.Printf("[github] rate limit exhausted (limit=%d remaining=%d); pausing GitHub calls until %s",
				c.limit, c.remaining, until.UTC().Format(time.RFC3339))
		}
	}
}

func githubIsRateLimited(status int, h http.Header, body []byte) bool {
	if status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return false
	}
	if h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != "" {
		return true
	}
	return strings.Contains(strings.ToLower(string(body)), "rate limit")
}

func githubHeaderInt(h http.Header, key string) (int, bool) {
	v := strings.TrimSpace(h.Get(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// blockedUntilTime reports whether calls are currently paused, and until when.
func (c *githubClient) blockedUntilTime() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blockedUntil.IsZero() || !time.Now().Before(c.blockedUntil) {
		return time.Time{}, false
	}
	return c.blockedUntil, true
}

// allowLowPriority reports whether background polling may still spend quota.
// It goes false once the remaining budget drops into the reserve, so the
// watcher degrades before it starves the rest of the hub.
func (c *githubClient) allowLowPriority() bool {
	if _, blocked := c.blockedUntilTime(); blocked {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.haveLimit {
		return true
	}
	if c.remaining >= c.effectiveReserveLocked() {
		return true
	}
	// Budget is inside the reserve: stay out until the window resets.
	return !c.reset.IsZero() && !time.Now().Before(c.reset)
}

// effectiveReserveLocked keeps the reserve proportional to the observed limit.
// A token whose hourly limit is at or below githubRateLimitReserve (a classic
// PAT scoped down, or an unauthenticated 60/hour budget) would otherwise sit
// permanently inside the reserve and degrade the watcher to merge-only forever.
func (c *githubClient) effectiveReserveLocked() int {
	reserve := githubRateLimitReserve
	if c.limit > 0 && reserve > c.limit/2 {
		reserve = c.limit / 2
	}
	return reserve
}

// budget reports the last observed quota state for logging.
func (c *githubClient) budget() (limit, remaining int, reset time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit, c.remaining, c.reset, c.haveLimit
}

func (c *githubClient) cachedETag(url string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.etags[url]; ok {
		return e.etag
	}
	return ""
}

func (c *githubClient) cachedBody(url string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.etags[url]
	if !ok {
		return nil, false
	}
	e.lastUsed = time.Now()
	return e.body, true
}

func (c *githubClient) store(url, etag string, body []byte) {
	if etag == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.etags) >= githubETagCacheMax {
		if _, exists := c.etags[url]; !exists {
			c.evictOldestLocked()
		}
	}
	c.etags[url] = &githubETagEntry{etag: etag, body: body, lastUsed: time.Now()}
}

func (c *githubClient) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, e := range c.etags {
		if oldestKey == "" || e.lastUsed.Before(oldest) {
			oldestKey, oldest = k, e.lastUsed
		}
	}
	if oldestKey != "" {
		delete(c.etags, oldestKey)
	}
}

func (c *githubClient) forget(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.etags, url)
}

// reset clears cached state. Tests use it to isolate the package-level client.
func (c *githubClient) resetState() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.etags = map[string]*githubETagEntry{}
	c.limit, c.remaining, c.haveLimit = 0, 0, false
	c.reset = time.Time{}
	c.blockedUntil = time.Time{}
	c.lastBlockLog = time.Time{}
}
