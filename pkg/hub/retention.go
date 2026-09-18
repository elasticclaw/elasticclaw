package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
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
// the migrations — starts failing. A cycle reclaims in four phases:
//
//	reference backfill -> compaction -> retention -> blob sweep
//
// The order matters. Compaction and retention are what release manifests,
// checkpoint rows, and the blob references those rows held; the blob sweep asks
// which blobs are left with no reference at all, so running it first would keep
// blobs whose last reference is about to disappear and leave them for the next
// cycle. The backfill comes first because the two release phases delete
// references, and it must not re-derive references those phases have just
// dropped.
//
// Blob reachability is REFERENCE COUNTED, not derived. checkpoint_blob_refs
// holds one row per (checkpoint, blob); a blob is deletable when no row names
// it. The sweeper does not parse manifests, follow tree blobs, or union digest
// columns — three review loops found the same class of defect in that
// reconstruction, every one of them "some state the derivation did not account
// for". See the table comment in db.go for the edge lifecycle.

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
	// adjustments names every knob the hub overrode: a floor that was applied,
	// or an ordering rule that was enforced. The sweeper's startup line reports
	// it, because a clamped value is the difference between the policy an
	// operator configured and the policy the hub is actually running.
	adjustments []string
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
//
// It must be at least one step of SQLite's busy handler, which backs off in a
// fixed ladder (1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, 100ms). A waiter
// several retries deep is sleeping 25-100ms, so a 2ms gap -- the previous value
// -- meant the deleter re-acquired the write lock before any waiter woke up,
// essentially every time: the lock was released on paper and held ~90% of wall
// time in practice, and concurrent writers still exhausted their 5s busy_timeout
// on a large first sweep. 50ms lands on a ladder step, so a waiter deep in the
// backoff actually gets a turn.
var retentionBatchPause = 50 * time.Millisecond

// retentionRowBudget bounds how long the batched row deletes may run in one
// cycle. A first sweep on a hub that has never run retention has millions of
// rows to remove; doing it in one cycle means hours of interleaved write-lock
// contention with no natural pause. Spreading it across cycles costs days of
// wall clock on the backlog and nothing on the steady state, where a cycle
// removes one interval's worth of rows and never comes near the budget.
var retentionRowBudget = 2 * time.Minute

// retentionCounts is what one cycle actually did, per phase.
type retentionCounts struct {
	compacted     int
	diagnostics   int
	checkpoints   int
	taskRunEvents int64
	messages      int64
	blobs         int
	// Bytes reclaimed on the FILESYSTEM, broken out by what produced them. Only
	// blobBytes used to be counted, which reported 0 for a cycle that removed
	// nothing but manifests and diagnostics logs. Row deletes appear in none of
	// these on purpose: deleting rows returns pages to SQLite's freelist and does
	// not shrink the database file at all without a VACUUM.
	blobBytes        int64
	manifestBytes    int64
	diagnosticsBytes int64
	// errors counts PHASES that failed outright. itemErrors counts individual
	// items a phase could not process -- one unremovable blob, one manifest
	// whose unlink failed -- which every phase logs and steps over, returning
	// nil. Without the second counter a cycle that failed to unlink 5,000 blobs
	// reported errors=0 blobs=0, which is byte-identical to the report of a
	// cycle where nothing was eligible in the first place.
	errors     int
	itemErrors int
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
			// Recorded, not merely logged. A rejected value is exactly as much a
			// difference between the configured policy and the running one as a
			// clamped value is, and it is easier to write by accident: Go's
			// ParseDuration has no day unit, so the obvious `max_age: 30d` is
			// rejected and silently became the 90-day default -- under a startup
			// line that said adjustments=[none].
			cfg.adjustments = append(cfg.adjustments,
				name+" "+strconv.Quote(value)+" is not a valid duration; using "+fallback.String())
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
	var applied bool
	if cfg.interval, applied = floor(cfg.interval, minRetentionInterval, "interval"); applied {
		cfg.adjustments = append(cfg.adjustments, "interval raised to the "+minRetentionInterval.String()+" minimum")
	}
	if cfg.maxAge, applied = floor(cfg.maxAge, minRetentionMaxAge, "max_age"); applied {
		cfg.adjustments = append(cfg.adjustments, "max_age raised to the "+minRetentionMaxAge.String()+" minimum")
	}
	if cfg.compactAfter, applied = floor(cfg.compactAfter, minRetentionCompactAfter, "compact_after"); applied {
		cfg.adjustments = append(cfg.adjustments, "compact_after raised to the "+minRetentionCompactAfter.String()+" minimum")
	}

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
		cfg.adjustments = append(cfg.adjustments,
			"compact_after lowered to "+replacement.String()+" so it stays below max_age")
		cfg.compactAfter = replacement
	}
	return cfg
}

