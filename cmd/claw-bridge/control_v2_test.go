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

func TestTaskLifecyclePayloadUsesAgentOwnedNamespace(t *testing.T) {
	payload := taskLifecyclePayload(map[string]interface{}{"gateway_turn_completed": true})
	if payload["task"] == nil || payload["execution"] != nil || len(payload) != 1 {
		t.Fatalf("task lifecycle payload = %#v", payload)
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

func TestTaskCancellationIsRegisteredBeforeExecutionStarts(t *testing.T) {
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token",
		bridgeRegistration(true), openTestBridgeControlStore(t))
	task := typesv2.AgentTask{ID: "task-immediate-cancel", Deadline: time.Now().Add(time.Minute)}
	taskCtx, cancel, activeCancellation := supervisor.prepareTaskExecution(context.Background(), task)
	defer cancel()
	defer supervisor.unregisterTaskCancellation("task-immediate-cancel", activeCancellation)

	// This is the ordering used by startTask: registration occurs on the reader
	// goroutine before execution is launched and before it can read a cancel.
	supervisor.cancelTask("task-immediate-cancel")
	select {
	case <-taskCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("immediate cancellation did not reach the registered task")
	}
}

func TestTerminalTaskClearsBridgeSnapshotIdentity(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-terminal", AttemptID: "attempt-terminal"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID,
		State: "building", StateVersion: 3, CurrentTask: &typesv2.AgentTask{ID: "task-terminal"}}

	supervisor.clearTaskSnapshot(binding, "task-terminal")
	if supervisor.snapshot.CurrentTask != nil {
		t.Fatalf("snapshot current task = %#v", supervisor.snapshot.CurrentTask)
	}
	stored, found, err := store.snapshot(binding)
	if err != nil || !found || stored.CurrentTask != nil {
		t.Fatalf("stored snapshot = %#v, found=%v, err=%v", stored, found, err)
	}
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
	if _, _, err := supervisor.acceptHubEnvelope(context.Background(), binding, envelope); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("stale assignment error = %v", err)
	}
	accepted, err := store.incomingByStatus(binding, "accepted")
	if err != nil || len(accepted) != 0 {
		t.Fatalf("stale assignment was journaled: %#v, err=%v", accepted, err)
	}
}

func journalRunningAssignment(t *testing.T, store *bridgeControlStore, binding workflowControlBinding,
	messageID string, task typesv2.AgentTask) {
	t.Helper()
	payload, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	version := task.StateVersion
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: messageID, Kind: typesv2.MessageAgentTaskAssign, RunID: binding.RunID,
		AttemptID: binding.AttemptID, TaskID: task.ID, ExpectedStateVersion: &version, Payload: payload}
	if duplicate, err := store.recordIncoming(envelope); err != nil || duplicate {
		t.Fatalf("journal assignment duplicate=%v err=%v", duplicate, err)
	}
	if claimed, err := store.setIncomingStatus(messageID, "accepted", "running"); err != nil || !claimed {
		t.Fatalf("mark running claimed=%v err=%v", claimed, err)
	}
}

func awaitOutboxKind(t *testing.T, store *bridgeControlStore, binding workflowControlBinding,
	kind typesv2.ControlMessageKind) typesv2.ControlEnvelope {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ready, err := store.ready(binding, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, envelope := range ready {
			if envelope.Kind == kind {
				return envelope
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("outbox never contained %s", kind)
	return typesv2.ControlEnvelope{}
}

func TestRecoverInterruptedResumesTaskStillCurrentOnHub(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-resume", AttemptID: "attempt-resume"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	task := typesv2.AgentTask{ID: "task-resume", RunID: binding.RunID, AttemptID: binding.AttemptID,
		State: "building", StateVersion: 5, Status: typesv2.AgentTaskRunning, Instructions: "keep reviewing",
		Deadline: time.Now().Add(time.Hour)}
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID,
		State: "building", StateVersion: 5, CurrentTask: &task}
	journalRunningAssignment(t, store, binding, "assign-resume", task)

	supervisor.recoverInterrupted(context.Background(), binding)

	// The task was resumed, not failed as unknown: with no gateway attached the
	// resumed execution fails with the gateway error, never the restart reason.
	envelope := awaitOutboxKind(t, store, binding, typesv2.MessageAgentTaskFailed)
	if envelope.TaskID != task.ID || strings.Contains(string(envelope.Payload), "outcome was unknown") ||
		!strings.Contains(string(envelope.Payload), "gateway is not ready") {
		t.Fatalf("resumed task envelope = %s %s", envelope.TaskID, envelope.Payload)
	}
}

func TestRecoverInterruptedFailsTaskWhenHubMovedOn(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-moved-on", AttemptID: "attempt-moved-on"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID,
		State: "building", StateVersion: 6}
	task := typesv2.AgentTask{ID: "task-orphaned", RunID: binding.RunID, AttemptID: binding.AttemptID,
		State: "building", StateVersion: 5, Status: typesv2.AgentTaskRunning, Instructions: "old work",
		Deadline: time.Now().Add(time.Hour)}
	journalRunningAssignment(t, store, binding, "assign-orphaned", task)

	supervisor.recoverInterrupted(context.Background(), binding)

	envelope := awaitOutboxKind(t, store, binding, typesv2.MessageAgentTaskFailed)
	if envelope.TaskID != task.ID || !strings.Contains(string(envelope.Payload), "outcome was unknown") {
		t.Fatalf("orphaned task envelope = %s %s", envelope.TaskID, envelope.Payload)
	}
}

func TestResumableTaskRejectsTerminalExpiredOrMismatchedTask(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-resumable", AttemptID: "attempt-resumable"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	future := time.Now().Add(time.Hour)

	if _, ok := supervisor.resumableTask(binding, "task-1"); ok {
		t.Fatal("task resumable without a snapshot")
	}
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID}
	if _, ok := supervisor.resumableTask(binding, "task-1"); ok {
		t.Fatal("task resumable without a current task")
	}
	for name, task := range map[string]typesv2.AgentTask{
		"terminal":  {ID: "task-1", Status: typesv2.AgentTaskFailed, Deadline: future},
		"expired":   {ID: "task-1", Status: typesv2.AgentTaskRunning, Deadline: time.Now().Add(-time.Minute)},
		"different": {ID: "task-2", Status: typesv2.AgentTaskRunning, Deadline: future},
	} {
		supervisor.snapshot.CurrentTask = &task
		if _, ok := supervisor.resumableTask(binding, "task-1"); ok {
			t.Fatalf("%s task was resumable", name)
		}
	}
	current := typesv2.AgentTask{ID: "task-1", Status: typesv2.AgentTaskRunning, Deadline: future,
		Instructions: "resume me"}
	supervisor.snapshot.CurrentTask = &current
	resumed, ok := supervisor.resumableTask(binding, "task-1")
	if !ok || resumed.Instructions != "resume me" {
		t.Fatalf("resumable task = %#v, ok=%v", resumed, ok)
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
