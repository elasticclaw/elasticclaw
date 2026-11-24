package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// errBridgeArtifactUnavailable marks a definitively missing connector artifact;
// callers must not retry bootstrap on it.
var errBridgeArtifactUnavailable = errors.New("bridge artifact unavailable")

const bridgePreflightTTL = 3 * time.Minute

type bridgePreflightResult struct {
	missing bool
	expires time.Time
}

type bridgeArtifactPreflight struct {
	client   *http.Client
	mu       sync.Mutex
	cache    map[string]bridgePreflightResult
	inflight singleflight.Group
}

var bootstrapBridgePreflight = bridgeArtifactPreflight{client: &http.Client{Timeout: 10 * time.Second}}

func (p *bridgeArtifactPreflight) check(ctx context.Context, ref string) error {
	result, _, _ := p.inflight.Do(ref, func() (any, error) {
		p.mu.Lock()
		cached, ok := p.cache[ref]
		p.mu.Unlock()
		if ok && time.Now().Before(cached.expires) {
			return cached.missing, nil
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		missing := p.missing(ctx, ref)
		p.mu.Lock()
		if p.cache == nil {
			p.cache = make(map[string]bridgePreflightResult)
		}
		for key, entry := range p.cache {
			if time.Now().After(entry.expires) {
				delete(p.cache, key)
			}
		}
		p.cache[ref] = bridgePreflightResult{missing: missing, expires: time.Now().Add(bridgePreflightTTL)}
		p.mu.Unlock()
		return missing, nil
	})
	if result.(bool) {
		err := fmt.Errorf("%w: %s", errBridgeArtifactUnavailable, ref)
		log.Printf("[bootstrap] %v", err)
		return err
	}
	return nil
}

func (p *bridgeArtifactPreflight) head(ctx context.Context, endpoint, token string, registry bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if registry {
		req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.docker.distribution.manifest.list.v2+json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return p.client.Do(req)
}

func (p *bridgeArtifactPreflight) missing(ctx context.Context, ref string) bool {
	endpoint := ref
	registry := !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://")
	if registry {
		host, name, ok := strings.Cut(ref, "/")
		if !ok || host == "" || name == "" {
			return false
		}
		tag := "latest"
		if repo, digest, ok := strings.Cut(name, "@"); ok {
			name, tag = repo, digest
		} else if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
			name, tag = name[:i], name[i+1:]
		}
		endpoint = "https://" + host + "/v2/" + name + "/manifests/" + tag
	}
	resp, err := p.head(ctx, endpoint, "", registry)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if registry && resp.StatusCode == http.StatusUnauthorized {
		token := p.anonymousToken(ctx, resp.Header.Get("WWW-Authenticate"))
		if token == "" {
			return false
		}
		resp, err = p.head(ctx, endpoint, token, true)
		if err != nil {
			return false
		}
		resp.Body.Close()
	}
	// Authentication failures, rate limits and transport errors are inconclusive.
	return resp.StatusCode == http.StatusNotFound
}

var bridgeAuthParamRE = regexp.MustCompile(`([a-zA-Z_]+)="([^"]*)"`)

func (p *bridgeArtifactPreflight) anonymousToken(ctx context.Context, challenge string) string {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	values := make(map[string]string)
	for _, match := range bridgeAuthParamRE.FindAllStringSubmatch(params, -1) {
		values[strings.ToLower(match[1])] = match[2]
	}
	realm, err := url.Parse(values["realm"])
	if err != nil || realm.Host == "" || (realm.Scheme != "http" && realm.Scheme != "https") {
		return ""
	}
	query := realm.Query()
	for _, key := range []string{"service", "scope"} {
		if values[key] != "" {
			query.Set(key, values[key])
		}
	}
	realm.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return ""
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var result struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result) != nil {
		return ""
	}
	if result.Token != "" {
		return result.Token
	}
	return result.AccessToken
}
