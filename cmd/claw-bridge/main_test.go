package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestWriteManagedGrokCredentialToOpenClaw(t *testing.T) {
	home := t.TempDir()
	agentDir := filepath.Join(home, ".openclaw", "agents", "main", "agent")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(agentDir, "openclaw-agent.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	existing := `{"version":1,"profiles":{"anthropic:default":{"type":"api_key","provider":"anthropic","key":"keep-me"},"xai:default":{"type":"oauth","provider":"xai","access":"old","refresh":"real-refresh","expires":1,"email":"dev@example.com"}}}`
	if _, err := db.Exec(`CREATE TABLE auth_profile_store (store_key TEXT NOT NULL PRIMARY KEY, store_json TEXT NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO auth_profile_store(store_key,store_json,updated_at) VALUES('primary',?,1)`, existing); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	credential := managedModelAuthCredential{Provider: "xai", Access: "current-access", Expires: 1893553445000}
	if err := writeManagedGrokCredentialToOpenClaw(home, credential); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var storeJSON string
	if err := db.QueryRow(`SELECT store_json FROM auth_profile_store WHERE store_key='primary'`).Scan(&storeJSON); err != nil {
		t.Fatal(err)
	}
	var store struct {
		Profiles map[string]struct {
			Access  string `json:"access"`
			Refresh string `json:"refresh"`
			Expires int64  `json:"expires"`
			Email   string `json:"email"`
			Key     string `json:"key"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(storeJSON), &store); err != nil {
		t.Fatal(err)
	}
	xai := store.Profiles["xai:default"]
	if xai.Access != "current-access" || xai.Refresh != "elasticclaw-managed" || xai.Expires != credential.Expires {
		t.Fatalf("unexpected managed xAI profile: %#v", xai)
	}
	if xai.Email != "dev@example.com" {
		t.Fatalf("existing xAI metadata was not preserved: %#v", xai)
	}
	if got := store.Profiles["anthropic:default"].Key; got != "keep-me" {
		t.Fatalf("unrelated auth profile was not preserved: %q", got)
	}
}

func TestInjectGatewayNudgeSendsOnActiveSessionMidTurn(t *testing.T) {
	received := make(chan gwFrame, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		var req gwFrame
		if err := wsjson.Read(r.Context(), conn, &req); err != nil {
			t.Error(err)
			return
		}
		received <- req
		_ = wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: req.ID, OK: true})
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	gs := &gatewaySession{conn: conn, pending: make(map[string]chan gwFrame), sessionKey: "session-1", ready: true, inFlight: &inFlightState{done: make(chan agentResult, 1)}}
	go gs.readLoop(ctx)
	old := activeGatewaySession()
	setActiveGatewaySession(gs)
	t.Cleanup(func() { setActiveGatewaySession(old) })
	injectGatewayNudge(ctx, "nudge text")
	select {
	case req := <-received:
		if req.Method != "sessions.send" {
			t.Fatalf("method=%q", req.Method)
		}
		var params map[string]string
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params["key"] != "session-1" || params["message"] != "nudge text" {
			t.Fatalf("params=%v", params)
		}
	case <-ctx.Done():
		t.Fatal("did not receive nudge request")
	}
}

func TestInjectGatewayNudgeDroppedWhenNoTurnInFlight(t *testing.T) {
	old := activeGatewaySession()
	setActiveGatewaySession(nil)
	t.Cleanup(func() { setActiveGatewaySession(old) })
	injectGatewayNudge(context.Background(), "nudge text") // nil session must not panic
	gs := &gatewaySession{ready: true, pending: make(map[string]chan gwFrame)}
	setActiveGatewaySession(gs)
	injectGatewayNudge(context.Background(), "nudge text") // no in-flight turn must not send
}

func TestWriteManagedGrokCredentialToCLI(t *testing.T) {
	home := t.TempDir()
	authDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(authDir, "auth.json")
	const canonical = "https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828"
	document := map[string]any{
		canonical:   map[string]any{"key": "access", "refresh_token": "rotating-secret", "metadata": "keep"},
		"unrelated": map[string]any{"key": "other-access", "refresh_token": "other-refresh"},
	}
	initial, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, initial, 0600); err != nil {
		t.Fatal(err)
	}
	credential := managedModelAuthCredential{Provider: "xai", Access: "current-access", Expires: 1893553445000}
	if err := writeManagedGrokCredentialToCLI(home, credential); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "rotating-secret") || !strings.Contains(string(data), "elasticclaw-managed") || !strings.Contains(string(data), "current-access") || !strings.Contains(string(data), "2030-01-02T03:04:05Z") || !strings.Contains(string(data), `"metadata": "keep"`) {
		t.Fatalf("unexpected managed Grok auth: %s", data)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["unrelated"]["key"] != "other-access" || got["unrelated"]["refresh_token"] != "other-refresh" {
		t.Fatalf("unrelated Grok credential was changed: %#v", got["unrelated"])
	}
}

func TestWriteManagedGrokCredentialToCLIRequiresCanonicalEntry(t *testing.T) {
	home := t.TempDir()
	authDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"unrelated":{"refresh_token":"keep"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := writeManagedGrokCredentialToCLI(home, managedModelAuthCredential{Provider: "xai", Access: "access", Expires: 1893553445000})
	if err == nil || !strings.Contains(err.Error(), "canonical xAI") {
		t.Fatalf("error = %v, want missing canonical credential", err)
	}
}

func resetGatewayProcessState(t *testing.T) {
	t.Helper()
	gatewayProcessState.Lock()
	oldExited := gatewayProcessState.exited
	oldRestartCount := gatewayProcessState.restartCount
	oldSession := gatewayProcessState.session
	gatewayProcessState.exited = false
	gatewayProcessState.restartCount = 0
	gatewayProcessState.session = nil
	gatewayProcessState.Unlock()
	t.Cleanup(func() {
		gatewayProcessState.Lock()
		gatewayProcessState.exited = oldExited
		gatewayProcessState.restartCount = oldRestartCount
		gatewayProcessState.session = oldSession
		gatewayProcessState.Unlock()
	})
}

func TestSuperviseGatewayLoopRestartsAndSupervisesNewProcess(t *testing.T) {
	resetGatewayProcessState(t)
	nextWaitStarted := make(chan struct{})
	releaseNextWait := make(chan struct{})
	done := make(chan struct{})
	var sleeps []time.Duration
	restarts := 0
	nextWaitSignaled := false
	initialWaitCalls := 0

	go func() {
		superviseGatewayLoop(
			func() error {
				initialWaitCalls++
				return errors.New("initial exit")
			},
			func() (func() error, int, error) {
				restarts++
				if restarts == 1 {
					return nil, 0, errors.New("start failed")
				}
				if restarts > 2 {
					return nil, 0, errors.New("start failed")
				}
				return func() error {
					if !nextWaitSignaled {
						close(nextWaitStarted)
						nextWaitSignaled = true
					}
					<-releaseNextWait
					return errors.New("replacement exit")
				}, 1234, nil
			},
			func(d time.Duration) { sleeps = append(sleeps, d) },
			time.Now,
			markGatewayProcessExited,
			markGatewayProcessRunning,
			incrementGatewayRestartCount,
		)
		close(done)
	}()

	<-nextWaitStarted
	if gatewayProcessExited() {
		t.Fatal("gateway remains exited after successful restart")
	}
	if got := gatewayRestartCount(); got != 1 {
		t.Fatalf("restart count = %d, want 1", got)
	}
	if len(sleeps) != 2 || sleeps[0] != 2*time.Second || sleeps[1] != 4*time.Second {
		t.Fatalf("restart delays = %v, want [2s 4s]", sleeps)
	}
	// The first restart attempt failed; the supervisor must retry the restart
	// without re-invoking the stale wait fn of the already-exited process.
	if initialWaitCalls != 1 {
		t.Fatalf("initial wait called %d times, want 1 (failed restart must not re-wait a dead process)", initialWaitCalls)
	}
	close(releaseNextWait)
	<-done
}

func TestSuperviseGatewayLoopExhaustsRestartBudget(t *testing.T) {
	resetGatewayProcessState(t)
	var sleeps []time.Duration
	waitCalls := 0
	superviseGatewayLoop(
		func() error {
			waitCalls++
			return errors.New("exit")
		},
		func() (func() error, int, error) { return nil, 0, errors.New("start failed") },
		func(d time.Duration) { sleeps = append(sleeps, d) },
		time.Now,
		markGatewayProcessExited,
		markGatewayProcessRunning,
		incrementGatewayRestartCount,
	)
	if !gatewayProcessExited() {
		t.Fatal("gateway marked running after exhausted restart budget")
	}
	if waitCalls != 1 {
		t.Fatalf("wait called %d times, want 1 (only real process exits should be awaited)", waitCalls)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("restart delays = %v, want %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("restart delay %d = %v, want %v", i, sleeps[i], want[i])
		}
	}
}

func TestSuperviseGatewayLoopStableUptimeResetsBudget(t *testing.T) {
	resetGatewayProcessState(t)
	var sleeps []time.Duration
	times := []time.Time{
		time.Unix(0, 0), time.Unix(0, 0),
		time.Unix(0, 0), time.Unix(60, 0),
	}
	nowCalls := 0
	now := func() time.Time {
		if nowCalls >= len(times) {
			return times[len(times)-1]
		}
		v := times[nowCalls]
		nowCalls++
		return v
	}
	restarts := 0
	superviseGatewayLoop(
		func() error { return errors.New("exit") },
		func() (func() error, int, error) {
			restarts++
			if restarts == 1 {
				return func() error { return errors.New("stable replacement exit") }, 1234, nil
			}
			return nil, 0, errors.New("start failed")
		},
		func(d time.Duration) { sleeps = append(sleeps, d) },
		now,
		markGatewayProcessExited,
		markGatewayProcessRunning,
		incrementGatewayRestartCount,
	)
	if len(sleeps) < 2 || sleeps[0] != 2*time.Second || sleeps[1] != 2*time.Second {
		t.Fatalf("restart delays = %v, want stable restart to reset second delay to 2s", sleeps)
	}
}

func TestGatewayRestartBaseFromEnv(t *testing.T) {
	t.Setenv("ELASTICCLAW_BRIDGE_RESTARTS", "")
	if got := gatewayRestartBaseFromEnv(); got != 0 {
		t.Fatalf("unset restart base = %d, want 0", got)
	}
	t.Setenv("ELASTICCLAW_BRIDGE_RESTARTS", "2")
	if got := gatewayRestartBaseFromEnv(); got != 2 {
		t.Fatalf("restart base = %d, want 2", got)
	}
	t.Setenv("ELASTICCLAW_BRIDGE_RESTARTS", "garbage")
	if got := gatewayRestartBaseFromEnv(); got != 0 {
		t.Fatalf("invalid restart base = %d, want 0", got)
	}
}

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

func TestMessageDeduperTracksDuplicatesAndEvictsOldest(t *testing.T) {
	deduper := newMessageDeduper()
	if deduper.seen("first") {
		t.Fatal("fresh id reported as already seen")
	}
	if !deduper.seen("first") {
		t.Fatal("repeated id was not reported as seen")
	}
	for i := 0; i < 128; i++ {
		if deduper.seen(fmt.Sprintf("id-%d", i)) {
			t.Fatalf("fresh id-%d reported as already seen", i)
		}
	}
	if deduper.seen("first") {
		t.Fatal("oldest id was not evicted after capacity was exceeded")
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
	// Partial staged content replaces the full managed set so OpenClaw defaults
	// (BOOTSTRAP.md/MEMORY.md) do not linger beside template instructions.
	if _, err := os.Stat(filepath.Join(activeDir, "BOOTSTRAP.md")); !os.IsNotExist(err) {
		t.Fatalf("BOOTSTRAP.md should be removed on partial staged sync, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(activeDir, "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatalf("MEMORY.md should be removed on partial staged sync, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(activeDir, ".elasticclaw-workspace-ready")); !os.IsNotExist(err) {
		t.Fatalf("ready marker should not be copied, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(activeDir, "passwd-link")); !os.IsNotExist(err) {
		t.Fatalf("symlink should not be copied, got err=%v", err)
	}
}

func TestSyncStagedWorkspacePartialSyncClearsManagedSet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	stagedDir := filepath.Join(home, "workspace")
	activeDir := filepath.Join(home, ".openclaw", "workspace")
	if err := os.MkdirAll(stagedDir, 0700); err != nil {
		t.Fatalf("mkdir staged: %v", err)
	}
	if err := os.MkdirAll(activeDir, 0700); err != nil {
		t.Fatalf("mkdir active: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stagedDir, "AGENTS.md"), []byte("from staged\n"), 0600); err != nil {
		t.Fatalf("write staged AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "AGENTS.md"), []byte("from openclaw\n"), 0600); err != nil {
		t.Fatalf("write active AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "SOUL.md"), []byte("stale openclaw default\n"), 0600); err != nil {
		t.Fatalf("write active SOUL.md: %v", err)
	}

	if err := syncStagedWorkspaceToOpenClawWorkspace(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	gotAgents, err := os.ReadFile(filepath.Join(activeDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(gotAgents) != "from staged\n" {
		t.Fatalf("AGENTS.md = %q, want staged content", string(gotAgents))
	}
	if _, err := os.Stat(filepath.Join(activeDir, "SOUL.md")); !os.IsNotExist(err) {
		t.Fatalf("SOUL.md should be cleared when staged has content but omits it, got err=%v", err)
	}
}

func TestSyncStagedWorkspaceEmptyStagedPreservesManagedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	stagedDir := filepath.Join(home, "workspace")
	activeDir := filepath.Join(home, ".openclaw", "workspace")
	if err := os.MkdirAll(stagedDir, 0700); err != nil {
		t.Fatalf("mkdir staged: %v", err)
	}
	if err := os.MkdirAll(activeDir, 0700); err != nil {
		t.Fatalf("mkdir active: %v", err)
	}
	// Ready marker alone does not count as staged content (and is never copied).
	if err := os.WriteFile(filepath.Join(stagedDir, ".elasticclaw-workspace-ready"), []byte("ready\n"), 0600); err != nil {
		t.Fatalf("write ready marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "AGENTS.md"), []byte("keep openclaw agents\n"), 0600); err != nil {
		t.Fatalf("write active AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(activeDir, "SOUL.md"), []byte("keep openclaw soul\n"), 0600); err != nil {
		t.Fatalf("write active SOUL.md: %v", err)
	}

	if err := syncStagedWorkspaceToOpenClawWorkspace(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	gotAgents, err := os.ReadFile(filepath.Join(activeDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if string(gotAgents) != "keep openclaw agents\n" {
		t.Fatalf("AGENTS.md = %q, want preserved when staged empty", string(gotAgents))
	}
	gotSoul, err := os.ReadFile(filepath.Join(activeDir, "SOUL.md"))
	if err != nil {
		t.Fatalf("read SOUL.md: %v", err)
	}
	if string(gotSoul) != "keep openclaw soul\n" {
		t.Fatalf("SOUL.md = %q, want preserved when staged empty", string(gotSoul))
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
	if !strings.Contains(script, `credential.helper "!$helper_path"`) {
		t.Fatalf("helper script should register helper as an explicit shell command:\n%s", script)
	}
	if !strings.Contains(script, `git config --global credential.useHttpPath true`) {
		t.Fatalf("helper script should include repo path in credential cache key:\n%s", script)
	}
	if !strings.Contains(script, `printf 'protocol=https\nhost=github.com\n\n' | "$helper_path"`) {
		t.Fatalf("helper script should execute helper with a minimal credential request for diagnostics:\n%s", script)
	}
	if !strings.Contains(script, `GitHub credential helper did not output 'username=x-access-token'`) {
		t.Fatalf("helper script should diagnose missing helper username:\n%s", script)
	}
	if !strings.Contains(script, `GitHub credential helper did not output a password`) {
		t.Fatalf("helper script should diagnose missing helper password:\n%s", script)
	}
	if !strings.Contains(script, `curl -sS`) {
		t.Fatalf("helper script should show curl errors during diagnostics:\n%s", script)
	}
	if !strings.Contains(script, `printf '%s\n' "$response" >&2`) {
		t.Fatalf("helper script should print token endpoint response when token is missing:\n%s", script)
	}
	if !strings.Contains(script, `git credential fill`) {
		t.Fatalf("helper script should verify credential fill before cloning:\n%s", script)
	}
	if !strings.Contains(script, `--data-urlencode "claw_token=$claw_token"`) {
		t.Fatalf("helper script should URL-encode claw token at runtime:\n%s", script)
	}
	if !strings.Contains(script, `path=*`) {
		t.Fatalf("helper script should parse the repo path from git credential input:\n%s", script)
	}
	if !strings.Contains(script, `${repo:+--data-urlencode "repo=$repo"}`) {
		t.Fatalf("helper script should pass the repo to the hub when a path is provided:\n%s", script)
	}
}

func TestDockerGitHubCredentialHelperBinaryPassesRepoFromPath(t *testing.T) {
	var gotRepo, gotClawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/github/token/claw-1" {
			t.Errorf("path = %s, want /api/github/token/claw-1", r.URL.Path)
		}
		gotClawToken = r.URL.Query().Get("claw_token")
		gotRepo = r.URL.Query().Get("repo")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"token":"ghs_example","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	tmp := t.TempDir()
	helperPath := filepath.Join(tmp, "elasticclaw-git-credentials")
	if err := os.WriteFile(helperPath, []byte(dockerGitHubCredentialHelperBinary(srv.URL+"/api/github/token/claw-1")), 0700); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	cmd := exec.Command(helperPath)
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=example-org/example-repo.git\n\n")
	cmd.Env = append(os.Environ(), "ELASTICCLAW_CLAW_TOKEN=secret-token")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	if gotClawToken != "secret-token" {
		t.Fatalf("claw_token = %q, want secret-token", gotClawToken)
	}
	if gotRepo != "example-org/example-repo" {
		t.Fatalf("repo = %q, want example-org/example-repo", gotRepo)
	}
	wantOutput := "password=ghs_example"
	if !strings.Contains(string(out), wantOutput) {
		t.Fatalf("helper output missing password line:\n%s", out)
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

func TestToolActivityOutcomeFields(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "call id comes from payload",
			run: func(t *testing.T) {
				data := map[string]interface{}{"tool_call": map[string]interface{}{"callId": "gateway-call-7"}}
				if got := toolCallID(data); got != "gateway-call-7" {
					t.Fatalf("payload call ID = %q", got)
				}
			},
		},
		{
			name: "entity ids inside tool arguments are not call ids",
			run: func(t *testing.T) {
				data := map[string]interface{}{
					"phase": "running",
					"input": map[string]interface{}{"id": "ENG-123", "call_id": "also-not-ours"},
				}
				if got := toolCallID(data); got != "" {
					t.Fatalf("call ID scraped from tool arguments: %q", got)
				}
			},
		},
		{
			name: "progress updates without an id reuse the open call",
			run: func(t *testing.T) {
				inf := &inFlightState{}
				at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
				first := agentActivity{Kind: "tool", Stream: "tool", Phase: "started", Tool: "exec", Command: "go test ./..."}
				inf.resolveToolCall(&first, at)
				progress := agentActivity{Kind: "tool", Stream: "tool", Phase: "in_progress", Tool: "exec", Command: "go test ./..."}
				inf.resolveToolCall(&progress, at.Add(time.Second))
				if first.CallID == "" || first.CallID != progress.CallID {
					t.Fatalf("IDs = %q, %q; want progress to reuse the open call", first.CallID, progress.CallID)
				}
				if want := toolCallSignature("tool", "exec", "go test ./...", "", "") + "-1"; first.CallID != want {
					t.Fatalf("first ID = %q, want %q", first.CallID, want)
				}
				if len(inf.inFlightCalls) != 1 {
					t.Fatalf("in-flight calls = %d, want 1 (no phantom entry per progress event)", len(inf.inFlightCalls))
				}
			},
		},
		{
			name: "sequential identical calls get distinct ids",
			run: func(t *testing.T) {
				inf := &inFlightState{}
				at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
				first := agentActivity{Kind: "tool", Stream: "tool", Phase: "started", Tool: "exec", Command: "go test ./..."}
				inf.resolveToolCall(&first, at)
				done := agentActivity{Kind: "tool", Stream: "tool", Phase: "completed", Tool: "exec", Command: "go test ./..."}
				inf.resolveToolCall(&done, at.Add(time.Second))
				if done.CallID != first.CallID || done.DurationMs != 1000 {
					t.Fatalf("terminal id/duration = %q/%d, want %q/1000", done.CallID, done.DurationMs, first.CallID)
				}
				second := agentActivity{Kind: "tool", Stream: "tool", Phase: "started", Tool: "exec", Command: "go test ./..."}
				inf.resolveToolCall(&second, at.Add(2*time.Second))
				if second.CallID == "" || second.CallID == first.CallID {
					t.Fatalf("IDs = %q, %q; want distinct ids across sequential calls", first.CallID, second.CallID)
				}
			},
		},
		{
			name: "terminal without id matches a payload-id start by signature",
			run: func(t *testing.T) {
				inf := &inFlightState{}
				at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
				started := agentActivity{Kind: "tool", Stream: "tool", Phase: "started", Tool: "exec", Command: "sleep 5", CallID: "gw-1"}
				inf.resolveToolCall(&started, at)
				done := agentActivity{Kind: "tool", Stream: "tool", Phase: "completed", Tool: "exec", Command: "sleep 5"}
				inf.resolveToolCall(&done, at.Add(5*time.Second))
				if done.CallID != "gw-1" || done.DurationMs != 5000 {
					t.Fatalf("terminal id/duration = %q/%d, want gw-1/5000", done.CallID, done.DurationMs)
				}
				if len(inf.inFlightCalls) != 0 {
					t.Fatalf("in-flight calls = %d, want 0", len(inf.inFlightCalls))
				}
			},
		},
		{
			name: "terminal event receives duration",
			run: func(t *testing.T) {
				inf := &inFlightState{}
				started := agentActivity{Kind: "tool", Phase: "started", Tool: "exec", CallID: "call-1"}
				inf.resolveToolCall(&started, time.Unix(100, 0))
				completed := agentActivity{Kind: "tool", Phase: "completed", Tool: "exec", CallID: "call-1"}
				inf.resolveToolCall(&completed, time.Unix(101, 250000000))
				if completed.DurationMs != 1250 || len(inf.inFlightCalls) != 0 {
					t.Fatalf("duration/state = %d/%d, want 1250/0", completed.DurationMs, len(inf.inFlightCalls))
				}
			},
		},
		{
			name: "exit code zero survives cleaning",
			run: func(t *testing.T) {
				code := nestedExitCode(map[string]interface{}{"exit_code": float64(0)}, "exit_code", "exitCode", "status_code")
				activity := cleanAgentActivity(agentActivity{Kind: "tool", ExitCode: code})
				if activity.ExitCode == nil || *activity.ExitCode != 0 {
					t.Fatalf("exit code = %#v, want pointer to 0", activity.ExitCode)
				}
			},
		},
		{
			name: "result truncates at rune boundary",
			run: func(t *testing.T) {
				result := truncateResult(strings.Repeat("é", 1001), 2000)
				if !utf8.ValidString(result) || !strings.HasSuffix(result, "\n… (truncated)") || !strings.HasPrefix(result, strings.Repeat("é", 1000)) {
					t.Fatalf("result was not safely truncated: %q", result[len(result)-min(len(result), 40):])
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, tt.run)
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

func TestResolveSubagentFields(t *testing.T) {
	longPrompt := strings.Repeat("x", 501)
	tests := []struct {
		name                                      string
		tool                                      string
		phase                                     string
		data                                      map[string]interface{}
		wantName, wantType, wantModel, wantPrompt string
		cleanPrompt                               bool
	}{
		{
			name: "task input fields",
			tool: "Task",
			data: map[string]interface{}{"input": map[string]interface{}{
				"description": "research auth flow", "prompt": "Inspect the auth flow.", "subagent_type": "research", "model": "gpt-5",
			}},
			wantName: "research auth flow", wantType: "research", wantModel: "gpt-5", wantPrompt: "Inspect the auth flow.",
		},
		{
			name: "task raw params fields",
			tool: "task",
			data: map[string]interface{}{"raw_params": map[string]interface{}{
				"description": "write tests", "prompt": "Add bridge tests.", "subagent_type": "coder", "model_id": "gpt-5-mini",
			}},
			wantName: "write tests", wantType: "coder", wantModel: "gpt-5-mini", wantPrompt: "Add bridge tests.",
		},
		{
			name:     "name equal to tool is never subagent name",
			tool:     "task",
			data:     map[string]interface{}{"name": "task", "description": "useful subagent"},
			wantName: "useful subagent",
		},
		{
			name:     "generic title falls through to nested description",
			tool:     "Task",
			phase:    "running",
			data:     map[string]interface{}{"title": "Task", "input": map[string]interface{}{"description": "research auth flow", "prompt": "Inspect the auth flow."}},
			wantName: "research auth flow", wantPrompt: "Inspect the auth flow.",
		},
		{
			name:  "generic top-level message does not shadow nested prompt",
			tool:  "Task",
			phase: "running",
			data: map[string]interface{}{
				"message": "Running Task",
				"input": map[string]interface{}{
					"description": "research auth flow", "prompt": "Inspect the auth flow end to end.", "subagent_type": "research",
				},
			},
			wantName: "research auth flow", wantType: "research", wantPrompt: "Inspect the auth flow end to end.",
		},
		{
			name:  "generic task field equal to tool name is skipped as prompt",
			tool:  "Task",
			phase: "running",
			data: map[string]interface{}{
				"task":  "Task",
				"input": map[string]interface{}{"prompt": "Real prompt."},
			},
			wantPrompt: "Real prompt.",
		},
		{
			name: "non task tool is empty",
			tool: "exec",
			data: map[string]interface{}{"input": map[string]interface{}{
				"description": "not a subagent", "prompt": "echo hi", "subagent_type": "coder", "model": "gpt-5",
			}},
		},
		{
			name: "task prompt truncates during cleaning",
			tool: "task",
			data: map[string]interface{}{"input": map[string]interface{}{
				"prompt": longPrompt,
			}},
			wantPrompt:  longPrompt,
			cleanPrompt: true,
		},
		{
			name:  "terminal message is not the prompt",
			tool:  "Task",
			phase: "completed",
			data:  map[string]interface{}{"message": "Task completed", "title": "Task completed"},
		},
		{
			name:  "terminal explicit input fields still resolve",
			tool:  "Task",
			phase: "completed",
			data: map[string]interface{}{
				"message": "Task completed",
				"input": map[string]interface{}{
					"description": "research auth flow", "prompt": "Inspect the auth flow.", "subagent_type": "research", "model": "gpt-5",
				},
			},
			wantName: "research auth flow", wantType: "research", wantModel: "gpt-5", wantPrompt: "Inspect the auth flow.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, subType, model, prompt := resolveSubagentFields(tt.tool, tt.phase, tt.data)
			if name != tt.wantName || subType != tt.wantType || model != tt.wantModel || prompt != tt.wantPrompt {
				t.Fatalf("resolveSubagentFields() = (%q, %q, %q, %q), want (%q, %q, %q, %q)", name, subType, model, prompt, tt.wantName, tt.wantType, tt.wantModel, tt.wantPrompt)
			}
			if tt.cleanPrompt {
				activity := cleanAgentActivity(agentActivity{SubagentPrompt: prompt})
				if activity.SubagentPrompt != truncateResult(longPrompt, 500) {
					t.Fatalf("SubagentPrompt = %q, want %q", activity.SubagentPrompt, truncateResult(longPrompt, 500))
				}
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

func TestInstallOpenClawWithoutSudoInstallsAndRunsPinnedVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake npm executable uses POSIX shell")
	}

	home := t.TempDir()
	fakeBin := t.TempDir()
	npmLog := filepath.Join(t.TempDir(), "npm-args")
	t.Setenv("HOME", home)
	t.Setenv("FAKE_NPM_LOG", npmLog)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	writeExecutable := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0755); err != nil {
			t.Fatalf("write executable %s: %v", path, err)
		}
	}
	writeExecutable(filepath.Join(fakeBin, "openclaw"), "#!/bin/sh\necho 'OpenClaw 0.0.0'\n")
	writeExecutable(filepath.Join(fakeBin, "npm"), fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$*" > "$FAKE_NPM_LOG"
mkdir -p "$HOME/.local/bin"
cat > "$HOME/.local/bin/openclaw" <<'EOF'
#!/bin/sh
echo 'OpenClaw %s'
EOF
chmod +x "$HOME/.local/bin/openclaw"
`, openClawVersion))

	if err := installOpenClaw(); err != nil {
		t.Fatalf("installOpenClaw(): %v", err)
	}

	args, err := os.ReadFile(npmLog)
	if err != nil {
		t.Fatalf("read npm arguments: %v", err)
	}
	gotArgs := strings.TrimSpace(string(args))
	if strings.Contains(gotArgs, "sudo") {
		t.Fatalf("npm install unexpectedly requires sudo: %s", gotArgs)
	}
	if !strings.Contains(gotArgs, "--prefix "+filepath.Join(home, ".local")) {
		t.Fatalf("npm install does not use the user prefix: %s", gotArgs)
	}
	if !strings.Contains(gotArgs, "openclaw@"+openClawVersion) {
		t.Fatalf("npm install does not pin OpenClaw: %s", gotArgs)
	}

	path, err := exec.LookPath("openclaw")
	if err != nil {
		t.Fatalf("find installed openclaw: %v", err)
	}
	wantPath := filepath.Join(home, ".local", "bin", "openclaw")
	if path != wantPath {
		t.Fatalf("openclaw path = %q, want %q", path, wantPath)
	}
	out, err := exec.Command("openclaw", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("run installed openclaw: %v: %s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "OpenClaw "+openClawVersion; got != want {
		t.Fatalf("openclaw --version = %q, want %q", got, want)
	}
}

func TestInstallSelectedModelPluginInstallsPinnedCodexPluginOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake openclaw executable uses POSIX shell")
	}

	home := t.TempDir()
	fakeBin := t.TempDir()
	invocations := filepath.Join(t.TempDir(), "openclaw-args")
	installedMarker := filepath.Join(t.TempDir(), "codex-installed")
	t.Setenv("HOME", home)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ELASTICCLAW_LLM_PROVIDER", "codex")
	t.Setenv("OPENCLAW_INVOCATIONS", invocations)
	t.Setenv("CODEX_INSTALLED_MARKER", installedMarker)

	fakeOpenClaw := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$OPENCLAW_INVOCATIONS"
if [ "$*" = "plugins info codex --json" ]; then
  test -f "$CODEX_INSTALLED_MARKER"
  exit $?
fi
case "$*" in
  "plugins install "*) touch "$CODEX_INSTALLED_MARKER" ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "openclaw"), []byte(fakeOpenClaw), 0755); err != nil {
		t.Fatalf("write fake openclaw: %v", err)
	}

	if err := installSelectedModelPlugin(); err != nil {
		t.Fatalf("installSelectedModelPlugin(): %v", err)
	}
	if err := installSelectedModelPlugin(); err != nil {
		t.Fatalf("second installSelectedModelPlugin(): %v", err)
	}

	logData, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatalf("read OpenClaw invocations: %v", err)
	}
	logText := string(logData)
	wantInstall := "plugins install npm:@openclaw/codex@" + codexPluginVersion
	if strings.Count(logText, wantInstall) != 1 {
		t.Fatalf("Codex plugin install count = %d, want 1\n%s", strings.Count(logText, wantInstall), logText)
	}
	if strings.Count(logText, "plugins info codex --json") != 3 {
		t.Fatalf("Codex plugin verification count = %d, want 3\n%s", strings.Count(logText, "plugins info codex --json"), logText)
	}
}

