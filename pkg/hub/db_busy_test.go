package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// busyTestDBs opens two pools on one file: holder takes the write lock, and
// writer fails fast (busy_timeout 0) while it is held.
func busyTestDBs(t *testing.T) (holder *sql.Tx, writer *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	writer, err = sql.Open("sqlite", path+"?_txlock=immediate&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	if _, err := db.Exec(`CREATE TABLE busy_probe(id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	holder, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Rollback() })
	return holder, writer
}

func TestIsSQLiteBusy(t *testing.T) {
	_, writer := busyTestDBs(t)
	_, err := writer.Exec(`INSERT INTO busy_probe(id) VALUES(1)`)
	if !isSQLiteBusy(err) {
		t.Fatalf("driver error %v not detected as busy", err)
	}
	if !isSQLiteBusy(fmt.Errorf("insert: %w", err)) {
		t.Fatal("wrapped driver error not detected as busy")
	}
	if !isSQLiteBusy(errors.New("database is locked (5) (SQLITE_BUSY)")) {
		t.Fatal("busy message not detected")
	}
	for _, err := range []error{nil, sql.ErrNoRows, errors.New("UNIQUE constraint failed")} {
		if isSQLiteBusy(err) {
			t.Fatalf("%v detected as busy", err)
		}
	}
}

func TestExecRetryBusyOutlastsLock(t *testing.T) {
	holder, writer := busyTestDBs(t)
	var waits []time.Duration
	prev := busyRetrySleep
	busyRetrySleep = func(d time.Duration) {
		waits = append(waits, d)
		if len(waits) == 2 {
			if err := holder.Commit(); err != nil {
				t.Errorf("release lock: %v", err)
			}
		}
	}
	t.Cleanup(func() { busyRetrySleep = prev })

	if _, err := execRetryBusy(writer, `INSERT INTO busy_probe(id) VALUES(1)`); err != nil {
		t.Fatalf("execRetryBusy: %v", err)
	}
	if len(waits) != 2 {
		t.Fatalf("waits=%v, want 2 before the lock was released", waits)
	}
	var n int
	if err := writer.QueryRow(`SELECT COUNT(*) FROM busy_probe`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows=%d,%v", n, err)
	}
}

func TestExecRetryBusyIsBounded(t *testing.T) {
	_, writer := busyTestDBs(t)
	var waits []time.Duration
	prev := busyRetrySleep
	busyRetrySleep = func(d time.Duration) { waits = append(waits, d) }
	t.Cleanup(func() { busyRetrySleep = prev })

	_, err := execRetryBusy(writer, `INSERT INTO busy_probe(id) VALUES(1)`)
	if !isSQLiteBusy(err) {
		t.Fatalf("err=%v, want busy after the last attempt", err)
	}
	if len(waits) != len(busyRetryDelays) {
		t.Fatalf("waits=%v, want %v", waits, busyRetryDelays)
	}
	var total time.Duration
	for _, d := range busyRetryDelays {
		total += d
	}
	if total > 30*time.Second {
		t.Fatalf("retry waits total %s; callers block on this", total)
	}
}

func TestExecRetryBusyDoesNotRetryOtherErrors(t *testing.T) {
	_, writer := busyTestDBs(t)
	prev := busyRetrySleep
	busyRetrySleep = func(time.Duration) { t.Fatal("retried a non-busy error") }
	t.Cleanup(func() { busyRetrySleep = prev })
	if _, err := execRetryBusy(writer, `INSERT INTO missing_table(id) VALUES(1)`); err == nil || isSQLiteBusy(err) {
		t.Fatalf("err=%v, want a non-busy error", err)
	}
}
