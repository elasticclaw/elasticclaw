package hub

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestStopAgentTerminalAtomicallyFinishesWorkflowRun(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,status,created_at) VALUES('terminal-atomic','tenant','terminal','connected',?)`, tm); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_name,workspace_name,status,claw_id,created_at) VALUES('terminal-run','tenant','workflow','workspace','running','terminal-atomic',?)`, tm); err != nil {
		t.Fatal(err)
	}

	s.stopAgentWithReason("terminal-atomic", "Bootstrap failed: API_KEY=do-not-log", true)

	var clawStatus, diagnostic, runStatus string
	if err := db.QueryRow(`SELECT status, bootstrap_diagnostic FROM claws WHERE id='terminal-atomic'`).Scan(&clawStatus, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_runs WHERE id='terminal-run'`).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if clawStatus != "error" || runStatus != "failed" {
		t.Fatalf("terminal states claw=%q run=%q, want error/failed", clawStatus, runStatus)
	}
	if strings.Contains(diagnostic, "do-not-log") || !strings.Contains(diagnostic, "Bootstrap failed") {
		t.Fatalf("diagnostic was not sanitized: %q", diagnostic)
	}
}

func TestTerminalTransitionClosedDBLogsLoudly(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("stopAgentTerminalWithReason panicked: %v", r)
			}
		}()
		s.stopAgentTerminalWithReason("closed-db", "failure", true)
	}()
	if !strings.Contains(logs.String(), "TERMINAL transition failed twice") {
		t.Fatalf("missing loud terminal failure log: %s", logs.String())
	}
	logs.Reset()
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("execTerminalStatus panicked: %v", r)
			}
		}()
		_, _ = s.execTerminalStatus("closed-db", `UPDATE claws SET status='error' WHERE id=?`, "closed-db")
	}()
	if !strings.Contains(logs.String(), "TERMINAL transition failed twice") {
		t.Fatalf("missing loud exec terminal failure log: %s", logs.String())
	}
}

func TestStopAgentTerminalIsIdempotent(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	tm := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,created_at) VALUES('terminal-once','tenant','once','fake','vm-1','connected',?)`, tm); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_name,workspace_name,status,claw_id,created_at) VALUES('once-run','tenant','workflow','workspace','running','terminal-once',?)`, tm); err != nil {
		t.Fatal(err)
	}
	terminations := 0
	s.terminateVMOverride = func(_, _ string) error { terminations++; return nil }
	s.stopAgentTerminalWithReason("terminal-once", "failure", false)
	s.stopAgentTerminalWithReason("terminal-once", "failure", false)
	if terminations != 1 {
		t.Fatalf("VM terminations=%d, want 1", terminations)
	}
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_runs WHERE id='once-run'`).Scan(&runStatus); err != nil || runStatus != "failed" {
		t.Fatalf("workflow run status=%q err=%v, want failed", runStatus, err)
	}
}

func TestTerminateVMForClawClearsOnlyNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"not found", errors.New("provider returned 404 not found"), ""},
		{"transient", errors.New("temporary network failure"), "vm-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newReaperTestServer(t, &types.HubConfig{})
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,provider_id,status,created_at) VALUES('vm-error','tenant','vm','fake','vm-1','error',?)`, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			s.terminateVMOverride = func(_, _ string) error { return tc.err }
			s.terminateVMForClaw("vm-error", "fake", "vm-1")
			var got string
			if err := db.QueryRow(`SELECT provider_id FROM claws WHERE id='vm-error'`).Scan(&got); err != nil || got != tc.want {
				t.Fatalf("provider_id=%q err=%v, want %q", got, err, tc.want)
			}
		})
	}
}
