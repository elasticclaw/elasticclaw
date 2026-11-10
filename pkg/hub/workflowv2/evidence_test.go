package workflowv2_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const policyWorkspaceYAML = `
schema_version: 2
name: engineering
repositories:
  api:
    provider: github
    repository: org/api
source_control:
  connections:
    github:
      provider: github
ci:
  connections:
    github-actions:
      provider: github
      source_control: github
  pipelines:
    github-pr:
      connection: github-actions
      repository: api
      workflow: ci.yml
review_systems:
  connections:
    github-reviews:
      provider: github
      source_control: github
`

const policyWorkflowYAML = `
schema_version: 2
name: delivery-policy
enabled: true
initial_state: reviewing
states:
  reviewing:
    phase: review
  fixing:
    phase: build
    on_enter:
      effects:
        - agent.task:
            prompt: Address the trusted review feedback.
            include_facts: [review.feedback]
transitions:
  ci_failed:
    from: reviewing
    on: ci.policy.evaluated
    when:
      ci:
        status:
          equals: unsatisfied
    to: fixing
  review_failed:
    from: reviewing
    on: review.policy.evaluated
    when:
      review:
        status:
          equals: unsatisfied
    to: fixing
ci:
  policies:
    merge-ready:
      all:
        - pipeline: github-pr
          checks: [lint, unit]
      satisfied_for: current_pr_head
review:
  policies:
    required-review:
      all:
        - connection: github-reviews
          approvals:
            minimum: 1
      invalidate_on_new_head: true
delivery:
  pull_requests:
    required: true
    minimum: 1
    ci_policy: merge-ready
    review_policy: required-review
    completion: all_merged
`

