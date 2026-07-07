package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// legacyBaseline upgrades a pre-versioning database to the exact 0001 schema
// and records version 1 in schema_migrations without executing 0001_init.sql
// as a migration. It re-runs, one last time, the idempotent statements older
// releases executed at every boot, so databases created by any previous
// version (including ones missing recently added columns) end up at the
// baseline schema. Everything runs in a single transaction: on failure the
// database is untouched and boot aborts.
func legacyBaseline(db *sql.DB, baselineName string) error {
	initSQL, err := migrationsFS.ReadFile("migrations/" + baselineName)
	if err != nil {
		return fmt.Errorf("read baseline migration: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("legacy baseline: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Base schema first: every CREATE is IF NOT EXISTS, so this is a no-op
	// for tables that already exist and creates any table added after the
	// database was first initialized. It must run before the ALTER TABLE
	// statements so that on missing tables those fail with "duplicate column
	// name" (ignorable) rather than "no such table".
	if _, err := tx.Exec(string(initSQL)); err != nil {
		return fmt.Errorf("legacy baseline: apply base schema: %w", err)
	}

	// Add columns that may be missing from databases created by older
	// versions. SQLite doesn't support IF NOT EXISTS on ALTER TABLE, so
	// "duplicate column name" is expected and ignored; any other error
	// aborts boot.
	for _, stmt := range legacyColumnAdds {
		if err := execIgnoreDuplicate(tx, stmt); err != nil {
			return fmt.Errorf("legacy baseline: %w", err)
		}
	}

	// Data migrations. These are idempotent by construction (guarded UPDATE,
	// INSERT OR IGNORE) but any execution error is a real failure.
	for _, stmt := range legacyDataMigrations {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("legacy baseline: statement %q: %w", summarizeStmt(stmt), err)
		}
	}

	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(1, ?, ?)`, baselineName, time.Now().UTC()); err != nil {
		return fmt.Errorf("legacy baseline: record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("legacy baseline: commit: %w", err)
	}
	return nil
}

// legacyColumnAdds is frozen: the columns are all part of 0001_init.sql, and
// any new column belongs in a new numbered migration, not here.
var legacyColumnAdds = []string{
	`ALTER TABLE claws ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN provider_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN default_model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN template_files TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE claws ADD COLUMN ssh_host TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN ssh_port INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE claws ADD COLUMN ssh_user TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN github_installation_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE claws ADD COLUMN github_repos TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN linear_workspace TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN nix INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE claws ADD COLUMN docker INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE claws ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE claws ADD COLUMN color TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN linear_issue_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN github_issue_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN shortcut_story_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN jira_issue_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN llm_key TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN pipeline_stage TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN bootstrap_ok INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE claws ADD COLUMN bootstrap_status TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN bootstrap_diagnostic TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN factory_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN concurrency_group TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN external_trigger_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN restore_checkpoint_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN restored_from_checkpoint_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN task_run_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claws ADD COLUMN workflow_volumes TEXT NOT NULL DEFAULT '[]'`,
	`ALTER TABLE claws ADD COLUMN trigger_actor_json TEXT NOT NULL DEFAULT '{}'`,
	`ALTER TABLE claw_prs ADD COLUMN last_comment_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE claw_prs ADD COLUMN pr_conditions_fired INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE claw_prs ADD COLUMN last_review_comment_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE claw_prs ADD COLUMN last_review_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE messages ADD COLUMN format TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE factory_triggers ADD COLUMN task_run_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE volume_leases ADD COLUMN access_token TEXT NOT NULL DEFAULT ''`,
}

// legacyDataMigrations is frozen for the same reason as legacyColumnAdds.
var legacyDataMigrations = []string{
	// Migrate existing Shortcut story IDs from linear_issue_id to shortcut_story_id
	`UPDATE claws SET shortcut_story_id = linear_issue_id WHERE linear_issue_id LIKE 'sc-%' AND shortcut_story_id = ''`,
	`
	INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at)
	SELECT lower(hex(randomblob(16))), factory_name, 'linear', 'linear:' || linear_issue_id, 'migration', '{}', id, 'active', created_at, created_at, created_at, created_at
	  FROM claws
	 WHERE factory_name != '' AND linear_issue_id != '' AND status != 'deleted'`,
	`
	INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at)
	SELECT lower(hex(randomblob(16))), factory_name, 'github-issues', 'github-issues:' || github_issue_id, 'migration', '{}', id, 'active', created_at, created_at, created_at, created_at
	  FROM claws
	 WHERE factory_name != '' AND github_issue_id != '' AND status != 'deleted'`,
	`
	INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at)
	SELECT lower(hex(randomblob(16))), factory_name, 'shortcut', 'shortcut:' || shortcut_story_id, 'migration', '{}', id, 'active', created_at, created_at, created_at, created_at
	  FROM claws
	 WHERE factory_name != '' AND shortcut_story_id != '' AND status != 'deleted'`,
	`
	INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at)
	SELECT lower(hex(randomblob(16))), factory_name, 'jira', 'jira:' || jira_issue_id, 'migration', '{}', id, 'active', created_at, created_at, created_at, created_at
	  FROM claws
	 WHERE factory_name != '' AND jira_issue_id != '' AND status != 'deleted'`,
}

// execIgnoreDuplicate runs an ALTER TABLE ... ADD COLUMN statement and treats
// "duplicate column name" as success (the column already exists — SQLite has
// no IF NOT EXISTS for ALTER TABLE). Any other error is returned so a real
// migration failure (full disk, locked or read-only database, corrupt schema)
// aborts boot instead of being silently ignored.
func execIgnoreDuplicate(tx *sql.Tx, stmt string) error {
	if _, err := tx.Exec(stmt); err != nil {
		if strings.Contains(err.Error(), "duplicate column name") {
			return nil
		}
		return fmt.Errorf("statement %q: %w", summarizeStmt(stmt), err)
	}
	return nil
}

// summarizeStmt compacts a SQL statement for error messages: whitespace is
// collapsed and long statements are truncated.
func summarizeStmt(stmt string) string {
	s := strings.Join(strings.Fields(stmt), " ")
	const max = 120
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
