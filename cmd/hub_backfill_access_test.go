package cmd

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestHubBackfillAccessCommand(t *testing.T) {
	t.Run("backfills configured admins and message users", func(t *testing.T) {
		dbPath, configPath := newBackfillAccessFixture(t, []string{" Admin@Example.com "}, []string{"reader@example.com", "ADMIN@example.com"})
		output, err := runBackfillAccessCommand(t, dbPath, configPath, false)
		if err != nil {
			t.Fatalf("run backfill: %v", err)
		}
		if !strings.Contains(output, "2 users: 2 created") {
			t.Fatalf("unexpected output: %q", output)
		}
		assertBackfillRoles(t, dbPath, map[string]string{"admin@example.com": "admin", "reader@example.com": "reader"})
	})

	t.Run("is idempotent and does not demote admins", func(t *testing.T) {
		dbPath, configPath := newBackfillAccessFixture(t, []string{"admin@example.com"}, []string{"reader@example.com"})
		if _, err := runBackfillAccessCommand(t, dbPath, configPath, false); err != nil {
			t.Fatal(err)
		}
		output, err := runBackfillAccessCommand(t, dbPath, configPath, false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output, "2 users: 0 created, 0 updated, 2 unchanged") {
			t.Fatalf("second output: %q", output)
		}
		assertBackfillRoles(t, dbPath, map[string]string{"admin@example.com": "admin", "reader@example.com": "reader"})
		assertBackfillUserCount(t, dbPath, 2)
	})

	t.Run("dry run writes no users", func(t *testing.T) {
		dbPath, configPath := newBackfillAccessFixture(t, []string{"admin@example.com"}, []string{"reader@example.com"})
		if _, err := runBackfillAccessCommand(t, dbPath, configPath, false); err != nil {
			t.Fatal(err)
		}
		before := backfillUserCount(t, dbPath)
		if _, err := runBackfillAccessCommand(t, dbPath, configPath, true); err != nil {
			t.Fatal(err)
		}
		if after := backfillUserCount(t, dbPath); after != before {
			t.Fatalf("user count after dry run = %d, want %d", after, before)
		}
	})

	t.Run("missing admins warns and writes nothing", func(t *testing.T) {
		dbPath, configPath := newBackfillAccessFixture(t, nil, []string{"reader@example.com"})
		_, err := runBackfillAccessCommand(t, dbPath, configPath, false)
		if err == nil || !strings.Contains(err.Error(), "auth.access.admins is empty") {
			t.Fatalf("error = %v, want missing administrators warning", err)
		}
		if backfillTableExists(t, dbPath, "users") {
			t.Fatal("missing-admin invocation created the users table")
		}
	})
}

func newBackfillAccessFixture(t *testing.T, admins, messageLogins []string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tenants (id TEXT PRIMARY KEY, name TEXT NOT NULL, token TEXT NOT NULL UNIQUE, claw_token TEXT NOT NULL UNIQUE, created_at DATETIME NOT NULL);
		CREATE TABLE messages (id TEXT PRIMARY KEY, claw_id TEXT NOT NULL, tenant_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT NOT NULL, format TEXT NOT NULL DEFAULT '', user_login TEXT, created_at DATETIME NOT NULL, delivered_at DATETIME);
		INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES('tenant','test','token','claw-token',datetime('now'))`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	for i, login := range messageLogins {
		if _, err := db.Exec(`INSERT INTO messages(id,claw_id,tenant_id,role,content,user_login,created_at) VALUES(?,?,?,'user','message',?,datetime('now'))`, string(rune('a'+i)), "claw", "tenant", login); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "hub.yaml")
	config := "auth:\n  access:\n"
	if admins != nil {
		config += "    admins:\n"
		for _, admin := range admins {
			config += "      - " + admin + "\n"
		}
	}
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return dbPath, configPath
}

func runBackfillAccessCommand(t *testing.T, dbPath, configPath string, dryRun bool) (string, error) {
	t.Helper()
	oldDB, oldConfig, oldDryRun := backfillAccessDB, backfillAccessConfig, backfillAccessDryRun
	t.Cleanup(func() { backfillAccessDB, backfillAccessConfig, backfillAccessDryRun = oldDB, oldConfig, oldDryRun })
	backfillAccessDB, backfillAccessConfig, backfillAccessDryRun = dbPath, configPath, dryRun
	var output bytes.Buffer
	command := hubBackfillAccessCmd
	command.SetOut(&output)
	t.Cleanup(func() { command.SetOut(nil) })
	err := runHubBackfillAccess(command, nil)
	return output.String(), err
}

func assertBackfillRoles(t *testing.T, dbPath string, want map[string]string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for login, role := range want {
		var got string
		if err := db.QueryRow(`SELECT r.name FROM users u JOIN roles r ON r.id=u.role_id WHERE u.login=?`, login).Scan(&got); err != nil {
			t.Fatalf("role for %s: %v", login, err)
		}
		if got != role {
			t.Errorf("role for %s = %q, want %q", login, got, role)
		}
	}
}

func assertBackfillUserCount(t *testing.T, dbPath string, want int) {
	t.Helper()
	if got := backfillUserCount(t, dbPath); got != want {
		t.Fatalf("user count = %d, want %d", got, want)
	}
}

func backfillUserCount(t *testing.T, dbPath string) int {
	return backfillTableCount(t, dbPath, "users")
}

func backfillTableCount(t *testing.T, dbPath, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func backfillTableExists(t *testing.T, dbPath, table string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count != 0
}
