package hub

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite, no CGO required
)

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
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
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN jira_issue_id TEXT NOT NULL DEFAULT ''`)
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
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN task_run_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN issue_title TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN workflow_volumes TEXT NOT NULL DEFAULT '[]'`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN trigger_actor_json TEXT NOT NULL DEFAULT '{}'`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN stop_comment_pending INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN no_progress_paused INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN preview_port INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN preview_url TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN preview_label TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN preview_ready INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN preview_ttl_seconds INTEGER NOT NULL DEFAULT 1800`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN preview_expires_at INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_comment_at TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN pr_conditions_fired INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN permanent_failure_count INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_review_comment_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_review_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN format TEXT NOT NULL DEFAULT ''`)
	if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN delivered_at DATETIME`); err == nil {
		// A failed backfill must abort startup: the column now exists, so a
		// later start would skip this branch and replay all history as pending.
		if _, backfillErr := db.Exec(`UPDATE messages SET delivered_at = created_at`); backfillErr != nil {
			return fmt.Errorf("backfill messages.delivered_at: %w", backfillErr)
		}
	}
	_, _ = db.Exec(`ALTER TABLE factory_triggers ADD COLUMN task_run_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE factory_triggers ADD COLUMN retry_count INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_attempts ADD COLUMN restored_checkpoint_id TEXT`)
	_, _ = db.Exec(`ALTER TABLE task_runs ADD COLUMN issue_title TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE task_runs ADD COLUMN issue_created_at INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_summaries ADD COLUMN issue_title TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE task_run_summaries ADD COLUMN issue_created_at INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_summaries ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_summaries ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_summaries ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_summaries ADD COLUMN estimated_cost_usd REAL NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_summaries ADD COLUMN usage_updated_at INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_usage ADD COLUMN committed_input_tokens INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_usage ADD COLUMN committed_output_tokens INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_usage ADD COLUMN committed_total_tokens INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_usage ADD COLUMN committed_cost_usd REAL NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE task_run_usage ADD COLUMN usage_day TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE task_run_prs ADD COLUMN last_agent_head_sha TEXT NOT NULL DEFAULT ''`)

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
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS claw_turn_observations (
		id                   TEXT PRIMARY KEY,
		claw_id              TEXT NOT NULL REFERENCES claws(id) ON DELETE CASCADE,
		response             TEXT NOT NULL,
		progress_fingerprint TEXT NOT NULL,
		created_at           DATETIME NOT NULL
	)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_claw_turn_observations_claw ON claw_turn_observations(claw_id, created_at)`)

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
		task_run_id    TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL DEFAULT 'claimed',
		retry_count     INTEGER NOT NULL DEFAULT 0,
		first_seen_at  DATETIME NOT NULL,
		last_seen_at   DATETIME NOT NULL,
		created_at     DATETIME NOT NULL,
		updated_at     DATETIME NOT NULL
	)`)
	_, _ = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_factory_triggers_key ON factory_triggers(factory_name, integration, trigger_key)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_factory_triggers_claw ON factory_triggers(claw_id)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_factory_triggers_integration_status ON factory_triggers(integration, status, claw_id)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS integration_poll_state (integration TEXT PRIMARY KEY, last_success_at DATETIME NOT NULL)`)
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
	_, _ = db.Exec(`
		INSERT OR IGNORE INTO factory_triggers(id, factory_name, integration, trigger_key, trigger_source, trigger_payload, claw_id, status, first_seen_at, last_seen_at, created_at, updated_at)
		SELECT lower(hex(randomblob(16))), factory_name, 'jira', 'jira:' || jira_issue_id, 'migration', '{}', id, 'active', created_at, created_at, created_at, created_at
		  FROM claws
		 WHERE factory_name != '' AND jira_issue_id != '' AND status != 'deleted'`)

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

	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS volume_leases (
		id              TEXT PRIMARY KEY,
		volume_id       TEXT NOT NULL,
		repo            TEXT NOT NULL,
		tag             TEXT NOT NULL,
		claw_id         TEXT NOT NULL,
		access_token    TEXT NOT NULL DEFAULT '',
		mode            TEXT NOT NULL CHECK(mode IN ('ro','rw')),
		mount           TEXT NOT NULL DEFAULT '',
		manifest_digest TEXT NOT NULL DEFAULT '',
		acquired_at     DATETIME NOT NULL,
		expires_at      DATETIME NOT NULL,
		heartbeat_at    DATETIME NOT NULL,
		released_at     DATETIME
	)`)
	_, _ = db.Exec(`ALTER TABLE volume_leases ADD COLUMN access_token TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_volume_leases_volume_active ON volume_leases(volume_id, released_at, expires_at)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_volume_leases_claw ON volume_leases(claw_id, released_at)`)

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
		jira_issue_id    TEXT NOT NULL DEFAULT '',
		issue_title      TEXT NOT NULL DEFAULT '',
		llm_key          TEXT NOT NULL DEFAULT '',
		pipeline_stage   TEXT NOT NULL DEFAULT '',
		bootstrap_ok        INTEGER NOT NULL DEFAULT 0,
		bootstrap_status    TEXT NOT NULL DEFAULT '',
		bootstrap_diagnostic TEXT NOT NULL DEFAULT '',
		factory_name     TEXT NOT NULL DEFAULT '',
		concurrency_group TEXT NOT NULL DEFAULT '',
		external_trigger_id TEXT NOT NULL DEFAULT '',
		restore_checkpoint_id TEXT NOT NULL DEFAULT '',
		restored_from_checkpoint_id TEXT NOT NULL DEFAULT '',
		task_run_id TEXT NOT NULL DEFAULT '',
		workflow_volumes TEXT NOT NULL DEFAULT '[]',
		trigger_actor_json TEXT NOT NULL DEFAULT '{}',
		stop_comment_pending INTEGER NOT NULL DEFAULT 0,
		no_progress_paused INTEGER NOT NULL DEFAULT 0,
		preview_port INTEGER NOT NULL DEFAULT 0,
		preview_url TEXT NOT NULL DEFAULT '',
		preview_label TEXT NOT NULL DEFAULT '',
		preview_ready INTEGER NOT NULL DEFAULT 0,
		preview_ttl_seconds INTEGER NOT NULL DEFAULT 1800,
		preview_expires_at INTEGER NOT NULL DEFAULT 0
	);



	CREATE TABLE IF NOT EXISTS messages (
		id         TEXT PRIMARY KEY,
		claw_id    TEXT NOT NULL REFERENCES claws(id),
		tenant_id  TEXT NOT NULL,
		role       TEXT NOT NULL,
		content    TEXT NOT NULL,
		format     TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		delivered_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS claw_turn_observations (
		id                   TEXT PRIMARY KEY,
		claw_id              TEXT NOT NULL REFERENCES claws(id) ON DELETE CASCADE,
		response             TEXT NOT NULL,
		progress_fingerprint TEXT NOT NULL,
		created_at           DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_claw_turn_observations_claw ON claw_turn_observations(claw_id, created_at);

	CREATE TABLE IF NOT EXISTS factory_triggers (
		id             TEXT PRIMARY KEY,
		factory_name   TEXT NOT NULL,
		integration    TEXT NOT NULL,
		trigger_key    TEXT NOT NULL,
		trigger_source TEXT NOT NULL DEFAULT '',
		trigger_payload TEXT NOT NULL DEFAULT '{}',
		claw_id        TEXT NOT NULL DEFAULT '',
		task_run_id    TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL DEFAULT 'claimed',
		retry_count     INTEGER NOT NULL DEFAULT 0,
		first_seen_at  DATETIME NOT NULL,
		last_seen_at   DATETIME NOT NULL,
		created_at     DATETIME NOT NULL,
		updated_at     DATETIME NOT NULL
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_factory_triggers_key ON factory_triggers(factory_name, integration, trigger_key);
	CREATE INDEX IF NOT EXISTS idx_factory_triggers_claw ON factory_triggers(claw_id);
	CREATE INDEX IF NOT EXISTS idx_factory_triggers_integration_status ON factory_triggers(integration, status, claw_id);
	CREATE TABLE IF NOT EXISTS integration_poll_state (
		integration TEXT PRIMARY KEY,
		last_success_at DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_messages_claw ON messages(claw_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_messages_pending ON messages(claw_id, created_at) WHERE delivered_at IS NULL;
	CREATE INDEX IF NOT EXISTS idx_claws_tenant  ON claws(tenant_id);

	CREATE TABLE IF NOT EXISTS task_runs (
		id                    TEXT PRIMARY KEY,
		tenant_id             TEXT NOT NULL,
		initial_attempt_id    TEXT NOT NULL,
		current_attempt_id    TEXT NOT NULL DEFAULT '',
		attempt_count         INTEGER NOT NULL DEFAULT 1 CHECK(attempt_count >= 1),
		run_kind              TEXT NOT NULL CHECK(run_kind IN ('code_task','pr_task')),
		owner_type            TEXT NOT NULL CHECK(owner_type IN ('workflow','factory','manual','external')),
		workspace_name        TEXT NOT NULL DEFAULT '',
		workflow_name         TEXT NOT NULL DEFAULT '',
		factory_name          TEXT NOT NULL DEFAULT '',
		owner_id              TEXT NOT NULL DEFAULT '',
		owner_display_name    TEXT NOT NULL DEFAULT '',
		integration           TEXT NOT NULL DEFAULT '',
		integration_workspace TEXT NOT NULL DEFAULT '',
		trigger_id            TEXT NOT NULL DEFAULT '',
		external_trigger_id   TEXT NOT NULL DEFAULT '',
		issue_id              TEXT NOT NULL DEFAULT '',
		issue_title           TEXT NOT NULL DEFAULT '',
		issue_created_at      INTEGER NOT NULL DEFAULT 0,
		claw_id               TEXT NOT NULL DEFAULT '',
		model                 TEXT NOT NULL DEFAULT '',
		llm_key               TEXT NOT NULL DEFAULT '',
		tags                  TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(tags) AND json_type(tags) = 'array'),
		analytics_enabled     INTEGER NOT NULL DEFAULT 1 CHECK(analytics_enabled IN (0,1)),
		requires_pr           INTEGER NOT NULL DEFAULT 1 CHECK(requires_pr IN (0,1)),
		excluded_reason       TEXT NOT NULL DEFAULT '',
		timeout_at            INTEGER NOT NULL DEFAULT 0,
		created_at            INTEGER NOT NULL,
		updated_at            INTEGER NOT NULL,
		UNIQUE(tenant_id, initial_attempt_id)
	);
	CREATE INDEX IF NOT EXISTS idx_task_runs_tenant_created ON task_runs(tenant_id, created_at DESC, id DESC);
	CREATE INDEX IF NOT EXISTS idx_task_runs_claw ON task_runs(claw_id);
	CREATE INDEX IF NOT EXISTS idx_task_runs_trigger ON task_runs(trigger_id);
	CREATE INDEX IF NOT EXISTS idx_task_runs_owner ON task_runs(workspace_name, owner_type, owner_display_name, created_at DESC);

	CREATE TABLE IF NOT EXISTS task_run_usage (
		id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, run_id TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
		session_key TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', model_provider TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
		committed_input_tokens INTEGER NOT NULL DEFAULT 0, committed_output_tokens INTEGER NOT NULL DEFAULT 0, committed_total_tokens INTEGER NOT NULL DEFAULT 0, committed_cost_usd REAL NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER, cache_write_tokens INTEGER, estimated_cost_usd REAL, cost_source TEXT NOT NULL DEFAULT 'gateway' CHECK(cost_source IN ('gateway','hub_pricing')),
		usage_day TEXT NOT NULL DEFAULT '',
		first_seen_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(tenant_id, run_id, session_key)
	);
	CREATE TABLE IF NOT EXISTS usage_daily (
		tenant_id TEXT NOT NULL, day TEXT NOT NULL, workspace_name TEXT NOT NULL DEFAULT '', factory_name TEXT NOT NULL DEFAULT '', workflow_name TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL,
		PRIMARY KEY(tenant_id, day, workspace_name, factory_name, workflow_name, model)
	);
	CREATE TABLE IF NOT EXISTS model_prices (model TEXT PRIMARY KEY, input_cost_per_token REAL NOT NULL, output_cost_per_token REAL NOT NULL, cache_read_cost_per_token REAL NOT NULL, cache_write_cost_per_token REAL NOT NULL, source TEXT NOT NULL, updated_at INTEGER NOT NULL);

	CREATE TABLE IF NOT EXISTS task_run_attempts (
		id             TEXT PRIMARY KEY,
		tenant_id      TEXT NOT NULL,
		run_id         TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
		attempt_id     TEXT NOT NULL,
		attempt_number INTEGER NOT NULL CHECK(attempt_number > 0),
		trigger_id     TEXT NOT NULL DEFAULT '',
		claw_id        TEXT NOT NULL DEFAULT '',
		status         TEXT NOT NULL DEFAULT 'running' CHECK(status IN ('running','succeeded','failed')),
		failure_type   TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('','creation_failed','provision_failed','bootstrap_failed','agent_stopped','manual_stop_before_delivery','done_without_pr','no_pr','pr_closed_unmerged','timeout','provider_lost','permission_or_auth_failed','unknown')),
		restored_checkpoint_id TEXT,
		started_at     INTEGER NOT NULL,
		finished_at    INTEGER NOT NULL DEFAULT 0,
		created_at     INTEGER NOT NULL,
		updated_at     INTEGER NOT NULL,
		UNIQUE(tenant_id, attempt_id),
		UNIQUE(tenant_id, run_id, attempt_number)
	);
	CREATE INDEX IF NOT EXISTS idx_task_run_attempts_run_number ON task_run_attempts(run_id, attempt_number);

	CREATE TABLE IF NOT EXISTS task_run_events (
		id                 TEXT PRIMARY KEY,
		tenant_id          TEXT NOT NULL,
		run_id             TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
		attempt_id         TEXT NOT NULL DEFAULT '',
		event_key          TEXT NOT NULL,
		source             TEXT NOT NULL DEFAULT 'hub' CHECK(source IN ('github','linear','shortcut','elasticclaw','hub','provider','agent','unknown')),
		source_event_id    TEXT NOT NULL DEFAULT '',
		source_delivery_id TEXT NOT NULL DEFAULT '',
		event_type         TEXT NOT NULL CHECK(event_type IN (
			'task_start','task_completed','run_claimed','run_queued','provision_started','claw_created','agent_started',
			'creation_failed','provision_failed','bootstrap_failed','model_selected','agent_stopped',
			'manual_stop_before_delivery','provider_lost','done_without_pr','permission_or_auth_failed',
			'timeout','unknown_failure','pr_associated','pr_opened','pr_closed_unmerged','pr_merged',
			'approval_only_pr_review','human_requested_changes','human_review_comment','human_pr_comment',
			'human_manual_code_push','human_tracker_update','human_dashboard_message',
			'human_manual_stop_or_resume','human_settings_or_status_change',
			'unknown_human_interaction','pr_replaced','correction','retraction'
		)),
		event_time         INTEGER NOT NULL,
		observed_at        INTEGER NOT NULL,
		actor_type         TEXT NOT NULL DEFAULT 'unknown' CHECK(actor_type IN ('agent','human','bot','system','unknown')),
		actor_source       TEXT NOT NULL DEFAULT '',
		actor_id           TEXT NOT NULL DEFAULT '',
		actor_login        TEXT NOT NULL DEFAULT '',
		actor_display_name TEXT NOT NULL DEFAULT '',
		actor_classification_reason TEXT NOT NULL DEFAULT '',
		interaction_role   TEXT NOT NULL DEFAULT '' CHECK(interaction_role IN ('','allowed_start','allowed_approval','allowed_merge','warning','neutral','terminal')),
		target_type        TEXT NOT NULL DEFAULT '',
		target_id          TEXT NOT NULL DEFAULT '',
		target_url         TEXT NOT NULL DEFAULT '',
		target_label       TEXT NOT NULL DEFAULT '',
		warning_type       TEXT NOT NULL DEFAULT '',
		failure_type       TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('','creation_failed','provision_failed','bootstrap_failed','agent_stopped','manual_stop_before_delivery','done_without_pr','no_pr','pr_closed_unmerged','timeout','provider_lost','permission_or_auth_failed','unknown')),
		detail             TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(detail) AND json_type(detail) = 'object'),
		created_at         INTEGER NOT NULL
	);
	-- task_run_events is read-heavy for dashboard filters/details; monitor write latency before adding more indexes.
	CREATE UNIQUE INDEX IF NOT EXISTS idx_task_run_events_tenant_key ON task_run_events(tenant_id, run_id, event_key);
	CREATE INDEX IF NOT EXISTS idx_task_run_events_run_time ON task_run_events(run_id, event_time, id);
	CREATE INDEX IF NOT EXISTS idx_task_run_events_type_time ON task_run_events(event_type, event_time);
	CREATE INDEX IF NOT EXISTS idx_task_run_events_tenant_run_time ON task_run_events(tenant_id, run_id, event_time, observed_at, event_key);
	CREATE INDEX IF NOT EXISTS idx_task_run_events_source_event ON task_run_events(tenant_id, source, source_event_id);
	CREATE INDEX IF NOT EXISTS idx_task_run_events_observed ON task_run_events(tenant_id, observed_at);

	CREATE TABLE IF NOT EXISTS task_run_prs (
		id              TEXT PRIMARY KEY,
		tenant_id       TEXT NOT NULL,
		run_id          TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
		repo            TEXT NOT NULL,
		pr_number       INTEGER NOT NULL CHECK(pr_number > 0),
		pr_url          TEXT NOT NULL DEFAULT '',
		head_sha        TEXT NOT NULL DEFAULT '',
		head_branch     TEXT NOT NULL DEFAULT '',
		last_agent_head_sha TEXT NOT NULL DEFAULT '',
		base_branch     TEXT NOT NULL DEFAULT '',
		state           TEXT NOT NULL DEFAULT 'unknown' CHECK(state IN ('open','closed','unknown')),
		merged          INTEGER NOT NULL DEFAULT 0 CHECK(merged IN (0,1)),
		opened_at       INTEGER NOT NULL DEFAULT 0,
		closed_at       INTEGER NOT NULL DEFAULT 0,
		merged_at       INTEGER NOT NULL DEFAULT 0,
		merged_by_login TEXT NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		UNIQUE(tenant_id, run_id, repo, pr_number)
	);
	CREATE INDEX IF NOT EXISTS idx_task_run_prs_run ON task_run_prs(run_id, state, merged);
	CREATE INDEX IF NOT EXISTS idx_task_run_prs_repo_pr ON task_run_prs(repo, pr_number);
	CREATE INDEX IF NOT EXISTS idx_task_run_prs_tenant_run ON task_run_prs(tenant_id, run_id, state, merged);
	CREATE INDEX IF NOT EXISTS idx_task_run_prs_tenant_merged ON task_run_prs(tenant_id, run_id, merged_at);

	CREATE TABLE IF NOT EXISTS task_run_summaries (
		id                      TEXT PRIMARY KEY,
		tenant_id               TEXT NOT NULL,
		run_id                  TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
		initial_attempt_id      TEXT NOT NULL DEFAULT '',
		current_attempt_id      TEXT NOT NULL DEFAULT '',
		status                  TEXT NOT NULL CHECK(status IN ('running','clean','human_in_the_loop','warning','failed')),
		phase                   TEXT NOT NULL CHECK(phase IN ('claimed','queued','provisioning','agent_running','pr_opened','waiting_for_merge','terminal')),
		attempt_count           INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
		owner_type              TEXT NOT NULL DEFAULT '',
		workspace_name          TEXT NOT NULL DEFAULT '',
		workflow_name           TEXT NOT NULL DEFAULT '',
		factory_name            TEXT NOT NULL DEFAULT '',
		owner_id                TEXT NOT NULL DEFAULT '',
		owner_display_name      TEXT NOT NULL DEFAULT '',
		run_kind                TEXT NOT NULL DEFAULT 'pr_task' CHECK(run_kind IN ('code_task','pr_task')),
		integration             TEXT NOT NULL DEFAULT '',
		integration_workspace   TEXT NOT NULL DEFAULT '',
		issue_id                TEXT NOT NULL DEFAULT '',
		issue_title             TEXT NOT NULL DEFAULT '',
		issue_created_at        INTEGER NOT NULL DEFAULT 0,
		claw_id                 TEXT NOT NULL DEFAULT '',
		model                   TEXT NOT NULL DEFAULT '',
		input_tokens            INTEGER NOT NULL DEFAULT 0,
		output_tokens           INTEGER NOT NULL DEFAULT 0,
		total_tokens            INTEGER NOT NULL DEFAULT 0,
		estimated_cost_usd      REAL NOT NULL DEFAULT 0,
		usage_updated_at        INTEGER NOT NULL DEFAULT 0,
		llm_key                 TEXT NOT NULL DEFAULT '',
		repo                    TEXT NOT NULL DEFAULT '',
		primary_pr_url          TEXT NOT NULL DEFAULT '',
		pr_count                INTEGER NOT NULL DEFAULT 0 CHECK(pr_count >= 0),
		open_pr_count           INTEGER NOT NULL DEFAULT 0 CHECK(open_pr_count >= 0),
		merged_pr_count         INTEGER NOT NULL DEFAULT 0 CHECK(merged_pr_count >= 0),
		closed_pr_count         INTEGER NOT NULL DEFAULT 0 CHECK(closed_pr_count >= 0),
		warning_types           TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(warning_types) AND json_type(warning_types) = 'array'),
		failure_type            TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('','creation_failed','provision_failed','bootstrap_failed','agent_stopped','manual_stop_before_delivery','done_without_pr','no_pr','pr_closed_unmerged','timeout','provider_lost','permission_or_auth_failed','unknown')),
		human_interaction_count INTEGER NOT NULL DEFAULT 0 CHECK(human_interaction_count >= 0),
		started_at              INTEGER NOT NULL,
		queued_at               INTEGER NOT NULL DEFAULT 0,
		provision_started_at    INTEGER NOT NULL DEFAULT 0,
		agent_started_at        INTEGER NOT NULL DEFAULT 0,
		pr_opened_at            INTEGER NOT NULL DEFAULT 0,
		merged_at               INTEGER NOT NULL DEFAULT 0,
		finished_at             INTEGER NOT NULL DEFAULT 0,
		timeout_at              INTEGER NOT NULL DEFAULT 0,
		last_event_at           INTEGER NOT NULL,
		materialized_at         INTEGER NOT NULL,
		updated_at              INTEGER NOT NULL,
		analytics_enabled       INTEGER NOT NULL DEFAULT 1 CHECK(analytics_enabled IN (0,1)),
		requires_pr             INTEGER NOT NULL DEFAULT 1 CHECK(requires_pr IN (0,1)),
		excluded_reason         TEXT NOT NULL DEFAULT '',
		UNIQUE(run_id),
		UNIQUE(tenant_id, run_id)
	);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_status ON task_run_summaries(tenant_id, status, started_at DESC);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_owner_started ON task_run_summaries(workspace_name, owner_type, owner_display_name, started_at DESC);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_run ON task_run_summaries(run_id);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_started_run ON task_run_summaries(tenant_id, started_at DESC, run_id DESC);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_workspace ON task_run_summaries(tenant_id, workspace_name, started_at DESC);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_workflow ON task_run_summaries(tenant_id, workspace_name, workflow_name, started_at DESC);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_factory ON task_run_summaries(tenant_id, factory_name, started_at DESC);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_model ON task_run_summaries(tenant_id, model, started_at DESC);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_repo ON task_run_summaries(tenant_id, repo, started_at DESC);
	CREATE INDEX IF NOT EXISTS idx_task_run_summaries_timeout ON task_run_summaries(tenant_id, timeout_at);

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
		last_review_id INTEGER NOT NULL DEFAULT 0, -- last top-level PR review ID seen
		pr_conditions_fired INTEGER NOT NULL DEFAULT 0,
		permanent_failure_count INTEGER NOT NULL DEFAULT 0,
		created_at  DATETIME NOT NULL,
		UNIQUE(claw_id, pr_url)
	);

	CREATE TABLE IF NOT EXISTS claw_pr_feedback_deliveries (
		claw_id       TEXT NOT NULL,
		feedback_type TEXT NOT NULL,
		github_id     INTEGER NOT NULL,
		created_at    DATETIME NOT NULL,
		PRIMARY KEY(claw_id, feedback_type, github_id)
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

	-- v10: pipeline_gate_results table for deterministic tool review gates
	CREATE TABLE IF NOT EXISTS pipeline_gate_results (
		claw_id      TEXT NOT NULL,
		stage_id     TEXT NOT NULL,
		output_name  TEXT NOT NULL,
		verdict      TEXT NOT NULL,  -- 'pass', 'fail', 'skipped', 'error'
		matched_path TEXT NOT NULL DEFAULT '',
		matched_value TEXT NOT NULL DEFAULT '',
		required     INTEGER NOT NULL DEFAULT 0,
		created_at   DATETIME NOT NULL,
		PRIMARY KEY (claw_id, stage_id)
	);
	CREATE INDEX IF NOT EXISTS idx_pipeline_gate_results_claw ON pipeline_gate_results(claw_id, created_at);

	-- v11: pipeline_stage_history tracks visited stages to prevent one-shot
	-- triggers (like output_matches) from re-firing on every message.
	CREATE TABLE IF NOT EXISTS pipeline_stage_history (
		claw_id    TEXT NOT NULL,
		stage_id   TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		PRIMARY KEY (claw_id, stage_id)
	);
	CREATE INDEX IF NOT EXISTS idx_pipeline_stage_history_claw ON pipeline_stage_history(claw_id, created_at);

	-- v12: workflow_runs table for cron and manual workflow execution history
	CREATE TABLE IF NOT EXISTS workflow_runs (
		id             TEXT PRIMARY KEY,
		tenant_id      TEXT NOT NULL DEFAULT '',
		workflow_name  TEXT NOT NULL,
		workspace_name TEXT NOT NULL,
		trigger_type   TEXT NOT NULL DEFAULT 'cron',  -- 'cron', 'manual'
		status         TEXT NOT NULL DEFAULT 'pending',  -- 'pending', 'running', 'completed', 'failed', 'skipped', 'timed_out', 'canceled'
		result         TEXT NOT NULL DEFAULT '',           -- 'success', 'failure', 'skipped', 'timed_out', 'canceled'
		claw_id        TEXT NOT NULL DEFAULT '',
		run_context    TEXT NOT NULL DEFAULT '{}',         -- JSON: trigger, workflow, repository info
		started_at     DATETIME,
		finished_at    DATETIME,
		created_at     DATETIME NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_workflow_runs_tenant ON workflow_runs(tenant_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_workflow_runs_workflow ON workflow_runs(tenant_id, workflow_name, workspace_name, created_at);
	CREATE INDEX IF NOT EXISTS idx_workflow_runs_status ON workflow_runs(tenant_id, status, created_at);
	CREATE INDEX IF NOT EXISTS idx_workflow_runs_claw ON workflow_runs(claw_id);
	`)
	if err != nil {
		return err
	}
	if err := rebuildTaskRunSummariesStatusV3(db); err != nil {
		return err
	}
	// Backfill rows that predate the usage_day column so cost corrections land
	// on the day the run's usage was last applied, not on the correction's day.
	if _, err := db.Exec(`UPDATE task_run_usage SET usage_day = strftime('%Y-%m-%d', updated_at/1000, 'unixepoch') WHERE usage_day = '' AND updated_at > 0`); err != nil {
		return fmt.Errorf("backfill task_run_usage.usage_day: %w", err)
	}
	if err := backfillTaskRunAnalyticsStatusV2(db); err != nil {
		return err
	}
	if err := backfillTaskRunAnalyticsStatusV3(db); err != nil {
		return err
	}
	if err := backfillTaskRunAgentStartedAtV1(db); err != nil {
		return err
	}
	for _, p := range []struct {
		model                          string
		in, out, cacheRead, cacheWrite float64
	}{
		{"claude-fable-5", 10, 50, 1, 12.5}, {"claude-opus-4-8", 5, 25, .5, 6.25}, {"claude-opus-4-7", 5, 25, .5, 6.25}, {"claude-opus-4-6", 5, 25, .5, 6.25},
		{"claude-sonnet-5", 3, 15, .3, 3.75}, {"claude-sonnet-4-6", 3, 15, .3, 3.75}, {"claude-haiku-4-5", 1, 5, .1, 1.25},
		{"gpt-5", 1.25, 10, .125, 0}, {"gpt-5-mini", .25, 2, .025, 0}, {"gpt-5-nano", .05, .40, .005, 0},
		{"gpt-5.1", 1.25, 10, .125, 0}, {"gpt-5.6", 1.25, 10, .125, 0},
		{"kimi-k2p7-code", 0.95, 4, .19, 0},
	} {
		// Upsert so price corrections in the static seed reach existing
		// databases; rows from other sources are left untouched.
		_, err = db.Exec(`INSERT INTO model_prices(model,input_cost_per_token,output_cost_per_token,cache_read_cost_per_token,cache_write_cost_per_token,source,updated_at) VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(model) DO UPDATE SET input_cost_per_token=excluded.input_cost_per_token,output_cost_per_token=excluded.output_cost_per_token,cache_read_cost_per_token=excluded.cache_read_cost_per_token,cache_write_cost_per_token=excluded.cache_write_cost_per_token,updated_at=excluded.updated_at WHERE model_prices.source='static'`, p.model, p.in/1e6, p.out/1e6, p.cacheRead/1e6, p.cacheWrite/1e6, "static", now().UnixMilli())
		if err != nil {
			return err
		}
	}
	return nil
}

func rebuildTaskRunSummariesStatusV3(db *sql.DB) error {
	var schema string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='task_run_summaries'`).Scan(&schema); err != nil {
		return fmt.Errorf("read task run summaries schema: %w", err)
	}
	if !strings.Contains(schema, "clean_success") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE task_run_summaries_new (
		id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, run_id TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
		initial_attempt_id TEXT NOT NULL DEFAULT '', current_attempt_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL CHECK(status IN ('running','clean','human_in_the_loop','warning','failed')),
		phase TEXT NOT NULL CHECK(phase IN ('claimed','queued','provisioning','agent_running','pr_opened','waiting_for_merge','terminal')),
		attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0), owner_type TEXT NOT NULL DEFAULT '', workspace_name TEXT NOT NULL DEFAULT '', workflow_name TEXT NOT NULL DEFAULT '', factory_name TEXT NOT NULL DEFAULT '', owner_id TEXT NOT NULL DEFAULT '', owner_display_name TEXT NOT NULL DEFAULT '', run_kind TEXT NOT NULL DEFAULT 'pr_task' CHECK(run_kind IN ('code_task','pr_task')), integration TEXT NOT NULL DEFAULT '', integration_workspace TEXT NOT NULL DEFAULT '', issue_id TEXT NOT NULL DEFAULT '', issue_title TEXT NOT NULL DEFAULT '', issue_created_at INTEGER NOT NULL DEFAULT 0, claw_id TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0, estimated_cost_usd REAL NOT NULL DEFAULT 0, usage_updated_at INTEGER NOT NULL DEFAULT 0, llm_key TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '', primary_pr_url TEXT NOT NULL DEFAULT '', pr_count INTEGER NOT NULL DEFAULT 0 CHECK(pr_count >= 0), open_pr_count INTEGER NOT NULL DEFAULT 0 CHECK(open_pr_count >= 0), merged_pr_count INTEGER NOT NULL DEFAULT 0 CHECK(merged_pr_count >= 0), closed_pr_count INTEGER NOT NULL DEFAULT 0 CHECK(closed_pr_count >= 0), warning_types TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(warning_types) AND json_type(warning_types) = 'array'), failure_type TEXT NOT NULL DEFAULT '' CHECK(failure_type IN ('','creation_failed','provision_failed','bootstrap_failed','agent_stopped','manual_stop_before_delivery','done_without_pr','no_pr','pr_closed_unmerged','timeout','provider_lost','permission_or_auth_failed','unknown')), human_interaction_count INTEGER NOT NULL DEFAULT 0 CHECK(human_interaction_count >= 0), started_at INTEGER NOT NULL, queued_at INTEGER NOT NULL DEFAULT 0, provision_started_at INTEGER NOT NULL DEFAULT 0, agent_started_at INTEGER NOT NULL DEFAULT 0, pr_opened_at INTEGER NOT NULL DEFAULT 0, merged_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0, timeout_at INTEGER NOT NULL DEFAULT 0, last_event_at INTEGER NOT NULL, materialized_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, analytics_enabled INTEGER NOT NULL DEFAULT 1 CHECK(analytics_enabled IN (0,1)), requires_pr INTEGER NOT NULL DEFAULT 1 CHECK(requires_pr IN (0,1)), excluded_reason TEXT NOT NULL DEFAULT '', UNIQUE(run_id), UNIQUE(tenant_id, run_id)
	)`); err != nil {
		return fmt.Errorf("create task run summaries v3: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO task_run_summaries_new SELECT id, tenant_id, run_id, initial_attempt_id, current_attempt_id, CASE status WHEN 'clean_success' THEN 'clean' WHEN 'warning_success' THEN 'warning' ELSE status END, phase, attempt_count, owner_type, workspace_name, workflow_name, factory_name, owner_id, owner_display_name, run_kind, integration, integration_workspace, issue_id, issue_title, issue_created_at, claw_id, model, input_tokens, output_tokens, total_tokens, estimated_cost_usd, usage_updated_at, llm_key, repo, primary_pr_url, pr_count, open_pr_count, merged_pr_count, closed_pr_count, warning_types, failure_type, human_interaction_count, started_at, queued_at, provision_started_at, agent_started_at, pr_opened_at, merged_at, finished_at, timeout_at, last_event_at, materialized_at, updated_at, analytics_enabled, requires_pr, excluded_reason FROM task_run_summaries`); err != nil {
		return fmt.Errorf("copy task run summaries v3: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE task_run_summaries; ALTER TABLE task_run_summaries_new RENAME TO task_run_summaries;
		CREATE INDEX idx_task_run_summaries_status ON task_run_summaries(tenant_id, status, started_at DESC);
		CREATE INDEX idx_task_run_summaries_owner_started ON task_run_summaries(workspace_name, owner_type, owner_display_name, started_at DESC);
		CREATE INDEX idx_task_run_summaries_run ON task_run_summaries(run_id);
		CREATE INDEX idx_task_run_summaries_started_run ON task_run_summaries(tenant_id, started_at DESC, run_id DESC);
		CREATE INDEX idx_task_run_summaries_workspace ON task_run_summaries(tenant_id, workspace_name, started_at DESC);
		CREATE INDEX idx_task_run_summaries_workflow ON task_run_summaries(tenant_id, workspace_name, workflow_name, started_at DESC);
		CREATE INDEX idx_task_run_summaries_factory ON task_run_summaries(tenant_id, factory_name, started_at DESC);
		CREATE INDEX idx_task_run_summaries_model ON task_run_summaries(tenant_id, model, started_at DESC);
		CREATE INDEX idx_task_run_summaries_repo ON task_run_summaries(tenant_id, repo, started_at DESC);
		CREATE INDEX idx_task_run_summaries_timeout ON task_run_summaries(tenant_id, timeout_at)`); err != nil {
		return fmt.Errorf("replace task run summaries v3: %w", err)
	}
	return tx.Commit()
}

// backfillTaskRunAnalyticsStatusV2 re-materializes existing summaries once so
// their status follows the PR-outcome and human-interaction rules. Each run is
// processed in its own transaction and failures are logged and skipped, so a
// single bad run cannot keep blocking startup; the hub_migrations sentinel is
// written at the end regardless (re-materialization is idempotent, and skipped
// runs are corrected the next time an event touches them).
func backfillTaskRunAnalyticsStatusV2(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS hub_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create hub migrations: %w", err)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_migrations WHERE name='task_run_analytics_status_v2'`).Scan(&applied); err != nil {
		return fmt.Errorf("check task run analytics status backfill: %w", err)
	}
	if applied > 0 {
		return nil
	}
	rows, err := db.Query(`SELECT id FROM task_runs ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("list task runs for status backfill: %w", err)
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	skipped := 0
	for _, runID := range runIDs {
		if err := backfillTaskRunStatus(db, runID); err != nil {
			skipped++
			log.Printf("[task-run-analytics] status backfill skipped run %s: %v", runID, err)
		}
	}
	if skipped > 0 {
		log.Printf("[task-run-analytics] status backfill skipped %d of %d run(s)", skipped, len(runIDs))
	}
	if _, err := db.Exec(`INSERT INTO hub_migrations(name, applied_at) VALUES('task_run_analytics_status_v2', ?) ON CONFLICT(name) DO NOTHING`, now().UnixMilli()); err != nil {
		return fmt.Errorf("mark task run analytics status backfill: %w", err)
	}
	return nil
}

func backfillTaskRunAnalyticsStatusV3(db *sql.DB) error {
	const migration = "task_run_analytics_status_v3"
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS hub_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create hub migrations: %w", err)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_migrations WHERE name=?`, migration).Scan(&applied); err != nil {
		return fmt.Errorf("check task run analytics status backfill: %w", err)
	}
	if applied > 0 {
		return nil
	}
	rows, err := db.Query(`SELECT id FROM task_runs ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("list task runs for status backfill: %w", err)
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	skipped := 0
	for _, runID := range runIDs {
		if err := backfillTaskRunStatus(db, runID); err != nil {
			skipped++
			log.Printf("[task-run-analytics] status v3 backfill skipped run %s: %v", runID, err)
		}
	}
	if skipped > 0 {
		log.Printf("[task-run-analytics] status v3 backfill skipped %d of %d run(s)", skipped, len(runIDs))
	}
	_, err = db.Exec(`INSERT INTO hub_migrations(name, applied_at) VALUES(?, ?) ON CONFLICT(name) DO NOTHING`, migration, now().UnixMilli())
	return err
}

// backfillTaskRunStatus re-materializes a single run in its own transaction.
func backfillTaskRunStatus(db *sql.DB, runID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := materializeTaskRunTx(tx, runID); err != nil {
		return err
	}
	return tx.Commit()
}

// backfillTaskRunAgentStartedAtV1 fills in agent_started_at for runs created
// before recordClawAgentStarted existed. Until that fix, the hub only ever
// checked the claw's status at run-creation time (always "provisioning" or
// "pending"), so agent_started_at was structurally never set — every run's
// "Agent started" funnel stage read 0 regardless of what actually happened.
//
// This can only approximate the real timestamp: no event recorded exactly
// when the agent came up, so it infers agent_started_at as provision_started_at
// (the closest lifecycle checkpoint we do have), falling back to the run's
// started_at if provisioning was never recorded either. Runs are only touched
// when there's unambiguous downstream evidence the agent actually ran: a PR
// was opened or merged, or the run finished with a failure type that can only
// be reached after the agent completed work (done_without_pr / no_pr /
// pr_closed_unmerged). Deliberately NOT treated as proof by themselves:
//
//   - failure_type='agent_stopped', the catch-all stopAgentTerminalWithReason
//     writes for every terminal stop — including retries exhausted while a
//     claw was still provisioning/bootstrapping and never connected at all.
//   - human_interaction_count > 0 — a dashboard message or manual stop can be
//     sent to a claw that's still provisioning; the interaction gets recorded
//     regardless of whether the agent ever came up.
//
// Treating either as proof of an agent start would fabricate agent_started_at
// for runs whose agent never ran.
//
// Because the inferred timestamp is a lower bound rather than the true agent
// start time, any duration metric derived from it (e.g. agent-start-to-PR-open)
// will read slightly high for backfilled runs. The funnel's "Agent started"
// count itself is unaffected: it only checks agent_started_at > 0.
//
// The migration only marks itself applied once every candidate is
// successfully backfilled; if any run is skipped (e.g. a transient DB error),
// the sentinel is left unwritten so the next hub startup retries — rows
// already backfilled simply stop matching the WHERE clause, so retries are
// cheap and idempotent.
func backfillTaskRunAgentStartedAtV1(db *sql.DB) error {
	const migration = "task_run_agent_started_at_v1"
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS hub_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create hub migrations: %w", err)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM hub_migrations WHERE name=?`, migration).Scan(&applied); err != nil {
		return fmt.Errorf("check agent_started_at backfill: %w", err)
	}
	if applied > 0 {
		return nil
	}

	rows, err := db.Query(`
		SELECT run_id, started_at, provision_started_at
		  FROM task_run_summaries
		 WHERE agent_started_at = 0
		   AND (
		         pr_opened_at > 0
		      OR merged_at > 0
		      OR (finished_at > 0 AND failure_type IN ('done_without_pr', 'no_pr', 'pr_closed_unmerged'))
		       )
		 ORDER BY started_at, run_id`)
	if err != nil {
		return fmt.Errorf("list runs for agent_started_at backfill: %w", err)
	}
	type candidate struct {
		runID      string
		inferredAt int64
	}
	var candidates []candidate
	for rows.Next() {
		var runID string
		var startedAt, provisionStartedAt int64
		if err := rows.Scan(&runID, &startedAt, &provisionStartedAt); err != nil {
			rows.Close()
			return err
		}
		inferredAt := provisionStartedAt
		if inferredAt == 0 {
			inferredAt = startedAt
		}
		candidates = append(candidates, candidate{runID: runID, inferredAt: inferredAt})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	skipped := 0
	for _, c := range candidates {
		if err := backfillTaskRunAgentStartedAt(db, c.runID, c.inferredAt); err != nil {
			skipped++
			log.Printf("[task-run-analytics] agent_started_at backfill skipped run %s: %v", c.runID, err)
		}
	}
	log.Printf("[task-run-analytics] agent_started_at backfill: %d of %d run(s) updated", len(candidates)-skipped, len(candidates))
	if skipped > 0 {
		// Leave the sentinel unwritten so the next hub startup retries the
		// runs that failed — already-backfilled rows no longer match the
		// WHERE clause above, so re-running is safe and only touches the
		// remainder.
		log.Printf("[task-run-analytics] agent_started_at backfill: %d run(s) skipped, will retry on next startup", skipped)
		return nil
	}

	_, err = db.Exec(`INSERT INTO hub_migrations(name, applied_at) VALUES(?, ?) ON CONFLICT(name) DO NOTHING`, migration, now().UnixMilli())
	return err
}

// backfillTaskRunAgentStartedAt records an inferred agent_started event for a
// single run and re-materializes it, in its own transaction.
func backfillTaskRunAgentStartedAt(db *sql.DB, runID string, inferredAtMillis int64) error {
	if inferredAtMillis <= 0 {
		return fmt.Errorf("no usable timestamp to infer agent_started_at from")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tenantID, attemptID string
	if err := tx.QueryRow(`SELECT tenant_id, current_attempt_id FROM task_runs WHERE id=?`, runID).Scan(&tenantID, &attemptID); err != nil {
		return fmt.Errorf("read task run: %w", err)
	}
	inferredAt := time.UnixMilli(inferredAtMillis).UTC()
	if err := recordTaskRunEventTx(tx, TaskRunEvent{
		TenantID:        tenantID,
		RunID:           runID,
		AttemptID:       attemptID,
		EventKey:        taskRunEventAgentStarted + ":" + runID,
		Source:          taskRunSourceHub,
		EventType:       taskRunEventAgentStarted,
		EventTime:       inferredAt,
		ObservedAt:      now(),
		ActorType:       taskRunActorSystem,
		InteractionRole: taskRunInteractionNeutral,
		Detail:          map[string]any{"backfilled": true, "reason": "agent_started_at_v1_migration"},
	}); err != nil {
		return fmt.Errorf("record agent_started event: %w", err)
	}
	if err := materializeTaskRunTx(tx, runID); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	return tx.Commit()
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
