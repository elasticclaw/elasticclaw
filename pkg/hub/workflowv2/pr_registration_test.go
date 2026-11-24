package workflowv2_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestRegisterPullRequestDoesNotDeliverAndSubmissionPromotes(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(fmt.Sprintf("control=%t", control), func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
				ID: "registered", TenantID: "tenant-1", InitialClawID: "claw-registered",
				WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(deliveryTransitionWorkflowYAML),
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.InspectRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			url := "https://github.example/org/api/pull/10"
			manifest := typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: url}}}
			payload, _ := json.Marshal(manifest)
			envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
				MessageID: "claim", Kind: typesv2.MessagePullRequestClaimed, RunID: run.ID,
				AttemptID: run.CurrentAttemptID, ExpectedStateVersion: &run.StateVersion, Payload: payload}
			verifier := deliveryVerifier(map[string]string{url: "head-1"})
			for i := 0; i < 2; i++ {
				if control {
					receipt, err := store.ApplyDeliveryControl(ctx, envelope, verifier)
					want := typesv2.DispositionAccepted
					if i == 1 {
						want = typesv2.DispositionDuplicate
					}
					if err != nil || receipt.Disposition != want {
						t.Fatalf("claim: %#v, %v", receipt, err)
					}
				} else {
					_, inserted, err := store.RegisterPullRequest(ctx, run.ID, run.CurrentAttemptID, manifest.PullRequests[0], verifier)
					if err != nil || inserted != (i == 0) {
						t.Fatalf("register: inserted=%t, %v", inserted, err)
					}
				}
			}
			var id string
			var registered, count int
			if err := db.QueryRow(`SELECT id,registered_only,COUNT(*) FROM workflow_v2_delivery_prs WHERE run_id=?`, run.ID).Scan(&id, &registered, &count); err != nil {
				t.Fatal(err)
			}
			if registered != 1 || count != 1 {
				t.Fatalf("registered=%d count=%d", registered, count)
			}
			after, err := store.InspectRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Facts, after.Facts) || after.Run.State != run.State || after.Run.StateVersion != run.StateVersion || after.Delivery.ActivePullRequests != 0 {
				t.Fatalf("registration changed delivery or run: %#v", after)
			}
			var events int
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND (kind='delivery.verified' OR kind LIKE 'delivery.policy.%')`, run.ID).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if events != 0 {
				t.Fatalf("emitted %d delivery events", events)
			}
			policy, err := store.EvaluateDeliveryPolicy(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if policy.PullRequestCount != 0 {
				t.Fatalf("policy includes registered PR: %#v", policy)
			}
			snapshot, err := store.Snapshot(ctx, run.ID, run.CurrentAttemptID)
			if err != nil || len(snapshot.Delivery) != 0 {
				t.Fatalf("snapshot: %#v, %v", snapshot.Delivery, err)
			}
			all, err := store.ActiveDeliveryTargetsAll(ctx)
			if err != nil || len(all) != 0 {
				t.Fatalf("periodic targets: %#v, %v", all, err)
			}
			byPR, err := store.ActiveDeliveryTargets(ctx, "org/api", 10)
			if err != nil || len(byPR) != 0 {
				t.Fatalf("webhook targets: %#v, %v", byPR, err)
			}
			byHead, err := store.ActiveDeliveryTargetsByHead(ctx, "head-1")
			if err != nil || len(byHead) != 0 {
				t.Fatalf("check targets: %#v, %v", byHead, err)
			}

			// A merged, registered-only PR cannot inflate delivery facts or policy.
			otherURL := "https://github.example/org/web/pull/20"
			mergedVerifier := workflowv2.PullRequestVerifierFunc(func(ctx context.Context, r workflowv2.Run, ws *typesv2.Workspace, c typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
				pr, err := deliveryVerifier(map[string]string{otherURL: "other-head"}).VerifyPullRequest(ctx, r, ws, c)
				pr.State = "merged"
				return pr, err
			})
			if _, _, err := store.RegisterPullRequest(ctx, run.ID, run.CurrentAttemptID, typesv2.PullRequestClaim{URL: otherURL}, mergedVerifier); err != nil {
				t.Fatal(err)
			}
			envelope.Kind, envelope.MessageID = typesv2.MessageDeliverySubmitted, "submit"
			receipt, err := store.ApplyDeliveryControl(ctx, envelope, verifier)
			if err != nil || receipt.Disposition != typesv2.DispositionAccepted {
				t.Fatalf("submit: %#v, %v", receipt, err)
			}
			if err := db.QueryRow(`SELECT registered_only FROM workflow_v2_delivery_prs WHERE id=?`, id).Scan(&registered); err != nil {
				t.Fatal(err)
			}
			if registered != 0 {
				t.Fatal("submission did not promote original row")
			}
			after, err = store.InspectRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Run.State != "reviewing" || after.Delivery.ActivePullRequests != 1 {
				t.Fatalf("promotion: %#v", after)
			}
			facts := after.Facts["delivery"].(map[string]interface{})
			if facts["count"] != float64(1) || facts["open"] != float64(1) || facts["merged"] != float64(0) || facts["all_merged"] != false {
				t.Fatalf("facts: %#v", facts)
			}
			policy, err = store.EvaluateDeliveryPolicy(ctx, run.ID)
			if err != nil || policy.PullRequestCount != 1 {
				t.Fatalf("policy: %#v, %v", policy, err)
			}
			// A delayed creation report must not downgrade or amend delivery.
			if _, inserted, err := store.RegisterPullRequest(ctx, run.ID, run.CurrentAttemptID, manifest.PullRequests[0], deliveryVerifier(map[string]string{url: "late-head"})); err != nil || inserted {
				t.Fatalf("late report: %t, %v", inserted, err)
			}
			var head string
			if err := db.QueryRow(`SELECT registered_only,current_head_sha FROM workflow_v2_delivery_prs WHERE id=?`, id).Scan(&registered, &head); err != nil {
				t.Fatal(err)
			}
			if registered != 0 || head != "head-1" {
				t.Fatalf("late report changed delivery: %d %s", registered, head)
			}
		})
	}
}

func TestRegisterPullRequestRechecksAttemptAfterVerification(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "register-revoked")
	url := "https://github.example/org/api/pull/10"
	verifier := workflowv2.PullRequestVerifierFunc(func(ctx context.Context, run workflowv2.Run, ws *typesv2.Workspace, claim typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		if _, err := store.StartAttempt(ctx, run.ID, "replacement-claw"); err != nil {
			t.Fatal(err)
		}
		return deliveryVerifier(map[string]string{url: "head"}).VerifyPullRequest(ctx, run, ws, claim)
	})
	if _, _, err := store.RegisterPullRequest(ctx, attempt.RunID, attempt.ID, typesv2.PullRequestClaim{URL: url}, verifier); err == nil {
		t.Fatal("accepted revoked attempt")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_prs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("revoked attempt inserted PR")
	}
}

func TestRegisterPullRequestRejectsUnverifiedClaims(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "register-invalid")
	for _, claim := range []typesv2.PullRequestClaim{
		{URL: "invalid"}, {URL: "https://github.example/org/unknown/pull/1"},
		{URL: "https://github.example/org/api/pull/1", Supersedes: "old"},
	} {
		if _, _, err := store.RegisterPullRequest(ctx, attempt.RunID, attempt.ID, claim, deliveryVerifier(map[string]string{claim.URL: "head"})); err == nil {
			t.Fatalf("accepted %#v", claim)
		}
	}
	if _, _, err := store.RegisterPullRequest(ctx, attempt.RunID, attempt.ID, typesv2.PullRequestClaim{URL: "https://github.example/org/api/pull/1"}, nil); err == nil {
		t.Fatal("accepted without verifier")
	}
}

func TestRegisterPullRequestMigrationPreservesSubmittedRows(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "register-migration")
	url := "https://github.example/org/api/pull/1"
	if _, err := store.SubmitDelivery(ctx, attempt.RunID, attempt.ID, typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: url}}}, deliveryVerifier(map[string]string{url: "head"})); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE workflow_v2_delivery_prs DROP COLUMN registered_only`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := workflowv2.Migrate(db); err != nil {
			t.Fatal(err)
		}
		var registered int
		if err := db.QueryRow(`SELECT registered_only FROM workflow_v2_delivery_prs`).Scan(&registered); err != nil {
			t.Fatal(err)
		}
		if registered != 0 {
			t.Fatal("migration demoted submitted PR")
		}
	}
}

