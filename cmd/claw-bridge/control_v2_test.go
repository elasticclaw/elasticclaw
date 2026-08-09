package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func openTestBridgeControlStore(t *testing.T) *bridgeControlStore {
	t.Helper()
	store, err := openBridgeControlStore(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store
}

func TestBridgeControlStoreDurablyDeduplicatesInboxAndReplaysOutbox(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-1", AttemptID: "attempt-1"}
	version := uint64(3)
	incoming := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "assign-1", Kind: typesv2.MessageAgentTaskAssign, RunID: binding.RunID,
		AttemptID: binding.AttemptID, TaskID: "task-1", ExpectedStateVersion: &version}
	duplicate, err := store.recordIncoming(incoming)
	if err != nil || duplicate {
		t.Fatalf("first incoming duplicate=%v err=%v", duplicate, err)
	}
	duplicate, err = store.recordIncoming(incoming)
	if err != nil || !duplicate {
		t.Fatalf("second incoming duplicate=%v err=%v", duplicate, err)
	}

	outgoing := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "plan-1", Kind: typesv2.MessagePlanSubmitted, RunID: binding.RunID,
		AttemptID: binding.AttemptID, ExpectedStateVersion: &version, Payload: []byte(`{"plan":{"summary":"safe"}}`)}
	if err := store.enqueue(outgoing); err != nil {
		t.Fatal(err)
	}
	ready, err := store.ready(binding, 10)
	if err != nil || len(ready) != 1 || ready[0].MessageID != outgoing.MessageID {
		t.Fatalf("ready = %#v, err=%v", ready, err)
	}
	if err := store.markSent(outgoing.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := store.acknowledge(typesv2.ControlReceipt{MessageID: outgoing.MessageID,
		Disposition: typesv2.DispositionAccepted, StateVersion: version}); err != nil {
		t.Fatal(err)
	}
	ready, err = store.ready(binding, 10)
	if err != nil || len(ready) != 0 {
		t.Fatalf("ready after receipt = %#v, err=%v", ready, err)
	}
}

func TestLocalControlEndpointBuildsBoundTypedEnvelope(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-local", AttemptID: "attempt-local"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw-local", "token",
		bridgeRegistration(true), store)
	supervisor.binding = &binding
	// A disconnected control socket still journals commands and returns pending
	// immediately; connected commands are covered by TestLocalControlEndpointWaitsForReceipt.
	supervisor.connected = false
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID,
		State: "planning", StateVersion: 7, CurrentTask: &typesv2.AgentTask{ID: "task-local"}}

	req := httptest.NewRequest(http.MethodPost, localControlPath,
		bytes.NewBufferString(`{"kind":"plan.submitted","payload":{"plan":{"summary":"ready"}}}`))
	recorder := httptest.NewRecorder()
	supervisor.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	ready, err := store.ready(binding, 10)
	if err != nil || len(ready) != 1 {
		t.Fatalf("ready = %#v, err=%v", ready, err)
	}
	if ready[0].RunID != binding.RunID || ready[0].AttemptID != binding.AttemptID ||
		ready[0].TaskID != "task-local" || ready[0].ExpectedStateVersion == nil || *ready[0].ExpectedStateVersion != 7 {
		t.Fatalf("envelope = %#v", ready[0])
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(ready[0].Payload, &payload); err != nil || payload["plan"] == nil {
		t.Fatalf("payload = %#v, err=%v", payload, err)
	}

	bad := httptest.NewRequest(http.MethodPost, localControlPath,
		bytes.NewBufferString(`{"kind":"conversation.message","payload":{}}`))
	badRecorder := httptest.NewRecorder()
	supervisor.ServeHTTP(badRecorder, bad)
	if badRecorder.Code != http.StatusBadRequest {
		t.Fatalf("conversation control status = %d, body = %s", badRecorder.Code, badRecorder.Body.String())
	}
}

