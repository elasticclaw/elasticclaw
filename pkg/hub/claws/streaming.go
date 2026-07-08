package claws

import "github.com/google/uuid"

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

	_, err := s.db.Exec(
		`INSERT INTO messages(id,claw_id,tenant_id,role,content,created_at) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET content=excluded.content`,
		msgID, clawID, tenantID, "claw", content, now(),
	)
	return err
}

// FlushStreamingSegment is the exported entry point for the pkg/hub bridge
// (and its tests, which stayed in pkg/hub).
func (s *Service) FlushStreamingSegment(clawID, tenantID string, cc *Conn) error {
	return s.flushStreamingSegment(clawID, tenantID, cc)
}
