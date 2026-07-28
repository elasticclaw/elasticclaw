package hub

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// BusinessHours describes a recurring local-time business schedule.
type BusinessHours struct {
	Loc      *time.Location // nil means 24/7 (no adjustment)
	StartMin int            // minutes since local midnight, e.g. 9*60
	EndMin   int            // e.g. 18*60
	Workdays [7]bool        // index by int(time.Weekday()), Sunday = 0
}

// DurationMs returns the elapsed business-time in milliseconds between two
// epoch-millisecond instants. Returns 0 if either bound is 0 (the absent
// sentinel used throughout this codebase) or if toMs <= fromMs.
func (b BusinessHours) DurationMs(fromMs, toMs int64) int64 {
	if fromMs == 0 || toMs == 0 || toMs <= fromMs {
		return 0
	}
	if b.Loc == nil || b.EndMin <= b.StartMin || !b.hasWorkdays() {
		return toMs - fromMs
	}

	from := time.UnixMilli(fromMs)
	to := time.UnixMilli(toMs)
	first := from.In(b.Loc)
	last := to.In(b.Loc)
	year, month, dayOfMonth := first.Date()
	lastYear, lastMonth, lastDayOfMonth := last.Date()
	// Days are addressed by calendar date and anchored at local noon: midnight
	// does not exist in timezones whose DST transition happens at 00:00, and
	// time.Date would silently normalize it into the previous day.
	lastDay := time.Date(lastYear, lastMonth, lastDayOfMonth, 12, 0, 0, 0, b.Loc)

	var total time.Duration
	for offset := 0; ; offset++ {
		day := time.Date(year, month, dayOfMonth+offset, 12, 0, 0, 0, b.Loc)
		if day.After(lastDay) {
			break
		}
		if b.Workdays[day.Weekday()] {
			windowStart := businessTimeOnDay(day, b.StartMin)
			windowEnd := businessTimeOnDay(day, b.EndMin)
			start := maxTime(from, windowStart)
			end := minTime(to, windowEnd)
			if end.After(start) {
				total += end.Sub(start)
			}
		}
	}
	return total.Milliseconds()
}

func (b BusinessHours) hasWorkdays() bool {
	for _, workday := range b.Workdays {
		if workday {
			return true
		}
	}
	return false
}

// businessTimeOnDay returns the wall-clock instant `minutes` after local
// midnight on day's calendar date. It builds the boundary from calendar fields
// rather than adding elapsed time to midnight, so a DST transition earlier in
// the day does not shift the whole business window by the offset delta.
func businessTimeOnDay(day time.Time, minutes int) time.Time {
	year, month, dayOfMonth := day.Date()
	// 24:00 is the following local midnight. Using that midnight (rather than
	// adding 24 elapsed hours) preserves the real length of a DST transition day.
	if minutes == 24*60 {
		return time.Date(year, month, dayOfMonth+1, 0, 0, 0, 0, day.Location())
	}
	return time.Date(year, month, dayOfMonth, minutes/60, minutes%60, 0, 0, day.Location())
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// BusinessHoursFromEnv loads the local business schedule for tz. Invalid
// configuration falls back to the documented defaults; an invalid timezone
// deliberately disables adjustment so existing 24/7 behavior is preserved.
func BusinessHoursFromEnv(tz string) BusinessHours {
	if tz == "" {
		// No timezone is the documented backward-compatible 24/7 default, not
		// a misconfiguration — logging it would fire on every legacy client.
		return BusinessHours{}
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		log.Printf("[analytics] invalid timezone %q, using 24/7: %v", tz, err)
		return BusinessHours{}
	}

	const defaultHours = "9:00-18:00"
	const defaultDays = "mon,tue,wed,thu,fri"
	hours := os.Getenv("HUB_BUSINESS_HOURS")
	if hours == "" {
		hours = defaultHours
	}
	start, end, err := parseBusinessHours(hours)
	if err != nil {
		log.Printf("[analytics] invalid HUB_BUSINESS_HOURS %q, using default %q: %v", hours, defaultHours, err)
		start, end, _ = parseBusinessHours(defaultHours)
	}

	days := os.Getenv("HUB_BUSINESS_DAYS")
	if days == "" {
		days = defaultDays
	}
	workdays, err := parseBusinessDays(days)
	if err != nil {
		log.Printf("[analytics] invalid HUB_BUSINESS_DAYS %q, using default %q: %v", days, defaultDays, err)
		workdays, _ = parseBusinessDays(defaultDays)
	}
	return BusinessHours{Loc: loc, StartMin: start, EndMin: end, Workdays: workdays}
}

func parseBusinessHours(value string) (int, int, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("must be start-end")
	}
	start, err := parseClock(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	end, err := parseClock(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	// Reject reversed or empty windows here rather than letting them through.
	// DurationMs treats end <= start as "no window configured" and falls back
	// to wall-clock, so accepting them would silently disable the adjustment
	// with no log line for the operator to find.
	if end <= start {
		return 0, 0, fmt.Errorf("end %q must be after start %q", parts[1], parts[0])
	}
	return start, end, nil
}

func parseClock(value string) (int, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid clock time %q", value)
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("invalid clock time %q", value)
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || hour < 0 || hour > 24 || minute < 0 || minute > 59 || hour == 24 && minute != 0 {
		return 0, fmt.Errorf("invalid clock time %q", value)
	}
	return hour*60 + minute, nil
}

func parseBusinessDays(value string) ([7]bool, error) {
	var days [7]bool
	weekday := map[string]time.Weekday{
		"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
		"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
	}
	for _, part := range strings.Split(value, ",") {
		day, ok := weekday[strings.ToLower(strings.TrimSpace(part))]
		if !ok {
			return [7]bool{}, fmt.Errorf("invalid business day %q", part)
		}
		days[day] = true
	}
	if !(BusinessHours{Workdays: days}).hasWorkdays() {
		return [7]bool{}, fmt.Errorf("no business days")
	}
	return days, nil
}
