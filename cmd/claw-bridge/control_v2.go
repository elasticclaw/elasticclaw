package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const (
	localControlPath   = "/v2/control"
	controlTokenHeader = "X-Claw-Token"
)

type bridgeProtocolRegistration = typesv2.BridgeRegistration

type workflowControlBinding struct {
	RunID     string `json:"run_id"`
	AttemptID string `json:"attempt_id"`
}

func (b workflowControlBinding) equal(other workflowControlBinding) bool {
	return b.RunID == other.RunID && b.AttemptID == other.AttemptID
}

type bridgeControlStore struct {
	db *sql.DB
}

func openBridgeControlStore(path string) (*bridgeControlStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	statements := []string{
		`CREATE TABLE IF NOT EXISTS bridge_control_inbox (
			message_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
			kind TEXT NOT NULL, envelope_json TEXT NOT NULL, status TEXT NOT NULL,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bridge_control_inbox_task
			ON bridge_control_inbox(run_id,attempt_id,status,created_at)`,
		`CREATE TABLE IF NOT EXISTS bridge_control_outbox (
			message_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
			kind TEXT NOT NULL, envelope_json TEXT NOT NULL, status TEXT NOT NULL,
			next_attempt_at INTEGER NOT NULL DEFAULT 0, receipt_json TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bridge_control_outbox_ready
			ON bridge_control_outbox(run_id,attempt_id,status,next_attempt_at,created_at)`,
		`CREATE TABLE IF NOT EXISTS bridge_control_snapshots (
			run_id TEXT NOT NULL, attempt_id TEXT NOT NULL, snapshot_json TEXT NOT NULL,
			updated_at INTEGER NOT NULL, PRIMARY KEY(run_id,attempt_id)
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &bridgeControlStore{db: db}, nil
}

func (s *bridgeControlStore) close() error { return s.db.Close() }

func (s *bridgeControlStore) saveSnapshot(snapshot typesv2.WorkflowSnapshot) error {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	_, err = s.db.Exec(`INSERT INTO bridge_control_snapshots(run_id,attempt_id,snapshot_json,updated_at)
		VALUES(?,?,?,?) ON CONFLICT(run_id,attempt_id) DO UPDATE SET
		snapshot_json=excluded.snapshot_json,updated_at=excluded.updated_at`,
		snapshot.RunID, snapshot.AttemptID, string(raw), now)
	return err
}

func (s *bridgeControlStore) snapshot(binding workflowControlBinding) (typesv2.WorkflowSnapshot, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT snapshot_json FROM bridge_control_snapshots WHERE run_id=? AND attempt_id=?`,
		binding.RunID, binding.AttemptID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return typesv2.WorkflowSnapshot{}, false, nil
	}
	if err != nil {
		return typesv2.WorkflowSnapshot{}, false, err
	}
	var snapshot typesv2.WorkflowSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return typesv2.WorkflowSnapshot{}, false, err
	}
	return snapshot, true, nil
}

