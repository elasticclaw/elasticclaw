package hub

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// An existing hub database predates last_ci_conclusion. The additive migration
// must backfill it as '' so previously observed SHAs are re-evaluated once and
// the poller's SELECT keeps working.
func TestMigrateAddsLastCIConclusionToExistingClawPRs(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Pre-migration shape of claw_prs.
	if _, err := db.Exec(`CREATE TABLE claw_prs (
		id TEXT PRIMARY KEY,
		claw_id TEXT NOT NULL,
		repo TEXT NOT NULL,
		pr_number INTEGER NOT NULL,
		pr_url TEXT NOT NULL,
		last_ci_sha TEXT NOT NULL DEFAULT '',
		last_comment_id INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,last_ci_sha,created_at) VALUES('p','c','o/r',1,'u','04cc3f49',datetime('now'))`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var sha, conclusion string
	if err := db.QueryRow(`SELECT last_ci_sha, last_ci_conclusion FROM claw_prs WHERE id='p'`).Scan(&sha, &conclusion); err != nil {
		t.Fatalf("select after migration: %v", err)
	}
	if sha != "04cc3f49" || conclusion != "" {
		t.Fatalf("row = (%q,%q), want (04cc3f49, \"\")", sha, conclusion)
	}
}
