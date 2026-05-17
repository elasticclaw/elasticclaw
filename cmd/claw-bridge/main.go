// claw-bridge runs on a provisioned VM and connects the local OpenClaw gateway
// to the ElasticClaw hub via WebSocket, proxying messages bidirectionally.
//
// Environment variables:
//
//	ELASTICCLAW_HUB_URL       - hub WebSocket URL (e.g. ws://hub.example.com)
//	ELASTICCLAW_CLAW_ID       - claw ID assigned by the hub
//	ELASTICCLAW_CLAW_TOKEN    - authentication token for the hub
//	ELASTICCLAW_GATEWAY       - local OpenClaw gateway address (default: localhost:18789)
//
// Bootstrap mode (--bootstrap or ELASTICCLAW_BOOTSTRAP=1):
//
//	ELASTICCLAW_BOOTSTRAP         - set to "1" to run bootstrap before normal bridge loop
//	ELASTICCLAW_NIX               - set to "true" to install Nix in background
//	ELASTICCLAW_DOCKER            - set to "true" to install Docker Engine
//	ELASTICCLAW_GATEWAY_PASSWORD  - OpenClaw gateway password
//	ELASTICCLAW_ONBOARD_FLAGS     - extra flags for openclaw onboard
//	OPENCLAW_DEFAULT_MODEL        - default agent model
//	(all LLM key env vars, LINEAR_API_KEY, etc. are passed through transparently)
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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

// ─── Inbound message queue ───────────────────────────────────────────────────
// Queues user messages when the hub connection is temporarily unavailable.
// Messages older than msgQueueTTL are dropped to prevent unbounded growth.

const msgQueueTTL = 10 * time.Minute

type queuedMsg struct {
	content  string
	queuedAt time.Time
}

type msgQueue struct {
	mu   sync.Mutex
	msgs []queuedMsg
}

func (q *msgQueue) push(content string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.msgs = append(q.msgs, queuedMsg{content: content, queuedAt: time.Now()})
}

// drain returns all non-expired messages and clears the queue.
func (q *msgQueue) drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for _, m := range q.msgs {
		if time.Since(m.queuedAt) < msgQueueTTL {
			out = append(out, m.content)
		} else {
			log.Printf("[bridge] dropped queued message (TTL exceeded): %q", m.content[:min(len(m.content), 60)])
		}
	}
	q.msgs = nil
	return out
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
		Remote struct {
			Token    string `json:"token"`
			Password string `json:"password"`
		} `json:"remote"`
		Port int `json:"port"`
	} `json:"gateway"`
}

type deviceIdentity struct {
	DeviceID      string `json:"deviceId"`
	PublicKeyPem  string `json:"publicKeyPem"`
	PrivateKeyPem string `json:"privateKeyPem"`
}

// ─── session key persistence ──────────────────────────────────────────────────

type bridgeSessionFile struct {
	SessionKey string `json:"sessionKey"`
}

func bridgeSessionPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".elasticclaw")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "bridge-session.json"), nil
}

func loadBridgeSession() string {
	path, err := bridgeSessionPath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var f bridgeSessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		return ""
	}
	return f.SessionKey
}

func saveBridgeSession(key string) {
	path, err := bridgeSessionPath()
	if err != nil {
		log.Printf("[session] failed to resolve session path: %v", err)
		return
	}
	data, _ := json.Marshal(bridgeSessionFile{SessionKey: key})
	if err := os.WriteFile(path, data, 0600); err != nil {
		log.Printf("[session] failed to save session: %v", err)
	}
}

// ─── openclaw gateway client ─────────────────────────────────────────────────

type gatewayClient struct {
	addr     string
	token    string // gateway auth token (may be empty for password auth)
	password string // gateway auth password (may be empty for token auth)
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
	var dev deviceIdentity
	if os.IsNotExist(err) {
		// device.json doesn't exist yet — the OpenClaw gateway creates it on first start.
		// Wait up to 30s for it to appear rather than generating a mismatched identity.
		// device.json should have been pre-generated during bootstrap.
		// Wait up to 120s as a fallback in case gateway creates it asynchronously.
		log.Printf("[gateway] device.json not found — waiting (up to 120s)...")
		var waitErr error
		var parseErr error
		for i := 0; i < 120; i++ {
			time.Sleep(time.Second)
			devData, waitErr = os.ReadFile(devPath)
			if waitErr != nil {
				continue
			}
			parseErr = json.Unmarshal(devData, &dev)
			if parseErr == nil {
				log.Printf("[gateway] device.json appeared after %ds", i+1)
				break
			}
		}
		if waitErr != nil {
			return nil, fmt.Errorf("device.json not found after 120s: %w", waitErr)
		}
		if parseErr != nil {
			return nil, fmt.Errorf("parse device.json: %w", parseErr)
		}
	} else if err != nil {
		return nil, fmt.Errorf("read device.json: %w", err)
	} else if err := json.Unmarshal(devData, &dev); err != nil {
		return nil, fmt.Errorf("parse device.json: %w", err)
	}

	// Determine auth based on the gateway's configured mode.
	// ELASTICCLAW_GATEWAY_PASSWORD is only used when mode is password (or unset).
	var token, password string
	switch cfg.Gateway.Auth.Mode {
	case "token":
		// Token mode: use the token from config, with remote token as fallback.
		token = cfg.Gateway.Auth.Token
		if token == "" {
			token = cfg.Gateway.Remote.Token
		}
	default:
		// Password mode (or legacy): config takes priority; env vars are fallback only.
		// The gateway generates its own auth password and writes it to gateway.auth.password.
		// Env var overrides would send the bootstrap password instead of the gateway's
		// current password, causing a mismatch.
		token = cfg.Gateway.Auth.Token
		password = cfg.Gateway.Auth.Password
		if token == "" {
			token = cfg.Gateway.Remote.Token
		}
		if password == "" {
			password = cfg.Gateway.Remote.Password
		}
		if password == "" {
			if envPw := os.Getenv("OPENCLAW_GATEWAY_PASSWORD"); envPw != "" {
				password = envPw
			}
			if envPw := os.Getenv("ELASTICCLAW_GATEWAY_PASSWORD"); envPw != "" {
				password = envPw
			}
		}
		if password != "" {
			token = ""
		}
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
	conn.SetReadLimit(32 * 1024 * 1024) // 32MB — gateway responses can be large

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
		"minProtocol": 4,
		"maxProtocol": 4,
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
			log.Printf("[gateway] connected (protocol 4)")
			return conn, nil
		}
		// Skip health/tick events during handshake
	}
}

// ─── persistent gateway session ──────────────────────────────────────────────

type agentResult struct {
	text string
	err  error
}

// inFlightState tracks a single in-progress agent turn.
type inFlightState struct {
	onChunk  func(string)
	fullText strings.Builder
	done     chan agentResult
}

