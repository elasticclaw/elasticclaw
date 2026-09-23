package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func retentionServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ELASTICCLAW_HUB_CONFIG", "/nonexistent/hub.yaml")
	return newCheckpointCompletionTestServer(t)
}

func retentionExec(t *testing.T, s *Server, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func retentionCount(t *testing.T, s *Server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func retentionBlob(t *testing.T, content string) string {
	t.Helper()
	sha := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	retentionFile(t, checkpointBlobPath(sha), content, time.Now())
	return sha
}

func retentionFile(t *testing.T, path, content string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func retentionExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if want && err != nil {
		t.Fatalf("expected %s: %v", path, err)
	}
	if !want && !os.IsNotExist(err) {
		t.Fatalf("expected missing %s, got %v", path, err)
	}
}

func retentionCheckpoint(t *testing.T, s *Server, id, claw, status, reason, root string, at time.Time) string {
	t.Helper()
	path := filepath.Join(hubDataDir(), "checkpoints", "manifests", id+".json")
	retentionFile(t, path, "{}", at)
	retentionExec(t, s, `INSERT INTO claw_checkpoints
 (id,tenant_id,claw_id,status,reason,root_tree_sha256,workspace_tree_sha256,manifest_path,created_at,pipeline_stage,hub_version,files_count,files_bytes)
 VALUES(?,'tenant',?,?,?,?,?,?,?,'work','version',7,123)`, id, claw, status, reason, root, root, path, at)
	return path
}

func retentionRecord(t *testing.T, s *Server, id, tree string, files ...string) {
	t.Helper()
	entries := make([]types.CheckpointFile, 0, len(files))
	for _, sha := range files {
		entries = append(entries, types.CheckpointFile{SHA256: sha})
	}
	if err := recordCheckpointTree(s.db, id, tree, entries); err != nil {
		t.Fatal(err)
	}
}

func retentionRun(t *testing.T, s *Server) {
	t.Helper()
	retentionExec(t, s, `INSERT INTO task_runs(id,tenant_id,initial_attempt_id,run_kind,owner_type,created_at,updated_at) VALUES('run','tenant','attempt','code_task','manual',0,0)`)
	retentionExec(t, s, `INSERT INTO task_run_attempts(id,tenant_id,run_id,attempt_id,attempt_number,started_at,created_at,updated_at) VALUES('attempt','tenant','run','attempt',1,0,0,0)`)
}

func TestRetentionPlanRecordsExpansion(t *testing.T) {
	s := retentionServer(t)
	tree := retentionBlob(t, "tree")
	file := retentionBlob(t, "file")
	for _, id := range []string{"first", "second"} {
		insertTestCheckpoint(t, s, id, "manual")
		plan := types.CheckpointPlan{RootSHA256: tree, Files: []types.CheckpointFile{{SHA256: tree, Path: ".checkpoint/tree.json"}, {SHA256: file}, {SHA256: file}, {SHA256: ""}}}
		body, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/checkpoints/"+id+"/plan", bytes.NewReader(body))
		req.Header.Set("X-Claw-Token", "claw-token")
		rr := httptest.NewRecorder()
		s.handleCheckpointInternal(rr, req)
		if rr.Code != 200 {
			t.Fatalf("plan: %d %s", rr.Code, rr.Body.String())
		}
		if got := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id=? AND status='creating' AND root_tree_sha256=?`, id, tree); got != 1 {
			t.Fatal("plan did not protect creating root")
		}
	}
	if got := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_trees`); got != 1 {
		t.Fatalf("trees=%d", got)
	}
	if got := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files WHERE tree_sha256=? AND file_sha256=?`, tree, file); got != 1 {
		t.Fatalf("files=%d", got)
	}
	if got := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files`); got != 1 {
		t.Fatalf("extra expansion entries: %d", got)
	}
}

func TestRetentionRejectsNonHexDigests(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	tree := retentionBlob(t, "escape-tree")
	config := filepath.Join(hubDataDir(), "hub.yaml")
	retentionFile(t, config, "secret", at)
	if err := recordCheckpointTree(s.db, "", "../../hub.yaml", nil); err == nil {
		t.Fatal("non-hex root recorded")
	}
	insertTestCheckpoint(t, s, "cp", "manual")
	body, err := json.Marshal(types.CheckpointPlan{RootSHA256: tree, Files: []types.CheckpointFile{{SHA256: "../../hub.yaml"}, {SHA256: ""}}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/checkpoints/cp/plan", bytes.NewReader(body))
	req.Header.Set("X-Claw-Token", "claw-token")
	rr := httptest.NewRecorder()
	s.handleCheckpointInternal(rr, req)
	if rr.Code != 200 {
		t.Fatalf("plan: %d %s", rr.Code, rr.Body.String())
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files`); n != 0 {
		t.Fatalf("non-hex file recorded: %d", n)
	}
	// Rows written before validation existed must be dropped without unlinking.
	retentionExec(t, s, `INSERT INTO checkpoint_tree_files VALUES(?,'../../hub.yaml')`, tree)
	retentionExec(t, s, `UPDATE claw_checkpoints SET status='compacted',root_tree_sha256='',message_tree_sha256='../../hub.yaml' WHERE id='cp'`)
	counts, err := s.collectCheckpointBlobs(at, false)
	if err != nil || counts.trees != 1 || counts.blobs != 1 {
		t.Fatalf("collection=%+v,%v", counts, err)
	}
	retentionExists(t, config, true)
	retentionExists(t, checkpointBlobPath(tree), false)
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files`); n != 0 {
		t.Fatalf("non-hex rows kept: %d", n)
	}
}

func TestFinalizeRefusesCheckpointNoLongerCreating(t *testing.T) {
	s := retentionServer(t)
	tree := retentionBlob(t, "[]")
	for _, id := range []string{"finalize", "skip", "metadata"} {
		insertTestCheckpoint(t, s, id, "manual")
	}
	if err := s.failCheckpoint("finalize", "hub restarted"); err != nil {
		t.Fatal(err)
	}
	retentionExec(t, s, `UPDATE claw_checkpoints SET status='failed' WHERE id IN ('skip','metadata')`)
	if err := s.finalizeCheckpoint("finalize", "tenant", "claw", tree); err == nil {
		t.Fatal("finalized a failed checkpoint")
	}
	if err := s.markCheckpointSkipped("skip", tree); err == nil {
		t.Fatal("skipped a failed checkpoint")
	}
	if err := s.completeMetadataOnlyCheckpoint("metadata", "claw", "manual", "bridge unreachable"); err == nil {
		t.Fatal("completed a failed checkpoint")
	}
	for _, id := range []string{"finalize", "skip", "metadata"} {
		if got := checkpointStatus(t, s, id); got != "failed" {
			t.Fatalf("%s=%s", id, got)
		}
	}
	retentionExists(t, checkpointManifestPath("finalize"), false)
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE manifest_path!='' OR message_tree_sha256!=''`); n != 0 {
		t.Fatalf("failed rows gained references: %d", n)
	}
}

func TestRestoreRevalidatesCheckpointBeforeReinstallingPointer(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	retentionCheckpoint(t, s, "cp", "claw", "ready", "manual", retentionBlob(t, "[]"), at)
	retentionExec(t, s, `UPDATE claws SET restore_checkpoint_id='cp' WHERE id='claw'`)
	if err := s.beginRestoreProvision("tenant", "claw", "cp"); err != nil {
		t.Fatalf("ready checkpoint refused: %v", err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claws WHERE id='claw' AND status='provisioning' AND restore_checkpoint_id='cp' AND restored_from_checkpoint_id='cp'`); n != 1 {
		t.Fatal("restore pointer not installed")
	}
	// The checkpoint expires between the claim and the reinstall; the VM is
	// already gone, so the claw must show the failure and drop the claim.
	retentionExec(t, s, `UPDATE claws SET status='connected', restored_from_checkpoint_id='' WHERE id='claw'`)
	retentionExec(t, s, `UPDATE claw_checkpoints SET status='compacted', manifest_path='' WHERE id='cp'`)
	if err := s.beginRestoreProvision("tenant", "claw", "cp"); err == nil || !strings.Contains(err.Error(), "no longer ready") {
		t.Fatalf("compacted checkpoint accepted: %v", err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claws WHERE id='claw' AND status='error' AND bootstrap_status='Restore aborted: checkpoint no longer ready' AND restore_checkpoint_id='' AND restored_from_checkpoint_id=''`); n != 1 {
		t.Fatal("aborted restore did not fail the claw and clear the claim")
	}
	if err := s.restoreClawFromCheckpoint(context.Background(), "tenant", "claw", "cp"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("restore of compacted checkpoint: %v", err)
	}
}

func TestRestoreAbortsWhenClaimOverwrittenByConcurrentRestore(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	tree := retentionBlob(t, "[]")
	retentionCheckpoint(t, s, "ours", "claw", "ready", "manual", tree, at)
	retentionCheckpoint(t, s, "theirs", "claw", "ready", "manual", tree, at)
	// Another restore claimed the claw during our pre-reset wait.
	retentionExec(t, s, `UPDATE claws SET restore_checkpoint_id='theirs' WHERE id='claw'`)
	if err := s.beginRestoreProvision("tenant", "claw", "ours"); err == nil {
		t.Fatal("stale claim reinstalled over a concurrent restore")
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claws WHERE id='claw' AND status='connected' AND restore_checkpoint_id='theirs' AND restored_from_checkpoint_id=''`); n != 1 {
		t.Fatal("claw touched while another restore owns the claim")
	}
}

func TestMessageBlobIsReferencedBeforeReuse(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	// No messages: the digest is that of an empty JSONL, already on disk.
	message := retentionBlob(t, "")
	retentionCheckpoint(t, s, "dead", "claw", "compacted", "manual", "", at)
	retentionExec(t, s, `UPDATE claw_checkpoints SET message_tree_sha256=? WHERE id='dead'`, message)
	insertTestCheckpoint(t, s, "live", "manual")
	sha, count, _, err := s.writeMessageCheckpointBlob("live", "claw", "tenant")
	if err != nil || sha != message || count != 0 {
		t.Fatalf("blob=%s count=%d err=%v", sha, count, err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='live' AND status='creating' AND message_tree_sha256=?`, message); n != 1 {
		t.Fatal("creating row does not name the reused message blob")
	}
	counts, err := s.collectCheckpointBlobs(at, false)
	if err != nil || counts.blobs != 0 {
		t.Fatalf("collection=%+v,%v", counts, err)
	}
	retentionExists(t, checkpointBlobPath(message), true)
	if _, _, _, err := s.writeMessageCheckpointBlob("dead", "claw", "tenant"); err == nil {
		t.Fatal("named a message blob from a compacted row")
	}
}

func TestMarkReadyRequiresPublishedMessageDigest(t *testing.T) {
	s := retentionServer(t)
	insertTestCheckpoint(t, s, "cp", "manual")
	sha, _, _, err := s.writeMessageCheckpointBlob("cp", "claw", "tenant")
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent complete published its own digest after ours.
	retentionExec(t, s, `UPDATE claw_checkpoints SET message_tree_sha256=? WHERE id='cp'`, strings.Repeat("f", 64))
	if err := s.markCheckpointReady("cp", sha, `completed_at=?`, now()); err == nil || !strings.Contains(err.Error(), "no longer creating") {
		t.Fatalf("stale digest went ready: %v", err)
	}
	if got := checkpointStatus(t, s, "cp"); got != "creating" {
		t.Fatalf("status=%s", got)
	}
	if err := s.markCheckpointReady("cp", strings.Repeat("f", 64), `completed_at=?`, now()); err != nil {
		t.Fatalf("current digest refused: %v", err)
	}
	if got := checkpointStatus(t, s, "cp"); got != "ready" {
		t.Fatalf("status=%s", got)
	}
}

func TestRetentionCompactionPreservesRetryAndRestorePointers(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	old := at.Add(-3 * time.Hour)
	retentionExec(t, s, `UPDATE claws SET status='error' WHERE id='claw'`)
	retentionExec(t, s, `INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES('other','tenant','other','','connected',?)`, at)
	retentionRun(t, s)
	for i, id := range []string{"restore", "restored", "attempt-pointer", "discard", "pick", "bootstrap"} {
		reason := "manual"
		if id == "bootstrap" {
			reason = "bootstrap"
		}
		retentionCheckpoint(t, s, id, "claw", "ready", reason, id+"-tree", old.Add(time.Duration(i)*time.Minute))
	}
	retentionExec(t, s, `UPDATE claws SET restore_checkpoint_id='restore',restored_from_checkpoint_id='restored' WHERE id='other'`)
	retentionExec(t, s, `UPDATE task_run_attempts SET restored_checkpoint_id='attempt-pointer' WHERE id='attempt'`)
	for _, status := range []string{"skipped", "failed", "creating"} {
		retentionCheckpoint(t, s, status, "claw", status, "manual", status+"-tree", old)
	}
	retentionExec(t, s, `UPDATE claw_checkpoints SET message_tree_sha256='keep-for-collector' WHERE id='discard'`)
	for _, claw := range []string{"recent", "live", "empty", "recent-compacted"} {
		status := "deleted"
		if claw == "live" {
			status = "connected"
		}
		retentionExec(t, s, `INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES(?,'tenant',?,'',?,?)`, claw, claw, status, old)
	}
	retentionCheckpoint(t, s, "recent-old", "recent", "failed", "manual", "recent-tree", old)
	retentionCheckpoint(t, s, "recent-new", "recent", "creating", "manual", "recent-tree", at.Add(-30*time.Minute))
	retentionCheckpoint(t, s, "live-old", "live", "failed", "manual", "live-tree", old)
	retentionCheckpoint(t, s, "empty-failed", "empty", "failed", "manual", "empty-tree", old)
	retentionCheckpoint(t, s, "old-with-recent-compacted", "recent-compacted", "failed", "manual", "old-tree", old)
	retentionCheckpoint(t, s, "recent-compacted-row", "recent-compacted", "compacted", "manual", "", at)
	n, err := s.compactCheckpoints(at, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Fatalf("compacted=%d, want 6", n)
	}
	for _, id := range []string{"restore", "restored", "attempt-pointer", "pick"} {
		if got := checkpointStatus(t, s, id); got != "ready" {
			t.Fatalf("%s=%s", id, got)
		}
		retentionExists(t, filepath.Join(hubDataDir(), "checkpoints", "manifests", id+".json"), true)
	}
	for _, id := range []string{"discard", "bootstrap", "skipped", "failed", "creating", "empty-failed"} {
		if got := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id=? AND status='compacted' AND manifest_path='' AND root_tree_sha256='' AND workspace_tree_sha256='' AND pipeline_stage='work' AND hub_version='version' AND files_count=7 AND files_bytes=123`, id); got != 1 {
			t.Fatalf("bad compacted row %s", id)
		}
		retentionExists(t, filepath.Join(hubDataDir(), "checkpoints", "manifests", id+".json"), false)
	}
	if got := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='discard' AND message_tree_sha256='keep-for-collector'`); got != 1 {
		t.Fatal("message pointer lost")
	}
	for _, id := range []string{"recent-old", "live-old", "old-with-recent-compacted"} {
		if got := checkpointStatus(t, s, id); got != "failed" {
			t.Fatalf("protected %s=%s", id, got)
		}
	}
	if n, err = s.compactCheckpoints(at, false); err != nil || n != 0 {
		t.Fatalf("second compaction=%d,%v", n, err)
	}
}

func TestRetentionCollectorSharedFilesAndMessages(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	shared := retentionBlob(t, "shared")
	unique := retentionBlob(t, "unique")
	a := retentionBlob(t, "tree-a")
	b := retentionBlob(t, "tree-b")
	message := retentionBlob(t, "messages")
	retentionCheckpoint(t, s, "a", "claw", "compacted", "manual", "", at)
	retentionCheckpoint(t, s, "b", "claw", "creating", "manual", b, at)
	retentionRecord(t, s, "", a, shared, unique)
	retentionRecord(t, s, "b", b, shared)
	retentionExec(t, s, `UPDATE claw_checkpoints SET message_tree_sha256=?`, message)
	counts, err := s.collectCheckpointBlobs(at, false)
	if err != nil {
		t.Fatal(err)
	}
	if counts.trees != 1 || counts.blobs != 2 || counts.bytes != int64(len("unique")+len("tree-a")) {
		t.Fatalf("collection=%+v", counts)
	}
	for _, sha := range []string{unique, a} {
		retentionExists(t, checkpointBlobPath(sha), false)
	}
	for _, sha := range []string{shared, b, message} {
		retentionExists(t, checkpointBlobPath(sha), true)
	}
	retentionExec(t, s, `UPDATE claw_checkpoints SET status='compacted',root_tree_sha256='' WHERE id='b'`)
	counts, err = s.collectCheckpointBlobs(at, false)
	if err != nil {
		t.Fatal(err)
	}
	if counts.trees != 1 || counts.blobs != 3 {
		t.Fatalf("second collection=%+v", counts)
	}
	for _, sha := range []string{shared, b, message} {
		retentionExists(t, checkpointBlobPath(sha), false)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files`); n != 0 {
		t.Fatalf("file refs=%d", n)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE message_tree_sha256!=''`); n != 0 {
		t.Fatalf("message refs=%d", n)
	}
}

func TestRetentionCollectorBatchesAndTemporaryFiles(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	tree := retentionBlob(t, "large-tree")
	files := make([]string, 501)
	for i := range files {
		files[i] = retentionBlob(t, fmt.Sprintf("file-%d", i))
	}
	retentionRecord(t, s, "", tree, files...)
	old := checkpointBlobPath(tree) + ".tmp-old"
	recent := checkpointBlobPath(tree) + ".tmp-new"
	retentionFile(t, old, "old", at.Add(-25*time.Hour))
	retentionFile(t, recent, "new", at)
	var dry retentionCollection
	next, err := s.collectCheckpointTreeBatch(tree, true, "", &dry)
	if err != nil || next == "" || dry.blobs != 500 {
		t.Fatalf("dry batch next=%q blobs=%d err=%v", next, dry.blobs, err)
	}
	if err := s.collectCheckpointTree(tree, true, &dry); err != nil || dry.trees != 1 || dry.blobs != 502+500 {
		t.Fatalf("dry tree=%+v err=%v", dry, err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files`); n != 501 {
		t.Fatalf("rows after dry run=%d", n)
	}
	var batch retentionCollection
	next, err = s.collectCheckpointTreeBatch(tree, false, "", &batch)
	if err != nil || next == "" || batch.blobs != 500 {
		t.Fatalf("first batch next=%q blobs=%d err=%v", next, batch.blobs, err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files`); n != 1 {
		t.Fatalf("rows after first batch=%d", n)
	}
	counts, err := s.collectCheckpointBlobs(at, false)
	if err != nil {
		t.Fatal(err)
	}
	if counts.trees != 1 || counts.blobs != 3 {
		t.Fatalf("collection=%+v", counts)
	}
	for _, sha := range files {
		retentionExists(t, checkpointBlobPath(sha), false)
	}
	retentionExists(t, old, false)
	retentionExists(t, recent, true)
}

func TestRetentionBackfillAndCollectionGate(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	file := retentionBlob(t, "legacy-file")
	data, err := json.Marshal([]types.CheckpointFile{{Path: "file", SHA256: file}})
	if err != nil {
		t.Fatal(err)
	}
	tree := retentionBlob(t, string(data))
	retentionCheckpoint(t, s, "legacy", "claw", "ready", "manual", tree, at)
	for i := 0; i < 50; i++ {
		retentionCheckpoint(t, s, fmt.Sprintf("missing-%d", i), "claw", "skipped", "manual", fmt.Sprintf("%064x", i+1), at)
	}
	dead := retentionBlob(t, "dead-tree")
	retentionRecord(t, s, "", dead)
	counts, err := s.collectCheckpointBlobs(at, false)
	if err != nil {
		t.Fatal(err)
	}
	if counts.trees != 0 || counts.blobs != 0 || counts.unexpanded != 51 {
		t.Fatalf("ungated collection=%+v", counts)
	}
	retentionExists(t, checkpointBlobPath(dead), true)
	remaining, err := s.backfillCheckpointTrees()
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining=%d, want 1", remaining)
	}
	counts, err = s.collectCheckpointBlobs(at, false)
	if err != nil {
		t.Fatal(err)
	}
	if counts.blobs != 0 || counts.unexpanded != 1 {
		t.Fatalf("incomplete gate=%+v", counts)
	}
	remaining, err = s.backfillCheckpointTrees()
	if err != nil || remaining != 0 {
		t.Fatalf("backfill=%d,%v", remaining, err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_trees`); n != 52 {
		t.Fatalf("tree count=%d", n)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files WHERE tree_sha256=? AND file_sha256=?`, tree, file); n != 1 {
		t.Fatal("legacy expansion missing")
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files`); n != 1 {
		t.Fatalf("missing trees expanded with files: %d", n)
	}
	counts, err = s.collectCheckpointBlobs(at, false)
	if err != nil || counts.trees != 1 {
		t.Fatalf("collection=%+v,%v", counts, err)
	}
	retentionExists(t, checkpointBlobPath(dead), false)
	retentionExists(t, checkpointBlobPath(file), true)
}

func TestRetentionExpiry(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	cutoff := at.Add(-90 * 24 * time.Hour)
	old := cutoff.Add(-time.Hour)
	retentionRun(t, s)
	for _, id := range []string{"expired", "restore", "restored", "attempt-pointer", "already-compacted", "unknown-status"} {
		retentionCheckpoint(t, s, id, "claw", "ready", "manual", id+"-tree", old)
	}
	retentionExec(t, s, `UPDATE claw_checkpoints SET status='compacted' WHERE id='already-compacted'`)
	retentionExec(t, s, `UPDATE claw_checkpoints SET status='unknown' WHERE id='unknown-status'`)
	retentionCheckpoint(t, s, "new", "claw", "ready", "manual", "new-tree", at)
	retentionCheckpoint(t, s, "boundary", "claw", "ready", "manual", "boundary-tree", cutoff)
	retentionExec(t, s, `UPDATE claws SET restore_checkpoint_id='restore',restored_from_checkpoint_id='restored' WHERE id='claw'`)
	retentionExec(t, s, `UPDATE task_run_attempts SET restored_checkpoint_id='attempt-pointer' WHERE id='attempt'`)
	n, err := s.expireCheckpoints(cutoff, false)
	if err != nil || n != 3 {
		t.Fatalf("expired=%d,%v", n, err)
	}
	for _, id := range []string{"expired", "already-compacted", "unknown-status"} {
		if got := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id=? AND status='compacted' AND root_tree_sha256='' AND workspace_tree_sha256='' AND manifest_path=''`, id); got != 1 {
			t.Fatalf("not expired: %s", id)
		}
		retentionExists(t, filepath.Join(hubDataDir(), "checkpoints", "manifests", id+".json"), false)
	}
	for _, id := range []string{"restore", "restored", "attempt-pointer", "new", "boundary"} {
		if got := checkpointStatus(t, s, id); got != "ready" {
			t.Fatalf("protected %s=%s", id, got)
		}
	}
	dir := filepath.Join(hubDataDir(), "diagnostics")
	for _, name := range []string{"old.log", "new.log", "nested/old.log"} {
		mtime := old
		if name == "new.log" {
			mtime = at
		}
		retentionFile(t, filepath.Join(dir, name), "log", mtime)
	}
	n, err = s.expireDiagnostics(cutoff, false)
	if err != nil || n != 1 {
		t.Fatalf("diagnostics=%d,%v", n, err)
	}
	retentionExists(t, filepath.Join(dir, "old.log"), false)
	retentionExists(t, filepath.Join(dir, "new.log"), true)
	retentionExists(t, filepath.Join(dir, "nested", "old.log"), true)
	retentionExec(t, s, `WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<5001)
 INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) SELECT 'old-'||n,'claw','tenant','user','old',? FROM seq`, old)
	retentionExec(t, s, `WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<5001)
 INSERT INTO task_run_events(id,tenant_id,run_id,event_key,event_type,event_time,observed_at,created_at) SELECT 'old-'||n,'tenant','run','old-'||n,'task_start',?,0,0 FROM seq`, old.UnixMilli())
	retentionExec(t, s, `INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES('boundary','claw','tenant','user','boundary',?)`, cutoff)
	retentionExec(t, s, `INSERT INTO task_run_events(id,tenant_id,run_id,event_key,event_type,event_time,observed_at,created_at) VALUES('boundary','tenant','run','boundary','task_start',?,0,0)`, cutoff.UnixMilli())
	n, err = s.expireRetentionRows(cutoff, false)
	if err != nil || n != 10002 {
		t.Fatalf("rows=%d,%v", n, err)
	}
	for _, table := range []string{"messages", "task_run_events"} {
		if n := retentionCount(t, s, "SELECT COUNT(*) FROM "+table); n != 1 {
			t.Fatalf("%s remaining=%d", table, n)
		}
	}
}

func TestRetentionDryRun(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	old := at.Add(-100 * 24 * time.Hour)
	retentionExec(t, s, `UPDATE claws SET status='error' WHERE id='claw'`)
	tree := retentionBlob(t, "dry-tree")
	file := retentionBlob(t, "dry-file")
	message := retentionBlob(t, "dry-message")
	retentionRecord(t, s, "", tree, file)
	manifest := retentionCheckpoint(t, s, "old", "claw", "failed", "manual", tree, old)
	retentionExec(t, s, `UPDATE claw_checkpoints SET message_tree_sha256=?`, message)
	n, err := s.compactCheckpoints(at, true)
	if err != nil || n != 1 {
		t.Fatalf("compaction=%d,%v", n, err)
	}
	n, err = s.expireCheckpoints(at.Add(-90*24*time.Hour), true)
	if err != nil || n != 1 {
		t.Fatalf("expiry=%d,%v", n, err)
	}
	counts, err := s.collectCheckpointBlobs(at, true)
	if err != nil || counts.trees != 1 || counts.blobs != 3 {
		t.Fatalf("collection=%+v,%v", counts, err)
	}
	diagnostic := filepath.Join(hubDataDir(), "diagnostics", "old.log")
	retentionFile(t, diagnostic, "log", old)
	n, err = s.expireDiagnostics(at, true)
	if err != nil || n != 1 {
		t.Fatalf("diagnostics=%d,%v", n, err)
	}
	retentionRun(t, s)
	retentionExec(t, s, `INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES('old','claw','tenant','user','old',?)`, old)
	retentionExec(t, s, `INSERT INTO task_run_events(id,tenant_id,run_id,event_key,event_type,event_time,observed_at,created_at) VALUES('old','tenant','run','old','task_start',?,0,0)`, old.UnixMilli())
	n, err = s.expireRetentionRows(at, true)
	if err != nil || n != 2 {
		t.Fatalf("rows=%d,%v", n, err)
	}
	for _, path := range []string{manifest, diagnostic, checkpointBlobPath(tree), checkpointBlobPath(file), checkpointBlobPath(message)} {
		retentionExists(t, path, true)
	}
	if got := checkpointStatus(t, s, "old"); got != "failed" {
		t.Fatalf("dry status=%s", got)
	}
	for _, table := range []string{"checkpoint_trees", "checkpoint_tree_files", "messages", "task_run_events"} {
		if n := retentionCount(t, s, "SELECT COUNT(*) FROM "+table); n != 1 {
			t.Fatalf("dry %s=%d", table, n)
		}
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE root_tree_sha256=? AND message_tree_sha256=? AND manifest_path=?`, tree, message, manifest); n != 1 {
		t.Fatal("dry run changed checkpoint")
	}
	legacy := retentionBlob(t, "[]")
	retentionCheckpoint(t, s, "legacy", "claw", "ready", "manual", legacy, old)
	// Dry run still records expansions; otherwise every later phase stays gated.
	remaining, err := s.backfillCheckpointTrees()
	if err != nil || remaining != 0 {
		t.Fatalf("dry backfill=%d,%v", remaining, err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_trees WHERE sha256=?`, legacy); n != 1 {
		t.Fatal("dry backfill did not record tree")
	}
}

func TestRetentionSettings(t *testing.T) {
	s := retentionServer(t)
	for _, cfg := range []*types.HubConfig{nil, {}} {
		s.hubCfg = cfg
		settings := s.retentionSettings()
		if settings.enabled || settings.interval != time.Hour || settings.maxAge != 2160*time.Hour {
			t.Fatalf("defaults=%+v", settings)
		}
	}
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: true, DryRun: true, Interval: "5m", MaxAge: "24h"}}
	settings := s.retentionSettings()
	if !settings.enabled || !settings.dryRun || settings.interval != 5*time.Minute || settings.maxAge != 24*time.Hour {
		t.Fatalf("settings=%+v", settings)
	}
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	s.hubCfg.Retention = &types.RetentionConfig{Enabled: true, Interval: "bad-retention-interval", MaxAge: "23h59m"}
	for i := 0; i < 2; i++ {
		settings = s.retentionSettings()
		if settings.interval != time.Hour || settings.maxAge != 2160*time.Hour {
			t.Fatalf("fallback=%+v", settings)
		}
	}
	if strings.Count(output.String(), "bad-retention-interval") != 1 || strings.Count(output.String(), "23h59m") != 1 {
		t.Fatalf("warnings should log once: %s", output.String())
	}
	s.hubCfg.Retention = &types.RetentionConfig{Interval: "4m59s", MaxAge: "invalid-retention-age"}
	settings = s.retentionSettings()
	if settings.enabled || settings.interval != time.Hour || settings.maxAge != 2160*time.Hour {
		t.Fatalf("floors=%+v", settings)
	}
}

func TestRetentionMigration(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE tenants(id TEXT PRIMARY KEY,name TEXT NOT NULL,token TEXT NOT NULL UNIQUE,claw_token TEXT NOT NULL UNIQUE,created_at DATETIME NOT NULL); INSERT INTO tenants VALUES('legacy','Legacy','token','claw-token',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO checkpoint_trees VALUES('tree',CURRENT_TIMESTAMP); INSERT INTO checkpoint_tree_files VALUES('tree','file')`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE id='legacy'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("legacy row=%d,%v", n, err)
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM checkpoint_trees t JOIN checkpoint_tree_files f ON f.tree_sha256=t.sha256`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("retention rows survived second migrate=%d,%v", n, err)
	}
	if _, err = db.Exec(`INSERT INTO checkpoint_tree_files VALUES('tree','file')`); err == nil {
		t.Fatal("duplicate tree-file accepted")
	}
	var schema string
	if err = db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='checkpoint_tree_files'`).Scan(&schema); err != nil || !strings.Contains(strings.ToUpper(schema), "WITHOUT ROWID") {
		t.Fatalf("schema=%s,%v", schema, err)
	}
	for _, name := range []string{"idx_checkpoint_tree_files_file", "idx_claw_checkpoints_root_tree", "idx_claw_checkpoints_message_tree"} {
		if err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%s=%d,%v", name, n, err)
		}
	}
}