// floor clamps a configured duration up to a safe minimum, logging when it
// does, and reports whether it clamped. Unlike parse, which rejects nonsense,
// this rejects the merely dangerous: a value that parses fine and would still
// destroy history faster than an operator could notice.
func floor(value, min time.Duration, name string) (time.Duration, bool) {
	if value < min {
		log.Printf("[retention] %s %s is below the minimum %s; using %s", name, value, min, min)
		return min, true
	}
	return value, false
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
	s.logRetentionPolicy()
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

// logRetentionPolicy states the effective policy exactly once, at startup.
//
// Nothing else does. A disabled sweeper is silent forever, so a misspelt or
// misplaced `enabled` key -- or a hub deployed before anyone opted in --
// produced no signal at all and looked identical to a sweeper that was running
// and finding nothing, for as long as anyone cared to wait. An
// enabled one only spoke an hour later, after its first cycle, and never said
// which values it was actually using: the floors and the compact_after/max_age
// ordering rule can both silently replace a configured value.
func (s *Server) logRetentionPolicy() {
	cfg := s.retentionSettings()
	if !s.retentionEnabled() {
		log.Printf("[retention] disabled: no reclamation cycle will run (set retention.enabled: true to arm it, ideally with dry_run first)")
		return
	}
	adjusted := "none"
	if len(cfg.adjustments) > 0 {
		adjusted = strings.Join(cfg.adjustments, "; ")
	}
	log.Printf("[retention] enabled: dry_run=%v interval=%s max_age=%s compact_after=%s adjustments=[%s] first_cycle_at=%s",
		cfg.dryRun, cfg.interval, cfg.maxAge, cfg.compactAfter, adjusted,
		s.retentionNow().Add(cfg.interval).Format(time.RFC3339))
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

	// First, and before anything releases a reference. A hub upgrading into
	// reference counting has rows with no edges at all, and until they have some
	// the edge table reads as "almost every blob is garbage". The sweep refuses
	// to delete anything until this has completed once (see
	// checkpointBlobRefsBackfilled), so a failure here costs a postponed sweep
	// and nothing else.
	//
	// It runs in a dry run too. It only ADDS references -- it can never select
	// more for deletion, only less -- and without it a dry run on an
	// un-backfilled hub would report the sweep declining, which tells an operator
	// nothing about the blast radius they are trying to measure.
	if err := s.backfillCheckpointBlobRefs(); err != nil {
		counts.errors++
		log.Printf("[retention] blob reference backfill: %v", err)
	}

	// released names the checkpoints whose references a REAL cycle would have
	// dropped by the time the sweep runs. In a real cycle it stays empty: the
	// phases below have already deleted those rows from checkpoint_blob_refs, so
	// the sweeper reads the truth. In a dry run nothing was deleted, so the
	// sweeper has to be told which checkpoints to read past -- otherwise the dry
	// run reports a fraction of what the real cycle removes, for the single most
	// destructive phase there is.
	//
	// This is the whole of what a dry run simulates now: a list of checkpoint ids
	// the phases themselves reported. It does not reconstruct manifests, trees or
	// digest columns, which is what the previous simulation had to do.
	var released []string

	compaction, err := s.compactFinalizedCheckpoints(compactCutoff, cfg.dryRun)
	counts.compacted = compaction.count
	counts.manifestBytes += compaction.bytes
	counts.itemErrors += compaction.itemErrors
	if err != nil {
		counts.errors++
		log.Printf("[retention] compaction: %v", err)
	}
	if cfg.dryRun {
		released = append(released, compaction.ids...)
	}

	windowCounts, expired := s.applyRetentionWindow(expiryCutoff, cfg.dryRun)
	counts.diagnostics = windowCounts.diagnostics
	counts.checkpoints = windowCounts.checkpoints
	counts.taskRunEvents = windowCounts.taskRunEvents
	counts.messages = windowCounts.messages
	counts.manifestBytes += windowCounts.manifestBytes
	counts.diagnosticsBytes += windowCounts.diagnosticsBytes
	counts.errors += windowCounts.errors
	counts.itemErrors += windowCounts.itemErrors
	if cfg.dryRun {
		released = append(released, expired...)
	}

	swept, bytes, blobItemErrors, err := s.sweepCheckpointBlobs(cfg.dryRun, released)
	counts.blobs, counts.blobBytes = swept, bytes
	counts.itemErrors += blobItemErrors
	if err != nil {
		counts.errors++
		log.Printf("[retention] blob sweep: %v", err)
	}

	verb := "removed"
	if cfg.dryRun {
		verb = "would remove"
	}
	log.Printf("[retention] cycle done in %s (dry_run=%v): %s compacted=%d diagnostics=%d checkpoints=%d task_run_events=%d messages=%d blobs=%d bytes_freed=%d (blobs=%d manifests=%d diagnostics=%d; row deletes free SQLite pages but do not shrink the database file without a VACUUM) phase_errors=%d item_errors=%d",
		time.Since(started).Round(time.Millisecond), cfg.dryRun, verb,
		counts.compacted, counts.diagnostics, counts.checkpoints,
		counts.taskRunEvents, counts.messages, counts.blobs,
		counts.blobBytes+counts.manifestBytes+counts.diagnosticsBytes,
		counts.blobBytes, counts.manifestBytes, counts.diagnosticsBytes,
		counts.errors, counts.itemErrors)

	// Deliberately last. These indexes only make the batched row deletes cheap,
	// and building one needs free disk for the whole B-tree -- so running it at
	// the TOP of the cycle, as this used to, meant a hub with a full disk spent
	// its first act on a doomed full-table index build under the single write
	// lock, before a single byte had been reclaimed. At the end, the cycle it
	// accelerates is the NEXT one, and the space this cycle freed is what it gets
	// to use.
	ensureRetentionIndexes(s.db)
}

// retentionSample collects a bounded number of examples alongside a total.
//
// The dry run used to log one line per blob, per checkpoint and per claw. The
// documented example cycle selects ~43k items, and journald's default rate limit
// is 10,000 messages per 30 seconds -- so the run that exists to be read drowned
// itself, and the summary line that is the entire point was among the messages
// dropped. A total plus a handful of examples is what an operator actually acts
// on.
type retentionSample struct {
	items []string
	total int
}

const retentionSampleMax = 5

func (r *retentionSample) add(v string) {
	r.total++
	if len(r.items) < retentionSampleMax {
		r.items = append(r.items, v)
	}
}

// describe renders "N thing(s) (e.g. a, b, c, and 40 more)", or "" when nothing
// was selected, so the caller can skip the line entirely.
func (r *retentionSample) describe(noun string) string {
	if r.total == 0 {
		return ""
	}
	out := fmt.Sprintf("%d %s (e.g. %s", r.total, noun, strings.Join(r.items, ", "))
	if r.total > len(r.items) {
		out += fmt.Sprintf(", and %d more", r.total-len(r.items))
	}
	return out + ")"
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
	(
		-- A merged PR only finalizes a claw that is no longer running. The PR
		-- watcher deliberately keeps a claw alive while it still has other open
		-- PRs, so "merged something" and "finished" are not the same claim.
		-- Compacting a live claw is not merely premature: retryCheckpointID
		-- walks BACK through older ready checkpoints when the newest is a
		-- bootstrap capture of state zero, so collapsing to one checkpoint can
		-- remove every usable recovery point and make a retry restart from
		-- nothing.
		--
		-- 'offline' and 'idle' belong in this list too, though they read like
		-- rest states. A claw goes 'offline' the moment its WebSocket drops and
		-- routinely self-recovers minutes later without anything else changing
		-- (the reaper only gives up after offline_grace); an 'idle' claw is
		-- resumable by design -- checkAgentIdleResume exists to wake it, and
		-- requestIdleCheckpoints keeps checkpointing it. Compacting either on
		-- the strength of a merged PR collapses the recovery points of a claw
		-- that is about to carry on working. They can still be compacted, but
		-- only through the staleness arm below, which requires that nothing has
		-- actually happened for compact_after.
		c.status NOT IN ('connected', 'starting', 'provisioning', 'offline', 'idle')
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
// Blobs are untouched here in the sense that no file is unlinked. What the
// UPDATE below does do is drop the compacted checkpoint's reference edges, in
// the same transaction — which is how compaction releases blobs by construction
// rather than by clearing digest columns and hoping the sweeper's derivation
// agrees. Whether any file actually goes is still a global question, answered
// once at the end of the cycle: a blob is shared by every checkpoint that
// captured the same bytes.
func (s *Server) compactFinalizedCheckpoints(cutoff time.Time, dryRun bool) (compactionResult, error) {
	var result compactionResult
	result.sample = &retentionSample{}
	ids, err := s.finalizedClawIDs(cutoff)
	if err != nil {
		return result, fmt.Errorf("list finalized claws: %w", err)
	}
	for i, clawID := range ids {
		if err := s.compactClawCheckpoints(clawID, dryRun, &result); err != nil {
			// Say where in the phase the failure landed. The earlier wording
			// claimed the phase "aborted after N of M claws" while the loop in
			// fact continues to the next claw, which is the opposite of what an
			// operator reading it would do next.
			result.itemErrors++
			log.Printf("[retention] compact claw %s (%d of %d): %v (continuing with the remaining claws)",
				shortID(clawID), i+1, len(ids), err)
			continue
		}
	}
	if dryRun {
		if line := result.sample.describe("checkpoint(s)"); line != "" {
			log.Printf("[retention] dry_run: would compact %s", line)
		}
	}
	return result, nil
}

// compactionResult is what the compaction phase did: how many checkpoints it
// compacted (or would have), which ones, how many bytes of manifest that
// reclaimed, and how many individual items it could not process.
type compactionResult struct {
	count      int
	ids        []string
	bytes      int64
	itemErrors int
	sample     *retentionSample
}

func (s *Server) compactClawCheckpoints(clawID string, dryRun bool, result *compactionResult) error {
	// 'failed' checkpoints never wrote a manifest, so they are not candidates
	// and are left exactly as they are. 'skipped' rows are handled separately
	// below: they are not candidates either, but they DO hold references.
	rows, err := s.db.Query(
		`SELECT id, COALESCE(manifest_path,''), COALESCE(root_tree_sha256,''), COALESCE(reason,'') FROM claw_checkpoints
		  WHERE claw_id = ? AND status = 'ready'
		  ORDER BY created_at DESC, id DESC`, clawID)
	if err != nil {
		return err
	}
	var candidates []compactionCandidate
	for rows.Next() {
		var c compactionCandidate
		if err := rows.Scan(&c.id, &c.manifestPath, &c.rootTree, &c.reason); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Before the early return below, not after it. A 'skipped' row holds
	// references to the tree of the ready checkpoint it duplicated, and that
	// ready row may have been compacted by an EARLIER cycle -- in which case this
	// claw is down to one ready candidate, takes the early return, and its
	// skipped rows go on holding a workspace nothing can restore from, forever.
	releasedSkipped, err := s.releaseSkippedCheckpointRefs(clawID, dryRun)
	if err != nil {
		result.itemErrors++
		log.Printf("[retention] release skipped checkpoint references for claw %s: %v", shortID(clawID), err)
	}
	// Reported among the released ids, not in the compacted COUNT: a skipped row
	// letting go of its blobs is not a checkpoint moving from "restorable" to
	// "counted". The dry run still needs to know about it, because the sweep must
	// read past those references too.
	result.ids = append(result.ids, releasedSkipped...)
	if len(candidates) <= 1 {
		return nil
	}
	keep, err := s.survivingCheckpoint(clawID, candidates)
	if err != nil {
		return err
	}
	for i, c := range candidates {
		if i == keep {
			continue
		}
		path := c.manifestPath
		if path == "" {
			path = checkpointManifestPath(c.id)
		}
		var size int64
		if info, err := os.Stat(path); err == nil {
			size = info.Size()
		}
		if dryRun {
			result.sample.add(shortID(c.id))
			result.count++
			result.bytes += size
			result.ids = append(result.ids, c.id)
			continue
		}
		// Mark the row FIRST, unlink second.
		//
		// The other order left a window where the manifest was gone and the row
		// still said 'ready' with a manifest_path pointing at it: if the UPDATE
		// then failed (ENOSPC, SQLITE_BUSY -- the conditions this sweeper runs
		// under), the next cycle could pick that row as the claw's survivor and
		// keep a checkpoint whose manifest does not exist. This order's failure
		// mode is a manifest left on disk under a 'compacted' row, which is
		// merely wasted bytes and which the retry below reclaims.
		//
		// The references go with the status, in one transaction: that is what
		// makes the blobs of a compacted checkpoint collectable. Clearing only
		// manifest_path/manifest_sha256 reclaimed a few hundred bytes of manifest
		// and left the whole workspace behind it reachable.
		//
		// The telemetry columns are deliberately untouched — they are the
		// analytics record, and losing them is what this design exists to avoid.
		if err := s.markCheckpointCompacted(c.id); err != nil {
			return err
		}
		// A manifest that is already gone is not an error: a previous cycle may
		// have been interrupted between the UPDATE and the unlink.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			result.itemErrors++
			log.Printf("[retention] remove manifest %s: %v", path, err)
		} else if err == nil {
			result.bytes += size
		}
		result.count++
		result.ids = append(result.ids, c.id)
	}
	return nil
}

// markCheckpointCompacted moves one checkpoint from "restorable" to "counted"
// and releases every blob it held, atomically.
func (s *Server) markCheckpointCompacted(checkpointID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE claw_checkpoints
		    SET status='compacted', manifest_path='', manifest_sha256='',
		        root_tree_sha256='', workspace_tree_sha256='', message_tree_sha256=''
		  WHERE id=?`, checkpointID); err != nil {
		return err
	}
	if err := deleteCheckpointBlobRefsTx(tx, checkpointID); err != nil {
		return err
	}
	return tx.Commit()
}

// releaseSkippedCheckpointRefs drops the blob references and tree digests of a
// finalized claw's 'skipped' checkpoints.
//
// markCheckpointSkipped records a skipped row holding the tree of the ready
// checkpoint it duplicated, which is honest while the claw is live: two
// checkpoints hold the same blobs and the blobs survive until both let go.
// Compaction is where the second one lets go. Without this, compacting the ready
// row released nothing at all, because a sibling skipped row still held the same
// workspace.
//
// It is safe because a skipped row is never restorable in the first place:
// restoreClawFromCheckpoint and restoreCheckpointFiles both require
// status='ready'. The telemetry columns are untouched, exactly as in compaction
// — the row remains the record that the claw was idle at that moment.
func (s *Server) releaseSkippedCheckpointRefs(clawID string, dryRun bool) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM claw_checkpoints WHERE claw_id=? AND status='skipped'
		   AND (root_tree_sha256 <> '' OR workspace_tree_sha256 <> '' OR message_tree_sha256 <> ''
		        OR EXISTS (SELECT 1 FROM checkpoint_blob_refs r WHERE r.checkpoint_id = claw_checkpoints.id))`,
		clawID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if dryRun || len(ids) == 0 {
		return ids, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return ids, err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(
			`UPDATE claw_checkpoints
			    SET root_tree_sha256='', workspace_tree_sha256='', message_tree_sha256=''
			  WHERE id=?`, id); err != nil {
			return ids, err
		}
		if err := deleteCheckpointBlobRefsTx(tx, id); err != nil {
			return ids, err
		}
	}
	return ids, tx.Commit()
}

type compactionCandidate struct{ id, manifestPath, rootTree, reason string }

// survivingCheckpoint picks the index of the checkpoint compaction must keep.
//
// Restorability alone is not the bar, because compaction's survivor only earns
// its keep if a retry would actually accept it. retryCheckpointIDWithCount
// (claw_retry.go) skips more than the empty-root-tree rows this used to reason
// about: it also skips a 'bootstrap' capture once the claw has progressed past
// state zero, and the checkpoint the previous attempt already restored from.
// Keeping a bootstrap capture and compacting away the claw's only real
// checkpoint left a claw that looked recoverable and, on the next retry, wasn't.
//
// Falling back to the newest restorable when nothing is retry-eligible is
// deliberate: eligibility is evaluated against today's state (a claw that has
// not progressed past bootstrap yet may do so tomorrow), and the row is still
// the analytics record either way. Keeping the wrong one is recoverable;
// keeping none is not.
func (s *Server) survivingCheckpoint(clawID string, candidates []compactionCandidate) (int, error) {
	var tenantID string
	if err := s.db.QueryRow(`SELECT tenant_id FROM claws WHERE id=?`, clawID).Scan(&tenantID); err != nil {
		return 0, err
	}
	restored, err := s.restoredCheckpointIDs(clawID)
	if err != nil {
		return 0, err
	}
	progressed := s.clawProgressedPastBootstrap(tenantID, clawID)
	if i := retryEligibleCheckpoint(candidates, progressed, restored); i >= 0 {
		return i, nil
	}
	return newestRestorableCheckpoint(candidates, restored), nil
}

// retryEligibleCheckpoint returns the index of the newest candidate the retry
// policy would accept, or -1 when none is. The conditions mirror the filter in
// retryCheckpointIDWithCount one for one.
func retryEligibleCheckpoint(candidates []compactionCandidate, progressedPastBootstrap bool, restored map[string]struct{}) int {
	for i, c := range candidates {
		if c.rootTree == "" || c.manifestPath == "" {
			continue
		}
		if c.reason == "bootstrap" && progressedPastBootstrap {
			continue
		}
		if _, ok := restored[c.id]; ok {
			continue
		}
		return i
	}
	return -1
}

// restoredCheckpointIDs names the checkpoint the LATEST recorded attempt
// restored from, or nothing when there is none.
//
// It mirrors retryCheckpointIDWithCount, which looks up exactly one id: the one
// the immediately preceding attempt used. Treating every historically restored
// id as ineligible, as this used to, is not the conservative direction it looks
// like — retry will happily reuse a checkpoint two attempts ago, so excluding
// them can only make compaction discard a checkpoint the next retry would have
// taken, in exchange for keeping one it would skip.
//
// The latest attempt is read by attempt_number rather than by subtracting one
// from a number compaction does not have: compaction runs at an arbitrary
// moment, not at a retry boundary.
func (s *Server) restoredCheckpointIDs(clawID string) (map[string]struct{}, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT COALESCE(restored_checkpoint_id,'') FROM task_run_attempts
		  WHERE run_id=(SELECT task_run_id FROM claws WHERE id=?)
		  ORDER BY attempt_number DESC LIMIT 1`, clawID).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	out := map[string]struct{}{}
	if id != "" {
		out[id] = struct{}{}
	}
	return out, nil
}

// newestRestorableCheckpoint picks the index of the checkpoint compaction must
// keep when nothing is retry-eligible, given the candidates in newest-first
// order.
//
// Recency alone is not enough. completeMetadataOnlyCheckpoint publishes a
// 'ready' row with an EMPTY root_tree_sha256 whenever the bridge is
// unreachable, and on the common death paths that row is by construction the
// newest one: handleClawKill removes the claw from s.claws before calling
// checkpointBeforeTermination, so the kill capture can only ever be
// metadata-only. Keeping it by timestamp meant compacting away every checkpoint
// that actually had files while retaining one retryCheckpointIDWithCount
// explicitly refuses — the claw kept a checkpoint and lost the ability to
// restore.
//
// The fallback honours the SAME predicate as retryEligibleCheckpoint minus the
// bootstrap rule: a root tree, a manifest, and not the id the latest attempt
// already restored from. Honouring only the root tree meant that when the
// eligible set was empty — which is precisely when this runs — the fallback
// could keep a manifest-less row retry filters out with `manifest_path != ”`,
// or the one row retry is guaranteed to skip. The bootstrap rule is the one
// condition deliberately relaxed here: a claw that has progressed past bootstrap
// can still be worth restoring to state zero if there is nothing else at all.
//
// A claw with no candidate satisfying any of it still keeps its newest: there is
// nothing better to keep, and the row is still the analytics record.
func newestRestorableCheckpoint(candidates []compactionCandidate, restored map[string]struct{}) int {
	for i, c := range candidates {
		if c.rootTree == "" || c.manifestPath == "" {
			continue
		}
		if _, ok := restored[c.id]; ok {
			continue
		}
		return i
	}
	// Second pass without the restored-id rule: keeping the checkpoint the last
	// attempt used still beats keeping one that restores nothing.
	for i, c := range candidates {
		if c.rootTree != "" && c.manifestPath != "" {
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
func (s *Server) applyRetentionWindow(cutoff time.Time, dryRun bool) (retentionCounts, []string) {
	var counts retentionCounts
	diagnostics, err := pruneDiagnosticsLogs(hubDataDir(), cutoff, dryRun)
	counts.diagnostics = diagnostics.removed
	counts.diagnosticsBytes = diagnostics.bytes
	counts.itemErrors += diagnostics.itemErrors
	if err != nil {
		counts.errors++
		log.Printf("[retention] diagnostics: %v", err)
	}
	expired, manifestBytes, itemErrors, err := s.pruneExpiredCheckpoints(cutoff, dryRun)
	counts.checkpoints = len(expired)
	counts.manifestBytes = manifestBytes
	counts.itemErrors += itemErrors
	if err != nil {
		counts.errors++
		log.Printf("[retention] checkpoints: %v", err)
	}
	// One budget for both batched tables, not one each: they contend for the
	// same single write lock, so the thing worth bounding is the total time
	// this cycle spends holding it.
	deadline := time.Now().Add(retentionRowBudget)
	// task_run_events is keyed on when the event happened, not when the hub
	// happened to record it: a late-arriving webhook for an old run belongs to
	// the old run's window.
	if n, err := s.pruneRowsBatched("task_run_events", "event_time", cutoff.UnixMilli(), dryRun, deadline); err != nil {
		counts.errors++
		counts.taskRunEvents = n
		log.Printf("[retention] task run events: %v", err)
	} else {
		counts.taskRunEvents = n
	}
	if n, err := s.pruneRowsBatched("messages", "created_at", cutoff, dryRun, deadline); err != nil {
		counts.errors++
		counts.messages = n
		log.Printf("[retention] messages: %v", err)
	} else {
		counts.messages = n
	}
	return counts, expired
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
func (s *Server) pruneRowsBatched(table, column string, cutoff any, dryRun bool, deadline time.Time) (int64, error) {
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
		// Stop at the budget rather than grinding through a multi-million-row
		// backlog in one cycle. The next cycle picks up exactly where this one
		// stopped: the predicate is age, and the rows left behind still match it.
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			log.Printf("[retention] %s: stopping after %d rows, cycle budget %s reached; the rest goes to the next cycle",
				table, total, retentionRowBudget)
			return total, nil
		}
		time.Sleep(retentionBatchPause)
	}
}

// diagnosticsPruneResult is what the diagnostics phase did.
type diagnosticsPruneResult struct {
	removed    int
	bytes      int64
	itemErrors int
}

// pruneDiagnosticsLogs removes captured gateway/bridge logs by file mtime.
// These files have no database row, so the filesystem timestamp is the only
// record of their age.
func pruneDiagnosticsLogs(dataDir string, cutoff time.Time, dryRun bool) (diagnosticsPruneResult, error) {
	var result diagnosticsPruneResult
	if dataDir == "" {
		return result, nil
	}
	dir := filepath.Join(dataDir, "diagnostics")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	sample := &retentionSample{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if dryRun {
			sample.add(entry.Name())
			result.removed++
			result.bytes += info.Size()
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			result.itemErrors++
			log.Printf("[retention] remove diagnostics log %s: %v", entry.Name(), err)
			continue
		}
		result.removed++
		result.bytes += info.Size()
	}
	if dryRun {
		if line := sample.describe("diagnostics log(s)"); line != "" {
			log.Printf("[retention] dry_run: would remove %s", line)
		}
	}
	return result, nil
}

// pruneExpiredCheckpoints deletes the row, its blob references and its manifest
// for checkpoints past the retention window. Unlike compaction, this is the
// point where the analytics record itself expires, so it applies to every
// status.
//
// It returns the ids it removed -- or, in a dry run, the ids it WOULD have
// removed, which is what lets the blob sweep read past the references a real
// cycle would already have dropped -- plus the manifest bytes reclaimed.
func (s *Server) pruneExpiredCheckpoints(cutoff time.Time, dryRun bool) ([]string, int64, int, error) {
	rows, err := s.db.Query(
		`SELECT id, COALESCE(manifest_path,'') FROM claw_checkpoints WHERE created_at < ?`, cutoff)
	if err != nil {
		return nil, 0, 0, err
	}
	type expired struct{ id, manifestPath string }
	var victims []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.manifestPath); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		victims = append(victims, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	var removed []string
	var bytes int64
	itemErrors := 0
	sample := &retentionSample{}
	for _, v := range victims {
		path := v.manifestPath
		if path == "" {
			path = checkpointManifestPath(v.id)
		}
		var size int64
		if info, err := os.Stat(path); err == nil {
			size = info.Size()
		}
		if dryRun {
			sample.add(shortID(v.id))
			removed = append(removed, v.id)
			bytes += size
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			itemErrors++
			log.Printf("[retention] remove expired manifest %s: %v", path, err)
			continue
		} else if err == nil {
			bytes += size
		}
		// The row and every blob it held go together. Nothing else would ever
		// release those references once the row is gone: the reconciliation that
		// collects edges without a row is a crash-recovery backstop, not a
		// primary path.
		if err := s.deleteExpiredCheckpoint(v.id); err != nil {
			return removed, bytes, itemErrors, fmt.Errorf("delete expired checkpoint (aborted after %d of %d): %w", len(removed), len(victims), err)
		}
		removed = append(removed, v.id)
	}
	if dryRun {
		if line := sample.describe("expired checkpoint(s)"); line != "" {
			log.Printf("[retention] dry_run: would delete %s", line)
		}
	}
	return removed, bytes, itemErrors, nil
}

// deleteExpiredCheckpoint removes one checkpoint row and its blob references
// atomically.
func (s *Server) deleteExpiredCheckpoint(checkpointID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteCheckpointBlobRefsTx(tx, checkpointID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM claw_checkpoints WHERE id=?`, checkpointID); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Blob sweep
// ---------------------------------------------------------------------------

// checkpointBlobRefsBackfillMigration names the one-time traversal that gives
// pre-existing checkpoints the reference edges they were never asked for.
const checkpointBlobRefsBackfillMigration = "checkpoint_blob_refs_backfill_v1"

// checkpointBlobRefsBackfilled reports whether that traversal has ever run to
// completion.
//
// THIS IS THE GATE THE SWEEP MUST NOT DELETE WITHOUT. A half-populated edge
// table is indistinguishable from "almost every blob is garbage": a hub that
// upgraded ten minutes ago, or one that crashed mid-backfill, would have the
// sweeper walk 13,000 blobs, find a reference for none of them, and reclaim the
// entire checkpoint store in one cycle. The completion marker is the only thing
// that separates "nothing references this" from "nothing has been asked yet".
func (s *Server) checkpointBlobRefsBackfilled() (bool, error) {
	return hubMigrationApplied(s.db, checkpointBlobRefsBackfillMigration)
}

// backfillCheckpointBlobRefs gives every pre-existing checkpoint the edges it
// would have recorded had it been created under reference counting.
//
// This is the traversal the keep set used to perform on every single cycle --
// parse each manifest, follow each tree blob, union the digest columns -- done
// exactly once. It is:
//
//   - idempotent: every insert is INSERT OR IGNORE on the composite key, so a
//     re-run over checkpoints already covered writes nothing.
//   - non-fatal per item: a manifest that cannot be read or parsed is logged,
//     counted, and skipped. On the disk-full hub this exists to relieve, hitting
//     unreadable files is a certainty, and one of them must not be able to
//     switch reclamation off for the length of the retention window -- that
//     failure mode is what made disk-full self-sustaining the first time.
//   - resumable: the completion marker is written only after the traversal
//     reaches the end. A crash leaves no marker, the sweep declines, and the
//     next cycle starts over into the same rows.
//
// Only 'ready' and 'skipped' rows are traversed, because they are the only
// statuses that hold anything. A 'compacted' or 'failed' row released its blobs
// by definition, and a 'creating' row's edges came from its plan (adopted from
// the legacy claim table by adoptLegacyCheckpointBlobClaims).
func (s *Server) backfillCheckpointBlobRefs() error {
	done, err := s.checkpointBlobRefsBackfilled()
	if err != nil {
		return err
	}
	if done {
		return nil
	}
	started := time.Now()
	rows, err := s.db.Query(`SELECT id, status, COALESCE(manifest_path,''), COALESCE(manifest_sha256,''),
		COALESCE(root_tree_sha256,''), COALESCE(message_tree_sha256,''), COALESCE(workspace_tree_sha256,'')
		 FROM claw_checkpoints WHERE status IN ('ready','skipped')`)
	if err != nil {
		return fmt.Errorf("list checkpoints to backfill: %w", err)
	}
	type row struct{ id, status, manifestPath, manifestSHA, rootSHA, msgSHA, workspaceSHA string }
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.status, &r.manifestPath, &r.manifestSHA, &r.rootSHA, &r.msgSHA, &r.workspaceSHA); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var edges int
	var badManifests, missingTrees, badTrees int
	for _, r := range pending {
		digests := map[string]struct{}{}
		trees := map[string]struct{}{}
		add := func(sha string, isTree bool) {
			clean := normalizeBlobDigest(sha)
			if clean == "" {
				return
			}
			digests[clean] = struct{}{}
			if isTree {
				trees[clean] = struct{}{}
			}
		}
		add(r.manifestSHA, false)
		add(r.msgSHA, false)
		add(r.rootSHA, true)
		add(r.workspaceSHA, true)

		// A row whose manifest_path is empty may still own a manifest at the
		// conventional path (legacy rows, an interrupted finalize). A 'compacted'
		// row may not: it gave its manifest up deliberately, so a file left there
		// is debris from an interrupted unlink, not a reference. Compacted rows
		// are not in this query at all, which keeps the rule from having to be
		// restated here.
		manifestPath := r.manifestPath
		if manifestPath == "" {
			manifestPath = checkpointManifestPath(r.id)
		}
		if data, err := os.ReadFile(manifestPath); err != nil {
			if !os.IsNotExist(err) {
				badManifests++
				log.Printf("[retention] backfill: cannot read manifest %s of checkpoint %s; its row digests are still recorded: %v",
					manifestPath, shortID(r.id), err)
			}
		} else {
			// Decode generically as well as into the struct: schema 1 inlined the
			// complete file list in the manifest, and a generic walk picks those
			// up along with any field a future schema adds.
			var decoded any
			var manifest checkpointManifest
			jsonErr := json.Unmarshal(data, &decoded)
			structErr := json.Unmarshal(data, &manifest)
			if jsonErr != nil || structErr != nil {
				badManifests++
				log.Printf("[retention] backfill: cannot parse manifest %s of checkpoint %s; its row digests are still recorded: %v",
					manifestPath, shortID(r.id), errors.Join(jsonErr, structErr))
			} else {
				for _, sha := range collectDigests(decoded) {
					add(sha, false)
				}
				add(manifest.Workspace.TreeSHA256, true)
			}
		}

		// The one step whose omission would be catastrophic: under schema 2 the
		// per-file digests exist ONLY inside the tree blob.
		for tree := range trees {
			data, err := os.ReadFile(checkpointBlobPath(tree))
			if os.IsNotExist(err) {
				missingTrees++
				log.Printf("[retention] backfill: tree blob %s of checkpoint %s is missing; the files it listed cannot be recorded and will be swept",
					tree, shortID(r.id))
				continue
			}
			if err != nil {
				badTrees++
				log.Printf("[retention] backfill: cannot read tree blob %s of checkpoint %s: %v", tree, shortID(r.id), err)
				continue
			}
			var files []types.CheckpointFile
			if err := json.Unmarshal(data, &files); err != nil {
				badTrees++
				log.Printf("[retention] backfill: cannot parse tree blob %s of checkpoint %s: %v", tree, shortID(r.id), err)
				continue
			}
			for _, f := range files {
				add(f.SHA256, false)
			}
		}

		if len(digests) == 0 {
			continue
		}
		list := make([]string, 0, len(digests))
		for d := range digests {
			list = append(list, d)
		}
		// One transaction per checkpoint, not one for the whole backfill. SQLite
		// has a single writer and this hub runs with a five-second busy_timeout;
		// a transaction spanning thousands of checkpoints would hold the write
		// lock long enough to fail every heartbeat and checkpoint row behind it.
		// A database error here (as opposed to a file error above) aborts the
		// backfill without a marker, so the next cycle retries the whole thing.
		if err := s.insertCheckpointBlobRefs(r.id, list); err != nil {
			return fmt.Errorf("backfill references for checkpoint %s: %w", shortID(r.id), err)
		}
		edges += len(list)
	}

	if err := markHubMigration(s.db, checkpointBlobRefsBackfillMigration); err != nil {
		return err
	}
	log.Printf("[retention] blob reference backfill complete in %s: %d checkpoint(s), %d reference(s); unreadable_manifests=%d missing_trees=%d unreadable_trees=%d (blobs listed only by a missing tree are now unreferenced and will be swept)",
		time.Since(started).Round(time.Millisecond), len(pending), edges, badManifests, missingTrees, badTrees)
	return nil
}

// insertCheckpointBlobRefs records edges for one checkpoint in its own
// transaction.
func (s *Server) insertCheckpointBlobRefs(checkpointID string, digests []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := addCheckpointBlobRefsTx(tx, checkpointID, digests); err != nil {
		return err
	}
	return tx.Commit()
}

// referencedBlobDigests reads the whole reference table into a set.
//
// One read, not one query per blob. The sweep visits ~13,000 files on the hub
// this was written for, and probing the database per file is 13,000 round trips
// against a database whose single write lock the rest of the hub is queueing on.
// The table is two narrow columns and the read is a single sequential scan;
// holding its distinct digests costs ~64 bytes each, which is a rounding error
// next to the directory walk that follows.
//
// released names checkpoints whose edges a real cycle would already have
// deleted. It is empty in a real cycle and carries the dry run's accounting;
// see retentionSweepOnce.
func (s *Server) referencedBlobDigests(released []string) (map[string]struct{}, error) {
	skip := make(map[string]struct{}, len(released))
	for _, id := range released {
		skip[id] = struct{}{}
	}
	rows, err := s.db.Query(`SELECT checkpoint_id, sha256 FROM checkpoint_blob_refs`)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint blob references: %w", err)
	}
	defer rows.Close()
	referenced := make(map[string]struct{})
	for rows.Next() {
		var checkpointID, sha string
		if err := rows.Scan(&checkpointID, &sha); err != nil {
			return nil, err
		}
		if _, dropped := skip[checkpointID]; dropped {
			continue
		}
		if clean := normalizeBlobDigest(sha); clean != "" {
			referenced[clean] = struct{}{}
		}
	}
	return referenced, rows.Err()
}

// collectDigests walks decoded JSON and returns every string that looks like a
// sha256 digest, with or without the "sha256:" prefix. Only the one-time
// backfill uses it: reading a manifest generically is how a schema-1 manifest's
// inlined file list is recovered, and the steady state never reads a manifest at
// all.
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

// blobSweepGrace is how recently a blob may have been written and still be
// spared by the sweep. It must exceed the longest plausible gap between a blob
// upload and the manifest that references it being written -- that is one
// checkpoint's upload phase, not one checkpoint interval.
const blobSweepGrace = time.Hour

// blobSweepFileHook runs once per candidate file inside the walk, before the
// keep/remove decision. It is nil in production and exists solely so a test can
// make a checkpoint plan arrive DURING the walk: that interleaving is the whole
// reason the interlock exists, and a test that seeds the claim before the sweep
// starts exercises the durable claim instead and never reaches it.
var blobSweepFileHook func(name string)

// sweepCheckpointBlobs deletes content-addressed blobs no checkpoint references
// any more, then prunes the fan-out directories it emptied. It runs once at the
// end of a cycle, never per claw: a blob is shared by every checkpoint that
// captured the same bytes, so the question is only meaningful globally.
//
// Two mechanisms protect a blob here, and they answer different questions.
// Neither is redundant:
//
//   - The REFERENCE TABLE answers "should this blob exist?". It is the whole
//     of the correctness argument: an edge exists for as long as a checkpoint
//     holds the blob, so a digest with no edge is genuinely unreachable. No
//     manifest is parsed and no tree is followed to establish that.
//   - The INTERLOCK (blobClaimMu / blobClaimWindow) answers "is it safe to
//     unlink right now?". The database and the filesystem do not share a
//     transaction, so there is always a gap between reading the referenced set
//     and calling os.Remove — and filepath.Walk runs for minutes across it. A
//     plan arriving inside that gap commits its edges, stats the file, and tells
//     the claw not to upload it, while the walker holds a snapshot taken before
//     those edges existed. planBlobMissing records into the window before its
//     stat and removeUnclaimedBlob re-checks it under the same mutex, so the
//     two interleavings are the two safe ones.
//
// Do not delete one as redundant with the other. Refcounting makes the decision
// correct; the interlock makes the deletion safe.
//
// One sweeper goroutine calls this, one cycle at a time, which is what lets the
// window be a single shared map rather than one per sweep.
func (s *Server) sweepCheckpointBlobs(dryRun bool, released []string) (int, int64, int, error) {
	// Arm the interlock BEFORE the referenced set is read, and disarm it only
	// after the walk. Every plan that commits its edges before this point is
	// visible to the query below; every plan that commits after it records into
	// the in-memory window instead. There is no instant in between where a
	// reference is invisible to both.
	s.beginBlobClaimWindow()
	defer s.endBlobClaimWindow()

	// The gate. An edge table that has never been backfilled says "nothing
	// references anything", which is the same sentence as "everything is
	// garbage" and must never be acted on.
	backfilled, err := s.checkpointBlobRefsBackfilled()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("check blob reference backfill: %w", err)
	}
	if !backfilled {
		log.Printf("[retention] blob sweep DECLINED: the checkpoint blob reference backfill has not completed, so an absent reference does not yet mean an unreferenced blob. No blob will be deleted until it does; see the preceding backfill error, if any.")
		return 0, 0, 0, nil
	}

	referenced, err := s.referencedBlobDigests(released)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("not sweeping: %w", err)
	}
	blobRoot := filepath.Join(checkpointsRoot(), "blobs", "sha256")
	if _, err := os.Stat(blobRoot); os.IsNotExist(err) {
		return 0, 0, 0, nil
	}
	// Everything written inside this window is off limits. This is a THIRD line
	// of defence behind the reference table and the interlock, and it earns its
	// place for what neither covers: a blob written by a path that never
	// recorded an edge for it at all.
	//
	// A grace window rather than a lock: the alternative is holding a mutex
	// across every blob upload and the whole sweep, which would stall uploads
	// for the length of a full directory walk.
	cutoff := time.Now().Add(-blobSweepGrace)
	removed, freed, itemErrors := 0, int64(0), 0
	sample := &retentionSample{}
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
		if blobSweepFileHook != nil {
			blobSweepFileHook(info.Name())
		}
		if _, ok := referenced[normalizeBlobDigest(info.Name())]; ok {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		size := info.Size()
		if dryRun {
			if s.blobClaimedDuringSweep(info.Name()) {
				return nil
			}
			sample.add(shortID(info.Name()))
			removed++
			freed += size
			return nil
		}
		// The last check, immediately before the unlink and under the same lock
		// the plan handler takes. A plan that arrived mid-walk has either
		// recorded its claim here -- in which case the blob is spared -- or not
		// yet stat'ed the file, in which case it will find it gone and ask the
		// claw to upload it again. The one outcome the interlock forbids is the
		// one that used to happen: the plan answers "already have it, skip the
		// upload" and the walker, holding a referenced set snapshotted minutes
		// earlier, unlinks it anyway.
		if removedFile, err := s.removeUnclaimedBlob(path, info.Name()); err != nil {
			itemErrors++
			log.Printf("[retention] remove blob %s: %v", path, err)
			return nil
		} else if !removedFile {
			return nil
		}
		removed++
		freed += size
		return nil
	})
	if err != nil {
		return removed, freed, itemErrors, fmt.Errorf("walk blobs (aborted after %d blobs, %d bytes): %w", removed, freed, err)
	}
	if dryRun {
		if line := sample.describe("blob(s)"); line != "" {
			log.Printf("[retention] dry_run: would sweep %s totalling %d bytes", line, freed)
		}
	} else {
		pruneEmptyDirs(blobRoot)
	}
	return removed, freed, itemErrors, nil
}

