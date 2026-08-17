package hub

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// The pre-phase-0a messages schema: identical to the current one except
// user_login is absent.
const prePhase0AMessagesSchema = `CREATE TABLE messages (
	id         TEXT PRIMARY KEY,
	claw_id    TEXT NOT NULL REFERENCES claws(id),
	tenant_id  TEXT NOT NULL,
	role       TEXT NOT NULL,
	content    TEXT NOT NULL,
	format     TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL,
	delivered_at DATETIME
)`

// The pre-phase-0a claw_prs schema: identical to the current one except
// state, merged, and merged_at are absent.
const prePhase0AClawPRsSchema = `CREATE TABLE claw_prs (
	id          TEXT PRIMARY KEY,
	claw_id     TEXT NOT NULL REFERENCES claws(id),
	repo        TEXT NOT NULL,
	pr_number   INTEGER NOT NULL,
	pr_url      TEXT NOT NULL,
	last_ci_sha TEXT NOT NULL DEFAULT '',
	last_ci_conclusion TEXT NOT NULL DEFAULT '',
	last_comment_id INTEGER NOT NULL DEFAULT 0,
	last_comment_at TEXT NOT NULL DEFAULT '',
	last_review_comment_id INTEGER NOT NULL DEFAULT 0,
	last_review_id INTEGER NOT NULL DEFAULT 0,
	pr_conditions_fired INTEGER NOT NULL DEFAULT 0,
	permanent_failure_count INTEGER NOT NULL DEFAULT 0,
	created_at  DATETIME NOT NULL,
	UNIQUE(claw_id, pr_url)
)`

// TestMigrateAppliesPhase0AColumnsToExistingDatabase verifies the full
// startup migration upgrades pre-phase-0a messages and claw_prs tables while
// preserving rows, and remains safe to run again.
func TestMigrateAppliesPhase0AColumnsToExistingDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE messages; DROP TABLE claw_prs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(prePhase0AMessagesSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(prePhase0AClawPRsSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tenants(id, name, token, claw_token, created_at) VALUES('tenant','tenant','token-1','claw-token-1',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, status, created_at) VALUES('claw-1','tenant','claw','running',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES('message-1','claw-1','tenant','user','old message',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, last_ci_sha, created_at) VALUES('pr-1','claw-1','acme/widgets',42,'https://github.com/acme/widgets/pull/42','abc123',1)`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate over pre-phase-0a schema: %v", err)
	}

	var userLogin sql.NullString
	var content string
	if err := db.QueryRow(`SELECT user_login, content FROM messages WHERE id='message-1'`).Scan(&userLogin, &content); err != nil {
		t.Fatalf("read migrated message: %v", err)
	}
	if userLogin.Valid {
		t.Fatalf("migrated user_login = %q, want NULL", userLogin.String)
	}
	if content != "old message" {
		t.Fatalf("migrated message content = %q, want %q", content, "old message")
	}

	var state string
	var merged int
	var mergedAt sql.NullString
	var repo, sha string
	if err := db.QueryRow(`SELECT state, merged, merged_at, repo, last_ci_sha FROM claw_prs WHERE id='pr-1'`).Scan(&state, &merged, &mergedAt, &repo, &sha); err != nil {
		t.Fatalf("read migrated claw_prs row: %v", err)
	}
	if state != "open" || merged != 0 || mergedAt.Valid {
		t.Fatalf("migrated claw_prs state/merged/merged_at = %q/%d/%v, want open/0/NULL", state, merged, mergedAt)
	}
	if repo != "acme/widgets" || sha != "abc123" {
		t.Fatalf("migrated claw_prs repo/last_ci_sha = %q/%q, want acme/widgets/abc123", repo, sha)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
