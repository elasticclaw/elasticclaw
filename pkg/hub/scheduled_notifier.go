package hub

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type scheduledReportBuilder func(ctx context.Context, s *Server) (*notify.Message, bool, error)

var scheduledReportRegistry = struct {
	sync.RWMutex
	builders map[string]scheduledReportBuilder
}{builders: make(map[string]scheduledReportBuilder)}

func registerScheduledReport(name string, builder scheduledReportBuilder) {
	scheduledReportRegistry.Lock()
	defer scheduledReportRegistry.Unlock()
	scheduledReportRegistry.builders[name] = builder
}

// ScheduledReportSupported reports whether name is a registered scheduled report.
func ScheduledReportSupported(name string) bool {
	scheduledReportRegistry.RLock()
	defer scheduledReportRegistry.RUnlock()
	_, ok := scheduledReportRegistry.builders[name]
	return ok
}

// scheduledReportNames lists every registered report, sorted. It backs the
// error text a rejected report name gets and the doctor check for a schedule
// naming a report this build does not carry.
func scheduledReportNames() []string {
	scheduledReportRegistry.RLock()
	defer scheduledReportRegistry.RUnlock()
	names := make([]string, 0, len(scheduledReportRegistry.builders))
	for name := range scheduledReportRegistry.builders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func scheduledReport(name string) (scheduledReportBuilder, bool) {
	scheduledReportRegistry.RLock()
	defer scheduledReportRegistry.RUnlock()
	builder, ok := scheduledReportRegistry.builders[name]
	return builder, ok
}

// scheduledSendTimeout bounds each report build and each destination send,
// mirroring the lifecycle notifier's 30s send cap: one slow destination must
// not delay every schedule behind it in the tick, and a hanging send would
// otherwise sit in stopScheduledNotifier's bounded wait, widening the
// shutdown duplicate-send window.
const scheduledSendTimeout = 30 * time.Second

// scheduledMaxTransientFailures bounds consecutive transient send failures
// for one (schedule, destination) slot, analogous to
// lifecycleMaxTransientFailures. Unclassified errors default to transient, so
// a permanently-undeliverable report a provider failed to classify would
// otherwise re-post the identical message every minute until the next slot.
// Unlike the lifecycle streak this one lives in memory (ticks are serial):
// losing it to a restart costs at most one fresh retry budget for the same
// slot, never a wedged cursor.
const scheduledMaxTransientFailures = 60

// startScheduledNotifier launches the background loop that fires scheduled
// reports. It stops when stopScheduledNotifier is called (graceful shutdown,
// before the DB closes).
func (s *Server) startScheduledNotifier() {
	stop := make(chan struct{})
	done := make(chan struct{})
	s.scheduledNotifierStop, s.scheduledNotifierDone = stop, done
	// tickCtx mirrors the stop channel as a context so the builds and sends
	// of a tick in flight at shutdown are cancelled instead of running out
	// stopScheduledNotifier's bounded wait.
	tickCtx, cancelTicks := context.WithCancel(context.Background())
	s.safeGo("scheduled notifier", func() {
		defer close(done)
		defer cancelTicks()
		go func() { <-stop; cancelTicks() }()
		for {
			timer := time.NewTimer(time.Minute)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			// Run the tick inline (not via safeGo) so ticks never overlap and
			// shutdown can wait for the one in flight. A panic is contained to
			// this iteration.
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[notify] scheduled notifier tick panic: %v\n%s", r, debug.Stack())
					}
				}()
				s.scheduledNotifierTick(tickCtx, s.nowFunc())
			}()
		}
	})
}

// stopScheduledNotifier stops the tick loop and waits (bounded) for an
// in-flight tick to finish, for the same reason stopLifecycleNotifier does:
// a tick in flight at shutdown can complete an external Slack send and then
// fail the dedupe-state upsert against a closed DB, re-sending the same slot
// after restart. The timeout keeps a pathologically slow tick from wedging
// shutdown, accepting the duplicate-send window in that case.
func (s *Server) stopScheduledNotifier(timeout time.Duration) {
	if s.scheduledNotifierStop == nil {
		return
	}
	close(s.scheduledNotifierStop)
	s.scheduledNotifierStop = nil
	select {
	case <-s.scheduledNotifierDone:
	case <-time.After(timeout):
		log.Printf("[notify] scheduled notifier tick still running after %v; shutting down anyway", timeout)
	}
}

