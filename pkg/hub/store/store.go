// Package store is the hub's data layer (phase-2 hub reorganization,
// item 2.4): one repository per aggregate (Claws, Messages, Settings,
// Tenants, Analytics), migrations, transactions for multi-step writes
// (WithTx) and SQLITE_BUSY retry centralized in the store wrapper.
//
// The package only knows pkg/types; services and handlers talk to the
// repositories and never issue raw SQL. All SQLite-specific SQL lives
// here, keeping the door open for an optional Postgres backend later:
// repositories build dynamic queries with the Placeholder constant and
// no SQLite-exclusive syntax leaks outside this package.
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

// WithTx runs fn inside a transaction. The transaction is committed when
// fn returns nil and rolled back otherwise. The whole transaction is
// retried (with backoff) when SQLite reports the database is busy, so
// multi-step writes never hand SQLITE_BUSY handling to callers.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return retryBusy(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	})
}

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

// query runs a multi-row query with busy retry. The caller owns rows.
func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	err := retryBusy(ctx, func() error {
		var queryErr error
		rows, queryErr = s.db.QueryContext(ctx, query, args...)
		return queryErr
	})
	return rows, err
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
// SQLITE_BUSY/SQLITE_LOCKED code names.
func isBusy(err error) bool {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}