func createPolicyDelivery(t *testing.T, store *workflowv2.Store, runID, head, state string) (workflowv2.Attempt, typesv2.VerifiedPullRequest) {
	t.Helper()
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: runID, TenantID: "tenant-1", WorkspaceYAML: []byte(policyWorkspaceYAML), WorkflowYAML: []byte(policyWorkflowYAML),
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(context.Background(), runID, "claw-policy")
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.example/org/api/pull/10"
	verified, err := store.SubmitDelivery(context.Background(), runID, attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: prURL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
		return typesv2.VerifiedPullRequest{URL: prURL, RepositoryName: "api", Repository: "org/api", Number: 10,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: head, State: state, VerifiedAt: now,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return attempt, verified[0]
}

func recordPolicyEvidence(t *testing.T, store *workflowv2.Store, runID string, pr typesv2.VerifiedPullRequest,
	domain, connection, externalID, kind, status string, payload map[string]interface{}, producer workflowv2.Producer) workflowv2.DeliveryPolicyResult {
	t.Helper()
	now := time.Date(2026, 8, 8, 12, 1, 0, 0, time.UTC)
	result, err := store.RecordEvidence(context.Background(), workflowv2.EvidenceInput{
		RunID: runID, PRID: pr.ID, HeadSHA: pr.HeadSHA, Domain: domain, Connection: connection,
		ExternalID: externalID, Kind: kind, Status: status, Payload: payload, ObservedAt: now,
		Provenance: typesv2.EvidenceProvenance{Producer: string(producer), Connection: connection, ObservedAt: now},
	}, producer)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHeadBoundCIAndReviewPoliciesRequireAllEvidence(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-policy", "head-1", "open")

	result := recordPolicyEvidence(t, store, "run-policy", pr, "ci", "github-actions", "lint-run", "lint", "success",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	if result.CISatisfied || result.CIStatus != "pending" || result.ReviewSatisfied || result.ReviewStatus != "pending" || result.Satisfied {
		t.Fatalf("policy satisfied too early: %#v", result)
	}
	result = recordPolicyEvidence(t, store, "run-policy", pr, "ci", "github-actions", "unit-run", "unit", "success",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	if !result.CISatisfied || result.CIStatus != "satisfied" || result.ReviewSatisfied || result.ReviewStatus != "pending" || result.Satisfied {
		t.Fatalf("CI result = %#v", result)
	}
	result = recordPolicyEvidence(t, store, "run-policy", pr, "review", "github-reviews", "alice", "approval", "approved",
		nil, workflowv2.ProducerReview)
	if !result.CISatisfied || !result.ReviewSatisfied || result.Satisfied || result.AllMerged {
		t.Fatalf("review result = %#v", result)
	}
	inspection, err := store.InspectRun(context.Background(), "run-policy")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.State != "reviewing" {
		t.Fatalf("satisfied review should not take failure edge: %#v", inspection.Run)
	}
}

func TestUnsatisfiedReviewLoopsBackToBuild(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-review-loop", "head-1", "open")
	recordPolicyEvidence(t, store, "run-review-loop", pr, "review", "github-reviews", "alice", "approval", "changes_requested",
		nil, workflowv2.ProducerReview)
	run, err := store.GetRun(context.Background(), "run-review-loop")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "fixing" || run.DisplayPhase != typesv2.PhaseBuild || run.StateVersion != 2 {
		t.Fatalf("run = %#v", run)
	}
}

func TestTypedReviewFeedbackIsIncludedInFixTask(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt, pr := createPolicyDelivery(t, store, "run-review-feedback", "head-1", "open")
	feedback := map[string]interface{}{"author": "alice", "body": "Handle the nil response before dereferencing it.",
		"url": "https://github.example/org/api/pull/10#discussion_r1"}
	observed := time.Date(2026, 8, 8, 12, 10, 0, 0, time.UTC)
	target := workflowv2.DeliveryTarget{
		RunID: "run-review-feedback", AttemptID: attempt.ID, PRID: pr.ID, Repository: pr.Repository,
		RepositoryName: pr.RepositoryName, Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA,
	}
	if _, err := store.ApplyReviewFeedback(context.Background(), target, "feedback-1", feedback, typesv2.EvidenceProvenance{
		Producer: string(workflowv2.ProducerReview), Principal: "alice", ObservedAt: observed,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyReviewFeedback(context.Background(), target, "feedback-older",
		map[string]interface{}{"body": "obsolete feedback"}, typesv2.EvidenceProvenance{
			Producer: string(workflowv2.ProducerReview), Principal: "alice", ObservedAt: observed.Add(-time.Minute),
		}); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("out-of-order feedback error = %v", err)
	}
	recordPolicyEvidence(t, store, "run-review-feedback", pr, "review", "github-reviews", "alice", "approval",
		"changes_requested", nil, workflowv2.ProducerReview)
	claim, err := store.ClaimEffect(context.Background(), "feedback-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	task, err := store.MaterializeAgentTask(context.Background(), claim.Effect.ID, claim.AttemptID,
		"feedback-worker", time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.Instructions, "Handle the nil response") ||
		!strings.Contains(task.Instructions, `"review.feedback"`) {
		t.Fatalf("task instructions = %s", task.Instructions)
	}
}

func TestEvidenceForOldHeadIsRejectedAndInvalidated(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt, pr := createPolicyDelivery(t, store, "run-stale-evidence", "head-1", "open")
	recordPolicyEvidence(t, store, "run-stale-evidence", pr, "ci", "github-actions", "lint-run", "lint", "success",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	initial := recordPolicyEvidence(t, store, "run-stale-evidence", pr, "ci", "github-actions", "unit-run", "unit", "success",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	if !initial.CISatisfied {
		t.Fatalf("initial head CI = %#v", initial)
	}
	prURL := pr.URL
	now := time.Date(2026, 8, 8, 12, 5, 0, 0, time.UTC)
	updated, err := store.SubmitDelivery(context.Background(), "run-stale-evidence", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: prURL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return typesv2.VerifiedPullRequest{URL: prURL, RepositoryName: "api", Repository: "org/api", Number: 10,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-2", State: "open", VerifiedAt: now,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordEvidence(context.Background(), workflowv2.EvidenceInput{
		RunID: "run-stale-evidence", PRID: pr.ID, HeadSHA: "head-1", Domain: "ci", Connection: "github-actions",
		ExternalID: "late", Kind: "unit", Status: "success",
		Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerCI)},
	}, workflowv2.ProducerCI); err == nil {
		t.Fatal("stale-head evidence was accepted")
	}
	result, err := store.EvaluateDeliveryPolicy(context.Background(), "run-stale-evidence")
	if err != nil {
		t.Fatal(err)
	}
	if result.CISatisfied {
		t.Fatalf("new head inherited old CI evidence: %#v; updated=%#v", result, updated)
	}
	returned, err := store.SubmitDelivery(context.Background(), "run-stale-evidence", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: prURL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return typesv2.VerifiedPullRequest{URL: prURL, RepositoryName: "api", Repository: "org/api", Number: 10,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: now.Add(time.Minute),
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now.Add(time.Minute)}}, nil
	}))
	if err != nil {
		t.Fatalf("return to prior head: %v", err)
	}
	result, err = store.EvaluateDeliveryPolicy(context.Background(), "run-stale-evidence")
	if err != nil {
		t.Fatal(err)
	}
	if result.CISatisfied {
		t.Fatalf("returned head inherited evidence from its old generation: %#v; returned=%#v", result, returned)
	}
	var generations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_heads WHERE pr_id=?`, pr.ID).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if generations != 3 {
		t.Fatalf("head generations = %d, want 3", generations)
	}
}

func TestTrustedPullRequestReconciliationInvalidatesOldHeadEvidence(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-pr-reconcile", "head-1", "open")
	recordPolicyEvidence(t, store, "run-pr-reconcile", pr, "ci", "github-actions", "lint-1", "lint", "success",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	targets, err := store.ActiveDeliveryTargets(context.Background(), "org/api", 10)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
	observed := time.Date(2026, 8, 8, 12, 6, 0, 0, time.UTC)
	reconciled := typesv2.VerifiedPullRequest{
		ID: pr.ID, URL: pr.URL, RepositoryName: "api", Repository: "org/api", Number: 10,
		SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-2", State: "open", VerifiedAt: observed,
		Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl),
			Connection: "github", ObservedAt: observed},
	}
	event, err := store.ReconcilePullRequest(context.Background(), targets[0], "github-sync-1",
		"pull_request.head_changed", reconciled)
	if err != nil {
		t.Fatal(err)
	}
	if event.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("event = %#v", event)
	}
	var head string
	var generations, currentEvidence, supersededEvidence int
	if err := db.QueryRow(`SELECT current_head_sha FROM workflow_v2_delivery_prs WHERE id=?`, pr.ID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_heads WHERE pr_id=?`, pr.ID).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE pr_id=? AND superseded_at=0`, pr.ID).Scan(&currentEvidence); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE pr_id=? AND superseded_at>0`, pr.ID).Scan(&supersededEvidence); err != nil {
		t.Fatal(err)
	}
	if head != "head-2" || generations != 2 || currentEvidence != 0 || supersededEvidence != 1 {
		t.Fatalf("head/generations/current/superseded = %q/%d/%d/%d", head, generations, currentEvidence, supersededEvidence)
	}
}

func TestSuspendedRunsReceiveExternalDeliveryEvidence(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-suspended-target", "head-1", "open")
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review'
		WHERE id='run-suspended-target'`); err != nil {
		t.Fatal(err)
	}
	byPR, err := store.ActiveDeliveryTargets(context.Background(), pr.Repository, pr.Number)
	if err != nil {
		t.Fatal(err)
	}
	byHead, err := store.ActiveDeliveryTargetsByHead(context.Background(), pr.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	all, err := store.ActiveDeliveryTargetsAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(byPR) != 1 || len(byHead) != 1 || len(all) != 1 {
		t.Fatalf("suspended targets by PR/head/all = %#v/%#v/%#v", byPR, byHead, all)
	}
	pr.BaseSHA, pr.MergeableState = "base-1", "dirty"
	if _, err := store.ReconcileMergeability(context.Background(), byPR[0], pr); err != nil {
		t.Fatal(err)
	}
	pr.HeadSHA = "head-2"
	if _, err := store.ReconcilePullRequest(context.Background(), byPR[0], "suspended-head", "pull_request.head_changed", pr); err != nil {
		t.Fatal(err)
	}
	recordPolicyEvidence(t, store, "run-suspended-target", pr, "ci", "github-actions", "lint", "lint", "failure",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	inspection, err := store.InspectRun(context.Background(), "run-suspended-target")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.Status != workflowv2.RunSuspended || inspection.Run.State != "reviewing" ||
		inspection.Run.WaitingReason != "operator review" || inspection.Run.StateVersion != 1 {
		t.Fatalf("evidence changed suspension: %#v", inspection.Run)
	}
	if prFacts := inspection.Facts["pull_request"].(map[string]interface{}); prFacts["head_sha"] != "head-2" {
		t.Fatalf("PR facts = %#v", prFacts)
	}
	// ci.policy.evaluated drives a transition: its facts update now and the
	// transition is deferred until recovery.
	if ci, ok := inspection.Facts["ci"].(map[string]interface{}); !ok || ci["status"] != "unsatisfied" {
		t.Fatalf("listened CI policy facts not updated while suspended: %#v", inspection.Facts["ci"])
	}
	// Deferring the CI transition must not leave delivery facts behind the
	// CI facts they summarize.
	ci := inspection.Facts["ci"].(map[string]interface{})
	delivery := inspection.Facts["delivery"].(map[string]interface{})
	if delivery["policy_revision"] != ci["policy_revision"] || delivery["ci_satisfied"] != false || delivery["satisfied"] != false {
		t.Fatalf("delivery facts stale while suspended: ci %#v delivery %#v", ci, delivery)
	}
	var receipts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_event_receipts WHERE run_id=? AND disposition='accepted'`, "run-suspended-target").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts == 0 {
		t.Fatal("missing accepted receipts")
	}
	// After recovery the reconciler sweep replays the deferred policy event
	// and fires the transition exactly once.
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='active',waiting_reason='' WHERE id='run-suspended-target'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.ReplayDeferredEvents(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run, err := store.GetRun(context.Background(), "run-suspended-target")
	if err != nil || run.State != "fixing" || run.StateVersion != 2 {
		t.Fatalf("recovered run = %#v, %v", run, err)
	}
}

func TestSuspendedRunDefersMergeUntilRecovery(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"  done:\n    phase: done\n    terminal: true\ntransitions:\n  merged:\n    from: reviewing\n    on: pull_request.merged\n    to: done\n", 1)
	workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	if _, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
		ID: "run-suspended-merge", TenantID: "tenant-1", WorkspaceYAML: []byte(policyWorkspaceYAML), WorkflowYAML: []byte(workflowYAML),
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(ctx, "run-suspended-merge", "claw-policy")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	pr := typesv2.VerifiedPullRequest{URL: "https://github.example/org/api/pull/10", RepositoryName: "api", Repository: "org/api",
		Number: 10, SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: now,
		Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}
	if _, err := store.SubmitDelivery(ctx, "run-suspended-merge", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: pr.URL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return pr, nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review' WHERE id='run-suspended-merge'`); err != nil {
		t.Fatal(err)
	}
	targets, err := store.ActiveDeliveryTargetsAll(ctx)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
	pr.State = "merged"
	result, err := store.ReconcilePullRequest(ctx, targets[0], "merge-event", "pull_request.merged", pr)
	if err != nil || !result.Deferred || result.Disposition != typesv2.DispositionAccepted || result.Run.State != "reviewing" {
		t.Fatalf("suspended merge = %#v, %v", result, err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM workflow_v2_delivery_prs WHERE run_id='run-suspended-merge'`).Scan(&state); err != nil || state != "merged" {
		t.Fatalf("delivery state = %q, %v", state, err)
	}
	inspection, err := store.InspectRun(ctx, "run-suspended-merge")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.Status != workflowv2.RunSuspended || inspection.Facts["delivery"].(map[string]interface{})["all_merged"] != true {
		t.Fatalf("suspended run = %#v facts %#v", inspection.Run, inspection.Facts["delivery"])
	}
	// The persisted merge is no longer drift for the reconciler.
	targets, err = store.ActiveDeliveryTargetsAll(ctx)
	if err != nil || len(targets) != 1 || targets[0].State != "merged" {
		t.Fatalf("merged targets = %#v, %v", targets, err)
	}
	// The operator command that resumes the run replays the deferred merge.
	resumed, err := store.ApplyCommand(ctx, "run-suspended-merge", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"})
	if err != nil || resumed.Transition == nil || resumed.Transition.ToState != "reviewing" {
		t.Fatalf("resume = %#v, %v", resumed, err)
	}
	run, err := store.GetRun(ctx, "run-suspended-merge")
	if err != nil || run.State != "done" {
		t.Fatalf("recovered run = %#v, %v", run, err)
	}
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending deferred events = %d, %v", pending, err)
	}
}

func TestSuspendedRunReplaysOnlyLatestObservationPerSubject(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"  rebasing:\n    phase: build\n  checking:\n    phase: review\ntransitions:\n"+
			"  conflicted:\n    from: reviewing\n    on: pull_request.conflicted\n    to: rebasing\n"+
			"  mergeable:\n    from: reviewing\n    on: pull_request.mergeable\n    to: checking\n", 1)
	workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	if _, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
		ID: "run-coalesce", TenantID: "tenant-1", WorkspaceYAML: []byte(policyWorkspaceYAML), WorkflowYAML: []byte(workflowYAML),
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(ctx, "run-coalesce", "claw-policy")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	pr := typesv2.VerifiedPullRequest{URL: "https://github.example/org/api/pull/10", RepositoryName: "api", Repository: "org/api",
		Number: 10, SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: now,
		Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}
	if _, err := store.SubmitDelivery(ctx, "run-coalesce", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: pr.URL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return pr, nil
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review' WHERE id='run-coalesce'`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"dirty", "clean"} {
		targets, err := store.ActiveDeliveryTargetsAll(ctx)
		if err != nil || len(targets) != 1 {
			t.Fatalf("targets = %#v, %v", targets, err)
		}
		pr.BaseSHA, pr.MergeableState = "base-1", state
		if result, err := store.ReconcileMergeability(ctx, targets[0], pr); err != nil || !result.Deferred {
			t.Fatalf("%s while suspended = %#v, %v", state, result, err)
		}
	}
	inspection, err := store.InspectRun(ctx, "run-coalesce")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Run.Status != workflowv2.RunSuspended || inspection.Facts["delivery"].(map[string]interface{})["any_conflicted"] != false {
		t.Fatalf("suspended run = %#v facts %#v", inspection.Run, inspection.Facts["delivery"])
	}
	if _, err := store.ApplyCommand(ctx, "run-coalesce", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-coalesce")
	if err != nil || run.Status != workflowv2.RunActive || run.State != "checking" {
		t.Fatalf("recovered run = %#v, %v (stale conflicted replayed?)", run, err)
	}
}

// A conflict queued while suspended must not replay once a later observation
// supersedes it, even when the workflow does not listen on the later kind.
func TestSuspendedRunDropsSupersededConflict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance func(t *testing.T, store *workflowv2.Store, pr *typesv2.VerifiedPullRequest)
	}{
		{"clean again", func(t *testing.T, store *workflowv2.Store, pr *typesv2.VerifiedPullRequest) {
			pr.MergeableState = "clean"
			reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
				return store.ReconcileMergeability(context.Background(), target, *pr)
			})
		}},
		{"merged", func(t *testing.T, store *workflowv2.Store, pr *typesv2.VerifiedPullRequest) {
			pr.State = "merged"
			reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
				return store.ReconcilePullRequest(context.Background(), target, "merge", "pull_request.merged", *pr)
			})
		}},
		{"head moved", func(t *testing.T, store *workflowv2.Store, pr *typesv2.VerifiedPullRequest) {
			pr.HeadSHA = "head-2"
			reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
				return store.ReconcilePullRequest(context.Background(), target, "push", "pull_request.head_changed", *pr)
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
				"  rebasing:\n    phase: build\ntransitions:\n"+
					"  conflicted:\n    from: reviewing\n    on: pull_request.conflicted\n    to: rebasing\n", 1)
			workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
			if _, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
				ID: "run-supersede", TenantID: "tenant-1", WorkspaceYAML: []byte(policyWorkspaceYAML), WorkflowYAML: []byte(workflowYAML),
			}); err != nil {
				t.Fatal(err)
			}
			attempt, err := store.StartAttempt(ctx, "run-supersede", "claw-policy")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
			pr := typesv2.VerifiedPullRequest{URL: "https://github.example/org/api/pull/10", RepositoryName: "api", Repository: "org/api",
				Number: 10, SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: now,
				Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}
			if _, err := store.SubmitDelivery(ctx, "run-supersede", attempt.ID, typesv2.DeliveryManifest{
				PullRequests: []typesv2.PullRequestClaim{{URL: pr.URL}},
			}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
				typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
				return pr, nil
			})); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review' WHERE id='run-supersede'`); err != nil {
				t.Fatal(err)
			}
			pr.BaseSHA, pr.MergeableState = "base-1", "dirty"
			if result := reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
				return store.ReconcileMergeability(ctx, target, pr)
			}); !result.Deferred {
				t.Fatalf("conflict while suspended was not deferred: %#v", result)
			}
			tc.advance(t, store, &pr)
			var pending int
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events WHERE defer_key LIKE ?`,
				workflowv2.MergeabilityDeferKey("")+"%").Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("superseded conflict still queued: %d, %v", pending, err)
			}
			if _, err := store.ApplyCommand(ctx, "run-supersede", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
				t.Fatal(err)
			}
			run, err := store.GetRun(ctx, "run-supersede")
			if err != nil || run.Status != workflowv2.RunActive || run.State != "reviewing" {
				t.Fatalf("recovered run = %#v, %v (stale conflicted replayed?)", run, err)
			}
		})
	}
}

func reconcileOnlyTarget(t *testing.T, store *workflowv2.Store,
	apply func(workflowv2.DeliveryTarget) (workflowv2.EventResult, error)) workflowv2.EventResult {
	t.Helper()
	targets, err := store.ActiveDeliveryTargetsAll(context.Background())
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
	result, err := apply(targets[0])
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReviewFeedbackRejectsStaleDeliveryHead(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-stale-feedback", "head-1", "open")
	targets, err := store.ActiveDeliveryTargets(context.Background(), pr.Repository, pr.Number)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %#v, %v", targets, err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_delivery_prs SET current_head_sha='head-2' WHERE id=?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyReviewFeedback(context.Background(), targets[0], "stale-feedback",
		map[string]interface{}{"body": "old head comment"}, typesv2.EvidenceProvenance{
			Producer: string(workflowv2.ProducerReview), Principal: "alice", ObservedAt: time.Now().UTC(),
		})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error = %v, want stale head rejection", err)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND id='stale-feedback'`,
		"run-stale-feedback").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("events = %d", events)
	}
}

