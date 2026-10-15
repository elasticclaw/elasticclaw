package hub

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const clawControlTokenHeader = "X-Claw-Token"

type workflowV2ControlBinding struct {
	RunID     string `json:"run_id"`
	AttemptID string `json:"attempt_id"`
}

type workflowV2ControlWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *workflowV2ControlWriter) write(ctx context.Context, frame typesv2.ControlFrame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(writeCtx, w.conn, frame)
}

// handleWorkflowV2ControlWS owns only deterministic workflow control. It does
// not accept or emit conversation messages, and it is independently
// authenticated and bound to one durable run attempt.
func (s *Server) handleWorkflowV2ControlWS(w http.ResponseWriter, r *http.Request) {
	headerToken := strings.TrimSpace(r.Header.Get(clawControlTokenHeader))
	tenantID, err := s.tenantByClawToken(headerToken)
	if headerToken == "" || err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(2 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	registrationCtx, registrationCancel := context.WithTimeout(ctx, 10*time.Second)
	defer registrationCancel()
	var first typesv2.ControlFrame
	if err := wsjson.Read(registrationCtx, conn, &first); err != nil || first.Type != typesv2.ControlFrameRegister || first.Registration == nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "expected control registration")
		return
	}
	registration := *first.Registration
	if err := typesv2.ValidateControlRegistration(registration); err != nil ||
		!hmac.Equal([]byte(headerToken), []byte(strings.TrimSpace(registration.Token))) {
		_ = conn.Close(websocket.StatusPolicyViolation, "invalid control registration")
		return
	}

	store := workflowv2.NewStore(s.db)
	if err := store.AuthorizeControlAttempt(ctx, registration.RunID, registration.AttemptID, registration.ClawID, tenantID); err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "workflow attempt is not authorized")
		return
	}
	connectionID := uuid.NewString()
	s.resumeWorkflowV2WorkTurns(registration.RunID, registration.AttemptID, connectionID)
	defer s.closeWorkflowV2WorkTurns(registration.RunID, registration.AttemptID, connectionID)

	snapshot, err := store.Snapshot(ctx, registration.RunID, registration.AttemptID)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, "workflow snapshot unavailable")
		return
	}

	writer := &workflowV2ControlWriter{conn: conn}
	if err := writer.write(ctx, typesv2.ControlFrame{Type: typesv2.ControlFrameRegistered, Snapshot: &snapshot}); err != nil {
		return
	}

	outboxErr := make(chan error, 1)
	go s.sendWorkflowV2ControlOutbox(ctx, store, writer, registration, tenantID, outboxErr)
	for {
		var frame typesv2.ControlFrame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return
		}
		if err := store.AuthorizeControlAttempt(ctx, registration.RunID, registration.AttemptID, registration.ClawID, tenantID); err != nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "workflow attempt is no longer active")
			return
		}
		switch frame.Type {
		case typesv2.ControlFrameReceipt:
			if frame.Receipt == nil || strings.TrimSpace(frame.Receipt.MessageID) == "" {
				_ = conn.Close(websocket.StatusPolicyViolation, "invalid control receipt")
				return
			}
			if err := store.AcknowledgeControl(ctx, registration.RunID, registration.AttemptID, *frame.Receipt); err != nil {
				_ = conn.Close(websocket.StatusPolicyViolation, "control receipt is not authorized")
				return
			}
		case typesv2.ControlFrameEnvelope:
			if frame.Envelope == nil {
				_ = conn.Close(websocket.StatusPolicyViolation, "missing control envelope")
				return
			}
			envelope := *frame.Envelope
			if envelope.RunID != registration.RunID || envelope.AttemptID != registration.AttemptID {
				_ = conn.Close(websocket.StatusPolicyViolation, "control envelope identity mismatch")
				return
			}
			receipt, applyErr := s.applyWorkflowV2ClawControl(ctx, store, envelope, connectionID)
			// The claw control may have moved the v2 run to a terminal state. If so,
			// finish the parent v1 task run and disconnect the claw.
			go s.maybeFinishWorkflowV2Parent(context.Background(), envelope.RunID)
			if applyErr != nil {
				if strings.TrimSpace(envelope.MessageID) == "" {
					_ = conn.Close(websocket.StatusPolicyViolation, "control message id is required")
					return
				}
				receipt, err = store.RejectAgentControl(ctx, envelope, applyErr.Error())
				if err != nil {
					_ = conn.Close(websocket.StatusInternalError, "control rejection could not be recorded")
					return
				}
			}
			if err := writer.write(ctx, typesv2.ControlFrame{Type: typesv2.ControlFrameReceipt, Receipt: &receipt}); err != nil {
				return
			}
		default:
			_ = conn.Close(websocket.StatusPolicyViolation, "unsupported control frame")
			return
		}
		select {
		case err := <-outboxErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[workflow-v2 control] outbox %s/%s: %v", registration.RunID, registration.AttemptID, err)
			}
			return
		default:
		}
	}
}

