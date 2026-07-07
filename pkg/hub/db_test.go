package hub

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/store"
)

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")

	for i := 0; i < 2; i++ {
		db, err := openDB(path)
		if err != nil {
			t.Fatalf("open db (boot %d): %v", i+1, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close db (boot %d): %v", i+1, err)
		}
	}
}

func TestMigrateFailsOnReadOnlyDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")

	// Create a valid database first so the file exists.
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("initial open db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// Make the database unwritable: read-only file in a read-only directory
	// (SQLite also needs the directory for the -wal/-journal files).
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod file: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
		_ = os.Chmod(path, 0o644)
	})

	_, err = openDB(path)
	if err == nil {
		t.Fatal("expected openDB to fail on a read-only database, got nil error")
	}
	if !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("expected a migration error mentioning 'migrate', got: %v", err)
	}
}

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
		if err := store.Migrate(db); err != nil {
			t.Fatalf("migrate old claws table: %v", err)
		}
		insertClawUsingTriggerActorDefault(t, db, "claw-default")

		assertClawTriggerActorJSONIsValid(t, db, "claw-default")
	})
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