func (s *Server) scheduledNotifierTick(ctx context.Context, nowAt time.Time) {
	cfg := s.notificationsConfig()
	if cfg == nil {
		// A removed notifications block must still clear the scheduled dedupe
		// rows, exactly as removing every schedule does: nothing else prunes
		// them, and a block re-added days later with an identical schedule
		// would otherwise inherit the stale row and replay a slot from the
		// removal window instead of seeding on first sight.
		s.pruneScheduledState(nil)
		return
	}
	// Gate on validity like the lifecycle tick does: nothing in the load path
	// validates a hand-written hub.yaml, and an invalid scheduled block can
	// misbehave without any error surfacing — two schedules sharing an id map
	// to one dedupe row and mutually reseed each other every tick, delivering
	// nothing. Only the config this tick consumes is judged (notifiers plus
	// the scheduled block, under this tick's own warning key): a defect in the
	// lifecycle block pauses lifecycle alerts, never scheduled reports.
	if err := types.ValidateScheduledNotificationsConfig(cfg); err != nil {
		s.logPollWarningOnce("scheduled-config", "[notify] invalid notifications config — scheduled reports paused: %v", err)
		return
	}
	s.clearPollWarning("scheduled-config")
	s.pruneScheduledState(cfg.Scheduled)
	for _, schedule := range cfg.Scheduled {
		s.runScheduledDelivery(ctx, nowAt, cfg, schedule)
	}
}

