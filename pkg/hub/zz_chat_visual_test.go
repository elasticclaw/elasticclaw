//go:build !production

package hub

// Throwaway local visual-verification harness for the agent chat UI.
//
// It is NOT a test: it boots a real hub server on a fixed port, seeds a set of
// claws whose conversations exercise every visual kind the web timeline can
// render, opens fake bridges so the claws report as connected, and then blocks
// forever. Gated on MANUAL_VERIFY so `go test ./...` never picks it up.
//
// See hack/chat-visual/README.md for the end-to-end recipe.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const (
	visualHubToken  = "test-token"
	visualClawToken = "claw-token"
	visualTenant    = "test-tenant-id"
)

func TestChatVisualHarness(t *testing.T) {
	if os.Getenv("MANUAL_VERIFY") == "" {
		t.Skip("set MANUAL_VERIFY=1 to run the chat visual harness")
	}

	port := os.Getenv("HUB_PORT")
	if port == "" {
		port = "8085"
	}
	addr := "localhost:" + port

	cfg := &types.HubConfig{
		Token:      visualHubToken,
		ClawToken:  visualClawToken,
		URL:        "http://" + addr,
		PublicURL:  "http://" + addr,
		UIPassword: "admin",
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")

	claws := seedVisualClaws(t, db)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	go func() {
		if err := http.Serve(ln, corsMiddleware(s.Handler())); err != nil {
			log.Printf("[chat-visual] serve stopped: %v", err)
		}
	}()

	waitForPort(t, addr)

	// Fake bridges: handleClaws forces "offline" for any claw without a live
	// WS session, so connected/idle states only exist while these are held.
	for _, c := range claws {
		if c.bridge {
			startFakeBridge(t, addr, c.id, c.name)
		}
	}

	fmt.Printf("\n[chat-visual] hub listening on http://%s\n", addr)
	fmt.Printf("[chat-visual] token (sessionStorage ec_hub_token): %s\n", visualHubToken)
	for _, c := range claws {
		fmt.Printf("[chat-visual] claw %-42s id=%s\n", c.name, c.id)
	}
	fmt.Printf("[chat-visual] blocking forever; Ctrl-C or kill the pid to stop\n\n")

	select {}
}

// ─── seeding ─────────────────────────────────────────────────────────────────

type visualClaw struct {
	id     string
	name   string
	bridge bool
}

func seedVisualClaws(t *testing.T, db *sql.DB) []visualClaw {
	t.Helper()

	rich := visualClaw{id: "claw-visual-rich", name: "fix/token-refresh-race", bridge: true}
	idle := visualClaw{id: "claw-visual-idle", name: "chore/bump-node-deps", bridge: true}
	broken := visualClaw{id: "claw-visual-error", name: "feature/webhook-retry-backoff", bridge: false}

	insertClaw(t, db, rich, "connected", "implement", "violet", []string{"repo:elasticclaw", "factory:faster"})
	insertClaw(t, db, idle, "connected", "review", "emerald", []string{"repo:elasticclaw"})
	insertClaw(t, db, broken, "error", "implement", "rose", []string{"repo:elasticclaw", "factory:faster"})

	seedRichConversation(t, db, rich.id)
	seedIdleConversation(t, db, idle.id)
	seedErrorConversation(t, db, broken.id)

	return []visualClaw{rich, idle, broken}
}

func insertClaw(t *testing.T, db *sql.DB, c visualClaw, status, stage, color string, tags []string) {
	t.Helper()
	tagsJSON, _ := json.Marshal(tags)
	_, err := db.Exec(`INSERT INTO claws(
			id, tenant_id, name, template, provider, status, pipeline_stage, color, tags,
			bootstrap_ok, bootstrap_status, github_issue_id, issue_title,
			last_seen, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.id, visualTenant, c.name, "elasticclaw", "docker", status, stage, color, string(tagsJSON),
		1, "ready", "4821", "Token refresh races with the gateway reconnect",
		time.Now(), time.Now().Add(-3*time.Hour),
	)
	if err != nil {
		t.Fatalf("insert claw %s: %v", c.id, err)
	}
}

// msg inserts one conversation row (user / claw / hub / system / state).
func msg(t *testing.T, db *sql.DB, clawID, id, role, content, format string, at time.Time, userLogin string) {
	t.Helper()
	var login interface{}
	if userLogin != "" {
		login = userLogin
	}
	_, err := db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,user_login,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, clawID, visualTenant, role, content, format, login, at, at,
	)
	if err != nil {
		t.Fatalf("insert message %s: %v", id, err)
	}
}

// act inserts an activity row in exactly the shape storeAgentActivity writes:
// content derived by activityContent, format "activity:<json>".
func act(t *testing.T, db *sql.DB, clawID, id string, at time.Time, activity map[string]interface{}) {
	t.Helper()
	raw, err := json.Marshal(activity)
	if err != nil {
		t.Fatalf("marshal activity %s: %v", id, err)
	}
	content := activityContent(activity)
	_, err = db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,format,created_at,delivered_at) VALUES(?,?,?,?,?,?,?,?)`,
		id, clawID, visualTenant, "activity", content, "activity:"+string(raw), at, at,
	)
	if err != nil {
		t.Fatalf("insert activity %s: %v", id, err)
	}
}

// toolPair writes the start + terminal events of one tool call.
func toolPair(t *testing.T, db *sql.DB, clawID, idBase string, start time.Time, durationMs int, base map[string]interface{}, terminal map[string]interface{}) {
	t.Helper()
	callID := "call-" + idBase
	startAct := map[string]interface{}{"kind": "tool", "phase": "running", "call_id": callID}
	for k, v := range base {
		startAct[k] = v
	}
	act(t, db, clawID, idBase+"-start", start, startAct)

	endAct := map[string]interface{}{"kind": "tool", "phase": "completed", "call_id": callID, "duration_ms": durationMs}
	for k, v := range base {
		endAct[k] = v
	}
	for k, v := range terminal {
		endAct[k] = v
	}
	act(t, db, clawID, idBase+"-end", start.Add(time.Duration(durationMs)*time.Millisecond), endAct)
}

// toolRunning writes only the start event, so the step stays live in the UI.
func toolRunning(t *testing.T, db *sql.DB, clawID, idBase string, start time.Time, base map[string]interface{}) {
	t.Helper()
	a := map[string]interface{}{"kind": "tool", "phase": "running", "call_id": "call-" + idBase}
	for k, v := range base {
		a[k] = v
	}
	act(t, db, clawID, idBase+"-start", start, a)
}

func seedRichConversation(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()
	// Timeline anchored to "now" so relative clocks in the UI read sensibly.
	base := time.Now().Add(-52 * time.Minute)
	at := func(offset time.Duration) time.Time { return base.Add(offset) }
	n := 0
	id := func(prefix string) string { n++; return fmt.Sprintf("%s-%03d", prefix, n) }

	// ── Turn 1: short ask, a few tool calls (stays collapsed as a summary) ──
	msg(t, db, clawID, id("m"), "user",
		"Can you look at the flaky token refresh test on main? It fails maybe 1 in 5 runs in CI.",
		"", at(0), "AnaBerg")
	msg(t, db, clawID, id("m"), "hub", "[hub] ▶ Stage: Implement", "", at(2*time.Second), "")

	for i, path := range []string{
		"pkg/hub/github_token_refresh_test.go",
		"pkg/hub/github.go",
		"pkg/hub/github_client.go",
		"pkg/hub/model_auth.go",
		"pkg/hub/retry.go",
		"go.mod",
	} {
		toolPair(t, db, clawID, id("a"), at(time.Duration(10+i*4)*time.Second), 120+i*30,
			map[string]interface{}{"tool": "Read", "path": path}, nil)
	}

	msg(t, db, clawID, id("m"), "claw", turn1Reply, "", at(90*time.Second), "")
	msg(t, db, clawID, id("m"), "system", "Session resumed after gateway reconnect", "", at(100*time.Second), "")
	msg(t, db, clawID, id("m"), "hub",
		"[hub] No turn has been running for 12 minutes — nudging the agent to report progress.",
		"", at(110*time.Second), "")
	msg(t, db, clawID, id("m"), "state", "implement → review", "", at(115*time.Second), "")

	// ── Turn 2: the rich one. Everything below lands in a single activity gap
	// so the web's trailing-summary prefetch expands it without a click. ──
	msg(t, db, clawID, id("m"), "user", turn2UserMessage, "", at(20*time.Minute), "AnaBerg")

	g := func(offset time.Duration) time.Time { return at(20*time.Minute + offset) }

	act(t, db, clawID, id("a"), g(2*time.Second), map[string]interface{}{
		"kind": "model_started", "message": "waiting for anthropic/claude-opus-4-6",
	})

	// read x4 → collapses into "Read 4 files"
	for i, path := range []string{
		"pkg/hub/github_token_refresh_test.go",
		"pkg/hub/github.go",
		"pkg/hub/server.go",
		"pkg/hub/db.go",
	} {
		toolPair(t, db, clawID, id("a"), g(time.Duration(6+i*3)*time.Second), 90+i*20,
			map[string]interface{}{"tool": "Read", "path": path}, nil)
	}

	// search
	toolPair(t, db, clawID, id("a"), g(22*time.Second), 340,
		map[string]interface{}{"tool": "Grep", "detail": `refreshInstallationToken\(` + " in pkg/hub"},
		map[string]interface{}{"result": "pkg/hub/github.go:412\npkg/hub/github.go:588\npkg/hub/github_client.go:97\npkg/hub/pr_watcher.go:233\n\n4 matches in 3 files"})

	// failing bash with error output
	toolPair(t, db, clawID, id("a"), g(30*time.Second), 48210,
		map[string]interface{}{"tool": "Bash", "command": "go test ./pkg/hub -run TestGitHubTokenRefresh -count=20 -race"},
		map[string]interface{}{
			"phase":     "failed",
			"exit_code": 1,
			"error":     failingTestOutput,
		})

	// edit with a diff result
	toolPair(t, db, clawID, id("a"), g(90*time.Second), 1120,
		map[string]interface{}{"tool": "Edit", "path": "pkg/hub/github.go"},
		map[string]interface{}{"result": editDiffResult})

	// write
	toolPair(t, db, clawID, id("a"), g(96*time.Second), 480,
		map[string]interface{}{"tool": "Write", "path": "pkg/hub/github_token_refresh_race_test.go"},
		map[string]interface{}{"result": "Wrote 84 lines to pkg/hub/github_token_refresh_race_test.go"})

	// web fetch
	toolPair(t, db, clawID, id("a"), g(104*time.Second), 860,
		map[string]interface{}{"tool": "WebFetch", "url": "https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app"},
		map[string]interface{}{"result": "Installation access tokens expire after 1 hour. Requesting a new token before expiry is allowed and does not invalidate the previous one."})

	// subagent spawn, finished
	toolPair(t, db, clawID, id("a"), g(112*time.Second), 74300,
		map[string]interface{}{
			"tool":            "Task",
			"detail":          "audit every refreshInstallationToken caller",
			"subagent_name":   "token-refresh-audit",
			"subagent_type":   "Explore",
			"subagent_model":  "claude-sonnet-5",
			"subagent_prompt": "Find every call site of refreshInstallationToken in pkg/hub and report which ones hold s.mu while calling it. Return file:line plus a one-line verdict for each.",
		},
		map[string]interface{}{"result": subagentResult})

	// long output that gets truncated by the expanded <pre>
	toolPair(t, db, clawID, id("a"), g(190*time.Second), 9120,
		map[string]interface{}{"tool": "Bash", "command": "go test ./pkg/hub/... -count=1 2>&1 | tail -n 60"},
		map[string]interface{}{"result": longTestOutput})

	// diagnostic + session error (warning tone)
	act(t, db, clawID, id("a"), g(200*time.Second), map[string]interface{}{
		"kind": "diagnostic", "detail": "gateway rss 812 MB / 2 GB, event loop lag 42 ms",
	})
	act(t, db, clawID, id("a"), g(205*time.Second), map[string]interface{}{
		"kind":  "session_error",
		"error": "gateway session key rotated mid-turn; reattached to session 0f3a91c2 without losing context",
	})

	// green run
	toolPair(t, db, clawID, id("a"), g(212*time.Second), 51840,
		map[string]interface{}{"tool": "Bash", "command": "go test ./pkg/hub -run TestGitHubTokenRefresh -count=50 -race"},
		map[string]interface{}{"result": "ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub\t51.840s"})

	msg(t, db, clawID, id("m"), "claw", turn2Reply, "", at(25*time.Minute), "")

	// ── Turn 3: in-progress. Trailing running steps keep the turn live. ──
	msg(t, db, clawID, id("m"), "user",
		"Nice. Add a regression test that fails without the fix, then open the PR in draft with AnaBerg as assignee.",
		"", time.Now().Add(-3*time.Minute), "AnaBerg")
	act(t, db, clawID, id("a"), time.Now().Add(-170*time.Second), map[string]interface{}{
		"kind": "model_started", "message": "waiting for anthropic/claude-opus-4-6",
	})
	toolRunning(t, db, clawID, id("a"), time.Now().Add(-150*time.Second), map[string]interface{}{
		"tool":            "Task",
		"detail":          "write the regression test",
		"subagent_name":   "regression-test-author",
		"subagent_type":   "general-purpose",
		"subagent_model":  "claude-opus-5",
		"subagent_prompt": "Write a table-driven test in pkg/hub that spawns 32 goroutines calling installationToken concurrently and fails if more than one refresh round-trip is issued.",
	})
	toolRunning(t, db, clawID, id("a"), time.Now().Add(-40*time.Second), map[string]interface{}{
		"tool": "Bash", "command": "go test ./pkg/hub -run TestGitHubTokenRefreshSingleFlight -race -count=50",
	})
}

func seedIdleConversation(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()
	base := time.Now().Add(-6 * time.Hour)
	msg(t, db, clawID, "idle-m1", "user", "Bump the web deps to the latest minor and make sure the build still passes.", "", base, "AnaBerg")
	msg(t, db, clawID, "idle-m2", "hub", "[hub] ▶ Stage: Review", "", base.Add(time.Second), "")
	act(t, db, clawID, "idle-a1", base.Add(4*time.Second), map[string]interface{}{
		"kind": "tool", "phase": "completed", "tool": "Bash", "call_id": "idle-1",
		"command": "npm --prefix web update --save", "duration_ms": 18400,
		"result": "changed 41 packages in 18s",
	})
	msg(t, db, clawID, "idle-m3", "claw",
		"Bumped 41 packages (all minor). `npm --prefix web run build` is green and the static export still emits every route.\n\nNothing else to do here — waiting on review. [DONE]",
		"", base.Add(30*time.Second), "")
}

func seedErrorConversation(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()
	base := time.Now().Add(-40 * time.Minute)
	msg(t, db, clawID, "err-m1", "user", "Add exponential backoff to the outbound webhook retries.", "", base, "AnaBerg")
	act(t, db, clawID, "err-a1", base.Add(8*time.Second), map[string]interface{}{
		"kind": "tool", "phase": "failed", "tool": "Bash", "call_id": "err-1",
		"command": "go build ./...", "exit_code": 2, "duration_ms": 4200,
		"error": "pkg/hub/external_webhook.go:118:21: undefined: backoffSchedule",
	})
	msg(t, db, clawID, "err-m2", "hub",
		"[hub] Automatic continuation paused: the last 3 turns never reached the agent — claw-bridge returned a transport error instead. Last error: websocket: close 1006 (abnormal closure). This is the sandbox or the gateway failing, not the agent's output; fix it and send a message to resume.",
		"", base.Add(20*time.Second), "")
}

// ─── fake bridge ─────────────────────────────────────────────────────────────

func startFakeBridge(t *testing.T, addr, clawID, name string) {
	t.Helper()
	ctx := context.Background()
	url := "ws://" + addr + "/claw/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial fake bridge for %s: %v", clawID, err)
	}
	conn.SetReadLimit(32 << 20)
	ready := true
	reg := types.WSMessage{Type: "register", Payload: types.RegisterPayload{
		ClawID:       clawID,
		Name:         name,
		Template:     "elasticclaw",
		Token:        visualClawToken,
		GatewayReady: &ready,
	}}
	if err := wsjson.Write(ctx, conn, reg); err != nil {
		t.Fatalf("register fake bridge for %s: %v", clawID, err)
	}
	go func() {
		defer conn.Close(websocket.StatusNormalClosure, "harness shutdown")
		for {
			var m types.WSMessage
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				log.Printf("[chat-visual] bridge %s read stopped: %v", name, err)
				return
			}
		}
	}()
}

func waitForPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("hub did not start listening on %s", addr)
}

// ─── content ─────────────────────────────────────────────────────────────────

const turn1Reply = `I can reproduce it. Running the suite 20 times gives 4 failures, always in ` + "`TestGitHubTokenRefresh/concurrent_refresh`" + `.

The shape of the failure points at the installation-token cache rather than the test:

- two goroutines miss the cache in the same window
- both issue a ` + "`POST /app/installations/:id/access_tokens`" + `
- the slower response wins and overwrites a **newer** token with an older ` + "`expires_at`" + `

Digging into ` + "`pkg/hub/github.go`" + ` now.`

var turn2UserMessage = strings.Join([]string{
	"That matches what we saw in the Depot logs last week, so let's fix it properly rather than retrying the test.",
	"",
	"Two things I care about:",
	"",
	"1. The fix has to be single-flight per installation id, not a global lock — the PR watcher polls 40 repos and a global lock would serialise all of them.",
	"2. Whatever you do must keep working when the hub has two processes running during a rolling deploy. If that means we accept a duplicate refresh across processes, say so explicitly in the PR description instead of pretending the race is gone.",
	"",
	"I attached the CI log from the worst run so you don't have to re-run it yourself.",
	"",
	"[Attachments]",
	"- depot-run-8821-token-refresh.log — /workspace/uploads/depot-run-8821-token-refresh.log (text/plain, 214.7 KB)",
}, "\n")