func TestCIPolicyUsesLatestCheckObservation(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-latest-ci", "head-1", "open")
	base := time.Date(2026, 8, 8, 12, 1, 0, 0, time.UTC)
	record := func(externalID, kind, status string, observed time.Time) workflowv2.DeliveryPolicyResult {
		result, err := store.RecordEvidence(context.Background(), workflowv2.EvidenceInput{
			RunID: "run-latest-ci", PRID: pr.ID, HeadSHA: pr.HeadSHA, Domain: "ci", Connection: "github-actions",
			ExternalID: externalID, Kind: kind, Status: status, Payload: map[string]interface{}{"pipeline": "github-pr"},
			ObservedAt: observed, Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerCI),
				Connection: "github-actions", ObservedAt: observed},
		}, workflowv2.ProducerCI)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	record("lint-old", "lint", "failure", base)
	record("unit", "unit", "success", base)
	latest := record("lint-new", "lint", "success", base.Add(time.Minute))
	if !latest.CISatisfied || latest.CIStatus != "satisfied" {
		t.Fatalf("newer successful rerun did not replace failure: %#v", latest)
	}
	regressed := record("lint-old", "lint", "failure", base.Add(-time.Minute))
	if !regressed.CISatisfied || regressed.CIStatus != "satisfied" {
		t.Fatalf("out-of-order evidence regressed policy: %#v", regressed)
	}
	failed := record("lint-newest", "lint", "failure", base.Add(2*time.Minute))
	if failed.CISatisfied || failed.CIStatus != "unsatisfied" {
		t.Fatalf("newest failure did not fail policy: %#v", failed)
	}
}

