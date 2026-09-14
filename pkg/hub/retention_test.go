package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newRetentionTestServer gives every test its own HOME, so checkpointsRoot and
// hubDataDir resolve inside the test's temp directory and a sweep can never
// touch the developer's real ~/.elasticclaw.
func newRetentionTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES(?,?,?,?,?)`,
		"tenant", "tenant", "token", "claw-token", now()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return &Server{db: db, hubCfg: &types.HubConfig{}, claws: map[string]*clawConn{}}
}

// insertRetentionClaw seeds a claw in a status the finalized predicate treats
// as genuinely done. 'idle' used to be the default here, which quietly made
// every fixture depend on idle counting as finished — it does not: an idle claw
// is resumable by design, so the merged-PR arm now excludes it and only the
// staleness arm can finalize it. Tests that want a live claw set the status
// themselves.
func insertRetentionClaw(t *testing.T, s *Server, id string, lastSeen time.Time) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, last_seen, created_at) VALUES(?,?,?,?,?,?,?)`,
		id, "tenant", id, "template", "completed", lastSeen, lastSeen); err != nil {
		t.Fatalf("insert claw %s: %v", id, err)
	}
}

func insertRetentionPR(t *testing.T, s *Server, clawID string, merged int) {
	t.Helper()
	// state must agree with merged. The predicate now uses the hub's canonical
	// unresolved-PR rule, which keys on state, and a row with merged=1 left at
	// state='open' is a shape the hub never writes -- db.go even carries a
	// migration to reconcile the two. Seeding it would test a fiction.
	state := "open"
	if merged == 1 {
		state = "merged"
	}
	if _, err := s.db.Exec(
		`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, state, merged, created_at) VALUES(?,?,?,?,?,?,?,?)`,
		clawID+"-pr", clawID, "owner/repo", 1, "https://example.test/pr/1", state, merged, now()); err != nil {
		t.Fatalf("insert pr for %s: %v", clawID, err)
	}
}

type retentionCheckpoint struct {
	id            string
	clawID        string
	status        string
	createdAt     time.Time
	rootTree      string
	messageTree   string
	writeManifest bool
	manifestBody  []byte
}

