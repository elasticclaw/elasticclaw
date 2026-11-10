package workflowv2_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const branchDeliveryYAML = `
delivery:
  pull_requests:
    branches:
      - repository: api
        pattern: feature*
`

func TestAdoptPullRequestsOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, status, tenant, branch string
		want                         int
	}{
		{"finished", "completed", "tenant-1", "feature", 1},
		{"cancelled", "cancelled", "tenant-1", "feature", 1},
		{"active", "active", "tenant-1", "feature", 0},
		{"suspended", "suspended", "tenant-1", "feature", 0},
		{"other tenant", "completed", "tenant-2", "feature", 0},
		{"nonmatching", "completed", "tenant-1", "unrelated", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			previous := createDeliveryRun(t, store, "previous")
			url := "https://github.example/org/api/pull/10"
			delivered, err := store.SubmitDelivery(ctx, previous.RunID, previous.ID, typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: url}}}, deliveryVerifier(map[string]string{url: "head"}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET status=?,tenant_id=? WHERE id=?`, tc.status, tc.tenant, previous.RunID); err != nil {
				t.Fatal(err)
			}
			run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "new", TenantID: "tenant-1", InitialClawID: "new-claw", WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(strings.ReplaceAll(deliveryTransitionWorkflowYAML, "delivery.verified", "delivery.adopted") + branchDeliveryYAML)})
			if err != nil {
				t.Fatal(err)
			}
			pr := delivered[0]
			oldID := pr.ID
			pr.SourceBranch = tc.branch
			pr.Supersedes = "untrusted-link"
			adopted, err := store.AdoptPullRequests(ctx, run.ID, []typesv2.VerifiedPullRequest{pr, pr}, "")
			if err != nil || len(adopted) != tc.want {
				t.Fatalf("adopted=%v err=%v", adopted, err)
			}
			inspection, err := store.InspectRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			facts := inspection.Facts["delivery"].(map[string]interface{})
			if facts["adopted"] != (tc.want > 0) || facts["adopted_count"] != float64(tc.want) || facts["count"] != float64(tc.want) {
				t.Fatalf("facts=%v", facts)
			}
			var active bool
			if err := db.QueryRow(`SELECT active FROM workflow_v2_delivery_prs WHERE id=?`, oldID).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != (tc.want == 0) {
				t.Fatalf("old active=%t", active)
			}
			if tc.want > 0 {
				if adopted[0].Supersedes != oldID || adopted[0].ID == oldID || inspection.Run.State != "reviewing" {
					t.Fatalf("replacement=%+v state=%s", adopted[0], inspection.Run.State)
				}
				var registered bool
				if err := db.QueryRow(`SELECT registered_only FROM workflow_v2_delivery_prs WHERE id=?`, adopted[0].ID).Scan(&registered); err != nil || registered {
					t.Fatalf("registered=%t err=%v", registered, err)
				}
				targets, err := store.ActiveDeliveryTargetsAll(ctx)
				if err != nil || len(targets) != 1 || targets[0].RunID != run.ID {
					t.Fatalf("targets=%+v err=%v", targets, err)
				}
				// A later registration/submission must preserve the hub-owned lineage.
				if _, err := store.SubmitDelivery(ctx, run.ID, run.CurrentAttemptID, typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: url}}}, deliveryVerifier(map[string]string{url: "head"})); err != nil {
					t.Fatal(err)
				}
				var supersedes string
				if err := db.QueryRow(`SELECT supersedes_id FROM workflow_v2_delivery_prs WHERE id=?`, adopted[0].ID).Scan(&supersedes); err != nil || supersedes != oldID {
					t.Fatalf("supersedes=%s err=%v", supersedes, err)
				}
			}
			if _, err := store.AdoptPullRequests(ctx, run.ID, []typesv2.VerifiedPullRequest{pr}, ""); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND kind='delivery.adopted'`, run.ID).Scan(&count); err != nil || count != tc.want {
				t.Fatalf("events=%d err=%v", count, err)
			}
		})
	}
}

