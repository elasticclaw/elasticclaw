package main

import "testing"

func TestHeartbeatPayloadHarnessAliases(t *testing.T) {
	for _, ready := range []bool{false, true} {
		for _, healthy := range []bool{false, true} {
			for _, restarts := range []int{0, 3} {
				payload := heartbeatPayload(healthy, ready, 42, restarts)
				for key, want := range map[string]interface{}{
					"gateway_healthy": healthy, "harness_healthy": healthy,
					"gateway_ready": ready, "harness_ready": ready,
					"restart_count": restarts, "harness_restart_count": restarts,
					"context_usage": 42,
				} {
					if got, ok := payload[key]; !ok || got != want {
						t.Errorf("payload[%q] = %v (present=%v), want %v", key, got, ok, want)
					}
				}
			}
		}
	}
	for _, snapshotKey := range []string{"", "stale-snapshot"} {
		for _, liveKey := range []string{"", "live-session"} {
			payload := heartbeatPayload(true, true, 42, 3)
			addHeartbeatSessionKeys(payload, snapshotKey, liveKey)
			for key, want := range map[string]string{
				"session_key": snapshotKey, "gateway_session_key": liveKey, "harness_session_key": liveKey,
			} {
				got, present := payload[key]
				if present != (want != "") || (present && got != want) {
					t.Errorf("payload[%q] = %v (present=%v), want %q with empty values omitted", key, got, present, want)
				}
			}
		}
	}
}
