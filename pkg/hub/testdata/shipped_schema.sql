-- shipped_schema.sql is the schema of a hub database migrated by the binary
-- production runs: every table and index, plus the hub_migrations markers that
-- binary writes. TestBootSurvivesAFullDiskFromTheShippedSchema loads it, forbids
-- the database to grow, and boots the CURRENT binary against it -- which is
-- what an upgrade on a disk-full hub does. Any fatal migration step that needs
-- a page fails that test.
--
-- Generated from commit 2554855403e3dbff539d37f5e3972f4ae06711a9 (main at the
-- time the retention branch was cut). Refresh it only once production is
-- running a newer schema, with:
--
--   go test ./pkg/hub -run TestBootSurvivesAFullDiskFromTheShippedSchema -update-shipped-schema
--
-- Refreshing it asserts that every migration step the current binary carries
-- has already been applied in production; do not refresh it to make the test
-- pass.
CREATE TABLE claw_checkpoints (
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
CREATE TABLE claw_pr_feedback_deliveries (
		claw_id       TEXT NOT NULL,
		feedback_type TEXT NOT NULL,
		github_id     INTEGER NOT NULL,
		created_at    DATETIME NOT NULL,
		PRIMARY KEY(claw_id, feedback_type, github_id)
	);
CREATE TABLE claw_prs (
		id          TEXT PRIMARY KEY,
		claw_id     TEXT NOT NULL REFERENCES claws(id),
		repo        TEXT NOT NULL,  -- e.g. "owner/repo"
		pr_number   INTEGER NOT NULL,
		pr_url      TEXT NOT NULL,
		title       TEXT NOT NULL DEFAULT '',
		last_ci_sha TEXT NOT NULL DEFAULT '',   -- last SHA we checked CI on
		last_ci_conclusion TEXT NOT NULL DEFAULT '', -- terminal CI verdict already delivered for last_ci_sha: '' | 'success' | 'failure'
		state       TEXT NOT NULL DEFAULT 'open',
		merged      INTEGER NOT NULL DEFAULT 0,
		merged_at   TEXT,
		last_comment_id INTEGER NOT NULL DEFAULT 0, -- last bugbot/pipeline comment ID seen
		last_comment_at TEXT NOT NULL DEFAULT '', -- timestamp of last seen comment
		last_review_comment_id INTEGER NOT NULL DEFAULT 0, -- last PR review comment ID seen
		last_review_id INTEGER NOT NULL DEFAULT 0, -- last top-level PR review ID seen
		pr_conditions_fired INTEGER NOT NULL DEFAULT 0,
		permanent_failure_count INTEGER NOT NULL DEFAULT 0,
		mention_only INTEGER NOT NULL DEFAULT 0, -- 1 = URL scanned from a message, not delivered via [DONE]; never gates finalization
		token_miss_count INTEGER NOT NULL DEFAULT 0, -- consecutive polls with no resolvable GitHub token for the repo; separate from permanent_failure_count (see migrate)
		last_mergeable_state TEXT NOT NULL DEFAULT '', -- last GitHub mergeable_state observed (e.g. "dirty"); used for one-shot conflict notifications
		created_at  DATETIME NOT NULL,
		UNIQUE(claw_id, pr_url)
	);
CREATE TABLE claw_turn_observations (
		id                   TEXT PRIMARY KEY,
		claw_id              TEXT NOT NULL REFERENCES claws(id) ON DELETE CASCADE,
		response             TEXT NOT NULL,
		progress_fingerprint TEXT NOT NULL,
		created_at           DATETIME NOT NULL
	);
CREATE TABLE claws (
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
		rebrief_pending INTEGER NOT NULL DEFAULT 0,
		no_progress_paused INTEGER NOT NULL DEFAULT 0,
		idle_since INTEGER NOT NULL DEFAULT 0,
		stage_entered_at INTEGER,
		stage_stalled_since INTEGER NOT NULL DEFAULT 0,
		idle_resume_at INTEGER NOT NULL DEFAULT 0,
		idle_resume_count INTEGER NOT NULL DEFAULT 0,
		llm_limited_until INTEGER NOT NULL DEFAULT 0,
		llm_limit_noticed_until INTEGER NOT NULL DEFAULT 0,
		pending_session_loss_notice TEXT NOT NULL DEFAULT ''
	);
CREATE TABLE dependency_status_state (
		id              TEXT PRIMARY KEY,
		status          TEXT NOT NULL DEFAULT '',
		message         TEXT NOT NULL DEFAULT '',
		since           INTEGER NOT NULL DEFAULT 0,
		notified_status TEXT NOT NULL DEFAULT '',
		-- The CheckedAt of the last snapshot that counted as an observation.
		-- The status cache outlives the watcher tick, so a re-served snapshot
		-- must not count as a second consecutive check toward the debounce.
		last_checked_at INTEGER NOT NULL DEFAULT 0,
		-- When the last degraded/down alert for this dependency was recorded,
		-- so the opt-in repeat_after can re-alert during a long outage.
		last_alert_at   INTEGER NOT NULL DEFAULT 0,
		updated_at      INTEGER NOT NULL DEFAULT 0
	);
CREATE TABLE factory_analytics (
		id           TEXT PRIMARY KEY,
		factory_name TEXT NOT NULL,
		issue_id     TEXT NOT NULL DEFAULT '',
		claw_id      TEXT NOT NULL DEFAULT '',
		action       TEXT NOT NULL,  -- 'claw_created', 'claw_terminated', 'error', 'pr_opened', 'pr_merged', 'pr_closed', 'done_signal'
		detail       TEXT NOT NULL DEFAULT '',
		result       TEXT NOT NULL DEFAULT '', -- 'success', 'failure', 'timeout', 'cancelled'
		created_at   DATETIME NOT NULL
	);
CREATE TABLE factory_events (
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
CREATE TABLE factory_triggers (
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
CREATE TABLE hub_migrations (name TEXT PRIMARY KEY, applied_at INTEGER NOT NULL);
CREATE TABLE hub_templates (
		name       TEXT PRIMARY KEY,
		files      TEXT NOT NULL DEFAULT '{}',  -- JSON map of filename -> content
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	);
CREATE TABLE infra_events (
		event_key   TEXT UNIQUE NOT NULL,
		event_type  TEXT NOT NULL,
		subject     TEXT NOT NULL,
		detail      TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(detail) AND json_type(detail) = 'object'),
		occurred_at INTEGER NOT NULL
	);
CREATE TABLE infra_notification_deliveries (
		event_rowid INTEGER NOT NULL,
		notifier TEXT NOT NULL,
		delivered_at INTEGER NOT NULL,
		status TEXT NOT NULL,
		PRIMARY KEY(event_rowid, notifier)
	);
CREATE TABLE integration_poll_state (integration TEXT PRIMARY KEY, last_success_at DATETIME NOT NULL);
CREATE TABLE llm_usage_limits (
		key_id           TEXT PRIMARY KEY,
		provider         TEXT NOT NULL DEFAULT '',
		reason           TEXT NOT NULL DEFAULT '',
		message          TEXT NOT NULL DEFAULT '',
		regain_at        INTEGER NOT NULL DEFAULT 0,
		retry_at         INTEGER NOT NULL DEFAULT 0,
		retries          INTEGER NOT NULL DEFAULT 0,
		detected_at      INTEGER NOT NULL DEFAULT 0,
		detected_claw_id TEXT NOT NULL DEFAULT '',
		-- A released row is kept, not deleted, so the retry counter survives
		-- the release. Without it a limit that lifts and immediately returns
		-- looks like a brand-new episode every time and the backoff never
		-- climbs. Rows are pruned once the episode is comfortably over.
		released_at      INTEGER NOT NULL DEFAULT 0
	);
CREATE TABLE messages (
		id         TEXT PRIMARY KEY,
		claw_id    TEXT NOT NULL REFERENCES claws(id),
		tenant_id  TEXT NOT NULL,
		role       TEXT NOT NULL,
		content    TEXT NOT NULL,
		format     TEXT NOT NULL DEFAULT '',
		user_login TEXT,
		created_at DATETIME NOT NULL,
		delivered_at DATETIME
	);
CREATE TABLE model_prices (model TEXT PRIMARY KEY, input_cost_per_token REAL NOT NULL, output_cost_per_token REAL NOT NULL, cache_read_cost_per_token REAL NOT NULL, cache_write_cost_per_token REAL NOT NULL, source TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE pipeline_gate_results (
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
CREATE TABLE pipeline_outputs (
		claw_id      TEXT NOT NULL,
		stage_id     TEXT NOT NULL,
		output_name  TEXT NOT NULL,
		exit_code    INTEGER NOT NULL DEFAULT 0,
		stdout       TEXT NOT NULL DEFAULT '',
		stderr       TEXT NOT NULL DEFAULT '',
		parsed_json  TEXT NOT NULL DEFAULT '{}',
		span_id      TEXT NOT NULL DEFAULT '',
		span_kind    TEXT NOT NULL DEFAULT 'INTERNAL',
		duration_ms  INTEGER NOT NULL DEFAULT 0,
		status       TEXT NOT NULL DEFAULT 'OK',
		records      TEXT NOT NULL DEFAULT '[]',
		created_at   DATETIME NOT NULL,
		PRIMARY KEY (claw_id, output_name)
	);
CREATE TABLE pipeline_stage_history (
		claw_id    TEXT NOT NULL,
		stage_id   TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		PRIMARY KEY (claw_id, stage_id)
	);
CREATE TABLE slack_notification_deliveries (
		event_id     TEXT PRIMARY KEY,
		run_id       TEXT NOT NULL,
		delivered_at INTEGER NOT NULL,
		message_ts   TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'sent'
	);
CREATE TABLE slack_notification_deliveries_v2 (
		event_id     TEXT NOT NULL,
		notifier     TEXT NOT NULL,
		run_id       TEXT NOT NULL,
		delivered_at INTEGER NOT NULL,
		message_ts   TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL DEFAULT 'sent',
		PRIMARY KEY (event_id, notifier)
	);
CREATE TABLE slack_notifier_state (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
CREATE TABLE slack_run_threads (
		run_id     TEXT PRIMARY KEY,
		tenant_id  TEXT NOT NULL,
		channel    TEXT NOT NULL,
		thread_ts  TEXT NOT NULL,
		created_at INTEGER NOT NULL
	);
CREATE TABLE ssh_known_hosts (
		host          TEXT PRIMARY KEY,
		key_type      TEXT NOT NULL,
		key_data      TEXT NOT NULL,
		fingerprint   TEXT NOT NULL,
		first_seen_at DATETIME NOT NULL,
		last_seen_at  DATETIME NOT NULL
	);
CREATE TABLE task_run_attempts (
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
CREATE TABLE task_run_events (
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
			'timeout','unknown_failure','agent_idle','stage_stalled','pr_associated','pr_opened','pr_closed_unmerged','pr_merged',
			'approval_only_pr_review','human_requested_changes','human_review_comment','human_pr_comment',
			'human_manual_code_push','human_tracker_update','human_dashboard_message',
			'human_manual_stop_or_resume','human_settings_or_status_change',
			'unknown_human_interaction','pr_replaced','correction','retraction','ci_succeeded','ci_failed'
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
CREATE TABLE task_run_prs (
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
		ready_at        INTEGER NOT NULL DEFAULT 0,
		merged_by_login TEXT NOT NULL DEFAULT '',
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL,
		UNIQUE(tenant_id, run_id, repo, pr_number)
	);
CREATE TABLE task_run_stages (
		tenant_id  TEXT NOT NULL,
		run_id     TEXT NOT NULL,
		seq        INTEGER NOT NULL,
		stage_id   TEXT NOT NULL,
		label      TEXT NOT NULL DEFAULT '',
		entered_at INTEGER NOT NULL,
		exited_at  INTEGER,
		source     TEXT NOT NULL DEFAULT 'live' CHECK(source IN ('live','backfill_messages','backfill_history','v2_transitions')),
		PRIMARY KEY (tenant_id, run_id, seq)
	);
CREATE TABLE task_run_summaries (
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
		ready_at                INTEGER NOT NULL DEFAULT 0,
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
CREATE TABLE task_run_usage (
		id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, run_id TEXT NOT NULL REFERENCES task_runs(id) ON DELETE CASCADE,
		session_key TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', model_provider TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
		committed_input_tokens INTEGER NOT NULL DEFAULT 0, committed_output_tokens INTEGER NOT NULL DEFAULT 0, committed_total_tokens INTEGER NOT NULL DEFAULT 0, committed_cost_usd REAL NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER, cache_write_tokens INTEGER, estimated_cost_usd REAL, cost_source TEXT NOT NULL DEFAULT 'gateway' CHECK(cost_source IN ('gateway','hub_pricing')),
		usage_day TEXT NOT NULL DEFAULT '',
		first_seen_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(tenant_id, run_id, session_key)
	);
CREATE TABLE task_runs (
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
CREATE TABLE tenants (
		id        TEXT PRIMARY KEY,
		name      TEXT NOT NULL,
		token     TEXT NOT NULL UNIQUE, -- user login token
		claw_token TEXT NOT NULL UNIQUE, -- token claws present on connect
		created_at DATETIME NOT NULL
	);
CREATE TABLE ticket_metadata (
		tenant_id TEXT NOT NULL, integration TEXT NOT NULL DEFAULT '', integration_workspace TEXT NOT NULL DEFAULT '', issue_id TEXT NOT NULL, requester TEXT NOT NULL DEFAULT '',
		requester_role TEXT NOT NULL DEFAULT '', team TEXT NOT NULL DEFAULT '', priority TEXT NOT NULL DEFAULT '',
		ask TEXT NOT NULL DEFAULT '', reported_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL, last_attempt_at INTEGER NOT NULL DEFAULT 0, consecutive_failures INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (tenant_id, integration, integration_workspace, issue_id)
	);
CREATE TABLE usage_daily (
		tenant_id TEXT NOT NULL, day TEXT NOT NULL, workspace_name TEXT NOT NULL DEFAULT '', factory_name TEXT NOT NULL DEFAULT '', workflow_name TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL,
		PRIMARY KEY(tenant_id, day, workspace_name, factory_name, workflow_name, model)
	);
CREATE TABLE volume_leases (
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
	);
CREATE TABLE workflow_runs (
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
CREATE TABLE workflow_v2_agent_tasks (
		id                  TEXT PRIMARY KEY,
		run_id              TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		effect_id           TEXT NOT NULL DEFAULT '',
		attempt_id          TEXT NOT NULL DEFAULT '',
		state               TEXT NOT NULL,
		state_version       INTEGER NOT NULL,
		status              TEXT NOT NULL CHECK(status IN ('assigned','running','completed','failed','cancelled','timed_out')),
		instructions        TEXT NOT NULL,
		allowed_actions     TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(allowed_actions) AND json_type(allowed_actions)='array'),
		required_artifacts  TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(required_artifacts) AND json_type(required_artifacts)='array'),
		heartbeat_deadline  INTEGER NOT NULL,
		deadline            INTEGER NOT NULL,
		last_heartbeat_at   INTEGER NOT NULL DEFAULT 0,
		terminal_reason     TEXT NOT NULL DEFAULT '',
		created_at          INTEGER NOT NULL,
		updated_at          INTEGER NOT NULL,
		finished_at         INTEGER NOT NULL DEFAULT 0
	);
CREATE TABLE workflow_v2_artifacts (
		id            TEXT PRIMARY KEY,
		run_id        TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		task_id       TEXT NOT NULL DEFAULT '',
		kind          TEXT NOT NULL,
		name          TEXT NOT NULL,
		revision      INTEGER NOT NULL CHECK(revision > 0),
		content_type  TEXT NOT NULL DEFAULT 'application/json',
		content_json  TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(content_json)),
		content_digest TEXT NOT NULL,
		status        TEXT NOT NULL CHECK(status IN ('submitted','accepted','rejected','superseded')),
		created_at    INTEGER NOT NULL,
		UNIQUE(run_id, kind, name, revision)
	);
CREATE TABLE workflow_v2_attempts (
		id          TEXT PRIMARY KEY,
		run_id      TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		claw_id     TEXT NOT NULL DEFAULT '',
		number      INTEGER NOT NULL CHECK(number > 0),
		status      TEXT NOT NULL CHECK(status IN ('provisioning','active','succeeded','failed','lost','cancelled')),
		started_at  INTEGER NOT NULL,
		heartbeat_at INTEGER NOT NULL DEFAULT 0,
		finished_at INTEGER NOT NULL DEFAULT 0,
		reason      TEXT NOT NULL DEFAULT '',
		UNIQUE(run_id, number)
	);
CREATE TABLE workflow_v2_context_bundles (
		id           TEXT PRIMARY KEY,
		run_id       TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		revision     TEXT NOT NULL,
		status       TEXT NOT NULL CHECK(status IN ('assembling','ready','failed')),
		sources_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(sources_json) AND json_type(sources_json)='array'),
		created_at   INTEGER NOT NULL,
		updated_at   INTEGER NOT NULL,
		UNIQUE(run_id, revision)
	);
CREATE TABLE workflow_v2_control_outbox (
		message_id       TEXT PRIMARY KEY,
		run_id           TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		attempt_id       TEXT NOT NULL DEFAULT '',
		task_id          TEXT NOT NULL DEFAULT '',
		kind             TEXT NOT NULL,
		envelope_json    TEXT NOT NULL CHECK(json_valid(envelope_json) AND json_type(envelope_json)='object'),
		status           TEXT NOT NULL CHECK(status IN ('pending','sent','acknowledged','cancelled')),
		attempt_count    INTEGER NOT NULL DEFAULT 0,
		next_attempt_at  INTEGER NOT NULL DEFAULT 0,
		last_error       TEXT NOT NULL DEFAULT '',
		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL,
		acknowledged_at  INTEGER NOT NULL DEFAULT 0
	);
CREATE TABLE workflow_v2_delivery_heads (
		id          TEXT PRIMARY KEY,
		pr_id       TEXT NOT NULL REFERENCES workflow_v2_delivery_prs(id) ON DELETE CASCADE,
		head_sha    TEXT NOT NULL,
		generation  INTEGER NOT NULL CHECK(generation > 0),
		observed_at INTEGER NOT NULL,
		UNIQUE(pr_id, generation)
	);
CREATE TABLE workflow_v2_delivery_prs (
		id                  TEXT PRIMARY KEY,
		run_id              TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		url                 TEXT NOT NULL,
		repository_name     TEXT NOT NULL,
		repository          TEXT NOT NULL,
		pr_number           INTEGER NOT NULL CHECK(pr_number > 0),
		source_branch       TEXT NOT NULL,
		base_branch         TEXT NOT NULL,
		current_head_sha    TEXT NOT NULL,
		state               TEXT NOT NULL CHECK(state IN ('open','closed','merged')),
		active              INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0,1)),
		supersedes_id       TEXT NOT NULL DEFAULT '',
		provenance_json     TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(provenance_json) AND json_type(provenance_json)='object'),
		verified_at         INTEGER NOT NULL,
		updated_at          INTEGER NOT NULL,
		UNIQUE(run_id, url),
		UNIQUE(run_id, repository, pr_number)
	);
CREATE TABLE workflow_v2_effect_attempts (
		id            TEXT PRIMARY KEY,
		effect_id     TEXT NOT NULL REFERENCES workflow_v2_effects(id) ON DELETE CASCADE,
		number        INTEGER NOT NULL CHECK(number > 0),
		status        TEXT NOT NULL CHECK(status IN ('running','succeeded','retryable_failed','permanent_failed','unknown','cancelled')),
		request_json  TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(request_json)),
		receipt_json  TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(receipt_json)),
		error         TEXT NOT NULL DEFAULT '',
		started_at    INTEGER NOT NULL,
		finished_at   INTEGER NOT NULL DEFAULT 0,
		UNIQUE(effect_id, number)
	);
CREATE TABLE workflow_v2_effects (
		id                 TEXT PRIMARY KEY,
		run_id             TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		origin_type        TEXT NOT NULL CHECK(origin_type IN ('initial','transition','event_clause','command')),
		origin_id          TEXT NOT NULL,
		definition_path    TEXT NOT NULL,
		effect_key         TEXT NOT NULL UNIQUE,
		kind               TEXT NOT NULL,
		payload_json       TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(payload_json)),
		status             TEXT NOT NULL CHECK(status IN ('planned','running','succeeded','retryable_failed','permanent_failed','unknown','cancelled')),
		attempt_count      INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
		lease_owner        TEXT NOT NULL DEFAULT '',
		lease_expires_at   INTEGER NOT NULL DEFAULT 0,
		next_attempt_at    INTEGER NOT NULL DEFAULT 0,
		receipt_json       TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(receipt_json) AND json_type(receipt_json)='object'),
		last_error         TEXT NOT NULL DEFAULT '',
		created_at         INTEGER NOT NULL,
		updated_at         INTEGER NOT NULL
	);
