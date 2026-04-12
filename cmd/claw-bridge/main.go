// claw-bridge runs on a provisioned VM and connects the local OpenClaw gateway
// to the ElasticClaw hub via WebSocket, proxying messages bidirectionally.
//
// Environment variables:
//
//	ELASTICCLAW_HUB_URL    - hub WebSocket URL (e.g. ws://hub.example.com)
//	ELASTICCLAW_CLAW_ID    - claw ID assigned by the hub
//	ELASTICCLAW_CLAW_TOKEN - authentication token for the hub
//	ELASTICCLAW_GATEWAY    - local OpenClaw gateway address (default: localhost:18789)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

type wsMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func main() {
	hubURL := mustEnv("ELASTICCLAW_HUB_URL")
	clawID := mustEnv("ELASTICCLAW_CLAW_ID")
	token := mustEnv("ELASTICCLAW_CLAW_TOKEN")
	gatewayAddr := envOr("ELASTICCLAW_GATEWAY", "localhost:18789")
	clawName := envOr("ELASTICCLAW_CLAW_NAME", clawID)
	templateName := envOr("ELASTICCLAW_TEMPLATE", "")

	// Normalise hub URL to ws:// scheme
	wsURL := strings.TrimRight(hubURL, "/")
	if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[7:]
	} else if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[8:]
	}
	wsURL += "/claw/ws"

	log.Printf("claw-bridge starting")
	log.Printf("  Hub:     %s", wsURL)
	log.Printf("  Claw ID: %s", clawID)
	log.Printf("  Gateway: %s", gatewayAddr)

	// Startup checks
	if _, err := exec.LookPath("openclaw"); err != nil {
		log.Printf("  ⚠️  openclaw not found in PATH: %v", err)
	} else {
		log.Printf("  ✓  openclaw found")
	}
	if checkGateway(gatewayAddr) {
		log.Printf("  ✓  gateway healthy at %s", gatewayAddr)
	} else {
		log.Printf("  ⚠️  gateway not responding at %s (will retry)", gatewayAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for {
		if err := run(ctx, wsURL, clawID, clawName, templateName, token, gatewayAddr); err != nil {
			if ctx.Err() != nil {
				log.Printf("shutting down")
				return
			}
			log.Printf("[bridge] disconnected from hub: %v", err)
			if !checkGateway(gatewayAddr) {
				log.Printf("[bridge] ⚠️  openclaw gateway not responding at %s", gatewayAddr)
			}
			log.Printf("[bridge] reconnecting in 5s...")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func run(ctx context.Context, wsURL, clawID, clawName, templateName, token, gatewayAddr string) error {
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": {"claw-bridge/1.0"}},
	})
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}
	defer conn.CloseNow()

	// Register with the hub
	reg := wsMsg{
		Type: "register",
		Payload: mustJSON(map[string]string{
			"claw_id":  clawID,
			"name":     clawName,
			"template": templateName,
			"token":    token,
		}),
	}
	if err := wsjson.Write(ctx, conn, reg); err != nil {
		return fmt.Errorf("register: %w", err)
	}

	// Expect registered ack
	var ack wsMsg
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		return fmt.Errorf("read ack: %w", err)
	}
	if ack.Type != "registered" {
		return fmt.Errorf("expected registered, got %s", ack.Type)
	}
	log.Printf("registered with hub as %s", clawID)

	// Heartbeat goroutine
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				health := checkGateway(gatewayAddr)
				_ = wsjson.Write(ctx, conn, wsMsg{
					Type:    "heartbeat",
					Payload: mustJSON(map[string]interface{}{"gateway_healthy": health}),
				})
			}
		}
	}()

	// Main read loop — forward hub messages to local OpenClaw gateway
	for {
		var msg wsMsg
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		log.Printf("recv type=%s payload=%s", msg.Type, string(msg.Payload))
		switch msg.Type {
		case "message":
			// Forward to local OpenClaw gateway and stream response back
			go func(connCtx context.Context, payload json.RawMessage) {
				var m map[string]interface{}
				if err := json.Unmarshal(payload, &m); err != nil {
					log.Printf("payload unmarshal error: %v, raw: %s", err, string(payload))
					return
				}
				content, _ := m["content"].(string)
				log.Printf("forwarding content: %q", content)
				if content == "" {
					log.Printf("empty content, skipping")
					return
				}

				// Use an independent context so the agent call isn't cancelled when the WS read loop cancels
				agentCtx, agentCancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer agentCancel()
				log.Printf("[bridge] → openclaw: %q", content)
				var reply string
				var agentErr error
				reply, agentErr = forwardToGatewayStreaming(agentCtx, content, func(chunk string) {
					// Send each chunk to the hub as a 'chunk' event
					chunkMsg := wsMsg{
						Type: "chunk",
						Payload: mustJSON(map[string]interface{}{
							"role":    "claw",
							"content": chunk,
						}),
					}
					_ = wsjson.Write(connCtx, conn, chunkMsg)
				})
				if agentErr != nil {
					log.Printf("[bridge] ✗ agent error: %v", agentErr)
					reply = fmt.Sprintf("⚠️ claw-bridge error: %v", agentErr)
				} else {
					log.Printf("[bridge] ← openclaw: %q", reply[:min(len(reply), 120)])
				}

				replyMsg := wsMsg{
					Type: "message",
					Payload: mustJSON(map[string]interface{}{
						"role":    "claw",
						"content": reply,
					}),
				}
				_ = wsjson.Write(connCtx, conn, replyMsg)
			}(ctx, msg.Payload)

		default:
			// ignore unknown message types
		}
	}
}

// forwardToGateway runs openclaw agent and streams stdout chunks to onChunk as they arrive.
// Returns the full reply text when done.
func forwardToGateway(ctx context.Context, _ string, message string) (string, error) {
	cmd := exec.CommandContext(ctx, "openclaw", "agent", "--local", "--session-id", "main", "--message", message)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}
	var buf bytes.Buffer
	readBuf := make([]byte, 256)
	for {
		n, readErr := stdout.Read(readBuf)
		if n > 0 {
			buf.Write(readBuf[:n])
		}
		if readErr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("openclaw agent error: %w\nstderr: %s", err, stderr.String())
	}
	reply := strings.TrimSpace(buf.String())
	if reply == "" {
		return "", fmt.Errorf("empty response from openclaw agent")
	}
	return reply, nil
}

// forwardToGatewayStreaming is like forwardToGateway but calls onChunk with each
// chunk as it arrives so the caller can stream to the hub in real time.
func forwardToGatewayStreaming(ctx context.Context, message string, onChunk func(chunk string)) (string, error) {
	cmd := exec.CommandContext(ctx, "openclaw", "agent", "--local", "--session-id", "main", "--message", message)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start: %w", err)
	}
	var buf bytes.Buffer
	readBuf := make([]byte, 64) // small reads for lower latency
	for {
		n, readErr := stdout.Read(readBuf)
		if n > 0 {
			chunk := string(readBuf[:n])
			buf.WriteString(chunk)
			if onChunk != nil {
				onChunk(chunk)
			}
		}
		if readErr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("openclaw agent error: %w\nstderr: %s", err, stderr.String())
	}
	return strings.TrimSpace(buf.String()), nil
}

// checkGateway returns true if the local OpenClaw gateway is responding.
func checkGateway(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s/healthz", addr), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
