package openclaw

import (
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// ClassifyErrorText classifies an OpenClaw turn failure from its text, in the
// precedence the hub has always applied: a definite usage limit, then model
// transport, provider transient and gateway connectivity. The remaining classes
// are informational; the hub treats them all as an unrecoverable error.
func ClassifyErrorText(errText string, definite bool) types.TurnErrorClass {
	if _, ok := types.ParseLLMUsageLimit(errText); ok && definite {
		return types.TurnErrorQuota
	}
	if BridgeErrorIsModelTransport(errText) {
		return types.TurnErrorModelTransport
	}
	if BridgeErrorIsProviderTransient(errText) {
		return types.TurnErrorTransient
	}
	if BridgeErrorIsGatewayConnectivity(errText) {
		return types.TurnErrorHarnessConnectivity
	}
	lower := strings.ToLower(errText)
	for _, group := range []struct {
		class   types.TurnErrorClass
		markers []string
	}{
		{types.TurnErrorQuota, []string{"usage limit", "credit", "billing"}},
		{types.TurnErrorAuth, []string{"unauthorized", "forbidden", "credential", "auth"}},
		{types.TurnErrorContextOverflow, []string{"context overflow", "prompt too large"}},
		{types.TurnErrorSessionLost, []string{"session not found", "session key not found", "unknown session", "session recovery failed"}},
	} {
		for _, marker := range group.markers {
			if strings.Contains(lower, marker) {
				return group.class
			}
		}
	}
	return types.TurnErrorFatal
}

// BridgeErrorIsProviderTransient identifies NEXT-1294's intermittent provider
// failure. The provider completed a tool call with invalid argument JSON, so
// OpenClaw never ran the tool and the intact session can safely continue. Keep
// this deliberately narrow: every other "Agent run failed" cause (auth,
// credits, schema rejection, and unknown provider failures) remains human-only
// under the NEXT-725 guarantee.
func BridgeErrorIsProviderTransient(errText string) bool {
	return strings.Contains(strings.ToLower(errText), "malformed json arguments")
}

func BridgeErrorIsModelTransport(errText string) bool {
	lower := strings.ToLower(errText)
	for _, marker := range []string{"unauthorized", "forbidden", "auth", "credential", "usage limit", "credit", "billing", "schema", "no space left", "enospc"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	return strings.Contains(lower, "llm request failed: network connection was interrupted") ||
		strings.Contains(lower, "the model did not produce a response before the model idle timeout")
}

// BridgeErrorIsGatewayConnectivity matches the claw-bridge strings that mean
// "could not talk to the local gateway" (cmd/claw-bridge/main.go). The
// reconnect wrapper ("...; gateway reconnect failed: <cause>") also carries
// causes that are not a hang: the gateway answering the handshake with a
// rejection, or a credential/disk failure. Those need a human, so any
// non-transient marker anywhere in the text wins over a connectivity marker.
func BridgeErrorIsGatewayConnectivity(errText string) bool {
	lower := strings.ToLower(errText)
	for _, marker := range []string{
		"connect rejected", "unauthorized", "forbidden", "auth", "credential",
		"no space left", "enospc", "agent run failed", "usage limit",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, marker := range []string{"gateway disconnected", "dial gateway", "gateway reconnect failed"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