// gatewaySession is a long-lived connection to the OpenClaw gateway that
// maintains a single persistent agent session across all messages.
type gatewaySession struct {
	client     *gatewayClient
	sessionKey string

	connMu sync.Mutex
	conn   *websocket.Conn

	// pending req/res tracking (reqID → response channel)
	pendMu  sync.Mutex
	pending map[string]chan gwFrame

	// in-flight message tracking (one at a time, serialised by sendMu)
	sendMu   sync.Mutex
	infMu    sync.RWMutex
	inFlight *inFlightState

	// context usage (0-100), updated after each turn
	ctxMu        sync.RWMutex
	contextUsage int

	// ready is true once the gateway session is established and ready for messages
	readyMu sync.RWMutex
	ready   bool
}

// newGatewaySession creates a gatewaySession, establishes the gateway
// connection, resolves (or creates) the persistent session, subscribes,
// and starts the background read loop.
func newGatewaySession(ctx context.Context, client *gatewayClient) (*gatewaySession, error) {
	gs := &gatewaySession{
		client:  client,
		pending: make(map[string]chan gwFrame),
	}

	if err := gs.connect(ctx); err != nil {
		return nil, err
	}

	// Start read loop BEFORE initSession — sendReq depends on the read loop
	// to dispatch responses; without it sendReq blocks forever.
	go gs.readLoop(ctx)

	if err := gs.initSession(ctx); err != nil {
		gs.conn.CloseNow()
		return nil, err
	}

	return gs, nil
}

// connect dials the gateway and stores the connection. Must be called with
// connMu unlocked.
func (gs *gatewaySession) connect(ctx context.Context) error {
	conn, err := gs.client.connectToGateway(ctx)
	if err != nil {
		return err
	}
	gs.connMu.Lock()
	gs.conn = conn
	gs.connMu.Unlock()
	return nil
}

// sendReq sends a gateway request and waits for the corresponding response.
func (gs *gatewaySession) sendReq(ctx context.Context, method string, params interface{}) (gwFrame, error) {
	paramsJSON, _ := json.Marshal(params)
	reqID := randomID()
	ch := make(chan gwFrame, 1)

	gs.pendMu.Lock()
	gs.pending[reqID] = ch
	gs.pendMu.Unlock()

	gs.connMu.Lock()
	conn := gs.conn
	gs.connMu.Unlock()

	if err := wsjson.Write(ctx, conn, gwFrame{
		Type:   "req",
		ID:     reqID,
		Method: method,
		Params: paramsJSON,
	}); err != nil {
		gs.pendMu.Lock()
		delete(gs.pending, reqID)
		gs.pendMu.Unlock()
		return gwFrame{}, fmt.Errorf("%s write: %w", method, err)
	}

	select {
	case resp := <-ch:
		if !resp.OK {
			msg := "unknown"
			if resp.Error != nil {
				msg = resp.Error.Message
			}
			return resp, fmt.Errorf("%s failed: %s", method, msg)
		}
		return resp, nil
	case <-ctx.Done():
		gs.pendMu.Lock()
		delete(gs.pending, reqID)
		gs.pendMu.Unlock()
		return gwFrame{}, ctx.Err()
	}
}

func (gs *gatewaySession) failPendingRequests(err error) {
	gs.pendMu.Lock()
	pending := gs.pending
	gs.pending = make(map[string]chan gwFrame)
	gs.pendMu.Unlock()

	for id, ch := range pending {
		ch <- gwFrame{
			Type: "res",
			ID:   id,
			Error: &gwError{
				Code:    "gateway_disconnected",
				Message: err.Error(),
			},
		}
	}
}

// initSession resolves the persistent session key (loading from disk and
// verifying, or creating a new one) and subscribes to it.
func (gs *gatewaySession) initSession(ctx context.Context) error {
	// Try to restore an existing session
	if key := loadBridgeSession(); key != "" {
		_, err := gs.sendReq(ctx, "sessions.get", map[string]string{"key": key})
		if err == nil {
			log.Printf("[session] restored existing session: %s", key)
			gs.sessionKey = key
			if err := gs.subscribe(ctx); err != nil {
				return err
			}
			gs.setReady()
			return nil
		}
		log.Printf("[session] saved session not found (%v), creating new one", err)
	}

	// Create a new session
	resp, err := gs.sendReq(ctx, "sessions.create", map[string]string{"agentId": "main"})
	if err != nil {
		return fmt.Errorf("sessions.create: %w", err)
	}
	var payload struct {
		Key string `json:"key"`
	}
	json.Unmarshal(resp.Payload, &payload)
	if payload.Key == "" {
		return fmt.Errorf("sessions.create returned empty key")
	}
	gs.sessionKey = payload.Key
	saveBridgeSession(payload.Key)
	log.Printf("[session] created new session: %s", gs.sessionKey)

	if err := gs.subscribe(ctx); err != nil {
		return err
	}
	gs.setReady()
	return nil
}

// subscribe registers for events on the current session key.
func (gs *gatewaySession) subscribe(ctx context.Context) error {
	_, err := gs.sendReq(ctx, "sessions.subscribe", map[string]string{"sessionKey": gs.sessionKey})
	if err != nil {
		return fmt.Errorf("sessions.subscribe: %w", err)
	}
	log.Printf("[session] subscribed to session events")
	return nil
}

// readLoop reads frames from the gateway forever, dispatching responses to
// pending channels and agent events to the in-flight handler.
// When the connection drops it reconnects and re-subscribes.
func (gs *gatewaySession) readLoop(ctx context.Context) {
	for {
		gs.connMu.Lock()
		conn := gs.conn
		gs.connMu.Unlock()

		var frame gwFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			log.Printf("[gateway] read error: %v — reconnecting in 3s", err)

			gs.failPendingRequests(fmt.Errorf("gateway disconnected"))

			// Fail any in-flight message
			gs.infMu.RLock()
			inf := gs.inFlight
			gs.infMu.RUnlock()
			if inf != nil {
				select {
				case inf.done <- agentResult{err: fmt.Errorf("gateway disconnected")}:
				default:
				}
			}

			time.Sleep(3 * time.Second)
			for {
				if ctx.Err() != nil {
					return
				}
				if err := gs.connect(ctx); err != nil {
					log.Printf("[gateway] reconnect failed: %v — retrying in 5s", err)
					time.Sleep(5 * time.Second)
					continue
				}
				if err := gs.subscribe(ctx); err != nil {
					log.Printf("[gateway] re-subscribe failed: %v — retrying in 5s", err)
					time.Sleep(5 * time.Second)
					continue
				}
				log.Printf("[gateway] reconnected and re-subscribed")
				break
			}
			continue
		}

		switch frame.Type {
		case "res":
			gs.pendMu.Lock()
			ch, ok := gs.pending[frame.ID]
			if ok {
				delete(gs.pending, frame.ID)
			}
			gs.pendMu.Unlock()
			if ok {
				ch <- frame
			}

		case "event":
			if frame.Event != "agent" {
				continue
			}
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
			if agentPayload.SessionKey != gs.sessionKey {
				continue
			}

			gs.infMu.RLock()
			inf := gs.inFlight
			gs.infMu.RUnlock()
			if inf == nil {
				continue
			}

			if agentPayload.Stream == "assistant" && agentPayload.Data.Delta != "" {
				inf.fullText.WriteString(agentPayload.Data.Delta)
				if inf.onChunk != nil {
					inf.onChunk(agentPayload.Data.Delta)
				}
			}
			if agentPayload.Stream == "lifecycle" && agentPayload.Data.Phase == "end" {
				log.Printf("[gateway] agent turn complete")
				inf.done <- agentResult{text: strings.TrimSpace(inf.fullText.String())}
			}
		}
	}
}

