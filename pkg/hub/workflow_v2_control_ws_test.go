package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func createWorkflowV2ControlServer(t *testing.T) (*Server, *workflowv2.Store, workflowv2.Attempt, *httptest.Server) {
	t.Helper()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", ClawToken: "claw-token"}, "", "", "")
	store := workflowv2.NewStore(db)
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-control-ws", TenantID: "test-tenant-id",
		WorkspaceYAML: []byte(workflowV2APIWorkspace), WorkflowYAML: []byte(workflowV2APIWorkflow),
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(context.Background(), "run-control-ws", "claw-control-ws")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(s.Handler())
	t.Cleanup(httpServer.Close)
	return s, store, attempt, httpServer
}

func TestWorkflowV2ControlWebSocketAuthenticatesBindsSnapshotsAndDeduplicates(t *testing.T) {
	_, store, attempt, server := createWorkflowV2ControlServer(t)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/claw/control/ws"

	unauthorized, response, err := websocket.Dial(context.Background(), wsURL, nil)
	if unauthorized != nil {
		unauthorized.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized dial response/error = %#v/%v", response, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{
		clawControlTokenHeader: {"claw-token"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	registration := typesv2.ControlRegistration{Token: "claw-token", ClawID: "claw-control-ws",
		RunID: "run-control-ws", AttemptID: attempt.ID,
		Bridge: typesv2.BridgeRegistration{BridgeVersion: "test", Protocols: []string{typesv2.ProtocolControlV2}}}
	if err := wsjson.Write(ctx, conn, typesv2.ControlFrame{Type: typesv2.ControlFrameRegister, Registration: &registration}); err != nil {
		t.Fatal(err)
	}
	var registered typesv2.ControlFrame
	if err := wsjson.Read(ctx, conn, &registered); err != nil {
		t.Fatal(err)
	}
	if registered.Type != typesv2.ControlFrameRegistered || registered.Snapshot == nil ||
		registered.Snapshot.RunID != "run-control-ws" || registered.Snapshot.AttemptID != attempt.ID {
		t.Fatalf("registered frame = %#v", registered)
	}

	syncEnvelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "sync-ws-1", Kind: typesv2.MessageWorkflowSync, RunID: "run-control-ws", AttemptID: attempt.ID}
	if err := store.EnqueueControl(ctx, syncEnvelope); err != nil {
		t.Fatal(err)
	}
	var delivered typesv2.ControlFrame
	if err := wsjson.Read(ctx, conn, &delivered); err != nil {
		t.Fatal(err)
	}
	if delivered.Type != typesv2.ControlFrameEnvelope || delivered.Envelope == nil || delivered.Envelope.MessageID != syncEnvelope.MessageID {
		t.Fatalf("delivered frame = %#v", delivered)
	}
	receipt := typesv2.ControlReceipt{MessageID: syncEnvelope.MessageID,
		Disposition: typesv2.DispositionAccepted, StateVersion: registered.Snapshot.StateVersion}
	if err := wsjson.Write(ctx, conn, typesv2.ControlFrame{Type: typesv2.ControlFrameReceipt, Receipt: &receipt}); err != nil {
		t.Fatal(err)
	}

	helpEnvelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "help-ws-1", Kind: typesv2.MessageHelpRequested, RunID: "run-control-ws", AttemptID: attempt.ID,
		Payload: []byte(`{"help":{"reason":"blocked"}}`)}
	for i, want := range []typesv2.ControlDisposition{typesv2.DispositionAccepted, typesv2.DispositionDuplicate} {
		if err := wsjson.Write(ctx, conn, typesv2.ControlFrame{Type: typesv2.ControlFrameEnvelope, Envelope: &helpEnvelope}); err != nil {
			t.Fatal(err)
		}
		var responseFrame typesv2.ControlFrame
		if err := wsjson.Read(ctx, conn, &responseFrame); err != nil {
			t.Fatal(err)
		}
		if responseFrame.Type != typesv2.ControlFrameReceipt || responseFrame.Receipt == nil || responseFrame.Receipt.Disposition != want {
			t.Fatalf("receipt %d = %#v, want %s", i, responseFrame, want)
		}
	}

	malformed := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "malformed-ws-1", Kind: typesv2.MessagePlanSubmitted,
		RunID: "run-control-ws", AttemptID: attempt.ID, Payload: []byte(`{"plan":{"summary":"missing CAS"}}`)}
	for i, want := range []typesv2.ControlDisposition{typesv2.DispositionRejected, typesv2.DispositionDuplicate} {
		if err := wsjson.Write(ctx, conn, typesv2.ControlFrame{Type: typesv2.ControlFrameEnvelope, Envelope: &malformed}); err != nil {
			t.Fatal(err)
		}
		var responseFrame typesv2.ControlFrame
		if err := wsjson.Read(ctx, conn, &responseFrame); err != nil {
			t.Fatal(err)
		}
		if responseFrame.Receipt == nil || responseFrame.Receipt.Disposition != want {
			t.Fatalf("malformed receipt %d = %#v, want %s", i, responseFrame, want)
		}
	}
	inspection, err := store.InspectRun(ctx, "run-control-ws")
	if err != nil {
		t.Fatal(err)
	}
	foundRejected := false
	for _, event := range inspection.RecentEvents {
		if event.ID == malformed.MessageID && event.Disposition == typesv2.DispositionRejected {
			foundRejected = true
		}
	}
	if !foundRejected {
		t.Fatalf("durable rejected event missing: %#v", inspection.RecentEvents)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		ready, err := store.ReadyControl(ctx, "run-control-ws", attempt.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(ready) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("acknowledged outbox remained ready: %#v", ready)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMainClawWebSocketNegotiatesWorkflowV2ControlWithoutChangingV1Registration(t *testing.T) {
	_, _, attempt, server := createWorkflowV2ControlServer(t)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/claw/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	oldConn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Write(ctx, oldConn, types.WSMessage{Type: "register", Payload: map[string]interface{}{
		"claw_id": "claw-control-ws", "token": "claw-token", "gateway_ready": true,
	}}); err != nil {
		t.Fatal(err)
	}
	var oldAck types.WSMessage
	if err := wsjson.Read(ctx, oldConn, &oldAck); err == nil {
		t.Fatal("old bridge was accepted for an active workflow v2 attempt")
	}
	oldConn.CloseNow()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, types.WSMessage{Type: "register", Payload: map[string]interface{}{
		"claw_id": "claw-control-ws", "token": "claw-token", "gateway_ready": true,
		"bridge_version": "test", "protocols": []string{typesv2.ProtocolConversationV1, typesv2.ProtocolControlV2},
	}}); err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Type    string `json:"type"`
		Payload struct {
			WorkflowControl *workflowV2ControlBinding `json:"workflow_control"`
		} `json:"payload"`
	}
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Type != "registered" || ack.Payload.WorkflowControl == nil ||
		ack.Payload.WorkflowControl.RunID != "run-control-ws" || ack.Payload.WorkflowControl.AttemptID != attempt.ID {
		t.Fatalf("registration ack = %#v", ack)
	}
}

func TestNewBridgeStillUsesConversationPathForV1(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", ClawToken: "claw-token"}, "", "", "")
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/claw/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := wsjson.Write(ctx, conn, types.WSMessage{Type: "register", Payload: map[string]interface{}{
		"claw_id": "claw-v1-new-bridge", "token": "claw-token", "gateway_ready": true,
		"bridge_version": "test", "protocols": []string{typesv2.ProtocolConversationV1, typesv2.ProtocolControlV2},
	}}); err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Type    string `json:"type"`
		Payload struct {
			ClawID          string                    `json:"claw_id"`
			WorkflowControl *workflowV2ControlBinding `json:"workflow_control"`
		} `json:"payload"`
	}
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Type != "registered" || ack.Payload.ClawID != "claw-v1-new-bridge" || ack.Payload.WorkflowControl != nil {
		t.Fatalf("v1 registration ack = %#v", ack)
	}
}
