package hub

import (
	"context"
	"os"
	"sync"
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

// ---------------------------------------------------------------------------
// The reservation is owned by the restore that wrote it last
// ---------------------------------------------------------------------------
//
// Two restores of one claw overlap whenever an operator restores again while
// the first is still in its termination checkpoint (up to 90 seconds) or its
// provisioning (minutes). The later reservation supersedes the earlier one;
// what must not happen is the earlier restore writing its own id back over
// it, tearing the sandbox down, and provisioning from a checkpoint that
// retention is now free to release under it.

// seedClawWithTwoRestorableCheckpoints seeds a live claw on a VM with two
// 'ready' checkpoints, cp-a and cp-b, to restore from.
func seedClawWithTwoRestorableCheckpoints(t *testing.T, s *Server) {
	t.Helper()
	reference := time.Now()
	insertRetentionClaw(t, s, "claw-owned", reference)
	if _, err := s.db.Exec(`UPDATE claws SET status='running', provider='daytona', provider_id='vm-1' WHERE id='claw-owned'`); err != nil {
		t.Fatal(err)
	}
	root, _, _ := planTreeFixtureSeeded(t, 1, "owned ")
	for _, id := range []string{"cp-a", "cp-b"} {
		insertRetentionCheckpoint(t, s, retentionCheckpoint{
			id: id, clawID: "claw-owned", status: "ready", createdAt: reference, rootTree: root, writeManifest: true})
	}
}

func clawRestoreColumns(t *testing.T, s *Server, clawID string) (status, reserved, restoredFrom, providerID string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT status, COALESCE(restore_checkpoint_id,''), COALESCE(restored_from_checkpoint_id,''), COALESCE(provider_id,'') FROM claws WHERE id=?`, clawID).
		Scan(&status, &reserved, &restoredFrom, &providerID); err != nil {
		t.Fatal(err)
	}
	return
}

// Revert verified against: the status UPDATE in restoreClawFromCheckpoint
// without its AND restore_checkpoint_id=? guard (as before).
//
// R1 restores from cp-a. While R1 is between its reservation and its commit,
// R2 restores from cp-b and reserves it, then parks. R1 runs on and must stop:
// no teardown, no reservation overwritten, no provision. R2 then completes as
// the owner. Both restores really run; the interleaving is held by channels.
func TestSupersededRestoreStopsAndKeepsTheNewerReservation(t *testing.T) {
	s := newRetentionTestServer(t)
	seedClawWithTwoRestorableCheckpoints(t, s)
	var (
		terminated   []string
		terminatedMu sync.Mutex
	)
	s.terminateVMOverride = func(_, id string) error {
		terminatedMu.Lock()
		defer terminatedMu.Unlock()
		terminated = append(terminated, id)
		return nil
	}

	r2Reserved := make(chan struct{})
	r1Done := make(chan struct{})
	r2Err := make(chan error, 1)
	restoreReservedHook = func(clawID, checkpointID string) {
		switch checkpointID {
		case "cp-a":
			// R1 is past its reservation and has destroyed nothing: start R2
			// and wait until it has reserved cp-b.
			go func() { r2Err <- s.restoreClawFromCheckpoint(context.Background(), "tenant", "claw-owned", "cp-b") }()
			<-r2Reserved
		case "cp-b":
			close(r2Reserved)
			<-r1Done
		}
	}
	t.Cleanup(func() { restoreReservedHook = nil })

	r1Err := s.restoreClawFromCheckpoint(context.Background(), "tenant", "claw-owned", "cp-a")
	if r1Err == nil {
		t.Fatal("R1 went on to provision although R2 had reserved the claw under it")
	}
	if !containsString(r1Err.Error(), "superseded") {
		t.Fatalf("R1 returned %v, want a superseded error", r1Err)
	}
	status, reserved, restoredFrom, providerID := clawRestoreColumns(t, s, "claw-owned")
	if reserved != "cp-b" {
		t.Fatalf("restore_checkpoint_id = %q after R1 stopped, want R2's cp-b", reserved)
	}
	if status != "running" || providerID != "vm-1" || restoredFrom != "" {
		t.Fatalf("R1 changed the claw it no longer owned: status %q, provider_id %q, restored_from %q", status, providerID, restoredFrom)
	}
	terminatedMu.Lock()
	n := len(terminated)
	terminatedMu.Unlock()
	if n != 0 {
		t.Fatalf("R1 terminated %v although R2 owned the claw", terminated)
	}

	close(r1Done)
	if err := <-r2Err; err != nil {
		t.Fatalf("R2, the owner, failed: %v", err)
	}
	status, reserved, restoredFrom, providerID = clawRestoreColumns(t, s, "claw-owned")
	if reserved != "cp-b" || restoredFrom != "cp-b" {
		t.Fatalf("after R2 committed: restore_checkpoint_id %q, restored_from %q, want cp-b for both", reserved, restoredFrom)
	}
	if status != "provisioning" && status != "error" {
		t.Fatalf("after R2 committed the claw is %q, want provisioning (or error once its provisioning failed)", status)
	}
	if providerID != "" {
		t.Fatalf("R2 left provider_id %q on the claw it tore down", providerID)
	}
	terminatedMu.Lock()
	got := append([]string(nil), terminated...)
	terminatedMu.Unlock()
	if len(got) != 1 || got[0] != "vm-1" {
		t.Fatalf("terminated VMs = %v, want exactly R2's teardown of vm-1", got)
	}
	// R2's provisioning runs in a goroutine and fails on this server (no
	// provider configured); let it settle before the store closes.
	waitForClawToLeave(t, s, "claw-owned", "provisioning")
}

// Revert verified against: releaseRestoreReservation without its
// AND restore_checkpoint_id=? guard.
//
// R1 reserves cp-a; R2 restores from cp-b to completion while R1 is still in
// its window; R1 then aborts at its provider lookup, before its teardown. The
// release R1 does on that abort must not touch R2's reservation.
func TestAbortedRestoreDoesNotReleaseTheNewerRestoresReservation(t *testing.T) {
	s := newRetentionTestServer(t)
	seedClawWithTwoRestorableCheckpoints(t, s)
	s.terminateVMOverride = func(_, _ string) error { return nil }

	restoreReservedHook = func(clawID, checkpointID string) {
		if checkpointID != "cp-a" {
			return
		}
		if err := s.restoreClawFromCheckpoint(context.Background(), "tenant", "claw-owned", "cp-b"); err != nil {
			t.Fatalf("R2: %v", err)
		}
		// Make R1's provider lookup fail by taking the row away from the
		// tenant it is acting for: the last exit before its teardown.
		if _, err := s.db.Exec(`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES('elsewhere','elsewhere','t2','c2',?)`, now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE claws SET tenant_id='elsewhere' WHERE id='claw-owned'`); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { restoreReservedHook = nil })

	if err := s.restoreClawFromCheckpoint(context.Background(), "tenant", "claw-owned", "cp-a"); err == nil {
		t.Fatal("R1 succeeded although its provider lookup could not find the claw")
	}
	_, reserved, _, _ := clawRestoreColumns(t, s, "claw-owned")
	if reserved != "cp-b" {
		t.Fatalf("restore_checkpoint_id = %q after R1 aborted, want R2's cp-b untouched", reserved)
	}
	waitForClawToLeave(t, s, "claw-owned", "provisioning")
}