func TestInstallSelectedModelPluginSkipsNonCodexProvider(t *testing.T) {
	t.Setenv("ELASTICCLAW_LLM_PROVIDER", "openai")
	t.Setenv("PATH", t.TempDir())

	if err := installSelectedModelPlugin(); err != nil {
		t.Fatalf("installSelectedModelPlugin(): %v", err)
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

func TestRestoreCLIModelAuthMigratesGrokOAuthToOpenClaw(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bundle := cliAuthBundle{
		Files: map[string]string{
			".grok/auth.json": base64.StdEncoding.EncodeToString([]byte(`{"https://auth.x.ai::client":{"key":"access-token","refresh_token":"refresh-token","expires_at":"2030-01-02T03:04:05Z","email":"dev@example.com","user_id":"user-123","oidc_issuer":"https://auth.x.ai"}}`)),
		},
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ELASTICCLAW_MODEL_AUTH_PROVIDER", "grok")
	t.Setenv("ELASTICCLAW_MODEL_AUTH_STATE", base64.StdEncoding.EncodeToString(data))

	if err := restoreCLIModelAuth(); err != nil {
		t.Fatalf("restore auth: %v", err)
	}
	authData, err := os.ReadFile(filepath.Join(home, ".openclaw", "agents", "main", "agent", "auth-profiles.json"))
	if err != nil {
		t.Fatalf("read OpenClaw auth profile: %v", err)
	}
	var auth struct {
		Version  int `json:"version"`
		Profiles map[string]struct {
			Type      string `json:"type"`
			Provider  string `json:"provider"`
			Access    string `json:"access"`
			Refresh   string `json:"refresh"`
			Expires   int64  `json:"expires"`
			Email     string `json:"email"`
			AccountID string `json:"accountId"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(authData, &auth); err != nil {
		t.Fatalf("parse OpenClaw auth profile: %v", err)
	}
	credential := auth.Profiles["xai:default"]
	if auth.Version != 1 || credential.Type != "oauth" || credential.Provider != "xai" {
		t.Fatalf("unexpected OpenClaw auth profile: %#v", auth)
	}
	if credential.Access != "access-token" || credential.Refresh != "elasticclaw-managed" || credential.Expires != 1893553445000 {
		t.Fatalf("OAuth tokens were not migrated: %#v", credential)
	}
	if credential.Email != "dev@example.com" || credential.AccountID != "user-123" {
		t.Fatalf("OAuth metadata was not migrated: %#v", credential)
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

func TestSanitizeActivityTextTruncatesAtReasonableLength(t *testing.T) {
	input := strings.Repeat("a", 5000)
	got := sanitizeActivityText(input)
	if len(got) > 2005 {
		t.Fatalf("sanitizeActivityText produced %d chars, want <= 2005: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("sanitizeActivityText should suffix with ...: %q", got)
	}
	// Content under the limit should be preserved unchanged.
	short := strings.Repeat("b", 1500)
	if got := sanitizeActivityText(short); got != short {
		t.Fatalf("sanitizeActivityText truncated short input: %q", got)
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

func TestDeliverInFlightDoesNotBlockWhenTerminalResultRacesTeardown(t *testing.T) {
	inf := &inFlightState{done: make(chan agentResult, 1)}
	inf.done <- agentResult{text: "already complete"}

	finished := make(chan struct{})
	go func() {
		deliverInFlight(inf, agentResult{text: "duplicate lifecycle end"})
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("terminal lifecycle delivery blocked on a full in-flight channel")
	}
}

func TestSessionKeyRotationFailsInFlightTurn(t *testing.T) {
	inf := &inFlightState{done: make(chan agentResult, 1)}
	gs := &gatewaySession{sessionKey: "old-session", inFlight: inf}

	gs.setSessionKey("new-session")

	select {
	case result := <-inf.done:
		if result.err == nil || !strings.Contains(result.err.Error(), "session key rotated") {
			t.Fatalf("rotation result = %#v, want session key rotation error", result)
		}
	case <-time.After(time.Second):
		t.Fatal("session key rotation did not fail the in-flight turn")
	}
}

// stableGoroutineCount samples runtime.NumGoroutine until two consecutive
// readings agree, so leak assertions start from a settled baseline instead of
// a count inflated by goroutines still winding down from earlier tests.
func stableGoroutineCount(t *testing.T) int {
	t.Helper()
	prev := runtime.NumGoroutine()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		cur := runtime.NumGoroutine()
		if cur == prev {
			return cur
		}
		prev = cur
	}
	return prev
}

func TestHubKeepalivesExitWhenConnectionContextIsCancelled(t *testing.T) {
	before := stableGoroutineCount(t)
	for range 10 {
		ctx, cancel := context.WithCancel(context.Background())
		startHubKeepalives(ctx, func(context.Context) error { return nil }, func() {}, func() {})
		cancel()
	}

	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Fatalf("goroutines after cancelled hub connections = %d, want at most %d", got, before+2)
	}
}

// TestRunHubLoopDoesNotLeakKeepaliveGoroutinesAcrossReconnects drives the real
// runHubLoop through several connect/disconnect cycles against an in-process
// fake hub and asserts the keepalive goroutines started per connection do not
// outlive it. This fails if the heartbeat/ping goroutines are keyed to the
// process-lifetime context instead of the per-connection context.
func TestRunHubLoopDoesNotLeakKeepaliveGoroutinesAcrossReconnects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		var reg hubMsg
		if err := wsjson.Read(r.Context(), conn, &reg); err != nil {
			return
		}
		if reg.Type != "register" {
			t.Errorf("first frame type = %q, want register", reg.Type)
			return
		}
		if err := wsjson.Write(r.Context(), conn, hubMsg{Type: "registered"}); err != nil {
			return
		}
		// Close immediately to force the bridge onto its reconnect path.
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	gwClient := &gatewayClient{addr: "127.0.0.1:0"}
	gwSession := &gatewaySession{pending: make(map[string]chan gwFrame)}
	proxy := newHTTPProxy(nil)
	queue := &msgQueue{}

	before := stableGoroutineCount(t)
	const cycles = 5
	for range cycles {
		err := runHubLoop(ctx, wsURL, "claw-test", "test-claw", "test-template", "tok", "",
			bridgeRegistration(false), nil, gwClient, gwSession, proxy, queue, newMessageDeduper())
		if err == nil {
			t.Fatal("runHubLoop returned nil error, want read error after hub-side close")
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Fatalf("goroutines after %d hub reconnect cycles = %d, want at most %d (baseline %d)", cycles, got, before+2, before)
	}
}

// TestRunHubLoopReportsMidTurnLockConflictRecovery proves the live hub-message
// path reports the recovery edge before closing the claw turn. Without that
// edge, a preserved session has no hub-side continuation and can stall forever.
// Coverage boundary: this exercises the LIVE turn call site only. The replay
// path after a hub reconnect calls the same reportSessionRecovery helper with
// the same arguments, but reaching it needs a different harness, so replacing
// that one call with `if true` still passes. The risk is bounded because both
// call sites now delegate to one helper rather than duplicating the block.
func TestRunHubLoopReportsMidTurnLockConflictRecovery(t *testing.T) {
	oldDelays := sessionLockConflictRetryDelays
	sessionLockConflictRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { sessionLockConflictRetryDelays = oldDelays })

	const lockConflict = "error: session file changed while embedded prompt lock was released"
	for _, tt := range []struct {
		name      string
		rotateKey bool
		wantEdge  string
	}{
		{name: "preserved", wantEdge: "session_preserved"},
		{name: "rotated", rotateKey: true, wantEdge: "session_rotated"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var gs *gatewaySession
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Errorf("accept gateway websocket: %v", err)
					return
				}
				defer conn.CloseNow()
				read := func(want string) (gwFrame, bool) {
					var req gwFrame
					if err := wsjson.Read(r.Context(), conn, &req); err != nil {
						t.Errorf("read %s request: %v", want, err)
						return req, false
					}
					if req.Method != want {
						t.Errorf("gateway request method = %q, want %q", req.Method, want)
						return req, false
					}
					return req, true
				}
				send, ok := read("sessions.send")
				if !ok {
					return
				}
				if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: send.ID, OK: true}); err != nil {
					t.Errorf("accept sessions.send: %v", err)
					return
				}
				if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]interface{}{
					"stream": "lifecycle", "sessionKey": "session-1", "data": map[string]string{"phase": "error", "error": lockConflict},
				})}); err != nil {
					t.Errorf("write lifecycle error: %v", err)
					return
				}
				abort, ok := read("sessions.abort")
				if !ok {
					return
				}
				if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abort.ID, OK: true}); err != nil {
					t.Errorf("accept sessions.abort: %v", err)
					return
				}
				probe, ok := read("sessions.describe")
				if !ok {
					return
				}
				if tt.rotateKey {
					gs.setSessionKey("session-2")
				}
				if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: probe.ID, OK: true}); err != nil {
					t.Errorf("accept sessions.describe: %v", err)
				}
			}))
			defer gateway.Close()

			received := make(chan []hubMsg, 1)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Errorf("accept hub websocket: %v", err)
					return
				}
				defer conn.CloseNow()
				var registration hubMsg
				if err := wsjson.Read(r.Context(), conn, &registration); err != nil {
					t.Errorf("read registration: %v", err)
					return
				}
				if err := wsjson.Write(r.Context(), conn, hubMsg{Type: "registered"}); err != nil {
					t.Errorf("write registration acknowledgement: %v", err)
					return
				}
				if err := wsjson.Write(r.Context(), conn, hubMsg{Type: "message", Payload: mustJSON(map[string]interface{}{"id": "turn-1", "content": "continue the task"})}); err != nil {
					t.Errorf("write hub turn: %v", err)
					return
				}
				var messages []hubMsg
				for {
					var msg hubMsg
					if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
						return
					}
					messages = append(messages, msg)
					if msg.Type == "message" {
						received <- messages
						return
					}
				}
			}))
			defer hub.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gatewayCtx, gatewayCancel := context.WithCancel(ctx)
			defer gatewayCancel()
			conn, _, err := websocket.Dial(gatewayCtx, "ws"+strings.TrimPrefix(gateway.URL, "http"), nil)
			if err != nil {
				t.Fatalf("dial gateway: %v", err)
			}
			defer conn.CloseNow()
			gs = &gatewaySession{sessionKey: "session-1", ready: true, conn: conn, pending: make(map[string]chan gwFrame)}
			go gs.readLoop(gatewayCtx)

			loopDone := make(chan error, 1)
			go func() {
				loopDone <- runHubLoop(ctx, "ws"+strings.TrimPrefix(hub.URL, "http"), "claw-test", "test-claw", "test-template", "tok", "",
					bridgeRegistration(false), nil, &gatewayClient{addr: "127.0.0.1:0"}, gs, newHTTPProxy(nil), &msgQueue{}, newMessageDeduper())
			}()

			select {
			case messages := <-received:
				var gotEdge string
				for _, msg := range messages {
					if msg.Type == "session_preserved" || msg.Type == "session_rotated" {
						gotEdge = msg.Type
					}
					if msg.Type == "message" {
						var payload map[string]string
						if err := json.Unmarshal(msg.Payload, &payload); err != nil {
							t.Fatalf("decode claw message: %v", err)
						}
						if payload["content"] != "" {
							t.Fatalf("claw message content = %q, want empty", payload["content"])
						}
					}
				}
				if gotEdge != tt.wantEdge {
					t.Fatalf("recovery edge = %q, want %q (messages: %#v)", gotEdge, tt.wantEdge, messages)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("hub did not receive recovery edge and empty claw reply")
			}
			cancel()
			select {
			case <-loopDone:
			case <-time.After(time.Second):
				t.Fatal("runHubLoop did not stop after cancellation")
			}
		})
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
		{name: "send request provider format rejection", err: &sessionSendRequestError{err: errString("LLM request failed: " + providerRequestFormatErrorFragment + ".")}, want: true},
		{name: "send request session file lock conflict handled by same-session retry", err: &sessionSendRequestError{err: errString("error: session file changed while embedded prompt lock was released: /home/elasticclaw/.openclaw/agents/main/sessions/4fb88b9b-8233-4f78-819f-516fbe605e32.jsonl")}, want: false},
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

func TestIsRecoverableSessionLifecycleError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "provider format rejection", err: errString("LLM request failed: " + providerRequestFormatErrorFragment + "."), want: true},
		{name: "accepted turn deadline", err: context.DeadlineExceeded, want: true},
		{name: "send request deadline", err: &sessionSendRequestError{err: context.DeadlineExceeded}, want: false},
		{name: "caller cancellation", err: context.Canceled, want: false},
		{name: "ordinary lifecycle error", err: errString("tool-policy: exec is not allowed"), want: false},
		{name: "session file lock conflict", err: errString("error: session file changed while embedded prompt lock was released: /home/elasticclaw/.openclaw/agents/main/sessions/4fb88b9b-8233-4f78-819f-516fbe605e32.jsonl"), want: true},
		{name: "session file changed without lock mention", err: errString("session file changed"), want: false},
		{name: "embedded prompt lock without file changed", err: errString("embedded prompt lock released"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRecoverableSessionLifecycleError(tt.err); got != tt.want {
				t.Fatalf("isRecoverableSessionLifecycleError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsSessionRotatedError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "session reset", err: errString("session file changed while embedded prompt lock was released; " + sessionRotatedErrorSuffix), want: true},
		{name: "other error", err: errString("some other error"), want: false},
		{name: "send error", err: &sessionSendRequestError{err: errString("session file changed while embedded prompt lock was released")}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSessionRotatedError(tt.err); got != tt.want {
				t.Fatalf("isSessionRotatedError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestSessionRecoverySentinelsAreMutuallyExclusive(t *testing.T) {
	if strings.Contains(sessionPreservedErrorSuffix, sessionRotatedErrorSuffix) || strings.Contains(sessionRotatedErrorSuffix, sessionPreservedErrorSuffix) {
		t.Fatalf("recovery sentinel strings must remain mutually exclusive")
	}
	rotated := errString("lock conflict; " + sessionRotatedErrorSuffix)
	preserved := errString("lock conflict; " + sessionPreservedErrorSuffix)
	if !isSessionRotatedError(rotated) || isSessionPreservedError(rotated) {
		t.Fatalf("rotated error classification is not exclusive")
	}
	if !isSessionPreservedError(preserved) || isSessionRotatedError(preserved) {
		t.Fatalf("preserved error classification is not exclusive")
	}
	both := errString(sessionRotatedErrorSuffix + "; " + sessionPreservedErrorSuffix)
	if isSessionRotatedError(both) || isSessionPreservedError(both) {
		t.Fatalf("an ambiguous error must not match either recovery protocol")
	}
}

func TestSessionRecoveryOutcome(t *testing.T) {
	preserved := &sessionPreservedError{err: errString("lock conflict"), key: "session-1"}
	rotated := errString("lock conflict; " + sessionRotatedErrorSuffix)
	tests := []struct {
		name       string
		err        error
		currentKey string
		want       string
	}{
		{name: "preserved matching key", err: preserved, currentKey: "session-1", want: "preserved"},
		{name: "preserved replaced key", err: preserved, currentKey: "session-2", want: "rotated"},
		{name: "rotated error", err: rotated, currentKey: "session-2", want: "rotated"},
		{name: "ordinary error", err: errString("ordinary failure"), currentKey: "session-1", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sessionRecoveryOutcome(tt.err, tt.currentKey); got != tt.want {
				t.Fatalf("sessionRecoveryOutcome(%v, %q) = %q, want %q", tt.err, tt.currentKey, got, tt.want)
			}
		})
	}
}

func TestReportSessionRecoveryEmitsHubEdgeAndClearsReply(t *testing.T) {
	preserved := &sessionPreservedError{err: errString("lock conflict"), key: "session-1"}
	for _, tt := range []struct {
		name       string
		currentKey string
		wantType   string
	}{
		{name: "matching key preserves session", currentKey: "session-1", wantType: "session_preserved"},
		{name: "divergent key rotates session", currentKey: "session-2", wantType: "session_rotated"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gs := &gatewaySession{sessionKey: tt.currentKey}
			queue := &msgQueue{}
			reply := "partially completed reply"
			var messages []hubMsg
			handled := reportSessionRecovery(preserved, &reply, gs, func(agentActivity) {}, func(msg hubMsg) error {
				messages = append(messages, msg)
				return nil
			}, queue)
			if !handled {
				t.Fatal("preserved recovery was not handled")
			}
			if reply != "" {
				t.Fatalf("reply = %q, want empty after recovery", reply)
			}
			if len(messages) != 1 || messages[0].Type != tt.wantType {
				t.Fatalf("hub messages = %#v, want one %q message", messages, tt.wantType)
			}
			// A successful live write is authoritative; replaying it would duplicate
			// a continuation edge.
			if len(queue.msgs) != 0 {
				t.Fatalf("successful recovery edge was queued: %#v", queue.msgs)
			}
		})
	}
}

func TestReportSessionRecoveryQueuesFailedEdgeForReplay(t *testing.T) {
	for _, wantType := range []string{"session_preserved", "session_rotated"} {
		t.Run(wantType, func(t *testing.T) {
			queue := &msgQueue{}
			gs := &gatewaySession{sessionKey: "session-1"}
			err := error(&sessionPreservedError{err: errString("lock conflict"), key: "session-1"})
			if wantType == "session_rotated" {
				gs.sessionKey = "session-2"
			}
			reply := "partial"
			if !reportSessionRecovery(err, &reply, gs, func(agentActivity) {}, func(hubMsg) error { return errString("hub disconnected") }, queue) {
				t.Fatal("recovery was not handled")
			}
			if len(queue.msgs) != 1 || queue.msgs[0].kind != queuedControl || queue.msgs[0].control.Type != wantType {
				t.Fatalf("queued recovery edge = %#v, want one %s control", queue.msgs, wantType)
			}
			var replayed []hubMsg
			replayQueued(queue, func(string, string) error { return nil }, func(msg hubMsg) error {
				replayed = append(replayed, msg)
				return nil
			}, func(string) { t.Fatal("replay must not rerun the user turn") })
			if len(replayed) != 1 || replayed[0].Type != wantType {
				t.Fatalf("replayed edges = %#v, want one %s edge", replayed, wantType)
			}
		})
	}
}

func TestReplayQueuedDeliversRecoveryEdgeBeforeEmptyReply(t *testing.T) {
	queue := &msgQueue{}
	queue.pushControl(hubMsg{Type: "session_preserved"})
	queue.pushReply("")
	var deliveries []string
	replayQueued(queue, func(_ string, content string) error {
		deliveries = append(deliveries, "reply:"+content)
		return nil
	}, func(msg hubMsg) error {
		deliveries = append(deliveries, "edge:"+msg.Type)
		return nil
	}, func(string) { t.Fatal("replay must not rerun the user turn") })
	if got, want := strings.Join(deliveries, ","), "edge:session_preserved,reply:"; got != want {
		t.Fatalf("replay order = %q, want %q", got, want)
	}
}

func TestGatewaySessionResetsAfterProviderFormatLifecycleError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const providerErr = "LLM request failed: " + providerRequestFormatErrorFragment + "."
	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()

		readRequest := func(wantMethod string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s request: %v", wantMethod, err)
				return gwFrame{}, false
			}
			if req.Method != wantMethod {
				t.Errorf("request method = %q, want %q", req.Method, wantMethod)
				return gwFrame{}, false
			}
			return req, true
		}

		sendReq, ok := readRequest("sessions.send")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: sendReq.ID, OK: true}); err != nil {
			t.Errorf("accept sessions.send: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type:  "event",
			Event: "agent",
			Payload: mustJSON(map[string]interface{}{
				"stream":     "lifecycle",
				"sessionKey": "session-1",
				"data": map[string]string{
					"phase": "error",
					"error": providerErr,
				},
			}),
		}); err != nil {
			t.Errorf("write lifecycle error: %v", err)
			return
		}

		abortReq, ok := readRequest("sessions.abort")
		if !ok {
			return
		}
		var abortParams map[string]string
		if err := json.Unmarshal(abortReq.Params, &abortParams); err != nil {
			t.Errorf("decode sessions.abort params: %v", err)
			return
		}
		if abortParams["key"] != "session-1" {
			t.Errorf("sessions.abort key = %q, want session-1", abortParams["key"])
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abortReq.ID, OK: true}); err != nil {
			t.Errorf("abort poisoned session: %v", err)
			return
		}

		createReq, ok := readRequest("sessions.create")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type:    "res",
			ID:      createReq.ID,
			OK:      true,
			Payload: mustJSON(map[string]string{"key": "session-2"}),
		}); err != nil {
			t.Errorf("create replacement session: %v", err)
			return
		}

		subscribeReq, ok := readRequest("sessions.subscribe")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: subscribeReq.ID, OK: true}); err != nil {
			t.Errorf("subscribe replacement session: %v", err)
			return
		}

		<-testDone
	}))
	defer srv.Close()
	defer close(testDone)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.CloseNow()

	gs := &gatewaySession{
		sessionKey: "session-1",
		conn:       conn,
		pending:    make(map[string]chan gwFrame),
	}
	go gs.readLoop(ctx)

	_, err = gs.SendMessage(ctx, "continue the task", nil, nil)
	if err == nil {
		t.Fatal("SendMessage returned nil error")
	}
	if !strings.Contains(err.Error(), providerErr) || !strings.Contains(err.Error(), "session reset") {
		t.Fatalf("SendMessage error = %q, want provider error and recovery notice", err)
	}
	if got := gs.getSessionKey(); got != "session-2" {
		t.Fatalf("session key = %q, want session-2", got)
	}
	if got := loadBridgeSession(); got != "session-2" {
		t.Fatalf("persisted session key = %q, want session-2", got)
	}
}

func TestGatewaySessionResetsAfterSessionFileLockConflict(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldDelays := sessionLockConflictRetryDelays
	sessionLockConflictRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { sessionLockConflictRetryDelays = oldDelays })

	const sessionErr = "error: session file changed while embedded prompt lock was released: /home/elasticclaw/.openclaw/agents/main/sessions/4fb88b9b-8233-4f78-819f-516fbe605e32.jsonl"
	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()

		readRequest := func(wantMethod string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s request: %v", wantMethod, err)
				return gwFrame{}, false
			}
			if req.Method != wantMethod {
				t.Errorf("request method = %q, want %q", req.Method, wantMethod)
				return gwFrame{}, false
			}
			return req, true
		}

		sendReq, ok := readRequest("sessions.send")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: sendReq.ID, OK: true}); err != nil {
			t.Errorf("accept sessions.send: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type:  "event",
			Event: "agent",
			Payload: mustJSON(map[string]interface{}{
				"stream":     "lifecycle",
				"sessionKey": "session-1",
				"data": map[string]string{
					"phase": "error",
					"error": sessionErr,
				},
			}),
		}); err != nil {
			t.Errorf("write lifecycle error: %v", err)
			return
		}

		abortReq, ok := readRequest("sessions.abort")
		if !ok {
			return
		}
		var abortParams map[string]string
		if err := json.Unmarshal(abortReq.Params, &abortParams); err != nil {
			t.Errorf("decode sessions.abort params: %v", err)
			return
		}
		if abortParams["key"] != "session-1" {
			t.Errorf("sessions.abort key = %q, want session-1", abortParams["key"])
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abortReq.ID, OK: true}); err != nil {
			t.Errorf("abort poisoned session: %v", err)
			return
		}

		// The accepted turn may have side effects, so recovery only probes the
		// original session and never replays sessions.send.
		for range sessionLockConflictRetryDelays {
			probeReq, ok := readRequest("sessions.describe")
			if !ok {
				return
			}
			if err := wsjson.Write(r.Context(), conn, gwFrame{
				Type: "res", ID: probeReq.ID, OK: false,
				Error: &gwError{Code: "unavailable", Message: "session unavailable"},
			}); err != nil {
				t.Errorf("reject session probe: %v", err)
				return
			}
		}

		createReq, ok := readRequest("sessions.create")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type:    "res",
			ID:      createReq.ID,
			OK:      true,
			Payload: mustJSON(map[string]string{"key": "session-2"}),
		}); err != nil {
			t.Errorf("create replacement session: %v", err)
			return
		}

		subscribeReq, ok := readRequest("sessions.subscribe")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: subscribeReq.ID, OK: true}); err != nil {
			t.Errorf("subscribe replacement session: %v", err)
			return
		}

		<-testDone
	}))
	defer srv.Close()
	defer close(testDone)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.CloseNow()

	gs := &gatewaySession{
		sessionKey: "session-1",
		conn:       conn,
		pending:    make(map[string]chan gwFrame),
	}
	go gs.readLoop(ctx)

	_, err = gs.SendMessage(ctx, "continue the task", nil, nil)
	if err == nil {
		t.Fatal("SendMessage returned nil error")
	}
	if !strings.Contains(err.Error(), sessionErr) || !strings.Contains(err.Error(), "session reset") {
		t.Fatalf("SendMessage error = %q, want session error and recovery notice", err)
	}
	if got := gs.getSessionKey(); got != "session-2" {
		t.Fatalf("session key = %q, want session-2", got)
	}
	if got := loadBridgeSession(); got != "session-2" {
		t.Fatalf("persisted session key = %q, want session-2", got)
	}
}

func TestGatewaySessionPreservesSessionAfterMidTurnLockConflictProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldDelays := sessionLockConflictRetryDelays
	sessionLockConflictRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { sessionLockConflictRetryDelays = oldDelays })

	const sessionErr = "error: session file changed while embedded prompt lock was released"
	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		readRequest := func(wantMethod string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s request: %v", wantMethod, err)
				return gwFrame{}, false
			}
			if req.Method != wantMethod {
				t.Errorf("request method = %q, want %q", req.Method, wantMethod)
				return gwFrame{}, false
			}
			return req, true
		}
		sendReq, ok := readRequest("sessions.send")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: sendReq.ID, OK: true}); err != nil {
			t.Errorf("accept sessions.send: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]interface{}{
			"stream": "lifecycle", "sessionKey": "session-1", "data": map[string]string{"phase": "error", "error": sessionErr},
		})}); err != nil {
			t.Errorf("write lifecycle error: %v", err)
			return
		}
		abortReq, ok := readRequest("sessions.abort")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abortReq.ID, OK: true}); err != nil {
			t.Errorf("abort session: %v", err)
			return
		}
		probeReq, ok := readRequest("sessions.describe")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: probeReq.ID, OK: true}); err != nil {
			t.Errorf("accept session probe: %v", err)
			return
		}
		// A successful probe must end the recovery: nothing else may reach the
		// gateway. A rotation would send sessions.create and a replay would send
		// a second sessions.send, and the accepted turn may already have run
		// tool side effects. Without this read the test would still pass while
		// silently blocking on an unanswered sessions.create.
		idleCtx, idleCancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer idleCancel()
		var extra gwFrame
		if err := wsjson.Read(idleCtx, conn, &extra); err == nil {
			t.Errorf("unexpected %q request after a successful probe; the session must be preserved", extra.Method)
		}
		<-testDone
	}))
	defer srv.Close()
	defer close(testDone)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.CloseNow()
	gs := &gatewaySession{sessionKey: "session-1", conn: conn, pending: make(map[string]chan gwFrame)}
	go gs.readLoop(ctx)
	_, err = gs.SendMessage(ctx, "continue the task", nil, nil)
	if err == nil || !strings.Contains(err.Error(), sessionErr) {
		t.Fatalf("SendMessage error = %v, want original lock conflict", err)
	}
	if !isSessionPreservedError(err) || isSessionRotatedError(err) {
		t.Fatalf("preserved session error must not be recognized as a rotation: %v", err)
	}
	if got := gs.getSessionKey(); got != "session-1" {
		t.Fatalf("session key = %q, want session-1", got)
	}
	if got := loadBridgeSession(); got != "" {
		t.Fatalf("persisted session key = %q, want empty (no rotation)", got)
	}
}

func TestGatewaySessionProbeRechecksConcurrentRotationAfterSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldDelays := sessionLockConflictRetryDelays
	sessionLockConflictRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { sessionLockConflictRetryDelays = oldDelays })

	const sessionErr = "error: session file changed while embedded prompt lock was released"
	var gs *gatewaySession
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		read := func(want string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s: %v", want, err)
				return req, false
			}
			if req.Method != want {
				t.Errorf("method = %q, want %q", req.Method, want)
				return req, false
			}
			return req, true
		}
		send, ok := read("sessions.send")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: send.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]interface{}{"stream": "lifecycle", "sessionKey": "session-1", "data": map[string]string{"phase": "error", "error": sessionErr}})}); err != nil {
			t.Error(err)
			return
		}
		abort, ok := read("sessions.abort")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abort.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		probe, ok := read("sessions.describe")
		if !ok {
			return
		}
		// Rotate after the read-only probe succeeds to exercise the defensive
		// re-check immediately before reporting the session as preserved.
		gs.setSessionKey("session-2")
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: probe.ID, OK: true}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	gs = &gatewaySession{sessionKey: "session-1", conn: conn, pending: make(map[string]chan gwFrame)}
	go gs.readLoop(ctx)
	_, err = gs.SendMessage(ctx, "continue the task", nil, nil)
	if err == nil || !isSessionRotatedError(err) || isSessionPreservedError(err) {
		t.Fatalf("SendMessage error = %v, want rotation only", err)
	}
}

func TestGatewaySessionProbeDetectsConcurrentRotation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldDelays := sessionLockConflictRetryDelays
	sessionLockConflictRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { sessionLockConflictRetryDelays = oldDelays })
	const sessionErr = "error: session file changed while embedded prompt lock was released"
	var gs *gatewaySession
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		read := func(want string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s: %v", want, err)
				return req, false
			}
			if req.Method != want {
				t.Errorf("method = %q, want %q", req.Method, want)
				return req, false
			}
			return req, true
		}
		send, ok := read("sessions.send")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: send.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]interface{}{"stream": "lifecycle", "sessionKey": "session-1", "data": map[string]string{"phase": "error", "error": sessionErr}})}); err != nil {
			t.Error(err)
			return
		}
		abort, ok := read("sessions.abort")
		if !ok {
			return
		}
		// The reconnect races recovery after it has chosen the turn key but
		// before the gateway receives the abort frame.
		gs.setSessionKey("session-2")
		var abortParams map[string]string
		if err := json.Unmarshal(abort.Params, &abortParams); err != nil {
			t.Errorf("decode sessions.abort params: %v", err)
			return
		}
		if abortParams["key"] != "session-1" {
			t.Errorf("sessions.abort key = %q, want turn session-1", abortParams["key"])
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abort.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		idleCtx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		var extra gwFrame
		if err := wsjson.Read(idleCtx, conn, &extra); err == nil {
			t.Errorf("unexpected %q after concurrent rotation", extra.Method)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	gs = &gatewaySession{sessionKey: "session-1", conn: conn, pending: make(map[string]chan gwFrame)}
	go gs.readLoop(ctx)
	_, err = gs.SendMessage(ctx, "continue the task", nil, nil)
	if err == nil || !isSessionRotatedError(err) || isSessionPreservedError(err) {
		t.Fatalf("SendMessage error = %v, want rotation only", err)
	}
}

func TestGatewaySessionLockConflictAlreadyRotatedSkipsAbortAndProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const sessionErr = "error: session file changed while embedded prompt lock was released"
	var gs *gatewaySession
	// The mismatch-check window inside SendMessage's recovery path (right
	// after the lifecycle error is delivered, right before it compares the
	// turn's key to the current key) is too narrow to land a concurrent
	// rotation into by timing alone. Use the sessionMismatchCheckHook test
	// seam to rotate the key deterministically at that exact point, exactly
	// as readLoop would do concurrently without holding sendMu.
	oldHook := sessionMismatchCheckHook
	t.Cleanup(func() { sessionMismatchCheckHook = oldHook })
	sessionMismatchCheckHook = func() {
		gs.sessionMu.Lock()
		gs.sessionKey = "session-2"
		gs.sessionMu.Unlock()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		var send gwFrame
		if err := wsjson.Read(r.Context(), conn, &send); err != nil || send.Method != "sessions.send" {
			t.Fatalf("read sessions.send: method=%q err=%v", send.Method, err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: send.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		// Tag the event with the still-current key so readLoop's foreign-session
		// filter dispatches it to the in-flight turn instead of dropping it.
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]interface{}{"stream": "lifecycle", "sessionKey": "session-1", "data": map[string]string{"phase": "error", "error": sessionErr}})}); err != nil {
			t.Error(err)
			return
		}
		idleCtx, cancel := context.WithTimeout(r.Context(), 150*time.Millisecond)
		defer cancel()
		var extra gwFrame
		if err := wsjson.Read(idleCtx, conn, &extra); err == nil {
			t.Errorf("unexpected %q after concurrent rotation", extra.Method)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	gs = &gatewaySession{sessionKey: "session-1", conn: conn, pending: make(map[string]chan gwFrame)}
	go gs.readLoop(ctx)
	_, err = gs.SendMessage(ctx, "continue the task", nil, nil)
	if err == nil || !isSessionRotatedError(err) || isSessionPreservedError(err) {
		t.Fatalf("SendMessage error = %v, want rotation only", err)
	}
}

func TestGatewaySessionProbesAfterTurnContextExpires(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	oldDelays := sessionLockConflictRetryDelays
	sessionLockConflictRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { sessionLockConflictRetryDelays = oldDelays })
	const sessionErr = "error: session file changed while embedded prompt lock was released"
	turnCtx, turnCancel := context.WithCancel(context.Background())
	defer turnCancel()
	probed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		read := func(want string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s: %v", want, err)
				return req, false
			}
			if req.Method != want {
				t.Errorf("method = %q, want %q", req.Method, want)
				return req, false
			}
			return req, true
		}
		send, ok := read("sessions.send")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: send.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]interface{}{"stream": "lifecycle", "sessionKey": "session-1", "data": map[string]string{"phase": "error", "error": sessionErr}})}); err != nil {
			t.Error(err)
			return
		}
		abort, ok := read("sessions.abort")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abort.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		// The accepted turn's context has expired, but recovery must use its
		// background probe context rather than abandon the decision here.
		turnCancel()
		probe, ok := read("sessions.describe")
		if !ok {
			return
		}
		close(probed)
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: probe.ID, OK: true}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	connCtx, connCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer connCancel()
	conn, _, err := websocket.Dial(connCtx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	gs := &gatewaySession{sessionKey: "session-1", conn: conn, pending: make(map[string]chan gwFrame)}
	go gs.readLoop(connCtx)
	_, err = gs.SendMessage(turnCtx, "continue the task", nil, nil)
	select {
	case <-probed:
	default:
		t.Fatal("SendMessage did not run a background probe after turn context expired")
	}
	if err == nil || (!isSessionPreservedError(err) && !isSessionRotatedError(err)) {
		t.Fatalf("SendMessage error = %v, want recovery sentinel", err)
	}
}

func TestGatewaySessionRetriesSameSessionAfterLockConflictSendError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldDelays := sessionLockConflictRetryDelays
	sessionLockConflictRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { sessionLockConflictRetryDelays = oldDelays })

	const sessionErr = "error: session file changed while embedded prompt lock was released: /home/elasticclaw/.openclaw/agents/main/sessions/4fb88b9b-8233-4f78-819f-516fbe605e32.jsonl"
	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()

		readRequest := func(wantMethod string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s request: %v", wantMethod, err)
				return gwFrame{}, false
			}
			if req.Method != wantMethod {
				t.Errorf("request method = %q, want %q", req.Method, wantMethod)
				return gwFrame{}, false
			}
			return req, true
		}

		// First send is rejected with the lock-conflict error.
		sendReq, ok := readRequest("sessions.send")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type: "res",
			ID:   sendReq.ID,
			OK:   false,
			Error: &gwError{
				Code:    "session_file_lock_conflict",
				Message: sessionErr,
			},
		}); err != nil {
			t.Errorf("reject first sessions.send: %v", err)
			return
		}

		// The retry must go to the SAME session — no rotation, no transcript loss.
		retrySendReq, ok := readRequest("sessions.send")
		if !ok {
			return
		}
		var retrySendParams map[string]string
		if err := json.Unmarshal(retrySendReq.Params, &retrySendParams); err != nil {
			t.Errorf("decode retry sessions.send params: %v", err)
			return
		}
		if retrySendParams["key"] != "session-1" {
			t.Errorf("retry sessions.send key = %q, want same session session-1", retrySendParams["key"])
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: retrySendReq.ID, OK: true}); err != nil {
			t.Errorf("accept retry sessions.send: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type:  "event",
			Event: "agent",
			Payload: mustJSON(map[string]interface{}{
				"stream":     "assistant",
				"sessionKey": "session-1",
				"data":       map[string]string{"delta": "hello"},
			}),
		}); err != nil {
			t.Errorf("write assistant event: %v", err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type:  "event",
			Event: "agent",
			Payload: mustJSON(map[string]interface{}{
				"stream":     "lifecycle",
				"sessionKey": "session-1",
				"data":       map[string]string{"phase": "end"},
			}),
		}); err != nil {
			t.Errorf("write lifecycle end: %v", err)
			return
		}

		<-testDone
	}))
	defer srv.Close()
	defer close(testDone)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.CloseNow()

	gs := &gatewaySession{
		sessionKey: "session-1",
		conn:       conn,
		pending:    make(map[string]chan gwFrame),
	}
	go gs.readLoop(ctx)

	reply, err := gs.SendMessage(ctx, "continue the task", nil, nil)
	if err != nil {
		t.Fatalf("SendMessage returned error: %v", err)
	}
	if reply != "hello" {
		t.Fatalf("SendMessage reply = %q, want hello", reply)
	}
	if got := gs.getSessionKey(); got != "session-1" {
		t.Fatalf("session key = %q, want unchanged session-1", got)
	}
	if got := loadBridgeSession(); got != "" {
		t.Fatalf("persisted session key = %q, want empty (no rotation)", got)
	}
}

func TestGatewaySessionRotatesAfterLockConflictRetriesExhausted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldDelays := sessionLockConflictRetryDelays
	sessionLockConflictRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { sessionLockConflictRetryDelays = oldDelays })

	const sessionErr = "error: session file changed while embedded prompt lock was released: /home/elasticclaw/.openclaw/agents/main/sessions/4fb88b9b-8233-4f78-819f-516fbe605e32.jsonl"
	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()

		readRequest := func(wantMethod string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s request: %v", wantMethod, err)
				return gwFrame{}, false
			}
			if req.Method != wantMethod {
				t.Errorf("request method = %q, want %q", req.Method, wantMethod)
				return gwFrame{}, false
			}
			return req, true
		}

		// Every same-session attempt (initial + all retries) is rejected.
		for i := 0; i <= len(sessionLockConflictRetryDelays); i++ {
			sendReq, ok := readRequest("sessions.send")
			if !ok {
				return
			}
			var sendParams map[string]string
			if err := json.Unmarshal(sendReq.Params, &sendParams); err != nil {
				t.Errorf("decode sessions.send params: %v", err)
				return
			}
			if sendParams["key"] != "session-1" {
				t.Errorf("sessions.send attempt %d key = %q, want session-1", i+1, sendParams["key"])
				return
			}
			if err := wsjson.Write(r.Context(), conn, gwFrame{
				Type: "res",
				ID:   sendReq.ID,
				OK:   false,
				Error: &gwError{
					Code:    "session_file_lock_conflict",
					Message: sessionErr,
				},
			}); err != nil {
				t.Errorf("reject sessions.send attempt %d: %v", i+1, err)
				return
			}
		}

		// Retries exhausted: bridge rotates to a fresh session as a last resort.
		abortReq, ok := readRequest("sessions.abort")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abortReq.ID, OK: true}); err != nil {
			t.Errorf("abort session before rotation: %v", err)
			return
		}

		createReq, ok := readRequest("sessions.create")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type:    "res",
			ID:      createReq.ID,
			OK:      true,
			Payload: mustJSON(map[string]string{"key": "session-2"}),
		}); err != nil {
			t.Errorf("create replacement session: %v", err)
			return
		}

		subscribeReq, ok := readRequest("sessions.subscribe")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: subscribeReq.ID, OK: true}); err != nil {
			t.Errorf("subscribe replacement session: %v", err)
			return
		}

		<-testDone
	}))
	defer srv.Close()
	defer close(testDone)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.CloseNow()

	gs := &gatewaySession{
		sessionKey: "session-1",
		conn:       conn,
		pending:    make(map[string]chan gwFrame),
	}
	go gs.readLoop(ctx)

	_, err = gs.SendMessage(ctx, "continue the task", nil, nil)
	if err == nil {
		t.Fatal("SendMessage returned nil error")
	}
	if !strings.Contains(err.Error(), sessionErr) || !strings.Contains(err.Error(), "session reset") {
		t.Fatalf("SendMessage error = %q, want session error and recovery notice", err)
	}
	if !isSessionRotatedError(err) {
		t.Fatalf("SendMessage error must be recognized as session rotation, got: %v", err)
	}
	if got := gs.getSessionKey(); got != "session-2" {
		t.Fatalf("session key = %q, want session-2", got)
	}
	if got := loadBridgeSession(); got != "session-2" {
		t.Fatalf("persisted session key = %q, want session-2", got)
	}
}

func TestGatewaySessionResetsAfterAcceptedTurnDeadline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	testDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()

		readRequest := func(wantMethod string) (gwFrame, bool) {
			var req gwFrame
			if err := wsjson.Read(r.Context(), conn, &req); err != nil {
				t.Errorf("read %s request: %v", wantMethod, err)
				return gwFrame{}, false
			}
			if req.Method != wantMethod {
				t.Errorf("request method = %q, want %q", req.Method, wantMethod)
				return gwFrame{}, false
			}
			return req, true
		}

		sendReq, ok := readRequest("sessions.send")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: sendReq.ID, OK: true}); err != nil {
			t.Errorf("accept sessions.send: %v", err)
			return
		}
		// Do not emit a lifecycle result. The accepted turn must time out in
		// sendMessageOnce before recovery aborts and rotates the session.

		abortReq, ok := readRequest("sessions.abort")
		if !ok {
			return
		}
		var abortParams map[string]string
		if err := json.Unmarshal(abortReq.Params, &abortParams); err != nil {
			t.Errorf("decode sessions.abort params: %v", err)
			return
		}
		if abortParams["key"] != "session-1" {
			t.Errorf("sessions.abort key = %q, want session-1", abortParams["key"])
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abortReq.ID, OK: true}); err != nil {
			t.Errorf("abort timed-out session: %v", err)
			return
		}

		createReq, ok := readRequest("sessions.create")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{
			Type:    "res",
			ID:      createReq.ID,
			OK:      true,
			Payload: mustJSON(map[string]string{"key": "session-2"}),
		}); err != nil {
			t.Errorf("create replacement session: %v", err)
			return
		}

		subscribeReq, ok := readRequest("sessions.subscribe")
		if !ok {
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: subscribeReq.ID, OK: true}); err != nil {
			t.Errorf("subscribe replacement session: %v", err)
			return
		}

		<-testDone
	}))
	defer srv.Close()
	defer close(testDone)

	readCtx, stopRead := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopRead()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.Dial(readCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.CloseNow()

	gs := &gatewaySession{
		sessionKey: "session-1",
		conn:       conn,
		pending:    make(map[string]chan gwFrame),
	}
	go gs.readLoop(readCtx)

	sendCtx, cancelSend := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelSend()
	_, err = gs.SendMessage(sendCtx, "continue the task", nil, nil)
	if err == nil {
		t.Fatal("SendMessage returned nil error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendMessage error = %v, want wrapped context deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "session reset") {
		t.Fatalf("SendMessage error = %q, want recovery notice", err)
	}
	if got := gs.getSessionKey(); got != "session-2" {
		t.Fatalf("session key = %q, want session-2", got)
	}
	if got := loadBridgeSession(); got != "session-2" {
		t.Fatalf("persisted session key = %q, want session-2", got)
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

	reconnected, replacedSession, err := gs.reconnectGateway(context.Background(), stale)
	if err != nil {
		t.Fatalf("reconnectGateway returned error: %v", err)
	}
	if reconnected {
		t.Fatal("reconnectGateway reported reconnect for stale expected connection")
	}
	if replacedSession {
		t.Fatal("reconnectGateway reported replacement for stale expected connection")
	}
	if got := gs.currentConn(); got != current {
		t.Fatalf("current connection changed on stale reconnect: got %p, want %p", got, current)
	}
}

func TestReconnectGatewayWithTimeoutWhenChallengeNeverArrives(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		// Simulate a gateway whose event loop is wedged after the upgrade: it
		// accepts WebSocket connections but never emits connect.challenge.
		<-r.Context().Done()
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	current := &websocket.Conn{}
	gs := &gatewaySession{
		client:  &gatewayClient{addr: strings.TrimPrefix(srv.URL, "http://")},
		conn:    current,
		pending: make(map[string]chan gwFrame),
	}
	started := time.Now()
	_, _, err = gs.reconnectGatewayWithTimeout(context.Background(), current, 100*time.Millisecond)
	if err == nil {
		t.Fatal("reconnectGatewayWithTimeout error = nil, want timeout")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("reconnect returned after %s, want roughly the timeout", elapsed)
	}
}

// TestReconnectGatewayReportsReplacementWhenSessionMissing covers the loss
// path: the gateway forgot the session, so reconnectGateway must fall back
// to sessions.create and report replacedSession=true so callers can announce
// the loss to the hub. Without this signal, readLoop and SendMessage would
// silently keep going on an empty transcript.
func TestReconnectGatewayReportsReplacementWhenSessionMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
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
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: connectReq.ID, OK: true}); err != nil {
			t.Errorf("write connect response: %v", err)
			return
		}

		var subReq gwFrame
		if err := wsjson.Read(ctx, conn, &subReq); err != nil {
			t.Errorf("read subscribe request: %v", err)
			return
		}
		if subReq.Method != "sessions.subscribe" {
			t.Errorf("request method = %q, want sessions.subscribe", subReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: subReq.ID, OK: false, Error: &gwError{Code: "not_found", Message: "session not found"}}); err != nil {
			t.Errorf("write subscribe error: %v", err)
			return
		}

		var createReq gwFrame
		if err := wsjson.Read(ctx, conn, &createReq); err != nil {
			t.Errorf("read create request: %v", err)
			return
		}
		if createReq.Method != "sessions.create" {
			t.Errorf("request method = %q, want sessions.create", createReq.Method)
			return
		}
		createPayload, _ := json.Marshal(map[string]string{"key": "session-2"})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: createReq.ID, OK: true, Payload: createPayload}); err != nil {
			t.Errorf("write create response: %v", err)
			return
		}

		var resubReq gwFrame
		if err := wsjson.Read(ctx, conn, &resubReq); err != nil {
			t.Errorf("read resubscribe request: %v", err)
			return
		}
		if resubReq.Method != "sessions.subscribe" {
			t.Errorf("request method = %q, want sessions.subscribe", resubReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: resubReq.ID, OK: true}); err != nil {
			t.Errorf("write resubscribe response: %v", err)
			return
		}
		<-ctx.Done()
	}))
	defer srv.Close()

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
	gs := &gatewaySession{
		client:     client,
		sessionKey: "session-1",
		pending:    make(map[string]chan gwFrame),
	}

	reconnected, replacedSession, err := gs.reconnectGateway(ctx, nil)
	if err != nil {
		t.Fatalf("reconnectGateway returned error: %v", err)
	}
	if !reconnected {
		t.Fatal("reconnectGateway reported reconnected=false, want true")
	}
	if !replacedSession {
		t.Fatal("reconnectGateway reported replacedSession=false, want true after a missing-session error")
	}
	if got := gs.getSessionKey(); got != "session-2" {
		t.Fatalf("session key = %q, want session-2 (fresh session)", got)
	}
}

// TestReadLoopAnnouncesSessionReplacementOnBackgroundReconnect covers the
// readLoop call site specifically (as opposed to reconnectGateway directly):
// a background reconnect after a dropped read must still fire
// onSessionReplaced when the gateway had forgotten the session, or the hub
// never learns the transcript was lost.
func TestReadLoopAnnouncesSessionReplacementOnBackgroundReconnect(t *testing.T) {
	var connections atomic.Int32
	firstHandshakeDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		connID := connections.Add(1)
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
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: connectReq.ID, OK: true}); err != nil {
			t.Errorf("write connect response: %v", err)
			return
		}

		if connID == 1 {
			// First connection: let readLoop start reading, then drop it so
			// readLoop's background reconnect kicks in.
			close(firstHandshakeDone)
			conn.CloseNow()
			return
		}

		var subReq gwFrame
		if err := wsjson.Read(ctx, conn, &subReq); err != nil {
			t.Errorf("read subscribe request: %v", err)
			return
		}
		if subReq.Method != "sessions.subscribe" {
			t.Errorf("request method = %q, want sessions.subscribe", subReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: subReq.ID, OK: false, Error: &gwError{Code: "not_found", Message: "session not found"}}); err != nil {
			t.Errorf("write subscribe error: %v", err)
			return
		}

		var createReq gwFrame
		if err := wsjson.Read(ctx, conn, &createReq); err != nil {
			t.Errorf("read create request: %v", err)
			return
		}
		if createReq.Method != "sessions.create" {
			t.Errorf("request method = %q, want sessions.create", createReq.Method)
			return
		}
		createPayload, _ := json.Marshal(map[string]string{"key": "session-2"})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: createReq.ID, OK: true, Payload: createPayload}); err != nil {
			t.Errorf("write create response: %v", err)
			return
		}

		var resubReq gwFrame
		if err := wsjson.Read(ctx, conn, &resubReq); err != nil {
			t.Errorf("read resubscribe request: %v", err)
			return
		}
		if resubReq.Method != "sessions.subscribe" {
			t.Errorf("request method = %q, want sessions.subscribe", resubReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: resubReq.ID, OK: true}); err != nil {
			t.Errorf("write resubscribe response: %v", err)
			return
		}
		<-ctx.Done()
	}))
	defer srv.Close()

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

	gs := &gatewaySession{
		client:     client,
		sessionKey: "session-1",
		conn:       conn,
		pending:    make(map[string]chan gwFrame),
	}
	replaced := make(chan struct{}, 1)
	gs.setOnSessionReplaced(func() { replaced <- struct{}{} })
	go gs.readLoop(ctx)

	select {
	case <-replaced:
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop's background reconnect did not announce a session replacement")
	}
	if got := gs.getSessionKey(); got != "session-2" {
		t.Fatalf("session key = %q, want session-2 (fresh session)", got)
	}
}

func TestNewGatewaySessionTimesOutWhenChallengeNeverArrives(t *testing.T) {
	connectionClosed := make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		// Accept the upgrade but never emit connect.challenge, as a frozen
		// gateway can after validating the WebSocket connection.
		//
		// Once Accept hijacks the connection, net/http no longer owns it, so
		// r.Context() never observes the peer closing the socket. Block on a
		// read instead: it only returns once the client tears down its side.
		_, _, _ = conn.Read(context.Background())
		close(connectionClosed)
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	const attemptTimeout = 100 * time.Millisecond
	started := time.Now()
	_, err = newGatewaySession(context.Background(), &gatewayClient{
		addr: strings.TrimPrefix(srv.URL, "http://"),
	}, attemptTimeout)
	if err == nil {
		t.Fatal("newGatewaySession error = nil, want timeout")
	}
	if elapsed := time.Since(started); elapsed < attemptTimeout/2 || elapsed > time.Second {
		t.Fatalf("newGatewaySession returned after %s, want roughly %s", elapsed, attemptTimeout)
	}
	select {
	case <-connectionClosed:
		// The attempt closed its connection; no read loop remains against it.
	case <-time.After(time.Second):
		t.Fatal("frozen gateway connection remained open after failed startup attempt")
	}
}

func TestNewGatewaySessionReadLoopOutlivesAttemptTimeout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	requestReceived := make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("local listener unavailable: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		challengePayload, _ := json.Marshal(map[string]string{"nonce": "nonce"})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "event", Event: "connect.challenge", Payload: challengePayload}); err != nil {
			t.Errorf("write challenge: %v", err)
			return
		}
		for {
			var req gwFrame
			if err := wsjson.Read(ctx, conn, &req); err != nil {
				return
			}
			switch req.Method {
			case "connect", "sessions.subscribe", "sessions.get":
				if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: req.ID, OK: true}); err != nil {
					t.Errorf("write %s response: %v", req.Method, err)
					return
				}
				if req.Method == "sessions.get" {
					close(requestReceived)
				}
			case "sessions.create":
				payload, _ := json.Marshal(map[string]string{"key": "session-1"})
				if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: req.ID, OK: true, Payload: payload}); err != nil {
					t.Errorf("write sessions.create response: %v", err)
					return
				}
			case "sessions.describe":
				// setReady() kicks off an async context-usage poll; answer it so
				// it doesn't trip the "unexpected request method" default below.
				if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: req.ID, OK: true}); err != nil {
					t.Errorf("write sessions.describe response: %v", err)
					return
				}
			default:
				t.Errorf("unexpected request method %q", req.Method)
				return
			}
		}
	}))
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	const attemptTimeout = 100 * time.Millisecond
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &gatewayClient{
		addr:   strings.TrimPrefix(srv.URL, "http://"),
		token:  "token",
		device: &deviceIdentity{DeviceID: "device-1", PublicKeyPem: testPEM("PUBLIC KEY"), PrivateKeyPem: testPEM("PRIVATE KEY")},
	}
	gs, err := newGatewaySession(parentCtx, client, attemptTimeout)
	if err != nil {
		t.Fatalf("newGatewaySession: %v", err)
	}
	defer gs.currentConn().CloseNow()

	time.Sleep(2 * attemptTimeout)
	requestCtx, requestCancel := context.WithTimeout(parentCtx, time.Second)
	defer requestCancel()
	if _, err := gs.sendReq(requestCtx, "sessions.get", map[string]string{"key": "session-1"}); err != nil {
		t.Fatalf("request after attempt timeout: %v", err)
	}
	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("read loop did not dispatch response after attempt timeout")
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
	replaced := make(chan struct{}, 1)
	gs.setOnSessionReplaced(func() { replaced <- struct{}{} })
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
	select {
	case <-replaced:
		t.Fatal("successful re-subscribe announced a session replacement")
	default:
	}
}

// TestSendMessageAnnouncesSessionReplacementOnReconnect covers the
// SendMessage call site specifically: a retryable send-write failure forces
// a reconnect, and when that reconnect finds the session gone it must still
// announce the replacement even though the retried send itself succeeds.
func TestSendMessageAnnouncesSessionReplacementOnReconnect(t *testing.T) {
	var connections atomic.Int32
	firstHandshakeDone := make(chan struct{})
	resubscribeDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		connID := connections.Add(1)
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
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: connectReq.ID, OK: true}); err != nil {
			t.Errorf("write connect response: %v", err)
			return
		}

		if connID == 1 {
			// First connection: handshake only, then get closed by the test
			// so the first sessions.send write fails.
			close(firstHandshakeDone)
			<-ctx.Done()
			return
		}

		var subReq gwFrame
		if err := wsjson.Read(ctx, conn, &subReq); err != nil {
			t.Errorf("read subscribe request: %v", err)
			return
		}
		if subReq.Method != "sessions.subscribe" {
			t.Errorf("request method = %q, want sessions.subscribe", subReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: subReq.ID, OK: false, Error: &gwError{Code: "not_found", Message: "session not found"}}); err != nil {
			t.Errorf("write subscribe error: %v", err)
			return
		}

		var createReq gwFrame
		if err := wsjson.Read(ctx, conn, &createReq); err != nil {
			t.Errorf("read create request: %v", err)
			return
		}
		if createReq.Method != "sessions.create" {
			t.Errorf("request method = %q, want sessions.create", createReq.Method)
			return
		}
		createPayload, _ := json.Marshal(map[string]string{"key": "session-2"})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: createReq.ID, OK: true, Payload: createPayload}); err != nil {
			t.Errorf("write create response: %v", err)
			return
		}

		var resubReq gwFrame
		if err := wsjson.Read(ctx, conn, &resubReq); err != nil {
			t.Errorf("read resubscribe request: %v", err)
			return
		}
		if resubReq.Method != "sessions.subscribe" {
			t.Errorf("request method = %q, want sessions.subscribe", resubReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: resubReq.ID, OK: true}); err != nil {
			t.Errorf("write resubscribe response: %v", err)
			return
		}
		close(resubscribeDone)

		// Creating a fresh session makes gatewaySession.setReady() kick off an
		// unrelated background sessions.describe (context-usage refresh) on
		// the same connection; it can arrive before or after the retried
		// sessions.send, so answer whichever request shows up first.
		var sendReq gwFrame
		for {
			var req gwFrame
			if err := wsjson.Read(ctx, conn, &req); err != nil {
				t.Errorf("read request after resubscribe: %v", err)
				return
			}
			if req.Method == "sessions.describe" {
				describePayload, _ := json.Marshal(map[string]interface{}{"session": map[string]interface{}{}})
				if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: req.ID, OK: true, Payload: describePayload}); err != nil {
					t.Errorf("write sessions.describe response: %v", err)
					return
				}
				continue
			}
			sendReq = req
			break
		}
		if sendReq.Method != "sessions.send" {
			t.Errorf("retry request method = %q, want sessions.send", sendReq.Method)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: sendReq.ID, OK: true}); err != nil {
			t.Errorf("write sessions.send response: %v", err)
			return
		}
		assistantPayload, _ := json.Marshal(map[string]interface{}{
			"stream":     "assistant",
			"sessionKey": "session-2",
			"data":       map[string]string{"delta": "hello"},
		})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "event", Event: "agent", Payload: assistantPayload}); err != nil {
			t.Errorf("write assistant event: %v", err)
			return
		}
		endPayload, _ := json.Marshal(map[string]interface{}{
			"stream":     "lifecycle",
			"sessionKey": "session-2",
			"data":       map[string]string{"phase": "end"},
		})
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "event", Event: "agent", Payload: endPayload}); err != nil {
			t.Errorf("write lifecycle event: %v", err)
			return
		}
		// Drain and answer any further requests (e.g. a still-pending
		// context-usage sessions.describe) instead of leaving them unread,
		// which would otherwise surface as a goroutine panic once the test
		// itself has already returned.
		for {
			var req gwFrame
			if err := wsjson.Read(ctx, conn, &req); err != nil {
				return
			}
			if req.Method == "sessions.describe" {
				describePayload, _ := json.Marshal(map[string]interface{}{"session": map[string]interface{}{}})
				_ = wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: req.ID, OK: true, Payload: describePayload})
			}
		}
	}))
	defer srv.Close()

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
	// Close from the client side so the first sessions.send write fails
	// immediately, without relying on readLoop to notice the drop first.
	conn.CloseNow()

	gs := &gatewaySession{
		client:     client,
		sessionKey: "session-1",
		conn:       conn,
		pending:    make(map[string]chan gwFrame),
	}
	replaced := make(chan struct{}, 1)
	gs.setOnSessionReplaced(func() { replaced <- struct{}{} })

	// reconnectGateway (subscribe/create/resubscribe) reads its own responses
	// synchronously. Only the retried sessions.send needs readLoop. Wait for
	// the connection swap, rather than the re-subscribe response: the latter
	// can arrive before reconnectGateway installs the new connection.
	go func() {
		select {
		case <-resubscribeDone:
		case <-ctx.Done():
			return
		}
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			if gs.currentConn() != conn {
				gs.readLoop(ctx)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	type sendResult struct {
		reply string
		err   error
	}
	resultCh := make(chan sendResult, 1)
	go func() {
		reply, err := gs.SendMessage(ctx, "hi", func(string) {}, nil)
		resultCh <- sendResult{reply, err}
	}()

	select {
	case <-replaced:
	case <-time.After(2 * time.Second):
		t.Fatal("SendMessage's reconnect did not announce a session replacement")
	}

	var result sendResult
	select {
	case result = <-resultCh:
	case <-time.After(3 * time.Second):
		t.Fatal("SendMessage did not return after reconnect")
	}
	if result.err != nil {
		t.Fatalf("SendMessage returned error: %v", result.err)
	}
	if result.reply != "hello" {
		t.Fatalf("reply = %q, want hello", result.reply)
	}
	if got := gs.getSessionKey(); got != "session-2" {
		t.Fatalf("session key = %q, want session-2 (fresh session)", got)
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

func TestMsgQueueReplyNeverExpires(t *testing.T) {
	q := &msgQueue{}
	q.pushReply("completed reply")
	// Backdate well beyond the TTL — replies must still survive.
	q.msgs[0].queuedAt = time.Now().Add(-2 * msgQueueTTL)

	out := q.drain()
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	if out[0].kind != queuedReply || out[0].content != "completed reply" {
		t.Fatalf("expected surviving reply, got kind=%d content=%q", out[0].kind, out[0].content)
	}
}

func TestMsgQueueExpiredInputBecomesNotice(t *testing.T) {
	q := &msgQueue{}
	q.pushInput("stale user input")
	q.msgs[0].queuedAt = time.Now().Add(-2 * msgQueueTTL)

	out := q.drain()
	if len(out) != 1 {
		t.Fatalf("expected 1 entry (notice only), got %d", len(out))
	}
	notice := out[0]
	if notice.kind != queuedNotice {
		t.Fatalf("expected queuedNotice, got kind=%d", notice.kind)
	}
	if notice.content == "stale user input" {
		t.Fatalf("raw input must not be returned")
	}
	if !strings.Contains(notice.content, "dropped") {
		t.Fatalf("notice should mention the drop: %q", notice.content)
	}
	if !strings.Contains(notice.content, "stale user input") {
		t.Fatalf("notice should include the preview: %q", notice.content)
	}
}

func TestMsgQueueOverflowEvictsOldestInputAndSurfacesNotice(t *testing.T) {
	q := &msgQueue{}
	for i := 0; i < msgQueueMax+5; i++ {
		q.pushInput(fmt.Sprintf("input-%d", i))
	}
	if len(q.msgs) != msgQueueMax {
		t.Fatalf("expected queue capped at %d, got %d", msgQueueMax, len(q.msgs))
	}
	// The 5 oldest inputs (input-0..input-4) must have been evicted.
	if q.msgs[0].content != "input-5" {
		t.Fatalf("expected oldest surviving input to be input-5, got %q", q.msgs[0].content)
	}

	out := q.drain()
	if len(out) == 0 || out[0].kind != queuedNotice {
		t.Fatalf("expected a leading notice about evictions")
	}
	if !strings.Contains(out[0].content, "5 queued message") {
		t.Fatalf("notice should count the 5 evictions: %q", out[0].content)
	}

	// Replies must never be evicted, even when the queue is full.
	q2 := &msgQueue{}
	for i := 0; i < msgQueueMax; i++ {
		q2.pushReply(fmt.Sprintf("reply-%d", i))
	}
	q2.pushReply("reply-overflow")
	if len(q2.msgs) != msgQueueMax+1 {
		t.Fatalf("expected replies to grow beyond cap, got %d", len(q2.msgs))
	}
	if q2.dropped != 0 {
		t.Fatalf("expected no drops when queue is full of replies, got %d", q2.dropped)
	}
}

func TestReplayQueuedDeliversReplyWithoutRerun(t *testing.T) {
	q := &msgQueue{}
	q.pushReply("finished reply")

	var delivered []string
	deliver := func(role, content string) error {
		if role != "claw" {
			t.Fatalf("expected role claw, got %q", role)
		}
		delivered = append(delivered, content)
		return nil
	}
	runTurn := func(content string) {
		t.Fatalf("runTurn must not be called for a completed reply (content=%q)", content)
	}

	replayQueued(q, deliver, func(hubMsg) error { return nil }, runTurn)

	if len(delivered) != 1 || delivered[0] != "finished reply" {
		t.Fatalf("expected reply delivered exactly once, got %v", delivered)
	}
}

func TestReplayQueuedRequeuesReplyOnDeliverFailure(t *testing.T) {
	q := &msgQueue{}
	q.pushReply("undelivered reply")
	originalAt := q.msgs[0].queuedAt

	deliver := func(role, content string) error {
		return errString("hub write failed")
	}
	runTurn := func(content string) {
		t.Fatalf("runTurn must not be called")
	}

	replayQueued(q, deliver, func(hubMsg) error { return nil }, runTurn)

	if len(q.msgs) != 1 {
		t.Fatalf("expected reply re-queued, got %d entries", len(q.msgs))
	}
	if q.msgs[0].kind != queuedReply || q.msgs[0].content != "undelivered reply" {
		t.Fatalf("expected the reply back in the queue, got %+v", q.msgs[0])
	}
	if !q.msgs[0].queuedAt.Equal(originalAt) {
		t.Fatalf("expected original queuedAt preserved: got %v want %v", q.msgs[0].queuedAt, originalAt)
	}
}

func TestReplayQueuedRunsTurnForInputs(t *testing.T) {
	q := &msgQueue{}
	q.pushInput("do the work")

	var deliverCalled bool
	deliver := func(role, content string) error {
		deliverCalled = true
		return nil
	}
	var ran []string
	runTurn := func(content string) {
		ran = append(ran, content)
	}

	replayQueued(q, deliver, func(hubMsg) error { return nil }, runTurn)

	if deliverCalled {
		t.Fatalf("deliver must not be called for a queued input")
	}
	if len(ran) != 1 || ran[0] != "do the work" {
		t.Fatalf("expected runTurn invoked with the input, got %v", ran)
	}
}

// countActiveSubagents feeds the heartbeat's subagents_active fields, which
// suppress the hub's "Agent stalled" notification — so it must count only
// spawned subagent sessions with a run actually in flight, and any payload it
// cannot understand must be an error (fields omitted from the heartbeat),
// never a fabricated zero.
func TestCountActiveSubagents(t *testing.T) {
	payload := []byte(`{"sessions":[
		{"key":"agent:main:subagent:abc","hasActiveRun":true},
		{"key":"agent:main:subagent:def","hasActiveRun":false},
		{"key":"agent:main:subagent:ghi"},
		{"key":"agent:main","hasActiveRun":true},
		{"key":"other:subagent:jkl","hasActiveRun":true,"activeRunIds":[]}
	]}`)
	count, err := countActiveSubagents(payload)
	if err != nil {
		t.Fatalf("countActiveSubagents: %v", err)
	}
	// abc and jkl only: the main session's own run is not subagent work, a
	// missing hasActiveRun is "no information" rather than active, and an
	// empty activeRunIds must not override an explicit hasActiveRun=true.
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	// An empty index is a successful "nothing running".
	count, err = countActiveSubagents([]byte(`{"sessions":[]}`))
	if err != nil || count != 0 {
		t.Fatalf("empty index: count=%d err=%v, want 0, nil", count, err)
	}

	// No sessions index at all: this shape carries no information, so it must
	// surface as an error, not as zero.
	if _, err := countActiveSubagents([]byte(`{}`)); err == nil {
		t.Fatal("missing sessions index did not error")
	}
	if _, err := countActiveSubagents([]byte(`{"sessions":null}`)); err == nil {
		t.Fatal("null sessions index did not error")
	}
	if _, err := countActiveSubagents([]byte(`not json`)); err == nil {
		t.Fatal("malformed payload did not error")
	}

	// A non-empty index whose rows do not carry the key field under the name
	// we expect (cross-version shape drift) must be an error: matching zero
	// rows out of an index we could not read would be a fabricated "nothing
	// running", and the hub treats that as positive evidence.
	if _, err := countActiveSubagents([]byte(`{"sessions":[
		{"sessionKey":"agent:main:subagent:abc","hasActiveRun":true},
		{"sessionKey":"agent:main","hasActiveRun":true}
	]}`)); err == nil {
		t.Fatal("keyless rows did not error")
	}
}

// subagentHeartbeatFields decides whether the heartbeat carries the
// subagents_active fields at all. The hub reads an explicit false as positive
// "nothing running" evidence and clears its suppression state, so the fields
// must appear only when the sessions.list poll actually succeeded — never on
// a poll error or a not-ready gateway.
func TestSubagentHeartbeatFields(t *testing.T) {
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevOut)

	// Not-ready gateway: no fields, and the poll must not even run.
	failLogged := false
	fields, suffix := subagentHeartbeatFields(false, func() (int, error) {
		t.Fatal("poll ran while the gateway session was not ready")
		return 0, nil
	}, &failLogged)
	if fields != nil || suffix != "" || failLogged {
		t.Fatalf("not-ready gateway: fields=%v suffix=%q failLogged=%v, want none", fields, suffix, failLogged)
	}

	// Successful poll: both fields, matching values.
	fields, suffix = subagentHeartbeatFields(true, func() (int, error) { return 2, nil }, &failLogged)
	if fields["subagents_active"] != true || fields["subagent_active_count"] != 2 || len(fields) != 2 {
		t.Fatalf("successful poll fields = %v, want subagents_active=true subagent_active_count=2", fields)
	}
	if !strings.Contains(suffix, "subagents_active=true") {
		t.Fatalf("log suffix %q does not surface the activity", suffix)
	}
	fields, _ = subagentHeartbeatFields(true, func() (int, error) { return 0, nil }, &failLogged)
	if fields["subagents_active"] != false || fields["subagent_active_count"] != 0 {
		t.Fatalf("zero-count poll fields = %v, want explicit false/0", fields)
	}

	// Failing poll (e.g. older gateway without sessions.list): both fields
	// omitted, and the failure is logged exactly once across repeats.
	pollErr := func() (int, error) { return 0, errors.New("method not found") }
	for i := 0; i < 3; i++ {
		if fields, suffix = subagentHeartbeatFields(true, pollErr, &failLogged); fields != nil || suffix != "" {
			t.Fatalf("failing poll %d produced fields=%v suffix=%q, want none", i, fields, suffix)
		}
	}
	if !failLogged {
		t.Fatal("failing poll did not arm the logged-once flag")
	}
	if got := strings.Count(logBuf.String(), "subagent activity poll failed"); got != 1 {
		t.Fatalf("failure logged %d times across 3 failing polls, want 1", got)
	}

	// A recovery re-arms the log for the next outage.
	if _, _ = subagentHeartbeatFields(true, func() (int, error) { return 1, nil }, &failLogged); failLogged {
		t.Fatal("successful poll did not re-arm the failure log")
	}
}

func TestAddHeartbeatSessionKeysKeepsSnapshotAndLiveKeysSeparate(t *testing.T) {
	payload := map[string]interface{}{}
	addHeartbeatSessionKeys(payload, "stale-snapshot", "live-gateway-session")
	if got := payload["session_key"]; got != "stale-snapshot" {
		t.Fatalf("session_key = %v, want usage snapshot", got)
	}
	if got := payload["gateway_session_key"]; got != "live-gateway-session" {
		t.Fatalf("gateway_session_key = %v, want live gateway key", got)
	}

	payload = map[string]interface{}{}
	addHeartbeatSessionKeys(payload, "old-snapshot", "")
	if _, ok := payload["gateway_session_key"]; ok {
		t.Fatalf("gateway_session_key = %v, want omitted without a live key", payload["gateway_session_key"])
	}
}

// A lifecycle error that is not a lock conflict skips the mismatch early
// return, so it reaches abortSession while a concurrent readLoop rotation may
// already have replaced the current key. The abort must still target the key
// the turn actually ran on: aborting the new session leaves the orphaned turn
// running and writing the old session file, which is the condition that
// produces the next lock conflict.
func TestGatewaySessionAbortsTurnKeyAfterConcurrentRotation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const lifecycleErr = "error: provider rejected the request schema or tool payload"
	var gs *gatewaySession
	oldHook := sessionMismatchCheckHook
	t.Cleanup(func() { sessionMismatchCheckHook = oldHook })
	sessionMismatchCheckHook = func() {
		gs.sessionMu.Lock()
		gs.sessionKey = "session-2"
		gs.sessionMu.Unlock()
	}
	abortKey := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		var send gwFrame
		if err := wsjson.Read(r.Context(), conn, &send); err != nil || send.Method != "sessions.send" {
			t.Errorf("read sessions.send: method=%q err=%v", send.Method, err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: send.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]interface{}{"stream": "lifecycle", "sessionKey": "session-1", "data": map[string]string{"phase": "error", "error": lifecycleErr}})}); err != nil {
			t.Error(err)
			return
		}
		var abort gwFrame
		if err := wsjson.Read(r.Context(), conn, &abort); err != nil || abort.Method != "sessions.abort" {
			t.Errorf("read sessions.abort: method=%q err=%v", abort.Method, err)
			return
		}
		var params struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(abort.Params, &params)
		abortKey <- params.Key
		if err := wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: abort.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		// Answer the rotation's sessions.create so SendMessage can finish.
		var create gwFrame
		if err := wsjson.Read(r.Context(), conn, &create); err == nil && create.Method == "sessions.create" {
			_ = wsjson.Write(r.Context(), conn, gwFrame{Type: "res", ID: create.ID, OK: true, Payload: mustJSON(map[string]string{"key": "session-3"})})
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	gs = &gatewaySession{sessionKey: "session-1", conn: conn, pending: make(map[string]chan gwFrame)}
	go gs.readLoop(ctx)
	go func() { _, _ = gs.SendMessage(ctx, "continue the task", nil, nil) }()
	select {
	case got := <-abortKey:
		if got != "session-1" {
			t.Fatalf("sessions.abort key = %q, want session-1 (the turn's key, not the rotated one)", got)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no sessions.abort observed")
	}
}
