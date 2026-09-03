package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

const (
	defaultExecRunTimeout          = 10 * time.Minute
	defaultDependencyUpdateTimeout = 30 * time.Minute
	maxExecOutputBytes             = 8000
)

// startCommandTask launches a deterministic exec.run or dependency.update task
// on the bridge. It records the incoming assignment as running, executes the
// command or generated dependency-update script, and emits a typed completion
// event back to the hub. These tasks never fall back to the agent gateway.
func (s *controlSupervisor) startCommandTask(ctx context.Context, binding workflowControlBinding,
	assignmentMessageID string, kind typesv2.ControlMessageKind, taskID string, payload json.RawMessage) {
	go s.executeCommandTask(ctx, binding, assignmentMessageID, kind, taskID, payload)
}

func (s *controlSupervisor) executeCommandTask(ctx context.Context, binding workflowControlBinding,
	assignmentMessageID string, kind typesv2.ControlMessageKind, taskID string, payload json.RawMessage) {
	claimed, err := s.store.setIncomingStatus(assignmentMessageID, "accepted", "running")
	if err != nil || !claimed {
		if err != nil {
			log.Printf("[control-v2] claim command task %s: %v", taskID, err)
		}
		return
	}

	taskCtx, cancel := context.WithCancel(ctx)
	activeCancellation := s.registerTaskCancellation(taskID, cancel)
	defer func() {
		cancel()
		s.unregisterTaskCancellation(taskID, activeCancellation)
	}()

	var receipt map[string]interface{}
	var completedKind typesv2.ControlMessageKind
	switch kind {
	case typesv2.MessageExecRunAssign:
		var cfg typesv2.ExecRunConfig
		if err := json.Unmarshal(payload, &cfg); err != nil {
			s.finishCommandTask(binding, taskID, assignmentMessageID, typesv2.MessageExecRunFailed,
				fmt.Errorf("decode exec.run config: %w", err))
			return
		}
		receipt, completedKind = s.runExecCommand(taskCtx, binding, taskID, cfg)
	case typesv2.MessageDependencyUpdateAssign:
		var cfg typesv2.DependencyUpdateConfig
		if err := json.Unmarshal(payload, &cfg); err != nil {
			s.finishCommandTask(binding, taskID, assignmentMessageID, typesv2.MessageDependencyUpdateFailed,
				fmt.Errorf("decode dependency.update config: %w", err))
			return
		}
		receipt, completedKind = s.runDependencyUpdateCommand(taskCtx, binding, taskID, cfg)
	default:
		s.finishCommandTask(binding, taskID, assignmentMessageID, typesv2.MessageExecRunFailed,
			fmt.Errorf("unsupported command task kind %q", kind))
		return
	}

	if err := s.enqueueCommandEvent(binding, taskID, completedKind, receipt); err != nil {
		log.Printf("[control-v2] queue command task completion %s: %v", taskID, err)
		return
	}
	_, _ = s.store.setIncomingStatus(assignmentMessageID, "running", "completed")
}

func (s *controlSupervisor) runExecCommand(ctx context.Context, binding workflowControlBinding,
	taskID string, cfg typesv2.ExecRunConfig) (map[string]interface{}, typesv2.ControlMessageKind) {
	timeout := defaultExecRunTimeout
	if cfg.Timeout != "" {
		if parsed, err := time.ParseDuration(cfg.Timeout); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", cfg.Command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	exitCode := 0
	succeeded := true
	errorMsg := ""
	if err != nil {
		succeeded = false
		if ctx.Err() == context.DeadlineExceeded {
			exitCode = 124
			errorMsg = fmt.Sprintf("command timed out after %s", timeout)
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			errorMsg = err.Error()
		}
	}
	if errorMsg == "" && !succeeded {
		errorMsg = fmt.Sprintf("exit code %d", exitCode)
	}

	receipt := map[string]interface{}{
		"exit_code": exitCode,
		"succeeded": succeeded,
		"stdout":    truncateOutput(stdout.String(), maxExecOutputBytes),
		"stderr":    truncateOutput(stderr.String(), maxExecOutputBytes),
	}
	if errorMsg != "" {
		receipt["error"] = errorMsg
	}
	if succeeded {
		return receipt, typesv2.MessageExecRunCompleted
	}
	return receipt, typesv2.MessageExecRunFailed
}

func (s *controlSupervisor) runDependencyUpdateCommand(ctx context.Context, binding workflowControlBinding,
	taskID string, cfg typesv2.DependencyUpdateConfig) (map[string]interface{}, typesv2.ControlMessageKind) {
	command, err := typesv2.BuildDependencyUpdateCommand(cfg)
	if err != nil {
		return map[string]interface{}{
			"succeeded": false,
			"error":     fmt.Sprintf("build dependency update command: %v", err),
		}, typesv2.MessageDependencyUpdateFailed
	}

	timeout := defaultDependencyUpdateTimeout
	if cfg.Timeout != "" {
		if parsed, err := time.ParseDuration(cfg.Timeout); err == nil && parsed > 0 {
			timeout = parsed
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()

	exitCode := 0
	succeeded := true
	errorMsg := ""
	if err != nil {
		succeeded = false
		if ctx.Err() == context.DeadlineExceeded {
			exitCode = 124
			errorMsg = fmt.Sprintf("dependency update timed out after %s", timeout)
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			errorMsg = err.Error()
		}
	}
	if errorMsg == "" && !succeeded {
		errorMsg = fmt.Sprintf("exit code %d", exitCode)
	}

	result := map[string]interface{}{
		"succeeded": succeeded,
	}
	if errorMsg != "" {
		result["error"] = errorMsg
	}
	if doc := lastJSONObject(stdout.String()); doc != nil {
		var parsed map[string]interface{}
		if err := json.Unmarshal(doc, &parsed); err == nil {
			for k, v := range parsed {
				result[k] = v
			}
		}
	}
	if succeeded {
		return result, typesv2.MessageDependencyUpdateCompleted
	}
	return result, typesv2.MessageDependencyUpdateFailed
}

func (s *controlSupervisor) finishCommandTask(binding workflowControlBinding, taskID, assignmentMessageID string,
	kind typesv2.ControlMessageKind, reason error) {
	if err := s.enqueueCommandEvent(binding, taskID, kind, map[string]interface{}{"succeeded": false, "error": reason.Error()}); err != nil {
		log.Printf("[control-v2] queue command task failure %s: %v", taskID, err)
		return
	}
	_, _ = s.store.setIncomingStatus(assignmentMessageID, "running", "failed")
}

func (s *controlSupervisor) enqueueCommandEvent(binding workflowControlBinding, taskID string,
	kind typesv2.ControlMessageKind, payload map[string]interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	version := s.stateVersion(binding)
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: uuid.NewString(), Kind: kind, RunID: binding.RunID, AttemptID: binding.AttemptID,
		TaskID: taskID, ExpectedStateVersion: &version, SentAt: time.Now().UTC(), Payload: raw}
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionClawToHub); err != nil {
		return err
	}
	return s.store.enqueue(envelope)
}

func truncateOutput(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max/2] + "\n...[truncated]\n" + value[len(value)-max/2:]
}

func lastJSONObject(s string) []byte {
	for close := strings.LastIndex(s, "}"); close >= 0; close = strings.LastIndex(s[:close], "}") {
		for open := strings.LastIndex(s[:close], "{"); open >= 0; open = strings.LastIndex(s[:open], "{") {
			candidate := s[open : close+1]
			var v any
			if json.Unmarshal([]byte(candidate), &v) == nil {
				return []byte(candidate)
			}
		}
	}
	return nil
}
