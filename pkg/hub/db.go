package hub

import (
	"database/sql"
	"fmt"
	"log"
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
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN docker INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN color TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN linear_issue_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN github_issue_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN shortcut_story_id TEXT NOT NULL DEFAULT ''`)
	// Migrate existing Shortcut story IDs from linear_issue_id to shortcut_story_id
	_, _ = db.Exec(`UPDATE claws SET shortcut_story_id = linear_issue_id WHERE linear_issue_id LIKE 'sc-%' AND shortcut_story_id = ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN llm_key TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN pipeline_stage TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN bootstrap_ok INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN bootstrap_status TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN bootstrap_diagnostic TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN factory_name TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN concurrency_group TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN external_trigger_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN restore_checkpoint_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN restored_from_checkpoint_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_comment_at TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN pr_conditions_fired INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_review_comment_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN format TEXT NOT NULL DEFAULT ''`)

	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS claw_checkpoints (
		id                    TEXT PRIMARY KEY,
		tenant_id             TEXT NOT NULL,
		claw_id               TEXT NOT NULL,
		status                TEXT NOT NULL DEFAULT 'creating',
		reason                TEXT NOT NULL DEFAULT '',
		created_by            TEXT NOT NULL DEFAULT 'hub',
		provider              TEXT NOT NULL DEFAULT '',
		provider_id_at_create TEXT NOT NULL DEFAULT '',
		manifest_sha256       TEXT NOT NULL DEFAULT '',
		manifest_path         TEXT NOT NULL DEFAULT '',
		root_tree_sha256      TEXT NOT NULL DEFAULT '',
		message_tree_sha256   TEXT NOT NULL DEFAULT '',
		workspace_tree_sha256 TEXT NOT NULL DEFAULT '',
		message_count         INTEGER NOT NULL DEFAULT 0,
		pr_count              INTEGER NOT NULL DEFAULT 0,
		repo_count            INTEGER NOT NULL DEFAULT 0,
		error                 TEXT NOT NULL DEFAULT '',
		created_at            DATETIME NOT NULL,
		completed_at          DATETIME
	)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_claw_checkpoints_claw ON claw_checkpoints(claw_id, created_at)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_claw_checkpoints_status ON claw_checkpoints(status, created_at)`)

	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS ssh_known_hosts (
		host          TEXT PRIMARY KEY,
		key_type      TEXT NOT NULL,
		key_data      TEXT NOT NULL,
		fingerprint   TEXT NOT NULL,
		first_seen_at DATETIME NOT NULL,
		last_seen_at  DATETIME NOT NULL
	)`)

	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS factory_triggers (
		id             TEXT PRIMARY KEY,
		factory_name   TEXT NOT NULL,
		integration    TEXT NOT NULL,
		trigger_key    TEXT NOT NULL,
		trigger_source TEXT NOT NULL DEFAULT '',
		trigger_payload TEXT NOT NULL DEFAULT '{}',
		claw_id        TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL DEFAULT 'claimed',
		first_seen_at  DATETIME NOT NULL,
		last_seen_at   DATETIME NOT NULL,
		created_at     DATETIME NOT NULL,
		updated_at     DATETIME NOT NULL
	)`)
	_, _ = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_factory_triggers_key ON factory_triggers(factory_name, integration, trigger_key)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_factory_triggers_claw ON factory_triggers(claw_id)`)
	_, _ = db.Exec(`
		INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at)
		SELECT lower(hex(randomblob(16))), factory_name, 'linear', 'linear:' || linear_issue_id, 'migration', '{}', id, 'active', created_at, created_at, created_at, created_at
		  FROM claws
		 WHERE factory_name != '' AND linear_issue_id != '' AND status != 'deleted'`)
	_, _ = db.Exec(`
		INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at)
		SELECT lower(hex(randomblob(16))), factory_name, 'github-issues', 'github-issues:' || github_issue_id, 'migration', '{}', id, 'active', created_at, created_at, created_at, created_at
		  FROM claws
		 WHERE factory_name != '' AND github_issue_id != '' AND status != 'deleted'`)
	_, _ = db.Exec(`
		INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at)
		SELECT lower(hex(randomblob(16))), factory_name, 'shortcut', 'shortcut:' || shortcut_story_id, 'migration', '{}', id, 'active', created_at, created_at, created_at, created_at
		  FROM claws
		 WHERE factory_name != '' AND shortcut_story_id != '' AND status != 'deleted'`)

	// Factory analytics — persistent metrics table
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS factory_analytics (
		id           TEXT PRIMARY KEY,
		factory_name TEXT NOT NULL,
		issue_id     TEXT NOT NULL DEFAULT '',
		claw_id      TEXT NOT NULL DEFAULT '',
		action       TEXT NOT NULL,  -- 'claw_created', 'claw_terminated', 'error', 'pr_opened', 'pr_merged', 'pr_closed', 'done_signal'
		detail       TEXT NOT NULL DEFAULT '',
		result       TEXT NOT NULL DEFAULT '', -- 'success', 'failure', 'timeout', 'cancelled'
		created_at   DATETIME NOT NULL
	)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_factory_analytics_factory ON factory_analytics(factory_name, created_at)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_factory_analytics_action ON factory_analytics(action, created_at)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_factory_analytics_claw ON factory_analytics(claw_id)`)

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
		docker           INTEGER NOT NULL DEFAULT 0,
		tags             TEXT NOT NULL DEFAULT '[]',
		color            TEXT NOT NULL DEFAULT '',
		linear_issue_id  TEXT NOT NULL DEFAULT '',
		github_issue_id  TEXT NOT NULL DEFAULT '',
		shortcut_story_id TEXT NOT NULL DEFAULT '',
		llm_key          TEXT NOT NULL DEFAULT '',
		pipeline_stage   TEXT NOT NULL DEFAULT '',
		bootstrap_ok        INTEGER NOT NULL DEFAULT 0,
		bootstrap_status    TEXT NOT NULL DEFAULT '',
		bootstrap_diagnostic TEXT NOT NULL DEFAULT '',
		factory_name     TEXT NOT NULL DEFAULT '',
		concurrency_group TEXT NOT NULL DEFAULT '',
		external_trigger_id TEXT NOT NULL DEFAULT '',
		restore_checkpoint_id TEXT NOT NULL DEFAULT '',
		restored_from_checkpoint_id TEXT NOT NULL DEFAULT ''
	);



	CREATE TABLE IF NOT EXISTS messages (
		id         TEXT PRIMARY KEY,
		claw_id    TEXT NOT NULL REFERENCES claws(id),
		tenant_id  TEXT NOT NULL,
		role       TEXT NOT NULL,
		content    TEXT NOT NULL,
		format     TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS factory_triggers (
		id             TEXT PRIMARY KEY,
		factory_name   TEXT NOT NULL,
		integration    TEXT NOT NULL,
		trigger_key    TEXT NOT NULL,
		trigger_source TEXT NOT NULL DEFAULT '',
		trigger_payload TEXT NOT NULL DEFAULT '{}',
		claw_id        TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL DEFAULT 'claimed',
		first_seen_at  DATETIME NOT NULL,
		last_seen_at   DATETIME NOT NULL,
		created_at     DATETIME NOT NULL,
		updated_at     DATETIME NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_factory_triggers_key ON factory_triggers(factory_name, integration, trigger_key);
	CREATE INDEX IF NOT EXISTS idx_factory_triggers_claw ON factory_triggers(claw_id);

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
		last_comment_id INTEGER NOT NULL DEFAULT 0, -- last bugbot/pipeline comment ID seen
		last_comment_at TEXT NOT NULL DEFAULT '', -- timestamp of last seen comment
		last_review_comment_id INTEGER NOT NULL DEFAULT 0, -- last PR review comment ID seen
		pr_conditions_fired INTEGER NOT NULL DEFAULT 0,
		created_at  DATETIME NOT NULL,
		UNIQUE(claw_id, pr_url)
	);

	CREATE TABLE IF NOT EXISTS claw_checkpoints (
		id                    TEXT PRIMARY KEY,
		tenant_id             TEXT NOT NULL,
		claw_id               TEXT NOT NULL,
		status                TEXT NOT NULL DEFAULT 'creating',
		reason                TEXT NOT NULL DEFAULT '',
		created_by            TEXT NOT NULL DEFAULT 'hub',
		provider              TEXT NOT NULL DEFAULT '',
		provider_id_at_create TEXT NOT NULL DEFAULT '',
		manifest_sha256       TEXT NOT NULL DEFAULT '',
		manifest_path         TEXT NOT NULL DEFAULT '',
		root_tree_sha256      TEXT NOT NULL DEFAULT '',
		message_tree_sha256   TEXT NOT NULL DEFAULT '',
		workspace_tree_sha256 TEXT NOT NULL DEFAULT '',
		message_count         INTEGER NOT NULL DEFAULT 0,
		pr_count              INTEGER NOT NULL DEFAULT 0,
		repo_count            INTEGER NOT NULL DEFAULT 0,
		error                 TEXT NOT NULL DEFAULT '',
		created_at            DATETIME NOT NULL,
		completed_at          DATETIME
	);
	CREATE INDEX IF NOT EXISTS idx_claw_checkpoints_claw ON claw_checkpoints(claw_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_claw_checkpoints_status ON claw_checkpoints(status, created_at);

	CREATE TABLE IF NOT EXISTS ssh_known_hosts (
		host          TEXT PRIMARY KEY,
		key_type      TEXT NOT NULL,
		key_data      TEXT NOT NULL,
		fingerprint   TEXT NOT NULL,
		first_seen_at DATETIME NOT NULL,
		last_seen_at  DATETIME NOT NULL
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

	-- v9: pipeline_outputs table for workflow script output capture
	CREATE TABLE IF NOT EXISTS pipeline_outputs (
		claw_id      TEXT NOT NULL,
		stage_id     TEXT NOT NULL,
		output_name  TEXT NOT NULL,
		exit_code    INTEGER NOT NULL DEFAULT 0,
		stdout       TEXT NOT NULL DEFAULT '',
		stderr       TEXT NOT NULL DEFAULT '',
		parsed_json  TEXT NOT NULL DEFAULT '{}',
		created_at   DATETIME NOT NULL,
		PRIMARY KEY (claw_id, output_name)
	);
	CREATE INDEX IF NOT EXISTS idx_pipeline_outputs_claw ON pipeline_outputs(claw_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_pipeline_outputs_stage ON pipeline_outputs(claw_id, stage_id);
	`)
	return err
}

// pruneFactoryAnalytics deletes factory_analytics rows older than 1 year.
// Should be called periodically (e.g. daily) from a background goroutine.
func pruneFactoryAnalytics(db *sql.DB) {
	_, err := db.Exec(`DELETE FROM factory_analytics WHERE created_at < datetime('now', '-1 year')`)
	if err != nil {
		log.Printf("[db] factory analytics prune error: %v", err)
	}
}

func now() time.Time { return time.Now().UTC() }
