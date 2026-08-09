package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

const workflowV2GitHubWorkspaceYAML = `
schema_version: 2
name: github-v2
repositories:
  api:
    provider: github
    repository: org/api
    source_control: github
source_control:
  connections:
    github:
      provider: github
ci:
  connections:
    github-actions:
      provider: github_actions
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

const workflowV2GitHubWorkflowYAML = `
schema_version: 2
name: github-delivery
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
            prompt: Address the trusted review feedback and update the pull request.
            include_facts: [review.feedback]
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

func TestWorkflowV2GitHubCIAndReviewUseTypedRuntimeOnly(t *testing.T) {
	oldTransport := http.DefaultTransport
	http.DefaultTransport = githubAppTokenTransport{base: oldTransport}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/check-runs") {
			_, _ = w.Write([]byte(`{"check_runs":[
                {"id":101,"name":"lint","status":"completed","conclusion":"success","completed_at":"2026-08-09T12:00:00Z"},
                {"id":102,"name":"unit","status":"completed","conclusion":"success","completed_at":"2026-08-09T12:00:01Z"}
            ]}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(gh.Close)

	cfg := &types.HubConfig{
		ClawToken:  "claw-token",
		GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}},
		Factories: []*types.FactoryConfig{{Name: "token-scope", Integration: "github", Template: "elasticclaw",
			Provider: "noop", Repos: []string{"org/api"}, WebhookSecret: "secret",
			Trigger: &types.GitHubTrigger{On: "pull_request"}}},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}
	s, db := NewTestServerWithConfig(t, cfg, gh.URL, "", "")
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,provider,status,created_at)
		VALUES('claw-v2-github','test-tenant-id','v2-github','github-v2','noop','connected',?)`, now()); err != nil {
		t.Fatal(err)
	}
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-v2-github", TenantID: "test-tenant-id", InitialClawID: "claw-v2-github",
		WorkspaceYAML: []byte(workflowV2GitHubWorkspaceYAML), WorkflowYAML: []byte(workflowV2GitHubWorkflowYAML),
	})
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/org/api/pull/10"
	verified, err := store.SubmitDelivery(context.Background(), run.ID, run.CurrentAttemptID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: prURL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		observed := time.Date(2026, 8, 9, 11, 59, 0, 0, time.UTC)
		return typesv2.VerifiedPullRequest{URL: prURL, RepositoryName: "api", Repository: "org/api", Number: 10,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: observed,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl),
				Connection: "github", ObservedAt: observed}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(verified) != 1 {
		t.Fatalf("verified delivery = %#v", verified)
	}

	var checkPayload githubCheckPayload
	checkPayload.Action = "completed"
	checkPayload.CheckRun.HeadSHA = "head-1"
	// GitHub omits pull_requests for some fork checks. V2 has no transcript
	// fallback or legacy PR poller, so resolve the independently verified head.
	checkPayload.Repository.FullName = "org/api"
	// Exercise the shared webhook entrypoint: V2 consumes typed evidence while
	// the unchanged V1 path remains inert because there is no legacy claw_pr row.
	s.processGitHubCheckEvent("check_run", checkPayload)
	policy, err := store.EvaluateDeliveryPolicy(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.CISatisfied || policy.ReviewSatisfied {
		t.Fatalf("policy after CI = %#v", policy)
	}

	var review githubPRReviewPayload
	review.Action = "submitted"
	review.Review.ID = 77
	review.Review.State = "changes_requested"
	review.Review.Body = "Handle the nil response before dereferencing it."
	review.Review.HTMLURL = prURL + "#pullrequestreview-77"
	review.Review.User.Login = "alice"
	review.Review.User.Type = "User"
	review.PullRequest.Number = 10
	review.PullRequest.HTMLURL = prURL
	review.Repository.FullName = "org/api"
	s.processGitHubPRReviewEvent(review)
	if err := s.drainWorkflowV2Effects(context.Background(), "test-v2-github-worker"); err != nil {
		t.Fatal(err)
	}
	var state, phase, instructions string
	if err := db.QueryRow(`SELECT state,display_phase FROM workflow_v2_runs WHERE id=?`, run.ID).Scan(&state, &phase); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT instructions FROM workflow_v2_agent_tasks WHERE run_id=?`, run.ID).Scan(&instructions); err != nil {
		t.Fatal(err)
	}
	if state != "fixing" || phase != "build" || !strings.Contains(instructions, "Handle the nil response") {
		t.Fatalf("state/phase/instructions = %q/%q/%s", state, phase, instructions)
	}
	var evidence, outbox, messages int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE run_id=?`, run.ID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_control_outbox WHERE run_id=?`, run.ID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id='claw-v2-github'`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if evidence != 3 || outbox != 1 || messages != 0 {
		t.Fatalf("evidence/outbox/messages = %d/%d/%d", evidence, outbox, messages)
	}
}