func insertRetentionCheckpoint(t *testing.T, s *Server, cp retentionCheckpoint) string {
	t.Helper()
	manifestPath := ""
	manifestSHA := ""
	if cp.writeManifest {
		body := cp.manifestBody
		if body == nil {
			body, _ = json.Marshal(checkpointManifest{
				Schema:       checkpointManifestSchema,
				CheckpointID: cp.id,
				ClawID:       cp.clawID,
				Workspace:    checkpointWorkspace{TreeSHA256: cp.rootTree},
				Messages:     checkpointMessages{BlobSHA256: cp.messageTree},
			})
		}
		manifestPath = checkpointManifestPath(cp.id)
		if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
			t.Fatalf("make manifest dir: %v", err)
		}
		if err := os.WriteFile(manifestPath, body, 0o640); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		manifestSHA = shaBytes(body)
	}
	if _, err := s.db.Exec(`INSERT INTO claw_checkpoints(
			id, tenant_id, claw_id, status, manifest_path, manifest_sha256,
			root_tree_sha256, message_tree_sha256, workspace_tree_sha256, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		cp.id, "tenant", cp.clawID, cp.status, manifestPath, manifestSHA,
		cp.rootTree, cp.messageTree, cp.rootTree, cp.createdAt); err != nil {
		t.Fatalf("insert checkpoint %s: %v", cp.id, err)
	}
	return manifestPath
}

func writeRetentionBlob(t *testing.T, contents []byte) string {
	t.Helper()
	sum := sha256.Sum256(contents)
	sha := hex.EncodeToString(sum[:])
	path := checkpointBlobPath(sha)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("make blob dir: %v", err)
	}
	if err := os.WriteFile(path, contents, 0o640); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return sha
}

func retentionCheckpointRow(t *testing.T, s *Server, id string) (status, manifestPath, manifestSHA string) {
	t.Helper()
	if err := s.db.QueryRow(
		`SELECT status, manifest_path, manifest_sha256 FROM claw_checkpoints WHERE id=?`, id).
		Scan(&status, &manifestPath, &manifestSHA); err != nil {
		t.Fatalf("read checkpoint %s: %v", id, err)
	}
	return
}

// The finalized predicate has two independent arms and both matter: a merged PR
// finalizes immediately no matter how fresh the claw is, and silence for
// compact_after finalizes a claw that never produced one.
func TestClawFinalizedPredicate(t *testing.T) {
	const compactAfter = 240 * time.Hour
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cutoff := reference.Add(-compactAfter)

	cases := []struct {
		name            string
		lastSeen        time.Time
		merged          *int
		checkpointAt    *time.Time
		wantFinalized   bool
		wantExplanation string
	}{
		{
			name:            "merged pr finalizes an active claw",
			lastSeen:        reference,
			merged:          intPtr(1),
			wantFinalized:   true,
			wantExplanation: "arm (a): the work landed, nothing more is coming",
		},
		{
			name:            "stale claw with no recent checkpoint",
			lastSeen:        reference.Add(-compactAfter - time.Hour),
			wantFinalized:   true,
			wantExplanation: "arm (b): unchanged for longer than compact_after",
		},
		{
			name:            "stale claw with a checkpoint inside the window",
			lastSeen:        reference.Add(-compactAfter - time.Hour),
			checkpointAt:    timePtr(reference.Add(-time.Hour)),
			wantFinalized:   false,
			wantExplanation: "still checkpointing, so it is still changing",
		},
		{
			name:            "stale claw whose only checkpoint predates the window",
			lastSeen:        reference.Add(-compactAfter - time.Hour),
			checkpointAt:    timePtr(reference.Add(-compactAfter - 2*time.Hour)),
			wantFinalized:   true,
			wantExplanation: "an old checkpoint does not revive arm (b)",
		},
		{
			name:            "recently seen claw with no pr",
			lastSeen:        reference.Add(-time.Hour),
			wantFinalized:   false,
			wantExplanation: "neither arm applies",
		},
		{
			name:            "recently seen claw with an unmerged pr",
			lastSeen:        reference.Add(-time.Hour),
			merged:          intPtr(0),
			wantFinalized:   false,
			wantExplanation: "an open PR is not a landed one",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			insertRetentionClaw(t, s, "claw", tc.lastSeen)
			if tc.merged != nil {
				insertRetentionPR(t, s, "claw", *tc.merged)
			}
			if tc.checkpointAt != nil {
				insertRetentionCheckpoint(t, s, retentionCheckpoint{
					id: "cp", clawID: "claw", status: "ready", createdAt: *tc.checkpointAt})
			}
			got, err := s.clawFinalized("claw", cutoff)
			if err != nil {
				t.Fatalf("clawFinalized: %v", err)
			}
			if got != tc.wantFinalized {
				t.Fatalf("clawFinalized = %v, want %v (%s)", got, tc.wantFinalized, tc.wantExplanation)
			}
			ids, err := s.finalizedClawIDs(cutoff)
			if err != nil {
				t.Fatalf("finalizedClawIDs: %v", err)
			}
			if tc.wantFinalized != (len(ids) == 1) {
				t.Fatalf("finalizedClawIDs = %v, want finalized=%v", ids, tc.wantFinalized)
			}
		})
	}
}

func intPtr(v int) *int              { return &v }
func timePtr(v time.Time) *time.Time { return &v }

// Compaction must leave exactly one restorable checkpoint per finalized claw:
// the newest ready one. Everything else loses its manifest but keeps its row,
// because the row is the analytics record.
func TestCompactFinalizedCheckpointsKeepsNewestReady(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insertRetentionClaw(t, s, "claw", reference)
	insertRetentionPR(t, s, "claw", 1)

	oldest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-old", clawID: "claw", status: "ready", createdAt: reference.Add(-72 * time.Hour), writeManifest: true})
	middle := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-mid", clawID: "claw", status: "ready", createdAt: reference.Add(-48 * time.Hour), writeManifest: true})
	newest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-new", clawID: "claw", status: "ready", createdAt: reference.Add(-24 * time.Hour), writeManifest: true})
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-skipped", clawID: "claw", status: "skipped", createdAt: reference.Add(-time.Hour)})
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-failed", clawID: "claw", status: "failed", createdAt: reference.Add(-time.Hour)})

	compacted, err := s.compactFinalizedCheckpoints(reference.Add(-240*time.Hour), false)
	if err != nil {
		t.Fatalf("compactFinalizedCheckpoints: %v", err)
	}
	if compacted.count != 2 {
		t.Fatalf("compacted %d checkpoints, want 2", compacted.count)
	}

	for _, id := range []string{"cp-old", "cp-mid"} {
		status, path, sha := retentionCheckpointRow(t, s, id)
		if status != "compacted" {
			t.Errorf("%s status = %q, want compacted", id, status)
		}
		if path != "" || sha != "" {
			t.Errorf("%s kept manifest references path=%q sha=%q", id, path, sha)
		}
	}
	for _, path := range []string{oldest, middle} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("manifest %s still exists", path)
		}
	}
	status, path, sha := retentionCheckpointRow(t, s, "cp-new")
	if status != "ready" || path != newest || sha == "" {
		t.Errorf("newest checkpoint was compacted: status=%q path=%q sha=%q", status, path, sha)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("newest manifest missing: %v", err)
	}
	for _, id := range []string{"cp-skipped", "cp-failed"} {
		status, _, _ := retentionCheckpointRow(t, s, id)
		if id == "cp-skipped" && status != "skipped" {
			t.Errorf("skipped checkpoint changed to %q", status)
		}
		if id == "cp-failed" && status != "failed" {
			t.Errorf("failed checkpoint changed to %q", status)
		}
	}

	// Compaction must never delete a row: the telemetry columns are the record.
	var rowCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_checkpoints`).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 5 {
		t.Fatalf("row count = %d, want 5; compaction deleted analytics rows", rowCount)
	}
}

func TestCompactFinalizedCheckpointsSkipsUnfinalizedClaw(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insertRetentionClaw(t, s, "claw", reference)
	first := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-1", clawID: "claw", status: "ready", createdAt: reference.Add(-2 * time.Hour), writeManifest: true})
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-2", clawID: "claw", status: "ready", createdAt: reference.Add(-time.Hour), writeManifest: true})

	compacted, err := s.compactFinalizedCheckpoints(reference.Add(-240*time.Hour), false)
	if err != nil {
		t.Fatalf("compactFinalizedCheckpoints: %v", err)
	}
	if compacted.count != 0 {
		t.Fatalf("compacted %d checkpoints of a live claw, want 0", compacted.count)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("manifest of a live claw was removed: %v", err)
	}
}

