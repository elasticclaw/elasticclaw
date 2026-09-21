package hub

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"modernc.org/sqlite"
)

// failingNextConfig arms one injected iteration failure: the failOnCall-th
// Next on any result set of a query containing querySubstring returns err.
// failOnCall 0 disarms it. Zero-valued while a test seeds its fixture, so the
// seed itself runs through the wrapper untouched.
type failingNextConfig struct {
	querySubstring string
	failOnCall     int32
	err            error
}

// failingNextDriver wraps the production SQLite driver so a test can make
// sql.Rows.Next stop with an error part-way through a result set -- the one
// condition database/sql reports only through rows.Err(), and the one a
// fixture cannot produce with data alone.
//
// The wrapper implements only the base driver.Conn/driver.Stmt/driver.Rows
// interfaces, so database/sql takes its Prepare-then-Query path; the inner
// modernc objects still do all the real work, argument conversion included.
type failingNextDriver struct {
	inner sqlite.Driver
	cfg   *failingNextConfig
}

var failingNextDriverSeq atomic.Int32

// registerFailingNextDriver registers a fresh wrapper under its own name (a
// driver name can be registered once per process) and returns that name.
func registerFailingNextDriver(cfg *failingNextConfig) string {
	name := fmt.Sprintf("hub-test-sqlite-failing-next-%d", failingNextDriverSeq.Add(1))
	sql.Register(name, &failingNextDriver{cfg: cfg})
	return name
}

func (d *failingNextDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &failingNextConn{Conn: conn, cfg: d.cfg}, nil
}

type failingNextConn struct {
	driver.Conn
	cfg *failingNextConfig
}

func (c *failingNextConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &failingNextStmt{Stmt: stmt, query: query, cfg: c.cfg}, nil
}

type failingNextStmt struct {
	driver.Stmt
	query string
	cfg   *failingNextConfig
}

func (s *failingNextStmt) Query(args []driver.Value) (driver.Rows, error) {
	rows, err := s.Stmt.Query(args)
	if err != nil {
		return nil, err
	}
	if s.cfg.failOnCall > 0 && strings.Contains(s.query, s.cfg.querySubstring) {
		return &failingNextRows{Rows: rows, cfg: s.cfg}, nil
	}
	return rows, nil
}

type failingNextRows struct {
	driver.Rows
	cfg   *failingNextConfig
	calls int32
}

func (r *failingNextRows) Next(dest []driver.Value) error {
	r.calls++
	if r.calls == r.cfg.failOnCall {
		return r.cfg.err
	}
	return r.Rows.Next(dest)
}

// newFailingNextServer seeds a file-backed hub database with the production
// openDB (so the schema is real), then reopens it through the wrapper driver
// and hands back a Server on that connection plus the shared config the test
// arms once its fixture is in place.
func newFailingNextServer(t *testing.T) (*Server, *failingNextConfig) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "hub.db")
	seed, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	cfg := &failingNextConfig{}
	db, err := sql.Open(registerFailingNextDriver(cfg), sqliteDSN(path))
	if err != nil {
		t.Fatalf("open wrapped db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, hubCfg: &types.HubConfig{}, claws: map[string]*clawConn{}}
	reference := time.Now()
	if _, err := s.db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "tenant", "token", "claw-token", reference); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	insertRetentionClaw(t, s, "claw", reference)
	return s, cfg
}

// blobFilesUnder lists every regular file under the checkpoint blob root.
func blobFilesUnder(t *testing.T) []string {
	t.Helper()
	root := filepath.Join(checkpointsRoot(), "blobs")
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("walk blob root: %v", err)
	}
	return files
}

// A read failure part-way through the messages query used to be
// indistinguishable from the end of the conversation: the loop stopped, the
// rows read so far were hashed, and a blob holding a PREFIX of the transcript
// was published under a digest that vouched for it. The checkpoint then
// referenced that blob as the whole conversation.
func TestWriteMessageCheckpointBlobFailsWhenIterationStopsEarly(t *testing.T) {
	s, cfg := newFailingNextServer(t)
	reference := time.Now()
	for i, content := range []string{"first", "second", "third"} {
		if _, err := s.db.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, format, created_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("m%d", i), "claw", "tenant", "user", content, "", reference.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	if got := blobFilesUnder(t); len(got) != 0 {
		t.Fatalf("blob root not empty before the test: %v", got)
	}

	// The second Next fails: one message has been read and encoded, two have
	// not. That is the prefix the old code published.
	injected := errors.New("injected: connection lost mid-iteration")
	*cfg = failingNextConfig{querySubstring: "FROM messages", failOnCall: 2, err: injected}

	sha, count, _, err := s.writeMessageCheckpointBlob("claw", "tenant")
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v (sha=%q count=%d), want the injected iteration error", err, sha, count)
	}
	if sha != "" {
		t.Fatalf("sha = %q on failure, want empty", sha)
	}
	if got := blobFilesUnder(t); len(got) != 0 {
		t.Fatalf("a blob was published from a truncated read: %v", got)
	}

	// Disarmed, the same connection reads the whole conversation and publishes
	// it: the wrapper is faithful, and the failure above was the injection.
	cfg.failOnCall = 0
	sha, count, _, err = s.writeMessageCheckpointBlob("claw", "tenant")
	if err != nil {
		t.Fatalf("writeMessageCheckpointBlob after disarming: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}
	if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
		t.Fatalf("blob not published after a clean read: %v", err)
	}
}

// The PR list is written into the manifest and a restore trusts it as the
// complete set of tracked work, so it fails the same way.
func TestCheckpointManifestFailsWhenPRIterationStopsEarly(t *testing.T) {
	s, cfg := newFailingNextServer(t)
	reference := time.Now()
	for i := 1; i <= 2; i++ {
		if _, err := s.db.Exec(`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, state, created_at) VALUES(?,?,?,?,?,?,?)`,
			fmt.Sprintf("pr%d", i), "claw", "org/repo", i, fmt.Sprintf("https://github.com/org/repo/pull/%d", i), "open", reference); err != nil {
			t.Fatalf("seed pr: %v", err)
		}
	}
	injected := errors.New("injected: connection lost mid-iteration")
	*cfg = failingNextConfig{querySubstring: "FROM claw_prs", failOnCall: 2, err: injected}

	if _, err := s.buildCheckpointManifest("cp", "claw", "", "", 0, reference, nil); !errors.Is(err, injected) {
		t.Fatalf("buildCheckpointManifest err = %v, want the injected iteration error", err)
	}

	cfg.failOnCall = 0
	manifest, err := s.buildCheckpointManifest("cp", "claw", "", "", 0, reference, nil)
	if err != nil {
		t.Fatalf("buildCheckpointManifest after disarming: %v", err)
	}
	if len(manifest.PRs) != 2 {
		t.Fatalf("manifest lists %d PRs, want 2", len(manifest.PRs))
	}
}
