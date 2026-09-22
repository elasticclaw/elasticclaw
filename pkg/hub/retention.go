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
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Storage reclamation for the hub's on-disk state.
//
// The hub accumulates three kinds of storage that nothing ever removed: one
// manifest per checkpoint, one content-addressed blob per distinct workspace
// file, and a captured gateway/bridge log per terminated Daytona claw. On the
// Faster hub that filled the disk, at which point every write path — including
// the migrations — starts failing. A cycle reclaims in four phases, then tidies:
//
//	reference backfill -> compaction -> retention -> blob sweep -> tree reference gc
//
// The order matters. Compaction and retention are what release manifests,
// checkpoint rows, and the blob references those rows held; the blob sweep asks
// which blobs are left with no reference at all, so running it first would keep
// blobs whose last reference is about to disappear and leave them for the next
// cycle. The backfill comes first because the two release phases delete
// references, and it must not re-derive references those phases have just
// dropped. The tree gc is last and optional: it removes the expansions of
// trees no checkpoint names any more, which pin nothing either way.
//
// Blob reachability is REFERENCE COUNTED, not derived. checkpoint_blob_refs
// holds the digests each checkpoint row names (root tree, message blob,
// manifest); tree_blob_refs holds the expansion of each distinct workspace tree
// into its files, once. A blob is deletable when no checkpoint edge names it
// and no referenced tree lists it. The sweeper does not parse manifests, follow
// tree blobs, or union digest columns — three review loops found the same class
// of defect in that reconstruction, every one of them "some state the
// derivation did not account for". See checkpointBlobRefTables in db.go for
// the edge lifecycle.

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

// retentionClock is the clock the budget is measured on. It is a variable so
// a test can advance it deterministically and observe that ONE budget spans a
// whole phase (TestCompactionBudgetSpansClaws): with the real clock, a pacer
// built per item and one built per phase are indistinguishable in a test that
// runs in milliseconds.
var retentionClock = time.Now

// retentionBudgetDeadline is when a phase that starts now must stop.
func retentionBudgetDeadline() time.Time {
	return retentionClock().Add(retentionRowBudget)
}

// retentionCheckpointBatch is how many checkpoint rows one transaction covers
// in the loops keyed by checkpoint id: compaction, the release of skipped
// rows, expiry and the reference backfill. Each row costs one UPDATE or DELETE
// plus at most a handful of edge rows, so a quarter of the row batch keeps the
// lock hold in the same range as one batch of pruneRowsBatched.
const retentionCheckpointBatch = retentionDeleteBatch / 4

// retentionSleep is how a pacer releases the write lock between batches. It is
// a variable so the pacing test can count the pauses instead of paying for them.
var retentionSleep = time.Sleep

// errRetentionBudget is what a pacer returns once the cycle budget is spent.
// A phase stops on it and returns what it has done so far WITHOUT a phase
// error: the items left behind still match the predicate that selected them,
// and the next cycle picks them up. The pacer has already logged where the
// phase stopped.
var errRetentionBudget = errors.New("cycle budget reached")

// retentionPacer is the one discipline every bulk loop in this file follows,
// and the one TestEveryBulkWriteLoopIsPaced enforces on any loop that begins
// write transactions: group items into one transaction, call yield after
// every commit, stop when yield says the budget is spent.
//
// Three loops used to be exempt -- expiry, compaction and the backfill each
// issued one transaction per row, thousands of them back to back. A commit
// holds the write lock for a WAL fsync and the next BEGIN IMMEDIATE re-took it
// before any waiter sleeping on the busy-handler ladder woke up, so the lock
// was released on paper and held ~90% of wall time in practice. The 15-second
// heartbeat UPDATE discards its error, so a BUSY heartbeat silently left
// last_seen stale -- and last_seen is what this sweeper's own staleness arm
// and the offline logic read.
type retentionPacer struct {
	phase string
	// deadline is when the phase must stop. Zero means unbounded, which only
	// direct callers in tests use.
	deadline time.Time
	done     int64
	stopped  bool
}

func newRetentionPacer(phase string, deadline time.Time) *retentionPacer {
	return &retentionPacer{phase: phase, deadline: deadline}
}

// yield is called after each committed write-lock hold with the number of
// items it covered. It pauses for retentionBatchPause so a waiter deep in the
// busy-handler ladder actually gets a turn, and returns errRetentionBudget
// once the deadline has passed -- checked before the pause, so a phase never
// sleeps on its way out. The stop is logged once: a phase that commits what it
// had in hand after being told to stop (the backfill's last batch) is not a
// second stop.
func (p *retentionPacer) yield(items int) error {
	p.done += int64(items)
	if p.stopped {
		return errRetentionBudget
	}
	if !p.deadline.IsZero() && !retentionClock().Before(p.deadline) {
		p.stopped = true
		log.Printf("[retention] %s: stopping after %d item(s), cycle budget %s reached; the rest goes to the next cycle",
			p.phase, p.done, retentionRowBudget)
		return errRetentionBudget
	}
	retentionSleep(retentionBatchPause)
	return nil
}

// retentionErrorLog bounds the per-item ERROR lines one phase may emit.
//
// retentionSample already bounds the dry run's selection lines, for the reason
// its comment gives: journald drops a burst above 10,000 lines per 30 seconds,
// and the cycle summary goes with it. The real path had the same shape on its
// error side. A cycle walks ~41k blobs, and when unlinks fail systemically -- a
// filesystem remounted read-only after ENOSPC I/O errors, a blob store restored
// with the wrong ownership, an immutable attribute -- that is ~41k error lines
// in seconds: the run that exists to be read destroys its own output. So each
// phase logs its first few failures with the error text, and flush emits one
// line saying how many more there were and what the first one was. The counts
// the summary line reports are tracked separately, in the phases' itemErrors.
type retentionErrorLog struct {
	phase string
	count int
	first string
}

const retentionErrorLogMax = 5

func (l *retentionErrorLog) add(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.count++
	if l.count == 1 {
		l.first = msg
	}
	if l.count <= retentionErrorLogMax {
		log.Printf("[retention] %s", msg)
	}
}

// flush emits the one line that stands in for everything add swallowed. It is
// called once, when the phase is done.
func (l *retentionErrorLog) flush() {
	if l.count > retentionErrorLogMax {
		log.Printf("[retention] %s: %d more item error(s) not logged; the first was: %s",
			l.phase, l.count-retentionErrorLogMax, l.first)
	}
}

// retentionCounts is what one cycle actually did, per phase.
type retentionCounts struct {
	compacted     int
	diagnostics   int
	checkpoints   int
	taskRunEvents int64
	messages      int64
	blobs         int
	// blobSweepDeclined is set when the sweep refused to run because some
	// checkpoint that holds blobs has no reference edge yet. Without it a
	// permanently failing backfill reads as blobs=0 in the cycle summary, which
	// is byte-identical to a cycle where every blob was referenced.
	blobSweepDeclined bool
	treeRefs          int64
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

// retentionConfigSnapshot reads the retention section under ONE hold of the
// config lock. Everything derived from the configuration -- whether the sweeper
// runs and with which knobs -- must come from the same read: the settings and
// AI-config apply paths swap s.hubCfg wholesale, and two separate reads could
// pair `enabled` from one configuration with the knobs of another.
func (s *Server) retentionConfigSnapshot() *types.RetentionConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return retentionConfig(s.hubCfg)
}

func (s *Server) retentionEnabled() bool {
	return retentionEnabledFrom(s.retentionConfigSnapshot())
}

func retentionEnabledFrom(r *types.RetentionConfig) bool {
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
	return retentionSettingsFrom(s.retentionConfigSnapshot())
}

