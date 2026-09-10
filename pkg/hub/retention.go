package hub

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Storage reclamation for the hub's on-disk state.
//
// The hub accumulates three kinds of storage that nothing ever removed: one
// manifest per checkpoint, one content-addressed blob per distinct workspace
// file, and a captured gateway/bridge log per terminated Daytona claw. On the
// Faster hub that filled the disk, at which point every write path — including
// the migrations — starts failing. A cycle reclaims in three phases:
//
//	compaction -> retention -> blob sweep
//
// The order matters. Compaction and retention are what release manifests and
// checkpoint rows; the blob sweep derives its keep set from whatever survived
// them, so running it first would keep blobs for manifests that are about to
// disappear and leave them orphaned until the next cycle.

const (
	defaultRetentionInterval     = time.Hour
	defaultRetentionMaxAge       = 90 * 24 * time.Hour
	defaultRetentionCompactAfter = 10 * 24 * time.Hour
)

type retentionSettings struct {
	interval     time.Duration
	maxAge       time.Duration
	compactAfter time.Duration
}

func retentionConfig(cfg *types.HubConfig) *types.RetentionConfig {
	if cfg == nil {
		return nil
	}
	return cfg.Retention
}

func (s *Server) retentionEnabled() bool {
	s.mu.RLock()
	r := retentionConfig(s.hubCfg)
	s.mu.RUnlock()
	if r == nil || r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

func (s *Server) retentionSettings() retentionSettings {
	cfg := retentionSettings{
		interval:     defaultRetentionInterval,
		maxAge:       defaultRetentionMaxAge,
		compactAfter: defaultRetentionCompactAfter,
	}
	s.mu.RLock()
	r := retentionConfig(s.hubCfg)
	s.mu.RUnlock()
	if r == nil {
		return cfg
	}
	parse := func(value string, fallback time.Duration, name string) time.Duration {
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			log.Printf("[retention] invalid %s %q; using %s", name, value, fallback)
			return fallback
		}
		return d
	}
	cfg.interval = parse(r.Interval, cfg.interval, "interval")
	cfg.maxAge = parse(r.MaxAge, cfg.maxAge, "max_age")
	cfg.compactAfter = parse(r.CompactAfter, cfg.compactAfter, "compact_after")
	return cfg
}

func (s *Server) retentionNow() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc().UTC()
	}
	return now()
}

// retentionSweeper runs a reclamation cycle on every tick.
//
// It deliberately does NOT run at boot. Deleting is irreversible and the
// window an operator has to notice a misconfigured retention policy is the
// window between the hub coming up and the first sweep: starting on the first
// tick means a fresh deploy always leaves `interval` (one hour by default) to
// export data or turn the sweeper off before anything is removed. Restarting
// the hub therefore never sweeps immediately, which also stops a crash loop
// from turning into a delete loop.
func (s *Server) retentionSweeper() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[retention] loop panic, restarting: %v", r)
				}
			}()
			ticker := time.NewTicker(s.retentionSettings().interval)
			defer ticker.Stop()
			for range ticker.C {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[retention] cycle panic: %v", r)
						}
					}()
					s.retentionSweepOnce()
				}()
			}
		}()
	}
}

// retentionSweepOnce runs one full reclamation cycle.
func (s *Server) retentionSweepOnce() {
	if !s.retentionEnabled() {
		return
	}
	cfg, n := s.retentionSettings(), s.retentionNow()

	compacted, err := s.compactFinalizedCheckpoints(n.Add(-cfg.compactAfter))
	if err != nil {
		log.Printf("[retention] compaction: %v", err)
	} else if compacted > 0 {
		log.Printf("[retention] compacted %d superseded checkpoint manifests", compacted)
	}

	s.applyRetentionWindow(n.Add(-cfg.maxAge))

	swept, bytes, err := s.sweepCheckpointBlobs()
	if err != nil {
		log.Printf("[retention] blob sweep skipped: %v", err)
	} else if swept > 0 {
		log.Printf("[retention] swept %d unreferenced blobs (%d bytes)", swept, bytes)
	}
}

// ---------------------------------------------------------------------------
// Finalized claws
// ---------------------------------------------------------------------------