// Revert verified against: markRestoreApplied without its
// AND restore_checkpoint_id=? guard.
//
// The provisioning that applies cp-a reads the reservation once
// (pendingRestoreCheckpoint) and writes files for minutes. A restore from cp-b
// that reserves in the meantime owns the claw; the first provisioning's
// completion must not clear that reservation while the second reads cp-b.
func TestCompletingRestoreClearsOnlyItsOwnReservation(t *testing.T) {
	s := newRetentionTestServer(t)
	seedClawWithTwoRestorableCheckpoints(t, s)
	if _, err := s.reserveRestoreSource("tenant", "claw-owned", "cp-a"); err != nil {
		t.Fatal(err)
	}
	applying := s.pendingRestoreCheckpoint("claw-owned")
	if applying != "cp-a" {
		t.Fatalf("fixture: provisioning read %q, want cp-a", applying)
	}
	if previous, err := s.reserveRestoreSource("tenant", "claw-owned", "cp-b"); err != nil || previous != "cp-a" {
		t.Fatalf("second reservation: previous %q, err %v", previous, err)
	}

	s.markRestoreApplied("claw-owned", applying)
	_, reserved, restoredFrom, _ := clawRestoreColumns(t, s, "claw-owned")
	if reserved != "cp-b" {
		t.Fatalf("restore_checkpoint_id = %q after the first restore applied, want the second's cp-b", reserved)
	}
	if restoredFrom == "cp-a" {
		t.Fatal("the claw records cp-a as what it was restored from while cp-b is being restored")
	}

	// The owner's completion clears it.
	s.markRestoreApplied("claw-owned", "cp-b")
	_, reserved, restoredFrom, _ = clawRestoreColumns(t, s, "claw-owned")
	if reserved != "" || restoredFrom != "cp-b" {
		t.Fatalf("after the owner applied: restore_checkpoint_id %q, restored_from %q, want none and cp-b", reserved, restoredFrom)
	}
}

// Revert verified against: releaseRestoreReservation without its log line.
//
// An aborted restore whose release also fails leaves a reservation on a claw
// whose status never changed; finalizedClawPredicateSQL then never finalizes
// it. The operator must be able to find that claw in the log.
func TestLeakedReservationIsNamedInTheLog(t *testing.T) {
	s := newRetentionTestServer(t)
	seedClawWithTwoRestorableCheckpoints(t, s)
	restoreReservedHook = func(clawID, checkpointID string) {
		// Every write after the reservation now fails, the release included.
		s.db.Close()
	}
	t.Cleanup(func() { restoreReservedHook = nil })

	var err error
	out := captureRetentionLog(t, func() {
		err = s.restoreClawFromCheckpoint(context.Background(), "tenant", "claw-owned", "cp-a")
	})
	if err == nil {
		t.Fatal("the restore succeeded against a closed store")
	}
	if !containsString(out, "leaked reservation") || !containsString(out, "claw-owned") || !containsString(out, "cp-a") {
		t.Fatalf("the log does not name the leaked reservation of cp-a on claw-owned:\n%s", out)
	}
}