func retentionSettingsFrom(r *types.RetentionConfig) retentionSettings {
	cfg := retentionSettings{
		interval:     defaultRetentionInterval,
		maxAge:       defaultRetentionMaxAge,
		compactAfter: defaultRetentionCompactAfter,
	}
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

// retentionPolicy is the whole of what a cycle needs to know: whether it runs
// at all, and with which knobs.
type retentionPolicy struct {
	enabled bool
	retentionSettings
}

// retentionPolicy reads the policy the configuration currently describes, from
// ONE snapshot of the configuration. `enabled` and the knobs used to be read
// under two separate lock holds, and a settings apply between them produced a
// policy that no configuration ever described.
func (s *Server) retentionPolicy() retentionPolicy {
	r := s.retentionConfigSnapshot()
	return retentionPolicy{enabled: retentionEnabledFrom(r), retentionSettings: retentionSettingsFrom(r)}
}

// same reports whether two policies would run the same cycle. adjustments is
// left out on purpose: it explains how the values were derived, and two
// derivations that land on the same values are the same policy.
func (p retentionPolicy) same(o retentionPolicy) bool {
	return p.enabled == o.enabled && p.dryRun == o.dryRun && p.interval == o.interval &&
		p.maxAge == o.maxAge && p.compactAfter == o.compactAfter
}

func (p retentionPolicy) String() string {
	return fmt.Sprintf("enabled=%v dry_run=%v interval=%s max_age=%s compact_after=%s",
		p.enabled, p.dryRun, p.interval, p.maxAge, p.compactAfter)
}

// retentionSweeper runs a reclamation cycle on every tick, under the policy
// the hub booted with.
//
// It deliberately does NOT run at boot. Deleting is irreversible and the
// window an operator has to notice a misconfigured retention policy is the
// window between the hub coming up and the first sweep: starting on the first
// tick means a fresh deploy always leaves `interval` (one hour by default) to
// export data or turn the sweeper off before anything is removed. Restarting
// the hub therefore never sweeps immediately, which also stops a crash loop
// from turning into a delete loop.
//
// The policy is READ ONCE, here, and never reloaded. See startRetentionRun.
func (s *Server) retentionSweeper() {
	run := s.startRetentionRun()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[retention] loop panic, restarting: %v", r)
				}
			}()
			ticker := time.NewTicker(run.policy.interval)
			defer ticker.Stop()
			for range ticker.C {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[retention] cycle panic: %v", r)
						}
					}()
					run.cycle()
				}()
			}
		}()
	}
}

// retentionRun is one sweeper's lifetime: the policy it booted with, and the
// last configured policy it compared that against.
//
// The retention section of hub.yaml takes effect at boot and NOT before the
// next restart, `enabled` and `dry_run` included. That is the contract the
// operator documentation states, and it is chosen over hot reload on purpose.
// The settings and AI-config apply paths replace s.hubCfg wholesale from a
// proposed YAML and rewrite hub.yaml with it; a sweeper reading s.hubCfg every
// tick therefore had two failure modes neither document mentioned. A proposal
// that merely omitted the `retention:` section dropped it from memory and from
// disk, after which the tick returned before its start line and the sweeper
// went silent with no log at all; and a proposal flipping dry_run to false armed
// irreversible deletion on the next tick, with no policy line and none of the
// grace a restart gives (the first cycle is always one interval after boot).
// A restart is cheap, is already how interval was applied, and puts every knob
// behind the same rule and the same startup line.
//
// What the sweeper does do is SAY when the configuration has diverged from the
// policy it is running: once per change, on the next tick, so an operator who
// edited the file and is waiting for the next cycle learns why nothing changed.
type retentionRun struct {
	s          *Server
	policy     retentionPolicy
	configured retentionPolicy
}

// startRetentionRun snapshots the configured policy and states it.
func (s *Server) startRetentionRun() *retentionRun {
	policy := s.retentionPolicy()
	logRetentionPolicyLine(policy, s.retentionNow())
	return &retentionRun{s: s, policy: policy, configured: policy}
}

// cycle runs one reclamation cycle under the boot policy, after reporting a
// configuration that no longer matches it.
func (r *retentionRun) cycle() {
	configured := r.s.retentionPolicy()
	if !configured.same(r.configured) {
		r.configured = configured
		if configured.same(r.policy) {
			log.Printf("[retention] configuration matches the running policy again [%s]", r.policy)
		} else {
			log.Printf("[retention] configuration changed since boot: configured [%s], running [%s]; retention settings are read once at boot, so the running policy stays in force until the hub restarts",
				configured, r.policy)
		}
	}
	r.s.retentionSweepCycle(r.policy)
}

// logRetentionPolicy states the policy the configuration currently describes.
// The sweeper states the one it booted with, through startRetentionRun.
func (s *Server) logRetentionPolicy() {
	logRetentionPolicyLine(s.retentionPolicy(), s.retentionNow())
}

// logRetentionPolicyLine states the effective policy exactly once, at startup.
//
// Nothing else does. A disabled sweeper is silent forever, so a misspelt or
// misplaced `enabled` key -- or a hub deployed before anyone opted in --
// produced no signal at all and looked identical to a sweeper that was running
// and finding nothing, for as long as anyone cared to wait. An
// enabled one only spoke an hour later, after its first cycle, and never said
// which values it was actually using: the floors and the compact_after/max_age
// ordering rule can both silently replace a configured value.
func logRetentionPolicyLine(policy retentionPolicy, now time.Time) {
	if !policy.enabled {
		log.Printf("[retention] disabled: no reclamation cycle will run (set retention.enabled: true and restart the hub to arm it, ideally with dry_run first)")
		return
	}
	adjusted := "none"
	if len(policy.adjustments) > 0 {
		adjusted = strings.Join(policy.adjustments, "; ")
	}
	log.Printf("[retention] enabled: dry_run=%v interval=%s max_age=%s compact_after=%s adjustments=[%s] first_cycle_at=%s",
		policy.dryRun, policy.interval, policy.maxAge, policy.compactAfter, adjusted,
		now.Add(policy.interval).Format(time.RFC3339))
}

// retentionSweepOnce runs one full reclamation cycle under the policy the
// configuration currently describes. The sweeper does not use it -- it runs
// its boot snapshot through retentionSweepCycle -- so this is the entry point
// for a caller that wants "one cycle, now, as configured".
func (s *Server) retentionSweepOnce() {
	s.retentionSweepCycle(s.retentionPolicy())
}

// retentionSweepCycle runs one full reclamation cycle under the given policy.
//
// Every cycle logs a start line and an end line, including the cycles that
// reclaim nothing -- and a disabled policy logs that it skipped. A sweeper that
// has been silent for a day is otherwise indistinguishable from one that is
// wedged, disabled, or crashing in a phase that only logs on success, and
// telling those apart after the fact was not possible at all.
func (s *Server) retentionSweepCycle(policy retentionPolicy) {
	if !policy.enabled {
		log.Printf("[retention] cycle skipped: retention disabled (set retention.enabled: true and restart the hub to arm it)")
		return
	}
	cfg, n := policy.retentionSettings, s.retentionNow()
	expiryCutoff, compactCutoff := n.Add(-cfg.maxAge), n.Add(-cfg.compactAfter)
	started := time.Now()
	log.Printf("[retention] cycle start: dry_run=%v expire_before=%s compact_before=%s",
		cfg.dryRun, expiryCutoff.Format(time.RFC3339), compactCutoff.Format(time.RFC3339))

	var counts retentionCounts

	// First, every cycle, and before anything releases a reference. A hub
	// upgrading into reference counting -- or one that ran a build without it
	// for a while and came back -- has 'ready' rows with no edges at all, and
	// until they have some the edge table reads as "almost every blob is
	// garbage". The sweep refuses to delete anything while any such row exists
	// (see unreferencedCheckpoints), so a failure here costs a postponed sweep
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

	sweep, err := s.sweepCheckpointBlobsResult(cfg.dryRun, released)
	counts.blobs, counts.blobBytes = sweep.removed, sweep.bytes
	counts.itemErrors += sweep.itemErrors
	counts.blobSweepDeclined = sweep.declined
	sweepRan := err == nil && !sweep.declined
	if err != nil {
		counts.errors++
		log.Printf("[retention] blob sweep: %v", err)
	}

	// Hygiene, after the sweep and never in its way. A tree expansion that no
	// checkpoint edge names any more pins nothing -- the keep set only expands
	// REFERENCED trees -- so leaving it behind costs rows, not blobs, and a
	// failure here must not be allowed to block the cycle that reclaims disk.
	//
	// Only in a cycle whose sweep RAN, which is the same thing as "every row
	// that holds blobs has its edges". The backfill commits a tree's expansion
	// in its own transaction, before the edge of the checkpoint that names it;
	// a row it did not finish this cycle -- the budget fired on the yield right
	// after the expansion, a second tree of the row could not be read, the
	// batch's commit failed -- has no edge yet, and its freshly written
	// expansion is exactly what this gc selects: up to a workspace's worth of
	// rows written and deleted in the same cycle, then written again in the
	// next, on the disk-full hub. While any such row exists the sweep has
	// declined, so there is nothing for the gc to make collectable anyway; it
	// waits for the cycle that clears the gate. Gating on the sweep rather than
	// on a "backfill stopped" flag covers every way a row can be left behind,
	// not only the budget.
	if !cfg.dryRun && sweepRan {
		if n, err := s.pruneUnreferencedTreeBlobRefs(newRetentionPacer("tree reference gc", retentionBudgetDeadline())); err != nil {
			counts.errors++
			counts.treeRefs = n
			log.Printf("[retention] tree reference gc: %v", err)
		} else {
			counts.treeRefs = n
		}
	}

	verb := "removed"
	if cfg.dryRun {
		verb = "would remove"
	}
	blobs := strconv.Itoa(counts.blobs)
	if counts.blobSweepDeclined {
		blobs = fmt.Sprintf("DECLINED(%d checkpoint(s) without references)", sweep.unreferenced)
	}
	log.Printf("[retention] cycle done in %s (dry_run=%v): %s compacted=%d diagnostics=%d checkpoints=%d task_run_events=%d messages=%d blobs=%s tree_refs=%d bytes_freed=%d (blobs=%d manifests=%d diagnostics=%d; row deletes free SQLite pages but do not shrink the database file without a VACUUM) phase_errors=%d item_errors=%d",
		time.Since(started).Round(time.Millisecond), cfg.dryRun, verb,
		counts.compacted, counts.diagnostics, counts.checkpoints,
		counts.taskRunEvents, counts.messages, blobs, counts.treeRefs,
		counts.blobBytes+counts.manifestBytes+counts.diagnosticsBytes,
		counts.blobBytes, counts.manifestBytes, counts.diagnosticsBytes,
		counts.errors, counts.itemErrors)

	// Deliberately last, and never in a dry run. These indexes only make the
	// batched row deletes cheap, and building one needs free disk for the whole
	// B-tree -- so running it at the TOP of the cycle, as this used to, meant a
	// hub with a full disk spent its first act on a doomed full-table index
	// build under the single write lock, before a single byte had been
	// reclaimed. At the end, the cycle it accelerates is the NEXT one, and the
	// space this cycle freed is what it gets to use.
	//
	// A dry run frees nothing, so it has no such space to offer -- and it is
	// the step the documentation tells an operator to run FIRST, on a hub that
	// is by hypothesis nearly full. Building two indexes over the two largest
	// tables there could consume the last free space before anything had been
	// reclaimed, and on a 100%-full hub it failed every cycle with two
	// `[migrate]` lines the documentation never mentioned. The documentation's
	// claim that a dry run writes only the reference backfill is true only with
	// this guard; the boot-time build in openDB still covers a hub that has the
	// space.
	if !cfg.dryRun {
		s.buildRetentionIndexesAfterCycle(counts.blobBytes + counts.manifestBytes + counts.diagnosticsBytes)
	}
}

