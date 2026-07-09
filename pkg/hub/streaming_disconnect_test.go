package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"net/http/httptest"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// TestMidStreamDisconnectPreservesReceivedContent is the phase-2 item 2.3
// streaming acceptance test: when the claw bridge drops mid-stream, the
// content received so far must be persisted (as an interrupted message)
// instead of being lost with the in-memory buffer.
func TestMidStreamDisconnectPreservesReceivedContent(t *testing.T) {
	ready := true
	clawID := "claw-mid-stream-disconnect"
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	clawWS, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatalf("dial claw ws: %v", err)
	}
	if err := wsjson.Write(ctx, clawWS, types.WSMessage{
		Type: "register",
		Payload: types.RegisterPayload{
			ClawID:       clawID,
			Name:         "claw stream",
			Template:     "elasticclaw",
			Token:        "claw-token",
			GatewayReady: &ready,
		},
	}); err != nil {
		t.Fatalf("register claw: %v", err)
	}
	var registered types.WSMessage
	if err := wsjson.Read(ctx, clawWS, &registered); err != nil {
		t.Fatalf("read registration ack: %v", err)
	}
	if registered.Type != "registered" {
		t.Fatalf("registration ack type = %q, want registered", registered.Type)
	}

	// Stream two chunks, then drop the connection before the final
	// "message" ever arrives.
	for _, chunk := range []string{"partial answer, first half ", "and the second half"} {
		if err := wsjson.Write(ctx, clawWS, types.WSMessage{
			Type:    "chunk",
			Payload: map[string]string{"content": chunk},
		}); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
	}
	// Wait until the hub buffered both chunks before killing the socket.
	deadline := time.Now().Add(5 * time.Second)
	for {
		cc, ok := s.clawReg.Get(clawID)
		if ok {
			cc.Mu.RLock()
			buffered := cc.StreamingBuf.Len()
			cc.Mu.RUnlock()
			if buffered >= len("partial answer, first half and the second half") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("hub never buffered the streamed chunks")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Abrupt mid-stream disconnect.
	_ = clawWS.Close(websocket.StatusAbnormalClosure, "bridge crashed")

	// The disconnect cleanup must persist the partial content.
	want := "partial answer, first half and the second half"
	deadline = time.Now().Add(5 * time.Second)
	for {
		var content string
		err := db.QueryRow(
			`SELECT content FROM messages WHERE claw_id=? AND role='claw' ORDER BY created_at DESC LIMIT 1`,
			clawID,
		).Scan(&content)
		if err == nil && strings.Contains(content, want) {
			if !strings.Contains(content, "[interrupted]") {
				t.Fatalf("persisted partial message not marked interrupted: %q", content)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("partial streaming content was not persisted after disconnect (last err=%v, content=%q)", err, content)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
