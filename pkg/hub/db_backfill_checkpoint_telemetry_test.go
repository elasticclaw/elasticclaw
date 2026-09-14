package hub

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func newTelemetryBackfillDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func writeTelemetryManifest(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func insertTelemetryRow(t *testing.T, db *sql.DB, id, manifestPath string, messageCount int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO claw_checkpoints(id, tenant_id, claw_id, status, manifest_path, message_count, created_at)
		VALUES(?,'tenant','claw','ready',?,?,datetime('now'))`, id, manifestPath, messageCount); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

type telemetryRow struct {
	stage, version                     string
	messageCount, filesCount, filesLen int64
}

func readTelemetryRow(t *testing.T, db *sql.DB, id string) telemetryRow {
	t.Helper()
	var row telemetryRow
	if err := db.QueryRow(`SELECT pipeline_stage, hub_version, message_count, files_count, files_bytes
		FROM claw_checkpoints WHERE id=?`, id).
		Scan(&row.stage, &row.version, &row.messageCount, &row.filesCount, &row.filesLen); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return row
}

// Both manifest schemas have to produce the same telemetry: schema 1 inlined
// files[] and schema 2 replaced it with the aggregates.
func TestBackfillCheckpointTelemetryHandlesBothManifestSchemas(t *testing.T) {
	cases := []struct {
		name string
		body string
		want telemetryRow
	}{
		{
			name: "schema 1 aggregates the inline file list",
			body: `{"schema":1,"hub":{"version":"1.2.3","pipeline_stage":"review"},
				"messages":{"count":7},
				"files":[{"path":"a","sha256":"aa","size":10},{"path":"b","sha256":"bb","size":32}]}`,
			want: telemetryRow{stage: "review", version: "1.2.3", messageCount: 7, filesCount: 2, filesLen: 42},
		},
		{
			name: "schema 2 reads the aggregates directly",
			body: `{"schema":2,"hub":{"version":"2.0.0","pipeline_stage":"build"},
				"messages":{"count":3},"files_count":9,"files_bytes":900}`,
			want: telemetryRow{stage: "build", version: "2.0.0", messageCount: 3, filesCount: 9, filesLen: 900},
		},
		{
			name: "an empty schema 2 workspace stays zero",
			body: `{"schema":2,"hub":{"version":"2.0.0","pipeline_stage":"build"},"messages":{"count":1}}`,
			want: telemetryRow{stage: "build", version: "2.0.0", messageCount: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTelemetryBackfillDB(t)
			insertTelemetryRow(t, db, "cp", writeTelemetryManifest(t, "cp.json", tc.body), 0)
			if err := backfillCheckpointTelemetryV1(db); err != nil {
				t.Fatalf("backfill: %v", err)
			}
			if got := readTelemetryRow(t, db, "cp"); got != tc.want {
				t.Fatalf("telemetry = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The backfill runs on every boot, so a second pass must change nothing, and it
// must never overwrite a message_count the row already carried.
func TestBackfillCheckpointTelemetryIsIdempotent(t *testing.T) {
	db := newTelemetryBackfillDB(t)
	path := writeTelemetryManifest(t, "cp.json",
		`{"schema":2,"hub":{"version":"2.0.0","pipeline_stage":"build"},"messages":{"count":3},"files_count":9,"files_bytes":900}`)
	insertTelemetryRow(t, db, "cp", path, 42)

	if err := backfillCheckpointTelemetryV1(db); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first := readTelemetryRow(t, db, "cp")
	if first.messageCount != 42 {
		t.Fatalf("backfill overwrote an existing message_count: %d", first.messageCount)
	}

	// Rewrite the manifest with different values. An idempotent backfill must
	// not touch the row again, because the row no longer needs filling.
	if err := os.WriteFile(path, []byte(
		`{"schema":2,"hub":{"version":"9.9.9","pipeline_stage":"other"},"files_count":1,"files_bytes":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backfillCheckpointTelemetryV1(db); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second := readTelemetryRow(t, db, "cp"); second != first {
		t.Fatalf("second pass changed the row: %+v -> %+v", first, second)
	}
}

// A manifest that is gone or corrupt is exactly the pre-existing damage this
// backfill exists to bound. It must be counted and skipped, never fatal, and it
// must not stop the rows that CAN be filled.
func TestBackfillCheckpointTelemetrySurvivesMissingManifest(t *testing.T) {
	db := newTelemetryBackfillDB(t)
	insertTelemetryRow(t, db, "cp-missing", filepath.Join(t.TempDir(), "gone.json"), 0)
	insertTelemetryRow(t, db, "cp-corrupt", writeTelemetryManifest(t, "corrupt.json", "{ not json"), 0)
	insertTelemetryRow(t, db, "cp-good", writeTelemetryManifest(t, "good.json",
		`{"schema":2,"hub":{"version":"2.0.0","pipeline_stage":"build"},"messages":{"count":3},"files_count":9,"files_bytes":900}`), 0)

	if err := backfillCheckpointTelemetryV1(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := (readTelemetryRow(t, db, "cp-good")); got.filesCount != 9 || got.version != "2.0.0" {
		t.Fatalf("readable manifest was not applied: %+v", got)
	}
	for _, id := range []string{"cp-missing", "cp-corrupt"} {
		if got := readTelemetryRow(t, db, id); got != (telemetryRow{}) {
			t.Fatalf("%s was modified despite an unusable manifest: %+v", id, got)
		}
	}
}

// Rows with no manifest path, or already-filled rows, are not candidates.
func TestBackfillCheckpointTelemetrySkipsIneligibleRows(t *testing.T) {
	db := newTelemetryBackfillDB(t)
	if _, err := db.Exec(`INSERT INTO claw_checkpoints(id, tenant_id, claw_id, status, created_at)
		VALUES('cp-skipped','tenant','claw','skipped',datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	insertTelemetryRow(t, db, "cp-nopath", "", 0)

	if err := backfillCheckpointTelemetryV1(db); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	for _, id := range []string{"cp-skipped", "cp-nopath"} {
		if got := readTelemetryRow(t, db, id); got != (telemetryRow{}) {
			t.Fatalf("%s was modified: %+v", id, got)
		}
	}
}
