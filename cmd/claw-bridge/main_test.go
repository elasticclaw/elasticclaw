package main

import (
	"context"
	"encoding/json"
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

func TestWaitForWorkspaceReadyIfRequested(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ELASTICCLAW_WAIT_FOR_WORKSPACE", "1")
	workspaceDir := filepath.Join(home, "workspace")
	if err := os.MkdirAll(workspaceDir, 0700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, ".elasticclaw-workspace-ready"), []byte("ready\n"), 0600); err != nil {
		t.Fatalf("write ready marker: %v", err)
	}

	if err := waitForWorkspaceReadyIfRequested(); err != nil {
		t.Fatalf("waitForWorkspaceReadyIfRequested(): %v", err)
	}
}

func TestNestedStringExtractsToolCommandDetails(t *testing.T) {
	tests := []struct {
		name string
		data map[string]interface{}
		keys []string
		want string
	}{
		{
			name: "direct command",
			data: map[string]interface{}{"command": "npm run build"},
			keys: []string{"command", "cmd", "input"},
			want: "npm run build",
		},
		{
			name: "nested item arguments",
			data: map[string]interface{}{
				"item": map[string]interface{}{
					"arguments": map[string]interface{}{"cmd": "go test ./pkg/hub"},
				},
			},
			keys: []string{"command", "cmd", "input"},
			want: "go test ./pkg/hub",
		},
		{
			name: "json encoded args",
			data: map[string]interface{}{
				"input": `{"command":"npm run lint"}`,
			},
			keys: []string{"command", "cmd", "input"},
			want: "npm run lint",
		},
		{
			name: "argv array",
			data: map[string]interface{}{
				"arguments": map[string]interface{}{
					"argv": []interface{}{"npm", "run", "test"},
				},
			},
			keys: []string{"command", "cmd", "argv", "input"},
			want: "npm run test",
		},
		{
			name: "plain input is not path",
			data: map[string]interface{}{
				"input": "npm run build",
			},
			keys: []string{"path", "file", "filename"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nestedString(tt.data, tt.keys...); got != tt.want {
				t.Fatalf("nestedString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveToolActivityDetailUsesOpenClawMeta(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		meta        string
		wantCommand string
		wantPath    string
		wantURL     string
		wantDetail  string
	}{
		{
			name:        "exec meta is command",
			tool:        "exec",
			meta:        "npm run build",
			wantCommand: "npm run build",
		},
		{
			name:     "read meta is path",
			tool:     "read",
			meta:     "/workspace/web/package.json",
			wantPath: "/workspace/web/package.json",
		},
		{
			name:    "web fetch url meta is url",
			tool:    "web_fetch",
			meta:    "https://example.com/docs",
			wantURL: "https://example.com/docs",
		},
		{
			name:       "unknown tool meta is detail",
			tool:       "memory_search",
			meta:       "query eslint",
			wantDetail: "query eslint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, path, url, detail := resolveToolActivityDetail(tt.tool, "", "", "", tt.meta, nil)
			if command != tt.wantCommand || path != tt.wantPath || url != tt.wantURL || detail != tt.wantDetail {
				t.Fatalf("resolveToolActivityDetail() = (%q, %q, %q, %q), want (%q, %q, %q, %q)", command, path, url, detail, tt.wantCommand, tt.wantPath, tt.wantURL, tt.wantDetail)
			}
		})
	}
}

func TestPatchOllamaLocalDevCatalogSetsRuntimeLimits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCLAW_DEFAULT_MODEL", "ollama/qwen2.5-coder:1.5b")

	catalogPath := filepath.Join(home, ".openclaw", "agents", "main", "agent", "plugins", "ollama", "catalog.json")
	if err := os.MkdirAll(filepath.Dir(catalogPath), 0700); err != nil {
		t.Fatalf("mkdir catalog dir: %v", err)
	}
	initial := map[string]interface{}{
		"providers": map[string]interface{}{
			"ollama": map[string]interface{}{
				"baseUrl": "http://127.0.0.1:11434",
				"models": []interface{}{
					map[string]interface{}{
						"id":            "qwen2.5-coder:1.5b",
						"contextWindow": float64(128000),
						"maxTokens":     float64(8192),
						"compat": map[string]interface{}{
							"supportsTools": true,
						},
					},
					map[string]interface{}{
						"id":            "llama3.2:3b",
						"baseUrl":       "http://127.0.0.1:11434",
						"contextWindow": float64(4096),
						"maxTokens":     float64(2048),
					},
				},
			},
			"ollama-cloud": map[string]interface{}{
				"baseUrl": "https://ollama.com",
			},
		},
	}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(catalogPath, data, 0600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}

	if err := patchOllamaLocalDevCatalog("http://ollama:11434"); err != nil {
		t.Fatalf("patch catalog: %v", err)
	}

	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var got struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			Models  []struct {
				ID            string `json:"id"`
				BaseURL       string `json:"baseUrl"`
				ContextWindow int    `json:"contextWindow"`
				MaxTokens     int    `json:"maxTokens"`
				Params        struct {
					NumCtx    int    `json:"num_ctx"`
					Thinking  bool   `json:"thinking"`
					KeepAlive string `json:"keep_alive"`
				} `json:"params"`
				Compat struct {
					SupportsTools            bool `json:"supportsTools"`
					SupportsUsageInStreaming bool `json:"supportsUsageInStreaming"`
				} `json:"compat"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	local := got.Providers["ollama"]
	if local.BaseURL != "http://ollama:11434" {
		t.Fatalf("local baseUrl = %q", local.BaseURL)
	}
	if got.Providers["ollama-cloud"].BaseURL != "https://ollama.com" {
		t.Fatalf("ollama-cloud was modified: %q", got.Providers["ollama-cloud"].BaseURL)
	}
	if len(local.Models) != 2 {
		t.Fatalf("models len = %d", len(local.Models))
	}
	model := local.Models[0]
	if model.BaseURL != "http://ollama:11434" || model.ContextWindow != 32768 || model.MaxTokens != 1024 {
		t.Fatalf("model limits not patched: %+v", model)
	}
	if model.Params.NumCtx != 32768 || model.Params.Thinking || model.Params.KeepAlive != "15m" {
		t.Fatalf("model params not patched: %+v", model.Params)
	}
	if !model.Compat.SupportsTools || !model.Compat.SupportsUsageInStreaming {
		t.Fatalf("model compat not patched: %+v", model.Compat)
	}
	otherModel := local.Models[1]
	if otherModel.BaseURL != "http://127.0.0.1:11434" || otherModel.ContextWindow != 4096 || otherModel.MaxTokens != 2048 {
		t.Fatalf("non-selected model was modified: %+v", otherModel)
	}
}

func TestSanitizeActivityTextRedactsRepeatedSecretPrefixes(t *testing.T) {
	input := `curl "https://api.example.com?access_token=abc&access_token=xyz" -H "Authorization: Bearer tok1" -H "X-Alt: Bearer tok2"`
	got := sanitizeActivityText(input)
	if strings.Contains(got, "abc") || strings.Contains(got, "xyz") || strings.Contains(got, "tok1") || strings.Contains(got, "tok2") {
		t.Fatalf("sanitizeActivityText leaked secret: %q", got)
	}
	if strings.Count(got, "access_token=[redacted]") != 2 {
		t.Fatalf("sanitizeActivityText redacted access_token count = %d, want 2 in %q", strings.Count(got, "access_token=[redacted]"), got)
	}
	if strings.Count(got, "Bearer [redacted]") != 2 {
		t.Fatalf("sanitizeActivityText redacted bearer count = %d, want 2 in %q", strings.Count(got, "Bearer [redacted]"), got)
	}
}

func TestPromoteInsufficientGatewayPairingPromotesReadOnlyDevice(t *testing.T) {
	home := t.TempDir()
	devicesDir := filepath.Join(home, ".openclaw", "devices")
	if err := os.MkdirAll(devicesDir, 0700); err != nil {
		t.Fatalf("mkdir devices dir: %v", err)
	}
	path := filepath.Join(devicesDir, "paired.json")
	initial := map[string]interface{}{
		"device-1": map[string]interface{}{
			"deviceId":       "device-1",
			"scopes":         []string{"operator.read"},
			"approvedScopes": []string{"operator.read"},
		},
		"device-2": map[string]interface{}{
			"deviceId":       "device-2",
			"scopes":         defaultScopes,
			"approvedScopes": defaultScopes,
		},
	}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write paired devices: %v", err)
	}

	promoted, err := promoteInsufficientGatewayPairing(home, "device-1", defaultScopes)
	if err != nil {
		t.Fatalf("promote pairing: %v", err)
	}
	if !promoted {
		t.Fatal("promoted = false, want true")
	}

	var after map[string]struct {
		ApprovedScopes []string `json:"approvedScopes"`
		Scopes         []string `json:"scopes"`
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read paired devices: %v", err)
	}
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatalf("parse paired devices: %v", err)
	}
	if !hasAllScopes(after["device-1"].ApprovedScopes, defaultScopes) {
		t.Fatalf("read-only paired device was not promoted: %#v", after["device-1"])
	}
	if _, ok := after["device-2"]; !ok {
		t.Fatalf("unrelated device was removed: %#v", after)
	}
}

func TestPromoteInsufficientGatewayPairingKeepsFullyScopedDevice(t *testing.T) {
	home := t.TempDir()
	devicesDir := filepath.Join(home, ".openclaw", "devices")
	if err := os.MkdirAll(devicesDir, 0700); err != nil {
		t.Fatalf("mkdir devices dir: %v", err)
	}
	path := filepath.Join(devicesDir, "paired.json")
	initial := map[string]interface{}{
		"device-1": map[string]interface{}{
			"deviceId":       "device-1",
			"scopes":         defaultScopes,
			"approvedScopes": defaultScopes,
		},
	}
	data, _ := json.Marshal(initial)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write paired devices: %v", err)
	}

	promoted, err := promoteInsufficientGatewayPairing(home, "device-1", defaultScopes)
	if err != nil {
		t.Fatalf("promote pairing: %v", err)
	}
	if promoted {
		t.Fatal("promoted = true, want false")
	}
}

func TestWriteFileAtomicReplacesContentAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paired.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0600); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	if err := writeFileAtomic(path, []byte(`{"new":true}`+"\n"), 0600); err != nil {
		t.Fatalf("write atomic: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "{\"new\":true}\n" {
		t.Fatalf("file content = %q, want new content", string(data))
	}
	matches, err := filepath.Glob(filepath.Join(dir, "paired.json.tmp-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temp files were not cleaned up: %v", matches)
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