// Retention deletes across all four targets on the same window.
func TestApplyRetentionWindowDeletesAllTargets(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cutoff := reference.Add(-90 * 24 * time.Hour)
	old, fresh := cutoff.Add(-24*time.Hour), cutoff.Add(24*time.Hour)

	insertRetentionClaw(t, s, "claw", reference)

	// Diagnostics logs, aged by mtime because they have no database row.
	diagDir := filepath.Join(hubDataDir(), "diagnostics")
	if err := os.MkdirAll(diagDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldLog := filepath.Join(diagDir, "claw-old-gateway.log")
	freshLog := filepath.Join(diagDir, "claw-fresh-gateway.log")
	for path, mtime := range map[string]time.Time{oldLog: old, freshLog: fresh} {
		if err := os.WriteFile(path, []byte("log"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	oldManifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-old", clawID: "claw", status: "ready", createdAt: old, writeManifest: true})
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp-fresh", clawID: "claw", status: "ready", createdAt: fresh, writeManifest: true})

	if _, err := s.db.Exec(`INSERT INTO task_runs(id, tenant_id, initial_attempt_id, run_kind, owner_type, created_at, updated_at)
		VALUES('run','tenant','attempt','code_task','manual',?,?)`, old.UnixMilli(), old.UnixMilli()); err != nil {
		t.Fatalf("insert task run: %v", err)
	}
	for _, ev := range []struct {
		id string
		at time.Time
	}{{"ev-old", old}, {"ev-fresh", fresh}} {
		if _, err := s.db.Exec(`INSERT INTO task_run_events(id, tenant_id, run_id, event_key, event_type, event_time, observed_at, created_at)
			VALUES(?,'tenant','run',?,'task_start',?,?,?)`,
			ev.id, ev.id, ev.at.UnixMilli(), ev.at.UnixMilli(), ev.at.UnixMilli()); err != nil {
			t.Fatalf("insert event %s: %v", ev.id, err)
		}
	}
	for _, msg := range []struct {
		id string
		at time.Time
	}{{"msg-old", old}, {"msg-fresh", fresh}} {
		if _, err := s.db.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES(?,?,?,?,?,?)`,
			msg.id, "claw", "tenant", "user", "hello", msg.at); err != nil {
			t.Fatalf("insert message %s: %v", msg.id, err)
		}
	}

	s.applyRetentionWindow(cutoff, false)

	if _, err := os.Stat(oldLog); !os.IsNotExist(err) {
		t.Errorf("expired diagnostics log survived")
	}
	if _, err := os.Stat(freshLog); err != nil {
		t.Errorf("in-window diagnostics log was deleted: %v", err)
	}
	if _, err := os.Stat(oldManifest); !os.IsNotExist(err) {
		t.Errorf("expired manifest survived")
	}
	for table, want := range map[string][]string{
		"claw_checkpoints": {"cp-fresh"},
		"task_run_events":  {"ev-fresh"},
		"messages":         {"msg-fresh"},
	} {
		rows, err := s.db.Query(`SELECT id FROM ` + table)
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		rows.Close()
		sort.Strings(got)
		if len(got) != len(want) || (len(got) == 1 && got[0] != want[0]) {
			t.Errorf("%s survivors = %v, want %v", table, got, want)
		}
	}
}

// The keep set must reach every blob a surviving checkpoint can restore from:
// the digests on the row, the digests inside the manifest, and — the one that
// would be catastrophic to miss — the per-file blobs listed inside the tree.
func TestCheckpointBlobKeepSetCoversRowsManifestsAndTrees(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insertRetentionClaw(t, s, "claw", reference)

	fileSHA := writeRetentionBlob(t, []byte("workspace file contents"))
	tree, _ := json.Marshal([]types.CheckpointFile{{Path: "workspace/a.txt", SHA256: fileSHA, Size: 23}})
	treeSHA := writeRetentionBlob(t, tree)
	messageSHA := writeRetentionBlob(t, []byte(`{"id":"m1"}`))
	orphanSHA := writeRetentionBlob(t, []byte("nothing references this"))

	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: treeSHA, messageTree: messageSHA, writeManifest: true})

	keep, err := s.checkpointBlobKeepSet(nil)
	if err != nil {
		t.Fatalf("checkpointBlobKeepSet: %v", err)
	}
	for _, sha := range []string{treeSHA, messageSHA, fileSHA} {
		if _, ok := keep[sha]; !ok {
			t.Errorf("keep set is missing %s", sha)
		}
	}
	if _, ok := keep[orphanSHA]; ok {
		t.Errorf("keep set retained an unreferenced blob")
	}

	// Age the orphan past the sweep's grace window. Blobs written moments ago
	// are spared on purpose -- an upload lands before the manifest referencing
	// it exists -- so a freshly written orphan is not yet eligible.
	aged := time.Now().Add(-2 * blobSweepGrace)
	if err := os.Chtimes(checkpointBlobPath(orphanSHA), aged, aged); err != nil {
		t.Fatal(err)
	}

	removed, _, _, err := s.sweepCheckpointBlobs(false, nil)
	if err != nil {
		t.Fatalf("sweepCheckpointBlobs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d blobs, want 1", removed)
	}
	for _, sha := range []string{treeSHA, messageSHA, fileSHA} {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Errorf("sweep deleted a referenced blob %s: %v", sha, err)
		}
	}
	if _, err := os.Stat(checkpointBlobPath(orphanSHA)); !os.IsNotExist(err) {
		t.Errorf("orphan blob survived the sweep")
	}
}

// A compacted checkpoint has no manifest left, so the tree digests on its row
// are the only thing standing between its blobs and deletion.
func TestCheckpointBlobKeepSetIncludesDatabaseTreeShasWithoutManifest(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insertRetentionClaw(t, s, "claw", reference)

	fileSHA := writeRetentionBlob(t, []byte("still referenced"))
	tree, _ := json.Marshal([]types.CheckpointFile{{Path: "workspace/a.txt", SHA256: fileSHA, Size: 16}})
	treeSHA := writeRetentionBlob(t, tree)
	messageSHA := writeRetentionBlob(t, []byte(`{"id":"m1"}`))

	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "compacted", createdAt: reference,
		rootTree: treeSHA, messageTree: messageSHA})

	keep, err := s.checkpointBlobKeepSet(nil)
	if err != nil {
		t.Fatalf("checkpointBlobKeepSet: %v", err)
	}
	for _, sha := range []string{treeSHA, messageSHA, fileSHA} {
		if _, ok := keep[sha]; !ok {
			t.Errorf("keep set is missing %s from a manifest-less row", sha)
		}
	}
}

// An unreadable manifest means the keep set cannot be proven complete. Deleting
// on a partial keep set silently destroys restore, so the sweep must abort and
// leave the disk full instead.
func TestSweepCheckpointBlobsAbortsOnUnparseableManifest(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insertRetentionClaw(t, s, "claw", reference)

	orphanSHA := writeRetentionBlob(t, []byte("would be swept if the sweep ran"))
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference,
		writeManifest: true, manifestBody: []byte("{ this is not json")})

	if _, err := s.checkpointBlobKeepSet(nil); err == nil {
		t.Fatal("checkpointBlobKeepSet accepted an unparseable manifest")
	}
	removed, _, _, err := s.sweepCheckpointBlobs(false, nil)
	if err == nil {
		t.Fatal("sweepCheckpointBlobs proceeded with an incomplete keep set")
	}
	if removed != 0 {
		t.Fatalf("removed %d blobs despite aborting", removed)
	}
	if _, err := os.Stat(checkpointBlobPath(orphanSHA)); err != nil {
		t.Fatalf("aborted sweep still deleted a blob: %v", err)
	}
}

// A present but unparseable tree blob is the same problem one level down: the
// files it references cannot be enumerated, so nothing may be deleted.
func TestSweepCheckpointBlobsAbortsOnUnparseableTreeBlob(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insertRetentionClaw(t, s, "claw", reference)

	treeSHA := writeRetentionBlob(t, []byte("not a tree"))
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference, rootTree: treeSHA})

	if _, _, _, err := s.sweepCheckpointBlobs(false, nil); err == nil {
		t.Fatal("sweepCheckpointBlobs proceeded past an unparseable tree blob")
	}
}

func TestRetentionSettingsDefaultsAndOverrides(t *testing.T) {
	cases := []struct {
		name                           string
		cfg                            *types.RetentionConfig
		interval, maxAge, compactAfter time.Duration
		enabled                        bool
	}{
		{
			// Absent section means the operator has said nothing, which must not
			// be read as consent to delete.
			name: "absent section is opt-out", cfg: nil, enabled: false,
			interval: defaultRetentionInterval, maxAge: defaultRetentionMaxAge, compactAfter: defaultRetentionCompactAfter,
		},
		{
			name:    "explicit values above the floors are honoured",
			cfg:     &types.RetentionConfig{Enabled: boolPtr(true), Interval: "15m", MaxAge: "720h", CompactAfter: "48h"},
			enabled: true, interval: 15 * time.Minute, maxAge: 720 * time.Hour, compactAfter: 48 * time.Hour,
		},
		{
			// Every one of these parses cleanly and would still be a disaster.
			name:    "dangerous values are clamped to the floors",
			cfg:     &types.RetentionConfig{Enabled: boolPtr(true), Interval: "1s", MaxAge: "1s", CompactAfter: "1s"},
			enabled: true, interval: minRetentionInterval, maxAge: minRetentionMaxAge, compactAfter: minRetentionCompactAfter,
		},
		{
			// compact_after past max_age would delete a checkpoint before it was
			// ever eligible for the cheaper compaction step.
			name:    "compact_after beyond max_age falls back",
			cfg:     &types.RetentionConfig{Enabled: boolPtr(true), MaxAge: "480h", CompactAfter: "960h"},
			enabled: true, interval: defaultRetentionInterval, maxAge: 480 * time.Hour, compactAfter: 240 * time.Hour,
		},
		{
			// The replacement must itself satisfy the ordering. max_age here is
			// above the 7d floor but below the 10d default, so falling back to
			// the default would leave compact_after >= max_age all the same.
			name:    "the fallback respects the ordering it enforces",
			cfg:     &types.RetentionConfig{Enabled: boolPtr(true), MaxAge: "192h", CompactAfter: "500h"},
			enabled: true, interval: defaultRetentionInterval, maxAge: 192 * time.Hour, compactAfter: 96 * time.Hour,
		},
		{
			name:    "garbage falls back per field",
			cfg:     &types.RetentionConfig{Enabled: boolPtr(true), Interval: "banana", MaxAge: "-5h", CompactAfter: ""},
			enabled: true, interval: defaultRetentionInterval, maxAge: defaultRetentionMaxAge, compactAfter: defaultRetentionCompactAfter,
		},
		{
			name: "master switch off", cfg: &types.RetentionConfig{Enabled: boolPtr(false)}, enabled: false,
			interval: defaultRetentionInterval, maxAge: defaultRetentionMaxAge, compactAfter: defaultRetentionCompactAfter,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{hubCfg: &types.HubConfig{Retention: tc.cfg}}
			got := s.retentionSettings()
			if got.interval != tc.interval || got.maxAge != tc.maxAge || got.compactAfter != tc.compactAfter {
				t.Fatalf("settings = %+v, want interval=%s maxAge=%s compactAfter=%s",
					got, tc.interval, tc.maxAge, tc.compactAfter)
			}
			if s.retentionEnabled() != tc.enabled {
				t.Fatalf("retentionEnabled = %v, want %v", s.retentionEnabled(), tc.enabled)
			}
		})
	}
}

// A disabled sweeper must not delete anything, including through the phases
// that do not read the master switch themselves.
func TestRetentionSweepOnceRespectsMasterSwitch(t *testing.T) {
	s := newRetentionTestServer(t)
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(false)}}
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	insertRetentionClaw(t, s, "claw", reference.Add(-400*24*time.Hour))
	manifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference.Add(-400 * 24 * time.Hour), writeManifest: true})

	s.retentionSweepOnce()

	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("disabled sweeper deleted a manifest: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_checkpoints`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("disabled sweeper deleted %d rows", 1-count)
	}
}

// A blob written moments ago is legitimately absent from the keep set: the
// upload lands before the manifest that references it. Deleting it leaves the
// checkpoint about to be published unrestorable.
func TestSweepCheckpointBlobsSparesRecentAndInFlightBlobs(t *testing.T) {
	s := newRetentionTestServer(t)
	blobDir := filepath.Join(checkpointsRoot(), "blobs", "sha256", "ab", "cd")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphanOld := filepath.Join(blobDir, strings.Repeat("a", 64))
	orphanNew := filepath.Join(blobDir, strings.Repeat("b", 64))
	inFlight := filepath.Join(blobDir, strings.Repeat("c", 64)+".tmp-1234")
	for _, p := range []string{orphanOld, orphanNew, inFlight} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Only the old orphan is eligible.
	old := time.Now().Add(-2 * blobSweepGrace)
	if err := os.Chtimes(orphanOld, old, old); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(orphanOld); !os.IsNotExist(err) {
		t.Error("an orphan older than the grace window should have been swept")
	}
	if _, err := os.Stat(orphanNew); err != nil {
		t.Error("a blob written inside the grace window must be spared: its manifest may not exist yet")
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Error("an in-flight .tmp- upload must never be swept")
	}
}

// A claw that merged one PR can still be running and still have other PRs open.
// Compacting it can destroy the older checkpoints retryCheckpointID falls back to.
func TestClawFinalizedIgnoresMergedPRWhileClawIsActive(t *testing.T) {
	s := newRetentionTestServer(t)
	cutoff := time.Now().Add(-240 * time.Hour)
	// last_seen is recent on purpose: the staleness arm must stay inert so the
	// merged-PR arm is the only thing that can finalize this claw, which is the
	// behaviour under test.
	insertRetentionClaw(t, s, "live", time.Now())
	if _, err := s.db.Exec(`UPDATE claws SET status='connected' WHERE id='live'`); err != nil {
		t.Fatal(err)
	}
	insertRetentionPR(t, s, "live", 1)

	finalized, err := s.clawFinalized("live", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if finalized {
		t.Error("a connected claw must not be finalized by a merged PR alone")
	}

	// Same claw, now offline but still holding an open PR. 'offline' is itself
	// a live status for this arm — a dropped WebSocket usually comes back — so
	// this stays unfinalized for two independent reasons.
	if _, err := s.db.Exec(`UPDATE claws SET status='offline' WHERE id='live'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, state, merged, created_at)
		 VALUES('live-pr2','live','owner/repo',2,'https://example.test/pr/2','open',0,?)`, now()); err != nil {
		t.Fatal(err)
	}
	finalized, err = s.clawFinalized("live", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if finalized {
		t.Error("a claw with a still-open PR must not be finalized by the merged arm")
	}

	// Open PR resolved, but the claw is still merely offline: it can reconnect,
	// so the merged-PR arm still must not fire. See
	// TestFinalizedPredicateTreatsOfflineAndIdleAsLive.
	if _, err := s.db.Exec(`UPDATE claw_prs SET state='closed' WHERE id='live-pr2'`); err != nil {
		t.Fatal(err)
	}
	finalized, err = s.clawFinalized("live", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if finalized {
		t.Error("an offline claw can still reconnect; a merged PR alone must not finalize it")
	}

	// Terminal status and nothing open: now it is genuinely done.
	if _, err := s.db.Exec(`UPDATE claws SET status='completed' WHERE id='live'`); err != nil {
		t.Fatal(err)
	}
	finalized, err = s.clawFinalized("live", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if !finalized {
		t.Error("a completed claw with a merged PR and nothing open is finalized")
	}
}

// A mention_only row is a PR URL the message scanner noticed; it gates nothing
// anywhere else in the hub, so it must not be able to hold compaction hostage.
func TestClawFinalizedIgnoresMentionOnlyPRs(t *testing.T) {
	s := newRetentionTestServer(t)
	cutoff := time.Now().Add(-240 * time.Hour)
	insertRetentionClaw(t, s, "claw", time.Now())
	insertRetentionPR(t, s, "claw", 1) // a genuinely merged PR

	if _, err := s.db.Exec(
		`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, state, merged, mention_only, created_at)
		 VALUES('claw-mention','claw','owner/repo',2,'https://example.test/pr/2','open',0,1,?)`, now()); err != nil {
		t.Fatal(err)
	}
	finalized, err := s.clawFinalized("claw", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if !finalized {
		t.Error("a mention-only row must not block finalization; it gates nothing elsewhere")
	}

	// A delivered row in a state that is neither merged nor closed does block.
	if _, err := s.db.Exec(`UPDATE claw_prs SET mention_only=0 WHERE id='claw-mention'`); err != nil {
		t.Fatal(err)
	}
	finalized, err = s.clawFinalized("claw", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if finalized {
		t.Error("a delivered unresolved PR must block finalization")
	}
}

// A referenced tree that is missing leaves the keep set incomplete: under
// schema 2 the per-file digests live only inside it. Sweeping anyway would
// turn one lost object into the loss of everything the checkpoint still had.
func TestSweepCheckpointBlobsAbortsOnMissingTreeBlob(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)

	orphan := writeRetentionBlob(t, []byte("unreferenced"))
	aged := time.Now().Add(-2 * blobSweepGrace)
	if err := os.Chtimes(checkpointBlobPath(orphan), aged, aged); err != nil {
		t.Fatal(err)
	}
	// A row referencing a tree digest whose blob was never written.
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference,
		rootTree: strings.Repeat("f", 64), writeManifest: true})

	if _, _, _, err := s.sweepCheckpointBlobs(false, nil); err == nil {
		t.Fatal("sweep must abort when a referenced tree blob is missing")
	}
	if _, err := os.Stat(checkpointBlobPath(orphan)); err != nil {
		t.Error("an aborted sweep must not have deleted anything")
	}
}

// ---------------------------------------------------------------------------
// In-flight checkpoints (pending blob claims)
// ---------------------------------------------------------------------------

// A checkpoint under construction owns its blobs even though its row carries no
// digests yet. The mtime grace window cannot carry that on its own: the plan
// handler and the upload handler both deduplicate on os.Stat, so a REUSED blob
// keeps an old mtime and would otherwise look exactly like an orphan.
func TestCheckpointBlobKeepSetHonoursPendingClaims(t *testing.T) {
	cases := []struct {
		name          string
		status        string
		clearClaim    bool
		wantKept      bool
		wantExplained string
	}{
		{
			name: "creating checkpoint keeps its planned blob", status: "creating",
			wantKept:      true,
			wantExplained: "the row has no digests yet; the claim is the only reference",
		},
		{
			name: "completed checkpoint releases the claim", status: "ready", clearClaim: true,
			wantKept:      false,
			wantExplained: "the row's own digests took over, and this blob is in none of them",
		},
		{
			name: "a claim whose checkpoint failed protects nothing", status: "failed", clearClaim: true,
			wantKept:      false,
			wantExplained: "a failed checkpoint will never reference it",
		},
		{
			name: "a stale claim under a non-creating row is ignored", status: "ready",
			wantKept:      false,
			wantExplained: "the keep set joins on status='creating', so a leaked row cannot pin blobs forever",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)

			// Written long ago and shared: exactly the deduplicated blob the
			// grace window fails to protect.
			plannedSHA := writeRetentionBlob(t, []byte("a blob an earlier claw already uploaded"))
			aged := time.Now().Add(-2 * blobSweepGrace)
			if err := os.Chtimes(checkpointBlobPath(plannedSHA), aged, aged); err != nil {
				t.Fatal(err)
			}

			// The claim is always recorded while the row is 'creating' — that
			// is the only state in which the plan handler can accept one — and
			// the row is then moved to the status under test, exactly as a real
			// checkpoint moves.
			insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "creating", createdAt: reference})
			if err := s.recordCheckpointPendingBlobs("cp", []types.CheckpointFile{
				{Path: "workspace/a.txt", SHA256: plannedSHA, Size: 39},
			}); err != nil {
				t.Fatalf("recordCheckpointPendingBlobs: %v", err)
			}
			if tc.status != "creating" {
				if _, err := s.db.Exec(`UPDATE claw_checkpoints SET status=? WHERE id='cp'`, tc.status); err != nil {
					t.Fatal(err)
				}
			}
			if tc.clearClaim {
				s.clearCheckpointPendingBlobs("cp")
			}

			keep, err := s.checkpointBlobKeepSet(nil)
			if err != nil {
				t.Fatalf("checkpointBlobKeepSet: %v", err)
			}
			if _, ok := keep[plannedSHA]; ok != tc.wantKept {
				t.Fatalf("keep set contains planned blob = %v, want %v (%s)", ok, tc.wantKept, tc.wantExplained)
			}

			if _, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil {
				t.Fatalf("sweepCheckpointBlobs: %v", err)
			}
			_, statErr := os.Stat(checkpointBlobPath(plannedSHA))
			if survived := statErr == nil; survived != tc.wantKept {
				t.Fatalf("blob survived the sweep = %v, want %v (%s)", survived, tc.wantKept, tc.wantExplained)
			}
		})
	}
}

// The full lifecycle in one pass: a claim protects a reused blob across a sweep
// while the checkpoint is creating, and stops protecting it once the checkpoint
// has completed and the row's own digests are authoritative.
func TestPendingClaimProtectsThenReleasesAcrossSweeps(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)

	plannedSHA := writeRetentionBlob(t, []byte("deduplicated from a checkpoint taken months ago"))
	aged := time.Now().Add(-2 * blobSweepGrace)
	if err := os.Chtimes(checkpointBlobPath(plannedSHA), aged, aged); err != nil {
		t.Fatal(err)
	}
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "creating", createdAt: reference})
	if err := s.recordCheckpointPendingBlobs("cp", []types.CheckpointFile{
		{Path: "workspace/a.txt", SHA256: plannedSHA, Size: 46},
	}); err != nil {
		t.Fatalf("recordCheckpointPendingBlobs: %v", err)
	}

	if removed, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil || removed != 0 {
		t.Fatalf("sweep removed %d blobs (err %v) while the checkpoint was creating", removed, err)
	}
	if _, err := os.Stat(checkpointBlobPath(plannedSHA)); err != nil {
		t.Fatalf("a blob an in-flight checkpoint had planned was swept: %v", err)
	}

	// The checkpoint completes without that file (it captured nothing in the
	// end), so the claim must be released and the blob become collectable.
	if _, err := s.db.Exec(`UPDATE claw_checkpoints SET status='ready' WHERE id='cp'`); err != nil {
		t.Fatal(err)
	}
	s.clearCheckpointPendingBlobs("cp")

	if removed, _, _, err := s.sweepCheckpointBlobs(false, nil); err != nil || removed != 1 {
		t.Fatalf("sweep removed %d blobs (err %v) after the claim was released, want 1", removed, err)
	}
}

