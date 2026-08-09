// Package workflowv2 implements the deterministic durable workflow runtime.
// It intentionally has no dependency on hub conversation/message types.
package workflowv2

import (
	"database/sql"
	"fmt"
)

// Migrate adds v2 runtime storage without changing or reinterpreting any v1
// workflow, claw, transcript, or analytics table.
func Migrate(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("workflow v2 migrate: database is nil")
	}
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS workflow_v2_runs (
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
		created_at         INTEGER NOT NULL,
		updated_at         INTEGER NOT NULL,
		finished_at        INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_runs_tenant_updated ON workflow_v2_runs(tenant_id, updated_at DESC, id);
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_runs_workflow ON workflow_v2_runs(tenant_id, workspace_name, workflow_name, updated_at DESC);
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_runs_status ON workflow_v2_runs(tenant_id, status, updated_at DESC);

	CREATE TABLE IF NOT EXISTS workflow_v2_attempts (
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
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_attempts_run ON workflow_v2_attempts(run_id, number);
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_attempts_claw ON workflow_v2_attempts(claw_id);

	CREATE TABLE IF NOT EXISTS workflow_v2_events (
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
	CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_v2_events_message ON workflow_v2_events(run_id, message_id) WHERE message_id != '';
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_events_run ON workflow_v2_events(run_id, received_at, id);
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_events_disposition ON workflow_v2_events(run_id, disposition, received_at);

	CREATE TABLE IF NOT EXISTS workflow_v2_event_receipts (
		id                     TEXT PRIMARY KEY,
		run_id                 TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		event_id               TEXT NOT NULL,
		message_id             TEXT NOT NULL DEFAULT '',
		disposition            TEXT NOT NULL CHECK(disposition IN ('accepted','duplicate','stale_state','rejected','unauthorized')),
		observed_state_version INTEGER NOT NULL,
		reason                 TEXT NOT NULL DEFAULT '',
		received_at            INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_event_receipts_run ON workflow_v2_event_receipts(run_id, received_at, id);

	CREATE TABLE IF NOT EXISTS workflow_v2_facts (
		run_id          TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		fact_key        TEXT NOT NULL,
		value_json      TEXT NOT NULL CHECK(json_valid(value_json)),
		producer        TEXT NOT NULL,
		provenance_json TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(provenance_json) AND json_type(provenance_json)='object'),
		event_id        TEXT NOT NULL REFERENCES workflow_v2_events(id),
		updated_at      INTEGER NOT NULL,
		PRIMARY KEY(run_id, fact_key)
	);

	CREATE TABLE IF NOT EXISTS workflow_v2_transitions (
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
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_transitions_run ON workflow_v2_transitions(run_id, to_version);

	CREATE TABLE IF NOT EXISTS workflow_v2_effects (
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
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_effects_ready ON workflow_v2_effects(status, next_attempt_at, lease_expires_at);
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_effects_run ON workflow_v2_effects(run_id, created_at, id);

	CREATE TABLE IF NOT EXISTS workflow_v2_effect_attempts (
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

	CREATE TABLE IF NOT EXISTS workflow_v2_agent_tasks (
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
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_agent_tasks_run ON workflow_v2_agent_tasks(run_id, created_at, id);
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_agent_tasks_liveness ON workflow_v2_agent_tasks(status, heartbeat_deadline, deadline);

	CREATE TABLE IF NOT EXISTS workflow_v2_artifacts (
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

	CREATE TABLE IF NOT EXISTS workflow_v2_context_bundles (
		id           TEXT PRIMARY KEY,
		run_id       TEXT NOT NULL REFERENCES workflow_v2_runs(id) ON DELETE CASCADE,
		revision     TEXT NOT NULL,
		status       TEXT NOT NULL CHECK(status IN ('assembling','ready','failed')),
		sources_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(sources_json) AND json_type(sources_json)='array'),
		created_at   INTEGER NOT NULL,
		updated_at   INTEGER NOT NULL,
		UNIQUE(run_id, revision)
	);

	CREATE TABLE IF NOT EXISTS workflow_v2_delivery_prs (
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
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_delivery_run ON workflow_v2_delivery_prs(run_id, active, state);

	CREATE TABLE IF NOT EXISTS workflow_v2_delivery_heads (
		id          TEXT PRIMARY KEY,
		pr_id       TEXT NOT NULL REFERENCES workflow_v2_delivery_prs(id) ON DELETE CASCADE,
		head_sha    TEXT NOT NULL,
		generation  INTEGER NOT NULL CHECK(generation > 0),
		observed_at INTEGER NOT NULL,
		UNIQUE(pr_id, generation)
	);

	CREATE TABLE IF NOT EXISTS workflow_v2_evidence (
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
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_evidence_subject ON workflow_v2_evidence(run_id, pr_id, head_generation, domain, superseded_at);

	CREATE TABLE IF NOT EXISTS workflow_v2_control_outbox (
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
	CREATE INDEX IF NOT EXISTS idx_workflow_v2_control_ready ON workflow_v2_control_outbox(status, next_attempt_at);
	`)
	if err != nil {
		return fmt.Errorf("workflow v2 migrate: %w", err)
	}
	return nil
}