// finalizedClawPredicateSQL is the canonical "this claw is done" test. It is a
// fragment rather than a helper function because a Python backfill has to
// reproduce it exactly against the same database; keeping it as one literal
// makes the two implementations diffable.
//
// A claw is finalized when EITHER it landed a merged PR — the work reached its
// destination, nothing more is coming — OR it has simply not changed for
// compact_after: no update to the claw row and no checkpoint recorded inside
// the window. The second arm is what covers claws that were abandoned, errored
// out, or never produced a PR at all.
//
// The merged-PR arm reads three tables because no single one is both durable
// and early. claw_prs is the earliest signal, but handleClawKill deletes those
// rows with the claw, so on its own it is nearly always empty: on the Faster
// hub exactly one row survives against sixty runs that actually merged, which
// would leave the merged arm dead and defer every compaction to the ten-day
// staleness arm. task_run_summaries and task_run_prs outlive the claw and carry
// the history, so they are what make the arm mean anything.
//
// The caller binds the cutoff (now - compact_after) twice. The alias `c` must
// be the claws table.
const finalizedClawPredicateSQL = `(
	EXISTS (SELECT 1 FROM claw_prs p WHERE p.claw_id = c.id AND p.merged = 1)
	OR EXISTS (
		SELECT 1 FROM task_run_summaries s
		 WHERE s.claw_id = c.id AND s.merged_pr_count > 0
	)
	OR EXISTS (
		SELECT 1 FROM task_run_prs rp
		  JOIN task_run_summaries rs ON rs.run_id = rp.run_id
		 WHERE rs.claw_id = c.id AND rp.merged = 1
	)
	OR (
		COALESCE(c.last_seen, c.created_at) < ?
		AND NOT EXISTS (
			SELECT 1 FROM claw_checkpoints cp WHERE cp.claw_id = c.id AND cp.created_at >= ?
		)
	)
)`

// clawFinalized evaluates the canonical predicate for one claw. A claw that no
// longer exists is not finalized: there is nothing left to compact.
func (s *Server) clawFinalized(clawID string, cutoff time.Time) (bool, error) {
	var finalized int
	err := s.db.QueryRow(
		`SELECT `+finalizedClawPredicateSQL+` FROM claws c WHERE c.id = ?`,
		cutoff, cutoff, clawID).Scan(&finalized)
	if err != nil {
		return false, err
	}
	return finalized == 1, nil
}

