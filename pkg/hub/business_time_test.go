package hub

import (
	"testing"
	"time"
)

func TestBusinessHoursDurationMs(t *testing.T) {
	utc := time.UTC
	weekdayHours := BusinessHours{
		Loc: utc, StartMin: 9 * 60, EndMin: 18 * 60,
		Workdays: [7]bool{time.Monday: true, time.Tuesday: true, time.Wednesday: true, time.Thursday: true, time.Friday: true},
	}
	ms := func(y int, m time.Month, d, h, min int, loc *time.Location) int64 {
		return time.Date(y, m, d, h, min, 0, 0, loc).UnixMilli()
	}
	hour := int64(time.Hour / time.Millisecond)

	tests := []struct {
		name     string
		business BusinessHours
		from, to int64
		want     int64
	}{
		{"nil location returns raw delta", BusinessHours{}, 100, 500, 400},
		{"zero from sentinel", weekdayHours, 0, ms(2024, time.January, 2, 10, 0, utc), 0},
		{"zero to sentinel", weekdayHours, ms(2024, time.January, 2, 10, 0, utc), 0, 0},
		{"non-positive interval", weekdayHours, 500, 500, 0},
		{"inside one business day", weekdayHours, ms(2024, time.January, 2, 10, 0, utc), ms(2024, time.January, 2, 12, 30, utc), 2*hour + 30*60*1000},
		{"crosses night boundary", weekdayHours, ms(2024, time.January, 2, 17, 0, utc), ms(2024, time.January, 3, 10, 0, utc), 2 * hour},
		{"spans full weekend", weekdayHours, ms(2024, time.January, 5, 17, 0, utc), ms(2024, time.January, 8, 10, 0, utc), 2 * hour},
		{"clamps to window", weekdayHours, ms(2024, time.January, 2, 7, 0, utc), ms(2024, time.January, 2, 20, 0, utc), 9 * hour},
		{"Saturday is outside business hours", weekdayHours, ms(2024, time.January, 6, 10, 0, utc), ms(2024, time.January, 6, 12, 0, utc), 0},
		{"multiple weeks", weekdayHours, ms(2024, time.January, 1, 9, 0, utc), ms(2024, time.January, 22, 18, 0, utc), 16 * 9 * hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.business.DurationMs(tt.from, tt.to); got != tt.want {
				t.Fatalf("DurationMs() = %d, want %d", got, tt.want)
			}
		})
	}
}

// A DST transition earlier in the day must not shift the business window, and
// a transition at local midnight must not shift the day cursor onto the wrong
// calendar day for the rest of the interval.
func TestBusinessHoursDurationMsDSTKeepsWallClockWindow(t *testing.T) {
	workdays := [7]bool{time.Monday: true, time.Tuesday: true, time.Wednesday: true, time.Thursday: true, time.Friday: true}
	hour := int64(time.Hour / time.Millisecond)

	jerusalem, err := time.LoadLocation("Asia/Jerusalem")
	if err != nil {
		t.Fatal(err)
	}
	// Israeli DST starts on Friday 2026-03-27 at 02:00, a workday.
	jerusalemHours := BusinessHours{Loc: jerusalem, StartMin: 9 * 60, EndMin: 18 * 60, Workdays: workdays}
	from := time.Date(2026, time.March, 27, 9, 0, 0, 0, jerusalem).UnixMilli()
	to := time.Date(2026, time.March, 27, 12, 0, 0, 0, jerusalem).UnixMilli()
	if got := jerusalemHours.DurationMs(from, to); got != 3*hour {
		t.Fatalf("transition-day window: DurationMs() = %d, want %d", got, 3*hour)
	}

	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	// Chile's DST starts at local midnight on Sunday 2026-09-06, so that
	// midnight does not exist.
	santiagoHours := BusinessHours{Loc: santiago, StartMin: 9 * 60, EndMin: 18 * 60, Workdays: workdays}
	from = time.Date(2026, time.September, 4, 17, 0, 0, 0, santiago).UnixMilli() // Friday
	to = time.Date(2026, time.September, 8, 10, 0, 0, 0, santiago).UnixMilli()   // Tuesday
	if got := santiagoHours.DurationMs(from, to); got != 11*hour {               // Fri 1h + Mon 9h + Tue 1h
		t.Fatalf("midnight-transition cursor: DurationMs() = %d, want %d", got, 11*hour)
	}
}

func TestBusinessHoursDurationMsDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	allDays := [7]bool{true, true, true, true, true, true, true}
	business := BusinessHours{Loc: loc, StartMin: 0, EndMin: 24 * 60, Workdays: allDays}

	tests := []struct {
		name       string
		from, to   time.Time
		wantMillis int64
	}{
		{
			name:       "spring forward March 10 2024 is 23 hours",
			from:       time.Date(2024, time.March, 10, 0, 0, 0, 0, loc),
			to:         time.Date(2024, time.March, 11, 0, 0, 0, 0, loc),
			wantMillis: int64(23 * time.Hour / time.Millisecond),
		},
		{
			name:       "fall back November 3 2024 is 25 hours",
			from:       time.Date(2024, time.November, 3, 0, 0, 0, 0, loc),
			to:         time.Date(2024, time.November, 4, 0, 0, 0, 0, loc),
			wantMillis: int64(25 * time.Hour / time.Millisecond),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := business.DurationMs(tt.from.UnixMilli(), tt.to.UnixMilli()); got != tt.wantMillis {
				t.Fatalf("DurationMs() = %d, want %d", got, tt.wantMillis)
			}
		})
	}
}

func TestBusinessHoursFromEnvRejectsReversedWindow(t *testing.T) {
	// A reversed or empty window must not reach DurationMs: it reads
	// end <= start as "unconfigured" and silently returns wall-clock, which
	// looks identical to the feature being off.
	for _, invalid := range []string{"18:00-09:00", "09:00-09:00"} {
		t.Run(invalid, func(t *testing.T) {
			if _, _, err := parseBusinessHours(invalid); err == nil {
				t.Fatalf("parseBusinessHours(%q) = nil error, want rejection", invalid)
			}

			t.Setenv("HUB_BUSINESS_HOURS", invalid)
			business := BusinessHoursFromEnv("America/Sao_Paulo")
			if business.StartMin != 9*60 || business.EndMin != 18*60 {
				t.Fatalf("window = %d-%d, want the 9:00-18:00 default", business.StartMin, business.EndMin)
			}

			loc, err := time.LoadLocation("America/Sao_Paulo")
			if err != nil {
				t.Fatalf("LoadLocation: %v", err)
			}
			// Wednesday 08:00 -> 12:00 is 3 business hours under the default
			// window; a silent wall-clock fallback would report 4.
			from := time.Date(2026, time.July, 1, 8, 0, 0, 0, loc)
			to := time.Date(2026, time.July, 1, 12, 0, 0, 0, loc)
			want := int64(3 * time.Hour / time.Millisecond)
			if got := business.DurationMs(from.UnixMilli(), to.UnixMilli()); got != want {
				t.Fatalf("DurationMs() = %d, want %d", got, want)
			}
		})
	}
}