func TestLocalControlEndpointWaitsForReceipt(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-receipt", AttemptID: "attempt-receipt"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	supervisor.connected = true
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID, StateVersion: 2}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		supervisor.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, localControlPath,
			bytes.NewBufferString(`{"kind":"plan.submitted","payload":{"plan":{"summary":"ready"}}}`)))
		done <- recorder
	}()
	deadline := time.Now().Add(time.Second)
	var messageID string
	for messageID == "" && time.Now().Before(deadline) {
		ready, err := store.ready(binding, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(ready) > 0 {
			messageID = ready[0].MessageID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if messageID == "" {
		t.Fatal("local command was not queued")
	}
	if err := store.acknowledge(typesv2.ControlReceipt{MessageID: messageID,
		Disposition: typesv2.DispositionAccepted, StateVersion: 3}); err != nil {
		t.Fatal(err)
	}
	select {
	case recorder := <-done:
		if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), string(typesv2.DispositionAccepted)) {
			t.Fatalf("status/body = %d/%s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("local control request did not return after receipt")
	}
}

func TestBridgeRegistrationOnlyAdvertisesControlWhenDurableStoreIsAvailable(t *testing.T) {
	legacy := bridgeRegistration(false)
	if !legacy.SupportsProtocol(typesv2.ProtocolConversationV1) || legacy.SupportsProtocol(typesv2.ProtocolControlV2) {
		t.Fatalf("legacy registration = %#v", legacy)
	}
	control := bridgeRegistration(true)
	if !control.SupportsProtocol(typesv2.ProtocolConversationV1) || !control.SupportsProtocol(typesv2.ProtocolControlV2) {
		t.Fatalf("control registration = %#v", control)
	}
}

func TestOldTaskCleanupCannotRemoveReplacementCancellation(t *testing.T) {
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token",
		bridgeRegistration(true), openTestBridgeControlStore(t))
	oldCancelled := make(chan struct{})
	oldCancellation := supervisor.registerTaskCancellation("task-reused", func() { close(oldCancelled) })
	newCancelled := make(chan struct{})
	newCancellation := supervisor.registerTaskCancellation("task-reused", func() { close(newCancelled) })

	supervisor.unregisterTaskCancellation("task-reused", oldCancellation)
	supervisor.cancelTask("task-reused")

	select {
	case <-newCancelled:
	default:
		t.Fatal("replacement task cancellation was removed by old task cleanup")
	}
	select {
	case <-oldCancelled:
		t.Fatal("cancelling the replacement task cancelled the old task")
	default:
	}
	supervisor.unregisterTaskCancellation("task-reused", newCancellation)
}

func TestBridgeRejectsTaskAssignmentOlderThanAuthoritativeSnapshot(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-stale-task", AttemptID: "attempt-stale-task"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID, StateVersion: 7}
	task := typesv2.AgentTask{ID: "task-stale", RunID: binding.RunID, AttemptID: binding.AttemptID,
		State: "building", StateVersion: 6, Status: typesv2.AgentTaskAssigned, Instructions: "obsolete work"}
	payload, _ := json.Marshal(task)
	version := uint64(6)
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "assign-stale", Kind: typesv2.MessageAgentTaskAssign, RunID: binding.RunID,
		AttemptID: binding.AttemptID, TaskID: task.ID, ExpectedStateVersion: &version, Payload: payload}
	if _, _, err := supervisor.acceptHubEnvelope(binding, envelope); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("stale assignment error = %v", err)
	}
	accepted, err := store.incomingByStatus(binding, "accepted")
	if err != nil || len(accepted) != 0 {
		t.Fatalf("stale assignment was journaled: %#v, err=%v", accepted, err)
	}
}

func TestWorkflowV2TaskPromptUsesTypedToolAndNotTranscriptMarkers(t *testing.T) {
	prompt := workflowV2TaskPrompt(typesv2.AgentTask{ID: "task-1", State: "building", Instructions: "Implement it."})
	if !strings.Contains(prompt, "claw-bridge control") || !strings.Contains(prompt, "never through phrases") {
		t.Fatalf("prompt = %q", prompt)
	}
	for _, marker := range []string{"[DONE]", "[READY_TO_COMMIT]"} {
		if strings.Contains(prompt, marker) {
			t.Fatalf("prompt contains transcript marker %q", marker)
		}
	}
}

func TestBridgeControlSnapshotPersistsAcrossProcessRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sqlite")
	first, err := openBridgeControlStore(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := typesv2.WorkflowSnapshot{RunID: "run-restart", AttemptID: "attempt-restart",
		State: "building", StateVersion: 11}
	if err := first.saveSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	second, err := openBridgeControlStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	loaded, found, err := second.snapshot(workflowControlBinding{RunID: snapshot.RunID, AttemptID: snapshot.AttemptID})
	if err != nil || !found || loaded.StateVersion != snapshot.StateVersion || loaded.State != snapshot.State {
		t.Fatalf("loaded = %#v, found=%v, err=%v", loaded, found, err)
	}
}

func TestBridgeControlOutboxRetryUsesStableMessageIdentity(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-retry", AttemptID: "attempt-retry"}
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "stable-message", Kind: typesv2.MessageHelpRequested,
		RunID: binding.RunID, AttemptID: binding.AttemptID, SentAt: time.Now().UTC()}
	if err := store.enqueue(envelope); err != nil {
		t.Fatal(err)
	}
	if err := store.enqueue(envelope); err != nil {
		t.Fatal(err)
	}
	ready, err := store.ready(binding, 10)
	if err != nil || len(ready) != 1 || ready[0].MessageID != envelope.MessageID {
		t.Fatalf("ready = %#v, err=%v", ready, err)
	}
}
