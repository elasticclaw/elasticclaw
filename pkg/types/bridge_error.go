package types

import "strings"

// Bridge transport errors (NEXT-725).
//
// When a turn fails before or inside the gateway, claw-bridge has no reply to
// send, so it sends the error text as the turn body. The hub used to read that
// as the agent speaking: on claw 1572c4e4 the sandbox disk filled up, every
// turn came back as "⚠️ claw-bridge error: ENOSPC: no space left on device",
// and the initial-plan gate answered each one with another correction — one
// hour per round trip, forever, with the word ENOSPC never once reaching the
// hub's journal.
//
// The prefixes below are what bridges ALREADY deployed write, and they are the
// only recognition signal that works today: a claw only gets a new bridge when
// it is created after a release, so a new protocol field would leave every
// live claw on the old path. The bridge builds its replies from these same
// constants so the two halves cannot drift apart silently.
const (
	// BridgeErrorPrefix opens the reply written when a live turn fails.
	BridgeErrorPrefix = "⚠️ claw-bridge error:"
	// BridgeReplayErrorPrefix opens the same reply on the reconnect-replay
	// path, which has always used a shorter label. Both are in the field.
	BridgeReplayErrorPrefix = "⚠️ error:"
)

// BridgeTransportError reports whether a claw turn body is a bridge error
// reply and returns the underlying error text.
//
// The prefix must OPEN the message, for the reason the [DONE]/[TERMINATE]
// matchers are anchored: an agent that quotes or explains an error it saw
// ("the build printed ⚠️ claw-bridge error: ...") is talking, and treating
// that turn as a dead transport would silently stop a working claw.
func BridgeTransportError(content string) (string, bool) {
	errText, _, ok := bridgeTransportError(content)
	return errText, ok
}

// BridgeTransportErrorIsDefinite additionally reports whether the reply carried
// the bridge's OWN label rather than the generic replay one.
//
// Only the definite form should count toward stopping a claw. "⚠️ error:" is
// short enough that an agent can plausibly open a turn with it while reporting
// a build failure, and mistaking that for a dead transport would pause a
// working claw and tell its operator a lie about the cause. Silencing the plan
// gate on the generic form is safe either way — a turn opening with an error
// is not a plan.
func BridgeTransportErrorIsDefinite(content string) (string, bool, bool) {
	return bridgeTransportError(content)
}

func bridgeTransportError(content string) (errText string, definite bool, ok bool) {
	trimmed := strings.TrimSpace(content)
	if rest, cut := strings.CutPrefix(trimmed, BridgeErrorPrefix); cut {
		return strings.TrimSpace(rest), true, true
	}
	if rest, cut := strings.CutPrefix(trimmed, BridgeReplayErrorPrefix); cut {
		return strings.TrimSpace(rest), false, true
	}
	return "", false, false
}