func TestRetentionSettingsPatchTakesEffectAndPreservesSection(t *testing.T) {
	s := retentionServer(t)
	t.Setenv("ELASTICCLAW_HUB_CONFIG", filepath.Join(t.TempDir(), "hub.yaml"))
	for _, body := range []string{`{"retention":{"enabled":true,"interval":"5m","maxAge":"24h","dryRun":true}}`, `{"uiPassword":"test-password"}`} {
		rr := httptest.NewRecorder()
		s.patchSettings(rr, httptest.NewRequest(http.MethodPatch, "/api/settings", strings.NewReader(body)))
		if rr.Code != http.StatusOK {
			t.Fatalf("patch=%d %s", rr.Code, rr.Body.String())
		}
		got := s.retentionSettings()
		if !got.enabled || !got.dryRun || got.interval != 5*time.Minute || got.maxAge != 24*time.Hour {
			t.Fatalf("updated settings=%+v", got)
		}
	}
	rr := httptest.NewRecorder()
	s.getSettings(rr, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var view struct {
		Retention *types.RetentionConfig `json:"retention"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Retention == nil || !view.Retention.Enabled {
		t.Fatalf("settings view=%s", rr.Body.String())
	}
}

func TestRetentionCycleBackfillsBeforeCompactingAndCollecting(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	old := at.Add(-100 * 24 * time.Hour)
	retentionExec(t, s, `UPDATE claws SET status='deleted' WHERE id='claw'`)
	file := retentionBlob(t, "cycle-file")
	data, err := json.Marshal([]types.CheckpointFile{{SHA256: file}})
	if err != nil {
		t.Fatal(err)
	}
	tree := retentionBlob(t, string(data))
	manifest := retentionCheckpoint(t, s, "legacy", "claw", "skipped", "manual", tree, old)
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	s.retainOnce(at, retentionSettings{enabled: true, maxAge: 90 * 24 * time.Hour})
	if got := checkpointStatus(t, s, "legacy"); got != "compacted" {
		t.Fatalf("cycle status=%s", got)
	}
	for _, path := range []string{manifest, checkpointBlobPath(tree), checkpointBlobPath(file)} {
		retentionExists(t, path, false)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_trees`); n != 0 {
		t.Fatalf("trees=%d", n)
	}
	if !strings.Contains(output.String(), "retention: compacted=1 expired=0 diagnostics=0 rows=0 trees=1 blobs=2 freed=0MB dry_run=false unexpanded=0") {
		t.Fatalf("cycle log=%s", output.String())
	}
}

func TestRetentionPlanReexpandsRegisteredTree(t *testing.T) {
	s := retentionServer(t)
	tree := retentionBlob(t, "revived-tree")
	shared := retentionBlob(t, "revived-file")
	retentionRecord(t, s, "", tree)
	retentionCheckpoint(t, s, "cp", "claw", "creating", "manual", "", time.Now().UTC())
	retentionRecord(t, s, "cp", tree, tree, shared)
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files WHERE tree_sha256=? AND file_sha256=?`, tree, shared); n != 1 {
		t.Fatalf("revived tree expansion rows=%d", n)
	}
}

func TestRetentionPlanExpandsStoredTreeFromDisk(t *testing.T) {
	s := retentionServer(t)
	a := retentionBlob(t, "stored-a")
	b := retentionBlob(t, "stored-b")
	data, err := json.Marshal([]types.CheckpointFile{{SHA256: a}, {SHA256: b}})
	if err != nil {
		t.Fatal(err)
	}
	tree := retentionBlob(t, string(data))
	insertTestCheckpoint(t, s, "cp", "manual")
	body, err := json.Marshal(types.CheckpointPlan{RootSHA256: tree})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/checkpoints/cp/plan", bytes.NewReader(body))
	req.Header.Set("X-Claw-Token", "claw-token")
	rr := httptest.NewRecorder()
	s.handleCheckpointInternal(rr, req)
	if rr.Code != 200 {
		t.Fatalf("plan: %d %s", rr.Code, rr.Body.String())
	}
	for _, file := range []string{a, b} {
		if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files WHERE tree_sha256=? AND file_sha256=?`, tree, file); n != 1 {
			t.Fatalf("stored tree file %s not expanded from disk", file)
		}
	}
}

