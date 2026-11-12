package hub

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
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

// A slot freed while workflow v2 activation is still running (for example
// during the PR adoption lookup) must not provision the pending claw: the
// activation may finish the run without any sandbox.
func TestWorkflowCreationHoldsPendingClawDuringActivation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		activation  error
		failDequeue bool
		want        string
		wantRun     string
	}{
		// The run does not exist, so the parent finish exits early and the
		// creator's backstop deletes the claw and promotes the queue.
		{"finished", workflowV2FinishedAtActivationError{runID: "missing-run", status: "completed"}, false, "deleted", "success"},
		// A failed dequeue must not leave the claw pending for the next
		// promotion to provision, and records the run's terminal result.
		{"finished dequeue failed", workflowV2FinishedAtActivationError{status: "completed"}, true, "deleted", "success"},
		{"cancelled dequeue failed", workflowV2FinishedAtActivationError{status: "cancelled"}, true, "deleted", "cancelled"},
		{"activated", nil, false, "connected", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
			t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
			s, db := newReaperTestServer(t, &types.HubConfig{
				ConcurrencyGroups: []*types.ConcurrencyGroup{{Name: "cruise-demo", Limit: 1}},
				Providers:         map[string]types.ProviderConfig{"noop": {Type: "noop"}},
			})
			insertQueueTestClaw(t, db, "active001", "connected", "cruise-demo", time.Now().UTC())
			workspace := &types.WorkspaceConfig{Name: "queue-workspace", Files: map[string]string{"README.md": "Queue test workspace"}}
			workflow := &types.WorkflowConfig{Name: "queue-workflow", Provider: "noop", ConcurrencyGroup: "cruise-demo"}
			id, _, err := s.createClawFromWorkflowWithOptions(workspace, workflow, workflowCreateOptions{
				beforeProvision: func(_ context.Context, id, _ string) error {
					if _, err := db.Exec(`INSERT INTO workflow_runs (id, tenant_id, workflow_name, workspace_name, trigger_type, status, claw_id, started_at, created_at)
						VALUES ('queue-run', 'default', 'queue-workflow', 'queue-workspace', 'cron', 'running', ?, ?, ?)`, id, time.Now().UTC(), time.Now().UTC()); err != nil {
						t.Fatal(err)
					}
					if tc.failDequeue {
						if _, err := db.Exec(`CREATE TRIGGER fail_queue_test_dequeue BEFORE UPDATE OF status ON claws
							WHEN NEW.status='provisioning' BEGIN SELECT RAISE(FAIL, 'dequeue disabled'); END`); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := db.Exec(`UPDATE claws SET status='deleted' WHERE id='active001'`); err != nil {
						t.Fatal(err)
					}
					s.promotePendingClaws()
					if got := queueTestStatus(t, db, id); got != "pending" {
						t.Fatalf("claw status=%q during activation, want pending", got)
					}
					if tc.activation != nil && !tc.failDequeue {
						// Queued behind the finished claw's slot until it is deleted.
						insertQueueTestClaw(t, db, "next0001", "pending", "cruise-demo", time.Now().UTC())
					}
					return tc.activation
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "connected" {
				waitForQueueTestPromotion(t, db, id)
				return
			}
			// The claw already left 'pending' when the creator returned, and
			// promotion only selects pending claws, so it cannot provision it.
			if got := queueTestStatus(t, db, id); got != "provisioning" && got != "deleted" {
				t.Fatalf("finished claw status=%q, want it out of the pending queue", got)
			}
			s.promotePendingClaws()
			deadline := time.Now().Add(3 * time.Second)
			for {
				var status, providerID string
				if err := db.QueryRow(`SELECT status, COALESCE(provider_id,'') FROM claws WHERE id=?`, id).Scan(&status, &providerID); err != nil {
					t.Fatal(err)
				}
				if providerID != "" {
					t.Fatalf("finished claw was provisioned: status=%q provider=%q", status, providerID)
				}
				if status == tc.want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("finished claw status=%q, want %q", status, tc.want)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !tc.failDequeue {
				// Deleting the finished claw freed the slot it held.
				waitForQueueTestPromotion(t, db, "next0001")
			}
			var runStatus string
			if err := db.QueryRow(`SELECT status FROM workflow_runs WHERE id='queue-run'`).Scan(&runStatus); err != nil {
				t.Fatal(err)
			}
			if runStatus != tc.wantRun {
				t.Fatalf("workflow run status=%q, want %q", runStatus, tc.wantRun)
			}
		})
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

func TestFactoryCreationQueuesBehindOlderPendingClaw(t *testing.T) {
	shortcut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"name":"Queue story"}`))
	}))
	defer shortcut.Close()
	for _, tc := range []struct {
		name        string
		integration string
		create      func(s *Server, factory *types.FactoryConfig) error
	}{
		{"factory", "", func(s *Server, factory *types.FactoryConfig) error {
			_, pending, err := s.createClawFromFactory(factory, "", map[string]string{"task": "Queue"}, nil, "manual trigger")
			if err == nil && !pending {
				t.Error("createClawFromFactory reported the claw as not pending")
			}
			return err
		}},
		{"external", "", func(s *Server, factory *types.FactoryConfig) error {
			_, err := s.createClawForExternalEvent(factory, externalWebhookPayload{EventType: "released"}, "trigger-1")
			return err
		}},
		{"github-issue", "github-issues", func(s *Server, factory *types.FactoryConfig) error {
			var payload githubIssuesWebhookPayload
			payload.Repository.FullName = "acme/app"
			payload.Issue.Number = 7
			return s.createClawForGitHubIssue(factory, payload, "webhook")
		}},
		{"github-pr", "github", func(s *Server, factory *types.FactoryConfig) error {
			var payload githubPRPayload
			payload.Number = 8
			payload.Repository.FullName = "acme/app"
			payload.PullRequest.HTMLURL = "https://github.com/acme/app/pull/8"
			return s.createClawForGitHubPR(factory, payload, "webhook")
		}},
		{"shortcut", "shortcut", func(s *Server, factory *types.FactoryConfig) error {
			s.shortcutBaseURL = shortcut.URL
			return s.createClawForShortcutStory(factory, shortcutAction{ID: 1, Name: "Queue story"}, "1", "token", "webhook")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
			t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
			factory := taskRunLifecycleFactoryConfig("queue-factory", tc.integration)
			factory.ConcurrencyGroup = "cruise-demo"
			// newReaperTestServer keeps the default connection pool: promotion
			// nests queries, which deadlocks under the single-connection helper.
			s, db := newReaperTestServer(t, &types.HubConfig{
				ConcurrencyGroups: []*types.ConcurrencyGroup{{Name: "cruise-demo", Limit: 3}},
				Providers:         map[string]types.ProviderConfig{"noop": {Type: "noop"}},
				Factories:         []*types.FactoryConfig{factory},
			})
			created := time.Now().UTC()
			insertQueueTestClaw(t, db, "active001", "connected", "cruise-demo", created)
			insertQueueTestClaw(t, db, "active002", "connected", "cruise-demo", created)
			insertQueueTestClaw(t, db, "oldest001", "pending", "cruise-demo", created.Add(-time.Minute))
			if err := tc.create(s, factory); err != nil {
				t.Fatal(err)
			}
			var id string
			if err := db.QueryRow(`SELECT id FROM claws WHERE id NOT IN ('active001','active002','oldest001')`).Scan(&id); err != nil {
				t.Fatalf("find new claw: %v", err)
			}
			waitForQueueTestPromotion(t, db, "oldest001")
			if got := queueTestStatus(t, db, id); got != "pending" {
				t.Fatalf("new claw status=%q, want pending behind the older claw", got)
			}
		})
	}
}

func TestWritableVolumeLeaseHoldsSlotUntilReleased(t *testing.T) {
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := newReaperTestServer(t, &types.HubConfig{ConcurrencyGroups: []*types.ConcurrencyGroup{{Name: "cruise-demo", Limit: 1}}})
	now := time.Now().UTC()
	insertQueueTestClaw(t, db, "syncing01", "deleted", "cruise-demo", now)
	insertQueueTestClaw(t, db, "stale0001", "deleted", "cruise-demo", now)
	insertQueueTestClaw(t, db, "pending01", "pending", "cruise-demo", now.Add(-time.Minute))
	insertLease := func(id, clawID string, heartbeat time.Time) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO volume_leases(id, volume_id, repo, tag, claw_id, access_token, mode, mount, manifest_digest, acquired_at, expires_at, heartbeat_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, "volumes/tenant/"+id, "volumes/tenant/"+id, "latest", clawID, "token", volumeModeRW, "/data", "sha256:x", heartbeat, now.Add(volumeLeaseTTL), heartbeat); err != nil {
			t.Fatal(err)
		}
	}
	// A lease whose holder stopped heartbeating long ago must not hold the slot.
	insertLease("stale", "stale0001", now.Add(-time.Hour))
	insertLease("live", "syncing01", now)

	// The deleted claw is still syncing its writable volume: the slot stays taken.
	s.promotePendingClaws()
	if got := queueTestStatus(t, db, "pending01"); got != "pending" {
		t.Fatalf("pending claw status=%q, want pending while the writable lease is held", got)
	}
	s.releaseWorkflowVolumeLeases("syncing01")
	s.promotePendingClaws()
	waitForQueueTestPromotion(t, db, "pending01")
}
