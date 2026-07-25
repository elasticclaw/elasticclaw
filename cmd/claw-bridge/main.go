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
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	_ "modernc.org/sqlite"
)

var (
	Version             = "dev"
	Commit              = "none"
	BuildDate           = "unknown"
	gatewayProcessState struct {
		sync.RWMutex
		exited       bool
		restartCount int
		session      *gatewaySession
	}
	gatewayRestartBase int
)

// ─── hub wire types ─────────────────────────────────────────────────────────

type hubMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func deliverInFlight(inf *inFlightState, result agentResult) {
	select {
	case inf.done <- result:
	default:
	}
}

func writeHubMessage(ctx context.Context, conn *websocket.Conn, msg hubMsg) error {
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(writeCtx, conn, msg)
}

func startHubKeepalives(ctx context.Context, ping func(context.Context) error, onPingFailure func(), heartbeat func()) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := ping(ctx); err != nil {
					log.Printf("[hub] ping failed: %v", err)
					onPingFailure()
					return
				}
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				heartbeat()
			}
		}
	}()
}

// ─── Inbound message queue ───────────────────────────────────────────────────
// Queues entries when the hub connection is temporarily unavailable. Unprocessed
// user inputs older than msgQueueTTL are dropped to avoid starting a stale turn
// much later, but completed replies and error notices NEVER expire — losing a
// finished reply would silently discard work (commits, PRs) the agent already did.

const (
	msgQueueTTL = 10 * time.Minute
	msgQueueMax = 256 // total entries; oldest inputs are evicted on overflow
)

type queuedKind int

const (
	queuedInput  queuedKind = iota // user message not yet processed
	queuedReply                    // completed agent reply awaiting delivery
	queuedNotice                   // error notice to surface to the hub
)

type queuedMsg struct {
	kind     queuedKind
	content  string
	queuedAt time.Time
}

type msgQueue struct {
	mu   sync.Mutex
	msgs []queuedMsg

	// dropped tracks inputs discarded (TTL or overflow) while the hub was
	// unreachable, so drain() can tell the hub instead of losing them silently.
	dropped         int
	droppedPreviews []string
}

type messageDeduper struct {
	mu    sync.Mutex
	ids   map[string]struct{}
	order []string
}

func newMessageDeduper() *messageDeduper { return &messageDeduper{ids: make(map[string]struct{})} }

// seen records id and reports whether it has already been processed.
func (d *messageDeduper) seen(id string) bool {
	if id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.ids[id]; ok {
		return true
	}
	d.ids[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > 128 {
		delete(d.ids, d.order[0])
		d.order = d.order[1:]
	}
	return false
}

// recordDropLocked notes a dropped input for the reconnect notice. Caller holds mu.
func (q *msgQueue) recordDropLocked(content string) {
	q.dropped++
	if len(q.droppedPreviews) < 10 {
		q.droppedPreviews = append(q.droppedPreviews, content[:min(len(content), 60)])
	}
}

// enqueueLocked appends an entry, enforcing msgQueueMax by evicting the oldest
// input. Replies/notices are never evicted; if no input can be evicted the queue
// is allowed to grow so completed replies are never lost. Caller holds mu.
func (q *msgQueue) enqueueLocked(m queuedMsg) {
	if len(q.msgs) >= msgQueueMax {
		for i, existing := range q.msgs {
			if existing.kind == queuedInput {
				q.recordDropLocked(existing.content)
				q.msgs = append(q.msgs[:i], q.msgs[i+1:]...)
				break
			}
		}
	}
	q.msgs = append(q.msgs, m)
}

// pushInput queues an unprocessed user message (subject to TTL).
func (q *msgQueue) pushInput(content string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueueLocked(queuedMsg{kind: queuedInput, content: content, queuedAt: time.Now()})
}

// pushReply queues a completed agent reply awaiting delivery (never expires).
func (q *msgQueue) pushReply(content string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueueLocked(queuedMsg{kind: queuedReply, content: content, queuedAt: time.Now()})
}

// requeue re-inserts an entry preserving its original queuedAt (used when a
// queued reply/notice fails to deliver again).
func (q *msgQueue) requeue(m queuedMsg) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueueLocked(m)
}

// drain returns all deliverable entries and clears the queue. Expired inputs are
// discarded but counted; if any were dropped a synthesized notice is prepended so
// the hub learns about the loss on reconnect instead of it vanishing silently.
func (q *msgQueue) drain() []queuedMsg {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []queuedMsg
	for _, m := range q.msgs {
		if m.kind == queuedInput && time.Since(m.queuedAt) >= msgQueueTTL {
			log.Printf("[bridge] dropped queued input (TTL exceeded): %q", m.content[:min(len(m.content), 60)])
			q.recordDropLocked(m.content)
			continue
		}
		out = append(out, m)
	}
	q.msgs = nil

	if q.dropped > 0 {
		notice := queuedMsg{
			kind:     queuedNotice,
			content:  fmt.Sprintf("⚠️ claw-bridge: %d queued message(s) were dropped while the hub was unreachable: %s", q.dropped, strings.Join(q.droppedPreviews, " | ")),
			queuedAt: time.Now(),
		}
		out = append([]queuedMsg{notice}, out...)
		q.dropped = 0
		q.droppedPreviews = nil
	}
	return out
}

// replayQueued delivers queued entries after reconnect. Completed replies and
// notices are written directly to the hub; only unprocessed inputs re-run a turn.
func replayQueued(queue *msgQueue, deliver func(role, content string) error, runTurn func(content string)) {
	for _, m := range queue.drain() {
		switch m.kind {
		case queuedReply, queuedNotice:
			if err := deliver("claw", m.content); err != nil {
				log.Printf("[bridge] replay deliver failed, re-queuing: %v", err)
				queue.requeue(m)
			}
		default: // queuedInput
			runTurn(m.content)
		}
	}
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

type cliAuthBundle struct {
	Files map[string]string `json:"files"`
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
	home     string
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
		home:     home,
	}, nil
}

func hasAllScopes(current, required []string) bool {
	have := make(map[string]bool, len(current))
	for _, scope := range current {
		have[scope] = true
	}
	for _, scope := range required {
		if !have[scope] {
			return false
		}
	}
	return true
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	cleanup = false
	return nil
}

func promoteInsufficientGatewayPairing(home, deviceID string, requiredScopes []string) (bool, error) {
	if deviceID == "" {
		return false, nil
	}
	path := filepath.Join(home, ".openclaw", "devices", "paired.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read paired devices: %w", err)
	}

	records := map[string]interface{}{}
	if err := json.Unmarshal(data, &records); err != nil {
		return false, fmt.Errorf("parse paired devices: %w", err)
	}
	rawRecord, ok := records[deviceID]
	if !ok {
		return false, nil
	}

	recordData, err := json.Marshal(rawRecord)
	if err != nil {
		return false, fmt.Errorf("encode paired device %s: %w", deviceID, err)
	}
	var record struct {
		Scopes         []string `json:"scopes"`
		ApprovedScopes []string `json:"approvedScopes"`
	}
	if err := json.Unmarshal(recordData, &record); err != nil {
		return false, fmt.Errorf("parse paired device %s: %w", deviceID, err)
	}
	scopes := record.ApprovedScopes
	if len(scopes) == 0 {
		scopes = record.Scopes
	}
	if hasAllScopes(scopes, requiredScopes) {
		return false, nil
	}

	recordMap, ok := rawRecord.(map[string]interface{})
	if !ok {
		return false, fmt.Errorf("paired device %s has unexpected shape", deviceID)
	}
	recordMap["scopes"] = requiredScopes
	recordMap["approvedScopes"] = requiredScopes
	recordMap["role"] = gwRole
	recordMap["roles"] = []string{gwRole}
	records[deviceID] = recordMap
	updated, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode paired devices: %w", err)
	}
	updated = append(updated, '\n')
	if err := writeFileAtomic(path, updated, 0600); err != nil {
		return false, fmt.Errorf("write paired devices: %w", err)
	}
	return true, nil
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
	// OpenClaw CLI commands can pair the shared sandbox device with read-only
	// scopes before claw-bridge connects. Password auth is our bootstrap-time
	// authority to use the gateway, so promote only this device's existing
	// record before requesting the bridge scopes.
	if gc.password != "" {
		if promoted, err := promoteInsufficientGatewayPairing(gc.home, gc.device.DeviceID, defaultScopes); err != nil {
			log.Printf("[gateway] warning: could not promote insufficient pairing for device %.16s: %v", gc.device.DeviceID, err)
		} else if promoted {
			log.Printf("[gateway] promoted pairing for device %.16s before requesting bridge scopes", gc.device.DeviceID)
		}
	}

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
			"version":    Version,
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

type agentActivity struct {
	Kind    string `json:"kind"`
	Stream  string `json:"stream,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Command string `json:"command,omitempty"`
	Path    string `json:"path,omitempty"`
	URL     string `json:"url,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// inFlightState tracks a single in-progress agent turn.
type inFlightState struct {
	onChunk             func(string)
	onActivity          func(agentActivity)
	fullText            strings.Builder
	done                chan agentResult
	mu                  sync.Mutex
	activeTool          *agentActivity
	activeToolStartedAt time.Time
	lastToolPulseAt     time.Time
	modelWaitStartedAt  time.Time
	lastModelPulseAt    time.Time
}

func (inf *inFlightState) emitActivity(a agentActivity) {
	if inf.onActivity != nil {
		inf.onActivity(a)
	}
}

func (inf *inFlightState) noteActivity(a agentActivity) {
	if a.Kind != "tool" {
		return
	}
	phase := strings.ToLower(strings.TrimSpace(a.Phase))
	inf.mu.Lock()
	defer inf.mu.Unlock()
	switch phase {
	case "running", "start", "started", "in_progress":
		copy := a
		if inf.activeTool == nil || inf.activeTool.Tool != copy.Tool || inf.activeTool.Command != copy.Command || inf.activeTool.Path != copy.Path || inf.activeTool.URL != copy.URL {
			inf.activeToolStartedAt = time.Now()
			inf.lastToolPulseAt = time.Time{}
		}
		inf.activeTool = &copy
		inf.modelWaitStartedAt = time.Time{}
		inf.lastModelPulseAt = time.Time{}
	case "completed", "complete", "done", "failed", "error", "cancelled", "canceled":
		inf.activeTool = nil
		inf.activeToolStartedAt = time.Time{}
		inf.lastToolPulseAt = time.Time{}
		inf.modelWaitStartedAt = time.Now()
		inf.lastModelPulseAt = time.Time{}
	}
}

func (inf *inFlightState) noteAssistantChunk() {
	inf.mu.Lock()
	defer inf.mu.Unlock()
	inf.modelWaitStartedAt = time.Time{}
	inf.lastModelPulseAt = time.Time{}
}

func (inf *inFlightState) toolProgressPulse() (agentActivity, bool) {
	inf.mu.Lock()
	defer inf.mu.Unlock()
	if inf.activeTool == nil || inf.activeToolStartedAt.IsZero() {
		return agentActivity{}, false
	}
	now := time.Now()
	elapsed := now.Sub(inf.activeToolStartedAt)
	if elapsed < 90*time.Second {
		return agentActivity{}, false
	}
	if !inf.lastToolPulseAt.IsZero() && now.Sub(inf.lastToolPulseAt) < 60*time.Second {
		return agentActivity{}, false
	}
	inf.lastToolPulseAt = now
	pulse := *inf.activeTool
	pulse.Phase = "running"
	pulse.Message = fmt.Sprintf("running for %s", formatActivityDuration(elapsed))
	return pulse, true
}

func (inf *inFlightState) modelProgressPulse() (agentActivity, bool) {
	inf.mu.Lock()
	defer inf.mu.Unlock()
	if inf.activeTool != nil || inf.modelWaitStartedAt.IsZero() {
		return agentActivity{}, false
	}
	now := time.Now()
	elapsed := now.Sub(inf.modelWaitStartedAt)
	if elapsed < 90*time.Second {
		return agentActivity{}, false
	}
	if !inf.lastModelPulseAt.IsZero() && now.Sub(inf.lastModelPulseAt) < 60*time.Second {
		return agentActivity{}, false
	}
	inf.lastModelPulseAt = now
	return agentActivity{
		Kind:    "model_started",
		Message: fmt.Sprintf("waiting for %s", formatActivityDuration(elapsed)),
	}, true
}

func cleanAgentActivity(a agentActivity) agentActivity {
	a.Tool = sanitizeActivityText(a.Tool)
	a.Detail = sanitizeActivityText(a.Detail)
	a.Command = sanitizeActivityText(a.Command)
	a.Path = sanitizeActivityText(a.Path)
	a.URL = sanitizeActivityText(a.URL)
	a.Message = sanitizeActivityText(a.Message)
	a.Error = sanitizeActivityText(a.Error)
	return a
}

func sanitizeActivityText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	replacers := []struct {
		prefix string
		value  string
	}{
		{"Bearer ", "Bearer [redacted]"},
		{"token=", "token=[redacted]"},
		{"access_token=", "access_token=[redacted]"},
		{"api_key=", "api_key=[redacted]"},
		{"OPENAI_API_KEY=", "OPENAI_API_KEY=[redacted]"},
		{"ANTHROPIC_API_KEY=", "ANTHROPIC_API_KEY=[redacted]"},
		{"FIREWORKS_API_KEY=", "FIREWORKS_API_KEY=[redacted]"},
		{"GITHUB_TOKEN=", "GITHUB_TOKEN=[redacted]"},
		{"GH_TOKEN=", "GH_TOKEN=[redacted]"},
	}
	for _, r := range replacers {
		value = redactActivityPrefix(value, r.prefix, r.value)
	}
	if len(value) > 240 {
		value = value[:237] + "..."
	}
	return value
}

