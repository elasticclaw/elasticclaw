package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestBridgeExecRunCommandEmitsCompleted(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-exec", AttemptID: "attempt-exec"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	version := uint64(1)
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID, StateVersion: version}

	cfg := typesv2.ExecRunConfig{Command: "echo hello", Timeout: "5s"}
	payload, _ := json.Marshal(cfg)
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "assign-exec", Kind: typesv2.MessageExecRunAssign, RunID: binding.RunID,
		AttemptID: binding.AttemptID, TaskID: "task-exec", ExpectedStateVersion: &version, Payload: payload}
	receipt, task, err := supervisor.acceptHubEnvelope(context.Background(), binding, envelope)
	if err != nil {
		t.Fatalf("accept envelope: %v", err)
	}
	if receipt.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("disposition = %q", receipt.Disposition)
	}
	if task != nil {
		t.Fatalf("unexpected task = %#v", task)
	}

	var completed *typesv2.ControlEnvelope
	deadline := time.Now().Add(2 * time.Second)
	for completed == nil && time.Now().Before(deadline) {
		ready, err := store.ready(binding, 10)
		if err != nil {
			t.Fatal(err)
		}
		for i := range ready {
			if ready[i].Kind == typesv2.MessageExecRunCompleted {
				completed = &ready[i]
				break
			}
		}
		if completed == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if completed == nil {
		t.Fatal("exec.run.completed event was not queued")
	}
	if completed.TaskID != "task-exec" {
		t.Fatalf("task id = %q", completed.TaskID)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(completed.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result["succeeded"] != true {
		t.Fatalf("succeeded = %v", result["succeeded"])
	}
	if strings.TrimSpace(result["stdout"].(string)) != "hello" {
		t.Fatalf("stdout = %q", result["stdout"])
	}
	if result["exit_code"] != 0.0 {
		t.Fatalf("exit_code = %v", result["exit_code"])
	}
}

func TestBridgeExecRunFailureEmitsFailed(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-exec-fail", AttemptID: "attempt-exec-fail"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	version := uint64(1)
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID, StateVersion: version}

	cfg := typesv2.ExecRunConfig{Command: "exit 7", Timeout: "5s"}
	payload, _ := json.Marshal(cfg)
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "assign-exec-fail", Kind: typesv2.MessageExecRunAssign, RunID: binding.RunID,
		AttemptID: binding.AttemptID, TaskID: "task-exec-fail", ExpectedStateVersion: &version, Payload: payload}
	if _, _, err := supervisor.acceptHubEnvelope(context.Background(), binding, envelope); err != nil {
		t.Fatal(err)
	}

	var failed *typesv2.ControlEnvelope
	deadline := time.Now().Add(2 * time.Second)
	for failed == nil && time.Now().Before(deadline) {
		ready, _ := store.ready(binding, 10)
		for i := range ready {
			if ready[i].Kind == typesv2.MessageExecRunFailed {
				failed = &ready[i]
				break
			}
		}
		if failed == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if failed == nil {
		t.Fatal("exec.run.failed event was not queued")
	}
	var result map[string]interface{}
	if err := json.Unmarshal(failed.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result["succeeded"] != false {
		t.Fatalf("succeeded = %v", result["succeeded"])
	}
	if result["exit_code"] != 7.0 {
		t.Fatalf("exit_code = %v", result["exit_code"])
	}
}

func TestBridgeExecRunTimeoutReportsExitCode124(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-exec-timeout", AttemptID: "attempt-exec-timeout"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	version := uint64(1)
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID, StateVersion: version}

	cfg := typesv2.ExecRunConfig{Command: "sleep 10", Timeout: "100ms"}
	payload, _ := json.Marshal(cfg)
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "assign-exec-timeout", Kind: typesv2.MessageExecRunAssign, RunID: binding.RunID,
		AttemptID: binding.AttemptID, TaskID: "task-exec-timeout", ExpectedStateVersion: &version, Payload: payload}
	if _, _, err := supervisor.acceptHubEnvelope(context.Background(), binding, envelope); err != nil {
		t.Fatal(err)
	}

	var failed *typesv2.ControlEnvelope
	deadline := time.Now().Add(2 * time.Second)
	for failed == nil && time.Now().Before(deadline) {
		ready, _ := store.ready(binding, 10)
		for i := range ready {
			if ready[i].Kind == typesv2.MessageExecRunFailed {
				failed = &ready[i]
				break
			}
		}
		if failed == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if failed == nil {
		t.Fatal("exec.run.failed event was not queued")
	}
	var result map[string]interface{}
	if err := json.Unmarshal(failed.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result["succeeded"] != false {
		t.Fatalf("succeeded = %v", result["succeeded"])
	}
	if result["exit_code"] != 124.0 {
		t.Fatalf("exit_code = %v, want 124", result["exit_code"])
	}
	if !strings.Contains(result["error"].(string), "timed out") {
		t.Fatalf("error = %q, want timeout message", result["error"])
	}
}

func TestBridgeExecRunDuplicateAssignmentIsRejected(t *testing.T) {
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-exec-dup", AttemptID: "attempt-exec-dup"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	version := uint64(1)
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID, StateVersion: version}

	cfg := typesv2.ExecRunConfig{Command: "sleep 0.5", Timeout: "5s"}
	payload, _ := json.Marshal(cfg)
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: "assign-dup", Kind: typesv2.MessageExecRunAssign, RunID: binding.RunID,
		AttemptID: binding.AttemptID, TaskID: "task-dup", ExpectedStateVersion: &version, Payload: payload}
	first, _, err := supervisor.acceptHubEnvelope(context.Background(), binding, envelope)
	if err != nil || first.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("first = %v, %v", first, err)
	}
	duplicate, _, err := supervisor.acceptHubEnvelope(context.Background(), binding, envelope)
	if err != nil || duplicate.Disposition != typesv2.DispositionDuplicate {
		t.Fatalf("duplicate = %v, %v", duplicate, err)
	}
}