CREATE TABLE workflow_v2_event_receipts (
		id                     TEXT PRIMARY KEY,
		run_id                 TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		event_id               TEXT NOT NULL,
		message_id             TEXT NOT NULL DEFAULT '',
		disposition            TEXT NOT NULL CHECK(disposition IN ('accepted','duplicate','stale_state','rejected','unauthorized')),
		observed_state_version INTEGER NOT NULL,
		reason                 TEXT NOT NULL DEFAULT '',
		received_at            INTEGER NOT NULL
	);
CREATE TABLE workflow_v2_events (
		id                     TEXT PRIMARY KEY,
		run_id                 TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		message_id             TEXT NOT NULL DEFAULT '',
		kind                   TEXT NOT NULL,
		expected_state_version INTEGER,
		observed_state_version INTEGER NOT NULL,
		disposition            TEXT NOT NULL CHECK(disposition IN ('accepted','duplicate','stale_state','rejected','unauthorized')),
		reason                 TEXT NOT NULL DEFAULT '',
		producer               TEXT NOT NULL,
		provenance_json        TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(provenance_json) AND json_type(provenance_json)='object'),
		payload_json           TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(payload_json) AND json_type(payload_json)='object'),
		facts_json             TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(facts_json) AND json_type(facts_json)='object'),
		received_at            INTEGER NOT NULL
	);
