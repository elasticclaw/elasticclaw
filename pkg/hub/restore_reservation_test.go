package hub

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ---------------------------------------------------------------------------
// The restore reserves its source before anything destructive
// ---------------------------------------------------------------------------
//
// restoreClawFromCheckpoint used to validate the checkpoint, take a
// termination checkpoint of up to 90 seconds, tear the sandbox down, and only
// then write restore_checkpoint_id -- the one thing compaction and expiry
// honour. Retention in that window released the source, and the restore went
// on against a checkpoint that no longer existed with the sandbox already
// gone. The tests of the retention guards insert restore_checkpoint_id by
// hand, so none of them ever saw the window; this one runs a full retention
// cycle inside it.

func waitForClawToLeave(t *testing.T, s *Server, clawID, status string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var current string
		if err := s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&current); err != nil {
			t.Fatal(err)
		}
		if current != status {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fixture: claw %s is still %q; the restore's provisioning goroutine did not settle", clawID, status)
}

// seedStaleClawsWithTwoCheckpoints is seedTwoStaleClawsWithTwoCheckpoints
// for the given claw ids.
func seedStaleClawsWithTwoCheckpoints(t *testing.T, s *Server, reference time.Time, claws ...string) (manifests map[string]string, files map[string][]string) {
	t.Helper()
	manifests = map[string]string{}
	files = map[string][]string{}
	for _, claw := range claws {
		insertRetentionClaw(t, s, claw, reference.Add(-400*24*time.Hour))
		oldRoot, oldFiles, _ := planTreeFixtureSeeded(t, 2, claw+" old ")
		newRoot, _, _ := planTreeFixtureSeeded(t, 1, claw+" new ")
		manifests[claw+"-old"] = insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: claw + "-old", clawID: claw, status: "ready", createdAt: reference.Add(-60 * 24 * time.Hour),
			rootTree: oldRoot, writeManifest: true})
		files[claw+"-old"] = append(oldFiles, oldRoot)
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: claw + "-newest", clawID: claw, status: "ready", createdAt: reference.Add(-30 * 24 * time.Hour),
			rootTree: newRoot, writeManifest: true})
	}
	return manifests, files
}

// Revert verified against: reserveRestoreSource without its UPDATE (the id
// written only by the status UPDATE at the end, as before).
func TestRestoreReservesItsSourceBeforeRetentionCanReleaseIt(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	s.nowFunc = func() time.Time { return reference }
	s.hubCfg = &types.HubConfig{Retention: &types.RetentionConfig{Enabled: boolPtr(true)}}
	// The control claw has the same shape and no restore, so the cycle that
	// runs inside the window is proven to compact; the other is being restored
	// from its old checkpoint. (Ids of at least eight characters: the failed
	// provisioning that follows the restore on this server logs clawID[:8].)
	const control, restoring = "claw-control", "claw-restoring"
	manifests, files := seedStaleClawsWithTwoCheckpoints(t, s, reference, control, restoring)

	var out string
	ran := false
	restoreReservedHook = func(clawID, checkpointID string) {
		if clawID != restoring || checkpointID != restoring+"-old" {
			t.Errorf("hook saw restore of %s from %s", clawID, checkpointID)
		}
		if status, _, _ := retentionCheckpointRow(t, s, restoring+"-old"); status != "ready" {
			t.Fatal("fixture: the source was not ready when the restore was validated")
		}
		out = captureRetentionLog(t, s.retentionSweepOnce)
		ran = true
	}
	t.Cleanup(func() { restoreReservedHook = nil })

	if err := s.restoreClawFromCheckpoint(context.Background(), "tenant", restoring, restoring+"-old"); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	if !ran {
		t.Fatal("fixture: retention never ran inside the restore's window")
	}
	// The restore's provisioning runs in a goroutine and fails on this server
	// (no provider is configured); let it settle before the store closes.
	waitForClawToLeave(t, s, restoring, "provisioning")

	if status, _, _ := retentionCheckpointRow(t, s, control+"-old"); status != "compacted" {
		t.Fatalf("%s-old = %q; the cycle inside the window did not compact, so it proves nothing:\n%s", control, status, out)
	}
	assertCheckpointIntact(t, s, restoring+"-old", manifests[restoring+"-old"], files[restoring+"-old"], out)
	var reserved string
	if err := s.db.QueryRow(`SELECT COALESCE(restore_checkpoint_id,'') FROM claws WHERE id=?`, restoring).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != restoring+"-old" {
		t.Fatalf("restore_checkpoint_id = %q after the restore, want %s-old", reserved, restoring)
	}
}

