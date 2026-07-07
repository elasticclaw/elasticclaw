package hub

// BenchmarkClawWSMessage measures the claw WebSocket message hot path
// (decode -> persist -> broadcast) end to end: a registered claw connection
// sends a complete "message" frame, the hub decodes it, persists it to the
// in-memory SQLite store, and broadcasts it to a connected user WebSocket.
// This is the baseline for future optimizations (re-architecture plan 3.7).
//
// Run with:
//
//	go test ./pkg/hub/ -run '^$' -bench BenchmarkClawWSMessage -benchmem

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func BenchmarkClawWSMessage(b *testing.B) {
	b.Setenv("ELASTICCLAW_HUB_CONFIG", b.TempDir()+"/hub.yaml")
	// Silence per-message hub logging during the benchmark.
	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(prevOut) })

	s, _ := NewTestServerWithConfig(b, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	ts := httptest.NewServer(s.Handler())
	b.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	wsBase := "ws" + strings.TrimPrefix(ts.URL, "http")

	// User connection that receives the broadcast.
	userWS, _, err := websocket.Dial(ctx, wsBase+"/api/ws?token=test-token", nil)
	if err != nil {
		b.Fatalf("dial user ws: %v", err)
	}
	b.Cleanup(func() { _ = userWS.Close(websocket.StatusNormalClosure, "done") })

	// Claw connection that produces messages.
	clawWS, _, err := websocket.Dial(ctx, wsBase+"/claw/ws", nil)
	if err != nil {
		b.Fatalf("dial claw ws: %v", err)
	}
	b.Cleanup(func() { _ = clawWS.Close(websocket.StatusNormalClosure, "done") })

	ready := true
	if err := wsjson.Write(ctx, clawWS, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID:       "claw-bench",
			Name:         "bench claw",
			Template:     "elasticclaw",
			Token:        "claw-token",
			GatewayReady: &ready,
		},
	}); err != nil {
		b.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(ctx, clawWS, &registered); err != nil || registered.Type != "registered" {
		b.Fatalf("registration ack = %+v, err = %v", registered, err)
	}

	// readBroadcast reads user WS frames until it sees the persisted claw
	// message with the given content (skipping claw_status/typing events).
	readBroadcast := func(content string) {
		for {
			var msg types.WSMessage
			if err := wsjson.Read(ctx, userWS, &msg); err != nil {
				b.Fatalf("read broadcast: %v", err)
			}
			if msg.Type != "message" {
				continue
			}
			payload, _ := json.Marshal(msg.Payload)
			var hm types.HubMessage
			if err := json.Unmarshal(payload, &hm); err == nil && hm.Content == content {
				return
			}
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		content := fmt.Sprintf("benchmark message %d", i)
		if err := wsjson.Write(ctx, clawWS, types.WSMessage{
			Type:    "message",
			Payload: types.HubMessage{Content: content},
		}); err != nil {
			b.Fatalf("write message %d: %v", i, err)
		}
		readBroadcast(content)
	}
	b.StopTimer()
}