CREATE TABLE workflow_v2_evidence (
		id              TEXT PRIMARY KEY,
		run_id          TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		pr_id           TEXT NOT NULL DEFAULT '',
		head_sha         TEXT NOT NULL DEFAULT '',
		head_generation  INTEGER NOT NULL DEFAULT 0 CHECK(head_generation >= 0),
		domain           TEXT NOT NULL CHECK(domain IN ('ci','review','pull_request','operator','effect','context')),
		connection       TEXT NOT NULL DEFAULT '',
		external_id      TEXT NOT NULL DEFAULT '',
		kind             TEXT NOT NULL,
		status           TEXT NOT NULL,
		payload_json     TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(payload_json)),
		provenance_json  TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(provenance_json) AND json_type(provenance_json)='object'),
		observed_at      INTEGER NOT NULL,
		superseded_at    INTEGER NOT NULL DEFAULT 0,
		UNIQUE(run_id, pr_id, domain, connection, external_id, kind, head_generation)
	);
CREATE TABLE workflow_v2_facts (
		run_id          TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		fact_key        TEXT NOT NULL,
		value_json      TEXT NOT NULL CHECK(json_valid(value_json)),
		producer        TEXT NOT NULL,
		provenance_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(provenance_json) AND json_type(provenance_json)='object'),
		event_id        TEXT NOT NULL REFERENCES workflow_v2_events(id),
		updated_at      INTEGER NOT NULL,
		PRIMARY KEY(run_id, fact_key)
	);
