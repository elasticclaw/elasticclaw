package hub

import (
	"context"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket/wsjson"
)

func TestGatewayUnhealthyInReconnectGrace(t *testing.T) {
	connected := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	const grace = 2 * time.Minute

	cases := []struct {
		name          string
		count         int
		connectedAt   time.Time
		lastGrantedAt time.Time
		grace         time.Duration
		now           time.Time
		want          bool
		why           string
	}{
		{
			name: "fresh connection still starting", count: 0, connectedAt: connected,
			grace: grace, now: connected.Add(30 * time.Second), want: true,
			why: "a gateway that has had 30s to start is not a failing gateway",
		},
		{
			name: "grace elapsed", count: 0, connectedAt: connected,
			grace: grace, now: connected.Add(3 * time.Minute), want: false,
			why: "past the grace an unhealthy gateway must start counting",
		},
		{
			name: "boundary is exclusive", count: 0, connectedAt: connected,
			grace: grace, now: connected.Add(grace), want: false,
			why: "exactly at the grace the window is over",
		},
		{
			name: "counter already non-zero", count: 1, connectedAt: connected,
			grace: grace, now: connected.Add(time.Second), want: false,
			why: "a gateway in a restart loop carries its counter into the reconnect and must not be shielded",
		},
		{
			name: "grace disabled", count: 0, connectedAt: connected,
			grace: 0, now: connected.Add(time.Second), want: false,
			why: "zero restores the un-graced behaviour",
		},
		{
			name: "no connection timestamp", count: 0, connectedAt: time.Time{},
			grace: grace, now: connected, want: false,
			why: "without a registration time there is no window to be inside of",
		},
		{
			name: "same connection keeps its open window", count: 0, connectedAt: connected,
			lastGrantedAt: connected.Add(time.Second), grace: grace, now: connected.Add(30 * time.Second),
			want: true,
			why: "the grace is a window on the connection, not a single heartbeat",
		},
		{
			name: "flapping bridge cannot renew the grace", count: 0, connectedAt: connected,
			lastGrantedAt: connected.Add(-time.Minute), grace: grace, now: connected.Add(time.Second),
			want: false,
			why:  "a gateway reconnecting faster than the renewal window would hold the counter at zero forever",
		},
		{
			name: "grace renews once the window has passed", count: 0, connectedAt: connected,
			lastGrantedAt: connected.Add(-(grace*gatewayGraceRenewalFactor + time.Minute)),
			grace:         grace, now: connected.Add(time.Second), want: true,
			why: "an isolated reconnect long after the last one is still protected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gatewayUnhealthyInReconnectGrace(tc.count, tc.connectedAt, tc.lastGrantedAt, tc.grace, tc.now); got != tc.want {
				t.Errorf("got %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

func settingsFor(t *testing.T, liveness *types.LivenessConfig) livenessSettings {
	t.Helper()
	s := &Server{hubCfg: &types.HubConfig{Liveness: liveness}}
	return s.livenessSettings()
}

func TestGatewayUnhealthyReconnectGraceConfig(t *testing.T) {
	if got := settingsFor(t, nil).gatewayUnhealthyReconnectGrace; got != defaultGatewayUnhealthyReconnectGrace {
		t.Errorf("no liveness config: grace = %s, want the %s default", got, defaultGatewayUnhealthyReconnectGrace)
	}

	if got := settingsFor(t, &types.LivenessConfig{}).gatewayUnhealthyReconnectGrace; got != defaultGatewayUnhealthyReconnectGrace {
		t.Errorf("unset knob: grace = %s, want the default", got)
	}

	if got := settingsFor(t, &types.LivenessConfig{
		GatewayUnhealthyReconnectGrace: "90s",
	}).gatewayUnhealthyReconnectGrace; got != 90*time.Second {
		t.Errorf("configured grace = %s, want 90s", got)
	}

	// Zero is meaningful: it turns the grace off without a deploy.
	for _, raw := range []string{"0", "0s"} {
		if got := settingsFor(t, &types.LivenessConfig{
			GatewayUnhealthyReconnectGrace: raw,
		}).gatewayUnhealthyReconnectGrace; got != 0 {
			t.Errorf("grace %q = %s, want 0 (disabled)", raw, got)
		}
	}

	// Garbage falls back rather than failing hub startup, matching every other
	// liveness knob.
	for _, raw := range []string{"soon", "-5m", "5"} {
		if got := settingsFor(t, &types.LivenessConfig{
			GatewayUnhealthyReconnectGrace: raw,
		}).gatewayUnhealthyReconnectGrace; got != defaultGatewayUnhealthyReconnectGrace {
			t.Errorf("invalid grace %q = %s, want the default", raw, got)
		}
	}
}

// End to end through the heartbeat handler: with the grace active, unhealthy
// heartbeats arriving right after the bridge registers must not spend the
// claw's escalation budget. This is the case that was replacing healthy claws.
func TestUnhealthyHeartbeatsDuringGraceDoNotCount(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		ClawToken: "claw-token",
		Liveness:  &types.LivenessConfig{GatewayUnhealthyReconnectGrace: "10m"},
	}, "", "", "")
	s.cronScheduler = newCronScheduler(s)
	const clawID = "gateway-grace-claw"
	conn := watchdogClaw(t, s, clawID)
	if _, err := db.Exec(`UPDATE claws SET provider='noop', status='connected', bootstrap_ok=1 WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < defaultGatewayUnhealthyMax; i++ {
		if err := wsjson.Write(context.Background(), conn, types.WSMessage{
			Type: "heartbeat", Payload: map[string]any{"gateway_healthy": false},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Give the handler time to have processed them, then assert nothing counted.
	if err := wsjson.Write(context.Background(), conn, types.WSMessage{
		Type: "heartbeat", Payload: map[string]any{"gateway_healthy": false},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)

	if got := s.gatewayUnhealthyCount(clawID); got != 0 {
		t.Errorf("unhealthy count = %d during the grace, want 0", got)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "connected" {
		t.Errorf("claw status = %q, want it left connected during the grace", status)
	}
}