// retentionIndexBuild is the sweeper's memory of its attempts to build the
// retention indexes after a cycle. One sweeper goroutine touches it, one cycle
// at a time.
type retentionIndexBuild struct {
	failures   int  // consecutive failed attempts
	skip       int  // reclaiming cycles still to let pass before the next attempt
	waitLogged bool // the "waiting for a cycle that reclaims" line was said
}

// retentionIndexBackoffMax caps how many reclaiming cycles a failed build sits
// out before trying again: eight is a working day at the default interval.
const retentionIndexBackoffMax = 8

// buildRetentionIndexesAfterCycle is the one write-lock hold in a cycle that no
// pacer bounds, so it is gated instead: the indexes are built only when one is
// actually missing, only after a cycle that reclaimed bytes on the filesystem,
// and with a backoff after a failed attempt.
//
// CREATE INDEX IF NOT EXISTS is free once the index exists, but on the hub
// whose boot could not build it -- the disk-full hub -- it is a scan of the two
// largest tables under the write lock that allocates the whole B-tree and then
// fails at the last page, and it used to run at the end of EVERY cycle. The
// 15-second heartbeat discards its BUSY error, so each doomed attempt could
// silently lose a heartbeat. A cycle that freed nothing has nothing to offer
// the build; a cycle that freed bytes is the only thing that can change the
// outcome, and even then an attempt that fails sits out an increasing number
// of such cycles, because on a hub reclaiming a little every hour the freed
// space is rarely the size of an index.
func (s *Server) buildRetentionIndexesAfterCycle(bytesFreed int64) {
	missing, err := missingRetentionIndexes(s.db)
	if err != nil {
		log.Printf("[retention] probe retention indexes: %v", err)
		return
	}
	if len(missing) == 0 {
		s.retentionIndexes = retentionIndexBuild{}
		return
	}
	state := &s.retentionIndexes
	if bytesFreed <= 0 {
		if !state.waitLogged {
			state.waitLogged = true
			log.Printf("[retention] retention index(es) %s missing; the build waits for a cycle that reclaims disk space, and this one freed nothing", strings.Join(missing, ", "))
		}
		return
	}
	if state.skip > 0 {
		state.skip--
		log.Printf("[retention] retention index(es) %s still missing after %d failed build(s); sitting out this cycle (%d more to go)", strings.Join(missing, ", "), state.failures, state.skip)
		return
	}
	if ensureRetentionIndexes(s.db) {
		*state = retentionIndexBuild{}
		return
	}
	state.failures++
	state.skip = min(1<<state.failures-1, retentionIndexBackoffMax)
}

// missingRetentionIndexes names the retention indexes that do not exist.
func missingRetentionIndexes(db *sql.DB) ([]string, error) {
	var missing []string
	for _, idx := range retentionIndexes {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx.name).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			missing = append(missing, idx.name)
		}
	}
	return missing, nil
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
// A claw with a restore in flight is not finalized whatever else is true of it.
// restoreClawFromCheckpoint and the retry path set restore_checkpoint_id before
// reprovisioning and markRestoreApplied clears it once every file has been
// written; in between, restoreCheckpointFilesTo reads the source checkpoint's
// tree and file blobs by path for minutes, with nothing else pinning the row.
// Compacting that claw could pick another row as the survivor and drop the
// source's manifest and edges mid-stream, after which the same cycle's sweep
// unlinks its files. A restore that failed leaves the id in place, and that is
// right too: the row is what the operator retries from.
//
// The caller binds the cutoff (now - compact_after) twice. The alias `c` must
// be the claws table.
const finalizedClawPredicateSQL = `(
	COALESCE(c.restore_checkpoint_id, '') = ''
	AND (
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
	)
)`

