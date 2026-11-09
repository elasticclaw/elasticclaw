package workflowv2_test

import (
	"context"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestMergeabilityChangesAndDeduplication(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt, pr := createPolicyDelivery(t, store, "mergeability", "head-1", "open")
	targets, err := store.ActiveDeliveryTargets(ctx, pr.Repository, pr.Number)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %v, %v", targets, err)
	}
	pr.BaseSHA = "base-1"
	for _, step := range []struct {
		state, base, kind  string
		conflicted, behind bool
	}{
		{"", "base-1", "", false, false},
		{"unknown", "base-1", "", false, false},
		{"clean", "base-1", "pull_request.mergeability_updated", false, false},
		{"dirty", "base-1", "pull_request.conflicted", true, false},
		{"dirty", "base-1", "", true, false},
		{"unknown", "base-1", "", true, false},
		{"behind", "base-1", "pull_request.behind", false, true},
		{"clean", "base-1", "pull_request.mergeable", false, false},
		{"clean", "base-1", "", false, false},
		{"dirty", "base-1", "pull_request.mergeability_updated", true, false},
		{"dirty", "base-2", "pull_request.conflicted", true, false},
	} {
		pr.MergeableState, pr.BaseSHA = step.state, step.base
		result, err := store.ReconcileMergeability(ctx, targets[0], pr)
		if err != nil {
			t.Fatalf("%s: %v", step.state, err)
		}
		if step.kind == "" {
			if result.EventID != "" {
				t.Fatalf("%s emitted %s", step.state, result.EventID)
			}
		} else {
			var kind string
			if err := db.QueryRow(`SELECT kind FROM workflow_v2_events WHERE id=?`, result.EventID).Scan(&kind); err != nil {
				t.Fatal(err)
			}
			if kind != step.kind {
				t.Fatalf("kind=%s, want %s", kind, step.kind)
			}
		}
		if result.EventID != "" {
			inspection, err := store.InspectRun(ctx, targets[0].RunID)
			if err != nil {
				t.Fatal(err)
			}
			delivery := inspection.Facts["delivery"].(map[string]interface{})
			if delivery["any_conflicted"] != step.conflicted || delivery["any_behind"] != step.behind {
				t.Fatalf("%s facts: %#v", step.state, delivery)
			}
		}
	}
	snapshot, err := store.Snapshot(ctx, targets[0].RunID, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Delivery) != 1 || snapshot.Delivery[0].MergeableState != "dirty" || snapshot.Delivery[0].BaseSHA != "base-2" {
		t.Fatalf("per-PR mergeability: %#v", snapshot.Delivery)
	}
	var conflicts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND kind='pull_request.conflicted'`, targets[0].RunID).Scan(&conflicts); err != nil {
		t.Fatal(err)
	}
	if conflicts != 2 {
		t.Fatalf("conflicted events = %d", conflicts)
	}
	if _, err := db.Exec(`UPDATE workflow_v2_delivery_prs SET registered_only=1 WHERE id=?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	pr.BaseSHA = "base-3"
	if _, err := store.ReconcileMergeability(ctx, targets[0], pr); err == nil {
		t.Fatal("registered-only PR accepted mergeability evidence")
	}
}

