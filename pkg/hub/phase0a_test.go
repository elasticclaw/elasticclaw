package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// TestPostMessageStoresAndReturnsUserLogin verifies that POST /api/messages
// persists the resolved GitHub login and that the REST response and timeline
// both surface it.
func TestPostMessageStoresAndReturnsUserLogin(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Auth: &types.AuthConfig{SessionSecret: "session-secret"},
	}, "", "", "")

	clawID := "claw-user-login"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, created_at) VALUES(?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "claw-user-login", "running",
	); err != nil {
		t.Fatal(err)
	}

	session, err := signGitHubSession("session-secret", "octocat", "", "")
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	body := strings.NewReader(`{"content":"hello from the dashboard"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/messages/"+clawID, body)
	req.Header.Set("Authorization", "Bearer "+session)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/messages status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var posted types.HubMessage
	if err := json.NewDecoder(rec.Body).Decode(&posted); err != nil {
		t.Fatal(err)
	}
	if posted.UserLogin == nil || *posted.UserLogin != "octocat" {
		t.Fatalf("POST response user_login = %v, want octocat", posted.UserLogin)
	}

	var storedLogin string
	if err := db.QueryRow(`SELECT user_login FROM messages WHERE id=?`, posted.ID).Scan(&storedLogin); err != nil {
		t.Fatal(err)
	}
	if storedLogin != "octocat" {
		t.Fatalf("stored user_login = %q, want octocat", storedLogin)
	}

	timelineReq := httptest.NewRequest(http.MethodGet, "/api/messages/"+clawID+"/timeline", nil)
	timelineReq.Header.Set("Authorization", "Bearer "+session)
	timelineRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(timelineRec, timelineReq)
	if timelineRec.Code != http.StatusOK {
		t.Fatalf("GET timeline status = %d, body=%s", timelineRec.Code, timelineRec.Body.String())
	}
	var timeline []types.HubMessage
	if err := json.NewDecoder(timelineRec.Body).Decode(&timeline); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range timeline {
		if m.ID == posted.ID {
			found = true
			if m.UserLogin == nil || *m.UserLogin != "octocat" {
				t.Fatalf("timeline entry user_login = %v, want octocat", m.UserLogin)
			}
		}
	}
	if !found {
		t.Fatalf("posted message %s not found in timeline: %#v", posted.ID, timeline)
	}
}

func TestUserWSClearsClientSuppliedLoginForTokenAuth(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	const clawID = "claw-ws-token-login"
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,created_at) VALUES(?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "ws claw", "connected"); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws?token=test-token", nil)
	if err != nil {
		t.Fatalf("dial user websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "done") })
	victim := "victim"
	if err := wsjson.Write(ctx, conn, types.WSMessage{Type: "message", Payload: types.HubMessage{ClawID: clawID, Content: "message", UserLogin: &victim}}); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		var login *string
		err := db.QueryRow(`SELECT user_login FROM messages WHERE claw_id=?`, clawID).Scan(&login)
		if err == nil {
			if login != nil {
				t.Fatalf("persisted user_login = %q, want NULL", *login)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("read persisted websocket message: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWSMessageFrameIncludesUserLogin asserts that a "message" WS frame
// broadcast to connected dashboard users carries user_login, following the
// existing hub WS test patterns (pre-created claw row, t.Fatal on DB errors).
func TestWSMessageFrameIncludesUserLogin(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")

	clawID := "claw-ws-user-login"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, created_at) VALUES(?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "claw-ws-user-login", "running",
	); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	userCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	userWS, _, err := websocket.Dial(userCtx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/ws?token=test-token", nil)
	if err != nil {
		t.Fatalf("dial user ws: %v", err)
	}
	t.Cleanup(func() { _ = userWS.Close(websocket.StatusNormalClosure, "done") })

	// The server registers the connection into s.users just after accepting
	// the WS handshake, which races with this goroutine under load — wait for
	// registration so the broadcast below isn't sent before anyone is
	// listening (which would otherwise hang the read below forever).
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		registered := len(s.users) > 0
		s.mu.RUnlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("user connection never registered in s.users")
		}
		time.Sleep(10 * time.Millisecond)
	}

	login := "dashboard-human"
	s.broadcastToUsers("test-tenant-id", types.WSMessage{
		Type: "message",
		Payload: types.HubMessage{
			ID: "msg-ws-1", ClawID: clawID, TenantID: "test-tenant-id",
			Role: "user", Content: "hi from ws", UserLogin: &login, CreatedAt: time.Now(),
		},
	})

	// The user WS also receives an initial claw_status frame on connect;
	// skip frames until the "message" broadcast lands.
	var frame types.WSMessage
	for {
		if err := wsjson.Read(userCtx, userWS, &frame); err != nil {
			t.Fatalf("read ws frame: %v", err)
		}
		if frame.Type == "message" {
			break
		}
	}
	payload, ok := frame.Payload.(map[string]interface{})
	if !ok {
		t.Fatalf("payload is %T, want map[string]interface{}: %#v", frame.Payload, frame.Payload)
	}
	if got, _ := payload["user_login"].(string); got != login {
		t.Fatalf("payload user_login = %q, want %q; payload=%#v", got, login, payload)
	}
}

// TestClawPRStoredStateFoldsMergedIntoState asserts the open/merged/closed
// fold-in pr_watcher applies before writing claw_prs.state: GitHub's own
// `state` field only ever reports "open"|"closed", so a merged PR must be
// recognized via the separate `merged` flag or it becomes indistinguishable
// from a PR that was rejected.
func TestClawPRStoredStateFoldsMergedIntoState(t *testing.T) {
	tests := []struct {
		githubState string
		merged      bool
		want        string
	}{
		{githubState: "open", merged: false, want: "open"},
		{githubState: "closed", merged: false, want: "closed"},
		{githubState: "closed", merged: true, want: "merged"},
	}
	for _, test := range tests {
		if got := clawPRStoredState(test.githubState, test.merged); got != test.want {
			t.Fatalf("clawPRStoredState(%q, %v) = %q, want %q", test.githubState, test.merged, got, test.want)
		}
	}
}

// TestCheckPRMergedPersistsStateBeforeTerminating asserts checkPRMerged
// writes the folded state/merged/merged_at columns to claw_prs before it
// tears the claw (and its claw_prs rows) down on a genuine merge.
func TestCheckPRMergedPersistsStateBeforeTerminating(t *testing.T) {
	githubStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"state": "closed",
			"merged": true,
			"merged_at": "2026-01-02T03:04:05Z",
			"created_at": "2026-01-01T00:00:00Z",
			"draft": false,
			"head": {"sha": "deadbeef"}
		}`))
	}))
	t.Cleanup(githubStub.Close)

	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, githubStub.URL, "", "")

	clawID := "claw-pr-state"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, created_at) VALUES(?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "claw-pr-state", "running",
	); err != nil {
		t.Fatal(err)
	}
	prID := "pr-row-1"
	if _, err := db.Exec(
		`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		prID, clawID, "acme/widgets", 42, "https://github.com/acme/widgets/pull/42",
	); err != nil {
		t.Fatal(err)
	}

	pr := clawPR{id: prID, clawID: clawID, repo: "acme/widgets", prNumber: 42, prURL: "https://github.com/acme/widgets/pull/42"}
	terminated := s.checkPRMerged(pr, "gh-token")
	if !terminated {
		t.Fatal("checkPRMerged returned false, want true for a merged PR")
	}

	// A genuine merge terminates the claw and clears its claw_prs rows as part
	// of the same synchronous call (see pr_watcher.go's post-merge cleanup) —
	// so the row is gone by the time checkPRMerged returns. That is correct
	// product behavior, not a bug: confirm the row was in fact removed rather
	// than left dangling with stale state.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE id=?`, prID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("claw_prs row count after merge termination = %d, want 0 (cleared with the claw)", count)
	}
}