// runScheduledDelivery seeds, builds, and sends one schedule's due slot. Its
// recover contains a deterministic panic (a report-builder bug) to THIS
// schedule: the tick-level recover alone would unwind the whole cfg.Scheduled
// loop, permanently starving every schedule after the panicking one in config
// order. The panicking schedule's still-pending destinations get their slot
// burned — analogous to the transient-failure cap — so the panic does not also
// log a stack trace every minute for the rest of the slot.
func (s *Server) runScheduledDelivery(ctx context.Context, nowAt time.Time, cfg *types.NotificationsConfig, schedule types.ScheduledNotificationConfig) {
	var (
		slot    time.Time
		digest  string
		pending []string
	)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[notify] scheduled report %q panicked — burning its %s slot: %v\n%s", schedule.Report, slot.Format(time.RFC3339), r, debug.Stack())
			// Re-marking a destination already delivered this slot is an
			// idempotent upsert of the same value, so the whole pending set
			// can be burned without tracking how far the send loop got.
			for _, via := range pending {
				s.setScheduledLastFired(schedule.ID, via, slot, digest)
			}
		}
	}()
	slot, ok := scheduledNotificationSlot(nowAt, schedule)
	if !ok {
		return
	}
	paused := schedule.Enabled != nil && !*schedule.Enabled
	digest = scheduledSlotDigest(schedule)
	for _, via := range schedule.Via {
		// TrimSpace matches the lifecycle runtime and doctor, both of which
		// resolve the trimmed name: an untrimmed via from a hand-written
		// hub.yaml would otherwise miss the notifier map here and burn one
		// slot per day as "permanently failed" while doctor reports it green.
		via := strings.TrimSpace(via)
		fired, firedDigest, found, err := s.scheduledLastFired(schedule.ID, via)
		if err != nil {
			log.Printf("[notify] read scheduled state for %s via %s: %v", schedule.ID, via, err)
			continue
		}
		// A destination with no state row is one this hub has never
		// delivered to — a schedule (or via) just created or first seen.
		// The slot search reaches up to 8 days back, so firing here would
		// replay a slot from before the schedule existed; seed the row to
		// the current slot instead and deliver from the next one on. A
		// row whose slot digest no longer matches was written under a
		// pre-edit at/timezone/weekdays definition: firing against it
		// would post an off-schedule send the moment the edit is saved,
		// so it is reseeded like a first sight and the edited schedule
		// delivers from its next slot on. A paused schedule advances the
		// same way: slots that pass while it is paused are skipped, not
		// queued up for the re-enable.
		edited := firedDigest != digest
		if !found || edited || paused {
			if !found || edited || fired.Before(slot) {
				s.setScheduledLastFired(schedule.ID, via, slot, digest)
			}
			continue
		}
		if fired.Before(slot) {
			pending = append(pending, via)
		}
	}
	if len(pending) == 0 {
		return
	}
	builder, found := scheduledReport(schedule.Report)
	if !found {
		log.Printf("[notify] scheduled report %q is not registered", schedule.Report)
		return
	}
	buildCtx, cancelBuild := context.WithTimeout(ctx, scheduledSendTimeout)
	message, hasReport, err := builder(buildCtx, s)
	cancelBuild()
	if err != nil {
		// A failing build gets the same bounded retry budget as a failing
		// send: fired never advances on a build error, so a persistently
		// erroring builder (a wedged DB, an empty tenants table) would
		// otherwise rebuild and log the identical line every minute for the
		// life of the process.
		if count := s.noteScheduledTransientFailure(schedule.ID, scheduledBuildFailureVia, slot); count < scheduledMaxTransientFailures {
			log.Printf("[notify] build scheduled report %q (failure %d/%d, will retry): %v", schedule.Report, count, scheduledMaxTransientFailures, err)
			return
		}
		log.Printf("[notify] giving up on scheduled report %q after %d consecutive build failures: %v", schedule.Report, scheduledMaxTransientFailures, err)
		s.clearScheduledTransientFailures(schedule.ID, scheduledBuildFailureVia)
		for _, via := range pending {
			s.setScheduledLastFired(schedule.ID, via, slot, digest)
		}
		return
	}
	s.clearScheduledTransientFailures(schedule.ID, scheduledBuildFailureVia)
	if !hasReport {
		for _, via := range pending {
			s.setScheduledLastFired(schedule.ID, via, slot, digest)
		}
		return
	}
	for _, via := range pending {
		notifier, err := s.notifierFor(via, cfg.Notifiers[via], s.hubSecretResolver())
		constructionFailed := err != nil
		if err == nil {
			sendCtx, cancelSend := context.WithTimeout(ctx, scheduledSendTimeout)
			_, err = notifier.Send(sendCtx, *message)
			cancelSend()
		}
		if err == nil {
			s.clearScheduledTransientFailures(schedule.ID, via)
			s.setScheduledLastFired(schedule.ID, via, slot, digest)
			continue
		}
		// A notifier that cannot be constructed right now (a secret
		// mid-rotation, a config half-applied) is retried on the transient
		// budget, mirroring the lifecycle notifier's hold-until-buildable —
		// but bounded by the cap, so a genuinely dead notifier still burns
		// the slot instead of holding it forever.
		if constructionFailed || notify.Classify(err) == notify.ErrorTransient {
			if count := s.noteScheduledTransientFailure(schedule.ID, via, slot); count < scheduledMaxTransientFailures {
				log.Printf("[notify] send scheduled report %q via %s (failure %d/%d, will retry): %v", schedule.Report, via, count, scheduledMaxTransientFailures, err)
				continue
			}
			log.Printf("[notify] giving up on scheduled report %q via %s after %d consecutive transient failures: %v", schedule.Report, via, scheduledMaxTransientFailures, err)
		} else {
			log.Printf("[notify] permanently failed scheduled report %q via %s: %v", schedule.Report, via, err)
		}
		s.clearScheduledTransientFailures(schedule.ID, via)
		s.setScheduledLastFired(schedule.ID, via, slot, digest)
	}
}

type scheduledFailureStreak struct {
	slot  time.Time
	count int
}

// scheduledBuildFailureVia keys a schedule's report-build failure streak in
// scheduledTransientFailures. The report is built once per tick per schedule,
// not per destination, so the build streak is keyed by the schedule alone; the
// NUL prefix cannot collide with a real via, which validation bans control
// characters from.
const scheduledBuildFailureVia = "\x00build"

// noteScheduledTransientFailure records one more consecutive transient
// failure for the destination's current slot and returns the streak length.
// A slot change resets the streak, so stale entries can never charge a later
// slot. The map is shared between the serial tick goroutine and the settings
// PATCH handler (pruneScheduledState), hence the lock.
func (s *Server) noteScheduledTransientFailure(id, via string, slot time.Time) int {
	s.scheduledFailureMu.Lock()
	defer s.scheduledFailureMu.Unlock()
	if s.scheduledTransientFailures == nil {
		s.scheduledTransientFailures = map[string]scheduledFailureStreak{}
	}
	key := scheduledStateKey(id, via)
	streak := s.scheduledTransientFailures[key]
	if !streak.slot.Equal(slot) {
		streak = scheduledFailureStreak{slot: slot}
	}
	streak.count++
	s.scheduledTransientFailures[key] = streak
	return streak.count
}

