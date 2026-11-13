package hub

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// A dry run must not take the write lock: the hourly dry pass used to hold
// BEGIN IMMEDIATE transactions long enough for other writers to hit
// SQLITE_BUSY. Holding the lock from another connection proves the dry pass
// never needs it; with the old transactions every phase below would time out.
func TestRetentionDryRunHoldsNoWriteLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ELASTICCLAW_HUB_CONFIG", "/nonexistent/hub.yaml")
	db, err := openDB(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{db: db, hubCfg: &types.HubConfig{}, claws: map[string]*clawConn{}}
	retentionExec(t, s, `INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES('tenant','tenant','token','claw-token',?)`, now())
	retentionExec(t, s, `INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES('claw','tenant','claw','template','error',?)`, now())

	at := time.Now().UTC()
	old := at.Add(-100 * 24 * time.Hour)
	tree := retentionBlob(t, "lock-tree")
	file := retentionBlob(t, "lock-file")
	message := retentionBlob(t, "lock-message")
	retentionRecord(t, s, "", tree, file)
	retentionCheckpoint(t, s, "old", "claw", "failed", "manual", tree, old)
	retentionExec(t, s, `UPDATE claw_checkpoints SET message_tree_sha256=?`, message)

	writer, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.Exec(`UPDATE tenants SET name=name`); err != nil {
		t.Fatal(err)
	}

	if n, err := s.compactCheckpoints(at, true); err != nil || n != 1 {
		t.Fatalf("compaction=%d,%v", n, err)
	}
	if n, err := s.expireCheckpoints(at.Add(-90*24*time.Hour), true); err != nil || n != 1 {
		t.Fatalf("expiry=%d,%v", n, err)
	}
	counts, err := s.collectCheckpointBlobs(at, true)
	if err != nil || counts.trees != 1 || counts.blobs != 3 {
		t.Fatalf("collection=%+v,%v", counts, err)
	}
}
