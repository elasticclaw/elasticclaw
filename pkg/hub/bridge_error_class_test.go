package hub

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/harness/openclaw"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

type bridgeErrorDecision struct {
	limitRouted       bool
	kind              string
	providerTransient bool
}

// legacyBridgeErrorDecision is the hub's text decision tree as it was before
// bridges sent error_class, kept verbatim so the class path can be proven
// equivalent to it.
func legacyBridgeErrorDecision(errText string, definite bool) bridgeErrorDecision {
	if _, ok := types.ParseLLMUsageLimit(errText); ok && definite {
		return bridgeErrorDecision{limitRouted: true}
	}
	providerTransient := bridgeErrorIsProviderTransient(errText)
	kind := ""
	if bridgeErrorIsModelTransport(errText) {
		kind = bridgeErrorPauseKindModelTransport
	} else if providerTransient {
		kind = bridgeErrorPauseKindProviderTransient
	} else if bridgeErrorIsGatewayConnectivity(errText) {
		kind = bridgeErrorPauseKindGateway
	}
	return bridgeErrorDecision{kind: kind, providerTransient: providerTransient}
}

// classBridgeErrorDecision mirrors observeTurnOutcome/observeBridgeErrorTurn
// deciding from the class.
func classBridgeErrorDecision(errText string, definite bool, class types.TurnErrorClass) bridgeErrorDecision {
	if class == types.TurnErrorQuota {
		if _, ok := types.ParseLLMUsageLimit(errText); ok && definite {
			return bridgeErrorDecision{limitRouted: true}
		}
	}
	providerTransient := class == types.TurnErrorTransient
	if class == types.TurnErrorModelTransport && bridgeErrorIsProviderTransient(errText) {
		providerTransient = true
	}
	kind := ""
	switch class {
	case types.TurnErrorModelTransport:
		kind = bridgeErrorPauseKindModelTransport
	case types.TurnErrorTransient:
		kind = bridgeErrorPauseKindProviderTransient
	case types.TurnErrorHarnessConnectivity:
		kind = bridgeErrorPauseKindGateway
	}
	return bridgeErrorDecision{kind: kind, providerTransient: providerTransient}
}

func TestBridgeErrorClassMatchesLegacyTextDecision(t *testing.T) {
	const limit = "LLM request rejected: You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC."
	const credit = "LLM request rejected: Your credit balance is too low to access the Anthropic API."
	base := []string{
		strings.TrimPrefix(incidentENOSPCTurn, types.BridgeErrorPrefix+" "),
		strings.TrimPrefix(incidentRecoveryTurn, types.BridgeErrorPrefix+" "),
		strings.TrimPrefix(incidentUsageLimitTurn, types.BridgeErrorPrefix+" "),
		gatewayDownErr,
		providerMalformedJSONErr,
		modelNetworkInterruptedErr,
		modelIdleTimeoutErr,
		"sessions.send write: use of closed network connection; gateway reconnect failed: dial gateway: failed to WebSocket dial: context deadline exceeded",
		"dial gateway: connection refused",
		"gateway reconnect failed: connect rejected: invalid token",
		"⚠️ Agent run failed (model: anthropic/claude-sonnet-5): authentication rejected",
		"⚠️ Agent run failed (model: anthropic/claude-sonnet-5): insufficient credits",
		"⚠️ Agent run failed (model: anthropic/claude-sonnet-5): provider rejected the request schema",
		"agent run failed",
		"unauthorized",
		"forbidden",
		"credentials missing",
		"billing failed",
		"schema rejected",
		"ENOSPC: no space left on device",
		limit,
		credit,
		"You have reached your specified API usage limits. You will regain access on 2026-09-01 at 06:30:45 UTC",
		"You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 PST",
		"gateway: LLM REQUEST REJECTED: you have REACHED YOUR SPECIFIED API USAGE LIMITS. You will regain access on 2026-09-01 at 00:00 utc.",
		"LLM request failed: network connection error.",
		"The AI service is temporarily overloaded. Please try again in a moment.",
		"context deadline exceeded",
		"context overflow",
		"prompt too large",
		"session not found",
		"unknown provider failure",
		"",
	}
	texts := append([]string(nil), base...)
	// Mixes, in both orders, so every precedence and veto pair is exercised.
	mixers := []string{gatewayDownErr, "malformed json arguments", modelNetworkInterruptedErr, modelIdleTimeoutErr, limit, credit, "ENOSPC", "unauthorized", "connect rejected", "agent run failed", "schema"}
	for _, a := range mixers {
		for _, b := range base {
			texts = append(texts, a+"; "+b, b+"; "+a)
		}
	}
	for _, text := range texts {
		for _, variant := range []string{text, strings.ToUpper(text)} {
			for _, definite := range []bool{true, false} {
				want := legacyBridgeErrorDecision(variant, definite)
				got := classBridgeErrorDecision(variant, definite, openclaw.ClassifyErrorText(variant, definite))
				if got != want {
					t.Errorf("definite=%v %q: class decision %+v, legacy %+v", definite, variant, got, want)
				}
			}
		}
	}
}

// The class from the bridge wins over the text; without one the hub falls
// back to classifying the text, as it does for older bridges.
func TestBridgeErrorClassDrivesPauseKind(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const content = types.BridgeErrorPrefix + " something the hub has no marker for"

	const classed = "claw-class-wins"
	cc := bridgeErrorClaw(t, s, classed)
	cc.deliveryInFlight = true
	for i := 0; i < bridgeErrorPauseThreshold; i++ {
		s.observeTurnOutcome(cc, classed, "turn", content, types.TurnErrorModelTransport)
	}
	if kind, _ := bridgeErrorRow(t, s, classed); !clawPaused(t, s, classed) || kind != bridgeErrorPauseKindModelTransport {
		t.Fatalf("classed: paused=%v kind=%q, want model transport pause", clawPaused(t, s, classed), kind)
	}

	const fallback = "claw-class-fallback"
	cc = bridgeErrorClaw(t, s, fallback)
	cc.deliveryInFlight = true
	for i := 0; i < bridgeErrorPauseThreshold; i++ {
		s.observeTurnOutcome(cc, fallback, "turn", content, "")
	}
	if kind, _ := bridgeErrorRow(t, s, fallback); !clawPaused(t, s, fallback) || kind != "" {
		t.Fatalf("fallback: paused=%v kind=%q, want human-only pause", clawPaused(t, s, fallback), kind)
	}

	const fallbackGateway = "claw-class-fallback-gateway"
	cc = bridgeErrorClaw(t, s, fallbackGateway)
	cc.deliveryInFlight = true
	for i := 0; i < bridgeErrorPauseThreshold; i++ {
		s.observeTurnOutcome(cc, fallbackGateway, "turn", types.BridgeErrorPrefix+" "+gatewayDownErr, "")
	}
	if kind, _ := bridgeErrorRow(t, s, fallbackGateway); kind != bridgeErrorPauseKindGateway {
		t.Fatalf("fallback gateway: kind=%q, want gateway", kind)
	}
}