func (s *Server) clearScheduledTransientFailures(id, via string) {
	s.scheduledFailureMu.Lock()
	defer s.scheduledFailureMu.Unlock()
	delete(s.scheduledTransientFailures, scheduledStateKey(id, via))
}

// pruneScheduledState drops the dedupe rows of (schedule, destination) pairs
// that are no longer configured, for the same reason pruneLifecycleRouteState
// drops route cursors: nothing else clears them, so a schedule id deleted and
// later re-created would resume from its stale row and replay a slot that
// belongs to the removal window, and orphaned rows would otherwise accumulate
// forever. Rows in a superseded key format fall out here too, as keys no
// configured pair produces.
//
// Beyond the minute tick, the settings PATCH handler calls this after a
// persisted notifications update: a schedule deleted and re-added under the
// same id within one tick interval would otherwise inherit the stale row and
// replay a slot from before the new schedule existed.
func (s *Server) pruneScheduledState(scheduled []types.ScheduledNotificationConfig) {
	configured := map[string]bool{}
	for _, schedule := range scheduled {
		for _, via := range schedule.Via {
			configured[scheduledStateKey(schedule.ID, strings.TrimSpace(via))] = true
		}
		// The build-failure streak key never reaches the DB, but it must
		// count as configured so the in-memory prune below keeps a live
		// build streak.
		configured[scheduledStateKey(schedule.ID, scheduledBuildFailureVia)] = true
	}
	rows, err := s.db.Query(`SELECT key FROM slack_notifier_state WHERE key LIKE ?`, scheduledStateKeyPrefix+"%")
	if err != nil {
		log.Printf("[notify] list scheduled state: %v", err)
		return
	}
	var stale []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			log.Printf("[notify] list scheduled state: %v", err)
			rows.Close()
			return
		}
		if !configured[key] {
			stale = append(stale, key)
		}
	}
	rows.Close()
	for _, key := range stale {
		if _, err := s.db.Exec(`DELETE FROM slack_notifier_state WHERE key=?`, key); err != nil {
			log.Printf("[notify] prune scheduled state %q: %v", key, err)
		}
	}
	// Drop the in-memory failure streaks of removed pairs too, mirroring
	// clearLifecycleTransientFailures: a streak entry is otherwise cleared
	// only from the send path of still-configured pairs, so a pair removed
	// mid-streak would leak its entry for the process lifetime and hand a
	// same-slot re-add a retry budget already spent.
	s.scheduledFailureMu.Lock()
	for key := range s.scheduledTransientFailures {
		if !configured[key] {
			delete(s.scheduledTransientFailures, key)
		}
	}
	s.scheduledFailureMu.Unlock()
}