// finalizedClawIDs lists every claw the predicate accepts.
func (s *Server) finalizedClawIDs(cutoff time.Time) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT c.id FROM claws c WHERE `+finalizedClawPredicateSQL, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ---------------------------------------------------------------------------
// Feature 2: compaction
// ---------------------------------------------------------------------------

// compactFinalizedCheckpoints drops the manifests of superseded checkpoints
// belonging to finalized claws, keeping the newest ready checkpoint of each so
// the claw stays restorable.
//
// The row itself is never deleted. Its telemetry columns (message_count,
// pipeline_stage, hub_version, files_count, files_bytes) ARE the analytics
// record of what the claw was doing; that record is exactly what the manifest
// used to hold and what pruning manifests used to destroy. Compaction moves a
// checkpoint from "restorable" to "counted", not to "forgotten".
//
// Blobs are untouched here. They are content-addressed and shared across every
// checkpoint (and every claw) that captured the same file, so a per-claw pass
// cannot tell whether dropping one is safe — only the mark-and-sweep at the end
// of the cycle can.
func (s *Server) compactFinalizedCheckpoints(cutoff time.Time) (int, error) {
	ids, err := s.finalizedClawIDs(cutoff)
	if err != nil {
		return 0, fmt.Errorf("list finalized claws: %w", err)
	}
	compacted := 0
	for _, clawID := range ids {
		n, err := s.compactClawCheckpoints(clawID)
		if err != nil {
			log.Printf("[retention] compact claw %s: %v", shortID(clawID), err)
			continue
		}
		compacted += n
	}
	return compacted, nil
}

func (s *Server) compactClawCheckpoints(clawID string) (int, error) {
	// 'skipped' and 'failed' checkpoints never wrote a manifest, so they are
	// not candidates and are left exactly as they are.
	rows, err := s.db.Query(
		`SELECT id, COALESCE(manifest_path,'') FROM claw_checkpoints
		  WHERE claw_id = ? AND status = 'ready'
		  ORDER BY created_at DESC, id DESC`, clawID)
	if err != nil {
		return 0, err
	}
	type candidate struct{ id, manifestPath string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.manifestPath); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(candidates) <= 1 {
		return 0, nil
	}
	compacted := 0
	// candidates[0] is the newest ready checkpoint: the one restore would pick.
	for _, c := range candidates[1:] {
		path := c.manifestPath
		if path == "" {
			path = checkpointManifestPath(c.id)
		}
		// A manifest that is already gone is not an error — a previous cycle may
		// have been interrupted between the unlink and the UPDATE — but the row
		// still has to be marked so the sweep stops counting it as a reference.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[retention] remove manifest %s: %v", path, err)
			continue
		}
		if _, err := s.db.Exec(
			`UPDATE claw_checkpoints SET status='compacted', manifest_path='', manifest_sha256='' WHERE id=?`,
			c.id); err != nil {
			return compacted, err
		}
		compacted++
	}
	return compacted, nil
}

// ---------------------------------------------------------------------------
// Feature 3: retention window
// ---------------------------------------------------------------------------

// applyRetentionWindow deletes everything older than the window across the four
// targets. Each target is independent: a failure in one is logged and the rest
// still run, because a single broken table must not stop the hub from
// reclaiming the disk space that is actually filling up.
func (s *Server) applyRetentionWindow(cutoff time.Time) {
	if n, err := pruneDiagnosticsLogs(hubDataDir(), cutoff); err != nil {
		log.Printf("[retention] diagnostics: %v", err)
	} else if n > 0 {
		log.Printf("[retention] removed %d expired diagnostics logs", n)
	}
	if n, err := s.pruneExpiredCheckpoints(cutoff); err != nil {
		log.Printf("[retention] checkpoints: %v", err)
	} else if n > 0 {
		log.Printf("[retention] removed %d expired checkpoints", n)
	}
	// task_run_events is keyed on when the event happened, not when the hub
	// happened to record it: a late-arriving webhook for an old run belongs to
	// the old run's window.
	if n, err := s.pruneRows(`DELETE FROM task_run_events WHERE event_time < ?`, cutoff.UnixMilli()); err != nil {
		log.Printf("[retention] task run events: %v", err)
	} else if n > 0 {
		log.Printf("[retention] removed %d expired task run events", n)
	}
	if n, err := s.pruneRows(`DELETE FROM messages WHERE created_at < ?`, cutoff); err != nil {
		log.Printf("[retention] messages: %v", err)
	} else if n > 0 {
		log.Printf("[retention] removed %d expired messages", n)
	}
}

func (s *Server) pruneRows(query string, args ...any) (int64, error) {
	res, err := s.db.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// pruneDiagnosticsLogs removes captured gateway/bridge logs by file mtime.
// These files have no database row, so the filesystem timestamp is the only
// record of their age.
func pruneDiagnosticsLogs(dataDir string, cutoff time.Time) (int, error) {
	if dataDir == "" {
		return 0, nil
	}
	dir := filepath.Join(dataDir, "diagnostics")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			log.Printf("[retention] remove diagnostics log %s: %v", entry.Name(), err)
			continue
		}
		removed++
	}
	return removed, nil
}

// pruneExpiredCheckpoints deletes both the manifest and the row for
// checkpoints past the retention window. Unlike compaction, this is the point
// where the analytics record itself expires, so it applies to every status.
func (s *Server) pruneExpiredCheckpoints(cutoff time.Time) (int, error) {
	rows, err := s.db.Query(
		`SELECT id, COALESCE(manifest_path,'') FROM claw_checkpoints WHERE created_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	type expired struct{ id, manifestPath string }
	var victims []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.manifestPath); err != nil {
			rows.Close()
			return 0, err
		}
		victims = append(victims, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	removed := 0
	for _, v := range victims {
		path := v.manifestPath
		if path == "" {
			path = checkpointManifestPath(v.id)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[retention] remove expired manifest %s: %v", path, err)
			continue
		}
		if _, err := s.db.Exec(`DELETE FROM claw_checkpoints WHERE id=?`, v.id); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// ---------------------------------------------------------------------------
// Blob sweep
// ---------------------------------------------------------------------------

// checkpointBlobKeepSet builds the set of blob digests that must survive.
//
// This is the dangerous part of the whole feature: a digest missing from the
// keep set is a file deleted out from under a restore that may not be attempted
// for weeks, and the loss is silent until then. So the set is built by union of
// every path that can reach a blob, and any doubt aborts the sweep instead of
// narrowing the set:
//
//  1. The tree/message/manifest digests recorded on every surviving
//     claw_checkpoints row. The DB is authoritative even for a row whose
//     manifest was compacted away.
//  2. Every 64-hex digest appearing anywhere in every surviving manifest file.
//     Scanning the decoded JSON generically rather than reading named fields
//     covers schema 1 (which inlined files[]) and schema 2 alike, and keeps
//     working if a future field adds another blob reference.
//  3. The files listed inside each kept workspace tree blob. This is the step
//     whose omission would be catastrophic: a tree is a JSON array of
//     {path, sha256, size}, and the per-file blobs it points at appear nowhere
//     in a schema-2 manifest or in the database. Without this closure the sweep
//     would delete the entire workspace of every checkpoint it just kept.
//
// A manifest that cannot be read or parsed, or a tree blob that is present but
// unparseable, returns an error: an incomplete keep set is worse than a full
// disk, and the operator can fix the file and let the next cycle run.
func (s *Server) checkpointBlobKeepSet() (map[string]struct{}, error) {
	keep := make(map[string]struct{})
	trees := make(map[string]struct{})
	add := func(sha string, isTree bool) {
		clean := normalizeBlobDigest(sha)
		if clean == "" {
			return
		}
		keep[clean] = struct{}{}
		if isTree {
			trees[clean] = struct{}{}
		}
	}

	rows, err := s.db.Query(`SELECT COALESCE(manifest_sha256,''), COALESCE(root_tree_sha256,''),
		COALESCE(message_tree_sha256,''), COALESCE(workspace_tree_sha256,'') FROM claw_checkpoints`)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint digests: %w", err)
	}
	for rows.Next() {
		var manifestSHA, rootSHA, msgSHA, workspaceSHA string
		if err := rows.Scan(&manifestSHA, &rootSHA, &msgSHA, &workspaceSHA); err != nil {
			rows.Close()
			return nil, err
		}
		// manifest_sha256 is the digest of the manifest file, which is not
		// stored as a blob — keeping it costs nothing and guards against a
		// future change that does store it.
		add(manifestSHA, false)
		add(msgSHA, false)
		add(rootSHA, true)
		add(workspaceSHA, true)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	manifestDir := filepath.Join(checkpointsRoot(), "manifests")
	entries, err := os.ReadDir(manifestDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read manifests: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(manifestDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read manifest %s: %w", entry.Name(), err)
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return nil, fmt.Errorf("parse manifest %s: %w", entry.Name(), err)
		}
		for _, sha := range collectDigests(decoded) {
			add(sha, false)
		}
		// The workspace tree is the one manifest digest whose contents must be
		// followed, so pick it out by name in addition to the generic scan.
		var manifest checkpointManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return nil, fmt.Errorf("parse manifest %s: %w", entry.Name(), err)
		}
		add(manifest.Workspace.TreeSHA256, true)
	}

	for tree := range trees {
		path := checkpointBlobPath(tree)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			// The tree blob is already gone; nothing it referenced can be
			// resolved any more, and there is no keep set to lose.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read tree blob %s: %w", tree, err)
		}
		var files []types.CheckpointFile
		if err := json.Unmarshal(data, &files); err != nil {
			return nil, fmt.Errorf("parse tree blob %s: %w", tree, err)
		}
		for _, f := range files {
			add(f.SHA256, false)
		}
	}
	return keep, nil
}

