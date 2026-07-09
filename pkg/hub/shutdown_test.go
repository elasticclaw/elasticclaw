package hub

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
)

// newTestWSPair returns both ends of a live WebSocket connection: the
// server-accepted conn (what the hub stores in s.claws) and the dialing
// client conn (what a claw bridge would hold).
func newTestWSPair(t *testing.T) (serverConn, clientConn *websocket.Conn) {
	t.Helper()
	connCh := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		connCh <- c
		<-done // keep the handler (and thus the conn) alive until test cleanup
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })
	server := <-connCh
	t.Cleanup(func() { _ = server.CloseNow() })
	return server, client
}

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

// TestShutdownBoundedByDrainWindow verifies that a background goroutine that
// never observes cancellation cannot hold Shutdown past the drain context:
// Shutdown must return once the drain window expires (Phase 0.1 acceptance:
// SIGTERM exits within the drain window).
func TestShutdownBoundedByDrainWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	s, err := NewServer("127.0.0.1:0", filepath.Join(dir, "hub.db"), dir, &types.HubConfig{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// A stuck goroutine that ignores the root context.
	block := make(chan struct{})
	s.bg.Go(func() error { <-block; return nil })
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Shutdown(ctx) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown blocked past the drain window on a stuck goroutine")
	}
}

// TestShutdownClosesClawsBeforeHTTPDrainCompletes verifies the Phase 0.1
// shutdown order: claw WebSocket close frames must be sent as soon as the
// listener stops accepting, not after in-flight HTTP requests finish draining.
func TestShutdownClosesClawsBeforeHTTPDrainCompletes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	s, err := NewServer("127.0.0.1:0", filepath.Join(dir, "hub.db"), dir, &types.HubConfig{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	serverConn, clientConn := newTestWSPair(t)
	s.mu.Lock()
	s.claws["claw-1"] = &clawConn{id: "claw-1", conn: serverConn}
	s.mu.Unlock()

	// HTTP server with one in-flight request that hangs until released.
	handlerStarted := make(chan struct{})
	release := make(chan struct{})
	s.httpSrv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-release
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = s.httpSrv.Serve(ln) }()
	go func() { _, _ = http.Get("http://" + ln.Addr().String() + "/hang") }()
	<-handlerStarted

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		shutdownDone <- s.Shutdown(ctx)
	}()

	// The claw must see the close frame while the HTTP request is still draining.
	readErr := make(chan error, 1)
	go func() {
		_, _, err := clientConn.Read(context.Background())
		readErr <- err
	}()
	select {
	case err := <-readErr:
		if websocket.CloseStatus(err) != websocket.StatusGoingAway {
			t.Fatalf("claw close status = %v, want StatusGoingAway", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("claw connection was not closed while HTTP drain was still in flight")
	}

	close(release)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
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