CREATE TABLE workflow_v2_runs (
		id                 TEXT PRIMARY KEY,
		tenant_id          TEXT NOT NULL,
		workspace_name     TEXT NOT NULL,
		workflow_name      TEXT NOT NULL,
		workspace_revision TEXT NOT NULL,
		workflow_revision  TEXT NOT NULL,
		workspace_yaml     TEXT NOT NULL,
		workflow_yaml      TEXT NOT NULL,
		state              TEXT NOT NULL,
		display_phase      TEXT NOT NULL,
		state_version      INTEGER NOT NULL CHECK(state_version >= 1),
		status             TEXT NOT NULL CHECK(status IN ('active','suspended','completed','cancelled')),
		waiting_reason     TEXT NOT NULL DEFAULT '',
		current_attempt_id TEXT NOT NULL DEFAULT '',
		current_task_id    TEXT NOT NULL DEFAULT '',
		context_bundle_id  TEXT NOT NULL DEFAULT '',
		trigger_type       TEXT NOT NULL DEFAULT 'manual',
		task_run_id        TEXT NOT NULL DEFAULT '',
		created_at         INTEGER NOT NULL,
		updated_at         INTEGER NOT NULL,
		finished_at        INTEGER NOT NULL DEFAULT 0
	);
CREATE TABLE workflow_v2_transitions (
		id                 TEXT PRIMARY KEY,
		run_id             TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		event_id           TEXT NOT NULL REFERENCES workflow_v2_events(id),
		definition_name    TEXT NOT NULL,
		from_state         TEXT NOT NULL,
		to_state           TEXT NOT NULL,
		from_version       INTEGER NOT NULL,
		to_version         INTEGER NOT NULL,
		workspace_revision TEXT NOT NULL,
		workflow_revision  TEXT NOT NULL,
		fact_delta_json    TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(fact_delta_json) AND json_type(fact_delta_json)='object'),
		created_at         INTEGER NOT NULL,
		UNIQUE(event_id),
		UNIQUE(run_id, to_version)
	);
