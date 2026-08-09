package workflowv2_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	workflowv2 "github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
	"github.com/google/uuid"
)

const deliveryWorkspaceYAML = `
schema_version: 2
name: engineering
repositories:
  api:
    provider: github
    repository: org/api
  web:
    provider: github
    repository: org/web
`

func createDeliveryRun(t *testing.T, store *workflowv2.Store, id string) workflowv2.Attempt {
	t.Helper()
	if _, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: id, TenantID: "tenant-1", WorkspaceYAML: []byte(deliveryWorkspaceYAML), WorkflowYAML: []byte(runtimeWorkflowYAML),
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.StartAttempt(context.Background(), id, "claw-delivery")
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

func deliveryVerifier(heads map[string]string) workflowv2.PullRequestVerifier {
	return workflowv2.PullRequestVerifierFunc(func(_ context.Context, _ workflowv2.Run, _ *typesv2.Workspace,
		claim typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		parts := strings.Split(strings.TrimPrefix(claim.URL, "https://github.example/org/"), "/pull/")
		if len(parts) != 2 {
			return typesv2.VerifiedPullRequest{}, fmt.Errorf("unrecognized URL")
		}
		number := 0
		if _, err := fmt.Sscanf(parts[1], "%d", &number); err != nil {
			return typesv2.VerifiedPullRequest{}, err
		}
		repositoryName := parts[0]
		return typesv2.VerifiedPullRequest{
			URL: claim.URL, RepositoryName: repositoryName, Repository: "org/" + repositoryName, Number: number,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: heads[claim.URL], State: "open",
			VerifiedAt: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC),
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl),
				Connection: "github", ObservedAt: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)},
		}, nil
	})
}

func TestSubmitDeliveryVerifiesMultipleWorkspaceRepositories(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-multi-pr")
	apiURL := "https://github.example/org/api/pull/10"
	webURL := "https://github.example/org/web/pull/20"
	verified, err := store.SubmitDelivery(context.Background(), "run-multi-pr", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: webURL}, {URL: apiURL}},
	}, deliveryVerifier(map[string]string{apiURL: "api-head-1", webURL: "web-head-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) != 2 || verified[0].Repository != "org/api" || verified[1].Repository != "org/web" {
		t.Fatalf("verified = %#v", verified)
	}
	inspection, err := store.InspectRun(context.Background(), "run-multi-pr")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Delivery.ActivePullRequests != 2 || inspection.Delivery.OpenPullRequests != 2 {
		t.Fatalf("delivery = %#v", inspection.Delivery)
	}
	deliveryFacts, ok := inspection.Facts["delivery"].(map[string]interface{})
	if !ok || deliveryFacts["count"] != float64(2) || deliveryFacts["all_merged"] != false {
		t.Fatalf("delivery facts = %#v", inspection.Facts["delivery"])
	}
}

func TestSubmitDeliveryRejectsAnyRepositoryOutsidePinnedWorkspaceAtomically(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-delivery-authority")
	apiURL := "https://github.example/org/api/pull/10"
	evilURL := "https://github.example/org/evil/pull/99"
	_, err := store.SubmitDelivery(context.Background(), "run-delivery-authority", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: apiURL}, {URL: evilURL}},
	}, deliveryVerifier(map[string]string{apiURL: "api-head", evilURL: "evil-head"}))
	if err == nil || !strings.Contains(err.Error(), "pinned workspace") {
		t.Fatalf("error = %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_prs WHERE run_id='run-delivery-authority'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("partially persisted %d pull requests", count)
	}
}