func (s *Server) applyWorkflowV2ClawControl(ctx context.Context, store *workflowv2.Store,
	envelope typesv2.ControlEnvelope, connectionID string) (typesv2.ControlReceipt, error) {
	if envelope.Kind == typesv2.MessageDeliverySubmitted || envelope.Kind == typesv2.MessagePullRequestClaimed {
		return store.ApplyDeliveryControl(ctx, envelope, s.workflowV2PullRequestVerifier())
	}
	switch envelope.Kind {
	case typesv2.MessageExecRunCompleted, typesv2.MessageExecRunFailed,
		typesv2.MessageDependencyUpdateCompleted, typesv2.MessageDependencyUpdateFailed:
		return store.ApplyCommandReceipt(ctx, envelope)
	}
	receipt, err := store.ApplyAgentControl(ctx, envelope)
	if err == nil && receipt.Disposition == typesv2.DispositionAccepted {
		s.trackWorkflowV2WorkTurn(envelope, connectionID)
	}
	return receipt, err
}

func (s *Server) sendWorkflowV2ControlOutbox(ctx context.Context, store *workflowv2.Store,
	writer *workflowV2ControlWriter, registration typesv2.ControlRegistration, tenantID string, result chan<- error) {
	defer writer.conn.CloseNow()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	// heldTicks counts consecutive held cycles; the hold is re-logged every
	// 120 ticks (~30s) so a run waiting on a slow or stuck workspace bootstrap
	// stays visible in hub logs instead of looking like a dead socket.
	heldTicks := 0
	for {
		if err := store.AuthorizeControlAttempt(ctx, registration.RunID, registration.AttemptID, registration.ClawID, tenantID); err != nil {
			result <- err
			return
		}
		// Hold task delivery until the claw's workspace bootstrap completed.
		// Replicated and exedev bridges start — and register here — while the
		// hub is still writing workspace files and cloning repositories; only
		// bootstrap_ok=1 marks that staging complete. Without this gate the
		// first exec.run executes against a sandbox that only contains the
		// early-staged flake files (#691). Providers that stage the workspace
		// before the bridge starts are ready immediately.
		ready, err := s.workflowV2ClawDispatchReady(ctx, registration.ClawID)
		if err != nil {
			result <- err
			return
		}
		if !ready {
			heldTicks++
			if heldTicks == 1 || heldTicks%120 == 0 {
				log.Printf("[workflow-v2 control] holding outbox for claw %s until workspace bootstrap completes", shortID(registration.ClawID))
			}
			select {
			case <-ctx.Done():
				result <- ctx.Err()
				return
			case <-ticker.C:
			}
			continue
		}
		heldTicks = 0
		envelopes, err := store.ReadyControl(ctx, registration.RunID, registration.AttemptID, 100)
		if err != nil {
			result <- err
			return
		}
		for i := range envelopes {
			envelope := envelopes[i]
			if err := writer.write(ctx, typesv2.ControlFrame{Type: typesv2.ControlFrameEnvelope, Envelope: &envelope}); err != nil {
				result <- err
				return
			}
			if err := store.MarkControlSent(ctx, envelope.MessageID); err != nil {
				result <- err
				return
			}
		}
		select {
		case <-ctx.Done():
			result <- ctx.Err()
			return
		case <-ticker.C:
		}
	}
}