func TestReconciledCheckSetCannotTransitionOnMixedSnapshot(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-atomic-ci-snapshot", "head-1", "open")
	old := time.Date(2026, 8, 8, 12, 1, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO workflow_v2_evidence(
		id,run_id,pr_id,head_sha,head_generation,domain,connection,external_id,kind,status,
		payload_json,provenance_json,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"old-unit", "run-atomic-ci-snapshot", pr.ID, pr.HeadSHA, 1, "ci", "github-actions",
		"github-pr:old-unit", "unit", "failure", `{"pipeline":"github-pr"}`,
		`{"producer":"ci","observed_at":"2026-08-08T12:01:00Z"}`, old.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	current := old.Add(time.Minute)
	input := func(externalID, kind, status string) workflowv2.EvidenceInput {
		return workflowv2.EvidenceInput{RunID: "run-atomic-ci-snapshot", PRID: pr.ID, HeadSHA: pr.HeadSHA,
			Domain: "ci", Connection: "github-actions", ExternalID: "github-pr:" + externalID,
			Kind: kind, Status: status, Payload: map[string]interface{}{"pipeline": "github-pr"},
			ObservedAt: current, Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerCI),
				Connection: "github-actions", ExternalID: externalID, ObservedAt: current, Reconciled: true}}
	}
	result, err := store.ReconcileEvidenceSet(context.Background(), []workflowv2.EvidenceInput{
		input("lint", "lint", "success"), input("unit", "unit", "pending"),
	}, workflowv2.ProducerCI)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(context.Background(), "run-atomic-ci-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if result.CIStatus != "pending" || run.State != "reviewing" || run.StateVersion != 1 {
		t.Fatalf("policy/run = %#v/%#v", result, run)
	}
	var currentRows, supersededRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE run_id=? AND superseded_at=0`,
		run.ID).Scan(&currentRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE run_id=? AND superseded_at>0`,
		run.ID).Scan(&supersededRows); err != nil {
		t.Fatal(err)
	}
	if currentRows != 2 || supersededRows != 1 {
		t.Fatalf("current/superseded evidence = %d/%d", currentRows, supersededRows)
	}
	olderInput := func(externalID, kind string) workflowv2.EvidenceInput {
		item := input(externalID, kind, "failure")
		item.ObservedAt = old
		item.Provenance.ObservedAt = old
		return item
	}
	result, err = store.ReconcileEvidenceSet(context.Background(), []workflowv2.EvidenceInput{
		olderInput("lint", "lint"), olderInput("unit", "unit"),
	}, workflowv2.ProducerCI)
	if err != nil {
		t.Fatal(err)
	}
	if result.CIStatus != "pending" {
		t.Fatalf("out-of-order snapshot regressed policy: %#v", result)
	}
	result, err = store.ReconcileEvidenceSnapshot(context.Background(), []workflowv2.EvidenceScope{{
		RunID: "run-atomic-ci-snapshot", PRID: pr.ID, HeadSHA: pr.HeadSHA, Domain: "ci",
		Connection: "github-actions", Pipeline: "github-pr",
	}}, nil, workflowv2.ProducerCI)
	if err != nil {
		t.Fatal(err)
	}
	if result.CIStatus != "pending" {
		t.Fatalf("empty snapshot policy = %#v", result)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE run_id=? AND superseded_at=0`,
		"run-atomic-ci-snapshot").Scan(&currentRows); err != nil {
		t.Fatal(err)
	}
	if currentRows != 0 {
		t.Fatalf("empty snapshot retained %d current evidence rows", currentRows)
	}
}

func TestPolicyPublicationSuppressesIdenticalSnapshotsButAllowsRevisionCycles(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-policy-revisions", "head-1", "open")
	observed := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	scopes := []workflowv2.EvidenceScope{{RunID: "run-policy-revisions", PRID: pr.ID,
		HeadSHA: pr.HeadSHA, Domain: "ci", Connection: "github-actions", Pipeline: "github-pr"}}
	inputs := []workflowv2.EvidenceInput{
		{RunID: "run-policy-revisions", PRID: pr.ID, HeadSHA: pr.HeadSHA, Domain: "ci",
			Connection: "github-actions", ExternalID: "github-pr:lint", Kind: "lint", Status: "success",
			Payload: map[string]interface{}{"pipeline": "github-pr"}, ObservedAt: observed,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerCI), ObservedAt: observed}},
		{RunID: "run-policy-revisions", PRID: pr.ID, HeadSHA: pr.HeadSHA, Domain: "ci",
			Connection: "github-actions", ExternalID: "github-pr:unit", Kind: "unit", Status: "success",
			Payload: map[string]interface{}{"pipeline": "github-pr"}, ObservedAt: observed.Add(time.Second),
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerCI), ObservedAt: observed.Add(time.Second)}},
	}
	reconcile := func(snapshot []workflowv2.EvidenceInput) {
		t.Helper()
		if _, err := store.ReconcileEvidenceSnapshot(context.Background(), scopes, snapshot, workflowv2.ProducerCI); err != nil {
			t.Fatal(err)
		}
	}
	countPolicyEvents := func() int {
		t.Helper()
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events
			WHERE run_id='run-policy-revisions' AND kind='ci.policy.evaluated'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	reconcile(inputs)
	reconcile(inputs)
	if got := countPolicyEvents(); got != 1 {
		t.Fatalf("identical success snapshots published %d CI policy events, want 1", got)
	}
	reconcile(nil)
	reconcile(inputs)
	if got := countPolicyEvents(); got != 3 {
		t.Fatalf("success -> pending -> success published %d CI policy events, want 3", got)
	}
}

func TestEvidenceIdentityIsScopedToPullRequest(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-evidence-pr-scope")
	apiURL := "https://github.example/org/api/pull/10"
	webURL := "https://github.example/org/web/pull/20"
	verified, err := store.SubmitDelivery(context.Background(), "run-evidence-pr-scope", attempt.ID,
		typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: apiURL}, {URL: webURL}}},
		deliveryVerifier(map[string]string{apiURL: "shared-head", webURL: "shared-head"}))
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 8, 8, 12, 1, 0, 0, time.UTC)
	for _, pr := range verified {
		if _, err := store.RecordEvidence(context.Background(), workflowv2.EvidenceInput{
			RunID: "run-evidence-pr-scope", PRID: pr.ID, HeadSHA: pr.HeadSHA, Domain: "ci", Connection: "github",
			ExternalID: "same-provider-id", Kind: "unit", Status: "success",
			Payload: map[string]interface{}{"pipeline": "shared"}, ObservedAt: observed,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerCI), ObservedAt: observed},
		}, workflowv2.ProducerCI); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE run_id='run-evidence-pr-scope'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("cross-PR evidence rows = %d, want 2", count)
	}
}

func TestEvidenceDomainCannotBeForgedByWrongAdapter(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-evidence-auth", "head-1", "open")
	_, err := store.RecordEvidence(context.Background(), workflowv2.EvidenceInput{
		RunID: "run-evidence-auth", PRID: pr.ID, HeadSHA: pr.HeadSHA, Domain: "ci", Connection: "github-actions",
		ExternalID: "forged", Kind: "lint", Status: "success",
		Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerAgent)},
	}, workflowv2.ProducerAgent)
	if err == nil {
		t.Fatal("agent forged CI evidence")
	}
}

// A CI result queued while suspended describes the head it was evaluated on;
// it must not replay after the PR head moved.
func TestSuspendedRunDropsDeferredCIResultAfterHeadMove(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "run-stale-ci", "head-1", "open")
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review' WHERE id='run-stale-ci'`); err != nil {
		t.Fatal(err)
	}
	recordPolicyEvidence(t, store, "run-stale-ci", pr, "ci", "github-actions", "lint", "lint", "failure",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events WHERE defer_key='ci.policy.evaluated'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("CI failure not deferred: %d, %v", queued, err)
	}
	pr.HeadSHA = "head-2"
	reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ReconcilePullRequest(ctx, target, "push", "pull_request.head_changed", pr)
	})
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events WHERE defer_key='ci.policy.evaluated'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("stale CI result still queued: %d, %v", queued, err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='active',waiting_reason='' WHERE id='run-stale-ci'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplayDeferredEvents(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-stale-ci")
	if err != nil || run.State != "reviewing" || run.StateVersion != 1 {
		t.Fatalf("recovered run = %#v, %v (stale CI failure replayed?)", run, err)
	}
}

// A PR edit observed after a queued head change must not drop that head
// change, whether or not the workflow listens on the edit.
func TestSuspendedRunKeepsDeferredHeadChangeAcrossEdits(t *testing.T) {
	for _, tc := range []struct {
		name         string
		listenUpdate bool
	}{{"edit not listened", false}, {"edit listened", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			transitions := "  pushed:\n    from: reviewing\n    on: pull_request.head_changed\n    to: fixing\n"
			if tc.listenUpdate {
				transitions += "  edited:\n    from: reviewing\n    on: pull_request.updated\n    to: checking\n"
			}
			workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
				"  checking:\n    phase: review\ntransitions:\n"+transitions, 1)
			workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
			if _, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
				ID: "run-edit", TenantID: "tenant-1", WorkspaceYAML: []byte(policyWorkspaceYAML), WorkflowYAML: []byte(workflowYAML),
			}); err != nil {
				t.Fatal(err)
			}
			attempt, err := store.StartAttempt(ctx, "run-edit", "claw-policy")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
			pr := typesv2.VerifiedPullRequest{URL: "https://github.example/org/api/pull/10", RepositoryName: "api", Repository: "org/api",
				Number: 10, SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: now,
				Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}
			if _, err := store.SubmitDelivery(ctx, "run-edit", attempt.ID, typesv2.DeliveryManifest{
				PullRequests: []typesv2.PullRequestClaim{{URL: pr.URL}},
			}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
				typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
				return pr, nil
			})); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review' WHERE id='run-edit'`); err != nil {
				t.Fatal(err)
			}
			pr.HeadSHA = "head-2"
			if result := reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
				return store.ReconcilePullRequest(ctx, target, "push", "pull_request.head_changed", pr)
			}); !result.Deferred {
				t.Fatalf("head change while suspended was not deferred: %#v", result)
			}
			reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
				return store.ReconcilePullRequest(ctx, target, "edit", "pull_request.updated", pr)
			})
			if _, err := store.ApplyCommand(ctx, "run-edit", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
				t.Fatal(err)
			}
			run, err := store.GetRun(ctx, "run-edit")
			if err != nil || run.Status != workflowv2.RunActive || run.State != "fixing" {
				t.Fatalf("recovered run = %#v, %v (deferred head change lost?)", run, err)
			}
		})
	}
}