func TestSubmitDeliveryRollsBackWhenWorkflowEventFails(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-delivery-event-atomic")
	if _, err := db.Exec(`CREATE TRIGGER reject_delivery_event BEFORE INSERT ON workflow_v2_events
		WHEN NEW.kind='delivery.verified' BEGIN SELECT RAISE(ABORT, 'simulated event failure'); END`); err != nil {
		t.Fatal(err)
	}
	apiURL := "https://github.example/org/api/pull/10"
	_, err := store.SubmitDelivery(context.Background(), "run-delivery-event-atomic", attempt.ID,
		typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: apiURL}}},
		deliveryVerifier(map[string]string{apiURL: "api-head"}))
	if err == nil {
		t.Fatal("simulated workflow event failure did not fail delivery")
	}
	var deliveries, heads int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_prs WHERE run_id='run-delivery-event-atomic'`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_heads`).Scan(&heads); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 || heads != 0 {
		t.Fatalf("delivery/head rows persisted after event failure: %d/%d", deliveries, heads)
	}
	var eventCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id='run-delivery-event-atomic' AND kind='delivery.verified'`).Scan(
		&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("delivery events persisted = %d", eventCount)
	}
}

func TestDeliveryHeadChangeSupersedesHeadBoundEvidence(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-head-change")
	apiURL := "https://github.example/org/api/pull/10"
	verified, err := store.SubmitDelivery(context.Background(), "run-head-change", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: apiURL}},
	}, deliveryVerifier(map[string]string{apiURL: "head-1"}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixMilli()
	if _, err := db.Exec(`INSERT INTO workflow_v2_evidence(
		id,run_id,pr_id,head_sha,domain,connection,external_id,kind,status,payload_json,provenance_json,observed_at)
		VALUES(?,?,?,?,?,?,?,?,?,'{}','{}',?)`, uuid.NewString(), "run-head-change", verified[0].ID, "head-1",
		"ci", "github", "check-1", "required-checks", "success", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitDelivery(context.Background(), "run-head-change", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: apiURL}},
	}, deliveryVerifier(map[string]string{apiURL: "head-2"})); err != nil {
		t.Fatal(err)
	}
	var currentHead string
	var generations, superseded int
	if err := db.QueryRow(`SELECT current_head_sha FROM workflow_v2_delivery_prs WHERE id=?`, verified[0].ID).Scan(&currentHead); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_heads WHERE pr_id=?`, verified[0].ID).Scan(&generations)
	_ = db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE pr_id=? AND superseded_at>0`, verified[0].ID).Scan(&superseded)
	if currentHead != "head-2" || generations != 2 || superseded != 1 {
		t.Fatalf("head/generations/superseded = %s/%d/%d", currentHead, generations, superseded)
	}
}

func TestEmptyDeliveryManifestSupportsNoPRWorkflow(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-no-pr")
	verified, err := store.SubmitDelivery(context.Background(), "run-no-pr", attempt.ID, typesv2.DeliveryManifest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) != 0 {
		t.Fatalf("verified = %#v", verified)
	}
	inspection, err := store.InspectRun(context.Background(), "run-no-pr")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Delivery.ActivePullRequests != 0 {
		t.Fatalf("delivery = %#v", inspection.Delivery)
	}
	deliveryFacts, ok := inspection.Facts["delivery"].(map[string]interface{})
	if !ok || deliveryFacts["all_merged"] != true {
		t.Fatalf("zero-PR delivery should be vacuously merged: %#v", inspection.Facts["delivery"])
	}
}

func TestClawSupersessionCannotRemoveOpenDelivery(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-supersede-open")
	oldURL := "https://github.example/org/api/pull/10"
	newURL := "https://github.example/org/api/pull/11"
	if _, err := store.SubmitDelivery(context.Background(), "run-supersede-open", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: oldURL}},
	}, deliveryVerifier(map[string]string{oldURL: "old-head", newURL: "new-head"})); err != nil {
		t.Fatal(err)
	}
	_, err := store.SubmitDelivery(context.Background(), "run-supersede-open", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: newURL, Supersedes: oldURL}},
	}, deliveryVerifier(map[string]string{oldURL: "old-head", newURL: "new-head"}))
	if err == nil || !strings.Contains(err.Error(), "hub-owned") {
		t.Fatalf("supersession error = %v", err)
	}
	var active, total int
	_ = db.QueryRow(`SELECT COALESCE(SUM(active),0),COUNT(*) FROM workflow_v2_delivery_prs WHERE run_id='run-supersede-open'`).Scan(&active, &total)
	if active != 1 || total != 1 {
		t.Fatalf("active/total deliveries = %d/%d", active, total)
	}
}

func TestAttemptRevokedDuringVerificationCannotWriteDelivery(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-revoked-delivery")
	prURL := "https://github.example/org/api/pull/10"
	verifier := workflowv2.PullRequestVerifierFunc(func(_ context.Context, _ workflowv2.Run, _ *typesv2.Workspace,
		claim typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		if _, err := store.StartAttempt(context.Background(), "run-revoked-delivery", "replacement-claw"); err != nil {
			t.Fatal(err)
		}
		now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
		return typesv2.VerifiedPullRequest{URL: claim.URL, RepositoryName: "api", Repository: "org/api", Number: 10,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head", State: "open", VerifiedAt: now,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}, nil
	})
	_, err := store.SubmitDelivery(context.Background(), "run-revoked-delivery", attempt.ID,
		typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: prURL}}}, verifier)
	if err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("error = %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_prs WHERE run_id='run-revoked-delivery'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("revoked attempt wrote %d deliveries", count)
	}
}

func TestClawCannotSupersedeSourceControlConfirmedClosedPR(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-supersede-closed")
	oldURL := "https://github.example/org/api/pull/10"
	newURL := "https://github.example/org/api/pull/11"
	states := map[string]string{oldURL: "open", newURL: "open"}
	heads := map[string]string{oldURL: "old-head", newURL: "new-head"}
	verifier := workflowv2.PullRequestVerifierFunc(func(_ context.Context, _ workflowv2.Run, _ *typesv2.Workspace,
		claim typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		parts := strings.Split(strings.TrimPrefix(claim.URL, "https://github.example/org/api/pull/"), "/")
		number := 0
		_, _ = fmt.Sscanf(parts[0], "%d", &number)
		now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
		return typesv2.VerifiedPullRequest{URL: claim.URL, RepositoryName: "api", Repository: "org/api", Number: number,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: heads[claim.URL], State: states[claim.URL], VerifiedAt: now,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), ObservedAt: now}}, nil
	})
	if _, err := store.SubmitDelivery(context.Background(), "run-supersede-closed", attempt.ID,
		typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: oldURL}}}, verifier); err != nil {
		t.Fatal(err)
	}
	states[oldURL] = "closed"
	if _, err := store.SubmitDelivery(context.Background(), "run-supersede-closed", attempt.ID,
		typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: oldURL}}}, verifier); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitDelivery(context.Background(), "run-supersede-closed", attempt.ID,
		typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: newURL, Supersedes: oldURL}}}, verifier); err == nil ||
		!strings.Contains(err.Error(), "hub-owned") {
		t.Fatalf("error = %v", err)
	}
	var oldActive, newCount int
	if err := db.QueryRow(`SELECT active FROM workflow_v2_delivery_prs WHERE run_id='run-supersede-closed' AND url=?`, oldURL).Scan(&oldActive); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_prs WHERE run_id='run-supersede-closed' AND url=?`, newURL).Scan(&newCount); err != nil {
		t.Fatal(err)
	}
	if oldActive != 1 || newCount != 0 {
		t.Fatalf("old active/new count = %d/%d", oldActive, newCount)
	}
}

func TestClawSupersessionIsRejectedBeforeSourceControlLookup(t *testing.T) {
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt := createDeliveryRun(t, store, "run-supersede-authority")
	newURL := "https://github.example/org/api/pull/11"
	called := false
	_, err := store.SubmitDelivery(context.Background(), "run-supersede-authority", attempt.ID,
		typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{{URL: newURL, Supersedes: "old-pr"}}},
		workflowv2.PullRequestVerifierFunc(func(_ context.Context, _ workflowv2.Run, _ *typesv2.Workspace,
			_ typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
			called = true
			return typesv2.VerifiedPullRequest{}, nil
		}))
	if err == nil || !strings.Contains(err.Error(), "hub-owned") {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("untrusted supersession reached source-control verifier")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_prs WHERE run_id='run-supersede-authority'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("supersession persisted %d deliveries", count)
	}
}