// workflowV2ClawDispatchReady reports whether the attempt's claw has finished
// workspace bootstrap staging and may receive workflow v2 task assignments.
// VM providers that stage the workspace after the bridge is already running
// (daytona, replicated, exedev) gate on bootstrap_ok; providers that stage
// before the bridge starts (docker, lambda-microvms, noop) are always ready.
// A missing claw row holds delivery — the row exists before provisioning, so
// absence means the claw was deleted and the run cleanup will close this socket.
func (s *Server) workflowV2ClawDispatchReady(ctx context.Context, clawID string) (bool, error) {
	var provider string
	var bootstrapOK int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(provider,''), COALESCE(bootstrap_ok,0) FROM claws WHERE id=?`, clawID).
		Scan(&provider, &bootstrapOK)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return allowWakeBeforeBootstrap(provider, bootstrapOK), nil
}

// V2 agent turns use typed control events, not legacy conversation streaming.
type workflowV2WorkKey struct{ RunID, AttemptID, TaskID string }
type workflowV2WorkTurn struct {
	ClawID       string
	ConnectionID string
	StartedAt    int64
}

func (s *Server) trackWorkflowV2WorkTurn(envelope typesv2.ControlEnvelope, connectionID string) {
	key := workflowV2WorkKey{envelope.RunID, envelope.AttemptID, envelope.TaskID}
	switch envelope.Kind {
	case typesv2.MessageAgentTaskStarted:
		var clawID string
		if err := s.db.QueryRow(`SELECT claw_id FROM workflow_v2_attempts WHERE id=? AND run_id=?`, envelope.AttemptID, envelope.RunID).Scan(&clawID); err != nil {
			log.Printf("[task-run-analytics] resolve v2 work attempt %s: %v", envelope.AttemptID, err)
			return
		}
		// SentAt records the actual bridge turn boundary, even after outbox delay.
		s.workflowV2WorkTurns.LoadOrStore(key, workflowV2WorkTurn{ClawID: clawID, ConnectionID: connectionID, StartedAt: envelope.SentAt.UnixMilli()})
	case typesv2.MessageAgentTaskCompleted, typesv2.MessageAgentTaskFailed:
		if value, ok := s.workflowV2WorkTurns.Load(key); ok {
			turn := value.(workflowV2WorkTurn)
			if turn.ConnectionID != connectionID || !s.workflowV2WorkTurns.CompareAndDelete(key, value) {
				return
			}
			if turn.StartedAt == 0 {
				return
			}
			s.persistTaskRunWorkInterval(turn.ClawID, taskRunWorkInterval{turn.StartedAt, envelope.SentAt.UnixMilli()})
		}
	}
}

// Connection ownership prevents an old socket's cleanup from closing a turn
// already transferred to a replacement socket. Only explicitly observed starts
// are resumed; historical task state is never used as a work-time proxy.
func (s *Server) resumeWorkflowV2WorkTurns(runID, attemptID, connectionID string) {
	startedAt := now().UnixMilli()
	s.workflowV2WorkTurns.Range(func(key, value any) bool {
		identity := key.(workflowV2WorkKey)
		if identity.RunID != runID || identity.AttemptID != attemptID {
			return true
		}
		turn := value.(workflowV2WorkTurn)
		turn.ConnectionID = connectionID
		if turn.StartedAt == 0 {
			turn.StartedAt = startedAt
		}
		s.workflowV2WorkTurns.CompareAndSwap(key, value, turn)
		return true
	})
}

func (s *Server) closeWorkflowV2WorkTurns(runID, attemptID, connectionID string) {
	endedAt := now().UnixMilli()
	s.workflowV2WorkTurns.Range(func(key, value any) bool {
		identity := key.(workflowV2WorkKey)
		turn := value.(workflowV2WorkTurn)
		if identity.RunID == runID && identity.AttemptID == attemptID && turn.ConnectionID == connectionID && turn.StartedAt != 0 {
			paused := turn
			paused.StartedAt = 0
			if s.workflowV2WorkTurns.CompareAndSwap(key, value, paused) {
				s.persistTaskRunWorkInterval(turn.ClawID, taskRunWorkInterval{turn.StartedAt, endedAt})
			}
		}
		return true
	})
}