func TestBranchRegistrationPromotesDelivery(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(fmt.Sprint(control), func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "promote", TenantID: "tenant-1", InitialClawID: "claw", WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(strings.Replace(deliveryTransitionWorkflowYAML, "transitions:", "  unexpected:\n    phase: build\ntransitions:\n  claimed:\n    from: reviewing\n    on: pull_request.claimed\n    to: unexpected", 1) + branchDeliveryYAML)})
			if err != nil {
				t.Fatal(err)
			}
			url := "https://github.example/org/api/pull/10"
			other := "https://github.example/org/web/pull/20"
			verifier := deliveryVerifier(map[string]string{url: "head", other: "head"})
			manifest := typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: url}, {URL: other}}}
			payload, _ := json.Marshal(manifest)
			envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "claim", Kind: typesv2.MessagePullRequestClaimed, RunID: run.ID, AttemptID: run.CurrentAttemptID, ExpectedStateVersion: &run.StateVersion, Payload: payload}
			for i := 0; i < 2; i++ {
				if control {
					receipt, err := store.ApplyDeliveryControl(ctx, envelope, verifier)
					want := typesv2.DispositionAccepted
					if i > 0 {
						want = typesv2.DispositionDuplicate
					}
					if err != nil || receipt.Disposition != want {
						t.Fatalf("receipt=%+v err=%v", receipt, err)
					}
				} else {
					for _, claim := range manifest.PullRequests {
						if _, _, err := store.RegisterPullRequest(ctx, run.ID, run.CurrentAttemptID, claim, verifier); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			inspection, err := store.InspectRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if inspection.Run.State != "reviewing" || inspection.Delivery.ActivePullRequests != 1 || inspection.Facts["delivery"].(map[string]interface{})["count"] != float64(1) {
				t.Fatalf("inspection=%+v", inspection)
			}
			policy, err := store.EvaluateDeliveryPolicy(ctx, run.ID)
			if err != nil || policy.PullRequestCount != 1 {
				t.Fatalf("policy=%+v err=%v", policy, err)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND kind='delivery.verified'`, run.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("events=%d err=%v", count, err)
			}
		})
	}
}

func TestAdoptionRollbackAndLookupFailure(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	old := createDeliveryRun(t, store, "old")
	url := "https://github.example/org/api/pull/10"
	pr, _, err := store.RegisterPullRequest(ctx, old.RunID, old.ID, typesv2.PullRequestClaim{URL: url}, deliveryVerifier(map[string]string{url: "head"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='completed' WHERE id='old'`); err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "new", TenantID: "tenant-1", WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(deliveryTransitionWorkflowYAML + branchDeliveryYAML)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_adoption BEFORE INSERT ON workflow_v2_events WHEN NEW.kind='delivery.adopted' BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdoptPullRequests(ctx, run.ID, []typesv2.VerifiedPullRequest{pr}, ""); err == nil {
		t.Fatal("expected rollback")
	}
	var active bool
	if err := db.QueryRow(`SELECT active FROM workflow_v2_delivery_prs WHERE id=?`, pr.ID).Scan(&active); err != nil || !active {
		t.Fatalf("active=%t err=%v", active, err)
	}
	adopted, err := store.AdoptPullRequests(ctx, run.ID, []typesv2.VerifiedPullRequest{pr}, "GitHub unavailable")
	if err != nil || len(adopted) != 0 {
		t.Fatalf("adopted=%v err=%v", adopted, err)
	}
	var payload string
	if err := db.QueryRow(`SELECT payload_json FROM workflow_v2_events WHERE run_id=? AND kind='delivery.adoption_checked'`, run.ID).Scan(&payload); err != nil || !strings.Contains(payload, "GitHub unavailable") {
		t.Fatalf("payload=%s err=%v", payload, err)
	}
}

func TestBranchRegistrationSkipsPROwnedByActiveRun(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	owner := createDeliveryRun(t, store, "owner")
	url := "https://github.example/org/api/pull/10"
	verifier := deliveryVerifier(map[string]string{url: "head"})
	if _, err := store.SubmitDelivery(ctx, owner.RunID, owner.ID, typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: url}}}, verifier); err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "blocked", TenantID: "tenant-1", InitialClawID: "claw", WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(deliveryTransitionWorkflowYAML + branchDeliveryYAML)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RegisterPullRequest(ctx, run.ID, run.CurrentAttemptID, typesv2.PullRequestClaim{URL: url}, verifier); err != nil {
		t.Fatal(err)
	}
	var registeredOnly bool
	if err := db.QueryRow(`SELECT registered_only FROM workflow_v2_delivery_prs WHERE run_id=?`, run.ID).Scan(&registeredOnly); err != nil || !registeredOnly {
		t.Fatalf("registered_only=%t err=%v", registeredOnly, err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND kind='delivery.verified'`, run.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("events=%d err=%v", count, err)
	}
}

const adoptionInitialEffectWorkflowYAML = `
schema_version: 2
name: adoption-initial-effect
enabled: true
initial_state: working
states:
  working:
    phase: build
    on_enter:
      effects:
        - agent.task:
            prompt: Update dependencies.
  awaiting:
    phase: review
transitions:
  adopted:
    from: working
    on: delivery.adopted
    to: awaiting
` + branchDeliveryYAML

// Adoption during activation leaves the initial state, so its on_enter effects
// must not run in the destination state.
func TestAdoptionDuringActivationCancelsInitialEffects(t *testing.T) {
	for _, tc := range []struct {
		on    string
		adopt bool
	}{
		{"delivery.adopted", true}, {"delivery.adopted", false},
		{"workflow.delivery.evaluated", true}, {"workflow.delivery.evaluated", false},
	} {
		adopt := tc.adopt
		workflowYAML := strings.Replace(adoptionInitialEffectWorkflowYAML, "on: delivery.adopted", "on: "+tc.on, 1)
		t.Run(fmt.Sprintf("%s/%t", tc.on, adopt), func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			previous := createDeliveryRun(t, store, "previous")
			url := "https://github.example/org/api/pull/10"
			delivered, err := store.SubmitDelivery(ctx, previous.RunID, previous.ID, typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: url}}}, deliveryVerifier(map[string]string{url: "head"}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='completed' WHERE id=?`, previous.RunID); err != nil {
				t.Fatal(err)
			}
			run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "new", TenantID: "tenant-1", InitialClawID: "new-claw", ActivationPending: true,
				WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(workflowYAML)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AssembleOrganizationContext(ctx, run.ID, workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, _ string, _ typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
				return typesv2.ContextBundleSource{}, fmt.Errorf("unexpected knowledge resolution")
			})); err != nil {
				t.Fatal(err)
			}
			var candidates []typesv2.VerifiedPullRequest
			if adopt {
				candidates = delivered
			}
			if _, err := store.AdoptPullRequests(ctx, run.ID, candidates, ""); err != nil {
				t.Fatal(err)
			}
			if err := store.CompleteActivation(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			current, err := store.GetRun(ctx, run.ID)
			if err != nil || current.Status != workflowv2.RunActive {
				t.Fatalf("run=%+v err=%v", current, err)
			}
			claim, err := store.ClaimEffect(ctx, "worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if adopt {
				if current.State != "awaiting" || claim != nil {
					t.Fatalf("state=%s claim=%+v", current.State, claim)
				}
				var status string
				if err := db.QueryRow(`SELECT status FROM workflow_v2_effects WHERE run_id=? AND origin_type='initial'`, run.ID).Scan(&status); err != nil || status != "cancelled" {
					t.Fatalf("initial effect status=%s err=%v", status, err)
				}
				return
			}
			if current.State != "working" || claim == nil || claim.Effect.RunID != run.ID || claim.Effect.Kind != "agent.task" {
				t.Fatalf("state=%s claim=%+v", current.State, claim)
			}
		})
	}
}