const failingTestOutput = `--- FAIL: TestGitHubTokenRefresh (2.41s)
    --- FAIL: TestGitHubTokenRefresh/concurrent_refresh (2.31s)
        github_token_refresh_test.go:142: expected 1 token request, got 3
        github_token_refresh_test.go:151: cached token expires_at went backwards:
              want >= 2026-09-14T12:41:07Z
              got    2026-09-14T12:40:52Z
==================
WARNING: DATA RACE
Write at 0x00c000412a30 by goroutine 87:
  github.com/elasticclaw/elasticclaw/pkg/hub.(*Server).cacheInstallationToken()
      /src/pkg/hub/github.go:601 +0x184

Previous write at 0x00c000412a30 by goroutine 74:
  github.com/elasticclaw/elasticclaw/pkg/hub.(*Server).cacheInstallationToken()
      /src/pkg/hub/github.go:601 +0x184
==================
FAIL
FAIL	github.com/elasticclaw/elasticclaw/pkg/hub	48.210s`

const editDiffResult = `@@ -404,18 +404,27 @@ func (s *Server) installationToken(ctx context.Context, id int64) (string, error)
-	if tok, ok := s.tokenCache[id]; ok && time.Until(tok.ExpiresAt) > tokenRefreshSkew {
-		return tok.Value, nil
-	}
-	fresh, err := s.refreshInstallationToken(ctx, id)
-	if err != nil {
-		return "", err
-	}
-	s.tokenCache[id] = fresh
-	return fresh.Value, nil
+	s.tokenMu.RLock()
+	tok, ok := s.tokenCache[id]
+	s.tokenMu.RUnlock()
+	if ok && time.Until(tok.ExpiresAt) > tokenRefreshSkew {
+		return tok.Value, nil
+	}
+	// Single-flight is keyed by installation id, not global: the PR watcher
+	// polls ~40 repos across installations and a shared lock would serialise
+	// every one of them behind the slowest GitHub round-trip.
+	key := strconv.FormatInt(id, 10)
+	value, err, _ := s.tokenGroup.Do(key, func() (interface{}, error) {
+		fresh, err := s.refreshInstallationToken(ctx, id)
+		if err != nil {
+			return nil, err
+		}
+		s.tokenMu.Lock()
+		// Never let a slower in-flight response overwrite a newer token.
+		if existing, ok := s.tokenCache[id]; !ok || fresh.ExpiresAt.After(existing.ExpiresAt) {
+			s.tokenCache[id] = fresh
+		}
+		cached := s.tokenCache[id]
+		s.tokenMu.Unlock()
+		return cached.Value, nil
+	})`

