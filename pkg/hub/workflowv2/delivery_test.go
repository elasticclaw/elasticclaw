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
}
