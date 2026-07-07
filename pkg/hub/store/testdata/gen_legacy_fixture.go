// gen_legacy_fixture.go regenerates hub-legacy.db. It lives under testdata so
// the toolchain ignores it; run it manually if the fixture ever needs to be
// rebuilt:
//
//	go run pkg/hub/store/testdata/gen_legacy_fixture.go pkg/hub/store/testdata/hub-legacy.db
package main

import (
	"database/sql"
	"log"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	path := os.Args[1]
	_ = os.Remove(path)
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE tenants (
			id        TEXT PRIMARY KEY,
			name      TEXT NOT NULL,
			token     TEXT NOT NULL UNIQUE,
			claw_token TEXT NOT NULL UNIQUE,
			created_at DATETIME NOT NULL
		)`,
		// A mid-vintage claws table: has early ALTER-added columns
		// (provider, factory_name, linear_issue_id) but none of the newer
		// ones (task_run_id, workflow_volumes, trigger_actor_json, ...).
		`CREATE TABLE claws (
			id             TEXT PRIMARY KEY,
			tenant_id      TEXT NOT NULL REFERENCES tenants(id),
			name           TEXT NOT NULL,
			template       TEXT NOT NULL DEFAULT '',
			provider       TEXT NOT NULL DEFAULT '',
			status         TEXT NOT NULL DEFAULT 'offline',
			last_seen      DATETIME,
			created_at     DATETIME NOT NULL,
			factory_name   TEXT NOT NULL DEFAULT '',
			linear_issue_id TEXT NOT NULL DEFAULT ''
		)`,
		// Messages before the format column existed.
		`CREATE TABLE messages (
			id         TEXT PRIMARY KEY,
			claw_id    TEXT NOT NULL REFERENCES claws(id),
			tenant_id  TEXT NOT NULL,
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			created_at DATETIME NOT NULL
		)`,
		`INSERT INTO tenants(id, name, token, claw_token, created_at)
		 VALUES('tenant-1', 'Fixture Tenant', 'token-1', 'claw-token-1', '2025-01-02 03:04:05')`,
		`INSERT INTO claws(id, tenant_id, name, template, provider, status, created_at, factory_name, linear_issue_id)
		 VALUES('claw-1', 'tenant-1', 'fixture-claw', 'default', 'docker', 'offline', '2025-01-02 03:04:06', 'fixture-factory', 'sc-123')`,
		`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at)
		 VALUES('msg-1', 'claw-1', 'tenant-1', 'user', 'hello from the old schema', '2025-01-02 03:04:07')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			log.Fatalf("%s: %v", s, err)
		}
	}
}