func redactActivityPrefix(value, prefix, replacement string) string {
	offset := 0
	lowerPrefix := strings.ToLower(prefix)
	for offset < len(value) {
		idx := strings.Index(strings.ToLower(value[offset:]), lowerPrefix)
		if idx < 0 {
			break
		}
		start := offset + idx
		end := start + len(prefix)
		for end < len(value) && value[end] != ' ' && value[end] != '&' && value[end] != '"' && value[end] != '\'' {
			end++
		}
		value = value[:start] + replacement + value[end:]
		offset = start + len(replacement)
	}
	return value
}

// gatewaySession is a long-lived connection to the OpenClaw gateway that
// maintains a single persistent agent session across all messages.
type gatewaySession struct {
	client *gatewayClient

	sessionMu  sync.RWMutex
	sessionKey string

	connMu sync.Mutex
	conn   *websocket.Conn

	reconnectMu sync.Mutex

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
	usage        gatewayUsage

	// ready is true once the gateway session is established and ready for messages
	readyMu sync.RWMutex
	ready   bool
}

type gatewayUsage struct {
	sessionKey                             string
	inputTokens, outputTokens, totalTokens *int
	estimatedCostUSD                       *float64
	model                                  string
	modelProvider                          string
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

func (gs *gatewaySession) getSessionKey() string {
	gs.sessionMu.RLock()
	defer gs.sessionMu.RUnlock()
	return gs.sessionKey
}

func (gs *gatewaySession) setSessionKey(key string) {
	gs.sessionMu.Lock()
	oldKey := gs.sessionKey
	gs.sessionKey = key
	gs.sessionMu.Unlock()

	if oldKey == "" || oldKey == key {
		return
	}
	gs.infMu.RLock()
	inf := gs.inFlight
	gs.infMu.RUnlock()
	if inf != nil {
		deliverInFlight(inf, agentResult{err: fmt.Errorf("gateway session key rotated")})
	}
}

func (gs *gatewaySession) currentConn() *websocket.Conn {
	gs.connMu.Lock()
	defer gs.connMu.Unlock()
	return gs.conn
}

func (gs *gatewaySession) isCurrentConn(conn *websocket.Conn) bool {
	gs.connMu.Lock()
	defer gs.connMu.Unlock()
	return gs.conn == conn
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

func sendReqOnConn(ctx context.Context, conn *websocket.Conn, method string, params interface{}) (gwFrame, error) {
	paramsJSON, _ := json.Marshal(params)
	reqID := randomID()
	if err := wsjson.Write(ctx, conn, gwFrame{
		Type:   "req",
		ID:     reqID,
		Method: method,
		Params: paramsJSON,
	}); err != nil {
		return gwFrame{}, fmt.Errorf("%s write: %w", method, err)
	}
	for {
		var resp gwFrame
		if err := wsjson.Read(ctx, conn, &resp); err != nil {
			return gwFrame{}, fmt.Errorf("%s read response: %w", method, err)
		}
		if resp.Type != "res" || resp.ID != reqID {
			continue
		}
		if !resp.OK {
			msg := "unknown"
			if resp.Error != nil {
				msg = resp.Error.Message
			}
			return resp, fmt.Errorf("%s failed: %s", method, msg)
		}
		return resp, nil
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
			gs.setSessionKey(key)
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
	gs.setSessionKey(payload.Key)
	saveBridgeSession(payload.Key)
	log.Printf("[session] created new session: %s", gs.getSessionKey())

	if err := gs.subscribe(ctx); err != nil {
		return err
	}
	gs.setReady()
	return nil
}

// subscribe registers for events on the current session key.
func (gs *gatewaySession) subscribe(ctx context.Context) error {
	_, err := gs.sendReq(ctx, "sessions.subscribe", map[string]string{"sessionKey": gs.getSessionKey()})
	if err != nil {
		return fmt.Errorf("sessions.subscribe: %w", err)
	}
	log.Printf("[session] subscribed to session events")
	return nil
}

func (gs *gatewaySession) subscribeOnConn(ctx context.Context, conn *websocket.Conn) error {
	_, err := sendReqOnConn(ctx, conn, "sessions.subscribe", map[string]string{"sessionKey": gs.getSessionKey()})
	if err != nil {
		return fmt.Errorf("sessions.subscribe: %w", err)
	}
	log.Printf("[session] subscribed to session events")
	return nil
}

func (gs *gatewaySession) createFreshSessionOnConn(ctx context.Context, conn *websocket.Conn, reason string) error {
	log.Printf("[session] creating fresh session after %s", reason)
	resp, err := sendReqOnConn(ctx, conn, "sessions.create", map[string]string{"agentId": "main"})
	if err != nil {
		return fmt.Errorf("sessions.create: %w", err)
	}
	var payload struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(resp.Payload, &payload)
	if payload.Key == "" {
		return fmt.Errorf("sessions.create returned empty key")
	}
	gs.setSessionKey(payload.Key)
	saveBridgeSession(payload.Key)
	if err := gs.subscribeOnConn(ctx, conn); err != nil {
		return err
	}
	gs.ctxMu.Lock()
	gs.contextUsage = 0
	gs.ctxMu.Unlock()
	log.Printf("[session] recovered with fresh session: %s", gs.getSessionKey())
	return nil
}

func (gs *gatewaySession) reconnectGateway(ctx context.Context, expectedOld *websocket.Conn) (bool, error) {
	gs.reconnectMu.Lock()
	defer gs.reconnectMu.Unlock()

	if expectedOld != nil && !gs.isCurrentConn(expectedOld) {
		return false, nil
	}

	conn, err := gs.client.connectToGateway(ctx)
	if err != nil {
		return false, err
	}
	installed := false
	defer func() {
		if !installed {
			conn.CloseNow()
		}
	}()

	createdFresh := false
	if gs.getSessionKey() != "" {
		if err := gs.subscribeOnConn(ctx, conn); err == nil {
			createdFresh = false
		} else {
			log.Printf("[gateway] re-subscribe failed after reconnect: %v", err)
			if !isMissingGatewaySessionError(err) {
				return false, err
			}
			if err := gs.createFreshSessionOnConn(ctx, conn, "gateway reconnect"); err != nil {
				return false, err
			}
			createdFresh = true
		}
	} else {
		if err := gs.createFreshSessionOnConn(ctx, conn, "gateway reconnect"); err != nil {
			return false, err
		}
		createdFresh = true
	}

	if expectedOld != nil && !gs.isCurrentConn(expectedOld) {
		return false, nil
	}
	gs.failPendingRequests(fmt.Errorf("gateway disconnected"))
	gs.connMu.Lock()
	old := gs.conn
	gs.conn = conn
	gs.connMu.Unlock()
	installed = true
	if old != nil && old != conn {
		old.CloseNow()
	}
	if createdFresh {
		gs.setReady()
	}
	return true, nil
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
			if !gs.isCurrentConn(conn) {
				continue
			}
			log.Printf("[gateway] read error: %v — reconnecting", err)

			gs.failPendingRequests(fmt.Errorf("gateway disconnected"))

			// Fail any in-flight message
			gs.infMu.RLock()
			inf := gs.inFlight
			gs.infMu.RUnlock()
			if inf != nil {
				deliverInFlight(inf, agentResult{err: fmt.Errorf("gateway disconnected")})
			}
			if gs.client == nil {
				return
			}

			for {
				if ctx.Err() != nil {
					return
				}
				if !gs.isCurrentConn(conn) {
					break
				}
				if _, err := gs.reconnectGateway(ctx, conn); err != nil {
					if !gs.isCurrentConn(conn) {
						break
					}
					log.Printf("[gateway] reconnect failed: %v — retrying in 5s", err)
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
					Delta   string `json:"delta"`
					Phase   string `json:"phase"`
					Error   string `json:"error"`
					Message string `json:"message"`
					Tool    string `json:"tool"`
					Name    string `json:"name"`
					Command string `json:"command"`
					Path    string `json:"path"`
					URL     string `json:"url"`
					URI     string `json:"uri"`
					Meta    string `json:"meta"`
					Status  string `json:"status"`
				} `json:"data"`
			}
			if err := json.Unmarshal(frame.Payload, &agentPayload); err != nil {
				continue
			}
			var rawAgentPayload struct {
				Data map[string]interface{} `json:"data"`
			}
			_ = json.Unmarshal(frame.Payload, &rawAgentPayload)
			if agentPayload.SessionKey != gs.getSessionKey() {
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
				inf.noteAssistantChunk()
				if inf.onChunk != nil {
					inf.onChunk(agentPayload.Data.Delta)
				}
			} else if agentPayload.Stream != "assistant" && agentPayload.Stream != "lifecycle" {
				tool := agentPayload.Data.Tool
				if tool == "" {
					tool = agentPayload.Data.Name
				}
				kind := "activity"
				switch {
				case tool != "":
					kind = "tool"
				case strings.Contains(agentPayload.Stream, "tool"):
					kind = "tool"
				case strings.Contains(agentPayload.Stream, "diagnostic"):
					kind = "diagnostic"
				}
				command := firstNonEmpty(
					agentPayload.Data.Command,
					nestedString(rawAgentPayload.Data, "command", "cmd", "shell_command", "shellCommand", "script", "argv", "input"),
				)
				path := firstNonEmpty(agentPayload.Data.Path, nestedString(rawAgentPayload.Data, "path", "file", "filename"))
				url := firstNonEmpty(agentPayload.Data.URL, agentPayload.Data.URI, nestedString(rawAgentPayload.Data, "url", "uri"))
				meta := firstNonEmpty(agentPayload.Data.Meta, nestedString(rawAgentPayload.Data, "meta"))
				command, path, url, detail := resolveToolActivityDetail(tool, command, path, url, meta, rawAgentPayload.Data)
				activity := cleanAgentActivity(agentActivity{
					Kind:    kind,
					Stream:  agentPayload.Stream,
					Phase:   agentPayload.Data.Phase,
					Tool:    tool,
					Detail:  detail,
					Command: command,
					Path:    path,
					URL:     url,
					Message: firstNonEmpty(agentPayload.Data.Message, agentPayload.Data.Status),
					Error:   agentPayload.Data.Error,
				})
				if kind == "tool" && activity.Command == "" && activity.Path == "" && activity.URL == "" && activity.Detail == "" {
					logMissingToolActivityDetail(agentPayload.Stream, activity.Phase, activity.Tool, rawAgentPayload.Data)
				}
				inf.noteActivity(activity)
				inf.emitActivity(activity)
			}
			if agentPayload.Stream == "lifecycle" {
				switch agentPayload.Data.Phase {
				case "end":
					log.Printf("[gateway] agent turn complete")
					deliverInFlight(inf, agentResult{text: strings.TrimSpace(inf.fullText.String())})
				case "error":
					msg := agentPayload.Data.Error
					if msg == "" {
						msg = agentPayload.Data.Message
					}
					if msg == "" {
						msg = "agent lifecycle error"
					}
					log.Printf("[gateway] agent turn error: %s", msg)
					inf.emitActivity(cleanAgentActivity(agentActivity{Kind: "session_error", Stream: "lifecycle", Phase: "error", Error: msg}))
					deliverInFlight(inf, agentResult{err: fmt.Errorf("%s", msg)})
				}
			}
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func resolveToolActivityDetail(tool, command, path, url, meta string, data map[string]interface{}) (string, string, string, string) {
	detail := nestedString(data, "title", "summary", "description", "label", "display", "display_name", "displayName", "text")
	cleanTool := strings.ToLower(strings.TrimSpace(tool))
	cleanMeta := strings.TrimSpace(meta)
	if cleanMeta == "" {
		return command, path, url, detail
	}
	switch cleanTool {
	case "exec", "bash":
		if command == "" {
			command = cleanMeta
		}
	case "read", "write", "edit", "attach", "memory_get":
		if path == "" {
			path = cleanMeta
		}
	case "web_fetch", "browser":
		if url == "" && looksLikeURL(cleanMeta) {
			url = cleanMeta
		} else if detail == "" {
			detail = cleanMeta
		}
	default:
		if detail == "" {
			detail = cleanMeta
		}
	}
	return command, path, url, detail
}

func looksLikeURL(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://")
}

func logMissingToolActivityDetail(stream, phase, tool string, data map[string]interface{}) {
	log.Printf(
		"[agent-activity] missing tool detail stream=%s phase=%s tool=%s keys=%s payload=%s",
		sanitizeActivityText(stream),
		sanitizeActivityText(phase),
		sanitizeActivityText(tool),
		strings.Join(sortedMapKeys(data), ","),
		summarizeActivityPayload(data),
	)
}

func sortedMapKeys(data map[string]interface{}) []string {
	if len(data) == 0 {
		return nil
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, sanitizeActivityText(key))
	}
	sort.Strings(keys)
	return keys
}

func summarizeActivityPayload(data map[string]interface{}) string {
	if len(data) == 0 {
		return "{}"
	}
	cleaned := sanitizeActivityPayloadValue(data, 0)
	bytes, err := json.Marshal(cleaned)
	if err != nil {
		return "{}"
	}
	return sanitizeActivityText(string(bytes))
}

func sanitizeActivityPayloadValue(value interface{}, depth int) interface{} {
	if depth > 2 {
		return "[nested]"
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			out[key] = sanitizeActivityPayloadValue(child, depth+1)
		}
		return out
	case []interface{}:
		limit := len(typed)
		if limit > 4 {
			limit = 4
		}
		out := make([]interface{}, 0, limit)
		for _, child := range typed[:limit] {
			out = append(out, sanitizeActivityPayloadValue(child, depth+1))
		}
		if len(typed) > limit {
			out = append(out, fmt.Sprintf("... %d more", len(typed)-limit))
		}
		return out
	case string:
		return sanitizeActivityText(typed)
	default:
		return typed
	}
}

func nestedString(data map[string]interface{}, keys ...string) string {
	if len(data) == 0 {
		return ""
	}
	for _, key := range keys {
		if value := nestedValueString(data[key], keys...); value != "" {
			return value
		}
	}
	for _, containerKey := range []string{"raw_params", "params", "input", "arguments", "args", "request", "item", "tool_call", "toolCall", "call", "details", "metadata", "payload"} {
		if value := nestedContainerString(data[containerKey], keys...); value != "" {
			return value
		}
	}
	return ""
}

func nestedContainerString(value interface{}, keys ...string) string {
	switch typed := value.(type) {
	case map[string]interface{}:
		return nestedString(typed, keys...)
	case []interface{}:
		for _, item := range typed {
			if value := nestedContainerString(item, keys...); value != "" {
				return value
			}
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "{") {
			var nested map[string]interface{}
			if err := json.Unmarshal([]byte(trimmed), &nested); err == nil {
				return nestedString(nested, keys...)
			}
		}
		if strings.HasPrefix(trimmed, "[") {
			var nested []interface{}
			if err := json.Unmarshal([]byte(trimmed), &nested); err == nil {
				return nestedContainerString(nested, keys...)
			}
		}
	}
	return ""
}

func nestedValueString(value interface{}, keys ...string) string {
	switch typed := value.(type) {
	case map[string]interface{}:
		return nestedString(typed, keys...)
	case []interface{}:
		var parts []string
		for _, item := range typed {
			if value := nestedValueString(item, keys...); value != "" {
				parts = append(parts, value)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " ")
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if strings.HasPrefix(trimmed, "{") {
			var nested map[string]interface{}
			if err := json.Unmarshal([]byte(trimmed), &nested); err == nil {
				if value := nestedString(nested, keys...); value != "" {
					return value
				}
			}
		}
		if strings.HasPrefix(trimmed, "[") {
			var nested []interface{}
			if err := json.Unmarshal([]byte(trimmed), &nested); err == nil {
				if value := nestedValueString(nested, keys...); value != "" {
					return value
				}
			}
		}
		return trimmed
	}
	return stringFromUnknown(value)
}

func stringFromUnknown(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []interface{}:
		var parts []string
		for _, item := range typed {
			if value := stringFromUnknown(item); value != "" {
				parts = append(parts, value)
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	default:
		return ""
	}
}

// refreshContextUsage polls sessions.describe and updates contextUsage.
func (gs *gatewaySession) refreshContextUsage(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Capture the key once so the stored snapshot is attributed to the session
	// we actually described, even if the session rotates while we wait.
	sessionKey := gs.getSessionKey()
	resp, err := gs.sendReq(pollCtx, "sessions.describe", map[string]string{"key": sessionKey})
	if err != nil {
		log.Printf("[session] sessions.describe for context usage: %v", err)
		return
	}
	var payload struct {
		Session struct {
			InputTokens      *int     `json:"inputTokens"`
			OutputTokens     *int     `json:"outputTokens"`
			TotalTokens      *int     `json:"totalTokens"`
			EstimatedCostUSD *float64 `json:"estimatedCostUsd"`
			Model            string   `json:"model"`
			ModelProvider    string   `json:"modelProvider"`
			ContextTokens    int      `json:"contextTokens"`
		} `json:"session"`
	}
	if err := json.Unmarshal(resp.Payload, &payload); err != nil {
		log.Printf("[session] unmarshal sessions.describe response: %v", err)
		return
	}
	gs.ctxMu.Lock()
	gs.usage = gatewayUsage{sessionKey: sessionKey, inputTokens: payload.Session.InputTokens, outputTokens: payload.Session.OutputTokens, totalTokens: payload.Session.TotalTokens, estimatedCostUSD: payload.Session.EstimatedCostUSD, model: payload.Session.Model, modelProvider: payload.Session.ModelProvider}
	if payload.Session.ContextTokens > 0 && payload.Session.TotalTokens != nil {
		usage := *payload.Session.TotalTokens * 100 / payload.Session.ContextTokens
		if usage > 100 {
			usage = 100
		}
		gs.contextUsage = usage
	}
	gs.ctxMu.Unlock()
}

// ContextUsage returns the last-known context window usage percentage (0-100).
func (gs *gatewaySession) ContextUsage() int {
	gs.ctxMu.RLock()
	defer gs.ctxMu.RUnlock()
	return gs.contextUsage
}

func (gs *gatewaySession) Usage() gatewayUsage {
	gs.ctxMu.RLock()
	defer gs.ctxMu.RUnlock()
	return gs.usage
}

// IsReady returns true once the persistent gateway session is established.
func (gs *gatewaySession) IsReady() bool {
	gs.readyMu.RLock()
	defer gs.readyMu.RUnlock()
	return gs.ready
}

func gatewayProcessExited() bool {
	gatewayProcessState.RLock()
	defer gatewayProcessState.RUnlock()
	return gatewayProcessState.exited
}

func gatewayRestartCount() int {
	gatewayProcessState.RLock()
	defer gatewayProcessState.RUnlock()
	return gatewayProcessState.restartCount
}

func markGatewayProcessRunning() {
	gatewayProcessState.Lock()
	gatewayProcessState.exited = false
	gatewayProcessState.Unlock()
}

func incrementGatewayRestartCount() int {
	gatewayProcessState.Lock()
	defer gatewayProcessState.Unlock()
	gatewayProcessState.restartCount++
	return gatewayProcessState.restartCount
}

func gatewayRestartBaseFromEnv() int {
	count, err := strconv.Atoi(os.Getenv("ELASTICCLAW_BRIDGE_RESTARTS"))
	if err != nil || count < 0 {
		return 0
	}
	return count
}

func markGatewayProcessExited() {
	gatewayProcessState.Lock()
	gatewayProcessState.exited = true
	gs := gatewayProcessState.session
	gatewayProcessState.Unlock()

	// Match readLoop's disconnect path so a caller waiting on a turn gets an
	// immediate error rather than waiting for its deadline.
	if gs == nil {
		return
	}
	gs.infMu.RLock()
	inf := gs.inFlight
	gs.infMu.RUnlock()
	if inf != nil {
		deliverInFlight(inf, agentResult{err: fmt.Errorf("gateway process exited")})
	}
}

func setActiveGatewaySession(gs *gatewaySession) {
	gatewayProcessState.Lock()
	gatewayProcessState.session = gs
	gatewayProcessState.Unlock()
}

func (gs *gatewaySession) setReady() {
	gs.readyMu.Lock()
	gs.ready = true
	gs.readyMu.Unlock()
	// Initial context usage refresh now that session is ready
	go gs.refreshContextUsage(context.Background())
}

func (gs *gatewaySession) createFreshSession(ctx context.Context, reason string) error {
	log.Printf("[session] creating fresh session after %s", reason)
	resp, err := gs.sendReq(ctx, "sessions.create", map[string]string{"agentId": "main"})
	if err != nil {
		return fmt.Errorf("sessions.create: %w", err)
	}
	var payload struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(resp.Payload, &payload)
	if payload.Key == "" {
		return fmt.Errorf("sessions.create returned empty key")
	}
	gs.setSessionKey(payload.Key)
	saveBridgeSession(payload.Key)
	if err := gs.subscribe(ctx); err != nil {
		return err
	}
	gs.ctxMu.Lock()
	gs.contextUsage = 0
	gs.ctxMu.Unlock()
	gs.setReady()
	log.Printf("[session] recovered with fresh session: %s", gs.getSessionKey())
	return nil
}

func (gs *gatewaySession) abortActiveSession(ctx context.Context) error {
	key := gs.getSessionKey()
	if key == "" {
		return nil
	}
	if _, err := gs.sendReq(ctx, "sessions.abort", map[string]string{"key": key}); err != nil {
		return fmt.Errorf("sessions.abort: %w", err)
	}
	return nil
}

// providerRequestFormatErrorFragment is emitted by OpenClaw when a provider
// rejects the assembled request schema or tool payload. Session recovery
// depends on this upstream compatibility string, so keep the coupling explicit.
const providerRequestFormatErrorFragment = "provider rejected the request schema or tool payload"

func isRecoverableSessionSendError(err error) bool {
	var sendErr *sessionSendRequestError
	if !errors.As(err, &sendErr) {
		return false
	}
	msg := strings.ToLower(sendErr.err.Error())
	return strings.Contains(msg, "context overflow") ||
		strings.Contains(msg, "prompt too large") ||
		isProviderRequestFormatError(sendErr.err)
}

func isProviderRequestFormatError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, providerRequestFormatErrorFragment)
}

// isRecoverableSessionLifecycleError identifies failures that can leave an
// accepted OpenClaw turn stuck in the persistent session. Unlike a rejected
// sessions.send request, these turns must not be replayed because they may
// already have performed tool side effects. Rotating the session lets the next
// hub message continue the story without inheriting the poisoned transcript.
func isRecoverableSessionLifecycleError(err error) bool {
	if err == nil {
		return false
	}
	var sendErr *sessionSendRequestError
	if errors.As(err, &sendErr) {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || isProviderRequestFormatError(err)
}

// SendMessage sends a user message to the persistent session, streams chunks
// via onChunk, and returns the full response text.
func (gs *gatewaySession) SendMessage(ctx context.Context, message string, onChunk func(string), onActivity func(agentActivity)) (string, error) {
	// Serialise: only one message in flight at a time
	gs.sendMu.Lock()
	defer gs.sendMu.Unlock()

	delays := []time.Duration{2 * time.Second, 5 * time.Second}
	gatewayRetried := false
	for attempt := 0; ; attempt++ {
		conn := gs.currentConn()
		reply, err := gs.sendMessageOnce(ctx, message, onChunk, onActivity)
		// Retry only failures from the initial sessions.send request. Once the
		// request is accepted, chunks may already be visible to the UI, so stream
		// or lifecycle errors must not replay the turn.
		if err == nil {
			return reply, err
		}
		if isRetryableLLMSendRequestError(err) && attempt < len(delays) {
			delay := delays[attempt]
			log.Printf("[gateway] transient LLM error on attempt %d/%d: %v — retrying in %s", attempt+1, len(delays)+1, err, delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			continue
		}
		if !gatewayRetried && isRetryableGatewaySendRequestError(err) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", ctxErr
			}
			reconnectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			reconnected, reconnectErr := gs.reconnectGateway(reconnectCtx, conn)
			cancel()
			if reconnectErr != nil {
				return "", fmt.Errorf("%w; gateway reconnect failed: %v", err, reconnectErr)
			}
			if reconnected {
				gatewayRetried = true
				log.Printf("[gateway] retrying message after reconnecting closed gateway connection")
			} else {
				log.Printf("[gateway] retrying message after another goroutine reconnected gateway")
			}
			continue
		}
		if isRecoverableSessionLifecycleError(err) {
			abortCtx, abortCancel := context.WithTimeout(context.Background(), 5*time.Second)
			abortErr := gs.abortActiveSession(abortCtx)
			abortCancel()
			if abortErr != nil {
				log.Printf("[session] failed to abort poisoned session before recovery: %v", abortErr)
			}
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			resetErr := gs.createFreshSession(recoveryCtx, err.Error())
			cancel()
			if resetErr != nil {
				return reply, fmt.Errorf("%w; session recovery failed: %v", err, resetErr)
			}
			return reply, fmt.Errorf("%w; OpenClaw session reset so the next message can continue", err)
		}
		if !isRecoverableSessionSendError(err) {
			return reply, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}

		recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resetErr := gs.createFreshSession(recoveryCtx, err.Error())
		cancel()
		if resetErr != nil {
			return "", fmt.Errorf("%w; session recovery failed: %v", err, resetErr)
		}

		log.Printf("[session] retrying message in fresh session after recoverable error")
		return gs.sendMessageOnce(ctx, message, onChunk, onActivity)
	}
}

func (gs *gatewaySession) sendMessageOnce(ctx context.Context, message string, onChunk func(string), onActivity func(agentActivity)) (string, error) {
	inf := &inFlightState{
		onChunk:    onChunk,
		onActivity: onActivity,
		done:       make(chan agentResult, 1),
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
		"key":     gs.getSessionKey(),
		"message": message,
	})
	if err != nil {
		return "", &sessionSendRequestError{err: err}
	}
	log.Printf("[gateway] message sent, streaming response...")
	inf.mu.Lock()
	inf.modelWaitStartedAt = time.Now()
	inf.lastModelPulseAt = time.Time{}
	inf.mu.Unlock()
	inf.emitActivity(agentActivity{Kind: "model_started", Message: "Waiting on model response"})

	// Wait for lifecycle/end from the read loop
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case result := <-inf.done:
			if result.err != nil {
				return "", result.err
			}
			// Refresh context usage after each turn (best-effort)
			go gs.refreshContextUsage(context.Background())
			return result.text, nil
		case <-ticker.C:
			if pulse, ok := inf.toolProgressPulse(); ok {
				inf.emitActivity(cleanAgentActivity(pulse))
			} else if pulse, ok := inf.modelProgressPulse(); ok {
				inf.emitActivity(cleanAgentActivity(pulse))
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

type sessionSendRequestError struct {
	err error
}

func (e *sessionSendRequestError) Error() string {
	return fmt.Sprintf("sessions.send: %v", e.err)
}

func (e *sessionSendRequestError) Unwrap() error {
	return e.err
}

func isRetryableLLMSendRequestError(err error) bool {
	var sendErr *sessionSendRequestError
	if !errors.As(err, &sendErr) {
		return false
	}
	return isRetryableLLMSendError(sendErr.err)
}

func isRetryableGatewaySendRequestError(err error) bool {
	var sendErr *sessionSendRequestError
	if !errors.As(err, &sendErr) {
		return false
	}
	return isRetryableGatewaySendWriteError(sendErr.err)
}

func isRetryableGatewaySendWriteError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.HasPrefix(msg, "sessions.send write:") {
		return false
	}
	if strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "context deadline") ||
		strings.Contains(msg, "deadline exceeded") {
		return false
	}
	return strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unexpected eof") ||
		(strings.Contains(msg, "websocket") && strings.Contains(msg, "closed"))
}

func isMissingGatewaySessionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "session not found") ||
		strings.Contains(msg, "session key not found") ||
		strings.Contains(msg, "unknown session")
}