func TestPullRequestClaimedGuardsStateAndDeduplicates(t *testing.T) {
	for _, changeDuringVerification := range []bool{false, true} {
		t.Run(fmt.Sprintf("change_during_verification=%t", changeDuringVerification), func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			attempt := createDeliveryRun(t, store, "claim-stale")
			version := uint64(0)
			if changeDuringVerification {
				version = 1
			}
			envelope := typesv2.ControlEnvelope{ProtocolVersion: typesv2.ControlProtocolVersion,
				MessageID: "claim-stale", Kind: typesv2.MessagePullRequestClaimed,
				RunID: attempt.RunID, AttemptID: attempt.ID, ExpectedStateVersion: &version,
				Payload: []byte(`{"pull_requests":[{"url":"https://github.example/org/api/pull/10"}]}`)}
			calls := 0
			verifier := workflowv2.PullRequestVerifierFunc(func(ctx context.Context, run workflowv2.Run, ws *typesv2.Workspace, claim typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
				calls++
				if _, err := db.Exec(`UPDATE workflow_v2_runs SET state_version=state_version+1 WHERE id=?`, run.ID); err != nil {
					t.Fatal(err)
				}
				return deliveryVerifier(map[string]string{claim.URL: "head"}).VerifyPullRequest(ctx, run, ws, claim)
			})
			receipt, err := store.ApplyDeliveryControl(ctx, envelope, verifier)
			if err != nil || receipt.Disposition != typesv2.DispositionStaleState {
				t.Fatalf("stale claim: %#v, %v", receipt, err)
			}
			receipt, err = store.ApplyDeliveryControl(ctx, envelope, verifier)
			if err != nil || receipt.Disposition != typesv2.DispositionDuplicate {
				t.Fatalf("duplicate claim: %#v, %v", receipt, err)
			}
			wantCalls := 0
			if changeDuringVerification {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("verification calls=%d, want %d", calls, wantCalls)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_prs`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("stale claim mutated PR collection")
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE kind='delivery.verified'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("claim recorded delivery.verified")
			}
		})
	}
}

func TestRegisteredPullRequestCannotBeReconciledDirectly(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "register-reconcile")
	url := "https://github.example/org/api/pull/10"
	pr, _, err := store.RegisterPullRequest(ctx, attempt.RunID, attempt.ID, typesv2.PullRequestClaim{URL: url}, deliveryVerifier(map[string]string{url: "head"}))
	if err != nil {
		t.Fatal(err)
	}
	target := workflowv2.DeliveryTarget{RunID: attempt.RunID, AttemptID: attempt.ID, PRID: pr.ID,
		URL: url, Repository: pr.Repository, RepositoryName: pr.RepositoryName, Number: pr.Number,
		HeadSHA: pr.HeadSHA, State: pr.State, WorkspaceYAML: deliveryWorkspaceYAML, WorkflowYAML: runtimeWorkflowYAML}
	pr.State = "merged"
	if _, err := store.ReconcilePullRequest(ctx, target, "registered-merge", "pull_request.merged", pr); err == nil {
		t.Fatal("registered-only PR accepted merge reconciliation")
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM workflow_v2_delivery_prs WHERE id=?`, pr.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "open" {
		t.Fatal("reconciliation mutated registered PR")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE id='registered-merge'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("reconciliation emitted registered PR merge event")
	}
}
