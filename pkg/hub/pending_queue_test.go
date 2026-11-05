package hub

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func insertQueueTestClaw(t *testing.T, db *sql.DB, id, status, group string, created time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,provider,status,concurrency_group,created_at) VALUES(?,'tenant',?,'noop',?,?,?)`, id, id, status, group, created); err != nil {
		t.Fatal(err)
	}
}

func queueTestStatus(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func waitForQueueTestPromotion(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		status := queueTestStatus(t, db, id)
		// Wait for the noop provider to finish as well, so no provisioning
		// goroutine is left using the database after test cleanup.
		if status == "connected" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("claw %s status=%q, want connected after promotion", id, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPipelineTerminalStagePromotesOldestPendingClaw(t *testing.T) {
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	for _, stage := range []string{"finished", "blocked"} {
		t.Run(stage, func(t *testing.T) {
			s, db := newReaperTestServer(t, &types.HubConfig{ConcurrencyGroups: []*types.ConcurrencyGroup{{Name: "cruise-demo", Limit: 1}}})
			// Keep this queue test independent of checkpoint filesystem storage.
			if _, err := db.Exec(`CREATE TRIGGER disable_queue_test_checkpoints BEFORE INSERT ON claw_checkpoints BEGIN SELECT RAISE(FAIL, 'checkpoints disabled for queue test'); END`); err != nil {
				t.Fatal(err)
			}
			created := time.Now().UTC()
			insertQueueTestClaw(t, db, "active001", "connected", "cruise-demo", created)
			insertQueueTestClaw(t, db, "oldest001", "pending", "cruise-demo", created.Add(-2*time.Minute))
			insertQueueTestClaw(t, db, "newest001", "pending", "cruise-demo", created.Add(-time.Minute))
			if !s.transitionPipelineStageWithContext("active001", pipeline.Stage{ID: stage, Label: stage, Terminal: true}, pipelineContext{}) {
				t.Fatal("terminal stage transition was not applied")
			}
			waitForQueueTestPromotion(t, db, "oldest001")
			if got := queueTestStatus(t, db, "active001"); got != "deleted" {
				t.Fatalf("terminal claw status=%q, want deleted", got)
			}
			if got := queueTestStatus(t, db, "newest001"); got != "pending" {
				t.Fatalf("newest claw status=%q, want pending", got)
			}
		})
	}
}

func TestWorkflowCreationQueuesBehindOlderPendingClaw(t *testing.T) {
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := newReaperTestServer(t, &types.HubConfig{
		ConcurrencyGroups: []*types.ConcurrencyGroup{{Name: "cruise-demo", Limit: 3}},
		Providers:         map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	})
	created := time.Now().UTC()
	insertQueueTestClaw(t, db, "active001", "connected", "cruise-demo", created)
	insertQueueTestClaw(t, db, "active002", "connected", "cruise-demo", created)
	insertQueueTestClaw(t, db, "oldest001", "pending", "cruise-demo", created.Add(-time.Minute))
	workspace := &types.WorkspaceConfig{Name: "queue-workspace", Files: map[string]string{"README.md": "Queue test workspace"}}
	workflow := &types.WorkflowConfig{Name: "queue-workflow", Provider: "noop", ConcurrencyGroup: "cruise-demo"}
	id, pending, err := s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{
		beforeProvision: func(_ context.Context, id, _ string) error {
			if got := queueTestStatus(t, db, id); got != "pending" {
				t.Fatalf("inserted claw status=%q, want pending", got)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("new claw bypassed older pending work despite the queue")
	}
	waitForQueueTestPromotion(t, db, "oldest001")
	if got := queueTestStatus(t, db, id); got != "pending" {
		t.Fatalf("new claw status=%q, want pending", got)
	}
	if got := s.countActiveClawsInGroup("cruise-demo"); got != 3 {
		t.Fatalf("active claws=%d, want 3", got)
	}
}

func TestReaperPromotesPendingClawWithoutOtherActions(t *testing.T) {
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := newReaperTestServer(t, &types.HubConfig{MaxConcurrentClaws: 1})
	insertQueueTestClaw(t, db, "pending01", "pending", "global", time.Now().UTC())
	s.reapOnce()
	waitForQueueTestPromotion(t, db, "pending01")
}

func TestHasPendingInGroupIncludesLegacyGlobalRows(t *testing.T) {
	s, db := newReaperTestServer(t, &types.HubConfig{})
	insertQueueTestClaw(t, db, "legacy001", "pending", "", time.Now().UTC())
	for _, group := range []string{"", "global"} {
		if !s.hasPendingInGroup(group) {
			t.Errorf("legacy pending claw missing from group %q", group)
		}
	}
	if s.hasPendingInGroup("cruise-demo") {
		t.Fatal("global pending claw leaked into another group")
	}
	insertQueueTestClaw(t, db, "named0001", "pending", "cruise-demo", time.Now().UTC())
	if !s.hasPendingInGroup("cruise-demo") {
		t.Fatal("named group's pending claw was not found")
	}
}