// restoreSourceCheckpointsSQL names every checkpoint some claw is restoring
// from right now. Expiry keeps its hands off these rows for the reason given
// on finalizedClawPredicateSQL: the restore reads their blobs by path for
// minutes, and deleting the row drops the edges that keep those blobs out of
// the same cycle's sweep. The row is deleted normally once the restore has
// applied (markRestoreApplied clears the id) or the claw is gone.
const restoreSourceCheckpointsSQL = `SELECT restore_checkpoint_id FROM claws WHERE COALESCE(restore_checkpoint_id, '') <> ''`

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
	result.errLog = &retentionErrorLog{phase: "compaction"}
	defer result.errLog.flush()
	result.pacer = newRetentionPacer("compaction", retentionBudgetDeadline())
	ids, err := s.finalizedClawIDs(cutoff)
	if err != nil {
		return result, fmt.Errorf("list finalized claws: %w", err)
	}
	for i, clawID := range ids {
		err := s.compactClawCheckpoints(clawID, cutoff, dryRun, &result)
		if errors.Is(err, errRetentionBudget) {
			break
		}
		if err != nil {
			// Say where in the phase the failure landed. The earlier wording
			// claimed the phase "aborted after N of M claws" while the loop in
			// fact continues to the next claw, which is the opposite of what an
			// operator reading it would do next.
			result.itemErrors++
			result.errLog.add("compact claw %s (%d of %d): %v (continuing with the remaining claws)",
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
	errLog     *retentionErrorLog
	// pacer is shared by every claw of the phase: the budget bounds the phase,
	// not one claw, and a claw with a single candidate would otherwise never
	// pause at all.
	pacer *retentionPacer
}

func (s *Server) compactClawCheckpoints(clawID string, cutoff time.Time, dryRun bool, result *compactionResult) error {
	// The list this claw came from was built once, at the top of the phase,
	// and the phase then runs for up to its budget with a pause after every
	// claw. A claw that resumed in the meantime -- reconnected, or entered a
	// retry that will restore from one of the rows below -- is no longer
	// finalized, and compacting it would collapse the recovery points of a
	// claw about to use them. Re-evaluate the predicate now, for this claw
	// alone. The write below re-checks the restore arm once more, under the
	// write lock, for the restore that begins between this read and that
	// UPDATE.
	if finalized, err := s.clawFinalized(clawID, cutoff); err != nil {
		return err
	} else if !finalized {
		return nil
	}
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
	defer rows.Close()
	var candidates []compactionCandidate
	for rows.Next() {
		var c compactionCandidate
		if err := rows.Scan(&c.id, &c.manifestPath, &c.rootTree, &c.reason); err != nil {
			return err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Before the early return below, not after it. A 'skipped' row holds
	// references to the tree of the ready checkpoint it duplicated, and that
	// ready row may have been compacted by an EARLIER cycle -- in which case this
	// claw is down to one ready candidate, takes the early return, and its
	// skipped rows go on holding a workspace nothing can restore from, forever.
	releasedSkipped, err := s.releaseSkippedCheckpointRefs(clawID, dryRun, result.pacer)
	// Reported among the released ids, not in the compacted COUNT: a skipped row
	// letting go of its blobs is not a checkpoint moving from "restorable" to
	// "counted". The dry run still needs to know about it, because the sweep must
	// read past those references too.
	result.ids = append(result.ids, releasedSkipped...)
	if errors.Is(err, errRetentionBudget) {
		return err
	}
	if err != nil {
		result.itemErrors++
		result.errLog.add("release skipped checkpoint references for claw %s: %v", shortID(clawID), err)
	}
	if len(candidates) <= 1 {
		return nil
	}
	keep, err := s.survivingCheckpoint(clawID, candidates)
	if err != nil {
		return err
	}
	type victim struct {
		id, path string
		size     int64
	}
	var victims []victim
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
		victims = append(victims, victim{id: c.id, path: path, size: size})
	}
	// Mark the rows FIRST, unlink second, one batch at a time.
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
	for start := 0; start < len(victims); start += retentionCheckpointBatch {
		batch := victims[start:min(start+retentionCheckpointBatch, len(victims))]
		ids := make([]string, 0, len(batch))
		for _, v := range batch {
			ids = append(ids, v.id)
		}
		compacted, err := s.markCheckpointsCompacted(ids)
		if err != nil {
			return err
		}
		for _, v := range batch {
			if _, ok := compacted[v.id]; !ok {
				// The UPDATE left this row alone: a restore of this claw began
				// after the candidates were listed. Its manifest stays, and so
				// does every other row of the claw -- the restore may be reading
				// any of them.
				continue
			}
			// A manifest that is already gone is not an error: a previous cycle
			// may have been interrupted between the UPDATE and the unlink.
			if err := os.Remove(v.path); err != nil && !os.IsNotExist(err) {
				result.itemErrors++
				result.errLog.add("remove manifest %s: %v", v.path, err)
			} else if err == nil {
				result.bytes += v.size
			}
			result.count++
			result.ids = append(result.ids, v.id)
		}
		if len(compacted) < len(batch) {
			return nil
		}
		if err := result.pacer.yield(len(batch)); err != nil {
			return err
		}
	}
	return nil
}

// markCheckpointCompacted moves one checkpoint from "restorable" to "counted"
// and releases every blob it held, atomically.
func (s *Server) markCheckpointCompacted(checkpointID string) error {
	_, err := s.markCheckpointsCompacted([]string{checkpointID})
	return err
}

// markCheckpointsCompacted does the same for one batch of checkpoints, in one
// transaction: either every row in the batch is compacted with its references
// gone, or none is. It returns the ids it actually compacted.
//
// The restore guard is re-evaluated INSIDE the UPDATE, under the write lock.
// The candidate list was read outside any transaction, and a restore that
// starts after that read -- an operator's restoreClawFromCheckpoint, or the
// retry path, which waits out its backoff and a termination checkpoint before
// it sets restore_checkpoint_id -- would otherwise have its source row marked
// compacted, its manifest unlinked, its edges dropped, and its file blobs
// swept by the same cycle while restoreCheckpointFilesTo is reading them. A
// row the guard excludes is reported as not compacted, and keeps its edges:
// dropping them is what makes its blobs collectable.
func (s *Server) markCheckpointsCompacted(checkpointIDs []string) (map[string]struct{}, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	compacted := make(map[string]struct{}, len(checkpointIDs))
	for _, checkpointID := range checkpointIDs {
		res, err := tx.Exec(
			`UPDATE claw_checkpoints
			    SET status='compacted', manifest_path='', manifest_sha256='',
			        root_tree_sha256='', workspace_tree_sha256='', message_tree_sha256=''
			  WHERE id=?
			    AND NOT EXISTS (
			        SELECT 1 FROM claws c
			         WHERE c.id = claw_checkpoints.claw_id
			           AND COALESCE(c.restore_checkpoint_id, '') <> '')`, checkpointID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := deleteCheckpointBlobRefsTx(tx, checkpointID); err != nil {
			return nil, err
		}
		compacted[checkpointID] = struct{}{}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return compacted, nil
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
func (s *Server) releaseSkippedCheckpointRefs(clawID string, dryRun bool, pacer *retentionPacer) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM claw_checkpoints WHERE claw_id=? AND status='skipped'
		   AND (root_tree_sha256 <> '' OR workspace_tree_sha256 <> '' OR message_tree_sha256 <> ''
		        OR EXISTS (SELECT 1 FROM checkpoint_blob_refs r WHERE r.checkpoint_id = claw_checkpoints.id))`,
		clawID)
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if dryRun || len(ids) == 0 {
		return ids, nil
	}
	// Batched, with the write lock released between batches, like every other
	// bulk delete in this file. A long-idle claw has thousands of skipped rows,
	// and one transaction over all of them -- even at a handful of rows each --
	// holds BEGIN IMMEDIATE past the 5s busy_timeout every other writer runs
	// with. Only the ids actually released are reported, so a batch that fails
	// leaves the dry-run accounting honest about the rest.
	var released []string
	for start := 0; start < len(ids); start += retentionCheckpointBatch {
		batch := ids[start:min(start+retentionCheckpointBatch, len(ids))]
		if err := s.releaseSkippedCheckpointRefsBatch(batch); err != nil {
			return released, err
		}
		released = append(released, batch...)
		if err := pacer.yield(len(batch)); err != nil {
			return released, err
		}
	}
	return released, nil
}

func (s *Server) releaseSkippedCheckpointRefsBatch(ids []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(
			`UPDATE claw_checkpoints
			    SET root_tree_sha256='', workspace_tree_sha256='', message_tree_sha256=''
			  WHERE id=?`, id); err != nil {
			return err
		}
		if err := deleteCheckpointBlobRefsTx(tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	// One budget for the three batched targets, not one each: they contend for
	// the same single write lock, so the thing worth bounding is the total time
	// this cycle spends holding it.
	deadline := retentionBudgetDeadline()
	expired, manifestBytes, itemErrors, err := s.pruneExpiredCheckpoints(cutoff, dryRun, newRetentionPacer("checkpoints", deadline))
	counts.checkpoints = len(expired)
	counts.manifestBytes = manifestBytes
	counts.itemErrors += itemErrors
	if err != nil {
		counts.errors++
		log.Printf("[retention] checkpoints: %v", err)
	}
	// task_run_events is keyed on when the event happened, not when the hub
	// happened to record it: a late-arriving webhook for an old run belongs to
	// the old run's window.
	if n, err := s.pruneRowsBatched("task_run_events", "event_time", cutoff.UnixMilli(), dryRun, newRetentionPacer("task_run_events", deadline)); err != nil {
		counts.errors++
		counts.taskRunEvents = n
		log.Printf("[retention] task run events: %v", err)
	} else {
		counts.taskRunEvents = n
	}
	if n, err := s.pruneRowsBatched("messages", "created_at", cutoff, dryRun, newRetentionPacer("messages", deadline)); err != nil {
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
func (s *Server) pruneRowsBatched(table, column string, cutoff any, dryRun bool, pacer *retentionPacer) (int64, error) {
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
		if err := pacer.yield(int(n)); err != nil {
			return total, nil
		}
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
	errLog := &retentionErrorLog{phase: "diagnostics"}
	defer errLog.flush()
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
			errLog.add("remove diagnostics log %s: %v", entry.Name(), err)
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
func (s *Server) pruneExpiredCheckpoints(cutoff time.Time, dryRun bool, pacer *retentionPacer) ([]string, int64, int, error) {
	rows, err := s.db.Query(
		`SELECT id, COALESCE(manifest_path,'') FROM claw_checkpoints
		  WHERE created_at < ? AND id NOT IN (`+restoreSourceCheckpointsSQL+`)`, cutoff)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	type expired struct{ id, manifestPath string }
	var victims []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.manifestPath); err != nil {
			return nil, 0, 0, err
		}
		victims = append(victims, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	var removed []string
	var bytes int64
	itemErrors := 0
	sample := &retentionSample{}
	errLog := &retentionErrorLog{phase: "checkpoints"}
	defer errLog.flush()
	type victim struct {
		id, path string
		size     int64
	}
	var pending []victim
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
		pending = append(pending, victim{id: v.id, path: path, size: size})
	}
	// Delete the rows FIRST, unlink second, one batch at a time -- the same
	// order compaction uses, for the same reason. Unlinking first left a window
	// where the manifest was gone and the row still said 'ready' with a
	// manifest_path pointing at it; if the DELETE then failed (ENOSPC,
	// SQLITE_BUSY -- the conditions this sweeper runs under), the next cycle's
	// compaction could pick that row as a claw's survivor and keep a checkpoint
	// that cannot be restored. This order's failure mode is a manifest left on
	// disk with no row, which is wasted bytes, and is counted below rather than
	// hidden.
	//
	// The rows and every blob they held go together, in one transaction. A
	// delete that fails rolls both back, so it can leave neither a row without
	// edges nor an edge without a row; the boot-time release of edges whose row
	// is gone (releaseOrphanedCheckpointBlobRefs) covers a crash, not this.
	for start := 0; start < len(pending); start += retentionCheckpointBatch {
		batch := pending[start:min(start+retentionCheckpointBatch, len(pending))]
		ids := make([]string, 0, len(batch))
		for _, v := range batch {
			ids = append(ids, v.id)
		}
		deleted, err := s.deleteExpiredCheckpoints(ids)
		if err != nil {
			return removed, bytes, itemErrors, fmt.Errorf("delete expired checkpoints (aborted after %d of %d): %w", len(removed), len(pending), err)
		}
		for _, v := range batch {
			if _, ok := deleted[v.id]; !ok {
				// A restore from this row began after the victims were listed;
				// the DELETE left it alone, and so must the unlink. It is not in
				// removed either: that list tells the sweep which references to
				// read past, and this row's references still stand.
				continue
			}
			removed = append(removed, v.id)
			if err := os.Remove(v.path); err != nil && !os.IsNotExist(err) {
				itemErrors++
				errLog.add("remove expired manifest %s: %v", v.path, err)
			} else if err == nil {
				bytes += v.size
			}
		}
		if err := pacer.yield(len(batch)); err != nil {
			break
		}
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
	_, err := s.deleteExpiredCheckpoints([]string{checkpointID})
	return err
}

// deleteExpiredCheckpoints does the same for one batch, in one transaction,
// and returns the ids it actually deleted.
//
// The restore guard of the victim list (restoreSourceCheckpointsSQL) is
// re-evaluated inside the DELETE, for the same reason markCheckpointsCompacted
// re-evaluates its own: the list is minutes old by the time the last batch is
// written, and a row some claw started restoring from in between must keep
// its row, its edges and its manifest. The row goes first and the edges only
// when it went, so a guarded row keeps the edges that protect its blobs.
func (s *Server) deleteExpiredCheckpoints(checkpointIDs []string) (map[string]struct{}, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	deleted := make(map[string]struct{}, len(checkpointIDs))
	for _, checkpointID := range checkpointIDs {
		res, err := tx.Exec(`DELETE FROM claw_checkpoints WHERE id=? AND id NOT IN (`+restoreSourceCheckpointsSQL+`)`, checkpointID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if err := deleteCheckpointBlobRefsTx(tx, checkpointID); err != nil {
			return nil, err
		}
		deleted[checkpointID] = struct{}{}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return deleted, nil
}

// ---------------------------------------------------------------------------
// Blob sweep
// ---------------------------------------------------------------------------

// unreferencedCheckpointsWhere selects the checkpoint rows that hold blobs --
// they are 'ready' or 'skipped' and their digest columns name something -- but
// have no reference edge at all.
//
// This predicate is both the backfill's work list and the sweep's gate, and it
// is evaluated EVERY cycle rather than once. The one-time version of the
// backfill, keyed on a completion marker, had a hole exactly the size of a
// rollback: deploy this build (marker written), roll back to a build that
// records no edges, run for a day, roll forward. Every checkpoint of that day
// is 'ready' with a valid manifest and zero edges, the marker says the backfill
// is done, the keep set omits them, and their blobs are older than the grace
// window by the time the sweep runs. The rows stay 'ready', so the loss
// surfaces only when a retry tries to restore from one.
//
// A row that names no digest holds nothing and is not selected: a 'skipped'
// row compaction has released has its columns cleared and its edges deleted in
// the same transaction, and would otherwise be re-selected forever. A 'ready'
// row always carries its manifest digest. The NOT EXISTS is one probe of the
// (checkpoint_id, sha256) primary key per row, so on a hub where every row
// already has edges the whole evaluation is one indexed pass over the rows
// that hold blobs.
//
// "Names a digest" means exactly what the writer accepts. The backfill records
// only values normalizeBlobDigest accepts, so the gate must select only rows
// with at least one such value: a row whose only non-empty column held
// something else -- an upper-case digest, a truncated one, a bridge's
// placeholder -- was selected by a `<> ”` test, yielded no edge, stayed on the
// work list, and held the sweep DECLINED forever while the backfill line
// counted it as processed. blobDigestSQL is that acceptance test in SQL, and
// TestGatePredicateMatchesTheWriter holds the two to each other.
//
// The test is PER COLUMN: a row is unreferenced when any one of its digest
// columns names a digest that has no edge under the row. "Does the row have
// any edge at all" was the previous test, and it let a row with a root edge
// but no edge for its message blob off the work list -- the sweep then read
// that message blob as garbage. Four indexed probes on the (checkpoint_id,
// sha256) primary key, instead of one, on the rows that hold blobs.
var unreferencedCheckpointsWhere = `status IN ('ready','skipped')
	   AND (` + unreferencedDigestSQL("manifest_sha256") + ` OR ` + unreferencedDigestSQL("root_tree_sha256") + `
	        OR ` + unreferencedDigestSQL("message_tree_sha256") + ` OR ` + unreferencedDigestSQL("workspace_tree_sha256") + `)`

// unreferencedDigestSQL is "col names a digest and the row has no edge for
// it" as a SQL predicate. The edge is probed by the bare digest, which is what
// the writer records.
func unreferencedDigestSQL(col string) string {
	return fmt.Sprintf(`(%s AND NOT EXISTS (SELECT 1 FROM checkpoint_blob_refs r WHERE r.checkpoint_id = claw_checkpoints.id AND r.sha256 = %s))`,
		blobDigestSQL(col), blobDigestBareSQL(col))
}

// blobDigestSQL is normalizeBlobDigest(col) <> "" as a SQL predicate: after
// trimming whitespace and an optional "sha256:" prefix, exactly 64 characters
// and every one of them a lower-case hex digit. NULL is not a digest.
func blobDigestSQL(col string) string {
	bare := blobDigestBareSQL(col)
	return fmt.Sprintf(`(length(%s) = 64 AND %s NOT GLOB '*[^0-9a-f]*')`, bare, bare)
}

// blobDigestBareSQL is the value normalizeBlobDigest would return for col,
// before the length and character checks: trimmed, prefix removed.
func blobDigestBareSQL(col string) string {
	trimmed := fmt.Sprintf(`trim(%s, ' ' || char(9,10,11,12,13))`, col)
	return fmt.Sprintf(`(CASE WHEN substr(%s,1,7)='sha256:' THEN substr(%s,8) ELSE %s END)`, trimmed, trimmed, trimmed)
}

// unreferencedCheckpoints counts the rows that hold blobs without an edge, and
// returns a few of their ids for the log line.
//
// THIS IS THE GATE THE SWEEP MUST NOT DELETE THROUGH. A row that holds blobs
// and has no edge reads, to the keep set, as "nothing references these blobs",
// which is the same sentence as "these blobs are garbage" and must never be
// acted on. The count is non-zero on a hub that upgraded ten minutes ago, on
// one that crashed mid-backfill, and on one that came back from a rollback --
// and in all three cases the answer is the same: give those rows their edges
// first (backfillCheckpointBlobRefs), and sweep only when none are left.
func (s *Server) unreferencedCheckpoints() (count int64, sample []string, err error) {
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM claw_checkpoints WHERE ` + unreferencedCheckpointsWhere).Scan(&count); err != nil {
		return 0, nil, fmt.Errorf("count checkpoints without blob references: %w", err)
	}
	if count == 0 {
		return 0, nil, nil
	}
	rows, err := s.db.Query(`SELECT id FROM claw_checkpoints WHERE `+unreferencedCheckpointsWhere+` ORDER BY created_at DESC LIMIT ?`, retentionSampleMax)
	if err != nil {
		return count, nil, fmt.Errorf("sample checkpoints without blob references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return count, nil, err
		}
		sample = append(sample, shortID(id))
	}
	return count, sample, rows.Err()
}

// backfillCheckpointBlobRefs gives every checkpoint that holds blobs without an
// edge the edges it would have recorded had it been created under reference
// counting. It runs at the top of every cycle; on a hub where nothing needs it,
// it costs the one indexed query in unreferencedCheckpointsWhere.
//
// It follows the same rule as the live writes: per checkpoint, the root /
// manifest / message edges; per DISTINCT tree, the expansion into its files,
// once. On the production hub that is ~15k checkpoint edges plus ~1M expansion
// rows for 331 distinct trees, where one edge per file per checkpoint was 16.2M
// rows -- the difference between a backfill that completes on a full disk and
// one that hits SQLITE_FULL and never does.
//
// A schema-1 manifest inlines the complete file list, and it ALSO names the
// tree: the base hub wrote Workspace.TreeSHA256 and Files together, and the
// row carries root_tree_sha256 besides. The tree is what gets expanded, once,
// exactly as for schema 2. The inlined list is used only as a fallback, as one
// edge per file under the checkpoint, when no tree of the checkpoint could be
// expanded -- none named, or the tree blob missing or unparseable. Recording
// the inlined list unconditionally would have written the 16.2M rows this
// design exists to avoid, on exactly the hub it targets: most of its 5,211
// manifests predate schema 2.
//
// It is:
//
//   - idempotent: every insert is INSERT OR IGNORE on the composite key, and a
//     row with any edge is not selected in the first place.
//   - honest about what it could not read. A manifest or tree that is MISSING
//     (ENOENT, with the checkpoint store present) is permanent: the blobs it
//     listed cannot be recovered by retrying, and they are counted and logged.
//     A manifest or tree that could not be READ for any other reason -- EMFILE,
//     EIO, a store that is not mounted -- is transient, and that checkpoint is
//     given NO edges this cycle: recording a partial set would take it off the
//     work list with its tree unexpanded, and the next sweep would unlink the
//     files. Left edge-less, it keeps the gate closed and is retried for free
//     next cycle. An unparseable file is permanent too (retrying cannot mend
//     it), and is treated like a missing one.
//
// Only 'ready' and 'skipped' rows are traversed, because they are the only
// statuses that hold anything. A 'compacted' or 'failed' row released its blobs
// by definition, and a 'creating' row's edges come from its plan, with the
// sweep's grace window covering the upload in between.
func (s *Server) backfillCheckpointBlobRefs() error {
	started := time.Now()
	rows, err := s.db.Query(`SELECT id, COALESCE(manifest_path,''), COALESCE(manifest_sha256,''),
		COALESCE(root_tree_sha256,''), COALESCE(message_tree_sha256,''), COALESCE(workspace_tree_sha256,'')
		 FROM claw_checkpoints WHERE ` + unreferencedCheckpointsWhere)
	if err != nil {
		return fmt.Errorf("list checkpoints to backfill: %w", err)
	}
	defer rows.Close()
	type row struct{ id, manifestPath, manifestSHA, rootSHA, msgSHA, workspaceSHA string }
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.manifestPath, &r.manifestSHA, &r.rootSHA, &r.msgSHA, &r.workspaceSHA); err != nil {
			return err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	// A checkpoint store that is not there at all is not "every blob is
	// missing": it is a blob root that is not mounted, and recording edges
	// against it would take these rows off the work list with their trees
	// unexpanded.
	blobRoot := filepath.Join(checkpointsRoot(), "blobs", "sha256")
	if _, err := os.Stat(blobRoot); err != nil {
		return fmt.Errorf("blob store %s is not readable; %d checkpoint(s) left without references: %w", blobRoot, len(pending), err)
	}

	var recorded, edges, expansions, treeRows, fallbacks int
	var badManifests, missingTrees, badTrees, unreadable, unrecordable int
	trees := map[string]treeOutcome{}
	errLog := &retentionErrorLog{phase: "blob reference backfill"}
	defer errLog.flush()

	// Paced like every other bulk loop in this file, in two places. Checkpoint
	// edges are grouped retentionCheckpointBatch rows to a transaction -- or
	// fewer, when schema-1 fallbacks make the batch heavy -- with the pacer's
	// pause after each commit; and a tree expansion, which is one transaction
	// of up to a workspace's worth of rows, is followed by a pause of its own.
	// A tree is always committed before the checkpoints that name it: an edge
	// to a tree with no expansion would let the sweep unlink that tree's files.
	//
	// The budget spreads a first backfill across cycles. The sweep declines
	// while any row is still edge-less (see unreferencedCheckpoints) and says
	// how much is outstanding, so a backfill that stops here costs a postponed
	// sweep and nothing else.
	pacer := newRetentionPacer("blob reference backfill", retentionBudgetDeadline())
	var batch []checkpointEdges
	batchRows := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.insertCheckpointBlobRefsBatch(batch); err != nil {
			return fmt.Errorf("backfill references (%d of %d recorded): %w", recorded, len(pending), err)
		}
		recorded += len(batch)
		edges += batchRows
		n := len(batch)
		batch, batchRows = batch[:0], 0
		return pacer.yield(n)
	}
	stopped := false
	for _, r := range pending {
		if stopped {
			break
		}
		digests := map[string]struct{}{}
		rowTrees := map[string]struct{}{}
		add := func(sha string, isTree bool) {
			clean := normalizeBlobDigest(sha)
			if clean == "" {
				return
			}
			digests[clean] = struct{}{}
			if isTree {
				rowTrees[clean] = struct{}{}
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
		var inlined []string
		if data, err := os.ReadFile(manifestPath); err != nil {
			if !os.IsNotExist(err) {
				unreadable++
				errLog.add("backfill: cannot read manifest %s of checkpoint %s (left without references; will retry next cycle): %v",
					manifestPath, shortID(r.id), err)
				continue
			}
		} else {
			var manifest struct {
				checkpointManifest
				Files []types.CheckpointFile `json:"files"`
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				badManifests++
				errLog.add("backfill: cannot parse manifest %s of checkpoint %s; its row digests are still recorded: %v",
					manifestPath, shortID(r.id), err)
			} else {
				add(manifest.Workspace.TreeSHA256, true)
				add(manifest.Messages.BlobSHA256, false)
				for _, f := range manifest.Files {
					if clean := normalizeBlobDigest(f.SHA256); clean != "" {
						inlined = append(inlined, clean)
					}
				}
			}
		}

		// The one step whose omission would be catastrophic: under schema 2 the
		// per-file digests exist ONLY inside the tree blob. Each distinct tree
		// is expanded once for the whole pass; the probe inside insertTreeBlobRefs
		// makes a tree already expanded by an earlier cycle free.
		expandedAny, transient := false, false
		for tree := range rowTrees {
			outcome, seen := trees[tree]
			if !seen {
				outcome = s.expandTreeForBackfill(tree, r.id, errLog)
				trees[tree] = outcome
				switch outcome.state {
				case treeExpanded:
					expansions++
					treeRows += outcome.rows
					if pacer.yield(0) != nil {
						stopped = true
					}
				case treeMissing:
					missingTrees++
				case treeUnparseable:
					badTrees++
				}
			}
			switch outcome.state {
			case treeExpanded, treeKnown:
				expandedAny = true
			case treeUnreadable:
				transient = true
			}
			if stopped {
				break
			}
		}
		if stopped {
			// This row's remaining trees were not expanded, so it gets no edges
			// this cycle; it stays on the work list. The rows before it that
			// are still in the batch are complete and are flushed below.
			break
		}
		if transient {
			unreadable++
			continue
		}
		if !expandedAny && len(inlined) > 0 {
			// Nothing else records these files: fall back to the schema-1 list,
			// as edges of the checkpoint itself.
			fallbacks++
			for _, sha := range inlined {
				digests[sha] = struct{}{}
			}
		}

		list := make([]string, 0, len(digests))
		for d := range digests {
			list = append(list, d)
		}
		if len(list) == 0 {
			// Unreachable while the gate selects exactly what the writer
			// accepts (unreferencedCheckpointsWhere); kept so that a drift
			// between the two is a named, counted phase error rather than a
			// row that is "processed" every cycle and never leaves the work
			// list. Such a row is not counted as recorded.
			unrecordable++
			errLog.add("backfill: checkpoint %s names no recordable digest (manifest=%q root=%q message=%q workspace=%q); it stays on the work list and the gate/writer disagreement must be fixed in code",
				shortID(r.id), r.manifestSHA, r.rootSHA, r.msgSHA, r.workspaceSHA)
			continue
		}
		batch = append(batch, checkpointEdges{id: r.id, digests: list})
		batchRows += len(list)
		// A database error here (as opposed to a file error above) aborts the
		// pass; the rows not reached are still edge-less and the next cycle
		// resumes with them.
		if len(batch) >= retentionCheckpointBatch || batchRows >= retentionDeleteBatch {
			if err := flush(); err != nil {
				if errors.Is(err, errRetentionBudget) {
					stopped = true
					break
				}
				return err
			}
		}
	}
	if err := flush(); err != nil && !errors.Is(err, errRetentionBudget) {
		return err
	}

	budget := ""
	if stopped {
		budget = "; stopped at the cycle budget, the rest goes to the next cycle"
	}
	log.Printf("[retention] blob reference backfill in %s: %d of %d checkpoint(s) given references, %d reference(s), %d tree expansion(s) (%d rows), %d schema-1 fallback(s); unparseable_manifests=%d missing_trees=%d unparseable_trees=%d unrecordable=%d (blobs listed only by a missing or unparseable tree are unreferenced and will be swept)%s",
		time.Since(started).Round(time.Millisecond), recorded, len(pending), edges, expansions, treeRows, fallbacks, badManifests, missingTrees, badTrees, unrecordable, budget)
	if unrecordable > 0 {
		return fmt.Errorf("backfill could not record %d checkpoint(s) that the gate selected: they name no digest the writer accepts, so the sweep declines on them every cycle; the gate predicate and normalizeBlobDigest have diverged", unrecordable)
	}
	if unreadable > 0 {
		return fmt.Errorf("backfill left %d checkpoint(s) without references: a manifest or tree could not be read for a reason other than being absent; the sweep declines until the next cycle records them", unreadable)
	}
	return nil
}

// treeExpansion is what the backfill found when it tried to expand one tree.
type treeExpansion int

const (
	treeKnown       treeExpansion = iota // already expanded by an earlier plan or cycle
	treeExpanded                         // expanded by this pass
	treeMissing                          // the tree blob is gone: permanent
	treeUnparseable                      // the tree blob is corrupt: permanent
	treeUnreadable                       // could not be read or recorded: transient, retry next cycle
)

type treeOutcome struct {
	state treeExpansion
	rows  int // expansion rows written, for treeExpanded
}

// expandTreeForBackfill reads one tree blob and records its expansion.
func (s *Server) expandTreeForBackfill(tree, checkpointID string, errLog *retentionErrorLog) treeOutcome {
	data, err := os.ReadFile(checkpointBlobPath(tree))
	if os.IsNotExist(err) {
		errLog.add("backfill: tree blob %s of checkpoint %s is missing; the files it listed cannot be recorded and will be swept",
			tree, shortID(checkpointID))
		return treeOutcome{state: treeMissing}
	}
	if err != nil {
		errLog.add("backfill: cannot read tree blob %s of checkpoint %s (will retry next cycle): %v", tree, shortID(checkpointID), err)
		return treeOutcome{state: treeUnreadable}
	}
	var files []types.CheckpointFile
	if err := json.Unmarshal(data, &files); err != nil {
		errLog.add("backfill: cannot parse tree blob %s of checkpoint %s: %v", tree, shortID(checkpointID), err)
		return treeOutcome{state: treeUnparseable}
	}
	n, err := s.insertTreeBlobRefs(tree, files)
	if err != nil {
		errLog.add("backfill: record expansion of tree %s (will retry next cycle): %v", tree, err)
		return treeOutcome{state: treeUnreadable}
	}
	if n == 0 {
		return treeOutcome{state: treeKnown}
	}
	return treeOutcome{state: treeExpanded, rows: n}
}

// insertCheckpointBlobRefs records edges for one checkpoint in its own
// transaction.
func (s *Server) insertCheckpointBlobRefs(checkpointID string, digests []string) error {
	return s.insertCheckpointBlobRefsBatch([]checkpointEdges{{id: checkpointID, digests: digests}})
}

// checkpointEdges is the reference edges one checkpoint holds.
type checkpointEdges struct {
	id      string
	digests []string
}

// insertCheckpointBlobRefsBatch records the edges of one batch of checkpoints
// in one transaction.
func (s *Server) insertCheckpointBlobRefsBatch(batch []checkpointEdges) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, cp := range batch {
		if err := addCheckpointBlobRefsTx(tx, cp.id, cp.digests); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// insertTreeBlobRefs records one tree's expansion in its own transaction and
// reports how many rows it wrote -- zero when the tree was already known.
func (s *Server) insertTreeBlobRefs(treeSHA string, files []types.CheckpointFile) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var before, after int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tree_blob_refs WHERE tree_sha256=?`, normalizeBlobDigest(treeSHA)).Scan(&before); err != nil {
		return 0, err
	}
	if err := addTreeBlobRefsTx(tx, treeSHA, files); err != nil {
		return 0, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tree_blob_refs WHERE tree_sha256=?`, normalizeBlobDigest(treeSHA)).Scan(&after); err != nil {
		return 0, err
	}
	return after - before, tx.Commit()
}

// pendingBackfillEstimate says how many edges the backfill still has to write
// for the checkpoints that hold blobs without an edge, for the sweep's DECLINED
// line: those checkpoints × their row digests, plus the files_count of every
// distinct root tree among them not yet expanded. It is an estimate --
// files_count is the row's own summary -- but it is the number an operator
// needs to tell "still working through it" from "will never finish on this
// disk".
func (s *Server) pendingBackfillEstimate() (checkpoints, trees, edges int64, err error) {
	if err = s.db.QueryRow(
		`SELECT COUNT(*) FROM claw_checkpoints WHERE ` + unreferencedCheckpointsWhere).Scan(&checkpoints); err != nil {
		return 0, 0, 0, err
	}
	var files int64
	if err = s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(files_count),0) FROM (
		   SELECT root_tree_sha256, MAX(files_count) AS files_count FROM claw_checkpoints
		    WHERE `+unreferencedCheckpointsWhere+` AND root_tree_sha256 <> ''
		      AND root_tree_sha256 NOT IN (SELECT tree_sha256 FROM tree_blob_refs)
		    GROUP BY root_tree_sha256)`).Scan(&trees, &files); err != nil {
		return 0, 0, 0, err
	}
	return checkpoints, trees, checkpoints*3 + files, nil
}

// pruneUnreferencedTreeBlobRefs deletes the expansions of trees no checkpoint
// edge names any more. It is hygiene: the keep set only expands referenced
// trees, so a stale expansion pins nothing, and this exists to keep the table
// from growing by one dead tree per compacted or expired workspace forever.
//
// The unit of deletion is one whole tree per statement, not a fixed row count.
// That is what makes addTreeBlobRefsTx's "any row means all rows" probe safe:
// the NOT EXISTS is re-evaluated inside the DELETE, under the same write lock a
// plan's transaction takes, so a plan that references the tree first leaves the
// expansion untouched and a plan that arrives after finds no rows and writes
// it again. A row-count batch could delete half a tree between a plan's probe
// and its commit. A tree is at most one workspace's file list, so the lock hold
// is bounded by the same thing that bounds a plan.
//
// The write lock is released between trees, and the deadline stops the pass
// rather than letting a first cycle over years of dead trees run unbounded;
// the rest goes to the next cycle.
// treeGCHook runs once per tree between the gc's SELECT and its DELETE. It is
// nil in production and exists solely so a test can commit a plan on that tree
// at exactly that point -- the interleaving the DELETE's NOT EXISTS re-check
// exists for, which no test that plans before or after the gc can reach.
var treeGCHook func(tree string)

func (s *Server) pruneUnreferencedTreeBlobRefs(pacer *retentionPacer) (int64, error) {
	var total int64
	for {
		trees, err := s.unreferencedTrees()
		if err != nil {
			return total, err
		}
		if len(trees) == 0 {
			return total, nil
		}
		for _, tree := range trees {
			if treeGCHook != nil {
				treeGCHook(tree)
			}
			// The NOT EXISTS here is not a repetition of the SELECT above: it
			// is the whole safety argument. The list was read outside any
			// transaction, and a plan may commit an edge to this tree between
			// that read and this DELETE. The plan's addTreeBlobRefsTx probed
			// "any row means all rows" and wrote nothing, so a DELETE that
			// trusted the stale list would leave a referenced tree with no
			// expansion, and the next sweep would unlink that workspace.
			// Re-evaluated under the write lock, the predicate sees the edge
			// and deletes nothing. TestTreeGCReChecksTheEdgeUnderTheWriteLock
			// plans a checkpoint at exactly this point.
			res, err := s.db.Exec(
				`DELETE FROM tree_blob_refs WHERE tree_sha256=?
				    AND NOT EXISTS (SELECT 1 FROM checkpoint_blob_refs r WHERE r.sha256=?)`, tree, tree)
			if err != nil {
				return total, fmt.Errorf("delete expansion of tree %s (aborted after %d rows): %w", tree, total, err)
			}
			n, _ := res.RowsAffected()
			total += n
			if err := pacer.yield(int(n)); err != nil {
				return total, nil
			}
		}
		if len(trees) < retentionDeleteBatch {
			return total, nil
		}
	}
}

// unreferencedTrees lists one batch of trees whose expansion no checkpoint
// edge names. Its own function so the cursor is closed by a defer rather than
// by hand on each exit of the gc's loop.
func (s *Server) unreferencedTrees() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT tree_sha256 FROM tree_blob_refs
		  WHERE tree_sha256 NOT IN (SELECT sha256 FROM checkpoint_blob_refs)
		  LIMIT ?`, retentionDeleteBatch)
	if err != nil {
		return nil, fmt.Errorf("list unreferenced trees: %w", err)
	}
	defer rows.Close()
	var trees []string
	for rows.Next() {
		var tree string
		if err := rows.Scan(&tree); err != nil {
			return nil, err
		}
		trees = append(trees, tree)
	}
	return trees, rows.Err()
}

// referencedBlobDigests builds the keep set:
//
//	SELECT sha256 FROM checkpoint_blob_refs
//	UNION
//	SELECT t.sha256 FROM tree_blob_refs t
//	 WHERE t.tree_sha256 IN (SELECT sha256 FROM checkpoint_blob_refs)
//
// evaluated as one read of the checkpoint edges followed by one indexed probe
// of tree_blob_refs per distinct digest they name. The IN-subquery is over ALL
// checkpoint edges rather than a root-tree column so that the edge table stays
// the single authority -- it is what gets deleted transactionally with the
// status change; a digest that is not a tree simply matches no expansion.
//
// It is done in two steps rather than as the one statement above because of
// released: in a dry run the phases mutate nothing, so the sweeper has to be
// told which checkpoints' edges a real cycle would already have dropped, and
// that filter belongs on the edge read, before the trees are expanded. The two
// reads are not one snapshot, and do not need to be: a plan committing between
// them adds edges the claim window already protects (it is armed before either
// read), and a release committing between them can only remove a tree whose
// files that release made collectable.
//
// The edge read is one scan of two narrow columns, at most three rows per
// checkpoint. The probes are one B-tree lookup per distinct digest, of which
// only the tree digests return anything.
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(referenced))
	for sha := range referenced {
		roots = append(roots, sha)
	}
	stmt, err := s.db.Prepare(`SELECT sha256 FROM tree_blob_refs WHERE tree_sha256=?`)
	if err != nil {
		return nil, fmt.Errorf("read tree blob references: %w", err)
	}
	defer stmt.Close()
	for _, root := range roots {
		if err := func() error {
			rows, err := stmt.Query(root)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var sha string
				if err := rows.Scan(&sha); err != nil {
					return err
				}
				if clean := normalizeBlobDigest(sha); clean != "" {
					referenced[clean] = struct{}{}
				}
			}
			return rows.Err()
		}(); err != nil {
			return nil, fmt.Errorf("expand tree %s: %w", root, err)
		}
	}
	return referenced, nil
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
	result, err := s.sweepCheckpointBlobsResult(dryRun, released)
	return result.removed, result.bytes, result.itemErrors, err
}

