package hub

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite, no CGO required
)

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
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
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN workflow_volumes TEXT NOT NULL DEFAULT '[]'`)
	_, _ = db.Exec(`ALTER TABLE claws ADD COLUMN trigger_actor_json TEXT NOT NULL DEFAULT '{}'`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_comment_at TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN pr_conditions_fired INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_review_comment_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE claw_prs ADD COLUMN last_review_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN format TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE factory_triggers ADD COLUMN task_run_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE task_run_attempts ADD COLUMN restored_checkpoint_id TEXT`)

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
		task_run_id    TEXT NOT NULL DEFAULT '',
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
		trigger_actor_json TEXT NOT NULL DEFAULT '{}'
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
		task_run_id    TEXT NOT NULL DEFAULT '',
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
		status                  TEXT NOT NULL CHECK(status IN ('running','clean_success','warning_success','failed')),
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
		claw_id                 TEXT NOT NULL DEFAULT '',
		model                   TEXT NOT NULL DEFAULT '',
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
