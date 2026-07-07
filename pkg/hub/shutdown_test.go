package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// TestGracefulShutdown boots a full server (background goroutines included)
// and verifies Shutdown cancels the root context, waits for all background
// goroutines, and closes the database — all within the drain window.
func TestGracefulShutdown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	s, err := NewServer("127.0.0.1:0", filepath.Join(dir, "hub.db"), dir, &types.HubConfig{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		done <- s.Shutdown(ctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Shutdown did not complete within 20s")
	}

	// The root context must be cancelled so background goroutines exited.
	select {
	case <-s.bgCtx.Done():
	default:
		t.Fatal("background context not cancelled after Shutdown")
	}

	// All background goroutines must have returned (Wait does not block).
	waitDone := make(chan struct{})
	go func() {
		_ = s.bg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("background goroutines still running after Shutdown")
	}

	// The database must be closed.
	if err := s.db.Ping(); err == nil {
		t.Fatal("database still open after Shutdown")
	}

	// A second Shutdown is a no-op and must not panic or error.
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// TestHealthzReturns503WhileDraining verifies /healthz flips from 200 to 503
// once shutdown starts, so load balancers stop routing traffic during drain.
func TestHealthzReturns503WhileDraining(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz before drain: got %d, want %d", resp.StatusCode, http.StatusOK)
	}

	s.draining.Store(true)

	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz during drain: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz during drain: got %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestShutdownGraceConfig(t *testing.T) {
	cases := []struct {
		cfg  string
		want time.Duration
	}{
		{"", 15 * time.Second},
		{"30s", 30 * time.Second},
		{"1m", time.Minute},
		{"bogus", 15 * time.Second},
		{"-5s", 15 * time.Second},
	}
	for _, tc := range cases {
		s := &Server{hubCfg: &types.HubConfig{ShutdownGrace: tc.cfg}}
		if got := s.shutdownGrace(); got != tc.want {
			t.Errorf("shutdownGrace(%q) = %s, want %s", tc.cfg, got, tc.want)
		}
	}
}
