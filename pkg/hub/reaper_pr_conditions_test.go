package hub

import (
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestLivenessSettingsPRConditionsMaxWait(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{}, "", "", "")
	if got := s.livenessSettings().prConditionsMaxWait; got != 2*time.Hour {
		t.Fatalf("default max wait = %s, want 2h", got)
	}
	s.hubCfg.Liveness = &types.LivenessConfig{PRConditionsMaxWait: "45m"}
	if got := s.livenessSettings().prConditionsMaxWait; got != 45*time.Minute {
		t.Fatalf("custom max wait = %s, want 45m", got)
	}
}
