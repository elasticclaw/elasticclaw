package hub

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
)

func newCheckpointCompletionTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	_, _ = db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "tenant", "token", "claw-token", now())
	_, _ = db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,?)`,
		"claw", "tenant", "claw", "template", "connected", now())
	return &Server{
		db:                db,
		hubCfg:            &types.HubConfig{},
		claws:             map[string]*clawConn{},
		checkpointWaiters: map[string]chan error{},
	}
}

func seedCheckpointCompletionState(t *testing.T, s *Server) {
	t.Helper()
	for _, id := range []string{"current", "pending"} {
		if err := s.insertCheckpoint(id, "tenant", "claw", "manual", "hub", "local", "provider-id"); err != nil {
			t.Fatalf("insert checkpoint %s: %v", id, err)
		}
	}
	s.claws["claw"] = &clawConn{
		id:                   "claw",
		tenantID:             "tenant",
		checkpointInProgress: true,
		pendingCheckpointID:  "pending",
	}
}

func completeCheckpoint(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/checkpoints/current/complete", bytes.NewBufferString(body))
	req.Header.Set("X-Claw-Token", "claw-token")
	rr := httptest.NewRecorder()
	s.handleCheckpointInternal(rr, req)
	return rr
}

func TestCheckpointCompleteErrorDrainsPendingCheckpoint(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	seedCheckpointCompletionState(t, s)

	rr := completeCheckpoint(t, s, `{"error":"bridge failed"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rr.Code, rr.Body.String())
	}

	assertPendingCheckpointDrained(t, s)
}

func TestCheckpointFinalizeErrorDrainsPendingCheckpoint(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	seedCheckpointCompletionState(t, s)

	rr := completeCheckpoint(t, s, `{"root_sha256":"`+strings.Repeat("a", 64)+`"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d: %s", rr.Code, rr.Body.String())
	}

	assertPendingCheckpointDrained(t, s)
}

func TestDispatchCheckpointWriteFailureRemovesWaiter(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	if err := s.insertCheckpoint("write-fail", "tenant", "claw", "manual", "hub", "local", "provider-id"); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}

	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "test closing")
	}))
	t.Cleanup(wsServer.Close)

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "closed before dispatch")

	cc := &clawConn{
		id:                   "claw",
		tenantID:             "tenant",
		conn:                 conn,
		checkpointInProgress: true,
	}
	s.claws["claw"] = cc

	if _, err := s.dispatchCheckpoint(context.Background(), cc, "claw", "write-fail", "manual", false, checkpointRequestTimeout); err == nil {
		t.Fatal("expected dispatch write failure")
	}

	s.checkpointMu.Lock()
	_, ok := s.checkpointWaiters["write-fail"]
	s.checkpointMu.Unlock()
	if ok {
		t.Fatal("expected failed dispatch waiter to be removed")
	}
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.checkpointInProgress {
		t.Fatal("expected failed dispatch to clear checkpoint in progress")
	}
}

func TestDaytonaRestoreCommandSingleQuotesPaths(t *testing.T) {
	remote := "/home/daytona/.openclaw/workspace/src/$SECRET/`touch /tmp/pwn`/it's.txt"
	cmd := checkpointDaytonaRestoreCommand(remote, []byte("hello"))

	if !strings.Contains(cmd, "mkdir -p "+checkpointShellQuote("/home/daytona/.openclaw/workspace/src/$SECRET/`touch /tmp/pwn`")) {
		t.Fatalf("expected single-quoted mkdir path, got %q", cmd)
	}
	if !strings.Contains(cmd, "> "+checkpointShellQuote(remote)) {
		t.Fatalf("expected single-quoted output path, got %q", cmd)
	}
	if strings.Contains(cmd, `mkdir -p "`) || strings.Contains(cmd, `> "`) {
		t.Fatalf("expected restore paths to avoid shell double quotes, got %q", cmd)
	}
}

func assertPendingCheckpointDrained(t *testing.T, s *Server) {
	t.Helper()
	cc := s.claws["claw"]
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.checkpointInProgress {
		t.Fatal("expected current checkpoint to be marked no longer in progress")
	}
	if cc.pendingCheckpointID != "" {
		t.Fatalf("expected pending checkpoint to be drained, got %q", cc.pendingCheckpointID)
	}
}