// collectDigests walks decoded JSON and returns every string that looks like a
// sha256 digest, with or without the "sha256:" prefix.
func collectDigests(node any) []string {
	var out []string
	switch v := node.(type) {
	case map[string]any:
		for _, child := range v {
			out = append(out, collectDigests(child)...)
		}
	case []any:
		for _, child := range v {
			out = append(out, collectDigests(child)...)
		}
	case string:
		if normalizeBlobDigest(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// normalizeBlobDigest returns the bare hex digest, or "" when the value is not
// one. checkpointBlobPath strips the same prefix when laying blobs out.
func normalizeBlobDigest(sha string) string {
	clean := strings.TrimPrefix(strings.TrimSpace(sha), "sha256:")
	if len(clean) != 64 {
		return ""
	}
	for _, r := range clean {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return clean
}

// sweepCheckpointBlobs deletes content-addressed blobs no surviving checkpoint
// can reach, then prunes the fan-out directories it emptied. It runs once at
// the end of a cycle, never per claw: a blob is shared by every checkpoint that
// captured the same bytes, so reachability is only meaningful globally.
func (s *Server) sweepCheckpointBlobs() (int, int64, error) {
	keep, err := s.checkpointBlobKeepSet()
	if err != nil {
		return 0, 0, fmt.Errorf("keep set incomplete, not sweeping: %w", err)
	}
	blobRoot := filepath.Join(checkpointsRoot(), "blobs", "sha256")
	if _, err := os.Stat(blobRoot); os.IsNotExist(err) {
		return 0, 0, nil
	}
	removed, freed := 0, int64(0)
	err = filepath.Walk(blobRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if _, ok := keep[normalizeBlobDigest(info.Name())]; ok {
			return nil
		}
		size := info.Size()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[retention] remove blob %s: %v", path, err)
			return nil
		}
		removed++
		freed += size
		return nil
	})
	if err != nil {
		return removed, freed, fmt.Errorf("walk blobs: %w", err)
	}
	pruneEmptyDirs(blobRoot)
	return removed, freed, nil
}

// pruneEmptyDirs removes the two-level fan-out directories the sweep emptied,
// depth first. The root itself is kept so the next checkpoint does not have to
// recreate it.
func pruneEmptyDirs(root string) {
	var walk func(dir string) bool
	walk = func(dir string) bool {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return false
		}
		empty := true
		for _, entry := range entries {
			if entry.IsDir() {
				if !walk(filepath.Join(dir, entry.Name())) {
					empty = false
				}
				continue
			}
			empty = false
		}
		if empty && dir != root {
			return os.Remove(dir) == nil
		}
		return empty
	}
	walk(root)
}
