package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/harness/openclaw"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func TestTurnErrorPreservesRecovery(t *testing.T) {
	for _, original := range []error{
		context.DeadlineExceeded,
		&sessionPreservedError{err: context.DeadlineExceeded, key: "session-1", reason: types.SessionLossReasonTurnTimeout},
		&sessionRotatedError{err: context.DeadlineExceeded, previousKey: "session-0", reason: types.SessionLossReasonGatewayReconnect},
	} {
		wrapped := &TurnError{Class: types.TurnErrorFatal, Err: original}
		if wrapped.Error() != original.Error() || !errors.Is(wrapped, context.DeadlineExceeded) || !errors.Is(wrapped, original) {
			t.Fatalf("wrapper changed error identity or text: %v", wrapped)
		}
		gs := &gatewaySession{sessionKey: "session-1"}
		if sessionRecoveryOutcome(wrapped, gs.SessionKey()) != sessionRecoveryOutcome(original, gs.SessionKey()) || sessionLossReason(wrapped) != sessionLossReason(original) {
			t.Fatalf("wrapper changed recovery: %v", wrapped)
		}
		report := func(err error) (bool, string, []hubMsg, []agentActivity) {
			reply := "partial"
			var messages []hubMsg
			var activities []agentActivity
			handled := reportSessionRecovery(err, "turn-1", &reply, gs, func(a agentActivity) { activities = append(activities, a) }, func(m hubMsg) error { messages = append(messages, m); return nil }, &msgQueue{})
			return handled, reply, messages, activities
		}
		h1, r1, m1, a1 := report(original)
		h2, r2, m2, a2 := report(wrapped)
		if h1 != h2 || r1 != r2 || !reflect.DeepEqual(m1, m2) || !reflect.DeepEqual(a1, a2) {
			t.Fatalf("wrapper changed recovery reporting: %v", wrapped)
		}
	}
}

type errorReplyHarness struct {
	*gatewaySession
	err error
}

func (h *errorReplyHarness) RunTurn(context.Context, string, string, EventSink) (string, error) {
	return "completed", h.err
}

func TestRunHubLoopErrorReplyClass(t *testing.T) {
	for _, replay := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			queue := &msgQueue{}
			if replay {
				queue.pushInput("turn-1", "continue")
			}
			received := make(chan hubMsg, 1)
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				var msg hubMsg
				if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
					t.Error(err)
					return
				}
				if err := wsjson.Write(r.Context(), conn, hubMsg{Type: "registered"}); err != nil {
					t.Error(err)
					return
				}
				if !replay {
					if err := wsjson.Write(r.Context(), conn, hubMsg{Type: "message", Payload: mustJSON(map[string]string{"id": "turn-1", "content": "continue"})}); err != nil {
						t.Error(err)
						return
					}
				}
				for {
					if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
						return
					}
					if msg.Type == "message" {
						received <- msg
						return
					}
				}
			}))
			h := &errorReplyHarness{gatewaySession: &gatewaySession{ready: true, sessionKey: "session-1"}}
			if failed {
				h.err = errors.New("gateway disconnected")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			done := make(chan error, 1)
			go func() {
				done <- runHubLoop(ctx, "ws"+strings.TrimPrefix(hub.URL, "http"), "claw-test", "test", "test", "tok", "", bridgeRegistration(false), nil, &gatewayClient{}, h, newHTTPProxy(nil), queue, newMessageDeduper())
			}()
			select {
			case msg := <-received:
				var payload map[string]string
				if err := json.Unmarshal(msg.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				want := map[string]string{"role": "claw", "content": "completed"}
				if failed {
					prefix := types.BridgeErrorPrefix
					if replay {
						prefix = types.BridgeReplayErrorPrefix
					}
					want["content"] = prefix + " gateway disconnected"
					want["error_class"] = string(types.TurnErrorHarnessConnectivity)
				}
				if !reflect.DeepEqual(payload, want) {
					t.Errorf("replay=%v failed=%v: payload=%v, want %v", replay, failed, payload, want)
				}
			case <-ctx.Done():
				t.Error("no turn reply")
			}
			cancel()
			<-done
			hub.Close()
		}
	}
}

func TestQueuedErrorReplyPreservesClass(t *testing.T) {
	q := &msgQueue{}
	const reply = types.BridgeErrorPrefix + " gateway disconnected"
	q.pushErrorReply(reply, types.TurnErrorHarnessConnectivity)
	ordinary := func(string, string) error { t.Fatal("error reply used untyped delivery"); return nil }
	runTurn := func(string, string) { t.Fatal("completed reply reran the turn") }
	replayQueued(q, ordinary, func(hubMsg) error { return errors.New("hub disconnected") }, runTurn)
	var got hubMsg
	replayQueued(q, ordinary, func(msg hubMsg) error { got = msg; return nil }, runTurn)
	var payload types.HubMessage
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if got.Type != "message" || payload.Role != "claw" || payload.Content != reply || payload.ErrorClass != types.TurnErrorHarnessConnectivity {
		t.Fatalf("replayed reply = %+v, %s", got, got.Payload)
	}
	if len(q.drain()) != 0 {
		t.Fatal("delivered reply remained queued")
	}
}

func TestHarnessRunTurnWrapsError(t *testing.T) {
	const cause = "malformed json arguments"
	gs := startLifecycleEndGateway(t, map[string]interface{}{"phase": "error", "error": cause})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := gs.RunTurn(ctx, "continue", "turn-1", EventSink{OnChunk: func(string) {}, OnActivity: func(agentActivity) {}})
	var turnErr *TurnError
	if !errors.As(err, &turnErr) || turnErr.Class != types.TurnErrorTransient || turnErr.Error() != cause {
		t.Fatalf("RunTurn error = %#v, want transient with original text", err)
	}
}

func TestTurnErrorClassPrefersHarnessClass(t *testing.T) {
	limit := errors.New("You have hit your usage limit. Try again at 5:00 PM.")
	for _, tc := range []struct {
		name     string
		err      error
		definite bool
		want     types.TurnErrorClass
	}{
		{"harness class wins over text", &TurnError{Class: types.TurnErrorSessionLost, Err: errors.New("gateway disconnected")}, true, types.TurnErrorSessionLost},
		{"harness class kept on replay", &TurnError{Class: types.TurnErrorTransient, Err: errors.New("boom")}, false, types.TurnErrorTransient},
		{"quota is definite only", &TurnError{Class: types.TurnErrorQuota, Err: limit}, false, openclaw.ClassifyErrorText(limit.Error(), false)},
		{"quota kept when definite", &TurnError{Class: types.TurnErrorQuota, Err: limit}, true, types.TurnErrorQuota},
		{"plain error falls back to text", errors.New("gateway disconnected"), true, openclaw.ClassifyErrorText("gateway disconnected", true)},
		{"empty class falls back to text", &TurnError{Err: errors.New("gateway disconnected")}, false, openclaw.ClassifyErrorText("gateway disconnected", false)},
	} {
		if got := turnErrorClass(tc.err, tc.definite); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
