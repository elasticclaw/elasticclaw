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

	// Minimums. These are not tuning knobs -- they are the line below which a
	// configured value stops describing a retention policy and starts
	// describing an accident.
	minRetentionInterval     = 10 * time.Minute
	minRetentionMaxAge       = 7 * 24 * time.Hour
	minRetentionCompactAfter = 24 * time.Hour
)

type retentionSettings struct {
	interval     time.Duration
	maxAge       time.Duration
	compactAfter time.Duration
	dryRun       bool
}

// retentionDeleteBatch bounds how many rows one retention DELETE may remove.
//
// SQLite has a single writer and this hub opens it with _txlock=immediate and a
// five-second busy_timeout, so an unbounded DELETE over a large table holds the
// write lock for the whole scan and every concurrent writer -- heartbeats,
// usage accounting, checkpoint rows -- waits out the timeout and fails BUSY.
// A batch is a lock hold an operator does not notice.
const retentionDeleteBatch = 1000

// retentionBatchPause is the gap between delete batches, during which the write
// lock is free. It is a variable so tests do not pay for it.
var retentionBatchPause = 2 * time.Millisecond

// retentionCounts is what one cycle actually did, per phase.
type retentionCounts struct {
	compacted     int
	diagnostics   int
	checkpoints   int
	taskRunEvents int64
	messages      int64
	blobs         int
	blobBytes     int64
	errors        int
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
		// Opt-in, not opt-out. Retention deletes irreversibly, and an absent
		// config section means the operator has not yet said anything about it
		// -- most often because they just upgraded. Defaulting to on would have
		// an existing hub start deleting 90-day-old data one interval after a
		// deploy that changed no configuration, and before any archive step had
		// a chance to run. The cost of defaulting off is that disk keeps
		// growing until someone opts in, which is the state the hub was already in.
		return false
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
	cfg.dryRun = r.DryRun
	cfg.interval = parse(r.Interval, cfg.interval, "interval")
	cfg.maxAge = parse(r.MaxAge, cfg.maxAge, "max_age")
	cfg.compactAfter = parse(r.CompactAfter, cfg.compactAfter, "compact_after")

	// Floors, mirroring what livenessSettings does for its own knobs. Every
	// value here multiplies into irreversible deletion, so a typo must not be
	// silently obeyed: max_age: 1s is a valid duration and would make the whole
	// history eligible on the next tick, and a one-second interval would walk
	// the entire blob tree continuously on a host whose disk was already the
	// problem.
	cfg.interval = floor(cfg.interval, minRetentionInterval, "interval")
	cfg.maxAge = floor(cfg.maxAge, minRetentionMaxAge, "max_age")
	cfg.compactAfter = floor(cfg.compactAfter, minRetentionCompactAfter, "compact_after")

	// Compacting must not outlive expiry: if compact_after were the larger of
	// the two, a checkpoint would be deleted outright before it was ever
	// eligible for the cheaper, reversible-in-spirit compaction step.
	if cfg.compactAfter >= cfg.maxAge {
		// Derive the replacement from max_age rather than reaching for the
		// default, which is not guaranteed to satisfy the very rule being
		// enforced: a max_age of 8d sits above the 7d floor yet below the 10d
		// default, so falling back to the default would leave compact_after
		// still >= max_age and the ordering still broken.
		//
		// Half of max_age always works. max_age has already been clamped to at
		// least minRetentionMaxAge (7d), so half of it is at least 3.5d, which
		// clears minRetentionCompactAfter (24h), and it is strictly below
		// max_age by construction.
		replacement := cfg.maxAge / 2
		log.Printf("[retention] compact_after %s is not below max_age %s; using %s",
			cfg.compactAfter, cfg.maxAge, replacement)
		cfg.compactAfter = replacement
	}
	return cfg
}

