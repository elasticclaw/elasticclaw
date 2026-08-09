package workflowv2_test

import (
	"context"
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

func TestSuspendedRunsAreNotExternalDeliveryTargets(t *testing.T) {
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
	if len(byPR) != 0 || len(byHead) != 0 || len(all) != 0 {
		t.Fatalf("suspended targets by PR/head/all = %#v/%#v/%#v", byPR, byHead, all)
	}
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
