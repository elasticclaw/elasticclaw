package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestV1AndV2ConversationMessagesRunConcurrentlyWithoutCrossingControlPlanes(t *testing.T) {
	const (
		v1ClawID = "claw-concurrent-v1"
		v2ClawID = "claw-concurrent-v2"
	)
	cfg := &types.HubConfig{Token: "test-token", ClawToken: "claw-token",
		Factories: []*types.FactoryConfig{{Name: "compat", Template: "elasticclaw", PipelineYAML: `
stages:
  - id: working
    entry: true
  - id: legacy_done
    terminal: true
    triggers:
      - message_contains: "[DONE]"
`}},
	}
	s, db := NewTestServerWithConfig(t, cfg, "", "", "")
	for _, clawID := range []string{v1ClawID, v2ClawID} {
		if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,tags,pipeline_stage,created_at)
			VALUES(?,?,?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", clawID, "elasticclaw",
			"connected", `["factory:compat"]`, "working"); err != nil {
			t.Fatal(err)
		}
	}
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-concurrent-v2", TenantID: "test-tenant-id", InitialClawID: v2ClawID,
		WorkspaceYAML: []byte(workflowV2APIWorkspace), WorkflowYAML: []byte(workflowV2APIWorkflow),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.drainWorkflowV2Effects(context.Background(), "concurrent-v2-worker"); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connect := func(clawID string) *websocket.Conn {
		t.Helper()
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/claw/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := wsjson.Write(ctx, conn, types.WSMessage{Type: "register", Payload: map[string]interface{}{
			"claw_id": clawID, "token": "claw-token", "gateway_ready": true,
			"bridge_version": "test", "protocols": []string{typesv2.ProtocolConversationV1, typesv2.ProtocolControlV2},
		}}); err != nil {
			conn.CloseNow()
			t.Fatal(err)
		}
		var ack types.WSMessage
		if err := wsjson.Read(ctx, conn, &ack); err != nil || ack.Type != "registered" {
			conn.CloseNow()
			t.Fatalf("register %s: ack=%#v err=%v", clawID, ack, err)
		}
		return conn
	}
	v1Conn := connect(v1ClawID)
	defer v1Conn.CloseNow()
	v2Conn := connect(v2ClawID)
	defer v2Conn.CloseNow()
	controlConn, _, err := websocket.Dial(ctx,
		"ws"+strings.TrimPrefix(server.URL, "http")+"/claw/control/ws", &websocket.DialOptions{HTTPHeader: http.Header{
			clawControlTokenHeader: {"claw-token"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer controlConn.CloseNow()
	registration := typesv2.ControlRegistration{Token: "claw-token", ClawID: v2ClawID,
		RunID: run.ID, AttemptID: run.CurrentAttemptID,
		Bridge: typesv2.BridgeRegistration{BridgeVersion: "test", Protocols: []string{typesv2.ProtocolControlV2}}}
	if err := wsjson.Write(ctx, controlConn,
		typesv2.ControlFrame{Type: typesv2.ControlFrameRegister, Registration: &registration}); err != nil {
		t.Fatal(err)
	}
	var registered, assignment typesv2.ControlFrame
	if err := wsjson.Read(ctx, controlConn, &registered); err != nil || registered.Type != typesv2.ControlFrameRegistered {
		t.Fatalf("control registration = %#v, err=%v", registered, err)
	}
	if err := wsjson.Read(ctx, controlConn, &assignment); err != nil || assignment.Envelope == nil ||
		assignment.Envelope.Kind != typesv2.MessageAgentTaskAssign {
		t.Fatalf("task assignment = %#v, err=%v", assignment, err)
	}
	if err := wsjson.Write(ctx, controlConn, typesv2.ControlFrame{Type: typesv2.ControlFrameReceipt,
		Receipt: &typesv2.ControlReceipt{MessageID: assignment.Envelope.MessageID,
			Disposition: typesv2.DispositionAccepted, StateVersion: 1}}); err != nil {
		t.Fatal(err)
	}
	stateVersion := uint64(1)
	started := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "concurrent-v2-started", Kind: typesv2.MessageAgentTaskStarted,
		RunID: run.ID, AttemptID: run.CurrentAttemptID, TaskID: assignment.Envelope.TaskID,
		ExpectedStateVersion: &stateVersion, Payload: []byte(`{"task":{"started":true}}`)}
	if err := wsjson.Write(ctx, controlConn,
		typesv2.ControlFrame{Type: typesv2.ControlFrameEnvelope, Envelope: &started}); err != nil {
		t.Fatal(err)
	}
	var startedReceipt typesv2.ControlFrame
	if err := wsjson.Read(ctx, controlConn, &startedReceipt); err != nil || startedReceipt.Receipt == nil ||
		startedReceipt.Receipt.Disposition != typesv2.DispositionAccepted {
		if startedReceipt.Receipt != nil {
			t.Fatalf("task-start receipt = %+v, err=%v", *startedReceipt.Receipt, err)
		}
		t.Fatalf("task-start frame = %#v, err=%v", startedReceipt, err)
	}

	turn := types.WSMessage{Type: "message", Payload: types.HubMessage{
		Content: "[DONE] https://github.com/org/repo/pull/42",
	}}
	completed := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "concurrent-v2-completed", Kind: typesv2.MessageAgentTaskCompleted,
		RunID: run.ID, AttemptID: run.CurrentAttemptID, TaskID: assignment.Envelope.TaskID,
		ExpectedStateVersion: &stateVersion, Payload: []byte(`{"task":{"result":"success"}}`)}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, conn := range []*websocket.Conn{v1Conn, v2Conn} {
		wg.Add(1)
		go func(conn *websocket.Conn) {
			defer wg.Done()
			errs <- wsjson.Write(ctx, conn, turn)
		}(conn)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- wsjson.Write(ctx, controlConn,
			typesv2.ControlFrame{Type: typesv2.ControlFrameEnvelope, Envelope: &completed})
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var completedReceipt typesv2.ControlFrame
	if err := wsjson.Read(ctx, controlConn, &completedReceipt); err != nil || completedReceipt.Receipt == nil ||
		completedReceipt.Receipt.Disposition != typesv2.DispositionAccepted || completedReceipt.Receipt.StateVersion != 2 {
		t.Fatalf("task-completion receipt = %#v, err=%v", completedReceipt, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	var v1Stage, v2Stage string
	var persisted int
	for time.Now().Before(deadline) {
		_ = db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, v1ClawID).Scan(&v1Stage)
		_ = db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, v2ClawID).Scan(&v2Stage)
		_ = db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id IN (?,?) AND role='claw'`,
			v1ClawID, v2ClawID).Scan(&persisted)
		if v1Stage == "legacy_done" && persisted == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v1Stage != "legacy_done" {
		t.Fatalf("V1 transcript trigger did not advance: stage=%q", v1Stage)
	}
	if v2Stage != "working" {
		t.Fatalf("V2 transcript crossed into the legacy pipeline: stage=%q", v2Stage)
	}
	if persisted != 2 {
		t.Fatalf("persisted conversation rows = %d, want 2", persisted)
	}
	var v2State string
	var v2Version uint64
	var v2Events, v2LegacyPRs, v2InjectedMessages int
	if err := db.QueryRow(`SELECT state,state_version FROM workflow_v2_runs WHERE id=?`, run.ID).
		Scan(&v2State, &v2Version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=?`, run.ID).Scan(&v2Events); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, v2ClawID).Scan(&v2LegacyPRs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role!='claw'`, v2ClawID).
		Scan(&v2InjectedMessages); err != nil {
		t.Fatal(err)
	}
	if v2State != "done" || v2Version != 2 || v2Events != 3 || v2LegacyPRs != 0 || v2InjectedMessages != 0 {
		t.Fatalf("V2 state/version/events/legacy_prs/injected = %q/%d/%d/%d/%d",
			v2State, v2Version, v2Events, v2LegacyPRs, v2InjectedMessages)
	}
}