func isRetryableLLMSendError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "api_error") ||
		strings.Contains(msg, "upstream error") ||
		strings.Contains(msg, "overloaded") ||
		strings.Contains(msg, "temporarily unavailable") ||
		strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "request timeout") ||
		strings.Contains(msg, "model timeout")
}

func formatActivityDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
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
			// Start the daemon immediately in the bg goroutine so that when
			// receivers (setupFlake or finishNix) get the nixDone signal, the
			// daemon is already running. This guarantees devShell pre-eval
			// has the daemon ready.
			// Redirect stdout/stderr *before* & so the daemon does not inherit
			// the pipes from runShell (which waits for pipes to close).
			_ = runShell("sudo /nix/var/nix/profiles/default/bin/nix-daemon >/dev/null 2>&1 &")
			time.Sleep(1 * time.Second)
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

// setupFlakeEnvironmentSync is the synchronous version that waits for Nix and sets up the flake.
// Used when we need the flake ready before starting the gateway.
func setupFlakeEnvironmentSync(nixDone <-chan error) error {
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
		case <-time.After(5 * time.Minute):
			return fmt.Errorf("timeout waiting for Nix install")
		}
	}

	// Ensure nix command is available
	if err := runShell("command -v nix"); err != nil {
		return fmt.Errorf("nix command not available after install: %w", err)
	}

	// Daemon was started by the nixInstallBg goroutine as soon as install completed.
	// Just set env + source + wait for nix to be usable before pre-eval.
	os.Setenv("NIX_REMOTE", "daemon")
	_ = runShell(". /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true")

	// Probe the *daemon* (not just the client). `nix --version` can succeed
	// with NIX_REMOTE=daemon even if the daemon is not listening.
	// Use `nix store ping` which actually contacts the store/daemon.
	// Fail closed if it never becomes ready (per workspace flake contract).
	for i := 0; i < 30; i++ {
		if err := runShell("nix store ping >/dev/null 2>&1"); err == nil {
			log.Printf("[bootstrap] nix daemon responded to ping after %ds", i)
			break
		}
		if i == 29 {
			return fmt.Errorf("nix daemon did not become ready (nix store ping never succeeded; check /tmp/nix-install.log and daemon logs)")
		}
		time.Sleep(1 * time.Second)
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
			if err := os.WriteFile(filepath.Join(flakeDir, "flake.lock"), flakeLockData, 0644); err != nil {
				return fmt.Errorf("failed to write flake.lock: %w", err)
			}
		}
	}

	// Create the provider-neutral flake-run wrapper that enters devShells.default.
	// Workspace flakes supply claw-wide tools (e.g. depot). This is the
	// contract used by both the agent gateway and deterministic run steps.
	// We deliberately do not use "nix profile install ." (wrong for dev-shell-only flakes).
	wrapperScript := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
