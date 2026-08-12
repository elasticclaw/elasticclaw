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
