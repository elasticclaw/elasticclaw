// Package store is the hub's data layer (phase-2 hub reorganization,
// item 2.4): one repository per aggregate (Claws, Messages, Settings,
// Tenants, Analytics), migrations, transactions for multi-step writes
// (WithTx) and SQLITE_BUSY retry centralized in the store wrapper.
//
// The package only knows pkg/types. The phase-2.4 target contract is
// that services and handlers talk to the repositories and never issue
// raw SQL, with all SQLite-specific SQL living here (repositories build
// dynamic queries with the Placeholder constant, keeping the door open
// for an optional Postgres backend later). The migration is incremental:
// this PR moves the claw, message, tenant, template and pruning call
// sites; the remaining raw-SQL call sites in pkg/hub (provisioning,
// workflow creation/state, analytics, ...) still hold the *sql.DB
// directly and are moved by the follow-up PRs in the phase-2 stack,
// after which the acceptance grep from the re-arch doc must be empty.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite, no CGO required
)

// Placeholder is the SQL parameter placeholder used when repositories
// build dynamic queries. A single constant keeps the placeholder style
// in one place for a future optional Postgres backend.
const Placeholder = "?"

// busy-retry tuning: short exponential backoff, bounded total wait.
const (
	busyMaxAttempts  = 5
	busyBaseInterval = 10 * time.Millisecond
)

// Store wraps the hub database and hands out the per-aggregate
// repositories. It is a cheap stateless wrapper around *sql.DB, so it
// can be rebuilt per call (mirroring how hub services are built).
type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the hub SQLite database at path and returns
// the store bound to it.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := Migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// New wraps an already-open (and migrated) database. Used by the hub
// server and tests that manage the *sql.DB lifecycle themselves.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB exposes the underlying database. It exists only as an escape hatch
// for legacy call sites that have not yet migrated to a repository; new
// code must use the repositories instead.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Repositories.

// Claws returns the claw-aggregate repository.
func (s *Store) Claws() *ClawsRepo { return &ClawsRepo{st: s} }

// Messages returns the message-aggregate repository.
func (s *Store) Messages() *MessagesRepo { return &MessagesRepo{st: s} }

// Settings returns the settings-aggregate repository (hub templates).
func (s *Store) Settings() *SettingsRepo { return &SettingsRepo{st: s} }

// Tenants returns the tenant-aggregate repository.
func (s *Store) Tenants() *TenantsRepo { return &TenantsRepo{st: s} }

// Analytics returns the analytics-aggregate repository.
func (s *Store) Analytics() *AnalyticsRepo { return &AnalyticsRepo{st: s} }

// commitTx commits the transaction. Indirection so tests can force a
// busy error on the commit path.
var commitTx = func(tx *sql.Tx) error { return tx.Commit() }

// WithTx runs fn inside a transaction. The transaction is committed when
// fn returns nil and rolled back otherwise. The whole transaction is
// retried (with backoff) when SQLite reports the database is busy while
// fn runs, so multi-step writes never hand SQLITE_BUSY handling to
// callers.
//
// A busy error from the commit itself is NOT retried: database/sql
// finalizes the Tx on the first Commit call, so the commit cannot be
// reissued on the same transaction, and blindly replaying fn in a new
// transaction after a failed commit is unsafe (the closure may carry
// non-idempotent side effects and the outcome at the caller boundary is
// ambiguous). Such errors are returned to the caller as-is.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	err := retryBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := commitTx(tx); err != nil {
			_ = tx.Rollback()
			return nonRetryableError{err}
		}
		return nil
	})
	var nr nonRetryableError
	if errors.As(err, &nr) {
		return nr.err
	}
	return err
}

// nonRetryableError marks an error that must not trigger a busy retry
// even when the underlying cause is SQLITE_BUSY (e.g. a failed commit).
type nonRetryableError struct{ err error }

func (e nonRetryableError) Error() string { return e.err.Error() }
func (e nonRetryableError) Unwrap() error { return e.err }

// exec runs a write statement with busy retry.
func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := retryBusy(ctx, func() error {
		var execErr error
		res, execErr = s.db.ExecContext(ctx, query, args...)
		return execErr
	})
	return res, err
}

// queryRowScan runs a single-row query with busy retry. sql.ErrNoRows is
// returned as-is (it is a result, not a transient failure).
func (s *Store) queryRowScan(ctx context.Context, query string, args []any, dest ...any) error {
	return retryBusy(ctx, func() error {
		return s.db.QueryRowContext(ctx, query, args...).Scan(dest...)
	})
}

// queryScan runs a multi-row query and drives scan inside the busy-retry
// loop, so SQLITE_BUSY surfaced while iterating rows (rows.Next /
// rows.Err) is retried just like a busy error from the initial query.
// scan may run more than once and must reset any accumulated state at
// the start of each invocation. rows.Err() is checked after scan
// returns; scan does not need to check it.
func (s *Store) queryScan(ctx context.Context, query string, args []any, scan func(rows *sql.Rows) error) error {
	return retryBusy(ctx, func() error {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		if err := scan(rows); err != nil {
			return err
		}
		return rows.Err()
	})
}

// retryBusy retries fn with exponential backoff while it fails with
// SQLITE_BUSY / "database is locked". Any other outcome (nil, ErrNoRows,
// constraint violations, ...) is returned immediately.
func retryBusy(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < busyMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(busyBaseInterval << (attempt - 1)):
			}
		}
		err = fn()
		if err == nil || !isBusy(err) {
			return err
		}
	}
	return err
}

// isBusy reports whether err is SQLite's "database is busy/locked"
// condition. modernc.org/sqlite surfaces it in the error text with the
// SQLITE_BUSY/SQLITE_LOCKED code names. Errors wrapped as
// nonRetryableError are never busy, whatever their cause.
func isBusy(err error) bool {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return false
	}
	var nr nonRetryableError
	if errors.As(err, &nr) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}
