package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestNormalizeHarnessHeartbeatFields(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		healthy       bool
		ready         string
		restarts      int
		key           string
	}{
		{"absent", `{}`, false, "null", 0, "null"},
		{"legacy", `{"gateway_healthy":true,"gateway_ready":true,"restart_count":3,"gateway_session_key":"old"}`, true, "true", 3, `"old"`},
		{"harness", `{"harness_healthy":true,"harness_ready":true,"harness_restart_count":3,"harness_session_key":"new"}`, true, "true", 3, `"new"`},
		{"zero values win", `{"gateway_healthy":true,"gateway_ready":true,"restart_count":3,"gateway_session_key":"old","harness_healthy":false,"harness_ready":false,"harness_restart_count":0,"harness_session_key":""}`, false, "false", 0, `""`},
		{"nonzero values win", `{"gateway_healthy":false,"gateway_ready":false,"restart_count":0,"gateway_session_key":"old","harness_healthy":true,"harness_ready":true,"harness_restart_count":3,"harness_session_key":"new"}`, true, "true", 3, `"new"`},
		{"null falls back", `{"gateway_healthy":true,"gateway_ready":false,"restart_count":3,"gateway_session_key":"old","harness_healthy":null,"harness_ready":null,"harness_restart_count":null,"harness_session_key":null}`, true, "false", 3, `"old"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hb struct{ heartbeatHarnessFields }
			if err := json.Unmarshal([]byte(tc.payload), &hb); err != nil {
				t.Fatal(err)
			}
			hb.normalizeHarnessFields()
			ready, _ := json.Marshal(hb.GatewayReady)
			key, _ := json.Marshal(hb.GatewaySessionKey)
			if hb.GatewayHealthy != tc.healthy || string(ready) != tc.ready || hb.RestartCount != tc.restarts || string(key) != tc.key {
				t.Fatalf("normalized heartbeat = %+v, ready=%s, key=%s; want healthy=%v ready=%s restarts=%d key=%s", hb, ready, key, tc.healthy, tc.ready, tc.restarts, tc.key)
			}
		})
	}
}

func TestHarnessHeartbeatCompatibility(t *testing.T) {
	for _, prefix := range []string{"gateway", "harness"} {
		t.Run(prefix, func(t *testing.T) {
			f := newSessionLossFixture(t, nil)
			f.cc.mu.Lock()
			f.cc.gatewayReady = false
			f.cc.connectedAt = time.Now().Add(-time.Hour)
			f.cc.deliveryInFlight = true // Keep recovery prompts queued.
			f.cc.mu.Unlock()
			if _, err := f.db.Exec(`UPDATE claws SET status='starting' WHERE id=?`, f.clawID); err != nil {
				t.Fatal(err)
			}
			restartField := "restart_count"
			if prefix == "harness" {
				restartField = "harness_restart_count"
			}
			for i, step := range []struct {
				healthy, ready bool
				restarts       int
				key            string
			}{
				{false, false, 4, "baseline"},
				{true, false, 4, "baseline"},
				{true, true, 4, "baseline"},
				{true, true, 4, "rotated"},
				{true, true, 5, "rotated"},
			} {
				payload := map[string]any{
					prefix + "_healthy": step.healthy, prefix + "_ready": step.ready,
					restartField: step.restarts, prefix + "_session_key": step.key,
					"context_usage": i + 1, "session_key": "stale-usage-snapshot",
				}
				if err := wsjson.Write(context.Background(), f.conn, types.WSMessage{Type: "heartbeat", Payload: payload}); err != nil {
					t.Fatal(err)
				}
				eventuallyWatchdog(t, func() bool {
					f.cc.mu.RLock()
					defer f.cc.mu.RUnlock()
					return f.cc.contextUsage == i+1
				}, "heartbeat processed")
				f.cc.mu.RLock()
				ready, healthy := f.cc.gatewayReady, !f.cc.gatewayHealthySince.IsZero()
				restarts, key, seen := f.cc.gatewayRestartCount, f.cc.gatewaySessionKey, f.cc.gatewaySessionKeySeen
				f.cc.mu.RUnlock()
				if ready != step.ready || healthy != (step.healthy && step.ready) || restarts != step.restarts || key != step.key || !seen {
					t.Fatalf("step %d: ready=%v healthy=%v restarts=%d key=%q seen=%v", i, ready, healthy, restarts, key, seen)
				}
				wantUnhealthy := 0
				if !step.healthy {
					wantUnhealthy = 1
				}
				if got := f.s.gatewayUnhealthyCount(f.clawID); got != wantUnhealthy {
					t.Fatalf("unhealthy count = %d, want %d", got, wantUnhealthy)
				}
				wantStatus := initialStatus(&step.ready)
				eventuallyWatchdog(t, func() bool {
					var status string
					return f.db.QueryRow(`SELECT status FROM claws WHERE id=?`, f.clawID).Scan(&status) == nil && status == wantStatus
				}, "readiness status")
				if i == 2 {
					f.s.mu.RLock()
					losses := f.s.sessionLosses[f.clawID]
					baseline := f.s.gatewayRestartCounts[f.clawID]
					f.s.mu.RUnlock()
					if losses != nil || baseline != 4 {
						t.Fatal("first heartbeat must establish a restart/session baseline without session loss")
					}
				}
				if i == 3 {
					eventuallyWatchdog(t, func() bool {
						f.s.mu.RLock()
						defer f.s.mu.RUnlock()
						losses := f.s.sessionLosses[f.clawID]
						return losses != nil && losses.announced["rotated"]
					}, "session loss on live session key change")
				}
			}
		})
	}
}

func TestRegisterHarnessReadyCompatibility(t *testing.T) {
	for _, field := range []string{"gateway_ready", "harness_ready"} {
		for _, ready := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s=%v", field, ready), func(t *testing.T) {
				s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
				const id = "harness-register-test"
				if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,bootstrap_ok,created_at) VALUES(?,?,?,?,?,?)`, id, "test-tenant-id", id, "starting", 1, now()); err != nil {
					t.Fatal(err)
				}
				// Suppress the asynchronous initial wake after registration.
				seedSessionResumeMessage(t, db, id, "claw", "Already started", now())
				ts := httptest.NewServer(s.Handler())
				t.Cleanup(ts.Close)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "done") })
				payload := map[string]any{"claw_id": id, "name": id, "token": "claw-token", field: ready}
				if field == "harness_ready" {
					payload["harness"] = "openclaw"
				}
				if err := wsjson.Write(ctx, conn, types.WSMessage{Type: "register", Payload: payload}); err != nil {
					t.Fatal(err)
				}
				var ack types.WSMessage
				if err := wsjson.Read(ctx, conn, &ack); err != nil || ack.Type != "registered" {
					t.Fatalf("registration ack = %q, err = %v", ack.Type, err)
				}
				cc := watchdogClawConn(t, s, id)
				if cc.gatewayReady != ready || (field == "harness_ready" && cc.harness != "openclaw") {
					t.Fatalf("ready=%v harness=%q", cc.gatewayReady, cc.harness)
				}
				var status string
				if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, id).Scan(&status); err != nil || status != initialStatus(&ready) {
					t.Fatalf("status = %q, err = %v; want %q", status, err, initialStatus(&ready))
				}
			})
		}
	}
}
