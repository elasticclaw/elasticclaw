package hub

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

		if _, err := db.Exec(`INSERT INTO tenants(id, name, token, claw_token, created_at) VALUES(?,?,?,?,?)`, "tenant", "Tenant", "token", "claw-token", time.Now()); err != nil {
			t.Fatalf("insert tenant: %v", err)
		}
		insertClawUsingTriggerActorDefault(t, db, "claw-default")

		assertClawTriggerActorJSONIsValid(t, db, "claw-default")
	})

	t.Run("migrated schema", func(t *testing.T) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "hub.db")+"?_time_format=sqlite&_pragma=foreign_keys(on)")
		if err != nil {
			t.Fatalf("open db: %v", err)
		}
		defer db.Close()

		if _, err := db.Exec(`
			CREATE TABLE claws (
				id TEXT PRIMARY KEY,
				tenant_id TEXT NOT NULL,
				name TEXT NOT NULL,
				template TEXT NOT NULL DEFAULT '',
				provider TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL DEFAULT 'offline',
				created_at DATETIME NOT NULL
			)`); err != nil {
			t.Fatalf("create old claws table: %v", err)
		}
		if err := migrate(db); err != nil {
			t.Fatalf("migrate old claws table: %v", err)
		}
		insertClawUsingTriggerActorDefault(t, db, "claw-default")

		assertClawTriggerActorJSONIsValid(t, db, "claw-default")
	})
}

// TestMigrateIdempotent verifies that running migrate() twice on the same
// database (simulating a second boot against an already-migrated schema)
// succeeds without error — the "duplicate column name" errors from re-running
// ALTER TABLE ADD COLUMN must be swallowed.
func TestMigrateIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrate(db); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate (idempotent boot) failed: %v", err)
	}
}

// TestMigrateAbortsOnRealError verifies that a genuine migration failure
// (here: a read-only database, standing in for full-disk/lock conditions)
// aborts boot with a clear error instead of being silently swallowed.
func TestMigrateAbortsOnRealError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")

	// Create a bare "claws" table (no columns beyond id) so the very first
	// ALTER TABLE ADD COLUMN in migrate() is a genuinely new column — not one
	// already covered by the "duplicate column name"/"no such table"
	// allowances — once the file is made read-only.
	setup, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := setup.Exec(`CREATE TABLE claws (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("create bare claws table: %v", err)
	}
	if err := setup.Close(); err != nil {
		t.Fatalf("close setup db: %v", err)
	}

	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=foreign_keys(on)&mode=ro")
	if err != nil {
		t.Fatalf("open read-only db: %v", err)
	}
	defer db.Close()

	err = migrate(db)
	if err == nil {
		t.Fatal("expected migrate to fail against a read-only database, got nil error")
	}
	if !strings.Contains(err.Error(), "migrate:") {
		t.Fatalf("expected error to be wrapped with migrate context, got: %v", err)
	}
}

func insertClawUsingTriggerActorDefault(t *testing.T, db *sql.DB, clawID string) {
	t.Helper()

	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, provider, status, created_at) VALUES(?,?,?,?,?,?,?)`, clawID, "tenant", "Claw", "", "docker", "provisioning", time.Now()); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
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
