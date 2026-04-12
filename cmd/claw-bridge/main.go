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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for {
		if err := run(ctx, wsURL, clawID, clawName, templateName, token, gatewayAddr); err != nil {
			if ctx.Err() != nil {
				log.Printf("shutting down")
				return
			}
			log.Printf("disconnected: %v — reconnecting in 5s", err)
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

		switch msg.Type {
		case "message":
			// Forward to local OpenClaw gateway and stream response back
			go func(payload json.RawMessage) {
				var m map[string]interface{}
				if err := json.Unmarshal(payload, &m); err != nil {
					return
				}
				content, _ := m["content"].(string)
				if content == "" {
					return
				}

				reply, err := forwardToGateway(ctx, gatewayAddr, content)
				if err != nil {
					log.Printf("gateway error: %v", err)
					reply = fmt.Sprintf("[error: %v]", err)
				}

				replyMsg := wsMsg{
					Type: "message",
					Payload: mustJSON(map[string]interface{}{
						"role":    "claw",
						"content": reply,
					}),
				}
				_ = wsjson.Write(ctx, conn, replyMsg)
			}(msg.Payload)

		default:
			// ignore unknown message types
		}
	}
}

// forwardToGateway sends a message to the local OpenClaw gateway using the
// openclaw CLI and returns the response text.
func forwardToGateway(ctx context.Context, _ string, message string) (string, error) {
	// Use openclaw agent --local to send a message and get a response.
	// The --local flag connects to the local gateway without needing channel config.
	cmd := exec.CommandContext(ctx, "openclaw", "agent", "--local", "--message", message)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("openclaw agent error: %w\nstderr: %s", err, stderr.String())
	}
	reply := strings.TrimSpace(stdout.String())
	if reply == "" {
		return "", fmt.Errorf("empty response from openclaw agent")
	}
	return reply, nil
}

// checkGateway returns true if the local OpenClaw gateway is responding.
func checkGateway(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
