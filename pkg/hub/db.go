package hub

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite, no CGO required
)

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	// Add columns that may be missing from older databases.
	// SQLite doesn't support IF NOT EXISTS on ALTER TABLE, so ignore errors.
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN provider TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN provider_id TEXT NOT NULL DEFAULT ''`)

	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS tenants (
		id        TEXT PRIMARY KEY,
		name      TEXT NOT NULL,
		token     TEXT NOT NULL UNIQUE, -- user login token
		claw_token TEXT NOT NULL UNIQUE, -- token claws present on connect
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS claws (
		id          TEXT PRIMARY KEY,
		tenant_id   TEXT NOT NULL REFERENCES tenants(id),
		name        TEXT NOT NULL,
		template    TEXT NOT NULL DEFAULT '',
		provider    TEXT NOT NULL DEFAULT '',
		provider_id TEXT NOT NULL DEFAULT '',
		status      TEXT NOT NULL DEFAULT 'offline',
		last_seen   DATETIME,
		created_at  DATETIME NOT NULL
	);



	CREATE TABLE IF NOT EXISTS messages (
		id         TEXT PRIMARY KEY,
		claw_id    TEXT NOT NULL REFERENCES claws(id),
		tenant_id  TEXT NOT NULL,
		role       TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_messages_claw ON messages(claw_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_claws_tenant  ON claws(tenant_id);
	`)
	return err
}

func now() time.Time { return time.Now().UTC() }
