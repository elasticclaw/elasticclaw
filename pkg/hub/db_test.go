package hub

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestClawsTriggerActorJSONDefaultIsValidJSON(t *testing.T) {
	t.Run("fresh schema", func(t *testing.T) {
		db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		defer db.Close()

		now := time.Now()
		if _, err := db.Exec(`INSERT INTO tenants(id, name, token, claw_token, created_at) VALUES(?,?,?,?,?)`, "tenant", "Tenant", "token", "claw-token", now); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, provider, status, created_at) VALUES(?,?,?,?,?,?,?)`, "claw-default", "tenant", "Claw", "", "docker", "provisioning", now); err != nil {
			t.Fatalf("insert claw: %v", err)
		}

		assertClawTriggerActorJSONIsValid(t, db, "claw-default")
	})

	t.Run("migrated schema", func(t *testing.T) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hub.db")+"?_time_format=sqlite&_pragma=foreign_keys(on)")
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		defer db.Close()

		if _, err := db.Exec(`CREATE TABLE claws (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL DEFAULT '')`); err != nil {
			t.Fatalf("create old claws table: %v", err)
		}
		if err := migrate(db); err != nil {
			t.Fatalf("migrate old claws table: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO claws(id) VALUES(?)`, "claw-default"); err != nil {
			t.Fatalf("insert migrated claw: %v", err)
		}

		assertClawTriggerActorJSONIsValid(t, db, "claw-default")
	})
}

func assertClawTriggerActorJSONIsValid(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()

	var raw string
	if err := db.QueryRow(`SELECT trigger_actor_json FROM claws WHERE id=?`, clawID).Scan(&raw); err != nil {
		t.Fatalf("select trigger_actor_json: %v", err)
	}

	var actor map[string]any
	if err := json.Unmarshal([]byte(raw), &actor); err != nil {
		t.Fatalf("expected trigger_actor_json default to be valid JSON, got %q: %v", raw, err)
	}
}