func retentionPlan(t *testing.T, s *Server, id string, plan types.CheckpointPlan) {
	t.Helper()
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/checkpoints/"+id+"/plan", bytes.NewReader(body))
	req.Header.Set("X-Claw-Token", "claw-token")
	rr := httptest.NewRecorder()
	s.handleCheckpointInternal(rr, req)
	if rr.Code != 200 {
		t.Fatalf("plan: %d %s", rr.Code, rr.Body.String())
	}
}

func TestFinalizeRecordsExpansionFromTreeBlob(t *testing.T) {
	s := retentionServer(t)
	file := retentionBlob(t, "planned-file")
	data, err := json.Marshal([]types.CheckpointFile{{SHA256: file}})
	if err != nil {
		t.Fatal(err)
	}
	tree := fmt.Sprintf("%x", sha256.Sum256(data))
	insertTestCheckpoint(t, s, "cp", "manual")
	// The plan names no files for a root the hub has never stored; the blob
	// uploaded afterwards is the authoritative expansion.
	retentionPlan(t, s, "cp", types.CheckpointPlan{RootSHA256: tree, Files: []types.CheckpointFile{}})
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files WHERE tree_sha256=?`, tree); n != 0 {
		t.Fatalf("empty plan recorded %d files", n)
	}
	retentionFile(t, checkpointBlobPath(tree), string(data), time.Now())
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", tree); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if got := checkpointStatus(t, s, "cp"); got != "ready" {
		t.Fatalf("status=%s", got)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files WHERE tree_sha256=? AND file_sha256=?`, tree, file); n != 1 {
		t.Fatal("finalize left the tree blob's file unreferenced")
	}
}

