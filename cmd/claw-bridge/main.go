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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// ─── hub wire types ─────────────────────────────────────────────────────────

type hubMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ─── openclaw gateway wire types ────────────────────────────────────────────

type gwFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Event   string          `json:"event,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	OK      bool            `json:"ok,omitempty"`
	Error   *gwError        `json:"error,omitempty"`
	Seq     int             `json:"seq,omitempty"`
}

type gwError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ─── openclaw config structs ─────────────────────────────────────────────────

type openclawConfig struct {
	Gateway struct {
		Auth struct {
			Mode     string `json:"mode"`
			Token    string `json:"token"`
			Password string `json:"password"`
		} `json:"auth"`
		Port int `json:"port"`
	} `json:"gateway"`
}

type deviceIdentity struct {
	DeviceID      string `json:"deviceId"`
	PublicKeyPem  string `json:"publicKeyPem"`
	PrivateKeyPem string `json:"privateKeyPem"`
}

// ─── openclaw gateway client ─────────────────────────────────────────────────

type gatewayClient struct {
	addr     string
	token    string   // gateway auth token (may be empty for password auth)
	password string   // gateway auth password (may be empty for token auth)
	device   *deviceIdentity
}

func loadGatewayClient(addr string) (*gatewayClient, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}

	cfgPath := filepath.Join(home, ".openclaw", "openclaw.json")
	cfgData, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read openclaw.json: %w", err)
	}
	var cfg openclawConfig
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return nil, fmt.Errorf("parse openclaw.json: %w", err)
	}

	devPath := filepath.Join(home, ".openclaw", "identity", "device.json")
	devData, err := os.ReadFile(devPath)
	if err != nil {
		return nil, fmt.Errorf("read device.json: %w", err)
	}
	var dev deviceIdentity
	if err := json.Unmarshal(devData, &dev); err != nil {
		return nil, fmt.Errorf("parse device.json: %w", err)
	}

	// ELASTICCLAW_GATEWAY_PASSWORD env var overrides config (set by bootstrap script)
	password := cfg.Gateway.Auth.Password
	if envPw := os.Getenv("ELASTICCLAW_GATEWAY_PASSWORD"); envPw != "" {
		password = envPw
	}
	token := cfg.Gateway.Auth.Token
	// When using password auth, clear token so device payload signature is empty-token
	if password != "" {
		token = ""
	}

	return &gatewayClient{
		addr:     addr,
		token:    token,
		password: password,
		device:   &dev,
	}, nil
}

// randomID generates a UUID-like random ID.
func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// base64URLEncode returns standard base64url (no padding).
func base64URLEncode(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// ed25519Sign signs payload with the PEM-encoded private key.
func ed25519Sign(privateKeyPem string, payload []byte) (string, error) {
	block, _ := pem.Decode([]byte(privateKeyPem))
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block")
	}
	// PKCS#8 Ed25519 private key: last 32 bytes are the seed
	if len(block.Bytes) < 32 {
		return "", fmt.Errorf("PEM key too short: %d bytes", len(block.Bytes))
	}
	seed := block.Bytes[len(block.Bytes)-32:]
	privKey := ed25519.NewKeyFromSeed(seed)
	sig := ed25519.Sign(privKey, payload)
	return base64URLEncode(sig), nil
}

// ed25519PublicKeyRaw extracts raw 32-byte public key from PEM.
func ed25519PublicKeyRaw(publicKeyPem string) (string, error) {
	block, _ := pem.Decode([]byte(publicKeyPem))
	if block == nil {
		return "", fmt.Errorf("failed to decode public key PEM")
	}
	// SPKI Ed25519: last 32 bytes are the raw key
	if len(block.Bytes) < 32 {
		return "", fmt.Errorf("public key PEM too short")
	}
	raw := block.Bytes[len(block.Bytes)-32:]
	return base64URLEncode(raw), nil
}

// buildDevicePayloadV3 matches OpenClaw's buildDeviceAuthPayloadV3.
// token is the gateway auth token (empty string for password auth).
func buildDevicePayloadV3(deviceID, clientID, clientMode, role string, scopes []string, signedAtMs int64, token, nonce, platform string) string {
	return strings.Join([]string{
		"v3",
		deviceID,
		clientID,
		clientMode,
		role,
		strings.Join(scopes, ","),
		fmt.Sprintf("%d", signedAtMs),
		token,
		nonce,
		platform,
		"", // deviceFamily
	}, "|")
}

const (
	clientID   = "cli"
	clientMode = "cli"
	gwRole     = "operator"
)

var defaultScopes = []string{
	"operator.admin",
	"operator.read",
	"operator.write",
	"operator.approvals",
	"operator.pairing",
}

// connectToGateway opens a WebSocket to the gateway, performs the auth
// handshake, and returns the live connection.
func (gc *gatewayClient) connectToGateway(ctx context.Context) (*websocket.Conn, error) {
	gwURL := fmt.Sprintf("ws://%s", gc.addr)
	conn, _, err := websocket.Dial(ctx, gwURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": {"claw-bridge/1.0"}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial gateway: %w", err)
	}

	// Expect connect.challenge event
	var challenge gwFrame
	if err := wsjson.Read(ctx, conn, &challenge); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("read challenge: %w", err)
	}
	if challenge.Event != "connect.challenge" {
		conn.CloseNow()
		return nil, fmt.Errorf("expected connect.challenge, got event=%q", challenge.Event)
	}

	var chalPayload struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(challenge.Payload, &chalPayload); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("parse challenge payload: %w", err)
	}
	nonce := chalPayload.Nonce
	signedAtMs := time.Now().UnixMilli()
	instanceID := randomID()

	// Build device signature
	// signatureToken = authToken when using token auth; empty when using password auth
	signatureToken := gc.token // empty string for password auth
	payloadStr := buildDevicePayloadV3(
		gc.device.DeviceID, clientID, clientMode, gwRole, defaultScopes,
		signedAtMs, signatureToken, nonce, "linux",
	)
	sig, err := ed25519Sign(gc.device.PrivateKeyPem, []byte(payloadStr))
	if err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("sign device payload: %w", err)
	}
	pubKey, err := ed25519PublicKeyRaw(gc.device.PublicKeyPem)
	if err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("extract public key: %w", err)
	}

	// Build auth object
	var auth map[string]interface{}
	if gc.password != "" {
		auth = map[string]interface{}{"password": gc.password}
	} else {
		auth = map[string]interface{}{"token": gc.token}
	}

	connectParams := map[string]interface{}{
		"minProtocol": 3,
		"maxProtocol": 3,
		"client": map[string]interface{}{
			"id":         clientID,
			"version":    "1.0.0",
			"platform":   "linux",
			"mode":       clientMode,
			"instanceId": instanceID,
		},
		"caps":   []string{},
		"auth":   auth,
		"role":   gwRole,
		"scopes": defaultScopes,
		"device": map[string]interface{}{
			"id":        gc.device.DeviceID,
			"publicKey": pubKey,
			"signature": sig,
			"signedAt":  signedAtMs,
			"nonce":     nonce,
		},
	}
	connectParamsJSON, _ := json.Marshal(connectParams)

	reqID := randomID()
	connectReq := gwFrame{
		Type:   "req",
		ID:     reqID,
		Method: "connect",
		Params: connectParamsJSON,
	}
	if err := wsjson.Write(ctx, conn, connectReq); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("send connect: %w", err)
	}

	// Read responses until we get our connect reply
	for {
		var resp gwFrame
		if err := wsjson.Read(ctx, conn, &resp); err != nil {
			conn.CloseNow()
			return nil, fmt.Errorf("read connect response: %w", err)
		}
		if resp.Type == "res" && resp.ID == reqID {
			if !resp.OK {
				msg := "unknown error"
				if resp.Error != nil {
					msg = resp.Error.Message
				}
				conn.CloseNow()
				return nil, fmt.Errorf("connect rejected: %s", msg)
			}
			log.Printf("[gateway] connected (protocol 3)")
			return conn, nil
		}
		// Skip health/tick events during handshake
	}
}

// sendAgentMessage sends a message to the gateway, streams chunks via onChunk,
// and returns the full response text.
func (gc *gatewayClient) sendAgentMessage(ctx context.Context, message string, onChunk func(string)) (string, error) {
	conn, err := gc.connectToGateway(ctx)
	if err != nil {
		return "", fmt.Errorf("gateway connect: %w", err)
	}
	defer conn.CloseNow()

	// Create a new session
	sessionID := "bridge-" + randomID()
	createParams, _ := json.Marshal(map[string]string{"agentId": "main"})
	createID := randomID()
	if err := wsjson.Write(ctx, conn, gwFrame{
		Type: "req", ID: createID, Method: "sessions.create", Params: createParams,
	}); err != nil {
		return "", fmt.Errorf("sessions.create write: %w", err)
	}
	var sessionKey string
	for {
		var resp gwFrame
		if err := wsjson.Read(ctx, conn, &resp); err != nil {
			return "", fmt.Errorf("sessions.create read: %w", err)
		}
		if resp.Type == "res" && resp.ID == createID {
			if !resp.OK {
				msg := "unknown"
				if resp.Error != nil {
					msg = resp.Error.Message
				}
				return "", fmt.Errorf("sessions.create failed: %s", msg)
			}
			var payload struct {
				Key string `json:"key"`
			}
			json.Unmarshal(resp.Payload, &payload)
			sessionKey = payload.Key
			break
		}
	}
	log.Printf("[gateway] created session: %s (internal: %s)", sessionKey, sessionID)

	// Subscribe to session events
	subParams, _ := json.Marshal(map[string]string{"sessionKey": sessionKey})
	subID := randomID()
	if err := wsjson.Write(ctx, conn, gwFrame{
		Type: "req", ID: subID, Method: "sessions.subscribe", Params: subParams,
	}); err != nil {
		return "", fmt.Errorf("sessions.subscribe write: %w", err)
	}
	for {
		var resp gwFrame
		if err := wsjson.Read(ctx, conn, &resp); err != nil {
			return "", fmt.Errorf("sessions.subscribe read: %w", err)
		}
		if resp.Type == "res" && resp.ID == subID {
			if !resp.OK {
				msg := "unknown"
				if resp.Error != nil {
					msg = resp.Error.Message
				}
				return "", fmt.Errorf("sessions.subscribe failed: %s", msg)
			}
			break
		}
	}

	// Send the message
	sendParams, _ := json.Marshal(map[string]string{"key": sessionKey, "message": message})
	sendID := randomID()
	if err := wsjson.Write(ctx, conn, gwFrame{
		Type: "req", ID: sendID, Method: "sessions.send", Params: sendParams,
	}); err != nil {
		return "", fmt.Errorf("sessions.send write: %w", err)
	}

	// Wait for send ack
	for {
		var resp gwFrame
		if err := wsjson.Read(ctx, conn, &resp); err != nil {
			return "", fmt.Errorf("sessions.send read: %w", err)
		}
		if resp.Type == "res" && resp.ID == sendID {
			if !resp.OK {
				msg := "unknown"
				if resp.Error != nil {
					msg = resp.Error.Message
				}
				return "", fmt.Errorf("sessions.send failed: %s", msg)
			}
			break
		}
	}
	log.Printf("[gateway] message sent, streaming response...")

	// Stream agent events until turn done
	var fullText strings.Builder
	for {
		var frame gwFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return "", fmt.Errorf("stream read: %w", err)
		}
		if frame.Type != "event" {
			continue
		}

		// agent stream events carry delta chunks
		if frame.Event == "agent" {
			var agentPayload struct {
				Stream     string `json:"stream"`
				SessionKey string `json:"sessionKey"`
				Data       struct {
					Delta string `json:"delta"`
					Phase string `json:"phase"`
				} `json:"data"`
			}
			if err := json.Unmarshal(frame.Payload, &agentPayload); err != nil {
				continue
			}
			// Only process events for our session
			if agentPayload.SessionKey != sessionKey {
				continue
			}
			if agentPayload.Stream == "assistant" && agentPayload.Data.Delta != "" {
				fullText.WriteString(agentPayload.Data.Delta)
				if onChunk != nil {
					onChunk(agentPayload.Data.Delta)
				}
			}
			if agentPayload.Stream == "lifecycle" && agentPayload.Data.Phase == "end" {
				log.Printf("[gateway] agent turn complete")
				return strings.TrimSpace(fullText.String()), nil
			}
		}
	}
}

// ─── main ────────────────────────────────────────────────────────────────────

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

	// Load gateway client config
	gwClient, err := loadGatewayClient(gatewayAddr)
	if err != nil {
		log.Fatalf("Failed to load gateway config: %v", err)
	}
	log.Printf("  Device:  %s", gwClient.device.DeviceID[:16]+"...")

	if checkGateway(gatewayAddr) {
		log.Printf("  ✓  gateway healthy at %s", gatewayAddr)
	} else {
		log.Printf("  ⚠️  gateway not responding at %s (will retry)", gatewayAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for {
		if err := runHubLoop(ctx, wsURL, clawID, clawName, templateName, token, gwClient); err != nil {
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

func runHubLoop(ctx context.Context, wsURL, clawID, clawName, templateName, token string, gwClient *gatewayClient) error {
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": {"claw-bridge/1.0"}},
	})
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}
	defer conn.CloseNow()

	// Register with the hub
	reg := hubMsg{
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
	var ack hubMsg
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
				health := checkGateway(gwClient.addr)
				_ = wsjson.Write(ctx, conn, hubMsg{
					Type:    "heartbeat",
					Payload: mustJSON(map[string]interface{}{"gateway_healthy": health}),
				})
			}
		}
	}()

	// Main read loop
	for {
		var msg hubMsg
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		log.Printf("recv type=%s", msg.Type)
		switch msg.Type {
		case "message":
			go func(connCtx context.Context, payload json.RawMessage) {
				var m map[string]interface{}
				if err := json.Unmarshal(payload, &m); err != nil {
					log.Printf("payload unmarshal error: %v", err)
					return
				}
				content, _ := m["content"].(string)
				if content == "" {
					return
				}

				agentCtx, agentCancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer agentCancel()

				log.Printf("[bridge] → openclaw: %q", content[:min(len(content), 80)])

				reply, agentErr := gwClient.sendAgentMessage(agentCtx, content, func(chunk string) {
					_ = wsjson.Write(connCtx, conn, hubMsg{
						Type: "chunk",
						Payload: mustJSON(map[string]interface{}{
							"role":    "claw",
							"content": chunk,
						}),
					})
				})
				if agentErr != nil {
					log.Printf("[bridge] ✗ agent error: %v", agentErr)
					reply = fmt.Sprintf("⚠️ claw-bridge error: %v", agentErr)
				} else {
					log.Printf("[bridge] ← openclaw: %q", reply[:min(len(reply), 120)])
				}

				_ = wsjson.Write(connCtx, conn, hubMsg{
					Type: "message",
					Payload: mustJSON(map[string]interface{}{
						"role":    "claw",
						"content": reply,
					}),
				})
			}(ctx, msg.Payload)

		default:
			// ignore unknown message types
		}
	}
}

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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
