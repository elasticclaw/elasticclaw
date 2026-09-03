package hub

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
)

func checkpointRestoreTestFiles(t *testing.T, contents []string) []types.CheckpointFile {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	files := make([]types.CheckpointFile, 0, len(contents))
	for i, content := range contents {
		sha := strings.Repeat(string(rune('a'+i)), 64)
		path := checkpointBlobPath(sha)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("make blob directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write blob: %v", err)
		}
		files = append(files, types.CheckpointFile{Path: "workspace/file-" + string(rune('a'+i)), SHA256: sha, Size: int64(len(content))})
	}
	return files
}

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

func TestRestoreCheckpointFilesToWritesFiles(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	files := checkpointRestoreTestFiles(t, []string{"one", "two"})
	written := make(map[string]string)
	var mu sync.Mutex
	err := s.restoreCheckpointFilesTo(context.Background(), "claw", "checkpoint", files, func(path string) string {
		return "/remote/" + path
	}, 1, func(_ context.Context, remote string, data []byte) error {
		mu.Lock()
		written[remote] = string(data)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got, want := written["/remote/workspace/file-a"], "one"; got != want {
		t.Fatalf("first file = %q, want %q", got, want)
	}
	if got, want := written["/remote/workspace/file-b"], "two"; got != want {
		t.Fatalf("second file = %q, want %q", got, want)
	}
	if len(written) != len(files) {
		t.Fatalf("wrote %d files, want %d", len(written), len(files))
	}
}

func TestRestoreCheckpointFilesToBoundsParallelism(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	files := checkpointRestoreTestFiles(t, make([]string, 32))
	for i := range files {
		path := checkpointBlobPath(files[i].SHA256)
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatalf("rewrite blob: %v", err)
		}
	}
	var inFlight, maxInFlight atomic.Int64
	err := s.restoreCheckpointFilesTo(context.Background(), "claw", "checkpoint", files, func(path string) string { return path }, 8, func(_ context.Context, _ string, _ []byte) error {
		current := inFlight.Add(1)
		for {
			max := maxInFlight.Load()
			if current <= max || maxInFlight.CompareAndSwap(max, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := maxInFlight.Load(); got > 8 {
		t.Fatalf("maximum concurrency = %d, want at most 8", got)
	}
	if got := maxInFlight.Load(); got < 2 {
		t.Fatalf("maximum concurrency = %d, want parallel writes", got)
	}
}

func TestRestoreCheckpointFilesToReturnsWriteError(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	files := checkpointRestoreTestFiles(t, []string{"good", "bad", "never"})
	var written []string
	err := s.restoreCheckpointFilesTo(context.Background(), "claw", "checkpoint", files, func(path string) string { return path }, 1, func(_ context.Context, remote string, _ []byte) error {
		if strings.HasSuffix(remote, "file-b") {
			return errors.New("upload failed")
		}
		written = append(written, remote)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "restore workspace/file-b") {
		t.Fatalf("restore error = %v, want wrapped path", err)
	}
	// The remaining files must not be uploaded after the first failure: the
	// worker's cancellation check must stop the rest of the manifest from
	// being sent once the group is cancelled.
	for _, remote := range written {
		if strings.HasSuffix(remote, "file-c") {
			t.Fatalf("kept restoring after failure: wrote %q", remote)
		}
	}
}

func TestRestoreCheckpointFilesToReportsProgress(t *testing.T) {
	s := newCheckpointCompletionTestServer(t)
	files := checkpointRestoreTestFiles(t, []string{"one", "two"})
	originalNow := checkpointRestoreNow
	base := time.Now()
	var calls atomic.Int64
	checkpointRestoreNow = func() time.Time {
		return base.Add(time.Duration(calls.Add(1)) * 11 * time.Second)
	}
	t.Cleanup(func() { checkpointRestoreNow = originalNow })
	if err := s.restoreCheckpointFilesTo(context.Background(), "claw", "checkpoint", files, func(path string) string { return path }, 1, func(_ context.Context, _ string, _ []byte) error {
		return nil
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT bootstrap_status FROM claws WHERE id='claw'`).Scan(&status); err != nil {
		t.Fatalf("read bootstrap status: %v", err)
	}
	if !strings.Contains(status, "Restoring checkpoint files (") {
		t.Fatalf("bootstrap status = %q, want restore progress", status)
	}
}

type blockingCheckpointSSHSession struct {
	closeStarted chan struct{}
	neverClose   chan struct{}
	once         sync.Once
}

func (s *blockingCheckpointSSHSession) CombinedOutput(string) ([]byte, error) {
	<-s.neverClose
	return nil, nil
}

func (s *blockingCheckpointSSHSession) Close() error {
	s.once.Do(func() { close(s.closeStarted) })
	<-s.neverClose
	return nil
}

func TestSSHWriteSessionHonorsContextDeadline(t *testing.T) {
	sess := &blockingCheckpointSSHSession{closeStarted: make(chan struct{}), neverClose: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := sshWriteSession(ctx, sess, "cat > file", func() error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write error = %v, want deadline exceeded", err)
	}
	select {
	case <-sess.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("session close was not started after context expiry")
	}
}

func TestSSHWriteSessionWithNewSessionHonorsContextDeadline(t *testing.T) {
	newSessionStarted := make(chan struct{})
	clientClosed := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := sshWriteSessionWithNewSession(ctx, "cat > file", func() (checkpointSSHSession, error) {
		close(newSessionStarted)
		select {}
	}, func() error {
		close(clientClosed)
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write error = %v, want deadline exceeded", err)
	}
	select {
	case <-newSessionStarted:
	case <-time.After(time.Second):
		t.Fatal("new session was not started")
	}
	select {
	case <-clientClosed:
	case <-time.After(time.Second):
		t.Fatal("client was not closed after context expiry")
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