func scheduledNotificationSlot(nowAt time.Time, schedule types.ScheduledNotificationConfig) (time.Time, bool) {
	location := time.UTC
	if schedule.Timezone != "" {
		var err error
		location, err = time.LoadLocation(schedule.Timezone)
		if err != nil {
			return time.Time{}, false
		}
	}
	at, err := time.Parse("15:04", schedule.At)
	if err != nil {
		return time.Time{}, false
	}
	localNow := nowAt.In(location)
	allowed := make(map[string]bool, len(schedule.Weekdays))
	for _, weekday := range schedule.Weekdays {
		allowed[weekday] = true
	}
	year, month, dayOfMonth := localNow.Date()
	for daysAgo := 0; daysAgo < 8; daysAgo++ {
		// Civil-date arithmetic (in UTC, where no gaps exist): AddDate on
		// localNow would carry its wall time along, and when that lands in a
		// midnight DST spring-forward gap Go normalizes it onto the previous
		// date — the walk then visits that date twice and never the
		// transition day, so restart catch-up could return an older slot
		// than the latest (a double delivery) or transiently none at all.
		day := time.Date(year, month, dayOfMonth-daysAgo, 12, 0, 0, 0, time.UTC)
		weekday := strings.ToLower(day.Weekday().String()[:3])
		if len(allowed) != 0 && !allowed[weekday] {
			continue
		}
		// A spring-forward DST gap can swallow the configured wall time;
		// time.Date then normalizes it to a real instant on one side of the
		// gap (which side is not guaranteed). Keep a normalized instant —
		// the post-gap one, e.g. 02:30 -> 03:30 — rather than skipping the
		// day: a weekday-restricted schedule whose only allowed day lands in
		// the gap would otherwise silently drop a full weekly cycle.
		candidate := time.Date(day.Year(), day.Month(), day.Day(), at.Hour(), at.Minute(), 0, 0, location)
		if got := candidate.Hour()*60 + candidate.Minute(); got != at.Hour()*60+at.Minute() {
			diff := at.Hour()*60 + at.Minute() - got
			if diff < -12*60 {
				diff += 24 * 60
			} else if diff > 12*60 {
				diff -= 24 * 60
			}
			// Both instants sit next to the gap; the later one is past it.
			if shifted := candidate.Add(time.Duration(diff) * time.Minute); shifted.After(candidate) {
				candidate = shifted
			}
		}
		if !candidate.After(localNow) {
			return candidate, true
		}
	}
	return time.Time{}, false
}

const scheduledStateKeyPrefix = "scheduled:last_fired:"

// scheduledStateKey joins the schedule id and destination with a NUL: both
// halves are free text that may contain any printable separator (a ':' most
// plausibly), so distinct (id, via) pairs could otherwise collide on one
// dedupe row. Control characters are banned in schedule ids and notifier
// names (types.ValidateNotificationsConfig), so NUL cannot occur in either.
func scheduledStateKey(id, via string) string {
	return scheduledStateKeyPrefix + id + "\x00" + via
}

// scheduledSlotDigest identifies the slot definition a dedupe row was written
// under. It is stored alongside the last-fired instant so an edit to at,
// timezone, or weekdays reads back as a digest mismatch and the row is
// reseeded like a first sight, instead of the pre-edit history counting as
// the new definition's own and firing an off-schedule send.
//
// At and Timezone are normalized so semantically identical definitions cannot
// read back as an edit: a hand-written "9:00" is zero-padded by any settings
// save (normalizeScheduledTimes), and the edit dialog seeds an absent
// timezone as the "UTC" the scheduler already defaults to. Without this, a
// representation-only rewrite reseeds every (id, via) row like a real edit
// and silently skips a pending or mid-retry slot.
func scheduledSlotDigest(schedule types.ScheduledNotificationConfig) string {
	at := schedule.At
	if parsed, err := time.Parse("15:04", strings.TrimSpace(at)); err == nil {
		at = parsed.Format("15:04")
	}
	timezone := schedule.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	// Weekdays are an unordered set everywhere else (scheduledNotificationSlot,
	// validation), and the settings screen's chips append in click order: a
	// semantically identical reorder must not read back as an edit — it would
	// reseed every (id, via) row and silently skip the pending slot.
	weekdays := append([]string(nil), schedule.Weekdays...)
	sort.Strings(weekdays)
	return at + "\x1f" + timezone + "\x1f" + strings.Join(weekdays, ",")
}

func (s *Server) scheduledLastFired(id, via string) (time.Time, string, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM slack_notifier_state WHERE key=?`, scheduledStateKey(id, via)).Scan(&raw)
	if err == sql.ErrNoRows {
		return time.Time{}, "", false, nil
	}
	if err != nil {
		return time.Time{}, "", false, err
	}
	// The value is "<RFC3339 slot>\n<slot digest>". A row written before the
	// digest existed parses as an empty digest and is reseeded on next sight.
	stamp, digest, _ := strings.Cut(raw, "\n")
	value, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("parse scheduled state %q: %w", raw, err)
	}
	return value, digest, true, nil
}

func (s *Server) setScheduledLastFired(id, via string, slot time.Time, digest string) {
	if _, err := s.db.Exec(`
		INSERT INTO slack_notifier_state(key, value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		scheduledStateKey(id, via), slot.Format(time.RFC3339)+"\n"+digest); err != nil {
		log.Printf("[notify] persist scheduled state for %s via %s: %v", id, via, err)
	}
}
