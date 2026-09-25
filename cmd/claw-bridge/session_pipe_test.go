package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// A pipe exercises the real WebSocket protocol without binding a network port.
type gatewayPipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (l *gatewayPipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *gatewayPipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *gatewayPipeListener) Addr() net.Addr { return &net.TCPAddr{} }

func dialPipeGateway(t *testing.T, handler http.Handler) *websocket.Conn {
	t.Helper()
	client, peer := net.Pipe()
	listener := &gatewayPipeListener{connections: make(chan net.Conn, 1), closed: make(chan struct{})}
	listener.connections <- peer
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	t.Cleanup(func() {
		client.Close()
		peer.Close()
		server.Close()
	})
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil }}
	conn, _, err := websocket.Dial(context.Background(), "ws://gateway.test", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func TestGatewaySessionClearsTurnMessageIDAfterSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn := dialPipeGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		var req gwFrame
		if err := wsjson.Read(ctx, conn, &req); err != nil || req.Method != "sessions.send" {
			t.Errorf("read sessions.send: %s, %v", req.Method, err)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "res", ID: req.ID, OK: true}); err != nil {
			t.Error(err)
			return
		}
		if err := wsjson.Write(ctx, conn, gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]any{
			"stream": "lifecycle", "sessionKey": "session-1", "data": map[string]string{"phase": "end"},
		})}); err != nil {
			t.Error(err)
		}
		<-ctx.Done()
	}))
	gs := &gatewaySession{sessionKey: "session-1", conn: conn, pending: make(map[string]chan gwFrame)}
	go gs.readLoop(ctx)
	if _, err := gs.SendMessage(ctx, "work", "input-1", nil, nil); err != nil {
		t.Fatal(err)
	}
	gs.infMu.Lock()
	defer gs.infMu.Unlock()
	if gs.turnMessageID != "" {
		t.Fatalf("idle reconnect would report stale interrupted_message_id %q", gs.turnMessageID)
	}
}
