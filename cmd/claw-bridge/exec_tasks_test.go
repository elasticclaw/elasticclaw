package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// setBridgeTestWorkspaceHome points HOME at a temp dir with a live
// ~/.openclaw/workspace so wrapped exec commands can cd into it.
func setBridgeTestWorkspaceHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".openclaw", "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestBridgeExecRunCommandEmitsCompleted(t *testing.T) {
	setBridgeTestWorkspaceHome(t)
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
	setBridgeTestWorkspaceHome(t)
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
	setBridgeTestWorkspaceHome(t)
	store := openTestBridgeControlStore(t)
	binding := workflowControlBinding{RunID: "run-exec-timeout", AttemptID: "attempt-exec-timeout"}
	supervisor := newControlSupervisor(context.Background(), "ws://invalid", "claw", "token", bridgeRegistration(true), store)
	supervisor.binding = &binding
	version := uint64(1)
	supervisor.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID, StateVersion: version}

	// Use a 1ns timeout so the context is already cancelled before the child
	// process can start. This avoids depending on the CI runner's ability to
	// reap a long-running process, while still exercising the timeout branch.
	cfg := typesv2.ExecRunConfig{Command: "sleep 10", Timeout: "1ns"}
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
	setBridgeTestWorkspaceHome(t)
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

func TestTruncateOutputPreservesUTF8(t *testing.T) {
	// 100 emoji (multi-byte runes) followed by ASCII marker.
	value := strings.Repeat("🚀", 100) + "END"
	out := truncateOutput(value, 20)
	if !strings.Contains(out, "[truncated]") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("truncated output is not valid UTF-8: %q", out)
	}
	if utf8.RuneCountInString(out) >= utf8.RuneCountInString(value) {
		t.Fatalf("truncated output should be shorter than original")
	}
}

func TestBridgeWorkspaceCommandRunsInLiveWorkspace(t *testing.T) {
	setBridgeTestWorkspaceHome(t)

	got := bridgeWorkspaceCommand(`bash scripts/prepare-deps-branch.sh`)
	want := `cd "$HOME/.openclaw/workspace" && bash scripts/prepare-deps-branch.sh`
	if got != want {
		t.Fatalf("command without flake = %q, want %q", got, want)
	}
	if strings.Contains(got, "flake-run") {
		t.Fatalf("non-flake workspace must not wrap in flake-run: %q", got)
	}
}

func TestBridgeWorkspaceCommandWrapsFlakeWorkspaces(t *testing.T) {
	home := setBridgeTestWorkspaceHome(t)
	if err := os.WriteFile(filepath.Join(home, ".openclaw", "workspace", "flake.nix"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}

	got := bridgeWorkspaceCommand("go test ./...")
	if !strings.HasPrefix(got, `~/.elasticclaw/flake-run bash -lc '`) {
		t.Fatalf("flake workspace command = %q, want flake-run wrapper", got)
	}
	if !strings.Contains(got, `cd "$HOME/.openclaw/workspace" && go test ./...`) {
		t.Fatalf("flake workspace command = %q, want cd into live workspace", got)
	}
}

// TestRunExecCommandResolvesWorkspaceRelativePaths is the bridge-side
// regression for #691: exec.run commands like "bash scripts/foo.sh" must
// execute in ~/.openclaw/workspace, not the bridge process working directory.
func TestRunExecCommandResolvesWorkspaceRelativePaths(t *testing.T) {
	home := setBridgeTestWorkspaceHome(t)
	scripts := filepath.Join(home, ".openclaw", "workspace", "scripts")
	if err := os.MkdirAll(scripts, 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scripts, "prepare-deps-branch.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho staged-ok\n"), 0700); err != nil {
		t.Fatal(err)
	}

	s := &controlSupervisor{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, kind := s.runExecCommand(ctx, workflowControlBinding{}, "task-691",
		typesv2.ExecRunConfig{Command: "bash scripts/prepare-deps-branch.sh"})
	if kind != typesv2.MessageExecRunCompleted {
		t.Fatalf("kind = %v receipt = %#v", kind, receipt)
	}
	if code, _ := receipt["exit_code"].(int); code != 0 {
		t.Fatalf("exit_code = %d receipt = %#v (workspace-relative script not found)", code, receipt)
	}
	if out, _ := receipt["stdout"].(string); strings.TrimSpace(out) != "staged-ok" {
		t.Fatalf("stdout = %q receipt = %#v", out, receipt)
	}
}
