package hub

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS tenants (
		id        TEXT PRIMARY KEY,
		name      TEXT NOT NULL,
		token     TEXT NOT NULL UNIQUE, -- user login token
		claw_token TEXT NOT NULL UNIQUE, -- token claws present on connect
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS claws (
		id         TEXT PRIMARY KEY,
		tenant_id  TEXT NOT NULL REFERENCES tenants(id),
		name       TEXT NOT NULL,
		template   TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL DEFAULT 'offline',
		last_seen  DATETIME,
		created_at DATETIME NOT NULL
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
