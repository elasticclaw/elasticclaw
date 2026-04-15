package hub

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"nhooyr.io/websocket"
)

// connectRelay connects the hub outbound to a relay server and pipes frames
// to the hub's local WebSocket handler. Reconnects automatically on disconnect.
//
// The relay URL should be: wss://relay.example.com
// The hub connects as: wss://relay.example.com/hub?id=<hubID>&token=<token>
// Bridges connect as:   wss://relay.example.com/bridge?id=<hubID>&token=<token>
func (s *Server) connectRelay(ctx context.Context, relayURL, hubID, token string) {
	relayURL = strings.TrimRight(relayURL, "/")
	endpoint := fmt.Sprintf("%s/hub?id=%s&token=%s", relayURL, hubID, token)

	for {
		if ctx.Err() != nil {
			return
		}
		log.Printf("[relay] connecting to %s (id=%s)", relayURL, hubID[:8])
		if err := s.relayLoop(ctx, endpoint, hubID); err != nil {
			log.Printf("[relay] disconnected: %v — reconnecting in 5s", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (s *Server) relayLoop(ctx context.Context, endpoint, hubID string) error {
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": {"elasticclaw-hub/1.0"}},
	})
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	defer conn.CloseNow()

	log.Printf("[relay] connected (id=%s)", hubID[:8])

	// Pipe frames between the relay connection and the hub's local /claw/ws handler.
	// We spin up a local WebSocket connection to ourselves and bridge the two.
	localURL := fmt.Sprintf("ws://localhost%s/claw/ws", s.localAddr())
	localConn, _, err := websocket.Dial(ctx, localURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": {"relay-bridge/1.0"}},
	})
	if err != nil {
		return fmt.Errorf("dial local hub: %w", err)
	}
	defer localConn.CloseNow()

	done := make(chan struct{}, 2)
	pipe := func(src, dst *websocket.Conn) {
		defer func() { done <- struct{}{} }()
		for {
			msgType, r, err := src.Reader(ctx)
			if err != nil {
				return
			}
			w, err := dst.Writer(ctx, msgType)
			if err != nil {
				return
			}
			if _, err := io.Copy(w, r); err != nil {
				w.Close()
				return
			}
			w.Close()
		}
	}
	go pipe(conn, localConn)
	go pipe(localConn, conn)

	// Keepalive: ping the relay every 30s to prevent idle timeouts
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.Ping(ctx); err != nil {
					return
				}
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()

	<-done
	return fmt.Errorf("pipe closed")
}

// localAddr returns the local listen address of the hub (e.g. ":8080").
func (s *Server) localAddr() string {
	return s.addr
}

// RelayToken derives the relay token for a given hub ID.
// If secret is empty, the claw token is used directly.
func RelayToken(secret, hubID, clawToken string) string {
	if secret == "" {
		return clawToken
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(hubID))
	return hex.EncodeToString(mac.Sum(nil))
}

// HubID derives a stable hub ID from the hub's identity key (first 32 hex chars of the public key fingerprint).
func HubID(publicKeyBase64 string) string {
	h := sha256.Sum256([]byte(publicKeyBase64))
	return hex.EncodeToString(h[:16]) // 32 hex chars
}