// blobSweepResult is what one sweep did, or -- declined -- why it did nothing.
type blobSweepResult struct {
	removed      int
	bytes        int64
	itemErrors   int
	declined     bool
	unreferenced int64 // checkpoints holding blobs without an edge, when declined
}

func (s *Server) sweepCheckpointBlobsResult(dryRun bool, released []string) (blobSweepResult, error) {
	var result blobSweepResult
	// Arm the interlock BEFORE the referenced set is read, and disarm it only
	// after the walk. Every plan that commits its edges before this point is
	// visible to the query below; every plan that commits after it records into
	// the in-memory window instead. There is no instant in between where a
	// reference is invisible to both.
	s.beginBlobClaimWindow()
	defer s.endBlobClaimWindow()

	// The gate. A checkpoint that holds blobs and has no edge says "nothing
	// references these", which is the same sentence as "these are garbage" and
	// must never be acted on -- whether the row predates reference counting,
	// the backfill crashed before reaching it, or it was written by a build
	// rolled back to in between. See unreferencedCheckpoints.
	unreferenced, unreferencedIDs, err := s.unreferencedCheckpoints()
	if err != nil {
		return result, fmt.Errorf("check for checkpoints without blob references: %w", err)
	}
	if unreferenced > 0 {
		result.declined = true
		result.unreferenced = unreferenced
		// Say how much is outstanding, not only that something is. The line
		// that reads the same on the first cycle after an upgrade and on the
		// hundredth cycle of a backfill that will never fit on this disk is a
		// line nobody can act on.
		if checkpoints, trees, edges, err := s.pendingBackfillEstimate(); err != nil {
			log.Printf("[retention] blob sweep DECLINED: %d checkpoint(s) hold blobs without a reference edge (e.g. %s), so an absent reference does not yet mean an unreferenced blob. No blob will be deleted until the backfill has given them edges; see the preceding backfill error, if any. (estimating the outstanding work failed: %v)", unreferenced, strings.Join(unreferencedIDs, ", "), err)
		} else {
			log.Printf("[retention] blob sweep DECLINED: %d checkpoint(s) hold blobs without a reference edge (e.g. %s), so an absent reference does not yet mean an unreferenced blob. No blob will be deleted until the backfill has given them edges; see the preceding backfill error, if any. Outstanding: %d checkpoint(s), %d unexpanded tree(s), ~%d edge(s) to write.", unreferenced, strings.Join(unreferencedIDs, ", "), checkpoints, trees, edges)
		}
		return result, nil
	}

	referenced, err := s.referencedBlobDigests(released)
	if err != nil {
		return result, fmt.Errorf("not sweeping: %w", err)
	}
	blobRoot := filepath.Join(checkpointsRoot(), "blobs", "sha256")
	if _, err := os.Stat(blobRoot); os.IsNotExist(err) {
		return result, nil
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
	errLog := &retentionErrorLog{phase: "blob sweep"}
	defer errLog.flush()
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
			errLog.add("remove blob %s: %v", path, err)
			return nil
		} else if !removedFile {
			return nil
		}
		removed++
		freed += size
		return nil
	})
	result.removed, result.bytes, result.itemErrors = removed, freed, itemErrors
	if err != nil {
		return result, fmt.Errorf("walk blobs (aborted after %d blobs, %d bytes): %w", removed, freed, err)
	}
	if dryRun {
		if line := sample.describe("blob(s)"); line != "" {
			log.Printf("[retention] dry_run: would sweep %s totalling %d bytes", line, freed)
		}
	} else {
		pruneEmptyDirs(blobRoot)
	}
	return result, nil
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
	return !s.claimBlobPresent(sha)
}