export PATH="/nix/var/nix/profiles/default/bin:$PATH"
. /nix/var/nix/profiles/default/etc/profile.d/nix-daemon.sh 2>/dev/null || true
cd %q
exec nix develop --accept-flake-config -c "$@"
`, flakeDir)
	wrapperPath := filepath.Join(home, ".elasticclaw", "flake-run")
	if err := os.WriteFile(wrapperPath, []byte(wrapperScript), 0755); err != nil {
		return fmt.Errorf("failed to write flake wrapper: %w", err)
	}

	// Pre-evaluate the devShell with an inexpensive command. This builds the
	// environment from the pinned flake.lock and fails fast on bad flakes.
	log.Printf("[bootstrap] pre-evaluating workspace devShell via flake-run...")
	if err := runShell(`~/.elasticclaw/flake-run true`); err != nil {
		return fmt.Errorf("workspace flake devShell evaluation failed (try 'nix develop --accept-flake-config' locally to debug): %w", err)
	}

	log.Printf("[bootstrap] flake environment ready at %s (devShell pre-evaluated)", flakeDir)
	return nil
}

// startGatewayWithFlake starts the gateway, optionally inside the flake environment.
func startGatewayWithFlake(useFlake bool) (*exec.Cmd, error) {
	if !useFlake {
		// No flake, use normal gateway start
		return startGateway()
	}

	log.Printf("[bootstrap] starting OpenClaw gateway inside nix develop via flake-run...")

	// Set env vars that suppress respawn / bonjour.
	os.Setenv("OPENCLAW_NO_RESPAWN", "1")
	os.Setenv("OPENCLAW_DISABLE_BONJOUR", "1")

	home, _ := os.UserHomeDir()
	logFile := filepath.Join(home, "openclaw-gateway.log")

	// Use the wrapper (created and pre-evaluated during setupFlakeEnvironmentSync).
	// This is consistent with how workflow run commands and other tools are invoked.
	script := `~/.elasticclaw/flake-run openclaw gateway run`

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
node_compatible() {
  command -v node >/dev/null 2>&1 &&
    node -e 'const [major, minor] = process.versions.node.split(".").map(Number); process.exit(major === 24 && minor >= 15 ? 0 : 1)'
}
if command -v git >/dev/null 2>&1 && command -v npm >/dev/null 2>&1 && node_compatible; then
  echo "Node: $(node --version)"
  echo "Git: $(git --version)"
  exit 0
fi
# Single apt-get update then install everything in one pass to avoid
# dpkg state corruption from multiple overlapping install calls.
sudo apt-get update -qq
sudo apt-get install -y --fix-broken curl ca-certificates git gnupg
if ! command -v npm >/dev/null 2>&1 || ! node_compatible; then
  if [ ! -f /etc/apt/sources.list.d/nodesource.sources ] && [ ! -f /etc/apt/sources.list.d/nodesource.list ]; then
    sudo mkdir -p /etc/apt/keyrings
    curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | \
      sudo gpg --batch --yes --dearmor -o /etc/apt/keyrings/nodesource.gpg
    echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_24.x nodistro main" | \
      sudo tee /etc/apt/sources.list.d/nodesource.list > /dev/null
  fi
  sudo apt-get update -qq
  sudo apt-get install -y --fix-broken nodejs
fi
echo "Node: $(node --version)"
echo "Git: $(git --version)"
`
	return runShell(script)
}

