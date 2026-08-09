package workflowv2_test

import (
	"context"
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
transitions:
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
