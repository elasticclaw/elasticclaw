package hub

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// A failed allocation must not pin the disk for the life of the process
// ---------------------------------------------------------------------------
//
// In WAL mode a transaction that spills its page cache writes frames to the
// -wal file as it goes. When it then fails -- SQLITE_FULL at the last page of
// a table copy, on the disk-full hub -- the frames are rolled back but the
// file keeps its grown size until the database is closed. Boot ran three
// table copies and two index builds through that path and then served with
// zero bytes free.
//
// What these tests reproduce, and what they do not: the WAL growth here comes
// from a transaction aborted by a UNIQUE conflict on its last row, not from
// ENOSPC. At the file level the two are the same event -- frames spilled, then
// a rollback that does not shrink the file -- and the assertion is on the
// -wal file's size, which is the pinned disk itself. A bounded filesystem
// would make the trigger faithful too, and there is no portable way to build
// one in a unit test; PRAGMA max_page_count is not a substitute, because it
// refuses the page before any frame is written and leaves the WAL small.

// openFileDB opens a file-backed database with the production settings and
// returns it with the path of its -wal file.
func openFileDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db, path + "-wal"
}

func walSize(t *testing.T, walPath string) int64 {
	t.Helper()
	info, err := os.Stat(walPath)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// spillingFailure is a transaction that writes several megabytes of frames
// past the page cache and then fails on its last row. It must fail, and it
// must have grown the WAL, or the fixture proves nothing.
func spillingFailure(t *testing.T, db *sql.DB, walPath string) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS spill (x, k INTEGER UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO spill VALUES(NULL, 2000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if size := walSize(t, walPath); size != 0 {
		t.Fatalf("WAL is %d bytes before the failed transaction", size)
	}
	_, err := db.Exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<2000)
		INSERT INTO spill SELECT zeroblob(4000), i FROM c`)
	if err == nil {
		t.Fatal("the spilling transaction did not fail; the fixture reaches nothing")
	}
	if size := walSize(t, walPath); size < 4<<20 {
		t.Fatalf("the failed transaction left the WAL at %d bytes; it did not spill, so the fixture does not model the pinned disk", size)
	}
}

// Revert verified against: runOptionalMigration without the truncate.
func TestFailedMigrationStepReleasesTheWALItGrew(t *testing.T) {
	db, walPath := openFileDB(t)
	var pinned int64
	runOptionalMigration(db, "spilling step", func(db *sql.DB) error {
		spillingFailure(t, db, walPath)
		pinned = walSize(t, walPath)
		return errors.New("database or disk is full (13)")
	})
	if size := walSize(t, walPath); size != 0 {
		t.Fatalf("the WAL is still %d bytes after the step failed (it had grown to %d); the space stays pinned until the hub restarts", size, pinned)
	}
}

// Revert verified against: ensureRetentionIndexes without the truncate.
//
// The index build is the sweeper's path too (buildRetentionIndexesAfterCycle),
// where no restart is coming. Under max_page_count the build itself fails
// before spilling, so the WAL is pinned first by a spilled failure and the
// assertion is that the failed build releases it.
func TestFailedRetentionIndexBuildReleasesThePinnedWAL(t *testing.T) {
	db, walPath := openFileDB(t)
	for _, stmt := range []string{
		`CREATE TABLE messages (id TEXT PRIMARY KEY, created_at INTEGER)`,
		`CREATE TABLE task_run_events (id TEXT PRIMARY KEY, event_time INTEGER)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	spillingFailure(t, db, walPath)
	forbidDatabaseGrowth(t, db)
	if ensureRetentionIndexes(db) {
		t.Fatal("the index build succeeded under the growth cap; the fixture does not reach the failure path")
	}
	if size := walSize(t, walPath); size != 0 {
		t.Fatalf("the WAL is still %d bytes after the index build failed", size)
	}
}

// Every hub connection bounds the WAL, so a large transaction that succeeds
// does not keep its size for the life of the process either.
func TestProductionDSNBoundsTheWAL(t *testing.T) {
	db, _ := openFileDB(t)
	var limit int64
	if err := db.QueryRow(`PRAGMA journal_size_limit`).Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != walSizeLimitBytes {
		t.Fatalf("journal_size_limit = %d, want %d", limit, walSizeLimitBytes)
	}
}

// ---------------------------------------------------------------------------
// The doomed allocations are not attempted on a visibly full disk
// ---------------------------------------------------------------------------

// modelFreeDisk makes the gate see the given number of free bytes.
func modelFreeDisk(t *testing.T, free int64) {
	t.Helper()
	previous := diskFreeBytes
	diskFreeBytes = func(string) (int64, error) { return free, nil }
	t.Cleanup(func() { diskFreeBytes = previous })
}

const narrowFailureTypeTable = `CREATE TABLE task_run_attempts (
	id TEXT PRIMARY KEY,
	failure_type TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('timeout','provider_lost','permission_or_auth_failed','unknown')))`

// Revert verified against: widenFailureTypeCheckV1 without the
// tableCopyFitsOnDisk check, and ensureRetentionIndexes without the
// indexBuildFitsOnDisk check.
func TestTableCopiesAndIndexBuildsAreSkippedOnALowDisk(t *testing.T) {
	t.Run("table copy", func(t *testing.T) {
		db, _ := openFileDB(t)
		if _, err := db.Exec(narrowFailureTypeTable); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO task_run_attempts VALUES('a', 'timeout')`); err != nil {
			t.Fatal(err)
		}
		modelFreeDisk(t, 1)
		err := widenFailureTypeCheckV1(db, "task_run_attempts")
		if err == nil || !strings.Contains(err.Error(), "needs about") {
			t.Fatalf("widen on a disk with one free byte: err = %v, want the skip", err)
		}
		if widened := failureTypeSchemaWidened(t, db); widened {
			t.Fatal("the copy ran on a disk with one free byte")
		}
		modelFreeDisk(t, 1<<40)
		if err := widenFailureTypeCheckV1(db, "task_run_attempts"); err != nil {
			t.Fatalf("widen with room: %v", err)
		}
		if !failureTypeSchemaWidened(t, db) {
			t.Fatal("the copy did not run with room on the disk")
		}
	})
	t.Run("index build", func(t *testing.T) {
		db, _ := openFileDB(t)
		for _, stmt := range []string{
			`CREATE TABLE messages (id TEXT PRIMARY KEY, created_at INTEGER)`,
			`CREATE TABLE task_run_events (id TEXT PRIMARY KEY, event_time INTEGER)`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
		modelFreeDisk(t, 1)
		if ensureRetentionIndexes(db) {
			t.Fatal("ensureRetentionIndexes reported every index built on a disk with one free byte")
		}
		for _, idx := range retentionIndexes {
			if retentionIndexBuilt(t, db, idx.name) {
				t.Fatalf("%s was built on a disk with one free byte", idx.name)
			}
		}
		modelFreeDisk(t, 1<<40)
		if !ensureRetentionIndexes(db) {
			t.Fatal("the indexes were not built with room on the disk")
		}
	})
	// The gate is an optimisation of the failure, not a second line of
	// defence: where the free space cannot be known, the step runs.
	t.Run("unknown free space lets the step run", func(t *testing.T) {
		db, err := sql.Open("sqlite", sqliteDSN(":memory:"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		if _, err := db.Exec(narrowFailureTypeTable); err != nil {
			t.Fatal(err)
		}
		modelFreeDisk(t, 1)
		if err := widenFailureTypeCheckV1(db, "task_run_attempts"); err != nil {
			t.Fatalf("widen on an in-memory database: %v", err)
		}
		if !failureTypeSchemaWidened(t, db) {
			t.Fatal("the copy was skipped without a free-space figure to skip it on")
		}
	})
}

func failureTypeSchemaWidened(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var schema string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='task_run_attempts'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	return strings.Contains(schema, "workspace_unresponsive")
}

func retentionIndexBuilt(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// The real free-space query answers on this platform, with a plausible
// figure, for the directory the database lives in.
func TestDiskFreeBytesQueriesTheFilesystem(t *testing.T) {
	free, err := diskFreeBytesOS(t.TempDir())
	if err != nil {
		t.Skipf("free disk space is not queried on this platform: %v", err)
	}
	if free <= 0 {
		t.Fatalf("free = %d on a filesystem a test directory was just created on", free)
	}
	if _, err := diskFreeBytesOS(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("statfs of a missing directory succeeded")
	}
}