// refreshContextUsage polls sessions.describe and updates contextUsage.
func (gs *gatewaySession) refreshContextUsage(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := gs.sendReq(pollCtx, "sessions.describe", map[string]string{"key": gs.sessionKey})
	if err != nil {
		log.Printf("[session] sessions.describe for context usage: %v", err)
		return
	}
	var payload struct {
		Session struct {
			TotalTokens   int `json:"totalTokens"`
			ContextTokens int `json:"contextTokens"`
		} `json:"session"`
	}
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		log.Printf("[session] unmarshal sessions.describe response: %v", err)
		return
	}
	if payload.Session.ContextTokens == 0 {
		return
	}
	usage := payload.Session.TotalTokens * 100 / payload.Session.ContextTokens
	if usage > 100 {
		usage = 100
	}
	gs.ctxMu.Lock()
	gs.contextUsage = usage
	gs.ctxMu.Unlock()
}

// ContextUsage returns the last-known context window usage percentage (0-100).
func (gs *gatewaySession) ContextUsage() int {
	gs.ctxMu.RLock()
	defer gs.ctxMu.RUnlock()
	return gs.contextUsage
}

// IsReady returns true once the persistent gateway session is established.
func (gs *gatewaySession) IsReady() bool {
	gs.readyMu.RLock()
	defer gs.readyMu.RUnlock()
	return gs.ready
}

func (gs *gatewaySession) setReady() {
	gs.readyMu.Lock()
	gs.ready = true
	gs.readyMu.Unlock()
	// Initial context usage refresh now that session is ready
	go gs.refreshContextUsage(context.Background())
}