CREATE INDEX idx_claw_checkpoints_claw ON claw_checkpoints(claw_id, created_at);
CREATE INDEX idx_claw_checkpoints_status ON claw_checkpoints(status, created_at);
CREATE INDEX idx_claw_turn_observations_claw ON claw_turn_observations(claw_id, created_at);
CREATE INDEX idx_claws_stage_stalled ON claws(stage_stalled_since) WHERE stage_stalled_since > 0;
CREATE INDEX idx_claws_tenant  ON claws(tenant_id);
CREATE INDEX idx_factory_analytics_action ON factory_analytics(action, created_at);
CREATE INDEX idx_factory_analytics_claw ON factory_analytics(claw_id);
CREATE INDEX idx_factory_analytics_factory ON factory_analytics(factory_name, created_at);
CREATE INDEX idx_factory_triggers_claw ON factory_triggers(claw_id);
CREATE INDEX idx_factory_triggers_integration_status ON factory_triggers(integration, status, claw_id);
CREATE UNIQUE INDEX idx_factory_triggers_key ON factory_triggers(factory_name, integration, trigger_key);
CREATE INDEX idx_messages_claw ON messages(claw_id, created_at);
CREATE INDEX idx_messages_pending ON messages(claw_id, created_at) WHERE delivered_at IS NULL;
CREATE INDEX idx_pipeline_gate_results_claw ON pipeline_gate_results(claw_id, created_at);
CREATE INDEX idx_pipeline_outputs_claw ON pipeline_outputs(claw_id, created_at);
CREATE INDEX idx_pipeline_outputs_stage ON pipeline_outputs(claw_id, stage_id);
CREATE INDEX idx_pipeline_stage_history_claw ON pipeline_stage_history(claw_id, created_at);
CREATE INDEX idx_slack_deliveries_time
		ON slack_notification_deliveries(delivered_at);
