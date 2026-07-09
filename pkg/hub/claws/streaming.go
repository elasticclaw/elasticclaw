package claws

import (
	"time"

	"github.com/google/uuid"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Periodic partial-commit thresholds for the streaming buffer (phase-2
// item 2.3): the in-flight response is persisted whenever this many new
// bytes accumulated or this much time passed since the last commit, so a
// mid-stream timeout or hub crash loses at most one window instead of the
// whole turn. The record keeps the same message ID and is overwritten by
// the finalizing "message" (or by the disconnect flush, which marks it
// "[interrupted]") — it is partial until the stream ends.
const (
	streamingFlushBytes    = 8 << 10
	streamingFlushInterval = 2 * time.Second
)

// flushStreamingSegment persists the accumulated streaming buffer of a claw
// as its own message segment (used when agent activity splits a turn),
// moved verbatim from pkg/hub server.go.
func (s *Service) flushStreamingSegment(clawID, tenantID string, cc *Conn) error {
	cc.Mu.Lock()
	if cc.StreamingBuf.Len() == 0 {
		cc.Mu.Unlock()
		return nil
	}
	msgID := cc.StreamingMsgID
	if msgID == "" {
		msgID = uuid.New().String()
	}
	content := cc.StreamingBuf.String()
	cc.StreamingMsgID = ""
	cc.StreamingBuf.Reset()
	cc.StreamingSplit = true
	cc.StreamingFlushedLen = 0
	cc.StreamingFlushedAt = time.Time{}
	cc.Mu.Unlock()

	return s.st.Messages().Upsert(s.baseCtx(), types.HubMessage{
		ID: msgID, ClawID: clawID, TenantID: tenantID, Role: "claw",
		Content: content, CreatedAt: now(),
	})
}

// FlushStreamingSegment is the exported entry point for the pkg/hub bridge
// (and its tests, which stayed in pkg/hub).
func (s *Service) FlushStreamingSegment(clawID, tenantID string, cc *Conn) error {
	return s.flushStreamingSegment(clawID, tenantID, cc)
}