// createSuspendedDelivery starts a run on workflowYAML, delivers PR #10 at
// head-1 and suspends the run.
func createSuspendedDelivery(t *testing.T, store *workflowv2.Store, db *sql.DB, runID, workflowYAML string) typesv2.VerifiedPullRequest {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
		ID: runID, TenantID: "tenant-1", WorkspaceYAML: []byte(policyWorkspaceYAML), WorkflowYAML: []byte(workflowYAML),
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(ctx, runID, "claw-policy")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	pr := typesv2.VerifiedPullRequest{URL: "https://github.example/org/api/pull/10", RepositoryName: "api", Repository: "org/api",
		Number: 10, SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: now,
		Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}
	verified, err := store.SubmitDelivery(ctx, runID, attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: pr.URL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return pr, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review' WHERE id=?`, runID); err != nil {
		t.Fatal(err)
	}
	return verified[0]
}

// A close queued while suspended must not replay once a later non-lifecycle
// observation (a push after a missed reopen webhook) records the PR as open.
func TestSuspendedRunDropsDeferredCloseAfterReopen(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"  done:\n    phase: done\n    terminal: true\ntransitions:\n  closed:\n    from: reviewing\n    on: pull_request.closed\n    to: done\n", 1)
	workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	pr := createSuspendedDelivery(t, store, db, "run-reopen", workflowYAML)
	pr.State = "closed"
	if result := reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ReconcilePullRequest(ctx, target, "close", "pull_request.closed", pr)
	}); !result.Deferred {
		t.Fatalf("close while suspended was not deferred: %#v", result)
	}
	pr.State, pr.HeadSHA = "open", "head-2"
	reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ReconcilePullRequest(ctx, target, "push", "pull_request.head_changed", pr)
	})
	if _, err := store.ApplyCommand(ctx, "run-reopen", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-reopen")
	if err != nil || run.Status != workflowv2.RunActive || run.State != "reviewing" {
		t.Fatalf("recovered run = %#v, %v (stale close replayed?)", run, err)
	}
}

// Review feedback queued while suspended is bound to the head it was left on
// and must not replay after the PR head moved.
func TestSuspendedRunDropsDeferredFeedbackAfterHeadMove(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"transitions:\n  feedback:\n    from: reviewing\n    on: review.feedback.received\n    to: fixing\n", 1)
	workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	pr := createSuspendedDelivery(t, store, db, "run-stale-deferred-feedback", workflowYAML)
	if result := reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ApplyReviewFeedback(ctx, target, "comment-1", map[string]interface{}{"body": "fix this"},
			typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerReview), Principal: "alice",
				ObservedAt: time.Date(2026, 8, 8, 12, 1, 0, 0, time.UTC)})
	}); !result.Deferred {
		t.Fatalf("feedback while suspended was not deferred: %#v", result)
	}
	pr.HeadSHA = "head-2"
	reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ReconcilePullRequest(ctx, target, "push", "pull_request.head_changed", pr)
	})
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events WHERE defer_key='review.feedback.received'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("stale feedback still queued: %d, %v", queued, err)
	}
	if _, err := store.ApplyCommand(ctx, "run-stale-deferred-feedback", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-stale-deferred-feedback")
	if err != nil || run.Status != workflowv2.RunActive || run.State != "reviewing" {
		t.Fatalf("recovered run = %#v, %v (stale feedback replayed?)", run, err)
	}
}

// Feedback edited while suspended must replay with its current content: the
// queued received event carries the original payload, which would otherwise
// override the refreshed fact in guard evaluation.
func TestSuspendedRunReplaysEditedFeedbackContent(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"transitions:\n  feedback:\n    from: reviewing\n    on: review.feedback.received\n    when:\n      review:\n        feedback:\n          severity:\n            equals: blocking\n    to: fixing\n", 1)
	workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	createSuspendedDelivery(t, store, db, "run-edited-feedback", workflowYAML)
	provenance := func(minute int) typesv2.EvidenceProvenance {
		return typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerReview), Principal: "alice",
			ExternalID: "comment-1", ObservedAt: time.Date(2026, 8, 8, 12, minute, 0, 0, time.UTC)}
	}
	if result := reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ApplyReviewFeedback(ctx, target, "comment-1",
			map[string]interface{}{"kind": "comment", "body": "fix this", "severity": "blocking"}, provenance(1))
	}); !result.Deferred {
		t.Fatalf("feedback while suspended was not deferred: %#v", result)
	}
	if result := reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ReconcileReviewFeedback(ctx, target, "comment-1",
			map[string]interface{}{"kind": "comment", "body": "never mind", "severity": "nit"}, provenance(2))
	}); result.Disposition != typesv2.DispositionAccepted {
		t.Fatalf("feedback edit while suspended = %#v", result)
	}
	if _, err := store.ApplyCommand(ctx, "run-edited-feedback", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-edited-feedback")
	if err != nil || run.Status != workflowv2.RunActive || run.State != "reviewing" {
		t.Fatalf("recovered run = %#v, %v (stale feedback content replayed?)", run, err)
	}
}

// A pull_request.mergeable queued while suspended survives a later
// mergeability change that keeps the PR out of dirty/behind: the reconciler
// already persisted the state and will not emit pull_request.mergeable again.
// A change back to dirty drops it.
func TestSuspendedRunKeepsDeferredMergeableAcrossMergeabilityUpdates(t *testing.T) {
	for _, tc := range []struct {
		name, state, guard, want string
	}{
		{"clean to blocked keeps mergeable", "blocked", "", "reviewing"},
		{"dirty again drops mergeable", "dirty", "", "rebasing"},
		// The replay must match guards against the current state, not the
		// queued clean one.
		{"clean guard sees blocked", "blocked", "clean", "rebasing"},
		{"blocked guard sees blocked", "blocked", "blocked", "reviewing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			guard := ""
			if tc.guard != "" {
				guard = "    when:\n      pull_request:\n        mergeable_state:\n          equals: " + tc.guard + "\n"
			}
			workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
				"  rebasing:\n    phase: build\ntransitions:\n"+
					"  conflicted:\n    from: reviewing\n    on: pull_request.conflicted\n    to: rebasing\n"+
					"  rebased:\n    from: rebasing\n    on: pull_request.mergeable\n"+guard+"    to: reviewing\n", 1)
			workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n" +
				"  resume_rebase:\n    from: rebasing\n    to: rebasing\n"
			pr := createSuspendedDelivery(t, store, db, "run-mergeable", workflowYAML)
			mergeability := func(state string) workflowv2.EventResult {
				pr.MergeableState = state
				pr.Provenance.ObservedAt = pr.Provenance.ObservedAt.Add(time.Second)
				return reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
					return store.ReconcileMergeability(ctx, target, pr)
				})
			}
			pr.BaseSHA = "base-1"
			if result := mergeability("dirty"); !result.Deferred {
				t.Fatalf("conflict while suspended was not deferred: %#v", result)
			}
			if _, err := store.ApplyCommand(ctx, "run-mergeable", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
				t.Fatal(err)
			}
			if run, err := store.GetRun(ctx, "run-mergeable"); err != nil || run.State != "rebasing" {
				t.Fatalf("conflict not replayed: %#v, %v", run, err)
			}
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review' WHERE id='run-mergeable'`); err != nil {
				t.Fatal(err)
			}
			if result := mergeability("clean"); !result.Deferred {
				t.Fatalf("mergeable while suspended was not deferred: %#v", result)
			}
			mergeability(tc.state)
			if _, err := store.ApplyCommand(ctx, "run-mergeable", "resume_rebase", workflowv2.CommandInput{ID: "resume-2", MessageID: "resume-2"}); err != nil {
				t.Fatal(err)
			}
			run, err := store.GetRun(ctx, "run-mergeable")
			if err != nil || run.Status != workflowv2.RunActive || run.State != tc.want {
				t.Fatalf("recovered run = %#v, %v, want state %s", run, err, tc.want)
			}
		})
	}
}

