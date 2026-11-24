package openclaw

import (
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestClassifyErrorText(t *testing.T) {
	const model = "LLM request failed: network connection was interrupted"
	const limit = "You have reached your specified API usage limits."
	for _, tt := range []struct {
		text         string
		live, replay types.TurnErrorClass
	}{
		{"gateway disconnected", types.TurnErrorHarnessConnectivity, types.TurnErrorHarnessConnectivity},
		{"dial gateway: refused", types.TurnErrorHarnessConnectivity, types.TurnErrorHarnessConnectivity},
		{"gateway reconnect failed: timeout", types.TurnErrorHarnessConnectivity, types.TurnErrorHarnessConnectivity},
		{"gateway disconnected; connect rejected", types.TurnErrorFatal, types.TurnErrorFatal},
		{"gateway disconnected; unauthorized", types.TurnErrorAuth, types.TurnErrorAuth},
		{"gateway disconnected; ENOSPC", types.TurnErrorFatal, types.TurnErrorFatal},
		{"gateway disconnected; Agent run failed", types.TurnErrorFatal, types.TurnErrorFatal},
		{model, types.TurnErrorModelTransport, types.TurnErrorModelTransport},
		{model + "; malformed json arguments", types.TurnErrorModelTransport, types.TurnErrorModelTransport},
		{"The model did not produce a response before the model idle timeout", types.TurnErrorModelTransport, types.TurnErrorModelTransport},
		{model + "; schema rejected", types.TurnErrorFatal, types.TurnErrorFatal},
		{"malformed json arguments", types.TurnErrorTransient, types.TurnErrorTransient},
		{"gateway disconnected; malformed json arguments", types.TurnErrorTransient, types.TurnErrorTransient},
		{limit + " malformed json arguments", types.TurnErrorQuota, types.TurnErrorTransient},
		{"Your credit balance is too low; gateway disconnected", types.TurnErrorQuota, types.TurnErrorHarnessConnectivity},
		{limit, types.TurnErrorQuota, types.TurnErrorQuota},
		{"billing failed", types.TurnErrorQuota, types.TurnErrorQuota},
		{"forbidden", types.TurnErrorAuth, types.TurnErrorAuth},
		{"credentials missing", types.TurnErrorAuth, types.TurnErrorAuth},
		{"authentication rejected", types.TurnErrorAuth, types.TurnErrorAuth},
		{"context overflow", types.TurnErrorContextOverflow, types.TurnErrorContextOverflow},
		{"prompt too large", types.TurnErrorContextOverflow, types.TurnErrorContextOverflow},
		{"session not found", types.TurnErrorSessionLost, types.TurnErrorSessionLost},
		{"session key not found", types.TurnErrorSessionLost, types.TurnErrorSessionLost},
		{"unknown session", types.TurnErrorSessionLost, types.TurnErrorSessionLost},
		{"session recovery failed", types.TurnErrorSessionLost, types.TurnErrorSessionLost},
		{"agent run failed", types.TurnErrorFatal, types.TurnErrorFatal},
		{"", types.TurnErrorFatal, types.TurnErrorFatal},
	} {
		t.Run(tt.text, func(t *testing.T) {
			for _, text := range []string{tt.text, strings.ToUpper(tt.text)} {
				for _, definite := range []bool{false, true} {
					want := tt.replay
					if definite {
						want = tt.live
					}
					if got := ClassifyErrorText(text, definite); got != want {
						t.Errorf("ClassifyErrorText(%q, %v) = %q, want %q", text, definite, got, want)
					}
				}
			}
		})
	}
}