const subagentResult = `7 call sites, 2 of them problematic:

pkg/hub/github.go:412        ok    — the new single-flight path
pkg/hub/github.go:588        BAD   — holds s.mu across the HTTP round-trip
pkg/hub/github_client.go:97  ok    — already behind installationToken
pkg/hub/pr_watcher.go:233    BAD   — calls refresh directly, bypassing the cache
pkg/hub/pr_watcher.go:301    ok
pkg/hub/factory_creator.go:88 ok
pkg/hub/workspace_api.go:145 ok

Verdict: fixing installationToken alone is not enough; pr_watcher.go:233 must
route through it or it will keep minting parallel tokens.`

var longTestOutput = strings.Join([]string{
	"ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub\t9.120s",
	"ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub/artifact\t0.412s",
	"ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub/notify\t0.201s",
	"ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub/pipeline\t1.884s",
	"ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub/workflowv2\t3.702s",
	"=== RUN   TestGitHubTokenRefresh",
	"=== PAUSE TestGitHubTokenRefresh",
	"=== CONT  TestGitHubTokenRefresh",
	"=== RUN   TestGitHubTokenRefresh/cold_cache",
	"=== RUN   TestGitHubTokenRefresh/warm_cache",
	"=== RUN   TestGitHubTokenRefresh/expired_within_skew",
	"=== RUN   TestGitHubTokenRefresh/concurrent_refresh",
	"=== RUN   TestGitHubTokenRefresh/refresh_error_is_not_cached",
	"--- PASS: TestGitHubTokenRefresh (2.09s)",
	"    --- PASS: TestGitHubTokenRefresh/cold_cache (0.01s)",
	"    --- PASS: TestGitHubTokenRefresh/warm_cache (0.00s)",
	"    --- PASS: TestGitHubTokenRefresh/expired_within_skew (0.01s)",
	"    --- PASS: TestGitHubTokenRefresh/concurrent_refresh (2.05s)",
	"    --- PASS: TestGitHubTokenRefresh/refresh_error_is_not_cached (0.02s)",
	"=== RUN   TestPRWatcherUsesInstallationTokenCache",
	"--- PASS: TestPRWatcherUsesInstallationTokenCache (0.31s)",
	"=== RUN   TestPRWatcherMultiPR",
	"--- PASS: TestPRWatcherMultiPR (0.44s)",
	"=== RUN   TestReaperOfflineClaw",
	"--- PASS: TestReaperOfflineClaw (0.12s)",
	"=== RUN   TestServerShutdownDrainsBridges",
	"--- PASS: TestServerShutdownDrainsBridges (0.28s)",
	"=== RUN   TestWorkflowV2ControlWS",
	"--- PASS: TestWorkflowV2ControlWS (1.07s)",
	"PASS",
	"coverage: 71.4% of statements",
	"ok  \tgithub.com/elasticclaw/elasticclaw/pkg/hub\t9.120s\tcoverage: 71.4% of statements",
}, "\n")

