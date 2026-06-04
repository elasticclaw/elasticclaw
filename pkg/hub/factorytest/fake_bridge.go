package factorytest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type ReceivedMessage struct {
	Type    string
	Content string
	Role    string
}

type FakeBridge struct {
	ClawID    string
	HubURL    string
	ClawToken string
	mu        sync.Mutex
	Messages  []ReceivedMessage
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
}

func ConnectFakeBridge(t *testing.T, hubURL, clawID, clawName, clawToken string) *FakeBridge {
	t.Helper()
	wsURL := strings.Replace(hubURL, "http://", "ws://", 1) + "/claw/ws"
	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Claw-Token": []string{clawToken}},
	})
	if err != nil {
		cancel()
		t.Fatalf("FakeBridge dial: %v", err)
	}
	gatewayReady := true
	reg := map[string]interface{}{
		"type": "register",
		"payload": map[string]interface{}{
			"claw_id":       clawID,
			"name":          clawName,
			"template":      "test",
			"token":         clawToken,
			"gateway_ready": &gatewayReady,
		},
	}
	if err := wsjson.Write(ctx, conn, reg); err != nil {
		cancel()
		t.Fatalf("FakeBridge register: %v", err)
	}
	fb := &FakeBridge{ClawID: clawID, HubURL: hubURL, ClawToken: clawToken, conn: conn, ctx: ctx, cancel: cancel}
	go fb.readLoop()
	t.Cleanup(fb.Disconnect)
	return fb
}

func (fb *FakeBridge) readLoop() {
	for {
		var msg map[string]interface{}
		if err := wsjson.Read(fb.ctx, fb.conn, &msg); err != nil {
			return
		}
		msgType, _ := msg["type"].(string)
		rm := ReceivedMessage{Type: msgType}
		if payload, ok := msg["payload"].(map[string]interface{}); ok {
			rm.Content, _ = payload["content"].(string)
			rm.Role, _ = payload["role"].(string)
		}
		fb.mu.Lock()
		fb.Messages = append(fb.Messages, rm)
		fb.mu.Unlock()

		// Respond to checkpoint requests so termination doesn't block
		if msgType == "checkpoint_create" {
			var checkpointID string
			if p, ok := msg["payload"].(map[string]interface{}); ok {
				checkpointID, _ = p["checkpoint_id"].(string)
			}
			// Complete checkpoint via HTTP API
			if checkpointID != "" {
				go fb.completeCheckpoint(checkpointID)
			}
		}
	}
}

func (fb *FakeBridge) SendMessage(content string) {
	msg := types.HubMessage{
		ID:      uuid.New().String(),
		ClawID:  fb.ClawID,
		Role:    "claw",
		Content: content,
	}
	_ = wsjson.Write(fb.ctx, fb.conn, map[string]interface{}{
		"type":    "message",
		"payload": msg,
	})
}

func (fb *FakeBridge) SendDone(prURL string) {
	fb.SendMessage(fmt.Sprintf("[DONE] %s", prURL))
}

func (fb *FakeBridge) WaitForMessage(t *testing.T, contains string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		fb.mu.Lock()
		for _, m := range fb.Messages {
			if strings.Contains(m.Content, contains) {
				fb.mu.Unlock()
				return m.Content
			}
		}
		fb.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("FakeBridge: timed out waiting for message containing %q", contains)
	return ""
}

func (fb *FakeBridge) completeCheckpoint(checkpointID string) {
	// POST to checkpoint complete endpoint
	url := fmt.Sprintf("%s/api/checkpoints/%s/complete?claw_token=%s", fb.HubURL, checkpointID, fb.ClawToken)
	body := map[string]interface{}{"root_sha256": ""}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[FakeBridge] checkpoint complete error: %v\n", err)
		return
	}
	resp.Body.Close()
	fmt.Printf("[FakeBridge] checkpoint complete status: %d\n", resp.StatusCode)
}

func (fb *FakeBridge) Disconnect() {
	fb.cancel()
	fb.conn.Close(websocket.StatusNormalClosure, "test done")
}
