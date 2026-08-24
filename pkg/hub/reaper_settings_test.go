package hub

import (
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestLivenessSettingsWatchdogDefaults(t *testing.T) {
	s := &Server{}
	cfg := s.livenessSettings()
	if cfg.gatewayUnhealthyMax != defaultGatewayUnhealthyMax || cfg.busyTurnMax != defaultBusyTurnMax || cfg.silentDeathMax != defaultSilentDeathMax {
		t.Fatalf("watchdog defaults = %#v, want %d/%s/%s", cfg, defaultGatewayUnhealthyMax, defaultBusyTurnMax, defaultSilentDeathMax)
	}
}

// TestLivenessSettingsGatewayUnhealthyDefault pins the escalation threshold to a
// literal: at the bridge's 15s heartbeat cadence it must stay well clear of the
// ~2.8 minute event-loop stalls a busy gateway produces, so a lower value would
// silently reintroduce the mid-work replacements it was raised to stop.
func TestLivenessSettingsGatewayUnhealthyDefault(t *testing.T) {
	s := &Server{}
	if got := s.livenessSettings().gatewayUnhealthyMax; got != 40 {
		t.Fatalf("default gatewayUnhealthyMax = %d, want 40", got)
	}

	checks := 25
	s = &Server{hubCfg: &types.HubConfig{Liveness: &types.LivenessConfig{
		GatewayUnhealthyChecks: &checks,
	}}}
	if got := s.livenessSettings().gatewayUnhealthyMax; got != 25 {
		t.Fatalf("configured gatewayUnhealthyMax = %d, want 25", got)
	}
}

func TestLivenessSettingsWatchdogConfig(t *testing.T) {
	tests := []struct {
		name        string
		busyTurnMax string
		wantBusy    time.Duration
	}{
		{name: "clamps below floor", busyTurnMax: "20m", wantBusy: minBusyTurnMax},
		{name: "honors above floor", busyTurnMax: "90m", wantBusy: 90 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := 7
			s := &Server{hubCfg: &types.HubConfig{Liveness: &types.LivenessConfig{
				GatewayUnhealthyChecks: &checks,
				BusyTurnMax:            tt.busyTurnMax,
				SilentDeathMax:         "8m",
			}}}
			cfg := s.livenessSettings()
			if cfg.gatewayUnhealthyMax != 7 || cfg.busyTurnMax != tt.wantBusy || cfg.silentDeathMax != 8*time.Minute {
				t.Fatalf("watchdog config = %#v, want 7/%s/8m", cfg, tt.wantBusy)
			}
		})
	}
}