const contextTransitionInitialEffectWorkflowYAML = `
schema_version: 2
name: context-initial-effect
enabled: true
initial_state: working
states:
  working:
    phase: build
    on_enter:
      effects:
        - agent.task:
            prompt: Update dependencies.
  awaiting:
    phase: review
transitions:
  context_ready:
    from: working
    on: context.bundle.ready
    to: awaiting
`

// Only adoption cancels initial effects; other activation-time transitions keep
// the pre-adoption behavior.
func TestContextTransitionDuringActivationKeepsInitialEffects(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "ctx", TenantID: "tenant-1", InitialClawID: "ctx-claw", ActivationPending: true,
		WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(contextTransitionInitialEffectWorkflowYAML)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssembleOrganizationContext(ctx, run.ID, workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, _ string, _ typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
		return typesv2.ContextBundleSource{}, fmt.Errorf("unexpected knowledge resolution")
	})); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteActivation(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetRun(ctx, run.ID)
	if err != nil || current.State != "awaiting" || current.Status != workflowv2.RunActive {
		t.Fatalf("run=%+v err=%v", current, err)
	}
	claim, err := store.ClaimEffect(ctx, "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.Effect.RunID != run.ID || claim.Effect.Kind != "agent.task" {
		t.Fatalf("claim=%+v", claim)
	}
}

// Promoting a registered branch PR during activation also leaves the initial
// state, so its on_enter effects must not run in the destination state.
func TestBranchPromotionDuringActivationCancelsInitialEffects(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "promote", TenantID: "tenant-1", InitialClawID: "promote-claw", ActivationPending: true,
		WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(strings.ReplaceAll(adoptionInitialEffectWorkflowYAML, "delivery.adopted", "delivery.verified"))})
	if err != nil {
		t.Fatal(err)
	}
	url := "https://github.example/org/api/pull/10"
	if _, _, err := store.RegisterPullRequest(ctx, run.ID, run.CurrentAttemptID, typesv2.PullRequestClaim{URL: url}, deliveryVerifier(map[string]string{url: "head"})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssembleOrganizationContext(ctx, run.ID, workflowv2.KnowledgeResolverFunc(func(_ context.Context, _ workflowv2.Run, _ string, _ typesv2.KnowledgeSource) (typesv2.ContextBundleSource, error) {
		return typesv2.ContextBundleSource{}, fmt.Errorf("unexpected knowledge resolution")
	})); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteActivation(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetRun(ctx, run.ID)
	if err != nil || current.State != "awaiting" || current.Status != workflowv2.RunActive {
		t.Fatalf("run=%+v err=%v", current, err)
	}
	claim, err := store.ClaimEffect(ctx, "worker", time.Minute)
	if err != nil || claim != nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_effects WHERE run_id=? AND origin_type='initial'`, run.ID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("initial effect status=%s err=%v", status, err)
	}
}