// SendMessage sends a user message to the persistent session, streams chunks
// via onChunk, and returns the full response text.
func (gs *gatewaySession) SendMessage(ctx context.Context, message string, onChunk func(string)) (string, error) {
	// Serialise: only one message in flight at a time
	gs.sendMu.Lock()
	defer gs.sendMu.Unlock()

	inf := &inFlightState{
		onChunk: onChunk,
		done:    make(chan agentResult, 1),
	}
	gs.infMu.Lock()
	gs.inFlight = inf
	gs.infMu.Unlock()
	defer func() {
		gs.infMu.Lock()
		gs.inFlight = nil
		gs.infMu.Unlock()
	}()

	// Send the message
	_, err := gs.sendReq(ctx, "sessions.send", map[string]string{
		"key":     gs.sessionKey,
		"message": message,
	})
	if err != nil {
		return "", fmt.Errorf("sessions.send: %w", err)
	}
	log.Printf("[gateway] message sent, streaming response...")

	// Wait for lifecycle/end from the read loop
	select {
	case result := <-inf.done:
		if result.err != nil {
			return "", result.err
		}
		// Refresh context usage after each turn (best-effort)
		go gs.refreshContextUsage(context.Background())
		return result.text, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ─── bootstrap mode ─────────────────────────────────────────────────────────
//
// runBootstrap performs all VM setup steps that previously lived in the bash
// heredoc. After runBootstrap returns, main() continues into the normal bridge
// connect loop — no restart required.

// runShell runs a bash -c script, streaming output to log.
func runShell(script string) error {
	cmd := exec.Command("bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// nixInstallBg starts the Nix installer in background and returns a done channel.
// The channel receives an error (or nil) when the install finishes.
// Capacity 2 so both setupFlakeEnvironmentSync and finishNix can receive.
func nixInstallBg() <-chan error {
	ch := make(chan error, 2)
	go func() {
		log.Printf("[bootstrap] starting Nix (Determinate Systems) in background...")
		script := `
NIX_INIT_MODE="none"
if command -v systemctl &>/dev/null && systemctl is-system-running --quiet 2>/dev/null; then
  NIX_INIT_MODE="systemd"
fi
curl --proto '=https' --tlsv1.2 -sSf -L https://install.determinate.systems/nix | \
  sh -s -- install linux --no-confirm --init "$NIX_INIT_MODE" >> /tmp/nix-install.log 2>&1
`
		err := runShell(script)
		if err != nil {
			log.Printf("[bootstrap] Nix install failed: %v", err)
		} else {
			log.Printf("[bootstrap] Nix install complete")
		}
		ch <- err
		ch <- err // Send twice so both receivers can read
	}()
	return ch
}

// dockerInstallBg starts the Docker Engine installer in background and returns
// a done channel. The channel receives an error (or nil) when the install finishes.
func dockerInstallBg() <-chan error {
	ch := make(chan error, 1)
	go func() {
		log.Printf("[bootstrap] starting Docker install in background...")
		script := `
set -euo pipefail
# Detect OS family (debian vs ubuntu) for the correct Docker repo
. /etc/os-release
if [ "$ID" = "debian" ] && [ -n "$VERSION_CODENAME" ]; then
  DOCKER_REPO="deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian $VERSION_CODENAME stable"
  DOCKER_GPG="https://download.docker.com/linux/debian/gpg"
else
  DOCKER_REPO="deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $VERSION_CODENAME stable"
  DOCKER_GPG="https://download.docker.com/linux/ubuntu/gpg"
fi
# Add Docker's official GPG key
sudo apt-get update -qq
sudo apt-get install -y --fix-broken ca-certificates curl gnupg
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL "$DOCKER_GPG" | sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/docker.gpg
sudo chmod a+r /etc/apt/keyrings/docker.gpg
# Add the repository to Apt sources
echo "$DOCKER_REPO" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update -qq
sudo apt-get install -y --fix-broken docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
echo "Docker: $(docker --version)"
`
		err := runShell(script)
		if err != nil {
			log.Printf("[bootstrap] Docker install failed: %v", err)
		} else {
			log.Printf("[bootstrap] Docker install complete")
		}
		ch <- err
	}()
	return ch
}

// finishDocker waits for the background Docker install to complete and adds
// the current user to the docker group so docker commands work without sudo.
func finishDocker(dockerDone <-chan error) {
	if dockerDone == nil {
		return
	}
	log.Printf("[bootstrap] waiting for Docker install...")
	if err := <-dockerDone; err != nil {
		log.Printf("[bootstrap] Docker install error: %v", err)
		return
	}
	// Add daytona user to docker group (common username on Replicated VMs)
	_ = runShell("sudo usermod -aG docker daytona 2>/dev/null || true")
	_ = runShell("sudo usermod -aG docker root 2>/dev/null || true")
	log.Printf("[bootstrap] Docker ready")
}

// setupFlakeEnvironment sets up a Nix flake environment if flake.nix exists in the template files.
// It writes the flake files to ~/.elasticclaw/flake/ and installs packages from it.
func setupFlakeEnvironment(nixDone <-chan error) error {
	// Check if flake.nix exists in the template files that were written by the hub
	// Template files are written to the workspace directory after bootstrap
	home, _ := os.UserHomeDir()
	workspaceDir := filepath.Join(home, "workspace")
	flakeNixPath := filepath.Join(workspaceDir, "flake.nix")

	// Check if flake.nix was written by the hub (it will exist if template had it)
	if _, err := os.Stat(flakeNixPath); os.IsNotExist(err) {
		// No flake.nix in template, nothing to do
		return nil
	}

	log.Printf("[bootstrap] flake.nix found in workspace, setting up flake environment...")

	// Wait for Nix to be ready if it's being installed
	if nixDone != nil {
		log.Printf("[bootstrap] waiting for Nix install before setting up flake...")
		select {
		case err := <-nixDone:
			if err != nil {
				log.Printf("[bootstrap] Nix install failed, skipping flake setup: %v", err)
				return nil
			}
		case <-time.After(30 * time.Second):
			log.Printf("[bootstrap] timeout waiting for Nix, trying flake setup anyway...")
		}
	}

	// Ensure nix command is available
	if err := runShell("command -v nix"); err != nil {
		log.Printf("[bootstrap] nix command not available, skipping flake setup")
		return nil
	}

	// Also check for flake.lock
	flakeLockPath := filepath.Join(workspaceDir, "flake.lock")
	hasFlakeLock := false
	if _, err := os.Stat(flakeLockPath); err == nil {
		hasFlakeLock = true
	}

	// Create a dedicated flake directory
	flakeDir := filepath.Join(home, ".elasticclaw", "flake")
	if err := os.MkdirAll(flakeDir, 0755); err != nil {
		return fmt.Errorf("failed to create flake directory: %w", err)
	}

	// Copy flake.nix and flake.lock to the flake directory
	flakeNixData, err := os.ReadFile(flakeNixPath)
	if err != nil {
		return fmt.Errorf("failed to read flake.nix: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.nix"), flakeNixData, 0644); err != nil {
		return fmt.Errorf("failed to write flake.nix: %w", err)
	}

	if hasFlakeLock {
		flakeLockData, err := os.ReadFile(flakeLockPath)
		if err == nil {
			os.WriteFile(filepath.Join(flakeDir, "flake.lock"), flakeLockData, 0644)
		}
	}

	log.Printf("[bootstrap] installing packages from flake in %s...", flakeDir)

	// Install packages from the flake using nix profile install
	// This makes the flake's packages available in the user's profile
	script := fmt.Sprintf(`
set -euo pipefail
export PATH="/nix/var/nix/profiles/default/bin:$PATH"
. /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true
cd %q
# Install the default package from the flake
nix profile install . --accept-flake-config 2>&1 || echo "Flake profile install may have partial failures, continuing..."
# Also try to install common development tools if they exist in the flake
nix profile install .#devShells.default --accept-flake-config 2>&1 || true
`, flakeDir)

	if err := runShell(script); err != nil {
		log.Printf("[bootstrap] flake profile install warning: %v", err)
		// Don't fail - the flake might be optional or partially broken
	}

	// Create a wrapper script that runs commands in the flake environment
	wrapperScript := fmt.Sprintf(`#!/bin/bash
# Flake environment wrapper - runs commands in the flake's dev shell
export PATH="/nix/var/nix/profiles/default/bin:$PATH"
. /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true
cd %q
exec nix develop --accept-flake-config -c "$@"
`, flakeDir)
	wrapperPath := filepath.Join(home, ".elasticclaw", "flake-run")
	os.WriteFile(wrapperPath, []byte(wrapperScript), 0755)

	log.Printf("[bootstrap] flake environment setup complete. Use %s to run commands in the flake.", wrapperPath)
	return nil
}

// setupFlakeEnvironmentSync is the synchronous version that waits for Nix and sets up the flake.
// Used when we need the flake ready before starting the gateway.
func setupFlakeEnvironmentSync(nixDone <-chan error) error {
	// This is the same as setupFlakeEnvironment but we always wait for nixDone
	home, _ := os.UserHomeDir()
	workspaceDir := filepath.Join(home, "workspace")
	flakeNixPath := filepath.Join(workspaceDir, "flake.nix")

	if _, err := os.Stat(flakeNixPath); os.IsNotExist(err) {
		return nil
	}

	log.Printf("[bootstrap] flake.nix found in workspace, setting up flake environment (sync)...")

	// Wait for Nix to complete - we need it for the gateway
	if nixDone != nil {
		log.Printf("[bootstrap] waiting for Nix install to complete...")
		select {
		case err := <-nixDone:
			if err != nil {
				return fmt.Errorf("Nix install failed: %w", err)
			}
			log.Printf("[bootstrap] Nix install complete")
		case <-time.After(120 * time.Second):
			return fmt.Errorf("timeout waiting for Nix install")
		}
	}

	// Ensure nix command is available
	if err := runShell("command -v nix"); err != nil {
		return fmt.Errorf("nix command not available after install: %w", err)
	}

	// Also check for flake.lock
	flakeLockPath := filepath.Join(workspaceDir, "flake.lock")
	hasFlakeLock := false
	if _, err := os.Stat(flakeLockPath); err == nil {
		hasFlakeLock = true
	}

	// Create a dedicated flake directory
	flakeDir := filepath.Join(home, ".elasticclaw", "flake")
	if err := os.MkdirAll(flakeDir, 0755); err != nil {
		return fmt.Errorf("failed to create flake directory: %w", err)
	}

	// Copy flake.nix and flake.lock to the flake directory
	flakeNixData, err := os.ReadFile(flakeNixPath)
	if err != nil {
		return fmt.Errorf("failed to read flake.nix: %w", err)
	}
	if err := os.WriteFile(filepath.Join(flakeDir, "flake.nix"), flakeNixData, 0644); err != nil {
		return fmt.Errorf("failed to write flake.nix: %w", err)
	}

	if hasFlakeLock {
		flakeLockData, err := os.ReadFile(flakeLockPath)
		if err == nil {
			os.WriteFile(filepath.Join(flakeDir, "flake.lock"), flakeLockData, 0644)
		}
	}

	log.Printf("[bootstrap] flake environment ready at %s", flakeDir)
	return nil
}

// startGatewayWithFlake starts the gateway, optionally inside the flake environment.
func startGatewayWithFlake(useFlake bool) (*exec.Cmd, error) {
	if !useFlake {
		// No flake, use normal gateway start
		return startGateway()
	}

	log.Printf("[bootstrap] starting OpenClaw gateway inside nix develop...")

	// Set env vars that suppress respawn / bonjour.
	os.Setenv("OPENCLAW_NO_RESPAWN", "1")
	os.Setenv("OPENCLAW_DISABLE_BONJOUR", "1")

	home, _ := os.UserHomeDir()
	flakeDir := filepath.Join(home, ".elasticclaw", "flake")
	logFile := filepath.Join(home, "openclaw-gateway.log")

	// Build the nix develop command that runs openclaw gateway
	// Properly escape the flakeDir to prevent shell injection
	script := fmt.Sprintf(`
set -euo pipefail
export PATH="/nix/var/nix/profiles/default/bin:$PATH"
. /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true
cd %q
exec nix develop --accept-flake-config -c openclaw gateway run
`, flakeDir)

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = os.Environ()
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open gateway log: %w", err)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("start gateway in nix develop: %w", err)
	}
	f.Close()
	log.Printf("[bootstrap] gateway PID %d (in nix develop), waiting for health...", cmd.Process.Pid)

	// Poll /healthz up to 60s.
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		if checkGateway("localhost:18789") {
			log.Printf("[bootstrap] gateway ready after %ds", i+1)
			return cmd, nil
		}
	}
	// Not fatal — bridge will retry.
	log.Printf("[bootstrap] WARNING: gateway did not respond in 60s — continuing anyway")
	return cmd, nil
}

// installNodeGit installs Node.js 24 and git via apt.
func installNodeGit() error {
	log.Printf("[bootstrap] installing Node.js 24 + git...")
	script := `
set -euo pipefail
# Single apt-get update then install everything in one pass to avoid
# dpkg state corruption from multiple overlapping install calls.
sudo apt-get update -qq
sudo apt-get install -y --fix-broken curl ca-certificates git
sudo mkdir -p /etc/apt/keyrings
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | \
  sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/nodesource.gpg
echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main" | \
  sudo tee /etc/apt/sources.list.d/nodesource.list > /dev/null
sudo apt-get update -qq
sudo apt-get install -y --fix-broken nodejs
echo "Node: $(node --version)"
`
	return runShell(script)
}

// installOpenClaw installs openclaw@2026.5.12 via npm.
func installOpenClaw() error {
	log.Printf("[bootstrap] installing openclaw@2026.5.12...")
	if err := runShell("sudo npm install -g openclaw@2026.5.12 --ignore-scripts"); err != nil {
		return fmt.Errorf("npm install openclaw: %w", err)
	}
	out, _ := exec.Command("openclaw", "--version").Output()
	log.Printf("[bootstrap] OpenClaw: %s", strings.TrimSpace(string(out)))
	return nil
}

// configureOpenClaw patches ~/.openclaw/openclaw.json with model, LLM keys,
// gateway auth, and disables the bonjour plugin.
func configureOpenClaw() error {
	log.Printf("[bootstrap] configuring OpenClaw...")

	// Collect all ELASTICCLAW_PROVIDER_CONFIG env var lines into the python script.
	// ELASTICCLAW_PROVIDER_CONFIG_B64 holds a base64-encoded python snippet that
	// was generated by the hub (the same snippet buildOpenClawProviderConfig returned).
	// If it's not set, we fall back to a minimal config patch.
	providerSnippet := os.Getenv("ELASTICCLAW_PROVIDER_CONFIG")

	gatewayPassword := os.Getenv("ELASTICCLAW_GATEWAY_PASSWORD")
	if gatewayPassword == "" {
		gatewayPassword = os.Getenv("OPENCLAW_GATEWAY_PASSWORD")
	}
	if gatewayPassword != "" {
		os.Setenv("OPENCLAW_GATEWAY_PASSWORD", gatewayPassword)
	}
	defaultModel := envOr("OPENCLAW_DEFAULT_MODEL", "anthropic/claude-sonnet-4-6")

	// Build the python config patch.
	// The provider snippet (if any) is inserted as a block that sets config['models'].
	var configPy string
	if providerSnippet != "" {
		// providerSnippet is the full python << 'PYEOF' ... PYEOF block from the hub.
		// We run it as a subprocess.
		configPy = providerSnippet
	} else {
		defaultModelJSON, _ := json.Marshal(defaultModel)
		gatewayPasswordJSON, _ := json.Marshal(gatewayPassword)

		// Minimal fallback: just set model + gateway auth.
		configPy = fmt.Sprintf(`python3 <<'PYEOF'
import json, os, sys
path = os.path.expanduser('~/.openclaw/openclaw.json')
try:
    with open(path) as f:
        config = json.load(f)
except:
    config = {}
config.setdefault('agents', {}).setdefault('defaults', {})['model'] = %s
config.setdefault('gateway', {})['bind'] = 'loopback'
config['gateway']['port'] = 18789
gw_password = %s
if gw_password:
    config['gateway']['auth'] = {'mode': 'password', 'password': gw_password}
    config['gateway']['remote'] = {'password': gw_password}
with open(path, 'w') as f:
    json.dump(config, f, indent=2)
print('OpenClaw config patched (minimal)')
PYEOF`, defaultModelJSON, gatewayPasswordJSON)
	}

	if err := runShell(configPy); err != nil {
		return fmt.Errorf("configure openclaw.json: %w", err)
	}

	// Disable bonjour plugin — not supported on Replicated VMs.
	_ = runShell("openclaw plugins disable bonjour 2>/dev/null || true")

	return nil
}

// onboardOpenClaw runs openclaw onboard to initialize the identity/config.
func onboardOpenClaw() error {
	log.Printf("[bootstrap] running openclaw onboard...")

	onboardFlags := os.Getenv("ELASTICCLAW_ONBOARD_FLAGS")
	script := fmt.Sprintf(
		`openclaw onboard --non-interactive --accept-risk --gateway-bind loopback --gateway-port 18789 --skip-daemon --skip-health %s 2>/dev/null || true`,
		onboardFlags,
	)
	return runShell(script)
}

// startGateway starts the openclaw gateway as a background process and waits
// up to 60s for it to become healthy.
func startGateway() (*exec.Cmd, error) {
	log.Printf("[bootstrap] starting OpenClaw gateway...")

	// Set env vars that suppress respawn / bonjour.
	os.Setenv("OPENCLAW_NO_RESPAWN", "1")
	os.Setenv("OPENCLAW_DISABLE_BONJOUR", "1")
	if gatewayPassword := os.Getenv("ELASTICCLAW_GATEWAY_PASSWORD"); gatewayPassword != "" {
		os.Setenv("OPENCLAW_GATEWAY_PASSWORD", gatewayPassword)
	}

	home, _ := os.UserHomeDir()
	logFile := filepath.Join(home, "openclaw-gateway.log")

	cmd := exec.Command("openclaw", "gateway", "run")
	cmd.Env = os.Environ()
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open gateway log: %w", err)
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("start gateway: %w", err)
	}
	f.Close()
	log.Printf("[bootstrap] gateway PID %d, waiting for health...", cmd.Process.Pid)

	// Poll /healthz up to 60s.
	for i := 0; i < 60; i++ {
		time.Sleep(time.Second)
		if checkGateway("localhost:18789") {
			log.Printf("[bootstrap] gateway ready after %ds", i+1)
			return cmd, nil
		}
	}
	// Not fatal — bridge will retry.
	log.Printf("[bootstrap] WARNING: gateway did not respond in 60s — continuing anyway")
	return cmd, nil
}

func stopGateway(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	log.Printf("[bootstrap] stopping initial gateway PID %d...", cmd.Process.Pid)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		log.Printf("[bootstrap] gateway stop warning: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		if err != nil {
			log.Printf("[bootstrap] gateway stopped: %v", err)
		}
	case <-time.After(10 * time.Second):
		log.Printf("[bootstrap] gateway did not stop after SIGTERM; killing")
		_ = cmd.Process.Kill()
		<-done
	}
}

