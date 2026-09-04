package hub

import (
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// An existing hub database predates last_ci_conclusion. The additive migration
// must backfill it as ” so previously observed SHAs are re-evaluated once and
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

	// Every hub restart re-runs migrate() against a database that already has the
	// column. addColumn must absorb that, not abort startup.
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := db.QueryRow(`SELECT last_ci_conclusion FROM claw_prs WHERE id='p'`).Scan(&conclusion); err != nil {
		t.Fatalf("select after second migration: %v", err)
	}
	if conclusion != "" {
		t.Fatalf("conclusion = %q after re-migrate, want \"\"", conclusion)
	}
}

// A fresh database gets claw_prs from CREATE TABLE, already carrying the
// column; the default must still be ” so a newly tracked PR is unevaluated.
func TestMigrateFreshDBHasLastCIConclusionDefault(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// foreign_keys is off on this bare connection, so claw_prs can be inserted
	// without a parent claws row.
	if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES('p','c','o/r',1,'u',datetime('now'))`); err != nil {
		t.Fatal(err)
	}

	var conclusion string
	if err := db.QueryRow(`SELECT last_ci_conclusion FROM claw_prs WHERE id='p'`).Scan(&conclusion); err != nil {
		t.Fatalf("select: %v", err)
	}
	if conclusion != "" {
		t.Fatalf("conclusion = %q, want \"\"", conclusion)
	}
}

func TestAddColumn(t *testing.T) {
	newDB := func(t *testing.T) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		if _, err := db.Exec(`CREATE TABLE t (a TEXT NOT NULL DEFAULT '')`); err != nil {
			t.Fatal(err)
		}
		return db
	}

	t.Run("adds missing column", func(t *testing.T) {
		db := newDB(t)
		if err := addColumn(db, "t", "b", `TEXT NOT NULL DEFAULT 'x'`); err != nil {
			t.Fatalf("addColumn: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO t(a) VALUES('1')`); err != nil {
			t.Fatal(err)
		}
		var b string
		if err := db.QueryRow(`SELECT b FROM t`).Scan(&b); err != nil {
			t.Fatalf("select: %v", err)
		}
		if b != "x" {
			t.Fatalf("b = %q, want x", b)
		}
	})

	t.Run("existing column is a no-op", func(t *testing.T) {
		db := newDB(t)
		if err := addColumn(db, "t", "a", `TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatalf("addColumn on existing column: %v", err)
		}
	})

	t.Run("missing table is a no-op", func(t *testing.T) {
		db := newDB(t)
		if err := addColumn(db, "not_a_table", "b", `TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatalf("addColumn on missing table: %v", err)
		}
	})

	// The whole point of the helper: anything other than the two benign cases
	// must surface instead of leaving migrate() to report success on a database
	// that is missing the column.
	t.Run("unexpected error propagates", func(t *testing.T) {
		db := newDB(t)
		err := addColumn(db, "t", "b", `TEXT NOT NULL DEFAULT`)
		if err == nil {
			t.Fatal("addColumn with a malformed column def returned nil")
		}
		if !strings.Contains(err.Error(), "t.b") {
			t.Fatalf("error %q does not name the table and column", err)
		}
	})

	t.Run("closed db propagates", func(t *testing.T) {
		db := newDB(t)
		db.Close()
		if err := addColumn(db, "t", "b", `TEXT NOT NULL DEFAULT ''`); err == nil {
			t.Fatal("addColumn on a closed db returned nil")
		}
	})
}

func TestIsBenignAddColumnErr(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (a TEXT)`); err != nil {
		t.Fatal(err)
	}

	// Guard the fallback string match against the real driver messages: if
	// modernc.org/sqlite ever reworded these, the hub would start failing on
	// every restart after the first.
	_, dup := db.Exec(`ALTER TABLE t ADD COLUMN a TEXT NOT NULL DEFAULT ''`)
	if dup == nil || !isBenignAddColumnErr(dup) {
		t.Fatalf("duplicate column error not recognised: %v", dup)
	}
	_, missing := db.Exec(`ALTER TABLE nope ADD COLUMN a TEXT NOT NULL DEFAULT ''`)
	if missing == nil || !isBenignAddColumnErr(missing) {
		t.Fatalf("missing table error not recognised: %v", missing)
	}
	_, syntax := db.Exec(`ALTER TABLE t ADD COLUMN b TEXT NOT NULL DEFAULT`)
	if syntax == nil || isBenignAddColumnErr(syntax) {
		t.Fatalf("syntax error wrongly treated as benign: %v", syntax)
	}
}