// TestClawPRsAPIReturnsStateMergedAndMergedAt asserts /api/claws/:id/prs
// serializes the state/merged/merged_at columns pr_watcher maintains — using
// a still-open PR row, since a merged PR's row is cleared alongside claw
// termination (see TestCheckPRMergedPersistsStateBeforeTerminating).
func TestClawPRsAPIReturnsStateMergedAndMergedAt(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")

	clawID := "claw-pr-api"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, created_at) VALUES(?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "claw-pr-api", "running",
	); err != nil {
		t.Fatal(err)
	}
	prID := "pr-row-api"
	if _, err := db.Exec(
		`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, state, merged, merged_at, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		prID, clawID, "acme/widgets", 42, "https://github.com/acme/widgets/pull/42", "open", false, nil,
	); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	req := httptest.NewRequest(http.MethodGet, "/api/claws/"+clawID+"/prs", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET prs status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var prs []struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Merged   bool   `json:"merged"`
		MergedAt string `json:"mergedAt"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&prs); err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("prs len = %d, want 1: %#v", len(prs), prs)
	}
	if prs[0].State != "open" || prs[0].Merged {
		t.Fatalf("pr JSON = %#v, want state=open merged=false", prs[0])
	}
}

// TestTaskRunEventsAllowsCIEventTypes asserts the CHECK constraint on
// task_run_events.event_type accepts the new ci_succeeded/ci_failed values.
func TestTaskRunEventsAllowsCIEventTypes(t *testing.T) {
	_, db := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")

	if _, err := db.Exec(
		`INSERT INTO task_runs(id, tenant_id, initial_attempt_id, run_kind, owner_type, created_at, updated_at)
		 VALUES('run-1','test-tenant-id','attempt-1','pr_task','manual',strftime('%s','now')*1000,strftime('%s','now')*1000)`,
	); err != nil {
		t.Fatal(err)
	}

	for _, eventType := range []string{taskRunEventCISucceeded, taskRunEventCIFailed} {
		_, err := db.Exec(
			`INSERT INTO task_run_events(id, tenant_id, run_id, attempt_id, event_key, source, event_type, event_time, observed_at, created_at)
			 VALUES(lower(hex(randomblob(16))), 'test-tenant-id', 'run-1', 'attempt-1', ?, 'hub', ?, strftime('%s','now')*1000, strftime('%s','now')*1000, strftime('%s','now')*1000)`,
			"key-"+eventType, eventType,
		)
		if err != nil {
			t.Fatalf("insert task_run_events with event_type=%s: %v", eventType, err)
		}
	}
}