// A restore that retention beat to the row must fail at validation, before
// the termination checkpoint and the teardown, and must leave the claw as it
// found it: no reservation, no status change, no sandbox gone.
func TestRestoreOfACompactedCheckpointFailsBeforeAnythingIsDestroyed(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	root, _, _ := planTreeFixtureSeeded(t, 1, "claw ")
	insertRetentionCheckpoint(t, s, retentionCheckpoint{
		id: "cp", clawID: "claw", status: "ready", createdAt: reference, rootTree: root, writeManifest: true})
	if _, err := s.markCheckpointsCompacted([]string{"cp"}); err != nil {
		t.Fatal(err)
	}
	restoreReservedHook = func(clawID, checkpointID string) {
		t.Errorf("the restore reserved %s although the checkpoint is compacted", checkpointID)
	}
	t.Cleanup(func() { restoreReservedHook = nil })

	err := s.restoreClawFromCheckpoint(context.Background(), "tenant", "claw", "cp")
	if err == nil || err.Error() != "checkpoint is not ready" {
		t.Fatalf("restore of a compacted checkpoint returned %v, want \"checkpoint is not ready\"", err)
	}
	var status, reserved string
	if err := s.db.QueryRow(`SELECT status, COALESCE(restore_checkpoint_id,'') FROM claws WHERE id='claw'`).Scan(&status, &reserved); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || reserved != "" {
		t.Fatalf("claw is %q with restore_checkpoint_id %q after the refused restore, want completed and none", status, reserved)
	}
	if n := countRetentionRows(t, s, `SELECT COUNT(*) FROM claw_checkpoints WHERE claw_id='claw' AND reason LIKE 'termination:%'`); n != 0 {
		t.Fatal("the refused restore took a termination checkpoint")
	}
}

// The reservation is undone when the restore aborts before it has destroyed
// anything, and put back to what the claw carried -- a failed earlier
// restore's id is the operator's retry point and must survive a refused one.
func TestAbortedRestoreReleasesItsReservation(t *testing.T) {
	s := newRetentionTestServer(t)
	reference := time.Now()
	insertRetentionClaw(t, s, "claw", reference)
	root, _, _ := planTreeFixtureSeeded(t, 1, "claw ")
	for _, id := range []string{"cp-earlier", "cp"} {
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: id, clawID: "claw", status: "ready", createdAt: reference, rootTree: root, writeManifest: true})
	}
	if _, err := s.db.Exec(`UPDATE claws SET restore_checkpoint_id='cp-earlier' WHERE id='claw'`); err != nil {
		t.Fatal(err)
	}
	// The provider lookup after the termination checkpoint is the last exit
	// before the teardown; make it fail by taking the claw's row away from
	// the tenant the restore is acting for.
	restoreReservedHook = func(clawID, checkpointID string) {
		var reserved string
		if err := s.db.QueryRow(`SELECT COALESCE(restore_checkpoint_id,'') FROM claws WHERE id='claw'`).Scan(&reserved); err != nil {
			t.Fatal(err)
		}
		if reserved != "cp" {
			t.Fatalf("restore_checkpoint_id = %q inside the window, want cp", reserved)
		}
		if _, err := s.db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES('elsewhere','elsewhere','t2','c2',?)`, now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE claws SET tenant_id='elsewhere' WHERE id='claw'`); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { restoreReservedHook = nil })

	if err := s.restoreClawFromCheckpoint(context.Background(), "tenant", "claw", "cp"); err == nil {
		t.Fatal("restore succeeded although its provider lookup could not find the claw")
	}
	var reserved string
	if err := s.db.QueryRow(`SELECT COALESCE(restore_checkpoint_id,'') FROM claws WHERE id='claw'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != "cp-earlier" {
		t.Fatalf("restore_checkpoint_id = %q after the aborted restore, want the earlier restore's cp-earlier back", reserved)
	}
	if _, err := os.Stat(checkpointManifestPath("cp")); err != nil {
		t.Fatalf("the aborted restore's source lost its manifest: %v", err)
	}
}
