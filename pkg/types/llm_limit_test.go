package types

import (
	"testing"
	"time"
)

func TestParseLLMUsageLimit(t *testing.T) {
	utc := func(s string) time.Time {
		at, err := time.ParseInLocation("2006-01-02 15:04:05", s, time.UTC)
		if err != nil {
			t.Fatalf("bad fixture time %q: %v", s, err)
		}
		return at
	}

	tests := []struct {
		name       string
		text       string
		wantOK     bool
		wantReason string
		wantRegain time.Time
	}{
		{
			// Verbatim from the hub journal, 2026-08-31 18:13 UTC.
			name:       "production usage limit with deadline",
			text:       "LLM request rejected: You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC.",
			wantOK:     true,
			wantReason: LLMLimitUsage,
			wantRegain: utc("2026-09-01 00:00:00"),
		},
		{
			name:       "seconds in the deadline",
			text:       "You have reached your specified API usage limits. You will regain access on 2026-09-01 at 06:30:45 UTC",
			wantOK:     true,
			wantReason: LLMLimitUsage,
			wantRegain: utc("2026-09-01 06:30:45"),
		},
		{
			name:       "date only",
			text:       "You have reached your specified API usage limits. You will regain access on 2026-09-01 UTC",
			wantOK:     true,
			wantReason: LLMLimitUsage,
			wantRegain: utc("2026-09-01 00:00:00"),
		},
		{
			// A zone we cannot resolve is worse than no zone: waking early
			// re-latches on the same rejection. Keep the limit, drop the time.
			name:       "non-UTC zone yields no deadline",
			text:       "You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 PST",
			wantOK:     true,
			wantReason: LLMLimitUsage,
		},
		{
			name:       "usage limit without any deadline",
			text:       "LLM request rejected: You have reached your specified API usage limits.",
			wantOK:     true,
			wantReason: LLMLimitUsage,
		},
		{
			name:       "credit balance never carries a deadline",
			text:       "LLM request rejected: Your credit balance is too low to access the Anthropic API.",
			wantOK:     true,
			wantReason: LLMLimitCredit,
		},
		{
			name:       "case and preamble are ignored",
			text:       "gateway: LLM REQUEST REJECTED: you have REACHED YOUR SPECIFIED API USAGE LIMITS. You will regain access on 2026-09-01 at 00:00 utc.",
			wantOK:     true,
			wantReason: LLMLimitUsage,
			wantRegain: utc("2026-09-01 00:00:00"),
		},
		{
			// The failure that started the incident. It is a transport error,
			// not a limit: classifying it as one would park the fleet with a
			// made-up deadline instead of pausing a genuinely broken bridge.
			name: "network connection error is not a limit",
			text: "LLM request failed: network connection error.",
		},
		{
			name: "overloaded is not a limit",
			text: "The AI service is temporarily overloaded. Please try again in a moment.",
		},
		{
			name: "disk full is not a limit",
			text: "ENOSPC: no space left on device",
		},
		{
			name: "empty",
			text: "   ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limit, ok := ParseLLMUsageLimit(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (limit %+v)", ok, tc.wantOK, limit)
			}
			if !tc.wantOK {
				return
			}
			if limit.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", limit.Reason, tc.wantReason)
			}
			if !limit.RegainAt.Equal(tc.wantRegain) {
				t.Errorf("regainAt = %v, want %v", limit.RegainAt, tc.wantRegain)
			}
			if limit.Message == "" {
				t.Error("message must keep the provider text for the operator")
			}
		})
	}
}

func TestLLMUsageLimitExpired(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	limit := LLMUsageLimit{Reason: LLMLimitUsage, RegainAt: at}

	if limit.Expired(at.Add(-time.Second)) {
		t.Error("not expired one second early")
	}
	if !limit.Expired(at) {
		t.Error("expired exactly at the regain instant")
	}
	if !limit.Expired(at.Add(time.Hour)) {
		t.Error("expired after the regain instant")
	}
	// A limit with no deadline must never look expired, or the scheduler would
	// release it on its very first tick and loop on the same rejection.
	if (LLMUsageLimit{Reason: LLMLimitCredit}).Expired(at) {
		t.Error("a limit with no deadline is never expired")
	}
}

// "UTC+3" is not UTC. The offset used to be dropped on the floor, yielding a
// deadline three hours wrong that the hub then treated as exact — strictly
// worse than the no-deadline fallback the UTC-only rule promises.
func TestParseLLMUsageLimitRejectsOffsetZones(t *testing.T) {
	for _, zone := range []string{"UTC+3", "UTC-5", "GMT+2", "GMT-05:30", "UTC+0530"} {
		text := "You have reached your specified API usage limits. You will regain access on 2026-09-02 at 00:00 " + zone
		limit, ok := ParseLLMUsageLimit(text)
		if !ok {
			t.Errorf("%s: limit not recognised at all", zone)
			continue
		}
		if !limit.RegainAt.IsZero() {
			t.Errorf("%s: regainAt = %v, want zero — an offset zone must fall back to no deadline, not be read as UTC",
				zone, limit.RegainAt)
		}
	}
}