func TestFinalizeKeepsMatchingExpansion(t *testing.T) {
	s := retentionServer(t)
	file := retentionBlob(t, "honest-file")
	data, err := json.Marshal([]types.CheckpointFile{{SHA256: file}})
	if err != nil {
		t.Fatal(err)
	}
	tree := retentionBlob(t, string(data))
	insertTestCheckpoint(t, s, "cp", "manual")
	retentionPlan(t, s, "cp", types.CheckpointPlan{RootSHA256: tree, Files: []types.CheckpointFile{{SHA256: file}}})
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", tree); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if got := checkpointStatus(t, s, "cp"); got != "ready" {
		t.Fatalf("status=%s", got)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_tree_files WHERE tree_sha256=?`, tree); n != 1 {
		t.Fatalf("expansion rows=%d", n)
	}
}

func TestRetentionIgnoresLegacyNonHexRoot(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	old := at.Add(-100 * 24 * time.Hour)
	retentionCheckpoint(t, s, "bad", "claw", "ready", "manual", "../x", at)
	diagnostic := filepath.Join(hubDataDir(), "diagnostics", "old.log")
	retentionFile(t, diagnostic, "log", old)
	retentionExec(t, s, `INSERT INTO messages(id,tenant_id,claw_id,role,content,created_at) VALUES('old','tenant','claw','user','hi',?)`, old)
	unexpanded, err := s.backfillCheckpointTrees()
	if err != nil || unexpanded != 0 {
		t.Fatalf("backfill=%d,%v", unexpanded, err)
	}
	s.retainOnce(at, retentionSettings{enabled: true, maxAge: 90 * 24 * time.Hour})
	retentionExists(t, diagnostic, false)
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("row expiry gated by non-hex root: %d", n)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_trees`); n != 0 {
		t.Fatalf("non-hex root registered: %d", n)
	}
}