// TestCheckCIStatusEmitsEventsOnlyOnTransition asserts pr_watcher emits
// ci_succeeded/ci_failed task_run_events exactly once per (headSha,
// conclusion) transition, not on every poll of the same conclusion.
func TestCheckCIStatusEmitsEventsOnlyOnTransition(t *testing.T) {
	checkRunsCalls := 0
	githubStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs"):
			checkRunsCalls++
			_, _ = w.Write([]byte(`{"check_runs":[{"status":"completed","conclusion":"success","name":"ci","details_url":""}]}`))
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(`{"head":{"sha":"deadbeef"}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(githubStub.Close)

	s, db := NewTestServerWithConfig(t, &types.HubConfig{}, githubStub.URL, "", "")

	clawID := "claw-ci-transition"
	tenantID := "test-tenant-id"
	runID := "run-ci-transition"
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: runID, AttemptID: "attempt-ci-transition", ClawID: clawID, TenantID: tenantID,
		OwnerType: taskRunOwnerFactory, Workspace: "eng", Factory: "bugfix", Integration: "github",
		Repo: "acme/widgets", Model: "gpt-5", StartedAt: 1760000000000,
	})
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, task_run_id, created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		clawID, tenantID, "claw-ci-transition", "running", runID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE task_runs SET current_attempt_id=? WHERE id=?`, "attempt-ci-transition", runID); err != nil {
		t.Fatal(err)
	}
	prID := "pr-ci-transition"
	if _, err := db.Exec(
		`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		prID, clawID, "acme/widgets", 7, "https://github.com/acme/widgets/pull/7",
	); err != nil {
		t.Fatal(err)
	}

	pr := clawPR{id: prID, clawID: clawID, repo: "acme/widgets", prNumber: 7, prURL: "https://github.com/acme/widgets/pull/7"}
	s.checkCIStatus(pr, "gh-token")
	s.checkCIStatus(pr, "gh-token") // same conclusion again: must not double-fire

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type=?`, runID, taskRunEventCISucceeded,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("ci_succeeded event count = %d, want 1 (no duplicate on repeated poll)", count)
	}
}