// floor clamps a configured duration up to a safe minimum, logging when it
// does. Unlike parse, which rejects nonsense, this rejects the merely
// dangerous: a value that parses fine and would still destroy history faster
// than an operator could notice.
func floor(value, min time.Duration, name string) time.Duration {
	if value < min {
		log.Printf("[retention] %s %s is below the minimum %s; using %s", name, value, min, min)
		return min
	}
	return value
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
//
// Every cycle logs a start line and an end line, including the cycles that
// reclaim nothing. A sweeper that has been silent for a day is otherwise
// indistinguishable from one that is wedged, disabled, or crashing in a phase
// that only logs on success, and telling those apart after the fact was not
// possible at all.
func (s *Server) retentionSweepOnce() {
	if !s.retentionEnabled() {
		return
	}
	cfg, n := s.retentionSettings(), s.retentionNow()
	expiryCutoff, compactCutoff := n.Add(-cfg.maxAge), n.Add(-cfg.compactAfter)
	started := time.Now()
	log.Printf("[retention] cycle start: dry_run=%v expire_before=%s compact_before=%s",
		cfg.dryRun, expiryCutoff.Format(time.RFC3339), compactCutoff.Format(time.RFC3339))

	var counts retentionCounts
	compacted, err := s.compactFinalizedCheckpoints(compactCutoff, cfg.dryRun)
	counts.compacted = compacted
	if err != nil {
		counts.errors++
		log.Printf("[retention] compaction: %v", err)
	}

	windowCounts := s.applyRetentionWindow(expiryCutoff, cfg.dryRun)
	counts.diagnostics = windowCounts.diagnostics
	counts.checkpoints = windowCounts.checkpoints
	counts.taskRunEvents = windowCounts.taskRunEvents
	counts.messages = windowCounts.messages
	counts.errors += windowCounts.errors

	swept, bytes, err := s.sweepCheckpointBlobs(cfg.dryRun)
	counts.blobs, counts.blobBytes = swept, bytes
	if err != nil {
		counts.errors++
		log.Printf("[retention] blob sweep: %v", err)
	}

	verb := "removed"
	if cfg.dryRun {
		verb = "would remove"
	}
	log.Printf("[retention] cycle done in %s (dry_run=%v): %s compacted=%d diagnostics=%d checkpoints=%d task_run_events=%d messages=%d blobs=%d bytes_freed=%d errors=%d",
		time.Since(started).Round(time.Millisecond), cfg.dryRun, verb,
		counts.compacted, counts.diagnostics, counts.checkpoints,
		counts.taskRunEvents, counts.messages, counts.blobs, counts.blobBytes, counts.errors)
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
// blobSweepGrace is how recently a blob may have been written and still be
// spared by the sweep. It must exceed the longest plausible gap between a blob
// upload and the manifest that references it being written -- that is one
// checkpoint's upload phase, not one checkpoint interval.
const blobSweepGrace = time.Hour

const finalizedClawPredicateSQL = `(
	(
		-- A merged PR only finalizes a claw that is no longer running. The PR
		-- watcher deliberately keeps a claw alive while it still has other open
		-- PRs, so "merged something" and "finished" are not the same claim.
		-- Compacting a live claw is not merely premature: retryCheckpointID
		-- walks BACK through older ready checkpoints when the newest is a
		-- bootstrap capture of state zero, so collapsing to one checkpoint can
		-- remove every usable recovery point and make a retry restart from
		-- nothing.
		c.status NOT IN ('connected', 'starting', 'provisioning')
		AND NOT EXISTS (
			-- The hub's canonical unresolved-PR test, copied from clawOpenPRCount
			-- (pr_watcher.go) rather than restated. The earlier wording here,
			-- merged = 0 AND state = 'open', was wrong in both directions: it
			-- missed a delivered PR sitting in any state that is not literally
			-- 'open', and it counted mention_only rows, which are PR URLs the
			-- message scanner noticed and which gate nothing anywhere else --
			-- one of those could block compaction for a finished claw forever.
			SELECT 1 FROM claw_prs op
			 WHERE op.claw_id = c.id
			   AND op.state NOT IN ('merged', 'closed')
			   AND op.mention_only = 0
		)
		AND (
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
		)
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
func (s *Server) compactFinalizedCheckpoints(cutoff time.Time, dryRun bool) (int, error) {
	ids, err := s.finalizedClawIDs(cutoff)
	if err != nil {
		return 0, fmt.Errorf("list finalized claws: %w", err)
	}
	compacted := 0
	for i, clawID := range ids {
		n, err := s.compactClawCheckpoints(clawID, dryRun)
		compacted += n
		if err != nil {
			// Say how far the phase got. "compaction failed" alone cannot be
			// told apart from "compaction did nothing", and the two call for
			// opposite responses.
			log.Printf("[retention] compact claw %s: %v (aborted after %d of %d claws)",
				shortID(clawID), err, i, len(ids))
			continue
		}
	}
	return compacted, nil
}

func (s *Server) compactClawCheckpoints(clawID string, dryRun bool) (int, error) {
	// 'skipped' and 'failed' checkpoints never wrote a manifest, so they are
	// not candidates and are left exactly as they are.
	rows, err := s.db.Query(
		`SELECT id, COALESCE(manifest_path,''), COALESCE(root_tree_sha256,'') FROM claw_checkpoints
		  WHERE claw_id = ? AND status = 'ready'
		  ORDER BY created_at DESC, id DESC`, clawID)
	if err != nil {
		return 0, err
	}
	type candidate struct{ id, manifestPath, rootTree string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.manifestPath, &c.rootTree); err != nil {
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
	roots := make([]string, len(candidates))
	for i, c := range candidates {
		roots[i] = c.rootTree
	}
	keep := newestRestorableCheckpoint(roots)
	compacted := 0
	for i, c := range candidates {
		if i == keep {
			continue
		}
		path := c.manifestPath
		if path == "" {
			path = checkpointManifestPath(c.id)
		}
		if dryRun {
			log.Printf("[retention] dry_run: would compact checkpoint %s of claw %s (manifest %s)",
				shortID(c.id), shortID(clawID), path)
			compacted++
			continue
		}
		// A manifest that is already gone is not an error — a previous cycle may
		// have been interrupted between the unlink and the UPDATE — but the row
		// still has to be marked so the sweep stops counting it as a reference.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[retention] remove manifest %s: %v", path, err)
			continue
		}
		// The tree digests go with the manifest.
		//
		// Clearing only manifest_path/manifest_sha256 reclaimed the manifest and
		// nothing else: the keep set reads digests off every row whatever its
		// status, so the root/workspace/message trees of a compacted checkpoint
		// stayed reachable and so did every file blob they list. Under schema 2 a
		// manifest is a few hundred bytes and the trees are the whole workspace,
		// which means compaction was reclaiming the small half and calling it
		// done. The telemetry columns are deliberately untouched — they are the
		// analytics record, and losing them is what this design exists to avoid.
		if _, err := s.db.Exec(
			`UPDATE claw_checkpoints
			    SET status='compacted', manifest_path='', manifest_sha256='',
			        root_tree_sha256='', workspace_tree_sha256='', message_tree_sha256=''
			  WHERE id=?`,
			c.id); err != nil {
			return compacted, err
		}
		compacted++
	}
	return compacted, nil
}

// newestRestorableCheckpoint picks the index of the checkpoint compaction must
// keep, given each candidate's root tree digest in newest-first order.
//
// Recency alone is not enough. completeMetadataOnlyCheckpoint publishes a
// 'ready' row with an EMPTY root_tree_sha256 whenever the bridge is
// unreachable, and on the common death paths that row is by construction the
// newest one: handleClawKill removes the claw from s.claws before calling
// checkpointBeforeTermination, so the kill capture can only ever be
// metadata-only. Keeping it by timestamp meant compacting away every checkpoint
// that actually had files while retaining one retryCheckpointIDWithCount
// explicitly refuses (it skips rootTree == "") — the claw kept a checkpoint and
// lost the ability to restore.
//
// A claw whose ready checkpoints are ALL metadata-only still keeps its newest:
// there is nothing better to keep, and the row is still the analytics record.
func newestRestorableCheckpoint(rootTrees []string) int {
	for i, root := range rootTrees {
		if root != "" {
			return i
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Feature 3: retention window
// ---------------------------------------------------------------------------

// applyRetentionWindow deletes everything older than the window across the four
// targets. Each target is independent: a failure in one is logged and the rest
// still run, because a single broken table must not stop the hub from
// reclaiming the disk space that is actually filling up.
func (s *Server) applyRetentionWindow(cutoff time.Time, dryRun bool) retentionCounts {
	var counts retentionCounts
	if n, err := pruneDiagnosticsLogs(hubDataDir(), cutoff, dryRun); err != nil {
		counts.errors++
		log.Printf("[retention] diagnostics: %v", err)
	} else {
		counts.diagnostics = n
	}
	if n, err := s.pruneExpiredCheckpoints(cutoff, dryRun); err != nil {
		counts.errors++
		counts.checkpoints = n
		log.Printf("[retention] checkpoints: %v", err)
	} else {
		counts.checkpoints = n
	}
	// task_run_events is keyed on when the event happened, not when the hub
	// happened to record it: a late-arriving webhook for an old run belongs to
	// the old run's window.
	if n, err := s.pruneRowsBatched("task_run_events", "event_time", cutoff.UnixMilli(), dryRun); err != nil {
		counts.errors++
		counts.taskRunEvents = n
		log.Printf("[retention] task run events: %v", err)
	} else {
		counts.taskRunEvents = n
	}
	if n, err := s.pruneRowsBatched("messages", "created_at", cutoff, dryRun); err != nil {
		counts.errors++
		counts.messages = n
		log.Printf("[retention] messages: %v", err)
	} else {
		counts.messages = n
	}
	return counts
}

// pruneRowsBatched deletes rows older than cutoff in bounded batches, releasing
// the write lock between them.
//
// One unbounded DELETE over messages or task_run_events is a full scan under
// the only write lock the database has, and this hub sets busy_timeout to five
// seconds: every heartbeat, usage update and checkpoint row written during that
// scan waits out the timeout and then fails BUSY. The sweeper exists to relieve
// a full disk, not to take the hub down while it does.
//
// table and column are compile-time constants from the caller, never operator
// or request input, so interpolating them into the statement is safe; they
// cannot be bound as parameters.
func (s *Server) pruneRowsBatched(table, column string, cutoff any, dryRun bool) (int64, error) {
	if dryRun {
		var n int64
		err := s.db.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM %q WHERE %q < ?`, table, column), cutoff).Scan(&n)
		if err != nil {
			return 0, err
		}
		if n > 0 {
			log.Printf("[retention] dry_run: would delete %d rows from %s", n, table)
		}
		return n, nil
	}
	query := fmt.Sprintf(
		`DELETE FROM %q WHERE rowid IN (SELECT rowid FROM %q WHERE %q < ? LIMIT ?)`,
		table, table, column)
	var total int64
	for {
		res, err := s.db.Exec(query, cutoff, retentionDeleteBatch)
		if err != nil {
			return total, fmt.Errorf("delete from %s (aborted after %d rows): %w", table, total, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			// The driver could not report a count, so the loop has no
			// termination condition left. Stop rather than risk spinning.
			return total, nil
		}
		total += n
		if n < retentionDeleteBatch {
			return total, nil
		}
		time.Sleep(retentionBatchPause)
	}
}