func TestRetentionGatesUppercaseHexRoot(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	tree := strings.ToUpper(retentionBlob(t, "[]"))
	retentionCheckpoint(t, s, "upper", "claw", "ready", "manual", tree, at)
	if n, err := s.unexpandedCheckpointTrees(); err != nil || n != 1 {
		t.Fatalf("uppercase root not gated: unexpanded=%d err=%v", n, err)
	}
	unexpanded, err := s.backfillCheckpointTrees()
	if err != nil || unexpanded != 0 {
		t.Fatalf("backfill=%d,%v", unexpanded, err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_trees WHERE sha256=?`, tree); n != 1 {
		t.Fatal("uppercase root not backfilled")
	}
}

func TestFinalizeRejectsNonHexRoot(t *testing.T) {
	s := retentionServer(t)
	insertTestCheckpoint(t, s, "cp", "manual")
	if err := s.finalizeCheckpoint("cp", "tenant", "claw", "../x"); err == nil {
		t.Fatal("finalized a non-hex root")
	}
	retentionExists(t, checkpointManifestPath("cp"), false)
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='cp' AND status='creating' AND root_tree_sha256='' AND manifest_path=''`); n != 1 {
		t.Fatal("non-hex root reached the row")
	}
}

func TestRetentionBackfillLeavesUnreadableTreeUnexpanded(t *testing.T) {
	s := retentionServer(t)
	unreadable := fmt.Sprintf("%064x", 1)
	if err := os.MkdirAll(checkpointBlobPath(unreadable), 0755); err != nil {
		t.Fatal(err)
	}
	retentionCheckpoint(t, s, "unreadable", "claw", "ready", "manual", unreadable, time.Now().UTC())
	remaining, err := s.backfillCheckpointTrees()
	if err != nil || remaining != 1 {
		t.Fatalf("backfill=%d,%v", remaining, err)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_trees`); n != 0 {
		t.Fatalf("unreadable tree registered: %d", n)
	}
}

func TestRetentionCycleGatesCompactionOnBackfill(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	old := at.Add(-3 * time.Hour)
	retentionExec(t, s, `UPDATE claws SET status='deleted' WHERE id='claw'`)
	trees := make([]string, 60)
	for i := range trees {
		data, err := json.Marshal([]types.CheckpointFile{{SHA256: retentionBlob(t, fmt.Sprintf("gate-file-%d", i))}})
		if err != nil {
			t.Fatal(err)
		}
		trees[i] = retentionBlob(t, string(data))
		retentionCheckpoint(t, s, fmt.Sprintf("legacy-%d", i), "claw", "ready", "manual", trees[i], old.Add(time.Duration(i)*time.Minute))
	}
	for i := 0; i < 3; i++ {
		s.retainOnce(at, retentionSettings{enabled: true, maxAge: 90 * 24 * time.Hour})
	}
	remaining := 0
	for _, tree := range trees {
		if _, err := os.Stat(checkpointBlobPath(tree)); err == nil {
			remaining++
		}
	}
	if remaining != 1 {
		t.Fatalf("tree blobs left=%d, want only the retry pick", remaining)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM checkpoint_trees`); n != 1 {
		t.Fatalf("registered trees=%d", n)
	}
}

func TestRetentionCompactionSkipsPreviouslyRestoredLikeRetry(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	old := at.Add(-3 * time.Hour)
	retentionExec(t, s, `UPDATE claws SET status='error',task_run_id='run' WHERE id='claw'`)
	retentionRun(t, s)
	retentionCheckpoint(t, s, "oldest", "claw", "ready", "manual", "oldest-tree", old)
	retentionCheckpoint(t, s, "older", "claw", "ready", "manual", "older-tree", old.Add(time.Minute))
	retentionCheckpoint(t, s, "newest", "claw", "ready", "manual", "newest-tree", old.Add(2*time.Minute))
	retentionExec(t, s, `UPDATE task_run_attempts SET restored_checkpoint_id='newest' WHERE id='attempt'`)
	// The pending retry's attempt row already exists, without a pointer.
	retentionExec(t, s, `INSERT INTO task_run_attempts(id,tenant_id,run_id,attempt_id,attempt_number,started_at,created_at,updated_at) VALUES('pending','tenant','run','pending',2,0,0,0)`)
	if n, err := s.compactCheckpoints(at, false); err != nil || n != 1 {
		t.Fatalf("compacted=%d,%v", n, err)
	}
	for _, id := range []string{"newest", "older"} {
		if got := checkpointStatus(t, s, id); got != "ready" {
			t.Fatalf("retry candidate %s compacted: %s", id, got)
		}
	}
	if got := checkpointStatus(t, s, "oldest"); got != "compacted" {
		t.Fatalf("oldest=%s", got)
	}
}

func TestRetentionCollectorKeepsDigestsSharedAcrossRoles(t *testing.T) {
	s := retentionServer(t)
	at := time.Now().UTC()
	live := retentionBlob(t, "live-tree")
	fileAndMessage := retentionBlob(t, "file-and-message")
	dead := retentionBlob(t, "dead-tree")
	messageAndFile := retentionBlob(t, "message-and-file")
	retentionCheckpoint(t, s, "live", "claw", "ready", "manual", live, at)
	retentionRecord(t, s, "live", live, fileAndMessage)
	retentionCheckpoint(t, s, "dead", "claw", "compacted", "manual", "", at)
	retentionExec(t, s, `UPDATE claw_checkpoints SET message_tree_sha256=? WHERE id='dead'`, fileAndMessage)
	retentionExec(t, s, `UPDATE claw_checkpoints SET message_tree_sha256=? WHERE id='live'`, messageAndFile)
	retentionRecord(t, s, "", dead, messageAndFile)
	counts, err := s.collectCheckpointBlobs(at, false)
	if err != nil {
		t.Fatal(err)
	}
	if counts.trees != 1 || counts.blobs != 1 {
		t.Fatalf("collection=%+v", counts)
	}
	retentionExists(t, checkpointBlobPath(dead), false)
	for _, sha := range []string{live, fileAndMessage, messageAndFile} {
		retentionExists(t, checkpointBlobPath(sha), true)
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='live' AND message_tree_sha256=?`, messageAndFile); n != 1 {
		t.Fatal("live message pointer cleared")
	}
	if n := retentionCount(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE id='dead' AND message_tree_sha256=''`); n != 1 {
		t.Fatal("dead message pointer kept")
	}
}