// ---------------------------------------------------------------------------
// The claim/unlink interlock
// ---------------------------------------------------------------------------
//
// referencedBlobDigests reads the edge table once and filepath.Walk then runs
// for minutes. A plan arriving inside that window commits its edges, sees the
// blob on disk via os.Stat, and tells the claw not to upload it -- while the
// walker, holding a snapshot taken before those edges existed, unlinks it. The
// checkpoint is then published 'ready' referencing a file that is gone, and
// nothing notices until a restore weeks later. Compaction earlier in the SAME
// cycle manufactures these candidates by releasing references, so the window is
// not hypothetical.
//
// Reference counting does not close this. It makes the DECISION correct; the
// gap it leaves is between the decision and the unlink, and no amount of
// accounting inside the database can cover a filesystem operation outside it.
//
// The fix is to make "claim then stat" and "check then unlink" mutually
// exclusive per digest. It is not a lock held across the sweep: each hold
// covers one map operation plus one Stat or one Remove.

// beginBlobClaimWindow arms the window at the start of a sweep.
func (s *Server) beginBlobClaimWindow() {
	s.blobClaimMu.Lock()
	s.blobClaimWindow = make(map[string]struct{})
	s.blobClaimMu.Unlock()
}

// endBlobClaimWindow disarms it, so nothing accumulates between cycles.
func (s *Server) endBlobClaimWindow() {
	s.blobClaimMu.Lock()
	s.blobClaimWindow = nil
	s.blobClaimMu.Unlock()
}