CREATE INDEX idx_task_run_attempts_run_number ON task_run_attempts(run_id, attempt_number);
CREATE INDEX idx_task_run_events_observed ON task_run_events(tenant_id, observed_at);
CREATE INDEX idx_task_run_events_run_time ON task_run_events(run_id, event_time, id);
CREATE INDEX idx_task_run_events_source_event ON task_run_events(tenant_id, source, source_event_id);
CREATE UNIQUE INDEX idx_task_run_events_tenant_key ON task_run_events(tenant_id, run_id, event_key);
CREATE INDEX idx_task_run_events_tenant_run_time ON task_run_events(tenant_id, run_id, event_time, observed_at, event_key);
CREATE INDEX idx_task_run_events_type_time ON task_run_events(event_type, event_time);
CREATE INDEX idx_task_run_prs_repo_pr ON task_run_prs(repo, pr_number);
CREATE INDEX idx_task_run_prs_run ON task_run_prs(run_id, state, merged);
CREATE INDEX idx_task_run_prs_tenant_merged ON task_run_prs(tenant_id, run_id, merged_at);
CREATE INDEX idx_task_run_prs_tenant_run ON task_run_prs(tenant_id, run_id, state, merged);
CREATE INDEX idx_task_run_stages_run ON task_run_stages(tenant_id, run_id, seq);
CREATE INDEX idx_task_run_summaries_factory ON task_run_summaries(tenant_id, factory_name, started_at DESC);
CREATE INDEX idx_task_run_summaries_model ON task_run_summaries(tenant_id, model, started_at DESC);
CREATE INDEX idx_task_run_summaries_owner_started ON task_run_summaries(workspace_name, owner_type, owner_display_name, started_at DESC);
CREATE INDEX idx_task_run_summaries_repo ON task_run_summaries(tenant_id, repo, started_at DESC);
CREATE INDEX idx_task_run_summaries_run ON task_run_summaries(run_id);
CREATE INDEX idx_task_run_summaries_started_run ON task_run_summaries(tenant_id, started_at DESC, run_id DESC);
CREATE INDEX idx_task_run_summaries_status ON task_run_summaries(tenant_id, status, started_at DESC);
CREATE INDEX idx_task_run_summaries_ticket_detail ON task_run_summaries(tenant_id, integration, integration_workspace, issue_id, started_at);
CREATE INDEX idx_task_run_summaries_ticket_page ON task_run_summaries(tenant_id, requires_pr, analytics_enabled, started_at DESC, integration, integration_workspace, issue_id, issue_created_at DESC, status);
CREATE INDEX idx_task_run_summaries_timeout ON task_run_summaries(tenant_id, timeout_at);
CREATE INDEX idx_task_run_summaries_workflow ON task_run_summaries(tenant_id, workspace_name, workflow_name, started_at DESC);
CREATE INDEX idx_task_run_summaries_workspace ON task_run_summaries(tenant_id, workspace_name, started_at DESC);
CREATE INDEX idx_task_runs_claw ON task_runs(claw_id);
CREATE INDEX idx_task_runs_owner ON task_runs(workspace_name, owner_type, owner_display_name, created_at DESC);
CREATE INDEX idx_task_runs_tenant_created ON task_runs(tenant_id, created_at DESC, id DESC);
CREATE INDEX idx_task_runs_trigger ON task_runs(trigger_id);
CREATE INDEX idx_volume_leases_claw ON volume_leases(claw_id, released_at);
CREATE INDEX idx_volume_leases_volume_active ON volume_leases(volume_id, released_at, expires_at);
CREATE INDEX idx_workflow_runs_claw ON workflow_runs(claw_id);
CREATE INDEX idx_workflow_runs_status ON workflow_runs(tenant_id, status, created_at);
CREATE INDEX idx_workflow_runs_tenant ON workflow_runs(tenant_id, created_at);
CREATE INDEX idx_workflow_runs_workflow ON workflow_runs(tenant_id, workflow_name, workspace_name, created_at);
CREATE INDEX idx_workflow_v2_agent_tasks_liveness ON workflow_v2_agent_tasks(status, heartbeat_deadline, deadline);
CREATE INDEX idx_workflow_v2_agent_tasks_run ON workflow_v2_agent_tasks(run_id, created_at, id);
CREATE INDEX idx_workflow_v2_attempts_claw ON workflow_v2_attempts(claw_id);
CREATE INDEX idx_workflow_v2_attempts_run ON workflow_v2_attempts(run_id, number);
CREATE INDEX idx_workflow_v2_control_ready ON workflow_v2_control_outbox(status, next_attempt_at);
CREATE INDEX idx_workflow_v2_delivery_run ON workflow_v2_delivery_prs(run_id, active, state);
CREATE INDEX idx_workflow_v2_effects_ready ON workflow_v2_effects(status, next_attempt_at, lease_expires_at);
CREATE INDEX idx_workflow_v2_effects_run ON workflow_v2_effects(run_id, created_at, id);
CREATE INDEX idx_workflow_v2_event_receipts_run ON workflow_v2_event_receipts(run_id, received_at, id);
CREATE INDEX idx_workflow_v2_events_disposition ON workflow_v2_events(run_id, disposition, received_at);
CREATE UNIQUE INDEX idx_workflow_v2_events_message ON workflow_v2_events(run_id, message_id) WHERE message_id != '';
CREATE INDEX idx_workflow_v2_events_run ON workflow_v2_events(run_id, received_at, id);
CREATE INDEX idx_workflow_v2_evidence_subject ON workflow_v2_evidence(run_id, pr_id, head_generation, domain, superseded_at);
CREATE INDEX idx_workflow_v2_runs_status ON workflow_v2_runs(tenant_id, status, updated_at DESC);
CREATE INDEX idx_workflow_v2_runs_tenant_updated ON workflow_v2_runs(tenant_id, updated_at DESC, id);
CREATE INDEX idx_workflow_v2_runs_workflow ON workflow_v2_runs(tenant_id, workspace_name, workflow_name, updated_at DESC);
CREATE INDEX idx_workflow_v2_transitions_run ON workflow_v2_transitions(run_id, to_version);
INSERT INTO hub_migrations(name, applied_at) VALUES('claw_prs_state_backfill_v1', 0);
INSERT INTO hub_migrations(name, applied_at) VALUES('claw_prs_state_backfill_v2', 0);
INSERT INTO hub_migrations(name, applied_at) VALUES('task_run_agent_started_at_v1', 0);
INSERT INTO hub_migrations(name, applied_at) VALUES('task_run_analytics_status_v2', 0);
INSERT INTO hub_migrations(name, applied_at) VALUES('task_run_analytics_status_v3', 0);
INSERT INTO hub_migrations(name, applied_at) VALUES('task_run_ready_at_v1', 0);
INSERT INTO hub_migrations(name, applied_at) VALUES('task_run_stages_v1', 0);
INSERT INTO hub_migrations(name, applied_at) VALUES('task_run_summaries_ticket_page_v3', 0);
