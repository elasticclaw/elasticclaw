package hub

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite, no CGO required
)

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
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
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN default_model TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN template_files TEXT NOT NULL DEFAULT '{}'`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN ssh_host TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN ssh_port INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN ssh_user TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN github_installation_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN github_repos TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN linear_workspace TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN nix INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN color TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN linear_issue_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN auto_fix_ci INTEGER NOT NULL DEFAULT 1`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN auto_fix_bugbot INTEGER NOT NULL DEFAULT 1`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN llm_key TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN pipeline_stage TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_comment_at TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN pr_conditions_fired INTEGER NOT NULL DEFAULT 0`)

	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS tenants (
		id        TEXT PRIMARY KEY,
		name      TEXT NOT NULL,
		token     TEXT NOT NULL UNIQUE, -- user login token
		claw_token TEXT NOT NULL UNIQUE, -- token claws present on connect
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS claws (
		id             TEXT PRIMARY KEY,
		tenant_id      TEXT NOT NULL REFERENCES tenants(id),
		name           TEXT NOT NULL,
		template       TEXT NOT NULL DEFAULT '',
		provider       TEXT NOT NULL DEFAULT '',
		provider_id    TEXT NOT NULL DEFAULT '',
		default_model  TEXT NOT NULL DEFAULT '',
		template_files TEXT NOT NULL DEFAULT '{}',
		status         TEXT NOT NULL DEFAULT 'offline',
		last_seen      DATETIME,
		created_at     DATETIME NOT NULL,
		ssh_host       TEXT NOT NULL DEFAULT '',
		ssh_port       INTEGER NOT NULL DEFAULT 0,
		ssh_user       TEXT NOT NULL DEFAULT '',
		github_installation_id INTEGER NOT NULL DEFAULT 0,
		github_repos   TEXT NOT NULL DEFAULT '',
		linear_workspace TEXT NOT NULL DEFAULT '',
		nix              INTEGER NOT NULL DEFAULT 0,
		tags             TEXT NOT NULL DEFAULT '[]',
		color            TEXT NOT NULL DEFAULT '',
		linear_issue_id  TEXT NOT NULL DEFAULT '',
		auto_fix_ci      INTEGER NOT NULL DEFAULT 1,
		auto_fix_bugbot  INTEGER NOT NULL DEFAULT 1,
		llm_key          TEXT NOT NULL DEFAULT '',
		pipeline_stage   TEXT NOT NULL DEFAULT ''
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

	CREATE TABLE IF NOT EXISTS hub_templates (
		name       TEXT PRIMARY KEY,
		files      TEXT NOT NULL DEFAULT '{}',  -- JSON map of filename -> content
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS claw_prs (
		id          TEXT PRIMARY KEY,
		claw_id     TEXT NOT NULL REFERENCES claws(id),
		repo        TEXT NOT NULL,  -- e.g. "owner/repo"
		pr_number   INTEGER NOT NULL,
		pr_url      TEXT NOT NULL,
		last_ci_sha TEXT NOT NULL DEFAULT '',   -- last SHA we checked CI on
		last_comment_id INTEGER NOT NULL DEFAULT 0, -- last bugbot comment ID seen
		last_comment_at TEXT NOT NULL DEFAULT '', -- timestamp of last seen comment
		pr_conditions_fired INTEGER NOT NULL DEFAULT 0,
		created_at  DATETIME NOT NULL,
		UNIQUE(claw_id, pr_url)
	);

	CREATE TABLE IF NOT EXISTS factory_events (
		id           TEXT PRIMARY KEY,
		factory_name TEXT NOT NULL,
		issue_id     TEXT NOT NULL,
		issue_title  TEXT NOT NULL DEFAULT '',
		prev_status  TEXT NOT NULL DEFAULT '',
		new_status   TEXT NOT NULL DEFAULT '',
		action       TEXT NOT NULL,  -- 'claw_created', 'claw_terminated', 'not_actionable'
		claw_id      TEXT NOT NULL DEFAULT '',
		detail       TEXT NOT NULL DEFAULT '',
		created_at   DATETIME NOT NULL
	);
	`)
	return err
}

func now() time.Time { return time.Now().UTC() }