// blobClaimedDuringSweep reports whether a plan claimed this digest since the
// current sweep began. Used by the dry run, which must report the same set the
// real run would remove.
func (s *Server) blobClaimedDuringSweep(name string) bool {
	digest := normalizeBlobDigest(name)
	s.blobClaimMu.Lock()
	defer s.blobClaimMu.Unlock()
	_, ok := s.blobClaimWindow[digest]
	return ok
}

// removeUnclaimedBlob unlinks the blob unless a plan claimed it mid-sweep. It
// reports whether the file was removed.
func (s *Server) removeUnclaimedBlob(path, name string) (bool, error) {
	digest := normalizeBlobDigest(name)
	s.blobClaimMu.Lock()
	defer s.blobClaimMu.Unlock()
	if _, claimed := s.blobClaimWindow[digest]; claimed {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}

// planBlobMissing answers the checkpoint plan's "do you already have this
// blob?" question, and is the claim half of the interlock above.
//
// The digest is recorded BEFORE the stat and under the same lock the walker
// takes, so the two possible interleavings are the two safe ones: either the
// walker has not reached the file yet and will now see the claim, or it has
// already unlinked it and this stat reports it missing, which asks the claw to
// upload it again.
//
// It also refreshes the mtime of a blob that is already present. The grace
// window is documented as a second line of defence behind the durable claim,
// but deduplication returns without writing -- so a reused blob kept an mtime
// from whenever some other claw first wrote it, months ago, and the window
// protected nothing on the one path where dedup actually happens. The earlier
// fix added the touch to the upload handler and the message-blob writer, which
// are the two paths that dedup on blobs they are ABOUT to write; the plan
// handler, the path that answers "do not upload", was the one that mattered.
func (s *Server) planBlobMissing(sha string) bool {
	path := checkpointBlobPath(sha)
	s.blobClaimMu.Lock()
	if s.blobClaimWindow != nil {
		if digest := normalizeBlobDigest(sha); digest != "" {
			s.blobClaimWindow[digest] = struct{}{}
		}
	}
	_, err := os.Stat(path)
	s.blobClaimMu.Unlock()
	if err == nil {
		touchCheckpointBlob(path)
	}
	return os.IsNotExist(err)
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
