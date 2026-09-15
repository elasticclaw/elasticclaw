package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestGatewayReadLoopGenericToolResultIsTerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		// Pinned OpenClaw emits result as the terminal tool-stream event.
		for _, data := range []map[string]interface{}{
			{"phase": "start", "name": "exec", "toolCallId": "generic-call", "args": map[string]string{"command": "printf fixture"}},
			{"phase": "result", "name": "exec", "toolCallId": "generic-call", "result": "fixture output", "exitCode": 0},
		} {
			frame := gwFrame{Type: "event", Event: "agent", Payload: mustJSON(map[string]interface{}{"stream": "tool", "sessionKey": "session-result", "data": data})}
			if err := wsjson.Write(ctx, conn, frame); err != nil {
				t.Error(err)
				return
			}
		}
		<-ctx.Done()
	}))
	defer srv.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	activities := make(chan agentActivity, 2)
	inf := &inFlightState{done: make(chan agentResult, 1), onActivity: func(a agentActivity) { activities <- a }}
	gs := &gatewaySession{conn: conn, pending: make(map[string]chan gwFrame), sessionKey: "session-result", inFlight: inf}
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); gs.readLoop(ctx) }()
	defer func() { cancel(); <-loopDone }()
	var observed []agentActivity
	for len(observed) < 2 {
		select {
		case a := <-activities:
			observed = append(observed, a)
		case <-ctx.Done():
			t.Fatal("missing tool activity")
		}
	}
	start, result := observed[0], observed[1]
	if start.Tool != "exec" || start.Phase != "start" || start.CallID != "generic-call" || start.Result != "" {
		t.Fatalf("invalid start: %+v", start)
	}
	if result.Tool != "exec" || result.Phase != "result" || result.CallID != start.CallID || result.Result != "fixture output" || result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("invalid terminal result: %+v", result)
	}
	inf.mu.Lock()
	defer inf.mu.Unlock()
	if inf.activeTool != nil || len(inf.inFlightCalls) != 0 || inf.modelWaitStartedAt.IsZero() {
		t.Fatal("terminal result left the generic tool active or uncorrelated")
	}
}
