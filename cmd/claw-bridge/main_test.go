package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func writeGatewayClientConfig(t *testing.T, home, config string) {
	t.Helper()
	cfgDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(filepath.Join(cfgDir, "identity"), 0700); err != nil {
		t.Fatalf("create openclaw dirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "openclaw.json"), []byte(config), 0600); err != nil {
		t.Fatalf("write openclaw config: %v", err)
	}
	device := `{"deviceId":"device-1","publicKeyPem":"pub","privateKeyPem":"priv"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "identity", "device.json"), []byte(device), 0600); err != nil {
		t.Fatalf("write device config: %v", err)
	}
}

func TestLoadGatewayClientUsesRemotePasswordFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ELASTICCLAW_GATEWAY_PASSWORD", "")
	t.Setenv("OPENCLAW_GATEWAY_PASSWORD", "")
	writeGatewayClientConfig(t, home, `{
  "gateway": {
    "auth": {"mode": "password"},
    "remote": {"password": "remote-password"}
  }
}`)

	client, err := loadGatewayClient("localhost:18789")
	if err != nil {
		t.Fatalf("load gateway client: %v", err)
	}
	if client.password != "remote-password" {
		t.Fatalf("password = %q, want remote-password", client.password)
	}
	if client.token != "" {
		t.Fatalf("token = %q, want empty when password is configured", client.token)
	}
}

func TestLoadGatewayClientConfigTakesPriorityOverEnvVar(t *testing.T) {
	// Config password must win over env var — the gateway generates its own
	// auth.password and writes it to the config; env var override would send
	// the bootstrap password instead, causing a mismatch.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ELASTICCLAW_GATEWAY_PASSWORD", "")
	t.Setenv("OPENCLAW_GATEWAY_PASSWORD", "env-password")
	writeGatewayClientConfig(t, home, `{
  "gateway": {
    "auth": {"mode": "password", "password": "auth-password"},
    "remote": {"password": "remote-password"}
  }
}`)

	client, err := loadGatewayClient("localhost:18789")
	if err != nil {
		t.Fatalf("load gateway client: %v", err)
	}
	if client.password != "auth-password" {
		t.Fatalf("password = %q, want auth-password (config must take priority over env var)", client.password)
	}
}

func TestLoadGatewayClientEnvVarFallbackWhenNoConfigPassword(t *testing.T) {
	// Env var is used only when config has no password (legacy/initial-setup case).
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ELASTICCLAW_GATEWAY_PASSWORD", "")
	t.Setenv("OPENCLAW_GATEWAY_PASSWORD", "env-password")
	writeGatewayClientConfig(t, home, `{
  "gateway": {
    "auth": {"mode": "password"},
    "remote": {}
  }
}`)

	client, err := loadGatewayClient("localhost:18789")
	if err != nil {
		t.Fatalf("load gateway client: %v", err)
	}
	if client.password != "env-password" {
		t.Fatalf("password = %q, want env-password (env var fallback when config has no password)", client.password)
	}
}

func TestGatewayReadLoopFailsPendingRequestsOnDisconnect(t *testing.T) {
	serverRead := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()

		var req gwFrame
		if err := wsjson.Read(r.Context(), conn, &req); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		close(serverRead)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.CloseNow()

	gs := &gatewaySession{
		conn:    conn,
		pending: make(map[string]chan gwFrame),
	}
	go gs.readLoop(ctx)

	errCh := make(chan error, 1)
	go func() {
		_, err := gs.sendReq(ctx, "sessions.get", map[string]string{"key": "session-1"})
		errCh <- err
	}()

	select {
	case <-serverRead:
	case <-time.After(time.Second):
		t.Fatal("server did not receive request")
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("sendReq returned nil error")
		}
		if !strings.Contains(err.Error(), "gateway disconnected") {
			t.Fatalf("sendReq error = %v, want gateway disconnected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sendReq did not return after gateway disconnect")
	}

	gs.pendMu.Lock()
	pendingLen := len(gs.pending)
	gs.pendMu.Unlock()
	if pendingLen != 0 {
		t.Fatalf("pending requests = %d, want 0", pendingLen)
	}
}
