// Package store owns the hub's SQLite schema and its versioned migrations.
//
// Migrations are numbered SQL files embedded from migrations/ (0001_init.sql,
// 0002_..., ...). Applied versions are recorded in the schema_migrations
// table, and each migration runs inside its own transaction (SQLite supports
// transactional DDL), so a failure aborts boot without leaving a half-applied
// migration behind.
//
// Downgrades are NOT supported: the runner only applies versions above the
// current one and fails if the database reports a version newer than the
// binary knows about. To roll back a release, restore hub.db from a backup.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration is a single numbered SQL file.
type migration struct {
	version int
	name    string // file name, e.g. "0001_init.sql"
	sql     string
}

var migrationNameRe = regexp.MustCompile(`^(\d+)_.+\.sql$`)

// loadMigrations reads and orders the embedded migration files, rejecting
// duplicate or malformed version numbers.
func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	seen := make(map[int]string)
	var ms []migration
	for _, e := range entries {
		m := migrationNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q does not match NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration file %q has invalid version", e.Name())
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", version, prev, e.Name())
		}
		seen[version] = e.Name()
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: version, name: e.Name(), sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// Migrate brings the database schema up to date. It is called at boot and any
// error must abort startup.
//
// Existing installs that predate versioned migrations (tables present but no
// schema_migrations) get one final run of the legacy idempotent catch-up (the
// same CREATE IF NOT EXISTS / ALTER TABLE statements older releases ran at
// every boot), and are then baselined at version 1 without executing
// 0001_init.sql. Fresh databases execute every migration from 0001 onward.
func Migrate(db *sql.DB) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	if len(ms) == 0 || ms[0].version != 1 {
		return fmt.Errorf("embedded migrations must start at version 1")
	}

	hasVersions, err := tableExists(db, "schema_migrations")
	if err != nil {
		return err
	}
	if !hasVersions {
		legacy, err := tableExists(db, "claws")
		if err != nil {
			return err
		}
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			applied_at DATETIME NOT NULL
		)`); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
		if legacy {
			// Pre-versioning install: bring it to the exact 0001 schema via
			// the legacy idempotent path, then record the baseline.
			if err := legacyBaseline(db, ms[0].name); err != nil {
				return err
			}
		}
	}

	current, err := currentVersion(db)
	if err != nil {
		return err
	}
	if current > ms[len(ms)-1].version {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); downgrades are not supported", current, ms[len(ms)-1].version)
	}

	for _, m := range ms {
		if m.version <= current {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration executes one migration and records it, atomically.
func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration %s: begin: %w", m.name, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	if _, err := tx.Exec(m.sql); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?,?,?)`, m.version, m.name, time.Now().UTC()); err != nil {
		return fmt.Errorf("migration %s: record version: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: commit: %w", m.name, err)
	}
	return nil
}

func currentVersion(db *sql.DB) (int, error) {
	var v sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return int(v.Int64), nil
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return n > 0, nil
}
