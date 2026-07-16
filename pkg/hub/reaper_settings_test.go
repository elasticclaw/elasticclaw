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
	checks := 7
	s := &Server{hubCfg: &types.HubConfig{Liveness: &types.LivenessConfig{
		GatewayUnhealthyChecks: &checks,
		BusyTurnMax:            "20m",
		SilentDeathMax:         "8m",
	}}}
	cfg := s.livenessSettings()
	if cfg.gatewayUnhealthyMax != 7 || cfg.busyTurnMax != 20*time.Minute || cfg.silentDeathMax != 8*time.Minute {
		t.Fatalf("watchdog config = %#v, want 7/20m/8m", cfg)
	}
}
