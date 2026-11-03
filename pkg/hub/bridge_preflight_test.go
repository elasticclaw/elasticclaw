package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBridgeArtifactPreflightHTTP(t *testing.T) {
	for _, status := range []int{200, 404, 401, 403, 429, 500, 405} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Errorf("method = %s, want HEAD", r.Method)
				}
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, "/artifact", http.StatusFound)
					return
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			p := bridgeArtifactPreflight{client: server.Client()}
			ref := server.URL + "/redirect"
			err := p.check(context.Background(), ref)
			if status != 404 {
				if err != nil {
					t.Fatalf("inconclusive or available artifact blocked bootstrap: %v", err)
				}
				return
			}
			if err == nil || err.Error() != "bridge artifact unavailable: "+ref {
				t.Fatalf("missing artifact error = %v", err)
			}
			msg := classifyAgentFailure("Bootstrap failed: connector download: " + err.Error())
			if msg.Kind != agentFailureBridgeArtifact || retryableAgentFailure(msg.Kind) {
				t.Fatalf("missing artifact classification = %s", msg.Kind)
			}
		})
	}
}

func TestBridgeArtifactPreflightNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	p := bridgeArtifactPreflight{client: server.Client()}
	if err := p.check(context.Background(), server.URL); err != nil {
		t.Fatalf("network error blocked bootstrap: %v", err)
	}
}

func TestBridgeArtifactPreflightCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	p := bridgeArtifactPreflight{client: server.Client()}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.check(context.Background(), server.URL); err == nil {
				t.Error("missing artifact accepted")
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("HEAD requests = %d, want 1", got)
	}
	p.mu.Lock()
	p.cache[server.URL] = bridgePreflightResult{missing: true, expires: time.Now().Add(-time.Second)}
	p.mu.Unlock()
	_ = p.check(context.Background(), server.URL)
	_ = p.check(context.Background(), server.URL+"/other")
	if got := calls.Load(); got != 3 {
		t.Fatalf("HEAD requests after expiration and new ref = %d, want 3", got)
	}
}

func TestBridgeArtifactPreflightRegistry(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		for _, status := range []int{200, 404, 403, 500} {
			t.Run(fmt.Sprintf("auth=%t/status=%d", authenticated, status), func(t *testing.T) {
				var server *httptest.Server
				var heads, tokens atomic.Int32
				server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/token" {
						tokens.Add(1)
						if r.Method != http.MethodGet || r.URL.Query().Get("service") != "registry" || r.URL.Query().Get("scope") != "repository:owner/bridge:pull" {
							t.Errorf("unexpected token request: %s %s", r.Method, r.URL)
						}
						fmt.Fprint(w, `{"token":"anonymous-token"}`)
						return
					}
					heads.Add(1)
					if r.Method != http.MethodHead || r.URL.Path != "/v2/owner/bridge/manifests/1w" || r.Header.Get("Accept") == "" {
						t.Errorf("unexpected manifest request: %s %s", r.Method, r.URL)
					}
					if authenticated && r.Header.Get("Authorization") != "Bearer anonymous-token" {
						w.Header().Set("WWW-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry",scope="repository:owner/bridge:pull"`)
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					w.WriteHeader(status)
				}))
				defer server.Close()
				p := bridgeArtifactPreflight{client: server.Client()}
				ref := strings.TrimPrefix(server.URL, "https://") + "/owner/bridge:1w"
				err := p.check(context.Background(), ref)
				if (err != nil) != (status == 404) {
					t.Fatalf("status %d: error = %v", status, err)
				}
				wantHeads, wantTokens := int32(1), int32(0)
				if authenticated {
					wantHeads, wantTokens = 2, 1
				}
				if heads.Load() != wantHeads || tokens.Load() != wantTokens {
					t.Fatalf("requests: heads=%d tokens=%d", heads.Load(), tokens.Load())
				}
			})
		}
	}
}

func TestBridgeArtifactPreflightErrorIsIdentifiable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	p := bridgeArtifactPreflight{client: server.Client()}
	err := p.check(context.Background(), server.URL)
	if !errors.Is(err, errBridgeArtifactUnavailable) {
		t.Fatalf("expected errBridgeArtifactUnavailable, got %v", err)
	}
}
