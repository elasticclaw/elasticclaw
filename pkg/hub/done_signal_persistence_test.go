package hub

import (
	"strings"
	"testing"
)

func TestDoneSignalRejectedWhenPRPersistenceFails(t *testing.T) {
	s := newDoneTestServer(t, "")
	const clawID = "claw-persistence-failure"
	if _, err := s.db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,linear_issue_id,created_at) VALUES(?,?,?,?,?,?,?)`, clawID, "test-tenant-id", "test", "elasticclaw", "connected", "LIN-1", now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE claw_prs`); err != nil {
		t.Fatal(err)
	}
	s.handleClawDoneSignal(clawID, "[DONE] https://github.com/owner/repo/pull/1")
	var status, message string
	if err := s.db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT content FROM messages WHERE claw_id=? ORDER BY created_at DESC LIMIT 1`, clawID).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if status != "connected" {
		t.Fatalf("status = %q, want connected", status)
	}
	if !strings.Contains(message, "Failed to register PR") || !strings.Contains(message, "Please resend") {
		t.Fatalf("unexpected retry message: %q", message)
	}
}