func (s *bridgeControlStore) recordIncoming(envelope typesv2.ControlEnvelope) (bool, error) {
	raw, err := json.Marshal(envelope)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC().UnixMilli()
	result, err := s.db.Exec(`INSERT INTO bridge_control_inbox(
		message_id,run_id,attempt_id,kind,envelope_json,status,created_at,updated_at)
		VALUES(?,?,?,?,?,'accepted',?,?) ON CONFLICT(message_id) DO NOTHING`, envelope.MessageID,
		envelope.RunID, envelope.AttemptID, string(envelope.Kind), string(raw), now, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 0, err
}

func (s *bridgeControlStore) setIncomingStatus(messageID, from, to string) (bool, error) {
	now := time.Now().UTC().UnixMilli()
	result, err := s.db.Exec(`UPDATE bridge_control_inbox SET status=?,updated_at=? WHERE message_id=? AND status=?`,
		to, now, messageID, from)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *bridgeControlStore) incomingByStatus(binding workflowControlBinding, status string) ([]typesv2.ControlEnvelope, error) {
	rows, err := s.db.Query(`SELECT envelope_json FROM bridge_control_inbox
		WHERE run_id=? AND attempt_id=? AND status=? ORDER BY created_at,message_id`,
		binding.RunID, binding.AttemptID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var envelopes []typesv2.ControlEnvelope
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var envelope typesv2.ControlEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return nil, err
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes, rows.Err()
}

func (s *bridgeControlStore) enqueue(envelope typesv2.ControlEnvelope) error {
	raw, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	_, err = s.db.Exec(`INSERT INTO bridge_control_outbox(
		message_id,run_id,attempt_id,kind,envelope_json,status,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,?,?,'pending',0,?,?) ON CONFLICT(message_id) DO NOTHING`, envelope.MessageID,
		envelope.RunID, envelope.AttemptID, string(envelope.Kind), string(raw), now, now)
	return err
}

func (s *bridgeControlStore) ready(binding workflowControlBinding, limit int) ([]typesv2.ControlEnvelope, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT envelope_json FROM bridge_control_outbox
		WHERE run_id=? AND attempt_id=? AND status IN ('pending','sent') AND next_attempt_at<=?
		ORDER BY created_at,message_id LIMIT ?`, binding.RunID, binding.AttemptID, time.Now().UTC().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var envelopes []typesv2.ControlEnvelope
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var envelope typesv2.ControlEnvelope
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			return nil, err
		}
		envelopes = append(envelopes, envelope)
	}
	return envelopes, rows.Err()
}

func (s *bridgeControlStore) markSent(messageID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE bridge_control_outbox SET status='sent',next_attempt_at=?,updated_at=?
		WHERE message_id=? AND status IN ('pending','sent')`, now.Add(5*time.Second).UnixMilli(), now.UnixMilli(), messageID)
	return err
}

func (s *bridgeControlStore) acknowledge(receipt typesv2.ControlReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	now := time.Now().UTC().UnixMilli()
	result, err := s.db.Exec(`UPDATE bridge_control_outbox SET status='acknowledged',receipt_json=?,updated_at=?
		WHERE message_id=? AND status IN ('pending','sent')`, string(raw), now, receipt.MessageID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("control receipt does not match a pending bridge message")
	}
	return nil
}

func (s *bridgeControlStore) receipt(messageID string) (typesv2.ControlReceipt, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT receipt_json FROM bridge_control_outbox
		WHERE message_id=? AND status='acknowledged'`, messageID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return typesv2.ControlReceipt{}, false, nil
	}
	if err != nil {
		return typesv2.ControlReceipt{}, false, err
	}
	var receipt typesv2.ControlReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return typesv2.ControlReceipt{}, false, err
	}
	return receipt, true, nil
}

type controlSocketWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

type activeTaskCancellation struct {
	cancel context.CancelFunc
}

func (w *controlSocketWriter) write(ctx context.Context, frame typesv2.ControlFrame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(writeCtx, w.conn, frame)
}

type controlSupervisor struct {
	ctx    context.Context
	wsURL  string
	clawID string
	token  string
	bridge typesv2.BridgeRegistration
	store  *bridgeControlStore

	mu          sync.RWMutex
	binding     *workflowControlBinding
	bindingStop context.CancelFunc
	connected   bool
	snapshot    *typesv2.WorkflowSnapshot
	gateway     *gatewaySession
	activeTasks map[string]*activeTaskCancellation
	recovered   map[workflowControlBinding]bool
}

func newControlSupervisor(ctx context.Context, wsURL, clawID, token string,
	bridge typesv2.BridgeRegistration, store *bridgeControlStore) *controlSupervisor {
	return &controlSupervisor{ctx: ctx, wsURL: wsURL, clawID: clawID, token: token,
		bridge: bridge, store: store, activeTasks: map[string]*activeTaskCancellation{},
		recovered: map[workflowControlBinding]bool{}}
}

func (s *controlSupervisor) setGatewaySession(session *gatewaySession) {
	s.mu.Lock()
	s.gateway = session
	s.mu.Unlock()
}

func (s *controlSupervisor) updateBinding(binding *workflowControlBinding) {
	s.mu.Lock()
	if binding == nil {
		if s.bindingStop != nil {
			s.bindingStop()
		}
		s.binding, s.bindingStop, s.connected, s.snapshot = nil, nil, false, nil
		s.mu.Unlock()
		return
	}
	if s.binding != nil && s.binding.equal(*binding) {
		s.mu.Unlock()
		return
	}
	if s.bindingStop != nil {
		s.bindingStop()
	}
	bindingCtx, cancel := context.WithCancel(s.ctx)
	copyBinding := *binding
	s.binding, s.bindingStop, s.connected = &copyBinding, cancel, false
	if snapshot, found, err := s.store.snapshot(copyBinding); err == nil && found {
		s.snapshot = &snapshot
	} else {
		s.snapshot = nil
	}
	s.mu.Unlock()
	go s.runBinding(bindingCtx, copyBinding)
}

func (s *controlSupervisor) runBinding(ctx context.Context, binding workflowControlBinding) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := s.runConnection(ctx, binding); err != nil && ctx.Err() == nil {
			log.Printf("[control-v2] disconnected: %v — retry in %v", err, backoff)
		}
		s.setConnected(binding, false)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}
}

func (s *controlSupervisor) runConnection(ctx context.Context, binding workflowControlBinding) error {
	conn, _, err := websocket.Dial(ctx, s.wsURL, &websocket.DialOptions{HTTPHeader: http.Header{
		controlTokenHeader: {s.token}, "User-Agent": {"claw-bridge/" + Version},
		"ngrok-skip-browser-warning": {"true"},
	}})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(2 << 20)
	writer := &controlSocketWriter{conn: conn}
	registration := typesv2.ControlRegistration{Token: s.token, ClawID: s.clawID,
		RunID: binding.RunID, AttemptID: binding.AttemptID, Bridge: s.bridge}
	if err := writer.write(ctx, typesv2.ControlFrame{Type: typesv2.ControlFrameRegister, Registration: &registration}); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	var ack typesv2.ControlFrame
	if err := wsjson.Read(ctx, conn, &ack); err != nil {
		return fmt.Errorf("read registration: %w", err)
	}
	if ack.Type != typesv2.ControlFrameRegistered || ack.Snapshot == nil ||
		ack.Snapshot.RunID != binding.RunID || ack.Snapshot.AttemptID != binding.AttemptID {
		return fmt.Errorf("invalid registration snapshot")
	}
	if err := s.store.saveSnapshot(*ack.Snapshot); err != nil {
		return fmt.Errorf("persist snapshot: %w", err)
	}
	s.setSnapshot(binding, *ack.Snapshot)
	s.setConnected(binding, true)
	if s.claimRecovery(binding) {
		s.recoverInterrupted(ctx, binding)
	}
	log.Printf("[control-v2] connected run=%s attempt=%s", binding.RunID, binding.AttemptID)

	connectionCtx, connectionCancel := context.WithCancel(ctx)
	defer connectionCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-connectionCtx.Done():
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(connectionCtx, 10*time.Second)
				err := conn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					conn.CloseNow()
					return
				}
			}
		}
	}()
	writeErr := make(chan error, 1)
	go s.sendOutbox(connectionCtx, binding, writer, writeErr)
	for {
		var frame typesv2.ControlFrame
		if err := wsjson.Read(connectionCtx, conn, &frame); err != nil {
			return err
		}
		switch frame.Type {
		case typesv2.ControlFrameReceipt:
			if frame.Receipt == nil {
				return fmt.Errorf("missing control receipt")
			}
			if err := s.store.acknowledge(*frame.Receipt); err != nil {
				return err
			}
			s.updateReceiptVersion(binding, *frame.Receipt)
		case typesv2.ControlFrameEnvelope:
			if frame.Envelope == nil {
				return fmt.Errorf("missing control envelope")
			}
			receipt, startTask, err := s.acceptHubEnvelope(binding, *frame.Envelope)
			if err != nil {
				receipt = typesv2.ControlReceipt{MessageID: frame.Envelope.MessageID,
					Disposition: typesv2.DispositionRejected, Reason: err.Error()}
			}
			if err := writer.write(connectionCtx, typesv2.ControlFrame{Type: typesv2.ControlFrameReceipt, Receipt: &receipt}); err != nil {
				return err
			}
			if startTask != nil && receipt.Disposition == typesv2.DispositionAccepted {
				s.startTask(ctx, binding, frame.Envelope.MessageID, *startTask)
			}
		default:
			return fmt.Errorf("unsupported control frame %q", frame.Type)
		}
		select {
		case err := <-writeErr:
			return err
		default:
		}
	}
}

func (s *controlSupervisor) sendOutbox(ctx context.Context, binding workflowControlBinding,
	writer *controlSocketWriter, result chan<- error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		envelopes, err := s.store.ready(binding, 100)
		if err != nil {
			result <- err
			writer.conn.CloseNow()
			return
		}
		for i := range envelopes {
			envelope := envelopes[i]
			if err := writer.write(ctx, typesv2.ControlFrame{Type: typesv2.ControlFrameEnvelope, Envelope: &envelope}); err != nil {
				result <- err
				writer.conn.CloseNow()
				return
			}
			if err := s.store.markSent(envelope.MessageID); err != nil {
				result <- err
				writer.conn.CloseNow()
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *controlSupervisor) acceptHubEnvelope(binding workflowControlBinding,
	envelope typesv2.ControlEnvelope) (typesv2.ControlReceipt, *typesv2.AgentTask, error) {
	if envelope.RunID != binding.RunID || envelope.AttemptID != binding.AttemptID {
		return typesv2.ControlReceipt{}, nil, fmt.Errorf("control envelope identity mismatch")
	}
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionHubToClaw); err != nil {
		return typesv2.ControlReceipt{}, nil, err
	}
	var task *typesv2.AgentTask
	switch envelope.Kind {
	case typesv2.MessageAgentTaskAssign:
		var decoded typesv2.AgentTask
		if err := json.Unmarshal(envelope.Payload, &decoded); err != nil {
			return typesv2.ControlReceipt{}, nil, fmt.Errorf("decode agent task: %w", err)
		}
		if decoded.ID != envelope.TaskID || decoded.RunID != binding.RunID || decoded.AttemptID != binding.AttemptID {
			return typesv2.ControlReceipt{}, nil, fmt.Errorf("agent task identity mismatch")
		}
		currentVersion := s.stateVersion(binding)
		if decoded.StateVersion < currentVersion {
			return typesv2.ControlReceipt{}, nil, fmt.Errorf("agent task state version %d is older than snapshot version %d",
				decoded.StateVersion, currentVersion)
		}
		if envelope.ExpectedStateVersion != nil && *envelope.ExpectedStateVersion != decoded.StateVersion {
			return typesv2.ControlReceipt{}, nil, fmt.Errorf("agent task envelope and payload state versions differ")
		}
		task = &decoded
	case typesv2.MessageAgentTaskCancel, typesv2.MessageRunSuspend, typesv2.MessageRunTerminate:
		// These controls cancel local task execution below after durable receipt.
	case typesv2.MessageWorkflowSync:
		var snapshot typesv2.WorkflowSnapshot
		if err := json.Unmarshal(envelope.Payload, &snapshot); err != nil {
			return typesv2.ControlReceipt{}, nil, fmt.Errorf("decode workflow snapshot: %w", err)
		}
		if snapshot.RunID != binding.RunID || snapshot.AttemptID != binding.AttemptID {
			return typesv2.ControlReceipt{}, nil, fmt.Errorf("workflow snapshot identity mismatch")
		}
		if err := s.store.saveSnapshot(snapshot); err != nil {
			return typesv2.ControlReceipt{}, nil, err
		}
		s.setSnapshot(binding, snapshot)
	case typesv2.MessageRunResume:
	default:
		return typesv2.ControlReceipt{}, nil, fmt.Errorf("bridge does not support control kind %q", envelope.Kind)
	}
	duplicate, err := s.store.recordIncoming(envelope)
	if err != nil {
		return typesv2.ControlReceipt{}, nil, err
	}
	if duplicate {
		return typesv2.ControlReceipt{MessageID: envelope.MessageID,
			Disposition: typesv2.DispositionDuplicate, StateVersion: s.stateVersion(binding)}, nil, nil
	}
	if task != nil {
		s.updateTaskSnapshot(binding, task)
	}
	if envelope.Kind == typesv2.MessageAgentTaskCancel {
		s.cancelTask(envelope.TaskID)
	}
	if envelope.Kind == typesv2.MessageRunSuspend || envelope.Kind == typesv2.MessageRunTerminate {
		s.cancelAllTasks()
	}
	return typesv2.ControlReceipt{MessageID: envelope.MessageID,
		Disposition: typesv2.DispositionAccepted, StateVersion: s.stateVersion(binding)}, task, nil
}

// startTask registers cancellation synchronously on the WebSocket reader path
// before it launches agent work. A cancellation frame read immediately after
// the assignment can therefore always find and stop this task.
func (s *controlSupervisor) startTask(ctx context.Context, binding workflowControlBinding,
	assignmentMessageID string, task typesv2.AgentTask) {
	taskCtx, cancel, activeCancellation := s.prepareTaskExecution(ctx, task)
	go s.executeTask(taskCtx, binding, assignmentMessageID, task, cancel, activeCancellation)
}

func (s *controlSupervisor) prepareTaskExecution(ctx context.Context,
	task typesv2.AgentTask) (context.Context, context.CancelFunc, *activeTaskCancellation) {
	deadline := task.Deadline
	if deadline.IsZero() {
		deadline = time.Now().Add(2 * time.Hour)
	}
	taskCtx, cancel := context.WithDeadline(ctx, deadline)
	activeCancellation := s.registerTaskCancellation(task.ID, cancel)
	return taskCtx, cancel, activeCancellation
}

func (s *controlSupervisor) executeTask(taskCtx context.Context, binding workflowControlBinding,
	assignmentMessageID string, task typesv2.AgentTask, cancel context.CancelFunc,
	activeCancellation *activeTaskCancellation) {
	defer func() {
		cancel()
		s.unregisterTaskCancellation(task.ID, activeCancellation)
	}()
	claimed, err := s.store.setIncomingStatus(assignmentMessageID, "accepted", "running")
	if err != nil || !claimed {
		if err != nil {
			log.Printf("[control-v2] claim task %s: %v", task.ID, err)
		}
		return
	}
	s.mu.RLock()
	gateway := s.gateway
	s.mu.RUnlock()
	if gateway == nil || !gateway.IsReady() {
		s.finishTask(binding, assignmentMessageID, task, typesv2.MessageAgentTaskFailed, "gateway is not ready")
		return
	}
	if err := s.enqueueTaskEvent(binding, task, typesv2.MessageAgentTaskStarted,
		taskLifecyclePayload(map[string]interface{}{"bridge_started_at": time.Now().UTC()})); err != nil {
		s.finishTask(binding, assignmentMessageID, task, typesv2.MessageAgentTaskFailed, err.Error())
		return
	}
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-taskCtx.Done():
				return
			case <-ticker.C:
				_ = s.enqueueTaskEvent(binding, task, typesv2.MessageAgentTaskHeartbeat,
					taskLifecyclePayload(map[string]interface{}{"heartbeat_at": time.Now().UTC()}))
			}
		}
	}()
	prompt := workflowV2TaskPrompt(task)
	response, sendErr := gateway.SendMessage(taskCtx, prompt, func(string) {}, func(agentActivity) {})
	close(heartbeatDone)
	if sendErr != nil {
		s.finishTask(binding, assignmentMessageID, task, typesv2.MessageAgentTaskFailed, sendErr.Error())
		return
	}
	digest := sha256.Sum256([]byte(response))
	payload := taskLifecyclePayload(map[string]interface{}{
		"gateway_turn_completed": true, "response_bytes": len(response), "response_sha256": hex.EncodeToString(digest[:]),
	})
	if err := s.enqueueTaskEvent(binding, task, typesv2.MessageAgentTaskCompleted, payload); err != nil {
		log.Printf("[control-v2] queue completion for task %s: %v", task.ID, err)
		return
	}
	_, _ = s.store.setIncomingStatus(assignmentMessageID, "running", "completed")
}

func (s *controlSupervisor) finishTask(binding workflowControlBinding, assignmentMessageID string,
	task typesv2.AgentTask, kind typesv2.ControlMessageKind, reason string) {
	if err := s.enqueueTaskEvent(binding, task, kind, taskLifecyclePayload(map[string]interface{}{"error": reason})); err != nil {
		log.Printf("[control-v2] queue terminal event for task %s: %v", task.ID, err)
		return
	}
	_, _ = s.store.setIncomingStatus(assignmentMessageID, "running", "failed")
}

func taskLifecyclePayload(values map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"task": values}
}

func (s *controlSupervisor) enqueueTaskEvent(binding workflowControlBinding, task typesv2.AgentTask,
	kind typesv2.ControlMessageKind, payload interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	version := task.StateVersion
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: uuid.NewString(), Kind: kind, RunID: binding.RunID, AttemptID: binding.AttemptID,
		TaskID: task.ID, ExpectedStateVersion: &version, SentAt: time.Now().UTC(), Payload: raw}
	if kind == typesv2.MessageAgentTaskHeartbeat {
		envelope.ExpectedStateVersion = nil
	}
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionClawToHub); err != nil {
		return err
	}
	return s.store.enqueue(envelope)
}

func (s *controlSupervisor) recoverInterrupted(ctx context.Context, binding workflowControlBinding) {
	envelopes, err := s.store.incomingByStatus(binding, "running")
	if err != nil {
		log.Printf("[control-v2] load interrupted tasks: %v", err)
		return
	}
	for _, envelope := range envelopes {
		if envelope.Kind != typesv2.MessageAgentTaskAssign {
			continue
		}
		var task typesv2.AgentTask
		if err := json.Unmarshal(envelope.Payload, &task); err != nil {
			continue
		}
		s.finishTask(binding, envelope.MessageID, task, typesv2.MessageAgentTaskFailed,
			"bridge restarted while task execution outcome was unknown")
	}
	accepted, err := s.store.incomingByStatus(binding, "accepted")
	if err != nil {
		return
	}
	for _, envelope := range accepted {
		if envelope.Kind != typesv2.MessageAgentTaskAssign {
			continue
		}
		var task typesv2.AgentTask
		if json.Unmarshal(envelope.Payload, &task) == nil {
			s.startTask(ctx, binding, envelope.MessageID, task)
		}
	}
}

func (s *controlSupervisor) claimRecovery(binding workflowControlBinding) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovered[binding] {
		return false
	}
	s.recovered[binding] = true
	return true
}

func workflowV2TaskPrompt(task typesv2.AgentTask) string {
	return fmt.Sprintf(`You are executing ElasticClaw workflow v2 task %s in state %s.

%s

Workflow execution is controlled only through typed messages, never through phrases in your response. Use:
  claw-bridge control status
  claw-bridge control plan --payload '<json>'
  claw-bridge control delivery <pull-request-url> [more URLs...]
  claw-bridge control help --payload '<json>'

The bridge reports task start, heartbeat, and terminal execution status automatically.`, task.ID, task.State, task.Instructions)
}

func (s *controlSupervisor) setConnected(binding workflowControlBinding, connected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding != nil && s.binding.equal(binding) {
		s.connected = connected
	}
}

func (s *controlSupervisor) setSnapshot(binding workflowControlBinding, snapshot typesv2.WorkflowSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding != nil && s.binding.equal(binding) {
		copySnapshot := snapshot
		s.snapshot = &copySnapshot
	}
}

func (s *controlSupervisor) updateTaskSnapshot(binding workflowControlBinding, task *typesv2.AgentTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding == nil || !s.binding.equal(binding) {
		return
	}
	if s.snapshot == nil {
		s.snapshot = &typesv2.WorkflowSnapshot{RunID: binding.RunID, AttemptID: binding.AttemptID}
	}
	copyTask := *task
	s.snapshot.CurrentTask = &copyTask
	s.snapshot.State = task.State
	s.snapshot.StateVersion = task.StateVersion
	snapshot := *s.snapshot
	_ = s.store.saveSnapshot(snapshot)
}

func (s *controlSupervisor) updateReceiptVersion(binding workflowControlBinding, receipt typesv2.ControlReceipt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding == nil || !s.binding.equal(binding) || s.snapshot == nil {
		return
	}
	if receipt.StateVersion > s.snapshot.StateVersion {
		s.snapshot.StateVersion = receipt.StateVersion
		snapshot := *s.snapshot
		_ = s.store.saveSnapshot(snapshot)
	}
}

func (s *controlSupervisor) stateVersion(binding workflowControlBinding) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.binding != nil && s.binding.equal(binding) && s.snapshot != nil {
		return s.snapshot.StateVersion
	}
	return 0
}

func (s *controlSupervisor) cancelTask(taskID string) {
	s.mu.RLock()
	activeCancellation := s.activeTasks[taskID]
	s.mu.RUnlock()
	if activeCancellation != nil {
		activeCancellation.cancel()
	}
}

func (s *controlSupervisor) registerTaskCancellation(taskID string,
	cancel context.CancelFunc) *activeTaskCancellation {
	activeCancellation := &activeTaskCancellation{cancel: cancel}
	s.mu.Lock()
	s.activeTasks[taskID] = activeCancellation
	s.mu.Unlock()
	return activeCancellation
}

func (s *controlSupervisor) unregisterTaskCancellation(taskID string,
	activeCancellation *activeTaskCancellation) {
	s.mu.Lock()
	if s.activeTasks[taskID] == activeCancellation {
		delete(s.activeTasks, taskID)
	}
	s.mu.Unlock()
}

func (s *controlSupervisor) cancelAllTasks() {
	s.mu.RLock()
	cancels := make([]context.CancelFunc, 0, len(s.activeTasks))
	for _, activeCancellation := range s.activeTasks {
		cancels = append(cancels, activeCancellation.cancel)
	}
	s.mu.RUnlock()
	for _, cancel := range cancels {
		cancel()
	}
}

type localControlRequest struct {
	Kind                 typesv2.ControlMessageKind `json:"kind"`
	TaskID               string                     `json:"task_id,omitempty"`
	ExpectedStateVersion *uint64                    `json:"expected_state_version,omitempty"`
	Payload              json.RawMessage            `json:"payload,omitempty"`
}

func (s *controlSupervisor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	binding, connected := s.binding, s.connected
	var snapshot *typesv2.WorkflowSnapshot
	if s.snapshot != nil {
		copySnapshot := *s.snapshot
		snapshot = &copySnapshot
	}
	s.mu.RUnlock()
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"connected": connected, "binding": binding, "snapshot": snapshot,
		})
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if binding == nil {
		http.Error(w, `{"error":"no active workflow v2 binding"}`, http.StatusConflict)
		return
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request localControlRequest
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	if request.TaskID == "" && snapshot != nil && snapshot.CurrentTask != nil {
		request.TaskID = snapshot.CurrentTask.ID
	}
	if request.ExpectedStateVersion == nil && snapshot != nil {
		version := snapshot.StateVersion
		request.ExpectedStateVersion = &version
	}
	envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
		MessageID: uuid.NewString(), Kind: request.Kind, RunID: binding.RunID, AttemptID: binding.AttemptID,
		TaskID: request.TaskID, ExpectedStateVersion: request.ExpectedStateVersion,
		SentAt: time.Now().UTC(), Payload: request.Payload}
	if request.Kind == typesv2.MessageAgentTaskHeartbeat || request.Kind == typesv2.MessageHelpRequested {
		envelope.ExpectedStateVersion = nil
	}
	if err := typesv2.ValidateControlEnvelope(envelope, typesv2.DirectionClawToHub); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	if err := s.store.enqueue(envelope); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	if connected {
		waitCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if receipt, found, err := s.waitForReceipt(waitCtx, envelope.MessageID); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
			return
		} else if found {
			status := http.StatusOK
			if receipt.Disposition != typesv2.DispositionAccepted && receipt.Disposition != typesv2.DispositionDuplicate {
				status = http.StatusConflict
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(receipt)
			return
		}
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"message_id": envelope.MessageID, "status": "pending"})
}

func (s *controlSupervisor) waitForReceipt(ctx context.Context, messageID string) (typesv2.ControlReceipt, bool, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		receipt, found, err := s.store.receipt(messageID)
		if err != nil || found {
			return receipt, found, err
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return typesv2.ControlReceipt{}, false, nil
			}
			return typesv2.ControlReceipt{}, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

type unavailableControlHandler struct{ reason string }

func (h unavailableControlHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, h.reason, http.StatusServiceUnavailable)
}

func defaultBridgeControlStorePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".elasticclaw", "control-v2.sqlite"), nil
}

func bridgeRegistration(controlAvailable bool) typesv2.BridgeRegistration {
	protocols := []string{typesv2.ProtocolConversationV1}
	capabilities := []string(nil)
	if controlAvailable {
		protocols = append(protocols, typesv2.ProtocolControlV2)
		capabilities = []string{"workflow.snapshot", "agent.task", "delivery.manifest", "local.control"}
	}
	return typesv2.BridgeRegistration{BridgeVersion: Version, Protocols: protocols, Capabilities: capabilities}
}

func runControlCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: claw-bridge control <status|send|plan|delivery|help> ...")
		return 2
	}
	if args[0] == "status" {
		return controlCLIRequest(http.MethodGet, nil)
	}
	request := localControlRequest{}
	switch args[0] {
	case "send":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: claw-bridge control send <kind> [--task-id=ID] [--state-version=N] [--payload=JSON]")
			return 2
		}
		request.Kind = typesv2.ControlMessageKind(args[1])
		args = args[2:]
	case "plan":
		request.Kind = typesv2.MessagePlanSubmitted
		args = args[1:]
	case "help":
		request.Kind = typesv2.MessageHelpRequested
		args = args[1:]
	case "delivery":
		request.Kind = typesv2.MessageDeliverySubmitted
		urls := args[1:]
		if len(urls) == 0 {
			fmt.Fprintln(os.Stderr, "usage: claw-bridge control delivery <pull-request-url> [more URLs...]")
			return 2
		}
		manifest := typesv2.DeliveryManifest{PullRequests: make([]typesv2.PullRequestClaim, len(urls))}
		for i, value := range urls {
			manifest.PullRequests[i] = typesv2.PullRequestClaim{URL: value}
		}
		request.Payload, _ = json.Marshal(manifest)
		args = nil
	default:
		fmt.Fprintf(os.Stderr, "unknown control command %q\n", args[0])
		return 2
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "--task-id="):
			request.TaskID = strings.TrimPrefix(arg, "--task-id=")
		case arg == "--task-id" && i+1 < len(args):
			i++
			request.TaskID = args[i]
		case strings.HasPrefix(arg, "--state-version="):
			value, err := strconv.ParseUint(strings.TrimPrefix(arg, "--state-version="), 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			request.ExpectedStateVersion = &value
		case arg == "--state-version" && i+1 < len(args):
			i++
			value, err := strconv.ParseUint(args[i], 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 2
			}
			request.ExpectedStateVersion = &value
		case strings.HasPrefix(arg, "--payload="):
			request.Payload = json.RawMessage(strings.TrimPrefix(arg, "--payload="))
		case arg == "--payload" && i+1 < len(args):
			i++
			request.Payload = json.RawMessage(args[i])
		default:
			fmt.Fprintf(os.Stderr, "unknown option %q\n", arg)
			return 2
		}
	}
	raw, err := json.Marshal(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return controlCLIRequest(http.MethodPost, raw)
}

func controlCLIRequest(method string, body []byte) int {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:18790"+localControlPath, reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	response, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_, _ = os.Stdout.Write(response)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 1
	}
	return 0
}