const openClawVersion = cliversion.OpenClawVersion
const codexPluginVersion = cliversion.CodexPluginVersion
const codexCLIVersion = cliversion.CodexCLIVersion
const grokCLIVersion = cliversion.GrokCLIVersion

// ensureUserNPMBinOnPath configures a user-owned npm prefix and makes binaries
// installed there visible to this process and every child process it starts.
// Some provider images run the bridge as an unprivileged user without sudo.
func ensureUserNPMBinOnPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve user home: empty path")
	}

	prefix := filepath.Join(home, ".local")
	binDir := filepath.Join(prefix, "bin")
	pathEntries := filepath.SplitList(os.Getenv("PATH"))
	for _, entry := range pathEntries {
		if entry == binDir {
			return prefix, nil
		}
	}
	pathEntries = append([]string{binDir}, pathEntries...)
	if err := os.Setenv("PATH", strings.Join(pathEntries, string(os.PathListSeparator))); err != nil {
		return "", fmt.Errorf("prepend user npm bin to PATH: %w", err)
	}
	return prefix, nil
}

func installUserNPMPackage(spec string) error {
	prefix, err := ensureUserNPMBinOnPath()
	if err != nil {
		return err
	}
	return runShell(fmt.Sprintf(
		"npm install -g --prefix %s %s --ignore-scripts",
		shellQuote(prefix),
		shellQuote(spec),
	))
}

// installOpenClaw installs the pinned OpenClaw CLI via npm.
func installOpenClaw() error {
	if _, err := ensureUserNPMBinOnPath(); err != nil {
		return err
	}
	if out, err := exec.Command("openclaw", "--version").CombinedOutput(); err == nil {
		version := strings.TrimSpace(string(out))
		if strings.Contains(version, openClawVersion) {
			log.Printf("[bootstrap] OpenClaw already installed: %s", version)
			return nil
		}
		log.Printf("[bootstrap] OpenClaw version %s found, installing pinned %s", version, openClawVersion)
	}
	log.Printf("[bootstrap] installing openclaw@%s...", openClawVersion)
	if err := installUserNPMPackage("openclaw@" + openClawVersion); err != nil {
		return fmt.Errorf("npm install openclaw: %w", err)
	}
	out, err := exec.Command("openclaw", "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify openclaw install: %w: %s", err, strings.TrimSpace(string(out)))
	}
	version := strings.TrimSpace(string(out))
	if !strings.Contains(version, openClawVersion) {
		return fmt.Errorf("verify openclaw install: got %q, want version %s", version, openClawVersion)
	}
	log.Printf("[bootstrap] OpenClaw: %s", version)
	return nil
}

func cliCodingProviderForModel(model string) string {
	switch {
	case strings.HasPrefix(model, "codex/"):
		return "codex"
	case strings.HasPrefix(model, "grok/"):
		return "grok"
	default:
		return ""
	}
}

func installNPMCLI(packageName, version, binaryName string) error {
	if strings.TrimSpace(version) == "" {
		return fmt.Errorf("empty version for %s", packageName)
	}
	if out, err := exec.Command(binaryName, "--version").CombinedOutput(); err == nil {
		installedVersion := strings.TrimSpace(string(out))
		if strings.Contains(installedVersion, version) {
			log.Printf("[bootstrap] %s already installed: %s", binaryName, installedVersion)
			return nil
		}
	}
	spec := packageName + "@" + version
	log.Printf("[bootstrap] installing %s...", spec)
	if err := installUserNPMPackage(spec); err != nil {
		return fmt.Errorf("npm install %s: %w", spec, err)
	}
	out, err := exec.Command(binaryName, "--version").CombinedOutput()
	if err != nil {
		log.Printf("[bootstrap] %s version check warning: %v: %s", binaryName, err, strings.TrimSpace(string(out)))
		return nil
	}
	log.Printf("[bootstrap] %s: %s", binaryName, strings.TrimSpace(string(out)))
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func restoreCLIModelAuth() error {
	state := strings.TrimSpace(os.Getenv("ELASTICCLAW_MODEL_AUTH_STATE"))
	if state == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(state)
	if err != nil {
		return fmt.Errorf("decode auth state: %w", err)
	}
	var bundle cliAuthBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return fmt.Errorf("parse auth state: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	for rel, encoded := range bundle.Files {
		clean := filepath.Clean(rel)
		if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
			continue
		}
		content, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("decode auth file %s: %w", rel, err)
		}
		path := filepath.Join(home, clean)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return fmt.Errorf("create auth dir %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			return fmt.Errorf("write auth file %s: %w", rel, err)
		}
	}
	if os.Getenv("ELASTICCLAW_MODEL_AUTH_PROVIDER") == "grok" {
		if err := migrateGrokOAuthToOpenClaw(home); err != nil {
			return fmt.Errorf("migrate Grok OAuth to OpenClaw: %w", err)
		}
	}
	log.Printf("[bootstrap] restored %d CLI auth files for %s", len(bundle.Files), os.Getenv("ELASTICCLAW_MODEL_AUTH_PROVIDER"))
	return nil
}

