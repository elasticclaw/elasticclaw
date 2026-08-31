package types

import (
	"regexp"
	"strings"
	"time"
)

// Provider usage limits.
//
// A turn that dies because the account ran out of allowance comes back through
// the SAME channel as a broken sandbox: claw-bridge has no reply, so it sends
// the provider's error text as the turn body (see BridgeTransportError). On
// 2026-08-31 the Faster fleet spent two hours reporting
//
//	LLM request failed: network connection error.
//
// before the gateway finally surfaced what was actually happening:
//
//	LLM request rejected: You have reached your specified API usage limits.
//	You will regain access on 2026-09-01 at 00:00 UTC.
//
// The two look identical to the hub, and the hub's answer to both — pause the
// claw, tell the operator the sandbox or the gateway is broken — is wrong for
// this one. Nothing is broken, the account is capped, and the provider even
// says exactly when it lifts. That last fact is the whole reason this parser
// exists: a limit with a known end is the one failure the hub can wait out on
// its own instead of burning a human's attention.
const (
	// LLMLimitUsage is the account-level cap configured in the provider's
	// console. It resets on the provider's own schedule (the billing period),
	// which is why these carry a RegainAt.
	LLMLimitUsage = "usage_limit"
	// LLMLimitCredit is an exhausted prepaid balance. It does not lift on a
	// timer — somebody has to add funds — so RegainAt stays zero.
	LLMLimitCredit = "credit_balance"
)

// LLMUsageLimit describes a provider rejection that means "not now, and here
// is why", as opposed to a transport failure that means "something is broken".
type LLMUsageLimit struct {
	// Reason is one of the LLMLimit* constants.
	Reason string
	// RegainAt is when the provider said access returns, in UTC. Zero means it
	// did not say: the caller picks its own retry delay, and must not read the
	// zero value as "already expired".
	RegainAt time.Time
	// Message is the provider's own sentence, kept verbatim for the operator.
	Message string
}

// Expired reports whether a known regain time has passed. A limit with no
// regain time is never "expired" — the caller schedules those itself.
func (l LLMUsageLimit) Expired(now time.Time) bool {
	return !l.RegainAt.IsZero() && !now.Before(l.RegainAt)
}

// regainAtPattern captures the deadline the provider volunteers.
//
// Only UTC is accepted. A named zone would have to be resolved against the
// hub's tzdata to become a real instant, and getting that wrong means either
// waking the fleet hours early (and re-latching on the same rejection) or
// leaving it parked past the reset. An unrecognised zone falls back to "no
// deadline", which is honest and still retries.
var regainAtPattern = regexp.MustCompile(`(?i)regain access on (\d{4}-\d{2}-\d{2})(?: at )?(\d{2}:\d{2}(?::\d{2})?)? *(UTC|GMT)`)

// limitPhrases are matched as substrings, not prefixes.
//
// The anchoring that keeps BridgeTransportError from mistaking an agent
// quoting an error for a dead transport already happened upstream: this only
// ever runs on a body claw-bridge itself wrote. Within that body the provider
// sentence can sit behind any gateway preamble, so a substring match is both
// safe and necessary.
var limitPhrases = []struct {
	phrase string
	reason string
}{
	{"reached your specified api usage limits", LLMLimitUsage},
	{"reached your specified usage limits", LLMLimitUsage},
	{"credit balance is too low", LLMLimitCredit},
}

// ParseLLMUsageLimit reports whether a bridge-error body is a provider usage
// limit and extracts when it lifts.
//
// Callers must pass a body already identified as a bridge transport error.
func ParseLLMUsageLimit(errText string) (LLMUsageLimit, bool) {
	trimmed := strings.TrimSpace(errText)
	if trimmed == "" {
		return LLMUsageLimit{}, false
	}
	lower := strings.ToLower(trimmed)

	reason := ""
	for _, candidate := range limitPhrases {
		if strings.Contains(lower, candidate.phrase) {
			reason = candidate.reason
			break
		}
	}
	if reason == "" {
		return LLMUsageLimit{}, false
	}

	limit := LLMUsageLimit{Reason: reason, Message: trimmed}
	if m := regainAtPattern.FindStringSubmatch(trimmed); m != nil {
		clock := m[2]
		if clock == "" {
			clock = "00:00:00"
		} else if len(clock) == len("15:04") {
			clock += ":00"
		}
		if at, err := time.ParseInLocation("2006-01-02 15:04:05", m[1]+" "+clock, time.UTC); err == nil {
			limit.RegainAt = at.UTC()
		}
	}
	return limit, true
}