// ---------------------------------------------------------------------------
// Unparseable manifests
// ---------------------------------------------------------------------------

// A truncated manifest is what a full disk leaves behind, and aborting on every
// one of them made the disk-full condition self-sustaining: retention switched
// itself off for the length of the window. An orphan cannot make the keep set
// incomplete, so only a REFERENCED one may abort.
func TestCheckpointBlobKeepSetAbortsOnlyOnReferencedBadManifests(t *testing.T) {
	cases := []struct {
		name       string
		referenced bool
		wantAbort  bool
		because    string
	}{
		{
			name: "orphan manifest is skipped", referenced: false, wantAbort: false,
			because: "no row points at it, so nothing can restore from it and the keep set stays complete",
		},
		{
			name: "referenced manifest aborts", referenced: true, wantAbort: true,
			because: "a live checkpoint's blob list is unreadable, so the keep set really would be short",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Now()
			insertRetentionClaw(t, s, "claw", reference)

			orphanSHA := writeRetentionBlob(t, []byte("unreferenced blob"))
			aged := time.Now().Add(-2 * blobSweepGrace)
			if err := os.Chtimes(checkpointBlobPath(orphanSHA), aged, aged); err != nil {
				t.Fatal(err)
			}

			if tc.referenced {
				insertRetentionCheckpoint(t, s, retentionCheckpoint{
					id: "cp", clawID: "claw", status: "ready", createdAt: reference,
					writeManifest: true, manifestBody: []byte("{ truncated by ENOSP")})
			} else {
				// A manifest file with no row at all: the shape left behind when
				// the row was deleted but the unlink did not happen.
				path := checkpointManifestPath("no-such-checkpoint")
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{ truncated by ENOSP"), 0o640); err != nil {
					t.Fatal(err)
				}
			}

			_, err := s.checkpointBlobKeepSet(nil)
			if gotAbort := err != nil; gotAbort != tc.wantAbort {
				t.Fatalf("keep set aborted = %v, want %v (%s): err=%v", gotAbort, tc.wantAbort, tc.because, err)
			}

			removed, _, _, err := s.sweepCheckpointBlobs(false, nil)
			if tc.wantAbort {
				if err == nil || removed != 0 {
					t.Fatalf("sweep proceeded (removed %d, err %v) on an incomplete keep set", removed, err)
				}
				if _, err := os.Stat(checkpointBlobPath(orphanSHA)); err != nil {
					t.Fatalf("an aborted sweep still deleted a blob: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("sweep aborted over an orphan manifest: %v", err)
			}
			if removed != 1 {
				t.Fatalf("sweep removed %d blobs, want 1: an orphan manifest must not disable reclamation", removed)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Compaction: keeping something that can actually restore
// ---------------------------------------------------------------------------

// The newest ready checkpoint is not automatically the useful one. A
// metadata-only capture (bridge unreachable) is 'ready' with an empty root
// tree, and on the kill path it is ALWAYS the newest, because handleClawKill
// drops the connection before asking for the checkpoint. Keeping it by
// timestamp deletes every checkpoint that has files and leaves a row
// retryCheckpointIDWithCount refuses to restore from.
func TestCompactClawCheckpointsKeepsTheNewestRestorable(t *testing.T) {
	cases := []struct {
		name        string
		roots       []string // newest first
		wantKeptIdx int
		because     string
	}{
		{
			name: "newest has files", roots: []string{"tree-new", "tree-old"}, wantKeptIdx: 0,
			because: "the newest is restorable, so recency wins as before",
		},
		{
			name: "newest is metadata-only", roots: []string{"", "tree-old"}, wantKeptIdx: 1,
			because: "the kill capture cannot restore anything; the one with files must survive",
		},
		{
			name: "two metadata-only captures on top", roots: []string{"", "", "tree-old"}, wantKeptIdx: 2,
			because: "walk back as far as needed to find a restorable checkpoint",
		},
		{
			name: "all metadata-only", roots: []string{"", "", ""}, wantKeptIdx: 0,
			because: "nothing better exists, so keep the newest; the row is still the analytics record",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			insertRetentionClaw(t, s, "claw", reference)
			insertRetentionPR(t, s, "claw", 1)

			ids := make([]string, len(tc.roots))
			for i, root := range tc.roots {
				ids[i] = fmt.Sprintf("cp-%d", i)
				rootSHA := ""
				if root != "" {
					rootSHA = writeRetentionBlob(t, []byte(root))
				}
				insertRetentionCheckpoint(t, s, retentionCheckpoint{
					id: ids[i], clawID: "claw", status: "ready",
					createdAt:     reference.Add(-time.Duration(i) * time.Hour),
					rootTree:      rootSHA,
					writeManifest: true})
			}

			compacted, err := s.compactFinalizedCheckpoints(reference.Add(-240*time.Hour), false)
			if err != nil {
				t.Fatalf("compactFinalizedCheckpoints: %v", err)
			}
			if want := len(tc.roots) - 1; compacted.count != want {
				t.Fatalf("compacted %d, want %d", compacted.count, want)
			}
			for i, id := range ids {
				status, _, _ := retentionCheckpointRow(t, s, id)
				wantStatus := "compacted"
				if i == tc.wantKeptIdx {
					wantStatus = "ready"
				}
				if status != wantStatus {
					t.Errorf("%s status = %q, want %q (%s)", id, status, wantStatus, tc.because)
				}
			}
		})
	}
}

// Compaction that leaves the tree digests on the row reclaims only the
// manifest, which under schema 2 is the small half of the checkpoint. The
// blobs are the point.
func TestCompactionReleasesTheBlobsItNoLongerNeeds(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	insertRetentionClaw(t, s, "claw", reference)
	insertRetentionPR(t, s, "claw", 1)

	// Two checkpoints with entirely disjoint contents, so nothing the older one
	// holds is kept alive by the newer.
	makeCheckpoint := func(id, contents string, createdAt time.Time) (fileSHA, treeSHA, msgSHA string) {
		fileSHA = writeRetentionBlob(t, []byte(contents+" file"))
		tree, _ := json.Marshal([]types.CheckpointFile{{Path: "workspace/a.txt", SHA256: fileSHA, Size: 1}})
		treeSHA = writeRetentionBlob(t, tree)
		msgSHA = writeRetentionBlob(t, []byte(contents+" messages"))
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: id, clawID: "claw", status: "ready", createdAt: createdAt,
			rootTree: treeSHA, messageTree: msgSHA, writeManifest: true})
		return
	}
	oldFile, oldTree, oldMsg := makeCheckpoint("cp-old", "old", reference.Add(-48*time.Hour))
	newFile, newTree, newMsg := makeCheckpoint("cp-new", "new", reference.Add(-24*time.Hour))

	if _, err := s.compactFinalizedCheckpoints(reference.Add(-240*time.Hour), false); err != nil {
		t.Fatalf("compactFinalizedCheckpoints: %v", err)
	}

	var root, workspace, message string
	if err := s.db.QueryRow(
		`SELECT root_tree_sha256, workspace_tree_sha256, message_tree_sha256 FROM claw_checkpoints WHERE id='cp-old'`).
		Scan(&root, &workspace, &message); err != nil {
		t.Fatal(err)
	}
	if root != "" || workspace != "" || message != "" {
		t.Fatalf("compacted row kept tree digests root=%q workspace=%q message=%q; its blobs stay reachable forever",
			root, workspace, message)
	}

	aged := time.Now().Add(-2 * blobSweepGrace)
	for _, sha := range []string{oldFile, oldTree, oldMsg, newFile, newTree, newMsg} {
		if err := os.Chtimes(checkpointBlobPath(sha), aged, aged); err != nil {
			t.Fatal(err)
		}
	}
	removed, freed, _, err := s.sweepCheckpointBlobs(false, nil)
	if err != nil {
		t.Fatalf("sweepCheckpointBlobs: %v", err)
	}
	if removed != 3 {
		t.Fatalf("swept %d blobs (%d bytes), want the compacted checkpoint's 3", removed, freed)
	}
	for _, sha := range []string{oldFile, oldTree, oldMsg} {
		if _, err := os.Stat(checkpointBlobPath(sha)); !os.IsNotExist(err) {
			t.Errorf("blob %s of the compacted checkpoint survived", sha)
		}
	}
	for _, sha := range []string{newFile, newTree, newMsg} {
		if _, err := os.Stat(checkpointBlobPath(sha)); err != nil {
			t.Errorf("sweep deleted a blob of the kept checkpoint %s: %v", sha, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Batched deletes
// ---------------------------------------------------------------------------

// One unbounded DELETE over messages or task_run_events is a full scan holding
// the only write lock the database has, and every concurrent writer waits out
// busy_timeout behind it. The work must be split, with the lock released
// between batches.
func TestPruneRowsBatchedSpansMultipleBatches(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cutoff := reference.Add(-90 * 24 * time.Hour)
	insertRetentionClaw(t, s, "claw", reference)

	const expired, kept = 2*retentionDeleteBatch + 7, 3
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < expired+kept; i++ {
		at := cutoff.Add(-time.Hour)
		if i >= expired {
			at = cutoff.Add(time.Hour)
		}
		if _, err := tx.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES(?,?,?,?,?,?)`,
			fmt.Sprintf("msg-%d", i), "claw", "tenant", "user", "hello", at); err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// A pause long enough to be measurable stands in for the lock being
	// released: a single unbounded DELETE would never reach it.
	previous := retentionBatchPause
	retentionBatchPause = 20 * time.Millisecond
	defer func() { retentionBatchPause = previous }()

	started := time.Now()
	deleted, err := s.pruneRowsBatched("messages", "created_at", cutoff, false, time.Time{})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("pruneRowsBatched: %v", err)
	}
	if deleted != expired {
		t.Fatalf("deleted %d rows, want %d", deleted, expired)
	}
	if elapsed < 2*retentionBatchPause {
		t.Fatalf("finished in %s: the delete did not yield between batches, so it held one long write lock", elapsed)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != kept {
		t.Fatalf("%d messages left, want %d", remaining, kept)
	}
}

// ---------------------------------------------------------------------------
// Auditability
// ---------------------------------------------------------------------------

// dry_run exists so an operator can see the blast radius before arming a policy
// on a hub that has never run one. It must select exactly what a real cycle
// would, report it, and touch nothing.
func TestRetentionSweepOnceDryRunReportsWithoutDeleting(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		name := "dry run keeps everything"
		if !dryRun {
			name = "a real cycle deletes the same selection"
		}
		t.Run(name, func(t *testing.T) {
			s := newRetentionTestServer(t)
			reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			s.nowFunc = func() time.Time { return reference }
			s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{
				Enabled: boolPtr(true), DryRun: dryRun}}

			expired := reference.Add(-defaultRetentionMaxAge - 24*time.Hour)
			insertRetentionClaw(t, s, "claw", expired)
			manifest := insertRetentionCheckpoint(t, s, retentionCheckpoint{
				id: "cp", clawID: "claw", status: "ready", createdAt: expired, writeManifest: true})
			if _, err := s.db.Exec(`INSERT INTO messages(id, claw_id, tenant_id, role, content, created_at) VALUES(?,?,?,?,?,?)`,
				"msg-old", "claw", "tenant", "user", "hello", expired); err != nil {
				t.Fatal(err)
			}
			orphanSHA := writeRetentionBlob(t, []byte("unreferenced"))
			aged := time.Now().Add(-2 * blobSweepGrace)
			if err := os.Chtimes(checkpointBlobPath(orphanSHA), aged, aged); err != nil {
				t.Fatal(err)
			}

			var logs bytes.Buffer
			previousOut, previousFlags := log.Writer(), log.Flags()
			log.SetOutput(&logs)
			log.SetFlags(0)
			defer func() {
				log.SetOutput(previousOut)
				log.SetFlags(previousFlags)
			}()

			s.retentionSweepOnce()

			var checkpoints, messages int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_checkpoints`).Scan(&checkpoints); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages); err != nil {
				t.Fatal(err)
			}
			_, manifestErr := os.Stat(manifest)
			_, blobErr := os.Stat(checkpointBlobPath(orphanSHA))

			if dryRun {
				if checkpoints != 1 || messages != 1 || manifestErr != nil || blobErr != nil {
					t.Fatalf("dry run deleted something: checkpoints=%d messages=%d manifest=%v blob=%v",
						checkpoints, messages, manifestErr, blobErr)
				}
			} else if checkpoints != 0 || messages != 0 || !os.IsNotExist(manifestErr) || !os.IsNotExist(blobErr) {
				t.Fatalf("real cycle left data behind: checkpoints=%d messages=%d manifest=%v blob=%v",
					checkpoints, messages, manifestErr, blobErr)
			}

			// Every cycle reports, so a clean one is distinguishable from a
			// stuck one. The counts are the same either way: dry_run changes
			// what is done, not what is selected.
			out := logs.String()
			for _, want := range []string{
				"cycle start",
				fmt.Sprintf("dry_run=%v", dryRun),
				"checkpoints=1",
				"messages=1",
				"blobs=1",
				"errors=0",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("cycle log is missing %q:\n%s", want, out)
				}
			}
		})
	}
}