func migrateGrokOAuthToOpenClaw(home string) error {
	type grokAuthEntry struct {
		Key          string `json:"key"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		Email        string `json:"email"`
		UserID       string `json:"user_id"`
		OIDCIssuer   string `json:"oidc_issuer"`
	}

	data, err := os.ReadFile(filepath.Join(home, ".grok", "auth.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var grokAuth map[string]grokAuthEntry
	if err := json.Unmarshal(data, &grokAuth); err != nil {
		return fmt.Errorf("parse Grok auth: %w", err)
	}
	var selected grokAuthEntry
	for _, entry := range grokAuth {
		if entry.Key != "" && entry.RefreshToken != "" {
			selected = entry
			break
		}
	}
	if selected.Key == "" || selected.RefreshToken == "" {
		return nil
	}

	expires := int64(0)
	if parsed, err := time.Parse(time.RFC3339Nano, selected.ExpiresAt); err == nil {
		expires = parsed.UnixMilli()
	}
	issuer := strings.TrimRight(selected.OIDCIssuer, "/")
	if issuer == "" {
		issuer = "https://auth.x.ai"
	}
	credential := map[string]interface{}{
		"type":          "oauth",
		"provider":      "xai",
		"access":        selected.Key,
		"refresh":       "elasticclaw-managed",
		"expires":       expires,
		"issuer":        issuer,
		"tokenEndpoint": issuer + "/oauth2/token",
	}
	if selected.Email != "" {
		credential["email"] = selected.Email
	}
	if selected.UserID != "" {
		credential["accountId"] = selected.UserID
	}

	authPath := filepath.Join(home, ".openclaw", "agents", "main", "agent", "auth-profiles.json")
	authStore := map[string]interface{}{}
	if existing, err := os.ReadFile(authPath); err == nil {
		if err := json.Unmarshal(existing, &authStore); err != nil {
			return fmt.Errorf("parse OpenClaw auth profiles: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	profiles, _ := authStore["profiles"].(map[string]interface{})
	if profiles == nil {
		profiles = map[string]interface{}{}
	}
	profiles["xai:default"] = credential
	authStore["version"] = 1
	authStore["profiles"] = profiles

	encoded, err := json.MarshalIndent(authStore, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(authPath), 0700); err != nil {
		return err
	}
	tempPath := authPath + ".tmp"
	if err := os.WriteFile(tempPath, encoded, 0600); err != nil {
		return err
	}
	if err := os.Rename(tempPath, authPath); err != nil {
		return err
	}
	return nil
}

func installSelectedCodingModelCLI() error {
	model := envOr("OPENCLAW_DEFAULT_MODEL", "")
	switch cliCodingProviderForModel(model) {
	case "codex":
		return installNPMCLI("@openai/codex", cliversion.FromEnv("ELASTICCLAW_CODEX_CLI_VERSION", codexCLIVersion), "codex")
	case "grok":
		return installNPMCLI("@xai-official/grok", cliversion.FromEnv("ELASTICCLAW_GROK_CLI_VERSION", grokCLIVersion), "grok")
	default:
		return nil
	}
}

func installSelectedModelPlugin() error {
	if strings.TrimSpace(os.Getenv("ELASTICCLAW_LLM_PROVIDER")) != "codex" {
		return nil
	}
	if err := exec.Command("openclaw", "plugins", "info", "codex", "--json").Run(); err == nil {
		log.Printf("[bootstrap] Codex plugin already installed")
		return nil
	}
	version := cliversion.FromEnv("ELASTICCLAW_CODEX_PLUGIN_VERSION", codexPluginVersion)
	if strings.TrimSpace(version) == "" {
		return fmt.Errorf("empty version for @openclaw/codex")
	}
	spec := "npm:@openclaw/codex@" + version
	log.Printf("[bootstrap] installing %s...", spec)
	out, err := exec.Command("openclaw", "plugins", "install", spec).CombinedOutput()
	if err != nil {
		return fmt.Errorf("install %s: %w: %s", spec, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("openclaw", "plugins", "info", "codex", "--json").CombinedOutput(); err != nil {
		return fmt.Errorf("verify Codex plugin install: %w: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("[bootstrap] Codex plugin: %s", version)
	return nil
}

func syncOpenClawAPIKeyAuth() error {
	script := strings.TrimSpace(os.Getenv("ELASTICCLAW_API_KEY_AUTH_SYNC"))
	if script == "" {
		return nil
	}
	log.Printf("[bootstrap] syncing OpenClaw API-key auth...")
	if err := runShell(script); err != nil {
		return fmt.Errorf("sync OpenClaw API-key auth: %w", err)
	}
	return nil
}

func syncOpenClawOAuthAuth() error {
	script := strings.TrimSpace(os.Getenv("ELASTICCLAW_OAUTH_AUTH_SYNC"))
	if script == "" {
		return nil
	}
	log.Printf("[bootstrap] syncing OpenClaw OAuth auth...")
	if err := runShell(script); err != nil {
		return fmt.Errorf("sync OpenClaw OAuth auth: %w", err)
	}
	return nil
}

type managedModelAuthCredential struct {
	Provider string `json:"provider"`
	Access   string `json:"access"`
	Expires  int64  `json:"expires"`
}

const managedGrokAuthSyncInterval = 5 * time.Minute

func applyManagedGrokOAuthCredential(credential managedModelAuthCredential) error {
	if credential.Provider != "xai" || credential.Access == "" || credential.Expires <= 0 {
		return fmt.Errorf("managed Grok credential response is incomplete")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	if err := writeManagedGrokCredentialToOpenClaw(home, credential); err != nil {
		return err
	}
	if err := writeManagedGrokCredentialToCLI(home, credential); err != nil {
		return fmt.Errorf("update local Grok credential: %w", err)
	}
	return nil
}

func writeManagedGrokCredentialToOpenClaw(home string, credential managedModelAuthCredential) error {
	dbPath := filepath.Join(home, ".openclaw", "agents", "main", "agent", "openclaw-agent.sqlite")
	db, err := sql.Open("sqlite", dbPath+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open OpenClaw auth database: %w", err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin OpenClaw auth update: %w", err)
	}
	defer tx.Rollback()
	var storeJSON string
	if err := tx.QueryRow(`SELECT store_json FROM auth_profile_store WHERE store_key='primary'`).Scan(&storeJSON); err != nil {
		return fmt.Errorf("read OpenClaw auth store: %w", err)
	}
	var store map[string]any
	if err := json.Unmarshal([]byte(storeJSON), &store); err != nil {
		return fmt.Errorf("parse OpenClaw auth store: %w", err)
	}
	profiles, _ := store["profiles"].(map[string]any)
	if profiles == nil {
		profiles = map[string]any{}
		store["profiles"] = profiles
	}
	profile, _ := profiles["xai:default"].(map[string]any)
	if profile == nil {
		profile = map[string]any{}
	}
	profile["type"] = "oauth"
	profile["provider"] = "xai"
	profile["access"] = credential.Access
	profile["refresh"] = "elasticclaw-managed"
	profile["expires"] = credential.Expires
	profiles["xai:default"] = profile
	updated, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("encode OpenClaw auth store: %w", err)
	}
	if _, err := tx.Exec(`UPDATE auth_profile_store SET store_json=?, updated_at=? WHERE store_key='primary'`, string(updated), time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("write OpenClaw auth store: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit OpenClaw auth update: %w", err)
	}
	return nil
}

func writeManagedGrokCredentialToCLI(home string, credential managedModelAuthCredential) error {
	authPath := filepath.Join(home, ".grok", "auth.json")
	data, err := os.ReadFile(authPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	const canonicalCredential = "https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828"
	entry, ok := document[canonicalCredential].(map[string]any)
	if !ok {
		return fmt.Errorf("Grok auth file has no canonical xAI OAuth credential")
	}
	entry["key"] = credential.Access
	entry["refresh_token"] = "elasticclaw-managed"
	entry["expires_at"] = time.UnixMilli(credential.Expires).UTC().Format(time.RFC3339Nano)
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	tempPath := authPath + ".tmp"
	if err := os.WriteFile(tempPath, updated, 0600); err != nil {
		return err
	}
	return os.Rename(tempPath, authPath)
}

var openClawWorkspaceManagedFiles = map[string]bool{
	"elasticclaw-config.yaml": true,
	"SOUL.md":                 true,
	"AGENTS.md":               true,
	"TOOLS.md":                true,
	"IDENTITY.md":             true,
	"USER.md":                 true,
	"MEMORY.md":               true,
	"BOOTSTRAP.md":            true,
	"HEARTBEAT.md":            true,
	"CONTEXT.md":              true,
	"SECRETS.md":              true,
	"TRIGGER_INPUTS.json":     true,
}

func syncStagedWorkspaceToOpenClawWorkspace() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	stagedDir := filepath.Join(home, "workspace")
	activeDir := filepath.Join(home, ".openclaw", "workspace")
	if _, err := os.Stat(stagedDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat staged workspace: %w", err)
	}
	if err := os.MkdirAll(activeDir, 0700); err != nil {
		return fmt.Errorf("create OpenClaw workspace: %w", err)
	}
	for name := range openClawWorkspaceManagedFiles {
		if err := os.RemoveAll(filepath.Join(activeDir, name)); err != nil {
			return fmt.Errorf("remove stale OpenClaw workspace file %s: %w", name, err)
		}
	}
	stagedAbs, err := filepath.Abs(stagedDir)
	if err != nil {
		return fmt.Errorf("abs staged workspace: %w", err)
	}
	activeAbs, err := filepath.Abs(activeDir)
	if err != nil {
		return fmt.Errorf("abs OpenClaw workspace: %w", err)
	}
	copied := 0
	err = filepath.Walk(stagedAbs, func(src string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(stagedAbs, src)
		if err != nil {
			return err
		}
		if rel == "." || rel == ".elasticclaw-workspace-ready" {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			log.Printf("[bootstrap] skipping workspace symlink %s", rel)
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		dest := filepath.Join(activeAbs, rel)
		if dest != activeAbs && !strings.HasPrefix(dest, activeAbs+string(filepath.Separator)) {
			return fmt.Errorf("workspace path escapes OpenClaw workspace: %s", rel)
		}
		if info.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read staged workspace file %s: %w", rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return fmt.Errorf("create OpenClaw workspace dir for %s: %w", rel, err)
		}
		mode := info.Mode().Perm()
		if mode == 0 {
			mode = 0600
		}
		if err := os.WriteFile(dest, data, mode); err != nil {
			return fmt.Errorf("write OpenClaw workspace file %s: %w", rel, err)
		}
		copied++
		return nil
	})
	if err != nil {
		return err
	}
	log.Printf("[bootstrap] synced %d staged workspace files into %s", copied, activeDir)

	// Nix flakes require flake.nix/flake.lock to be tracked by git when the
	// workspace directory is a git repo. Ensure they are staged so subsequent
	// nix develop / flake-run invocations can evaluate the flake.
	if _, err := os.Stat(filepath.Join(activeDir, ".git")); err == nil {
		for _, name := range []string{"flake.nix", "flake.lock"} {
			p := filepath.Join(activeDir, name)
			if _, err := os.Stat(p); err == nil {
				if err := runShell(fmt.Sprintf("cd %q && git add %q", activeDir, name)); err != nil {
					log.Printf("[bootstrap] warning: git add %s: %v", name, err)
				}
			}
		}
	}
	return nil
}

type bootstrapRepoAccess struct {
	Repo        string `json:"repo"`
	Permissions string `json:"permissions"`
}

func configuredGitHubRepos() ([]bootstrapRepoAccess, error) {
	raw := strings.TrimSpace(os.Getenv("ELASTICCLAW_GITHUB_REPOS"))
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var repos []bootstrapRepoAccess
	if err := json.Unmarshal([]byte(raw), &repos); err != nil {
		return nil, fmt.Errorf("parse ELASTICCLAW_GITHUB_REPOS: %w", err)
	}
	out := repos[:0]
	for _, repo := range repos {
		repo.Repo = strings.TrimSpace(repo.Repo)
		if repo.Repo != "" {
			out = append(out, repo)
		}
	}
	return out, nil
}

func repoDirectoryName(repo string) string {
	parts := strings.Split(repo, "/")
	if len(parts) == 0 {
		return repo
	}
	name := strings.TrimSpace(parts[len(parts)-1])
	return strings.TrimSuffix(name, ".git")
}

func dockerGitHubCredentialHelperScript(tokenEndpoint string) string {
	return fmt.Sprintf(`set -euo pipefail
if ! command -v git >/dev/null 2>&1; then
  echo "git is required to clone configured repositories" >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required to refresh GitHub credentials" >&2
  exit 1
fi
if ! command -v sed >/dev/null 2>&1; then
  echo "sed is required to parse GitHub credential responses" >&2
  exit 1
fi
helper_dir="${HOME}/.elasticclaw/bin"
helper_path="${helper_dir}/elasticclaw-git-credentials"
mkdir -p "$helper_dir"
old_umask="$(umask)"
umask 0077
cat > "$helper_path" << 'CREDEOF'
#!/bin/sh
set -eu
claw_token="${ELASTICCLAW_CLAW_TOKEN:-}"
if [ -z "$claw_token" ]; then
  echo "ELASTICCLAW_CLAW_TOKEN is required for GitHub credentials" >&2
  exit 1
fi
response="$(curl -sS --max-time 35 --get --data-urlencode "claw_token=$claw_token" %s)"
token="$(printf '%%s' "$response" | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
if [ -z "$token" ]; then
  echo "GitHub token response did not include token" >&2
  printf '%%s\n' "$response" >&2
  exit 1
fi
printf 'protocol=https\n'
printf 'host=github.com\n'
printf 'username=x-access-token\n'
printf 'password=%%s\n' "$token"
CREDEOF
umask "$old_umask"
chmod 0700 "$helper_path"
git config --global --unset-all credential.helper >/dev/null 2>&1 || true
git config --global credential.helper "!$helper_path"
git config --global --get-all credential.helper | grep -Fx "!$helper_path" >/dev/null
helper_check="$("$helper_path" 2>&1)" || {
  echo "GitHub credential helper failed during bootstrap:" >&2
  printf '%%s\n' "$helper_check" >&2
  exit 1
}
printf '%%s\n' "$helper_check" | grep -Fx 'username=x-access-token' >/dev/null || {
  echo "GitHub credential helper did not output 'username=x-access-token'" >&2
  exit 1
}
printf '%%s\n' "$helper_check" | grep -E '^password=.' >/dev/null || {
  echo "GitHub credential helper did not output a password" >&2
  exit 1
}
credential_check="$(printf 'protocol=https\nhost=github.com\n\n' | GIT_TERMINAL_PROMPT=0 git credential fill 2>&1)" || {
  echo "git credential fill failed after helper registration:" >&2
  git config --global --get-all credential.helper >&2 || true
  printf '%%s\n' "$credential_check" >&2
  exit 1
}
printf '%%s\n' "$credential_check" | grep -Fx 'username=x-access-token' >/dev/null
printf '%%s\n' "$credential_check" | grep -E '^password=.' >/dev/null
`, shellQuote(tokenEndpoint))
}

func installDockerGitHubCredentialHelper() error {
	repos, err := configuredGitHubRepos()
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		return nil
	}
	hubURL := strings.TrimRight(os.Getenv("ELASTICCLAW_HUB_URL"), "/")
	clawID := os.Getenv("ELASTICCLAW_CLAW_ID")
	clawToken := os.Getenv("ELASTICCLAW_CLAW_TOKEN")
	if hubURL == "" || clawID == "" || clawToken == "" {
		return fmt.Errorf("GitHub repositories configured but hub URL, claw ID, or claw token is missing")
	}
	script := dockerGitHubCredentialHelperScript(hubURL + "/api/github/token/" + url.PathEscape(clawID))
	if err := runShell(script); err != nil {
		return fmt.Errorf("install GitHub credential helper: %w", err)
	}
	log.Printf("[bootstrap] GitHub credential helper installed for %d configured repositories", len(repos))
	return nil
}

func dockerGitHubCloneScript(workspaceDir string, repos []bootstrapRepoAccess) string {
	if len(repos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	b.WriteString("export GIT_TERMINAL_PROMPT=0\n")
	fmt.Fprintf(&b, "cd %s\n", shellQuote(workspaceDir))
	b.WriteString("git config --global --get credential.helper >/dev/null\n")
	for _, repo := range repos {
		dir := repoDirectoryName(repo.Repo)
		cloneURL := "https://github.com/" + repo.Repo + ".git"
		fmt.Fprintf(&b, "echo %s\n", shellQuote("[bootstrap] cloning "+repo.Repo+" into "+dir))
		fmt.Fprintf(&b, "if [ ! -d %s ]; then git clone %s %s; else git -C %s remote set-url origin %s || true; git -C %s fetch origin; branch=\"$(git -C %s symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's#^origin/##')\"; if [ -z \"$branch\" ]; then branch=\"$(git -C %s rev-parse --abbrev-ref HEAD)\"; fi; git -C %s reset --hard \"origin/$branch\"; fi\n",
			shellQuote(dir), shellQuote(cloneURL), shellQuote(dir), shellQuote(dir), shellQuote(cloneURL), shellQuote(dir), shellQuote(dir), shellQuote(dir), shellQuote(dir))
		fmt.Fprintf(&b, "test -d %s\n", shellQuote(filepath.Join(dir, ".git")))
	}
	return b.String()
}

func cloneConfiguredGitHubRepos() error {
	repos, err := configuredGitHubRepos()
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	workspaceDir := filepath.Join(home, ".openclaw", "workspace")
	if err := os.MkdirAll(workspaceDir, 0700); err != nil {
		return fmt.Errorf("create OpenClaw workspace: %w", err)
	}
	if err := runShell(dockerGitHubCloneScript(workspaceDir, repos)); err != nil {
		return fmt.Errorf("clone configured repositories: %w", err)
	}
	log.Printf("[bootstrap] cloned or updated %d configured repositories", len(repos))
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
	log.Printf("[bootstrap] OpenClaw config model=%q config_patch=%t gateway_password=%t", defaultModel, providerSnippet != "", gatewayPassword != "")

	// Build the python config patch.
	// The provider snippet (if any) patches openclaw.json: sets the agent default
	// model, strips the legacy top-level models catalog, and configures the gateway.
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

func patchOllamaLocalDevCatalog(baseURL string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	modelID := ""
	if model := os.Getenv("OPENCLAW_DEFAULT_MODEL"); strings.HasPrefix(model, "ollama/") {
		modelID = strings.TrimPrefix(model, "ollama/")
	}
	catalogPath := filepath.Join(home, ".openclaw", "agents", "main", "agent", "plugins", "ollama", "catalog.json")

	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if err := patchOllamaLocalDevCatalogFile(catalogPath, baseURL, modelID); err == nil {
			log.Printf("[bootstrap] patched Ollama local dev catalog: %s", baseURL)
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("patch Ollama catalog: %w", lastErr)
}

func patchOllamaLocalDevCatalogFile(path, baseURL, modelID string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var catalog map[string]interface{}
	if err := json.Unmarshal(data, &catalog); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	providers, ok := catalog["providers"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("catalog has no providers object")
	}
	provider, ok := providers["ollama"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("catalog has no ollama provider")
	}

	provider["baseUrl"] = baseURL
	if models, ok := provider["models"].([]interface{}); ok {
		for _, item := range models {
			model, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if modelID != "" && model["id"] != modelID {
				continue
			}
			model["baseUrl"] = baseURL
			model["contextWindow"] = 32768
			model["maxTokens"] = 1024
			model["params"] = map[string]interface{}{
				"num_ctx":    32768,
				"thinking":   false,
				"keep_alive": "15m",
			}
			compat, ok := model["compat"].(map[string]interface{})
			if !ok {
				compat = map[string]interface{}{}
				model["compat"] = compat
			}
			compat["supportsTools"] = true
			compat["supportsUsageInStreaming"] = true
		}
	}

	patched, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, patched, 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
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

const (
	gatewayRestartBudget = 3
	gatewayStableUptime  = 60 * time.Second
)

// superviseGatewayLoop waits for a gateway process and replaces it after an
// unexpected exit. Its hooks keep the restart policy testable without starting
// real processes.
func superviseGatewayLoop(wait func() error, restart func() (func() error, int, error), sleep func(time.Duration), now func() time.Time, exited func(), running func(), restarted func() int) {
	attempts := 0
	for {
		startedAt := now()
		if err := wait(); err != nil {
			log.Printf("[supervisor] gateway exited: %v", err)
		} else {
			log.Printf("[supervisor] gateway exited")
		}
		exited()

		if now().Sub(startedAt) >= gatewayStableUptime {
			attempts = 0
		}

		// Retry restarts here instead of re-waiting on the dead process: a
		// failed attempt must not call the stale wait fn again.
		replaced := false
		for attempts < gatewayRestartBudget {
			sleep(2 * time.Second << attempts)
			attempts++
			nextWait, pid, err := restart()
			if err != nil {
				log.Printf("[supervisor] gateway restart attempt %d failed: %v", attempts, err)
				continue
			}

			restartCount := restarted()
			running()
			log.Printf("[supervisor] gateway restarted (pid=%d, restart_count=%d)", pid, restartCount)
			wait = nextWait
			replaced = true
			break
		}
		if !replaced {
			log.Printf("[supervisor] gateway restart budget exhausted after %d attempts — leaving unhealthy for hub watchdog", gatewayRestartBudget)
			return
		}
	}
}

func superviseGateway(gateway *exec.Cmd, hasFlake bool) {
	superviseGatewayLoop(
		gateway.Wait,
		func() (func() error, int, error) {
			cmd, err := startGatewayWithFlake(hasFlake)
			if err != nil {
				return nil, 0, err
			}
			if !checkGateway("localhost:18789") {
				stopGateway(cmd)
				return nil, 0, fmt.Errorf("gateway did not become healthy")
			}
			return cmd.Wait, cmd.Process.Pid, nil
		},
		time.Sleep,
		time.Now,
		markGatewayProcessExited,
		markGatewayProcessRunning,
		incrementGatewayRestartCount,
	)
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
		// Redirect before & so it does not keep runShell pipes open.
		_ = runShell("sudo /nix/var/nix/profiles/default/bin/nix-daemon >/dev/null 2>&1 &")
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

func waitForWorkspaceReadyIfRequested() error {
	if os.Getenv("ELASTICCLAW_WAIT_FOR_WORKSPACE") != "1" {
		return nil
	}
	home, _ := os.UserHomeDir()
	readyPath := filepath.Join(home, "workspace", ".elasticclaw-workspace-ready")
	log.Printf("[bootstrap] waiting for workspace files at %s...", readyPath)
	for i := 0; i < 300; i++ {
		if _, err := os.Stat(readyPath); err == nil {
			log.Printf("[bootstrap] workspace files ready after %ds", i)
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("workspace files not ready after 300s")
}

// runBootstrap executes all VM bootstrap steps, then returns so the caller
// can continue into the normal bridge connect loop.
func runBootstrap() error {
	log.Printf("[bootstrap] starting claw-bridge bootstrap mode")

	if err := waitForWorkspaceReadyIfRequested(); err != nil {
		return err
	}

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

	// Start finishNix goroutine early. It waits on nixDone and starts the daemon +
	// sets NIX_REMOTE. This ensures the daemon is available before any
	// pre-eval of devShells (flake-run) or gateway start inside nix develop.
	go finishNix(nixDone)

	// Step 2b: Start Docker install in background (if requested)
	var dockerDone <-chan error
	if os.Getenv("ELASTICCLAW_DOCKER") == "true" {
		dockerDone = dockerInstallBg()
	}

	// Step 3: Install OpenClaw (needs Node)
	if err := installOpenClaw(); err != nil {
		return fmt.Errorf("installOpenClaw: %w", err)
	}

	// Step 3b: Install pinned CLI-backed model providers when selected.
	if err := installSelectedCodingModelCLI(); err != nil {
		return fmt.Errorf("installSelectedCodingModelCLI: %w", err)
	}

	if err := restoreCLIModelAuth(); err != nil {
		return fmt.Errorf("restoreCLIModelAuth: %w", err)
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
	if err := installSelectedModelPlugin(); err != nil {
		return fmt.Errorf("installSelectedModelPlugin: %w", err)
	}
	if err := syncOpenClawOAuthAuth(); err != nil {
		return err
	}
	if err := syncStagedWorkspaceToOpenClawWorkspace(); err != nil {
		return fmt.Errorf("syncStagedWorkspaceToOpenClawWorkspace: %w", err)
	}

	if err := installDockerGitHubCredentialHelper(); err != nil {
		return fmt.Errorf("installDockerGitHubCredentialHelper: %w", err)
	}
	if err := cloneConfiguredGitHubRepos(); err != nil {
		return fmt.Errorf("cloneConfiguredGitHubRepos: %w", err)
	}

	if err := syncOpenClawAPIKeyAuth(); err != nil {
		return err
	}

	// Step 5: Set up flake environment BEFORE starting gateway if flake.nix exists
	// This ensures the gateway runs inside the flake environment.
	// Fail closed: a bad declared flake must not produce a claw without the declared tools.
	if hasFlake {
		if err := setupFlakeEnvironmentSync(nixDone); err != nil {
			return fmt.Errorf("setup workspace flake: %w", err)
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
	if strings.HasPrefix(envOr("OPENCLAW_DEFAULT_MODEL", ""), "ollama/") {
		if err := patchOllamaLocalDevCatalog("http://ollama:11434"); err != nil {
			log.Printf("[bootstrap] warning: patch Ollama local dev catalog: %v", err)
		} else {
			stopGateway(gateway)
			gateway, err = startGatewayWithFlake(hasFlake)
			if err != nil {
				return fmt.Errorf("restartGateway after Ollama catalog patch: %w", err)
			}
		}
	}
	go superviseGateway(gateway, hasFlake)

	// Step 9: Wait for device.json
	if err := waitForDeviceJSON(); err != nil {
		return fmt.Errorf("waitForDeviceJSON: %w", err)
	}

	// Step 10: finish Docker (Nix finisher was started early so daemon is ready
	// for any flake pre-eval / nix develop before gateway).
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

func printVersion() {
	fmt.Printf("claw-bridge %s (commit: %s, built: %s)\n", Version, Commit, BuildDate)
}

func main() {
	// Retain restart history when an external supervisor relaunches this bridge.
	gatewayRestartBase = gatewayRestartBaseFromEnv()

	// Handle version requests very early, before any env var requirements.
	// Supports: claw-bridge version, claw-bridge --version, claw-bridge -v
	if len(os.Args) > 1 {
		arg := os.Args[1]
		if arg == "version" || arg == "--version" || arg == "-v" {
			printVersion()
			os.Exit(0)
		}
	}

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
	modelAuthToken := strings.TrimSpace(os.Getenv("ELASTICCLAW_MODEL_AUTH_TOKEN"))
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
	deduper := newMessageDeduper()
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
	log.Printf("[bridge] gateway session ready: %s", gwSession.getSessionKey())
	setActiveGatewaySession(gwSession)

	// Start status channel goroutine (lightweight second WS to hub)
	go runStatusChannel(ctx, wsURL, clawID, clawName, templateName, token)

	for {
		if err := runHubLoop(ctx, wsURL, clawID, clawName, templateName, token, modelAuthToken, gwClient, gwSession, proxy, queue, deduper); err != nil {
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
					pingDeadlineCtx, pingDeadlineCancel := context.WithTimeout(pingCtx, 10*time.Second)
					err := conn.Ping(pingDeadlineCtx)
					pingDeadlineCancel()
					if err != nil {
						log.Printf("[status] ping failed: %v", err)
						// Tear the connection down so the read loop unblocks and
						// reconnects; a pong timeout does not close the conn itself.
						conn.CloseNow()
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

func runHubLoop(ctx context.Context, wsURL, clawID, clawName, templateName, token, modelAuthToken string, gwClient *gatewayClient, gwSession *gatewaySession, proxy *httpProxy, queue *msgQueue, deduper *messageDeduper) error {
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
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()
	defer conn.CloseNow()
	writeHub := func(msg hubMsg) error {
		return writeHubMessage(connCtx, conn, msg)
	}

	// Register with the hub — gateway_ready=false until session is established
	reg := hubMsg{
		Type: "register",
		Payload: mustJSON(map[string]interface{}{
			"claw_id":          clawID,
			"name":             clawName,
			"template":         templateName,
			"token":            token,
			"model_auth_token": modelAuthToken,
			"gateway_ready":    gwSession.IsReady(),
		}),
	}
	if err := writeHub(reg); err != nil {
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
	var registered struct {
		ModelAuthCredential *managedModelAuthCredential `json:"model_auth_credential"`
		ModelAuthError      string                      `json:"model_auth_error"`
	}
	if len(ack.Payload) > 0 && string(ack.Payload) != "null" {
		if err := json.Unmarshal(ack.Payload, &registered); err != nil {
			return fmt.Errorf("parse registered payload: %w", err)
		}
	}
	var lastModelAuthSync atomic.Int64
	if registered.ModelAuthCredential != nil {
		if err := applyManagedGrokOAuthCredential(*registered.ModelAuthCredential); err != nil {
			return fmt.Errorf("apply registered model credential: %w", err)
		}
		lastModelAuthSync.Store(time.Now().UnixNano())
	} else if registered.ModelAuthError != "" {
		log.Printf("[model-auth] %s", registered.ModelAuthError)
	}
	log.Printf("registered with hub as %s", clawID)

	writeActivity := func(activity agentActivity) {
		_ = writeHub(hubMsg{
			Type:    "agent_activity",
			Payload: mustJSON(cleanAgentActivity(activity)),
		})
	}

	// Replay any queued entries that accumulated while we were disconnected.
	// Completed replies/notices are delivered directly; only unprocessed inputs
	// re-run an agent turn.
	deliver := func(role, content string) error {
		return writeHub(hubMsg{
			Type:    "message",
			Payload: mustJSON(map[string]interface{}{"role": role, "content": content}),
		})
	}
	runTurn := func(content string) {
		go func(c string) {
			agentCtx, agentCancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer agentCancel()
			reply, agentErr := gwSession.SendMessage(agentCtx, c, func(chunk string) {
				_ = writeHub(hubMsg{
					Type:    "chunk",
					Payload: mustJSON(map[string]interface{}{"role": "claw", "content": chunk}),
				})
			}, func(activity agentActivity) {
				writeActivity(activity)
			})
			if agentErr != nil {
				reply = fmt.Sprintf("⚠️ error: %v", agentErr)
			}
			if writeErr := deliver("claw", reply); writeErr != nil {
				// Hub connection dropped — queue the completed reply, not the input,
				// so the turn is not re-run (avoids duplicating side effects).
				log.Printf("[bridge] hub write failed, queuing completed reply for redelivery: %v", writeErr)
				queue.pushReply(reply)
			}
		}(content)
	}
	replayQueued(queue, deliver, runTurn)

	// Wire up the HTTP proxy send function for this connection
	proxy.mu.Lock()
	proxy.send = func(msg hubMsg) error {
		return writeHub(msg)
	}
	proxy.mu.Unlock()

	startHubKeepalives(connCtx, func(pingCtx context.Context) error {
		pingCtx, cancel := context.WithTimeout(pingCtx, 10*time.Second)
		defer cancel()
		return conn.Ping(pingCtx)
	}, func() {
		// A failed ping means the connection is dead or half-open. The websocket
		// library does not close the conn on a pong timeout, so tear it down
		// explicitly to unblock the main read loop and trigger reconnection.
		connCancel()
		conn.CloseNow()
	}, func() {
		lastSyncUnixNano := lastModelAuthSync.Load()
		if strings.TrimSpace(os.Getenv("ELASTICCLAW_MODEL_AUTH_PROVIDER")) == "grok" &&
			(lastSyncUnixNano == 0 || time.Since(time.Unix(0, lastSyncUnixNano)) >= managedGrokAuthSyncInterval) {
			if err := writeHub(hubMsg{Type: "model_auth_sync"}); err == nil {
				lastModelAuthSync.Store(time.Now().UnixNano())
			}
		}
		go gwSession.refreshContextUsage(connCtx)
		health := !gatewayProcessExited() && checkGateway(gwClient.addr)
		cu := gwSession.ContextUsage()
		usage := gwSession.Usage()
		restarts := gatewayRestartBase + gatewayRestartCount()
		log.Printf("[heartbeat] sending: gateway_healthy=%v gateway_ready=%v context_usage=%d%% restart_count=%d", health, gwSession.IsReady(), cu, restarts)
		heartbeatPayload := map[string]interface{}{"gateway_healthy": health, "gateway_ready": gwSession.IsReady(), "context_usage": cu, "restart_count": restarts}
		if usage.sessionKey != "" {
			heartbeatPayload["session_key"] = usage.sessionKey
		}
		if usage.inputTokens != nil {
			heartbeatPayload["input_tokens"] = usage.inputTokens
		}
		if usage.outputTokens != nil {
			heartbeatPayload["output_tokens"] = usage.outputTokens
		}
		if usage.totalTokens != nil {
			heartbeatPayload["total_tokens"] = usage.totalTokens
		}
		if usage.estimatedCostUSD != nil {
			heartbeatPayload["estimated_cost_usd"] = usage.estimatedCostUSD
		}
		if usage.model != "" {
			heartbeatPayload["model"] = usage.model
		}
		if usage.modelProvider != "" {
			heartbeatPayload["model_provider"] = usage.modelProvider
		}
		_ = writeHub(hubMsg{Type: "heartbeat", Payload: mustJSON(heartbeatPayload)})
	})

	// Main read loop
	for {
		var msg hubMsg
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return fmt.Errorf("read: %w", err)
		}

		log.Printf("recv type=%s", msg.Type)
		switch msg.Type {
		case "message":
			go func(payload json.RawMessage) {
				var m map[string]interface{}
				if err := json.Unmarshal(payload, &m); err != nil {
					log.Printf("payload unmarshal error: %v", err)
					return
				}
				content, _ := m["content"].(string)
				if content == "" {
					return
				}
				id, _ := m["id"].(string)
				if id == "" {
					log.Printf("[bridge] warning: message without id, dedup disabled for it")
				}
				if deduper.seen(id) {
					log.Printf("[bridge] skipping duplicate message id=%s", id)
					return
				}
				if !gwSession.IsReady() {
					log.Printf("[bridge] gateway not ready, queuing message for later")
					queue.pushInput(content)
					return
				}

				agentCtx, agentCancel := context.WithTimeout(context.Background(), 30*time.Minute)
				defer agentCancel()

				log.Printf("[bridge] → openclaw: %q", content[:min(len(content), 80)])

				reply, agentErr := gwSession.SendMessage(agentCtx, content, func(chunk string) {
					_ = writeHub(hubMsg{
						Type: "chunk",
						Payload: mustJSON(map[string]interface{}{
							"role":    "claw",
							"content": chunk,
						}),
					})
				}, func(activity agentActivity) {
					writeActivity(activity)
				})
				if agentErr != nil {
					log.Printf("[bridge] ✗ agent error: %v", agentErr)
					reply = fmt.Sprintf("⚠️ claw-bridge error: %v", agentErr)
				} else {
					log.Printf("[bridge] ← openclaw: %q", reply[:min(len(reply), 120)])
				}

				if writeErr := deliver("claw", reply); writeErr != nil {
					// Hub connection dropped — queue the completed reply for redelivery,
					// not the input, so the turn is not re-run on reconnect.
					log.Printf("[bridge] hub write failed, queuing completed reply for redelivery: %v", writeErr)
					queue.pushReply(reply)
				}
			}(msg.Payload)

		case "model_auth_credential":
			var credential managedModelAuthCredential
			if err := json.Unmarshal(msg.Payload, &credential); err != nil {
				log.Printf("[model-auth] invalid managed credential: %v", err)
			} else if err := applyManagedGrokOAuthCredential(credential); err != nil {
				log.Printf("[model-auth] apply managed credential: %v", err)
			} else {
				lastModelAuthSync.Store(time.Now().UnixNano())
			}

		case "http_proxy_res":
			var res httpProxyRes
			if err := json.Unmarshal(msg.Payload, &res); err == nil {
				proxy.deliver(res)
			}

		case "file":
			go handleFileMessage(connCtx, conn, msg.Payload)

		case "file_read":
			go handleFileReadMessage(connCtx, conn, msg.Payload)

		case "checkpoint_create":
			// Intentionally uses the signal ctx, not connCtx: the handler reports
			// back over HTTP (never writes to conn), and an in-progress checkpoint
			// upload must survive hub reconnects.
			go handleCheckpointCreate(ctx, msg.Payload)

		case "volume_attach":
			go handleVolumeAttach(connCtx, conn, msg.Payload)

		case "volume_sync":
			go handleVolumeSync(connCtx, conn, msg.Payload)

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
