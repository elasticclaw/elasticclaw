package hub

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// busyRetryDelays bounds execRetryBusy. Each attempt already waits up to
// busy_timeout (5s), so the worst case is about 40s: long enough to outlast a
// write-heavy maintenance pass, short enough for the goroutines that call it.
var busyRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// busyRetrySleep is a test seam for the waits between execRetryBusy attempts.
var busyRetrySleep = time.Sleep

// isSQLiteBusy reports whether err is SQLite's "database is locked", including
// its extended codes (SQLITE_BUSY_RECOVERY, SQLITE_BUSY_SNAPSHOT, ...).
func isSQLiteBusy(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// execRetryBusy runs a single autocommit statement, retrying while another
// connection holds the write lock past busy_timeout. A busy statement did not
// commit, so retrying it is safe. Writes whose loss strands a claw (an injected
// prompt, a stage transition) use it instead of failing on the first lock.
func execRetryBusy(db *sql.DB, query string, args ...any) (sql.Result, error) {
	result, err := db.Exec(query, args...)
	for _, delay := range busyRetryDelays {
		if !isSQLiteBusy(err) {
			break
		}
		busyRetrySleep(delay)
		result, err = db.Exec(query, args...)
	}
	return result, err
}
