// Package aichat implements the workspace product assistant independently of hub workflows.
package aichat

import (
	"context"
	"database/sql"
	"fmt"
)

// Migrate creates the AI Chat schema. Every statement is idempotent; the
// transaction and the hub's SQLite busy timeout serialize concurrent openers.
func Migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range schema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ai chat schema: %w", err)
		}
	}
	return tx.Commit()
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS ai_chat_threads (
 id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, workspace TEXT NOT NULL,
 owner_login TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL,
 topic TEXT, visibility TEXT NOT NULL DEFAULT 'private', panel_json TEXT,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, archived_at INTEGER
 )`,
	`CREATE INDEX IF NOT EXISTS idx_ai_chat_threads_owner ON ai_chat_threads(tenant_id, owner_login, updated_at)`,
	`CREATE TABLE IF NOT EXISTS ai_chat_messages (
 id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES ai_chat_threads(id),
 seq INTEGER NOT NULL, role TEXT NOT NULL, content TEXT NOT NULL DEFAULT '',
 blocks_json TEXT, interview_json TEXT, panel_json TEXT, model TEXT,
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 status TEXT NOT NULL DEFAULT 'pending', created_at INTEGER NOT NULL
 )`,
	`CREATE INDEX IF NOT EXISTS idx_ai_chat_messages_thread_seq ON ai_chat_messages(thread_id, seq)`,
	`CREATE TABLE IF NOT EXISTS ai_chat_tool_runs (
 id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES ai_chat_messages(id),
 seq INTEGER NOT NULL, tool TEXT NOT NULL, provider TEXT NOT NULL,
 args_json TEXT, summary TEXT, result_json TEXT, row_count INTEGER,
 duration_ms INTEGER, error TEXT, created_at INTEGER NOT NULL
 )`,
	`CREATE TABLE IF NOT EXISTS ai_chat_artifacts (
 id TEXT PRIMARY KEY, slug TEXT NOT NULL, tenant_id TEXT NOT NULL,
 workspace TEXT NOT NULL, owner_login TEXT NOT NULL,
 thread_id TEXT NOT NULL REFERENCES ai_chat_threads(id), title TEXT NOT NULL,
 current_version INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL, deleted_at INTEGER
 )`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_ai_chat_artifacts_slug ON ai_chat_artifacts(slug)`,
	`CREATE INDEX IF NOT EXISTS idx_ai_chat_artifacts_owner ON ai_chat_artifacts(owner_login, updated_at)`,
	`CREATE TABLE IF NOT EXISTS ai_chat_artifact_versions (
 artifact_id TEXT NOT NULL REFERENCES ai_chat_artifacts(id), version INTEGER NOT NULL,
 message_id TEXT NOT NULL REFERENCES ai_chat_messages(id), path TEXT NOT NULL,
 bytes INTEGER NOT NULL, sha256 TEXT NOT NULL, created_at INTEGER NOT NULL,
 PRIMARY KEY (artifact_id, version)
 )`,
	`CREATE TABLE IF NOT EXISTS ai_chat_handoffs (
 id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES ai_chat_threads(id),
 message_id TEXT NOT NULL REFERENCES ai_chat_messages(id), kind TEXT NOT NULL,
 provider TEXT NOT NULL, external_ref TEXT, url TEXT, payload_json TEXT,
 created_at INTEGER NOT NULL
 )`,
}
