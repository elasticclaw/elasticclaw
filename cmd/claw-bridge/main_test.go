package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestSyncStagedWorkspaceToOpenClawWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	stagedDir := filepath.Join(home, "workspace")
	activeDir := filepath.Join(home, ".openclaw", "workspace")
	if err := os.MkdirAll(filepath.Join(stagedDir, "scripts"), 0700); err != nil {
		t.Fatalf("mkdir staged workspace: %v", err)
	}
	if err := os.MkdirAll(activeDir, 0700); err != nil {
		t.Fatalf("mkdir active workspace: %v", err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(filepath.Join(stagedDir, "elasticclaw-config.yaml"), "schema_version: v1\nname: lexipol\nprovider: docker\n")
	write(filepath.Join(stagedDir, "AGENTS.md"), "You are the Lexipol factory agent.\n")
	write(filepath.Join(stagedDir, "scripts", "bootstrap.sh"), "#!/bin/sh\n")
	write(filepath.Join(stagedDir, ".elasticclaw-workspace-ready"), "ready\n")
	write(filepath.Join(activeDir, "BOOTSTRAP.md"), "Who am I? Who are you?\n")
	write(filepath.Join(activeDir, "MEMORY.md"), "blank slate\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(stagedDir, "passwd-link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if err := syncStagedWorkspaceToOpenClawWorkspace(); err != nil {
		t.Fatalf("syncStagedWorkspaceToOpenClawWorkspace(): %v", err)
	}

	assertFile := func(rel, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(activeDir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", rel, string(got), want)
		}
	}
	assertFile("elasticclaw-config.yaml", "schema_version: v1\nname: lexipol\nprovider: docker\n")
	assertFile("AGENTS.md", "You are the Lexipol factory agent.\n")
	assertFile(filepath.Join("scripts", "bootstrap.sh"), "#!/bin/sh\n")
	if _, err := os.Stat(filepath.Join(activeDir, "BOOTSTRAP.md")); !os.IsNotExist(err) {
		t.Fatalf("BOOTSTRAP.md should be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(activeDir, "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatalf("MEMORY.md should be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(activeDir, ".elasticclaw-workspace-ready")); !os.IsNotExist(err) {
		t.Fatalf("ready marker should not be copied, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(activeDir, "passwd-link")); !os.IsNotExist(err) {
		t.Fatalf("symlink should not be copied, got err=%v", err)
	}
}

func TestConfiguredGitHubReposParsesDockerBootstrapEnv(t *testing.T) {
	t.Setenv("ELASTICCLAW_GITHUB_REPOS", `[{"repo":"praetoriandigital/accreditation-workbench-lambdas","permissions":"write"},{"repo":"  ","permissions":"read"}]`)

	repos, err := configuredGitHubRepos()
	if err != nil {
		t.Fatalf("configuredGitHubRepos(): %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("repo count = %d, want 1", len(repos))
	}
	if repos[0].Repo != "praetoriandigital/accreditation-workbench-lambdas" {
		t.Fatalf("repo = %q", repos[0].Repo)
	}
	if got := repoDirectoryName(repos[0].Repo); got != "accreditation-workbench-lambdas" {
		t.Fatalf("repoDirectoryName() = %q", got)
	}
}

func TestDockerGitHubCredentialHelperScriptDoesNotPersistClawToken(t *testing.T) {
	script := dockerGitHubCredentialHelperScript("https://factory.example/api/github/token/claw-1")

	if strings.Contains(script, "secret-claw-token") {
		t.Fatalf("helper script persisted claw token:\n%s", script)
	}
	if strings.Contains(script, "python3") {
		t.Fatalf("helper script should not depend on python3:\n%s", script)
	}
	if strings.Contains(script, "sudo ") {
		t.Fatalf("helper script should not require sudo:\n%s", script)
	}
	if strings.Contains(script, "/usr/local/bin/elasticclaw-git-credentials") {
		t.Fatalf("helper script should install under the user's home directory:\n%s", script)
	}
	if !strings.Contains(script, "umask 0077") {
		t.Fatalf("helper script should create credential helper with restrictive permissions:\n%s", script)
	}
	if !strings.Contains(script, `ELASTICCLAW_CLAW_TOKEN`) {
		t.Fatalf("helper script should read claw token from runtime environment:\n%s", script)
	}
	if !strings.Contains(script, `${HOME}/.elasticclaw/bin`) {
		t.Fatalf("helper script should install under HOME:\n%s", script)
	}
	if !strings.Contains(script, `--data-urlencode "claw_token=$claw_token"`) {
		t.Fatalf("helper script should URL-encode claw token at runtime:\n%s", script)
	}
}

func TestDockerGitHubCloneScriptResetsDivergedRepos(t *testing.T) {
	script := dockerGitHubCloneScript("/home/node/.openclaw/workspace", []bootstrapRepoAccess{{
		Repo:        "elasticclaw/elasticclaw",
		Permissions: "write",
	}})

	if strings.Contains(script, "pull --ff-only") {
		t.Fatalf("clone script should not use pull --ff-only:\n%s", script)
	}
	if !strings.Contains(script, "fetch origin") || !strings.Contains(script, `reset --hard "origin/$branch"`) {
		t.Fatalf("clone script should fetch and hard reset existing repos:\n%s", script)
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
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse raw catalog: %v", err)
	}
	rawProviders, _ := raw["providers"].(map[string]interface{})
	rawOllama, _ := rawProviders["ollama"].(map[string]interface{})
	if _, ok := rawOllama["timeoutSeconds"]; ok {
		t.Fatal("local ollama provider should not override timeoutSeconds")
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

func TestCLICodingProviderForModel(t *testing.T) {
	tests := map[string]string{
		"codex/gpt-5.5":        "codex",
		"grok/grok-build-0.1":  "grok",
		"anthropic/claude-foo": "",
		"":                     "",
	}
	for model, want := range tests {
		if got := cliCodingProviderForModel(model); got != want {
			t.Fatalf("cliCodingProviderForModel(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestRestoreCLIModelAuthWritesBundleFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bundle := cliAuthBundle{
		Files: map[string]string{
			".codex/auth.json": base64.StdEncoding.EncodeToString([]byte(`{"token":"test"}`)),
		},
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ELASTICCLAW_MODEL_AUTH_PROVIDER", "codex")
	t.Setenv("ELASTICCLAW_MODEL_AUTH_STATE", base64.StdEncoding.EncodeToString(data))

	if err := restoreCLIModelAuth(); err != nil {
		t.Fatalf("restore auth: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatalf("read restored auth: %v", err)
	}
	if string(got) != `{"token":"test"}` {
		t.Fatalf("restored auth = %q", string(got))
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

func TestIsRecoverableSessionSendError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "caller deadline", err: context.DeadlineExceeded, want: false},
		{name: "lifecycle context overflow not retryable", err: errString("context overflow detected"), want: false},
		{name: "lifecycle prompt too large not retryable", err: errString("Context overflow: prompt too large for the model"), want: false},
		{name: "send request context overflow", err: &sessionSendRequestError{err: errString("context overflow detected")}, want: true},
		{name: "send request prompt too large", err: &sessionSendRequestError{err: errString("Context overflow: prompt too large for the model")}, want: true},
		{name: "send failure", err: errString("sessions.send failed: tool crashed"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRecoverableSessionSendError(tt.err); got != tt.want {
				t.Fatalf("isRecoverableSessionSendError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsRetryableLLMSendError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "api upstream", err: errString("api_error: upstream error"), want: true},
		{name: "overloaded", err: errString("provider overloaded, try again"), want: true},
		{name: "temporary unavailable", err: errString("model temporarily unavailable"), want: true},
		{name: "rate limit", err: errString("rate limit exceeded"), want: true},
		{name: "request timeout", err: errString("request timeout waiting for model"), want: true},
		{name: "model timeout", err: errString("model timeout waiting for completion"), want: true},
		{name: "transport timeout", err: errString("sessions.send: i/o timeout"), want: false},
		{name: "permanent tool error", err: errString("tool-policy: exec is not allowed"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableLLMSendError(tt.err); got != tt.want {
				t.Fatalf("isRetryableLLMSendError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsRetryableLLMSendRequestError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "lifecycle upstream", err: errString("api_error: upstream error"), want: false},
		{name: "lifecycle model timeout", err: errString("model timeout waiting for completion"), want: false},
		{name: "send request upstream", err: &sessionSendRequestError{err: errString("api_error: upstream error")}, want: true},
		{name: "send request model timeout", err: &sessionSendRequestError{err: errString("model timeout waiting for completion")}, want: true},
		{name: "send request transport timeout", err: &sessionSendRequestError{err: errString("i/o timeout")}, want: false},
		{name: "send request permanent", err: &sessionSendRequestError{err: errString("tool-policy: exec is not allowed")}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableLLMSendRequestError(tt.err); got != tt.want {
				t.Fatalf("isRetryableLLMSendRequestError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsRetryableGatewaySendRequestError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context deadline", err: context.DeadlineExceeded, want: false},
		{name: "plain non-wrapped closed connection", err: errString("sessions.send write: use of closed network connection"), want: false},
		{name: "wrapped closed network connection", err: &sessionSendRequestError{err: errString("sessions.send write: use of closed network connection")}, want: true},
		{name: "wrapped marshaler closed connection string", err: &sessionSendRequestError{err: errString("sessions.send write: failed to marshal JSON: use of closed network connection")}, want: true},
		{name: "wrapped connection reset", err: &sessionSendRequestError{err: errString("sessions.send write: write tcp 127.0.0.1:123->127.0.0.1:456: connection reset by peer")}, want: true},
		{name: "wrapped broken pipe", err: &sessionSendRequestError{err: errString("sessions.send write: write tcp 127.0.0.1:123->127.0.0.1:456: broken pipe")}, want: true},
		{name: "wrapped websocket close unexpected eof", err: &sessionSendRequestError{err: errString("sessions.send write: failed to write msg: WebSocket closed: unexpected EOF")}, want: true},
		{name: "wrapped gateway disconnected after accept", err: &sessionSendRequestError{err: errString("sessions.send failed: gateway disconnected")}, want: false},
		{name: "wrapped io timeout", err: &sessionSendRequestError{err: errString("sessions.send write: i/o timeout")}, want: false},
		{name: "lifecycle closed connection not wrapped", err: errString("lifecycle: use of closed network connection"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableGatewaySendRequestError(tt.err); got != tt.want {
				t.Fatalf("isRetryableGatewaySendRequestError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsMissingGatewaySessionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "session not found", err: errString("sessions.subscribe: sessions.subscribe failed: session not found"), want: true},
		{name: "session key not found", err: errString("sessions.subscribe failed: session key not found"), want: true},
		{name: "unknown session", err: errString("sessions.subscribe failed: unknown session key"), want: true},
		{name: "plain not found", err: errString("sessions.subscribe: not found"), want: false},
		{name: "user not found", err: errString("sessions.subscribe failed: user not found"), want: false},
		{name: "route not found", err: errString("sessions.subscribe failed: route not found"), want: false},
		{name: "permission denied", err: errString("sessions.subscribe failed: permission denied"), want: false},
		{name: "transport timeout", err: errString("sessions.subscribe write: i/o timeout"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMissingGatewaySessionError(tt.err); got != tt.want {
				t.Fatalf("isMissingGatewaySessionError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestReconnectGatewayReportsStaleConnectionNoop(t *testing.T) {
	current := &websocket.Conn{}
	stale := &websocket.Conn{}
	gs := &gatewaySession{conn: current}

	reconnected, err := gs.reconnectGateway(context.Background(), stale)
	if err != nil {
		t.Fatalf("reconnectGateway returned error: %v", err)
	}
	if reconnected {
		t.Fatal("reconnectGateway reported reconnect for stale expected connection")
	}
	if got := gs.currentConn(); got != current {
		t.Fatalf("current connection changed on stale reconnect: got %p, want %p", got, current)
	}
}

func TestGatewaySessionRetriesSendAfterClosedGatewayWrite(t *testing.T) {
	var connections atomic.Int32
	var acceptedSends atomic.Int32
	firstHandshakeDone := make(chan struct{})
	firstGatewayClosed := make(chan struct{})
	testDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()

		connID := connections.Add(1)
		ctx := r.Context()
		challengePayload, _ := json.Marshal(map[string]string{"nonce": "nonce"})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "event", Event: "connect.challenge", Payload: challengePayload}); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		var connectReq gwFrame
		if err := wsjson.Read(ctx, conn, &connectReq); err != nil {
			t.Errorf("read connect request: %v", err)
			return
		}
		if connectReq.Method != "connect" {
			t.Errorf("first request method = %q, want connect", connectReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: connectReq.ID, OK: true}); err != nil {
			t.Errorf("write connect response: %v", err)
			return
		}

		if connID == 1 {
			close(firstHandshakeDone)
			conn.CloseNow()
			close(firstGatewayClosed)
			return
		}

		var subReq gwFrame
		if err := wsjson.Read(ctx, conn, &subReq); err != nil {
			t.Errorf("read subscribe request: %v", err)
			return
		}
		if subReq.Method != "sessions.subscribe" {
			t.Errorf("reconnect request method = %q, want sessions.subscribe", subReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: subReq.ID, OK: true}); err != nil {
			t.Errorf("write subscribe response: %v", err)
			return
		}

		var sendReq gwFrame
		if err := wsjson.Read(ctx, conn, &sendReq); err != nil {
			t.Errorf("read sessions.send request: %v", err)
			return
		}
		if sendReq.Method != "sessions.send" {
			t.Errorf("retry request method = %q, want sessions.send", sendReq.Method)
			return
		}
		acceptedSends.Add(1)
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: sendReq.ID, OK: true}); err != nil {
			t.Errorf("write sessions.send response: %v", err)
			return
		}

		assistantPayload, _ := json.Marshal(map[string]interface{}{
			"stream":     "assistant",
			"sessionKey": "session-1",
			"data":       map[string]string{"delta": "hello"},
		})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "event", Event: "agent", Payload: assistantPayload}); err != nil {
			t.Errorf("write assistant event: %v", err)
			return
		}
		endPayload, _ := json.Marshal(map[string]interface{}{
			"stream":     "lifecycle",
			"sessionKey": "session-1",
			"data":       map[string]string{"phase": "end"},
		})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "event", Event: "agent", Payload: endPayload}); err != nil {
			t.Errorf("write lifecycle event: %v", err)
			return
		}
		<-testDone
	}))
	defer srv.Close()
	defer close(testDone)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client := &gatewayClient{
		addr:  strings.TrimPrefix(wsURL, "ws://"),
		token: "token",
		device: &deviceIdentity{
			DeviceID:      "device-1",
			PublicKeyPem:  testPEM("PUBLIC KEY"),
			PrivateKeyPem: testPEM("PRIVATE KEY"),
		},
		home: t.TempDir(),
	}
	conn, err := client.connectToGateway(ctx)
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	select {
	case <-firstHandshakeDone:
	case <-time.After(time.Second):
		t.Fatal("first gateway handshake did not complete")
	}
	select {
	case <-firstGatewayClosed:
	case <-time.After(time.Second):
		t.Fatal("first gateway did not close")
	}
	conn.CloseNow()

	gs := &gatewaySession{
		client:     client,
		sessionKey: "session-1",
		conn:       conn,
		pending:    make(map[string]chan gwFrame),
	}
	go gs.readLoop(ctx)

	var chunks []string
	start := time.Now()
	reply, err := gs.SendMessage(ctx, "hi", func(chunk string) {
		chunks = append(chunks, chunk)
	}, nil)
	if err != nil {
		t.Fatalf("SendMessage returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("SendMessage took %s, want retry to complete without waiting for readLoop reconnect backoff", elapsed)
	}
	if reply != "hello" {
		t.Fatalf("reply = %q, want hello", reply)
	}
	if strings.Join(chunks, "") != "hello" {
		t.Fatalf("chunks = %q, want hello", strings.Join(chunks, ""))
	}
	if got := acceptedSends.Load(); got != 1 {
		t.Fatalf("accepted sends = %d, want 1", got)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want 2", got)
	}
}

func testPEM(typ string) string {
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  typ,
		Bytes: []byte("01234567890123456789012345678901"),
	}))
}

type errString string

func (e errString) Error() string { return string(e) }