// waitForDeviceJSON waits up to 120s for ~/.openclaw/identity/device.json.
func waitForDeviceJSON() error {
	home, _ := os.UserHomeDir()
	devPath := filepath.Join(home, ".openclaw", "identity", "device.json")
	log.Printf("[bootstrap] waiting for device.json...")
	for i := 0; i < 120; i++ {
		if _, err := os.Stat(devPath); err == nil {
			log.Printf("[bootstrap] device.json ready after %ds", i)
			return nil
		}
		time.Sleep(time.Second)
	}
	log.Printf("[bootstrap] WARNING: device.json not found after 120s — bridge will poll internally")
	return nil
}

// finishNix waits for the background Nix install to complete, updates the
// current PATH, and starts the nix-daemon if needed.
func finishNix(nixDone <-chan error) {
	if nixDone == nil {
		return
	}
	log.Printf("[bootstrap] waiting for Nix install...")
	if err := <-nixDone; err != nil {
		log.Printf("[bootstrap] Nix install error: %v", err)
	}
	profile := "/nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh"
	if _, err := os.Stat(profile); err == nil {
		nixBin := "/nix/var/nix/profiles/default/bin"
		os.Setenv("PATH", nixBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	if err := runShell("pgrep -x nix-daemon"); err != nil {
		// Not running — start it
		_ = runShell("sudo /nix/var/nix/profiles/default/bin/nix-daemon &")
		time.Sleep(2 * time.Second)
		os.Setenv("NIX_REMOTE", "daemon")
	}
	// Persist Nix profile for future shells.
	_ = runShell(
		`echo '. /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true' |` +
			` sudo tee /etc/profile.d/nix.sh > /dev/null`,
	)
	out, _ := exec.Command("nix", "--version").Output()
	log.Printf("[bootstrap] Nix: %s", strings.TrimSpace(string(out)))
}

// hasWorkspaceFlake checks if flake.nix exists in the workspace directory
func hasWorkspaceFlake() bool {
	home, _ := os.UserHomeDir()
	workspaceDir := filepath.Join(home, "workspace")
	flakeNixPath := filepath.Join(workspaceDir, "flake.nix")
	_, err := os.Stat(flakeNixPath)
	return err == nil
}

// runBootstrap executes all VM bootstrap steps, then returns so the caller
// can continue into the normal bridge connect loop.
func runBootstrap() error {
	log.Printf("[bootstrap] starting claw-bridge bootstrap mode")

	// Check early if we have a workspace flake - this affects how we start the gateway
	hasFlake := hasWorkspaceFlake()
	if hasFlake {
		log.Printf("[bootstrap] flake.nix detected in workspace, will set up flake environment")
	}

	// Step 1: Install Node.js 24 + git (serial)
	if err := installNodeGit(); err != nil {
		return fmt.Errorf("installNodeGit: %w", err)
	}

	// Step 2: Start Nix install in background (if requested or if we have a flake)
	var nixDone <-chan error
	if os.Getenv("ELASTICCLAW_NIX") == "true" || hasFlake {
		// Auto-enable Nix if a flake is present
		nixDone = nixInstallBg()
	}

	// Step 2b: Start Docker install in background (if requested)
	var dockerDone <-chan error
	if os.Getenv("ELASTICCLAW_DOCKER") == "true" {
		dockerDone = dockerInstallBg()
	}

	// Step 3: Install OpenClaw (needs Node)
	if err := installOpenClaw(); err != nil {
		return fmt.Errorf("installOpenClaw: %w", err)
	}

	// Step 4: Run openclaw onboard (initializes ~/.openclaw directory)
	home, _ := os.UserHomeDir()
	ocCfgPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if _, err := os.Stat(ocCfgPath); os.IsNotExist(err) {
		if err := onboardOpenClaw(); err != nil {
			log.Printf("[bootstrap] onboard warning: %v (continuing)", err)
		}
	} else {
		log.Printf("[bootstrap] openclaw.json already exists, skipping onboard")
	}

	// Step 5: Set up flake environment BEFORE starting gateway if flake.nix exists
	// This ensures the gateway runs inside the flake environment
	if hasFlake {
		if err := setupFlakeEnvironmentSync(nixDone); err != nil {
			log.Printf("[bootstrap] flake setup warning: %v (continuing without flake)", err)
			hasFlake = false // Fall back to non-flake mode
		}
	}

	// Step 6: Start the gateway once so OpenClaw writes its final first-run config.
	// If we have a flake, this will run inside nix develop
	initialGateway, err := startGatewayWithFlake(hasFlake)
	if err != nil {
		return fmt.Errorf("startGateway initial: %w", err)
	}
	stopGateway(initialGateway)

	// Step 7: Configure openclaw.json (model, LLM keys, gateway auth, disable bonjour)
	if err := configureOpenClaw(); err != nil {
		return fmt.Errorf("configureOpenClaw: %w", err)
	}

	// Step 8: Restart gateway and wait for health (inside flake if present)
	gateway, err := startGatewayWithFlake(hasFlake)
	if err != nil {
		return fmt.Errorf("startGateway: %w", err)
	}
	go func() {
		if err := gateway.Wait(); err != nil {
			log.Printf("[bootstrap] gateway exited: %v", err)
		} else {
			log.Printf("[bootstrap] gateway exited")
		}
	}()

	// Step 9: Wait for device.json
	if err := waitForDeviceJSON(); err != nil {
		return fmt.Errorf("waitForDeviceJSON: %w", err)
	}

	// Step 10: After bridge connects, finish Nix and Docker in background
	go finishNix(nixDone)
	go finishDocker(dockerDone)

	if notifyFile := os.Getenv("ELASTICCLAW_BOOTSTRAP_NOTIFY_FILE"); notifyFile != "" {
		if err := os.WriteFile(notifyFile, []byte("ok\n"), 0600); err != nil {
			return fmt.Errorf("write bootstrap notify file: %w", err)
		}
	}

	log.Printf("[bootstrap] bootstrap complete — continuing to bridge connect loop")
	return nil
}

// ─── main ────────────────────────────────────────────────────────────────────

// ─── Local HTTP proxy ───────────────────────────────────────────────────────
// The bridge listens on 127.0.0.1:18790 and forwards requests to the hub
// via the existing WS connection. This allows tools inside the sandbox
// (e.g. git credential helper) to reach hub APIs without a public hub URL.

type httpProxyReq struct {
	ReqID  string            `json:"req_id"`
	Method string            `json:"method"`
	Path   string            `json:"path"`
	Query  string            `json:"query,omitempty"`
	Body   string            `json:"body,omitempty"`
	Header map[string]string `json:"header,omitempty"`
}

type httpProxyRes struct {
	ReqID  string `json:"req_id"`
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// httpProxy holds pending proxy requests waiting for hub responses.
type httpProxy struct {
	mu      sync.Mutex
	pending map[string]chan httpProxyRes
	send    func(hubMsg) error
}

func newHTTPProxy(send func(hubMsg) error) *httpProxy {
	return &httpProxy{
		pending: make(map[string]chan httpProxyRes),
		send:    send,
	}
}

func (p *httpProxy) deliver(res httpProxyRes) {
	log.Printf("[bridge-proxy] deliver req_id=%s status=%d body_len=%d", res.ReqID, res.Status, len(res.Body))
	p.mu.Lock()
	ch, ok := p.pending[res.ReqID]
	if ok {
		delete(p.pending, res.ReqID)
	}
	p.mu.Unlock()
	if ok {
		ch <- res
	} else {
		log.Printf("[bridge-proxy] deliver dropped req_id=%s (no pending waiter)", res.ReqID)
	}
}

func (p *httpProxy) ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.send != nil
}

func (p *httpProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("[bridge-proxy] incoming %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	// Wait up to 30s for the bridge to connect to the hub
	for i := 0; i < 30; i++ {
		if p.ready() {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !p.ready() {
		log.Printf("[bridge-proxy] not ready for %s %s", r.Method, r.URL.Path)
		http.Error(w, "bridge not connected to hub", http.StatusServiceUnavailable)
		return
	}
	reqID := fmt.Sprintf("%d", time.Now().UnixNano())
	var body string
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	req := httpProxyReq{
		ReqID:  reqID,
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Body:   body,
		Header: headers,
	}
	ch := make(chan httpProxyRes, 1)
	p.mu.Lock()
	p.pending[reqID] = ch
	p.mu.Unlock()

	p.mu.Lock()
	sendFn := p.send
	p.mu.Unlock()
	if sendFn == nil {
		log.Printf("[bridge-proxy] sendFn nil req_id=%s", reqID)
		http.Error(w, "bridge not connected", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[bridge-proxy] forwarding req_id=%s method=%s path=%s query=%s headers=%v body_len=%d", reqID, req.Method, req.Path, req.Query, req.Header, len(req.Body))
	if err := sendFn(hubMsg{Type: "http_proxy_req", Payload: mustJSON(req)}); err != nil {
		log.Printf("[bridge-proxy] send error req_id=%s err=%v", reqID, err)
		p.mu.Lock()
		delete(p.pending, reqID)
		p.mu.Unlock()
		http.Error(w, "bridge send error", http.StatusBadGateway)
		return
	}

	select {
	case res := <-ch:
		log.Printf("[bridge-proxy] response req_id=%s status=%d body_len=%d", reqID, res.Status, len(res.Body))
		w.WriteHeader(res.Status)
		_, _ = w.Write([]byte(res.Body))
	case <-time.After(10 * time.Second):
		log.Printf("[bridge-proxy] timeout req_id=%s path=%s", reqID, req.Path)
		p.mu.Lock()
		delete(p.pending, reqID)
		p.mu.Unlock()
		http.Error(w, "hub proxy timeout", http.StatusGatewayTimeout)
	}
}

func main() {
	// Subcommand dispatch: linear CLI embedded in the bridge binary.
	if len(os.Args) > 1 && os.Args[1] == "linear" {
		os.Exit(runLinearCLI(os.Args[2:]))
	}

	// Bootstrap mode: run all VM setup steps before entering the bridge loop.
	// Activated by --bootstrap flag or ELASTICCLAW_BOOTSTRAP=1 env var.
	bootstrapMode := os.Getenv("ELASTICCLAW_BOOTSTRAP") == "1"
	for _, arg := range os.Args[1:] {
		if arg == "--bootstrap" {
			bootstrapMode = true
		}
	}
	if bootstrapMode {
		if err := runBootstrap(); err != nil {
			log.Fatalf("[bootstrap] fatal: %v", err)
		}
	}

	hubURL := mustEnv("ELASTICCLAW_HUB_URL")
	clawID := mustEnv("ELASTICCLAW_CLAW_ID")
	token := mustEnv("ELASTICCLAW_CLAW_TOKEN")
	gatewayAddr := envOr("ELASTICCLAW_GATEWAY", "localhost:18789")
	clawName := envOr("ELASTICCLAW_CLAW_NAME", clawID)
	templateName := envOr("ELASTICCLAW_TEMPLATE", "")

	// Build WebSocket URL from hub URL directly
	wsURL := strings.TrimRight(hubURL, "/")
	if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[7:]
	} else if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[8:]
	}
	wsURL += "/claw/ws"
	log.Printf("  Mode:    direct")

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

	// Start local HTTP proxy so tools in the sandbox can reach hub APIs
	// via http://localhost:18790 without needing a public hub URL.
	proxy := newHTTPProxy(nil) // send func wired up in runHubLoop
	queue := &msgQueue{}
	go func() {
		log.Printf("[bridge] local HTTP proxy on 127.0.0.1:18790")
		if err := http.ListenAndServe("127.0.0.1:18790", proxy); err != nil {
			log.Printf("[bridge] HTTP proxy error: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Establish the persistent gateway session before entering the hub loop.
	// Retry until we succeed (gateway may not be ready yet).
	var gwSession *gatewaySession
	for {
		if ctx.Err() != nil {
			return
		}
		if !checkGateway(gatewayAddr) {
			log.Printf("[bridge] waiting for gateway at %s...", gatewayAddr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}
		gwSession, err = newGatewaySession(ctx, gwClient)
		if err != nil {
			log.Printf("[bridge] failed to init gateway session: %v — retrying in 5s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		break
	}
	log.Printf("[bridge] gateway session ready: %s", gwSession.sessionKey)

	// Start status channel goroutine (lightweight second WS to hub)
	go runStatusChannel(ctx, wsURL, clawID, clawName, templateName, token)

	for {
		if err := runHubLoop(ctx, wsURL, clawID, clawName, templateName, token, gwClient, gwSession, proxy, queue); err != nil {
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

// runStatusChannel maintains a lightweight second WebSocket to the hub for
// watchdog / health pings. It replies to status_ping with status_pong.
func runStatusChannel(ctx context.Context, wsURL, clawID, clawName, templateName, token string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{
				"User-Agent":                 {"claw-bridge/1.0"},
				"ngrok-skip-browser-warning": {"true"},
			},
		})
		if err != nil {
			log.Printf("[status] dial failed: %v — retry in %v", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
		}
		backoff = time.Second
		conn.SetReadLimit(1 << 20) // 1MB

		reg := hubMsg{
			Type: "register",
			Payload: mustJSON(map[string]interface{}{
				"claw_id":  clawID,
				"name":     clawName,
				"template": templateName,
				"token":    token,
				"channel":  "status",
			}),
		}
		if err := wsjson.Write(ctx, conn, reg); err != nil {
			conn.CloseNow()
			log.Printf("[status] register failed: %v — retry", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		var ack hubMsg
		if err := wsjson.Read(ctx, conn, &ack); err != nil || ack.Type != "registered" {
			conn.CloseNow()
			log.Printf("[status] register ack failed: %v — retry", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		log.Printf("[status] connected")

		// Start a background goroutine to send WebSocket ping frames every 30s.
		// This keeps the connection alive at the TCP/WebSocket layer, preventing
		// proxies and the nhooyr/websocket library from dropping the idle connection.
		pingCtx, pingCancel := context.WithCancel(ctx)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-pingCtx.Done():
					return
				case <-ticker.C:
					if err := conn.Ping(pingCtx); err != nil {
						log.Printf("[status] ping failed: %v", err)
						return
					}
				}
			}
		}()

		// Read loop — reply to pings
		for {
			var msg hubMsg
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				log.Printf("[status] read error: %v — reconnecting", err)
				break
			}
			if msg.Type == "status_ping" {
				_ = wsjson.Write(ctx, conn, hubMsg{Type: "status_pong"})
			}
		}
		pingCancel() // stop the ping goroutine before closing the connection
		conn.CloseNow()
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func runHubLoop(ctx context.Context, wsURL, clawID, clawName, templateName, token string, gwClient *gatewayClient, gwSession *gatewaySession, proxy *httpProxy, queue *msgQueue) error {
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			"User-Agent":                 {"claw-bridge/1.0"},
			"ngrok-skip-browser-warning": {"true"},
		},
	})
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}
	conn.SetReadLimit(32 * 1024 * 1024) // 32MB
	defer conn.CloseNow()

	// Register with the hub — gateway_ready=false until session is established
	reg := hubMsg{
		Type: "register",
		Payload: mustJSON(map[string]interface{}{
			"claw_id":       clawID,
			"name":          clawName,
			"template":      templateName,
			"token":         token,
			"gateway_ready": gwSession.IsReady(),
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

	// Replay any queued messages that arrived while we were disconnected
	if queued := queue.drain(); len(queued) > 0 {
		log.Printf("[bridge] replaying %d queued message(s)", len(queued))
		connCtx := ctx
		for _, content := range queued {
			go func(c string) {
				agentCtx, agentCancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer agentCancel()
				reply, agentErr := gwSession.SendMessage(agentCtx, c, func(chunk string) {
					_ = wsjson.Write(connCtx, conn, hubMsg{
						Type:    "chunk",
						Payload: mustJSON(map[string]interface{}{"role": "claw", "content": chunk}),
					})
				})
				if agentErr != nil {
					reply = fmt.Sprintf("⚠️ error: %v", agentErr)
				}
				if writeErr := wsjson.Write(connCtx, conn, hubMsg{
					Type:    "message",
					Payload: mustJSON(map[string]interface{}{"role": "claw", "content": reply}),
				}); writeErr != nil {
					// Hub connection dropped — queue original content for replay on reconnect
					log.Printf("[bridge] hub write failed, queuing original message for replay: %v", writeErr)
					queue.push(c)
				}
			}(content)
		}
	}

	// Wire up the HTTP proxy send function for this connection
	proxy.mu.Lock()
	proxy.send = func(msg hubMsg) error {
		return wsjson.Write(ctx, conn, msg)
	}
	proxy.mu.Unlock()

	// Heartbeat goroutine — includes context_usage from persistent session
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Refresh context usage before sending heartbeat (best-effort)
				go gwSession.refreshContextUsage(ctx)
				health := checkGateway(gwClient.addr)
				cu := gwSession.ContextUsage()
				log.Printf("[heartbeat] sending: gateway_healthy=%v gateway_ready=%v context_usage=%d%%", health, gwSession.IsReady(), cu)
				_ = wsjson.Write(ctx, conn, hubMsg{
					Type: "heartbeat",
					Payload: mustJSON(map[string]interface{}{
						"gateway_healthy": health,
						"gateway_ready":   gwSession.IsReady(),
						"context_usage":   cu,
					}),
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
				if !gwSession.IsReady() {
					log.Printf("[bridge] gateway not ready, queuing message for later")
					queue.push(content)
					return
				}

				agentCtx, agentCancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer agentCancel()

				log.Printf("[bridge] → openclaw: %q", content[:min(len(content), 80)])

				reply, agentErr := gwSession.SendMessage(agentCtx, content, func(chunk string) {
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

				if writeErr := wsjson.Write(connCtx, conn, hubMsg{
					Type:    "message",
					Payload: mustJSON(map[string]interface{}{"role": "claw", "content": reply}),
				}); writeErr != nil {
					// Hub connection dropped — queue original content for replay on reconnect
					log.Printf("[bridge] hub write failed, queuing original message for replay: %v", writeErr)
					queue.push(content)
				}
			}(ctx, msg.Payload)

		case "http_proxy_res":
			var res httpProxyRes
			if err := json.Unmarshal(msg.Payload, &res); err == nil {
				proxy.deliver(res)
			}

		case "file":
			go handleFileMessage(ctx, conn, msg.Payload)

		case "file_read":
			go handleFileReadMessage(ctx, conn, msg.Payload)

		default:
			// ignore unknown message types
		}
	}
}

func checkGateway(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