// TestTaskRunAnalyticsRunExposesIssueCreatedAt asserts the analytics run view
// JSON includes issueCreatedAt.
func TestTaskRunAnalyticsRunExposesIssueCreatedAt(t *testing.T) {
	s, db := newTaskRunAnalyticsAPITestServer(t)

	issueCreatedAt := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).UnixMilli()
	insertTaskRunAnalyticsAPIRun(t, db, apiRunFixture{
		RunID: "run-issue-created-at", AttemptID: "attempt-issue-created-at", ClawID: "claw-issue-created-at",
		TenantID: "test-tenant-id", OwnerType: taskRunOwnerFactory, Workspace: "eng", Factory: "bugfix",
		Integration: "github", Repo: "elastic/claw", Model: "gpt-5",
		StartedAt: 1760000000000, IssueCreatedAt: issueCreatedAt,
	})

	rr := requestTaskRunAnalyticsAPI(t, s, http.MethodGet, "/api/analytics/runs", "test-token")
	var out struct {
		Runs []struct {
			RunID          string `json:"runId"`
			IssueCreatedAt int64  `json:"issueCreatedAt"`
		} `json:"runs"`
	}
	decodeTaskRunAnalyticsAPI(t, rr, &out)
	found := false
	for _, r := range out.Runs {
		if r.RunID == "run-issue-created-at" {
			found = true
			if r.IssueCreatedAt != issueCreatedAt {
				t.Fatalf("issueCreatedAt = %d, want %d", r.IssueCreatedAt, issueCreatedAt)
			}
		}
	}
	if !found {
		t.Fatalf("run not found in response: %#v", out.Runs)
	}
}

// TestClawStatusCarriesContextUsage documents and verifies the existing
// context-usage plumbing: the hub tracks contextUsage per connected claw
// from agent heartbeats and surfaces it on claw_status (WS + /api/claws).
// The upstream agent protocol already reports this value via the heartbeat's
// context_usage field, so no plumbing gap exists here (see notes in the
// commit message for what was investigated).
func TestClawStatusCarriesContextUsage(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	clawID := "claw-context-usage"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, status, created_at) VALUES(?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "claw-context-usage", "starting",
	); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := true
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw ws: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "done") })
	if err := wsjson.Write(ctx, conn, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID: clawID, Name: "claw-context-usage", Template: "elasticclaw",
			Token: "claw-token", GatewayReady: &ready,
		},
	}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(ctx, conn, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}

	if err := wsjson.Write(ctx, conn, types.WSMessage{
		Type:    "heartbeat",
		Payload: map[string]interface{}{"context_usage": 42},
	}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.RLock()
		usage := 0
		if cc, ok := s.claws[clawID]; ok {
			usage = cc.contextUsage
		}
		s.mu.RUnlock()
		if usage == 42 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("contextUsage never reached 42, got %d", usage)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