// A replayed observation carries its queue-time payload, which overrides facts
// in guard evaluation. Delivery aggregates must reflect sibling PRs that
// changed after the observation was queued.
func TestSuspendedRunReplaysCurrentDeliveryAggregates(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"  done:\n    phase: done\n    terminal: true\ntransitions:\n"+
			"  all_closed:\n    from: reviewing\n    on: pull_request.closed\n    when:\n      delivery:\n        open:\n          equals: 0\n    to: done\n", 1)
	workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	if _, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
		ID: "run-aggregates", TenantID: "tenant-1", WorkspaceYAML: []byte(policyWorkspaceYAML), WorkflowYAML: []byte(workflowYAML),
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(ctx, "run-aggregates", "claw-policy")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	newPR := func(number int, head string) typesv2.VerifiedPullRequest {
		return typesv2.VerifiedPullRequest{URL: fmt.Sprintf("https://github.example/org/api/pull/%d", number), RepositoryName: "api",
			Repository: "org/api", Number: number, SourceBranch: fmt.Sprintf("feature-%d", number), BaseBranch: "main", HeadSHA: head,
			State: "open", VerifiedAt: now, Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}
	}
	byURL := map[string]typesv2.VerifiedPullRequest{}
	for _, pr := range []typesv2.VerifiedPullRequest{newPR(10, "head-a"), newPR(11, "head-b")} {
		byURL[pr.URL] = pr
	}
	verified, err := store.SubmitDelivery(ctx, "run-aggregates", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: newPR(10, "").URL}, {URL: newPR(11, "").URL}},
	}, workflowv2.PullRequestVerifierFunc(func(_ context.Context, _ workflowv2.Run, _ *typesv2.Workspace,
		claim typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return byURL[claim.URL], nil
	}))
	if err != nil || len(verified) != 2 {
		t.Fatalf("delivery = %#v, %v", verified, err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='operator review' WHERE id='run-aggregates'`); err != nil {
		t.Fatal(err)
	}
	reconcile := func(pr typesv2.VerifiedPullRequest, eventID, kind string) workflowv2.EventResult {
		t.Helper()
		targets, err := store.ActiveDeliveryTargetsAll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range targets {
			if target.Number == pr.Number {
				result, err := store.ReconcilePullRequest(ctx, target, eventID, kind, pr)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
		}
		t.Fatalf("no target for PR %d in %#v", pr.Number, targets)
		return workflowv2.EventResult{}
	}
	a, b := verified[0], verified[1]
	if a.Number != 10 {
		a, b = b, a
	}
	b.State = "closed"
	reconcile(b, "close-b", "pull_request.closed")
	a.State = "closed"
	if result := reconcile(a, "close-a", "pull_request.closed"); !result.Deferred {
		t.Fatalf("close while suspended was not deferred: %#v", result)
	}
	b.State = "open"
	reconcile(b, "reopen-b", "pull_request.reopened")
	if _, err := store.ApplyCommand(ctx, "run-aggregates", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-aggregates")
	if err != nil || run.Status != workflowv2.RunActive || run.State != "reviewing" {
		t.Fatalf("recovered run = %#v, %v (stale delivery.open replayed?)", run, err)
	}
}

// Evidence recorded while suspended may invalidate the resume destination's
// invariant while a queued transition leaves that state. Recovery must replay
// the queue before judging the invariant, or the run stays suspended for good.
func TestSuspendedRunRecoversThroughInvariantInvalidatedWhileSuspended(t *testing.T) {
	invariant := "    invariant:\n      ci.status:\n        not_equals: unsatisfied\n"
	withInvariant := strings.Replace(policyWorkflowYAML, "  reviewing:\n    phase: review\n",
		"  reviewing:\n    phase: review\n"+invariant, 1)
	withResume := withInvariant + "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	t.Run("queued transition leaves the state", func(t *testing.T) {
		ctx := context.Background()
		db := openRuntimeDB(t)
		store := workflowv2.NewStore(db)
		pr := createSuspendedDelivery(t, store, db, "run-invariant", withResume)
		recordPolicyEvidence(t, store, "run-invariant", pr, "ci", "github-actions", "lint", "lint", "failure",
			map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
		result, err := store.ApplyCommand(ctx, "run-invariant", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"})
		if err != nil || result.Disposition != typesv2.DispositionAccepted {
			t.Fatalf("resume = %#v, %v", result, err)
		}
		run, err := store.GetRun(ctx, "run-invariant")
		if err != nil || run.Status != workflowv2.RunActive || run.State != "fixing" {
			t.Fatalf("recovered run = %#v, %v (resume rejected on suspended evidence?)", run, err)
		}
	})
	t.Run("nothing queued leaves the state", func(t *testing.T) {
		ctx := context.Background()
		db := openRuntimeDB(t)
		store := workflowv2.NewStore(db)
		// No transition listens on CI, so nothing is queued and the broken
		// invariant must still reject the resume.
		noCITransition := strings.Replace(withResume, "    on: ci.policy.evaluated\n", "    on: custom.never\n", 1)
		pr := createSuspendedDelivery(t, store, db, "run-invariant-strict", noCITransition)
		recordPolicyEvidence(t, store, "run-invariant-strict", pr, "ci", "github-actions", "lint", "lint", "failure",
			map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
		if _, err := store.ApplyCommand(ctx, "run-invariant-strict", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
			t.Fatal(err)
		}
		run, err := store.GetRun(ctx, "run-invariant-strict")
		if err != nil || run.Status != workflowv2.RunSuspended || run.State != "reviewing" {
			t.Fatalf("run = %#v, %v (resumed into a broken invariant?)", run, err)
		}
	})
	t.Run("queued observation does not leave the state", func(t *testing.T) {
		ctx := context.Background()
		db := openRuntimeDB(t)
		store := workflowv2.NewStore(db)
		// The queued CI result matches no transition from reviewing, so after
		// replay the invariant is still broken and the run suspends again.
		noMatch := strings.Replace(withResume, "          equals: unsatisfied\n    to: fixing\n  review_failed:",
			"          equals: never\n    to: fixing\n  review_failed:", 1)
		pr := createSuspendedDelivery(t, store, db, "run-invariant-replay", noMatch)
		recordPolicyEvidence(t, store, "run-invariant-replay", pr, "ci", "github-actions", "lint", "lint", "failure",
			map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
		if _, err := store.ApplyCommand(ctx, "run-invariant-replay", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
			t.Fatal(err)
		}
		run, err := store.GetRun(ctx, "run-invariant-replay")
		if err != nil || run.Status != workflowv2.RunSuspended || run.State != "reviewing" {
			t.Fatalf("run = %#v, %v (active with a broken invariant?)", run, err)
		}
	})
}

// A recovery that postpones the destination invariant must not release effects
// before the replay judges it: the run stays held (suspended, so the effect
// worker cannot claim) until the queue drains, and a failed replay leaves it
// held for the reconciler sweep instead of active with a broken invariant.
func TestRecoveryHoldsEffectsUntilReplaySettlesInvariant(t *testing.T) {
	invariant := "    invariant:\n      ci.status:\n        not_equals: unsatisfied\n"
	onEnter := "    on_enter:\n      effects:\n        - agent.task:\n            prompt: Review the delivery.\n"
	withInvariant := strings.Replace(policyWorkflowYAML, "  reviewing:\n    phase: review\n",
		"  reviewing:\n    phase: review\n"+invariant+onEnter, 1)
	withResume := withInvariant + "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	noMatch := strings.Replace(withResume, "          equals: unsatisfied\n    to: fixing\n  review_failed:",
		"          equals: never\n    to: fixing\n  review_failed:", 1)
	for _, tc := range []struct {
		name, workflow, state string
		status                workflowv2.RunStatus
		claimPrompt           string
	}{
		{name: "queued transition leaves the state", workflow: withResume, state: "fixing", status: workflowv2.RunActive,
			claimPrompt: "Address the trusted review feedback."},
		{name: "queued observation does not leave the state", workflow: noMatch, state: "reviewing", status: workflowv2.RunSuspended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			pr := createSuspendedDelivery(t, store, db, "run-held", tc.workflow)
			// The initial review task ran before the run was suspended.
			if _, err := db.Exec(`UPDATE workflow_v2_effects SET status='succeeded' WHERE run_id='run-held'`); err != nil {
				t.Fatal(err)
			}
			recordPolicyEvidence(t, store, "run-held", pr, "ci", "github-actions", "lint", "lint", "failure",
				map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
			if _, err := db.Exec(`CREATE TRIGGER fail_replay BEFORE DELETE ON workflow_v2_deferred_events
				BEGIN SELECT RAISE(ABORT, 'injected replay failure'); END`); err != nil {
				t.Fatal(err)
			}
			result, err := store.ApplyCommand(ctx, "run-held", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"})
			if err != nil || result.Disposition != typesv2.DispositionAccepted {
				t.Fatalf("resume = %#v, %v", result, err)
			}
			if result.Run.Status != workflowv2.RunSuspended || result.Run.State != "reviewing" {
				t.Fatalf("resumed run = %#v (released before the invariant was judged?)", result.Run)
			}
			if claim, err := store.ClaimEffect(ctx, "worker-1", time.Minute); err != nil || claim != nil {
				t.Fatalf("claim while held = %#v, %v (effects released before the invariant was judged?)", claim, err)
			}
			if _, err := db.Exec(`DROP TRIGGER fail_replay`); err != nil {
				t.Fatal(err)
			}
			if err := store.ReplayDeferredEvents(ctx); err != nil {
				t.Fatal(err)
			}
			run, err := store.GetRun(ctx, "run-held")
			if err != nil || run.Status != tc.status || run.State != tc.state {
				t.Fatalf("settled run = %#v, %v", run, err)
			}
			// The unverified entry into reviewing never runs its effects: the run
			// left the state, or was suspended in it, before its invariant held.
			var status, lastError string
			if err := db.QueryRow(`SELECT status,last_error FROM workflow_v2_effects WHERE run_id=? AND origin_type='transition'
				AND json_extract(payload_json,'$.prompt')='Review the delivery.'`, "run-held").Scan(&status, &lastError); err != nil {
				t.Fatal(err)
			}
			if status != string(workflowv2.EffectCancelled) || !strings.Contains(lastError, "reviewing") {
				t.Fatalf("held entry effect = %s (%q), want cancelled", status, lastError)
			}
			claim, err := store.ClaimEffect(ctx, "worker-1", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if tc.claimPrompt == "" {
				if claim != nil {
					t.Fatalf("claim on suspended run = %#v", claim)
				}
				return
			}
			if claim == nil || claim.Effect.Payload["prompt"] != tc.claimPrompt {
				t.Fatalf("claim after replay = %#v, want the %q task", claim, tc.claimPrompt)
			}
			if next, err := store.ClaimEffect(ctx, "worker-2", time.Minute); err != nil || next != nil {
				t.Fatalf("second claim = %#v, %v (held entry effect released?)", next, err)
			}
		})
	}
}

// A replayed event that matches no transition while the invariant is broken
// must not plan its clause effects: a live event would have been rejected
// before scheduling them, and once the settle suspends the run they would run
// on a later recovery.
func TestReplayDoesNotPlanClauseEffectsUnderBrokenInvariant(t *testing.T) {
	invariant := "    invariant:\n      ci.status:\n        not_equals: unsatisfied\n"
	withInvariant := strings.Replace(policyWorkflowYAML, "  reviewing:\n    phase: review\n",
		"  reviewing:\n    phase: review\n"+invariant, 1)
	noMatch := strings.Replace(withInvariant, "          equals: unsatisfied\n    to: fixing\n  review_failed:",
		"          equals: never\n    to: fixing\n  review_failed:", 1)
	clause := "events:\n  ci.policy.evaluated:\n    clauses:\n      - from: reviewing\n        effects:\n" +
		"          - agent.task:\n              prompt: Investigate the CI failure.\n"
	workflowYAML := noMatch + clause + "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	for _, tc := range []struct {
		name    string
		recover func(t *testing.T, ctx context.Context, store *workflowv2.Store, db *sql.DB)
	}{
		{name: "held entry", recover: func(t *testing.T, ctx context.Context, store *workflowv2.Store, db *sql.DB) {
			if _, err := store.ApplyCommand(ctx, "run-clause", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "held state", recover: func(t *testing.T, ctx context.Context, store *workflowv2.Store, db *sql.DB) {
			// Recovered outside the event path; the sweep replays the queue.
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='active',waiting_reason='' WHERE id='run-clause'`); err != nil {
				t.Fatal(err)
			}
			if err := store.ReplayDeferredEvents(ctx); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRuntimeDB(t)
			store := workflowv2.NewStore(db)
			pr := createSuspendedDelivery(t, store, db, "run-clause", workflowYAML)
			if _, err := db.Exec(`UPDATE workflow_v2_effects SET status='succeeded' WHERE run_id='run-clause'`); err != nil {
				t.Fatal(err)
			}
			recordPolicyEvidence(t, store, "run-clause", pr, "ci", "github-actions", "lint", "lint", "failure",
				map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
			tc.recover(t, ctx, store, db)
			run, err := store.GetRun(ctx, "run-clause")
			if err != nil || run.Status != workflowv2.RunSuspended || run.State != "reviewing" {
				t.Fatalf("settled run = %#v, %v (active with a broken invariant?)", run, err)
			}
			var planned int
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_effects WHERE run_id='run-clause'
				AND origin_type='event_clause' AND status IN ('planned','retryable_failed')`).Scan(&planned); err != nil {
				t.Fatal(err)
			}
			if planned != 0 {
				t.Fatalf("%d clause effect(s) planned under a broken invariant", planned)
			}
			// A later recovery must not release work planned while the invariant was broken.
			if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='active',waiting_reason='' WHERE id='run-clause'`); err != nil {
				t.Fatal(err)
			}
			if claim, err := store.ClaimEffect(ctx, "worker-1", time.Minute); err != nil || claim != nil {
				t.Fatalf("claim after recovery = %#v, %v", claim, err)
			}
		})
	}
}