// claimBlobPresent is the claim half of the interlock as a primitive: record
// the digest into the window, then stat, then refresh the mtime of a blob that
// is there -- all under ONE hold of the lock. It reports whether the blob is on
// disk.
//
// Every path that answers "the hub already has this blob, do not write it"
// goes through here -- the plan handler, the blob upload handler and the
// message-blob writer -- because each of them is a dedup, and a dedup outside
// the interlock is a bare stat racing the walker's unlink. The message blob was
// the last one: it is the one referenced digest a plan never names, so its
// finalize could commit an edge to a file the sweep had just removed.
//
// The touch is inside the hold, not after it, for the message blob. The plan
// and upload paths have their edges committed before they get here, so for
// them the mtime is a second line of defence behind the edge; the message
// blob's edge is committed later, in the finalize transaction, so between this
// claim and that commit the fresh mtime is the ONLY thing protecting it from a
// sweep that armed its window after the claim. With the touch outside the
// lock, beginBlobClaimWindow could run between the Unlock and the Chtimes:
// that sweep records no claim, reads a keep set without the edge, and can
// lstat the file with its months-old mtime. Inside the lock, a sweep's arm
// serialises strictly before the claim (and sees it in the window) or strictly
// after the touch (and sees the fresh mtime). Chtimes is one syscall.
func (s *Server) claimBlobPresent(sha string) bool {
	path := checkpointBlobPath(sha)
	if path == "" {
		// Not a digest, so nothing under the blob root can correspond to it,
		// and nothing outside the blob root may be stat-ed or touched for it.
		return false
	}
	s.blobClaimMu.Lock()
	defer s.blobClaimMu.Unlock()
	if s.blobClaimWindow != nil {
		if digest := normalizeBlobDigest(sha); digest != "" {
			s.blobClaimWindow[digest] = struct{}{}
		}
	}
	if _, err := os.Stat(path); err != nil {
		return false
	}
	if err := touchCheckpointBlob(path); err != nil {
		reportBlobTouchFailure(path, err)
	}
	return true
}

// blobTouchFailureOnce bounds the touch-failure log to one line per process.
// A store the hub cannot Chtimes -- restored under another owner, mounted with
// the wrong options -- fails the touch for every blob of every plan, and the
// plan handler is called with ~12k digests per checkpoint; one line that
// names the condition is what an operator can act on, twelve thousand an
// hour is what journald drops.
var blobTouchFailureOnce sync.Once

// reportBlobTouchFailure makes a failed touch visible. The failure is not
// worth failing a checkpoint over -- the edge, once committed, protects the
// blob regardless -- but it must not be invisible either: on a store where
// every touch fails, a deduplicated blob keeps a months-old mtime, and for
// the message blob that mtime is the only protection between its claim and
// its finalize.
func reportBlobTouchFailure(path string, err error) {
	blobTouchFailureOnce.Do(func() {
		log.Printf("[checkpoint] cannot refresh the mtime of reused blob %s: %v (the sweep's grace window does not protect reused blobs on this store; further failures are not logged)", path, err)
	})
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