func TestMergeabilityKeepsMultiplePRFactsConsistent(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	attempt, first := createPolicyDelivery(t, store, "multiple-prs", "head-1", "open")
	second := first
	second.ID, second.Number, second.URL, second.HeadSHA = "", 11, "https://github.example/org/api/pull/11", "head-2"
	prs, err := store.SubmitDelivery(ctx, "multiple-prs", attempt.ID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: second.URL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace, typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return second, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	second = prs[0]
	targets, err := store.ActiveDeliveryTargetsAll(ctx)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets=%#v, err=%v", targets, err)
	}
	first.BaseSHA, first.MergeableState = "base", "dirty"
	second.BaseSHA, second.MergeableState = "base", "behind"
	if _, err := store.ReconcilePullRequest(ctx, targets[1], "second-pr", "pull_request.updated", second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileMergeability(ctx, targets[1], second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileMergeability(ctx, targets[0], first); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.InspectRun(ctx, "multiple-prs")
	if err != nil {
		t.Fatal(err)
	}
	prFacts := inspection.Facts["pull_request"].(map[string]interface{})
	if prFacts["id"] != first.ID || prFacts["number"] != float64(first.Number) || prFacts["head_sha"] != first.HeadSHA ||
		prFacts["url"] != first.URL || prFacts["mergeable_state"] != "dirty" {
		t.Fatalf("mixed PR facts: %#v", prFacts)
	}
	delivery := inspection.Facts["delivery"].(map[string]interface{})
	if delivery["any_conflicted"] != true || delivery["any_behind"] != true {
		t.Fatalf("aggregate facts=%#v", delivery)
	}
	// An uncomputed new head cannot retain the old head's conflict fact.
	first.HeadSHA = "head-3"
	first.MergeableState = ""
	if _, err := store.ReconcilePullRequest(ctx, targets[0], "first-new-head", "pull_request.head_changed", first); err != nil {
		t.Fatal(err)
	}
	inspection, err = store.InspectRun(ctx, "multiple-prs")
	if err != nil {
		t.Fatal(err)
	}
	prFacts = inspection.Facts["pull_request"].(map[string]interface{})
	delivery = inspection.Facts["delivery"].(map[string]interface{})
	if prFacts["mergeable_state"] != "" || delivery["any_conflicted"] != false || delivery["any_behind"] != true {
		t.Fatalf("stale mergeability facts: %#v / %#v", prFacts, delivery)
	}
	// The same state on a new head must produce a fresh observation.
	first.MergeableState = "dirty"
	targets[0].HeadSHA = first.HeadSHA
	result, err := store.ReconcileMergeability(ctx, targets[0], first)
	if err != nil || result.EventID == "" {
		t.Fatalf("new-head observation: %#v, %v", result, err)
	}
}

// The dedupe tuple lives in the delivery row, which a head move does not blank:
// the first observation on the new head fires its typed event exactly once.
func TestMergeabilityAfterHeadMoveFiresOnNewHead(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeDB(t)
	store := workflowv2.NewStore(db)
	_, pr := createPolicyDelivery(t, store, "mergeability-head", "head-1", "open")
	target := func() workflowv2.DeliveryTarget {
		targets, err := store.ActiveDeliveryTargets(ctx, pr.Repository, pr.Number)
		if err != nil || len(targets) != 1 {
			t.Fatalf("targets: %v, %v", targets, err)
		}
		return targets[0]
	}
	kindOf := func(result workflowv2.EventResult) string {
		if result.EventID == "" {
			return ""
		}
		var kind string
		if err := db.QueryRow(`SELECT kind FROM workflow_v2_events WHERE id=?`, result.EventID).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		return kind
	}
	pr.BaseSHA, pr.MergeableState = "base-1", "dirty"
	if result, err := store.ReconcileMergeability(ctx, target(), pr); err != nil || kindOf(result) != "pull_request.conflicted" {
		t.Fatalf("head-1 dirty = %#v, %v", result, err)
	}
	pr.HeadSHA = "head-2"
	if _, err := store.ReconcilePullRequest(ctx, target(), "push", "pull_request.head_changed", pr); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"pull_request.conflicted", ""} {
		result, err := store.ReconcileMergeability(ctx, target(), pr)
		if err != nil || kindOf(result) != want {
			t.Fatalf("head-2 dirty #%d = %q, %v; want %q", i, kindOf(result), err, want)
		}
	}
	pr.MergeableState = "clean"
	if result, err := store.ReconcileMergeability(ctx, target(), pr); err != nil || kindOf(result) != "pull_request.mergeable" {
		t.Fatalf("head-2 clean = %q, %v", kindOf(result), err)
	}
}