// Clause effects that a replay parks under a broken invariant run once a later
// replayed event restores the invariant before the settle: the CI clause
// closes the gate and the review clause reopens it.
func TestReplayReleasesPostponedClauseEffectsWhenInvariantSettles(t *testing.T) {
	invariant := "    invariant:\n      work.gate_closed:\n        exists: false\n"
	withInvariant := strings.Replace(policyWorkflowYAML, "  reviewing:\n    phase: review\n",
		"  reviewing:\n    phase: review\n"+invariant, 1)
	noMatch := strings.Replace(withInvariant, "          equals: unsatisfied\n    to: fixing\n  review_failed:",
		"          equals: never\n    to: fixing\n  review_failed:", 1)
	clauses := "events:\n  ci.policy.evaluated:\n    clauses:\n      - from: reviewing\n        set:\n          work.gate_closed: true\n        effects:\n" +
		"          - agent.task:\n              prompt: Investigate the CI failure.\n" +
		"  review.policy.evaluated:\n    clauses:\n      - from: reviewing\n        set:\n          work.gate_closed: null\n"
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	pr := createSuspendedDelivery(t, store, db, "run-postponed", noMatch+clauses)
	if _, err := db.Exec(`UPDATE workflow_v2_effects SET status='succeeded' WHERE run_id='run-postponed'`); err != nil {
		t.Fatal(err)
	}
	recordPolicyEvidence(t, store, "run-postponed", pr, "ci", "github-actions", "lint", "lint", "failure",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	recordPolicyEvidence(t, store, "run-postponed", pr, "review", "github-reviews", "alice", "approval", "approved",
		nil, workflowv2.ProducerReview)
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='active',waiting_reason='' WHERE id='run-postponed'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplayDeferredEvents(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-postponed")
	if err != nil || run.Status != workflowv2.RunActive || run.State != "reviewing" {
		t.Fatalf("settled run = %#v, %v", run, err)
	}
	claim, err := store.ClaimEffect(ctx, "worker-1", time.Minute)
	if err != nil || claim == nil || claim.Effect.Kind != "agent.task" {
		t.Fatalf("claim after settle = %#v, %v (postponed clause effect lost?)", claim, err)
	}
}

// A self-loop transition does not leave the state, so it keeps the clause
// effects a replay parked there until the settle judges the invariant: the CI
// clause closes the gate and a reviewing -> reviewing transition reopens it.
func TestReplaySelfLoopKeepsPostponedClauseEffects(t *testing.T) {
	invariant := "    invariant:\n      work.gate_closed:\n        exists: false\n"
	withInvariant := strings.Replace(policyWorkflowYAML, "  reviewing:\n    phase: review\n",
		"  reviewing:\n    phase: review\n"+invariant, 1)
	noMatch := strings.Replace(withInvariant, "          equals: unsatisfied\n    to: fixing\n  review_failed:",
		"          equals: never\n    to: fixing\n  review_failed:", 1)
	selfLoop := strings.Replace(noMatch, "ci:\n  policies:",
		"  review_passed:\n    from: reviewing\n    on: review.policy.evaluated\n    when:\n      review:\n        status:\n          not_equals: unsatisfied\n"+
			"    set:\n      work.gate_closed: null\n    to: reviewing\nci:\n  policies:", 1)
	clauses := "events:\n  ci.policy.evaluated:\n    clauses:\n      - from: reviewing\n        set:\n          work.gate_closed: true\n        effects:\n" +
		"          - agent.task:\n              prompt: Investigate the CI failure.\n"
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	pr := createSuspendedDelivery(t, store, db, "run-self-loop", selfLoop+clauses)
	if _, err := db.Exec(`UPDATE workflow_v2_effects SET status='succeeded' WHERE run_id='run-self-loop'`); err != nil {
		t.Fatal(err)
	}
	recordPolicyEvidence(t, store, "run-self-loop", pr, "ci", "github-actions", "lint", "lint", "failure",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	recordPolicyEvidence(t, store, "run-self-loop", pr, "review", "github-reviews", "alice", "approval", "approved",
		nil, workflowv2.ProducerReview)
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='active',waiting_reason='' WHERE id='run-self-loop'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplayDeferredEvents(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-self-loop")
	if err != nil || run.Status != workflowv2.RunActive || run.State != "reviewing" || run.StateVersion < 2 {
		t.Fatalf("settled run = %#v, %v (self-loop not taken?)", run, err)
	}
	claim, err := store.ClaimEffect(ctx, "worker-1", time.Minute)
	if err != nil || claim == nil || claim.Effect.Kind != "agent.task" {
		t.Fatalf("claim after settle = %#v, %v (self-loop cancelled the parked clause effect?)", claim, err)
	}
}

// A live observation on an active run supersedes one queued for the same
// subject: a failed recovery replay leaves the queue for the sweep, and the
// sweep must not replay a CI failure that a later live CI result superseded.
func TestLiveObservationSupersedesQueuedDeferredEvent(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := policyWorkflowYAML + "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	pr := createSuspendedDelivery(t, store, db, "run-live", workflowYAML)
	recordPolicyEvidence(t, store, "run-live", pr, "ci", "github-actions", "lint", "lint", "failure",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	if _, err := db.Exec(`CREATE TRIGGER fail_replay BEFORE DELETE ON workflow_v2_deferred_events
		BEGIN SELECT RAISE(ABORT, 'injected replay failure'); END`); err != nil {
		t.Fatal(err)
	}
	// reviewing has no invariant, so the resume releases the run active and
	// the failed replay leaves the queued CI failure for the sweep.
	result, err := store.ApplyCommand(ctx, "run-live", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"})
	if err != nil || result.Run.Status != workflowv2.RunActive || result.Run.State != "reviewing" {
		t.Fatalf("resume = %#v, %v", result, err)
	}
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events WHERE run_id='run-live'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("queued after failed replay = %d, %v", queued, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_replay`); err != nil {
		t.Fatal(err)
	}
	live := recordPolicyEvidence(t, store, "run-live", pr, "ci", "github-actions", "lint", "lint", "success",
		map[string]interface{}{"pipeline": "github-pr"}, workflowv2.ProducerCI)
	if live.CIStatus != "pending" {
		t.Fatalf("live CI result = %#v", live)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events WHERE run_id='run-live'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("queued after live CI result = %d, %v (superseded observation kept)", queued, err)
	}
	if err := store.ReplayDeferredEvents(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run-live")
	if err != nil || run.Status != workflowv2.RunActive || run.State != "reviewing" {
		t.Fatalf("run after sweep = %#v, %v (stale CI failure replayed?)", run, err)
	}
}

// A conflict queued while suspended describes the head it was observed on; an
// agent resubmission that verifies a new head must not let it replay.
func TestSuspendedRunDropsDeferredConflictAfterResubmission(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"  rebasing:\n    phase: build\ntransitions:\n"+
			"  conflicted:\n    from: reviewing\n    on: pull_request.conflicted\n    to: rebasing\n", 1)
	workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	pr := createSuspendedDelivery(t, store, db, "run-resubmit", workflowYAML)
	pr.BaseSHA, pr.MergeableState = "base-1", "dirty"
	pr.Provenance.ObservedAt = pr.Provenance.ObservedAt.Add(time.Second)
	if result := reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ReconcileMergeability(ctx, target, pr)
	}); !result.Deferred {
		t.Fatalf("conflict while suspended was not deferred: %#v", result)
	}
	run, err := store.GetRun(ctx, "run-resubmit")
	if err != nil {
		t.Fatal(err)
	}
	resubmitted := pr
	resubmitted.HeadSHA, resubmitted.MergeableState, resubmitted.BaseSHA = "head-2", "", ""
	if _, err := store.SubmitDelivery(ctx, "run-resubmit", run.CurrentAttemptID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: pr.URL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return resubmitted, nil
	})); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events WHERE run_id='run-resubmit'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("stale conflict still queued after resubmission: %d, %v", queued, err)
	}
	if _, err := store.ApplyCommand(ctx, "run-resubmit", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	if run, err := store.GetRun(ctx, "run-resubmit"); err != nil || run.State != "reviewing" || run.Status != workflowv2.RunActive {
		t.Fatalf("recovered run = %#v, %v (stale conflict replayed?)", run, err)
	}
}

// Replay itself refuses a queued mergeability observation whose PR head moved,
// whatever path moved it.
func TestReplayDropsDeferredMergeabilityForMovedHead(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"  rebasing:\n    phase: build\ntransitions:\n"+
			"  conflicted:\n    from: reviewing\n    on: pull_request.conflicted\n    to: rebasing\n", 1)
	pr := createSuspendedDelivery(t, store, db, "run-moved", workflowYAML)
	pr.BaseSHA, pr.MergeableState = "base-1", "dirty"
	pr.Provenance.ObservedAt = pr.Provenance.ObservedAt.Add(time.Second)
	if result := reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
		return store.ReconcileMergeability(ctx, target, pr)
	}); !result.Deferred {
		t.Fatalf("conflict while suspended was not deferred: %#v", result)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_delivery_prs SET current_head_sha='head-2' WHERE run_id='run-moved'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_runs SET status='active',waiting_reason='' WHERE id='run-moved'`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplayDeferredEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if run, err := store.GetRun(ctx, "run-moved"); err != nil || run.State != "reviewing" || run.Status != workflowv2.RunActive {
		t.Fatalf("recovered run = %#v, %v (stale conflict replayed?)", run, err)
	}
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_deferred_events WHERE run_id='run-moved'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("stale conflict still queued: %d, %v", queued, err)
	}
}

// A conflict a suspended run deferred and then dropped (the head moved away)
// never ran its transition, so the same tuple recurring after a force-push back
// must fire pull_request.conflicted again rather than dedupe against it.
func TestSuspendedRunRefiresDroppedConflictAfterForcePushBack(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	workflowYAML := strings.Replace(policyWorkflowYAML, "transitions:\n",
		"  rebasing:\n    phase: build\ntransitions:\n"+
			"  conflicted:\n    from: reviewing\n    on: pull_request.conflicted\n    to: rebasing\n", 1)
	workflowYAML += "commands:\n  resume:\n    from: reviewing\n    to: reviewing\n"
	pr := createSuspendedDelivery(t, store, db, "run-force-back", workflowYAML)
	step := func() { pr.Provenance.ObservedAt = pr.Provenance.ObservedAt.Add(time.Second) }
	observe := func(state string) workflowv2.EventResult {
		t.Helper()
		step()
		pr.BaseSHA, pr.MergeableState = "base-1", state
		return reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
			return store.ReconcileMergeability(ctx, target, pr)
		})
	}
	push := func(head string) {
		t.Helper()
		step()
		pr.HeadSHA = head
		reconcileOnlyTarget(t, store, func(target workflowv2.DeliveryTarget) (workflowv2.EventResult, error) {
			return store.ReconcilePullRequest(ctx, target, "push-"+head+pr.Provenance.ObservedAt.String(), "pull_request.head_changed", pr)
		})
	}
	if result := observe("dirty"); !result.Deferred {
		t.Fatalf("conflict while suspended was not deferred: %#v", result)
	}
	push("head-2")
	observe("clean")
	push("head-1")
	result := observe("dirty")
	var kind string
	if err := db.QueryRow(`SELECT kind FROM workflow_v2_events WHERE id=?`, result.EventID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "pull_request.conflicted" || !result.Deferred {
		t.Fatalf("recurring dropped conflict = %s, deferred=%v", kind, result.Deferred)
	}
	if _, err := store.ApplyCommand(ctx, "run-force-back", "resume", workflowv2.CommandInput{ID: "resume-1", MessageID: "resume-1"}); err != nil {
		t.Fatal(err)
	}
	if run, err := store.GetRun(ctx, "run-force-back"); err != nil || run.State != "rebasing" {
		t.Fatalf("recovered run = %#v, %v (conflict not replayed)", run, err)
	}
	// Once the replay ran the transition, the tuple is spent.
	push("head-3")
	observe("clean")
	push("head-1")
	result = observe("dirty")
	if err := db.QueryRow(`SELECT kind FROM workflow_v2_events WHERE id=?`, result.EventID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if kind != "pull_request.mergeability_updated" {
		t.Fatalf("replayed conflict tuple fired again as %s", kind)
	}
}
