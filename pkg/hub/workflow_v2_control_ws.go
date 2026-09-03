package hub

import (
	"context"
	"crypto/hmac"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
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
			receipt, applyErr := s.applyWorkflowV2ClawControl(ctx, store, envelope)
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
	envelope typesv2.ControlEnvelope) (typesv2.ControlReceipt, error) {
	if envelope.Kind == typesv2.MessageDeliverySubmitted || envelope.Kind == typesv2.MessagePullRequestClaimed {
		return store.ApplyDeliveryControl(ctx, envelope, s.workflowV2PullRequestVerifier())
	}
	switch envelope.Kind {
	case typesv2.MessageExecRunCompleted, typesv2.MessageExecRunFailed,
		typesv2.MessageDependencyUpdateCompleted, typesv2.MessageDependencyUpdateFailed:
		return store.ApplyCommandReceipt(ctx, envelope)
	}
	return store.ApplyAgentControl(ctx, envelope)
}

func (s *Server) sendWorkflowV2ControlOutbox(ctx context.Context, store *workflowv2.Store,
	writer *workflowV2ControlWriter, registration typesv2.ControlRegistration, tenantID string, result chan<- error) {
	defer writer.conn.CloseNow()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := store.AuthorizeControlAttempt(ctx, registration.RunID, registration.AttemptID, registration.ClawID, tenantID); err != nil {
			result <- err
			return
		}
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
