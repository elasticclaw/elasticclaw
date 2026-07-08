package claws

import (
	"context"

	"github.com/google/uuid"

	"github.com/elasticclaw/elasticclaw/pkg/types"
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
	cc.Mu.Unlock()

	return s.st.Messages().Upsert(context.Background(), types.HubMessage{
		ID: msgID, ClawID: clawID, TenantID: tenantID, Role: "claw",
		Content: content, CreatedAt: now(),
	})
}

// FlushStreamingSegment is the exported entry point for the pkg/hub bridge
// (and its tests, which stayed in pkg/hub).
func (s *Service) FlushStreamingSegment(clawID, tenantID string, cc *Conn) error {
	return s.flushStreamingSegment(clawID, tenantID, cc)
}