var turn2Reply = strings.Join([]string{
	"## Fixed — the cache, not the test",
	"",
	"The flake was real. `installationToken` read the cache, missed, and refreshed without any coordination, so N concurrent callers issued N token requests and the **slowest response won**, writing an older `expires_at` over a newer one.",
	"",
	"### What changed",
	"",
	"1. `installationToken` now goes through a `singleflight.Group` keyed by installation id.",
	"2. The cache write only accepts a token whose `expires_at` is *after* the one already stored.",
	"3. `pr_watcher.go:233` was calling `refreshInstallationToken` directly, bypassing the cache entirely — it now routes through `installationToken`.",
	"",
	"The interesting part is the key choice:",
	"",
	"```go",
	"// Keyed by installation id, not global: the PR watcher polls ~40 repos",
	"// across installations and a shared key would serialise every one of them",
	"// behind the slowest GitHub round-trip.",
	"key := strconv.FormatInt(id, 10)",
	"value, err, _ := s.tokenGroup.Do(key, func() (interface{}, error) {",
	"\tfresh, err := s.refreshInstallationToken(ctx, id)",
	"\tif err != nil {",
	"\t\treturn nil, err",
	"\t}",
	"\treturn s.cacheNewestToken(id, fresh), nil",
	"})",
	"```",
	"",
	"### Results",
	"",
	"| Run | Before | After |",
	"| --- | ------ | ----- |",
	"| `-count=20` | 4 failures | 0 failures |",
	"| `-count=50 -race` | 11 failures | 0 failures |",
	"| Token requests / 32 goroutines | 3–7 | 1 |",
	"| Wall clock, 40-repo poll | 12.4s | 12.6s |",
	"",
	"### About the rolling deploy",
	"",
	"> Whatever you do must keep working when the hub has two processes running during a rolling deploy.",
	"",
	"It does not fully. `singleflight` is per-process, so during a rolling deploy two hubs can each mint a token for the same installation. That is **safe** — GitHub does not invalidate the previous installation token when a new one is issued ([docs](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app)) — but it is not zero. I put that caveat in the PR description verbatim rather than claiming the race is gone.",
	"",
	"### The diff, in short",
	"",
	"```diff",
	"-\tfresh, err := s.refreshInstallationToken(ctx, id)",
	"-\ts.tokenCache[id] = fresh",
	"+\tvalue, err, _ := s.tokenGroup.Do(key, func() (interface{}, error) {",
	"+\t\tfresh, err := s.refreshInstallationToken(ctx, id)",
	"+\t\treturn s.cacheNewestToken(id, fresh), err",
	"+\t})",
	"```",
	"",
	"Reproduce locally with:",
	"",
	"```bash",
	"go test ./pkg/hub -run TestGitHubTokenRefresh -count=50 -race",
	"```",
	"",
	"and the web-side type that consumes the token is unchanged:",
	"",
	"```ts",
	"export interface InstallationToken {",
	"  value: string",
	"  /** ISO instant; the hub guarantees this only ever moves forward. */",
	"  expiresAt: string",
	"}",
	"```",
	"",
	"Next: regression test + draft PR.",
}, "\n")