// pruneDiagnosticsLogs removes captured gateway/bridge logs by file mtime.
// These files have no database row, so the filesystem timestamp is the only
// record of their age.
func pruneDiagnosticsLogs(dataDir string, cutoff time.Time, dryRun bool) (int, error) {
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
		if dryRun {
			log.Printf("[retention] dry_run: would remove diagnostics log %s", entry.Name())
			removed++
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
func (s *Server) pruneExpiredCheckpoints(cutoff time.Time, dryRun bool) (int, error) {
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
		if dryRun {
			log.Printf("[retention] dry_run: would delete expired checkpoint %s (manifest %s)", shortID(v.id), path)
			removed++
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[retention] remove expired manifest %s: %v", path, err)
			continue
		}
		if _, err := s.db.Exec(`DELETE FROM claw_checkpoints WHERE id=?`, v.id); err != nil {
			return removed, fmt.Errorf("delete expired checkpoint (aborted after %d of %d): %w", removed, len(victims), err)
		}
		// A checkpoint that expired while still 'creating' takes its blob claim
		// with it; nothing else would ever release it once the row is gone.
		s.clearCheckpointPendingBlobs(v.id)
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
//  3. Every digest a checkpoint still in 'creating' declared at plan time. The
//     row itself carries no digests until it completes, so this durable claim
//     is the only thing that distinguishes a blob about to be referenced from
//     an orphan. It is not covered by the mtime grace window: the plan and the
//     upload handler both deduplicate on os.Stat, so a reused blob keeps its
//     original mtime, which can be arbitrarily old.
//  4. The files listed inside each kept workspace tree blob. This is the step
//     whose omission would be catastrophic: a tree is a JSON array of
//     {path, sha256, size}, and the per-file blobs it points at appear nowhere
//     in a schema-2 manifest or in the database. Without this closure the sweep
//     would delete the entire workspace of every checkpoint it just kept.
//
// A manifest that cannot be read or parsed, or a tree blob that is present but
// unparseable, returns an error: an incomplete keep set is worse than a full
// disk, and the operator can fix the file and let the next cycle run.
//
// The one exception is an unparseable manifest that NO claw_checkpoints row
// references. That file is already unreachable — nothing can restore from it —
// so it cannot make the keep set incomplete, and treating it as fatal was
// actively harmful: manifests used to be written with a bare os.WriteFile, so
// the disk-full condition this feature exists to relieve is exactly what leaves
// a truncated one behind, and one such file disabled the sweep permanently with
// no way to self-heal. It is logged and skipped so retention can remove it.
func (s *Server) checkpointBlobKeepSet() (map[string]struct{}, error) {
	keep := make(map[string]struct{})
	trees := make(map[string]struct{})
	// Manifests a row still points at. A manifest outside this set is an orphan
	// whatever its contents, and the id/path pair is collected because a legacy
	// row may store a path that is not checkpointManifestPath(id).
	referencedManifests := make(map[string]struct{})
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

	rows, err := s.db.Query(`SELECT id, status, COALESCE(manifest_path,''), COALESCE(manifest_sha256,''), COALESCE(root_tree_sha256,''),
		COALESCE(message_tree_sha256,''), COALESCE(workspace_tree_sha256,'') FROM claw_checkpoints`)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint digests: %w", err)
	}
	for rows.Next() {
		var id, status, manifestPath, manifestSHA, rootSHA, msgSHA, workspaceSHA string
		if err := rows.Scan(&id, &status, &manifestPath, &manifestSHA, &rootSHA, &msgSHA, &workspaceSHA); err != nil {
			rows.Close()
			return nil, err
		}
		if manifestPath != "" {
			referencedManifests[manifestPath] = struct{}{}
		} else if status != "compacted" {
			// A compacted row gave up its manifest deliberately, so a file left
			// at its conventional path is debris from an interrupted unlink, not
			// a reference. Every other status may legitimately own one even with
			// the column unset (legacy rows, an interrupted finalize).
			referencedManifests[checkpointManifestPath(id)] = struct{}{}
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

	// Checkpoints under construction. Their blobs are on disk (or already were,
	// deduplicated from an earlier checkpoint) but no row names them yet.
	pending, err := s.db.Query(`SELECT p.sha256 FROM claw_checkpoint_pending_blobs p
		 JOIN claw_checkpoints c ON c.id = p.checkpoint_id
		 WHERE c.status = 'creating'`)
	if err != nil {
		return nil, fmt.Errorf("read pending checkpoint blobs: %w", err)
	}
	for pending.Next() {
		var sha string
		if err := pending.Scan(&sha); err != nil {
			pending.Close()
			return nil, err
		}
		add(sha, false)
	}
	pending.Close()
	if err := pending.Err(); err != nil {
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
		_, referenced := referencedManifests[path]
		// An unusable manifest only threatens the keep set when a row still
		// points at it. Orphans are skipped so one truncated file cannot switch
		// reclamation off for the length of the retention window.
		unusable := func(err error) error {
			if referenced {
				return err
			}
			log.Printf("[retention] ignoring orphan manifest %s (no checkpoint row references it): %v", entry.Name(), err)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if skipErr := unusable(fmt.Errorf("read manifest %s: %w", entry.Name(), err)); skipErr != nil {
				return nil, skipErr
			}
			continue
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			if skipErr := unusable(fmt.Errorf("parse manifest %s: %w", entry.Name(), err)); skipErr != nil {
				return nil, skipErr
			}
			continue
		}
		for _, sha := range collectDigests(decoded) {
			add(sha, false)
		}
		// The workspace tree is the one manifest digest whose contents must be
		// followed, so pick it out by name in addition to the generic scan.
		var manifest checkpointManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			if skipErr := unusable(fmt.Errorf("parse manifest %s: %w", entry.Name(), err)); skipErr != nil {
				return nil, skipErr
			}
			continue
		}
		add(manifest.Workspace.TreeSHA256, true)
	}

	for tree := range trees {
		path := checkpointBlobPath(tree)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			// A missing tree makes the keep set incomplete, exactly as an
			// unparseable one does, so it aborts for the same reason.
			//
			// The previous reasoning -- "the tree is gone, so nothing it
			// referenced is resolvable" -- confused the checkpoint being broken
			// with its content being worthless. Under schema 2 the per-file
			// digests exist ONLY inside that tree, so continuing here means the
			// sweep cannot see those files, decides they are unreachable, and
			// deletes them. One missing object would become the irreversible
			// loss of everything the checkpoint still had, and would also
			// destroy any chance of reconstructing the tree from its parts.
			return nil, fmt.Errorf("tree blob %s referenced but missing", tree)
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
func (s *Server) sweepCheckpointBlobs(dryRun bool) (int, int64, error) {
	keep, err := s.checkpointBlobKeepSet()
	if err != nil {
		return 0, 0, fmt.Errorf("keep set incomplete, not sweeping: %w", err)
	}
	blobRoot := filepath.Join(checkpointsRoot(), "blobs", "sha256")
	if _, err := os.Stat(blobRoot); os.IsNotExist(err) {
		return 0, 0, nil
	}
	// Everything written inside this window is off limits. This is now a SECOND
	// line of defence, not the primary one: the durable pending-blob claim
	// recorded at plan time is what actually protects a checkpoint under
	// construction, because it covers reused blobs whose mtime is old. The
	// window still earns its place for anything the claim cannot cover — a blob
	// written by a path that never planned it, or one whose claim was released
	// by the row completing a moment before the walk reached the file.
	//
	// A grace window rather than a lock: the alternative is holding a mutex
	// across every blob upload and the whole sweep, which would stall uploads
	// for the length of a full directory walk.
	cutoff := time.Now().Add(-blobSweepGrace)
	removed, freed := 0, int64(0)
	err = filepath.Walk(blobRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.Contains(info.Name(), ".tmp-") {
			// An upload in progress. It has no digest name, so it would
			// otherwise look like an orphan and be deleted out from under the
			// os.Rename that is about to publish it.
			return nil
		}
		if _, ok := keep[normalizeBlobDigest(info.Name())]; ok {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		size := info.Size()
		if dryRun {
			log.Printf("[retention] dry_run: would sweep blob %s (%d bytes)", info.Name(), size)
			removed++
			freed += size
			return nil
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[retention] remove blob %s: %v", path, err)
			return nil
		}
		removed++
		freed += size
		return nil
	})
	if err != nil {
		return removed, freed, fmt.Errorf("walk blobs (aborted after %d blobs, %d bytes): %w", removed, freed, err)
	}
	if !dryRun {
		pruneEmptyDirs(blobRoot)
	}
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
