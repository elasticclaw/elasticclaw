package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestWorkflowV2PeriodicReconciliationRecoversLostGitHubWebhooks(t *testing.T) {
	oldTransport := http.DefaultTransport
	oldClient := defaultGitHubClient
	http.DefaultTransport = githubAppTokenTransport{base: oldTransport}
	defaultGitHubClient = newGitHubClient()
	t.Cleanup(func() {
		http.DefaultTransport = oldTransport
		defaultGitHubClient = oldClient
	})

	var pullRequests, checks, workflows, reviews, inlineComments, discussionComments atomic.Int64
	var exposeWebhookComment atomic.Bool
	var editWebhookComment atomic.Bool
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repos/org/api/pulls/10"):
			pullRequests.Add(1)
			_, _ = w.Write([]byte(`{"id":410,"html_url":"https://github.com/org/api/pull/10","state":"open","merged":false,
				"head":{"sha":"head-2","ref":"feature"},"base":{"ref":"main"}}`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			checks.Add(1)
			_, _ = w.Write([]byte(`{"check_runs":[
				{"id":101,"name":"lint","status":"completed","conclusion":"success","completed_at":"2026-08-09T12:00:00Z","app":{"slug":"github-actions"},"check_suite":{"id":501}},
				{"id":102,"name":"unit","status":"completed","conclusion":"success","completed_at":"2026-08-09T12:00:01Z","app":{"slug":"github-actions"},"check_suite":{"id":502}}
			]}`))
		case strings.Contains(r.URL.Path, "/actions/runs"):
			workflows.Add(1)
			_, _ = w.Write([]byte(`{"workflow_runs":[
				{"name":"CI","path":".github/workflows/ci.yml","check_suite_id":501},
				{"name":"CI","path":".github/workflows/ci.yml","check_suite_id":502}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/repos/org/api/pulls/10/reviews"):
			reviews.Add(1)
			_, _ = w.Write([]byte(`[
				{"id":77,"state":"CHANGES_REQUESTED","body":"Recover this missed review.",
				 "html_url":"https://github.com/org/api/pull/10#pullrequestreview-77","commit_id":"head-2",
				 "submitted_at":"2026-08-09T12:01:00Z","user":{"login":"alice","type":"User"}},
				{"id":76,"state":"APPROVED","body":"stale","commit_id":"head-1",
				 "submitted_at":"2026-08-09T11:00:00Z","user":{"login":"bob","type":"User"}}
			]`))
		case strings.HasSuffix(r.URL.Path, "/repos/org/api/pulls/10/comments"):
			inlineComments.Add(1)
			body := `[
				{"id":88,"body":"Recover this missed inline comment.",
				 "html_url":"https://github.com/org/api/pull/10#discussion_r88","path":"api.go","line":42,
				 "commit_id":"head-2","created_at":"2026-08-09T12:02:00Z","updated_at":"2026-08-09T12:02:00Z",
				 "user":{"login":"carol","type":"User"}},
				{"id":87,"body":"old head","commit_id":"head-1","created_at":"2026-08-09T11:02:00Z",
				 "updated_at":"2026-08-09T11:02:00Z","user":{"login":"dave","type":"User"}}
			`
			if exposeWebhookComment.Load() {
				commentBody := "Webhook and poll see the same feedback."
				updatedAt := "2026-08-09T12:03:00Z"
				if editWebhookComment.Load() {
					commentBody = "Edited feedback converges without another fix task."
					updatedAt = "2026-08-09T12:04:00Z"
				}
				body += `,{"id":89,"body":"` + commentBody + `",
				 "html_url":"https://github.com/org/api/pull/10#discussion_r89","path":"api.go","line":43,
				 "commit_id":"head-2","created_at":"2026-08-09T12:03:00Z","updated_at":"` + updatedAt + `",
				 "user":{"login":"erin","type":"User"}}`
			}
			_, _ = w.Write([]byte(body + `]`))
		case strings.HasSuffix(r.URL.Path, "/repos/org/api/issues/10/comments"):
			discussionComments.Add(1)
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
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
		VALUES('claw-v2-reconcile','test-tenant-id','v2-reconcile','github-v2','noop','connected',?)`, now()); err != nil {
		t.Fatal(err)
	}
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: "run-v2-reconcile", TenantID: "test-tenant-id", InitialClawID: "claw-v2-reconcile",
		WorkspaceYAML: []byte(workflowV2GitHubWorkspaceYAML), WorkflowYAML: []byte(workflowV2GitHubWorkflowYAML),
	})
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/org/api/pull/10"
	observed := time.Date(2026, 8, 9, 11, 0, 0, 0, time.UTC)
	if _, err := store.SubmitDelivery(context.Background(), run.ID, run.CurrentAttemptID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: prURL}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace,
		typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		return typesv2.VerifiedPullRequest{URL: prURL, RepositoryName: "api", Repository: "org/api", Number: 10,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: observed,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl),
				Connection: "github", ObservedAt: observed}}, nil
	})); err != nil {
		t.Fatal(err)
	}

	// No webhook entrypoint is called: the periodic pass must recover every
	// authoritative domain observation from GitHub.
	if err := s.reconcileWorkflowV2GitHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.drainWorkflowV2Effects(context.Background(), "test-v2-reconcile-worker"); err != nil {
		t.Fatal(err)
	}
	var head, prState, runState, phase, instructions string
	if err := db.QueryRow(`SELECT current_head_sha,state FROM workflow_v2_delivery_prs WHERE run_id=?`,
		run.ID).Scan(&head, &prState); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state,display_phase FROM workflow_v2_runs WHERE id=?`, run.ID).
		Scan(&runState, &phase); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT instructions FROM workflow_v2_agent_tasks WHERE run_id=?`, run.ID).
		Scan(&instructions); err != nil {
		t.Fatal(err)
	}
	if head != "head-2" || prState != "open" || runState != "fixing" || phase != "build" ||
		!strings.Contains(instructions, "Recover this missed inline comment") {
		t.Fatalf("head/pr/run/phase/instructions = %q/%q/%q/%q/%q", head, prState, runState, phase, instructions)
	}
	var currentEvidence, messages, eventCount, taskCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_evidence WHERE run_id=? AND superseded_at=0`,
		run.ID).Scan(&currentEvidence); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id='claw-v2-reconcile'`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=?`, run.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_agent_tasks WHERE run_id=?`, run.ID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if currentEvidence != 3 || messages != 0 || pullRequests.Load() != 1 || checks.Load() != 1 ||
		workflows.Load() != 1 || reviews.Load() != 1 || inlineComments.Load() != 1 || discussionComments.Load() != 1 {
		t.Fatalf("evidence/messages/API calls = %d/%d/%d/%d/%d/%d/%d/%d", currentEvidence, messages,
			pullRequests.Load(), checks.Load(), workflows.Load(), reviews.Load(), inlineComments.Load(), discussionComments.Load())
	}
	var webhookComment githubPRReviewCommentPayload
	webhookComment.Action = "created"
	webhookComment.Comment.ID = 89
	webhookComment.Comment.Body = "Webhook and poll see the same feedback."
	webhookComment.Comment.HTMLURL = "https://github.com/org/api/pull/10#discussion_r89"
	webhookComment.Comment.Path = "api.go"
	webhookComment.Comment.Line = 43
	webhookComment.Comment.CommitID = "head-2"
	webhookComment.Comment.UpdatedAt = "2026-08-09T12:03:00Z"
	webhookComment.Comment.User.Login = "erin"
	webhookComment.Comment.User.Type = "User"
	webhookComment.PullRequest.Number = 10
	webhookComment.PullRequest.HTMLURL = prURL
	webhookComment.Repository.FullName = "org/api"
	s.processGitHubPRReviewCommentEvent(webhookComment)
	var feedbackAfterWebhook int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events
		WHERE run_id=? AND kind='review.feedback.received'`, run.ID).Scan(&feedbackAfterWebhook); err != nil {
		t.Fatal(err)
	}
	exposeWebhookComment.Store(true)

	// A second pass converges the CI policy fact to the aggregate revision that
	// includes the review snapshot written later in the first pass.
	if err := s.reconcileWorkflowV2GitHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	var convergedEvents, convergedTasks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=?`, run.ID).Scan(&convergedEvents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_agent_tasks WHERE run_id=?`, run.ID).Scan(&convergedTasks); err != nil {
		t.Fatal(err)
	}
	var feedbackAfterPoll int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events
		WHERE run_id=? AND kind='review.feedback.received'`, run.ID).Scan(&feedbackAfterPoll); err != nil {
		t.Fatal(err)
	}
	if feedbackAfterPoll != feedbackAfterWebhook {
		t.Fatalf("webhook feedback count changed from %d to %d after polling the same provider comment",
			feedbackAfterWebhook, feedbackAfterPoll)
	}
	if convergedTasks != taskCount {
		t.Fatalf("reconciliation convergence changed task count from %d to %d", taskCount, convergedTasks)
	}
	editWebhookComment.Store(true)
	if err := s.reconcileWorkflowV2GitHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	var receivedAfterEdit, reconciledAfterEdit, tasksAfterEdit int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events
		WHERE run_id=? AND kind='review.feedback.received'`, run.ID).Scan(&receivedAfterEdit); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events
		WHERE run_id=? AND kind='review.feedback.reconciled'`, run.ID).Scan(&reconciledAfterEdit); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_agent_tasks WHERE run_id=?`, run.ID).Scan(&tasksAfterEdit); err != nil {
		t.Fatal(err)
	}
	var feedbackJSON string
	if err := db.QueryRow(`SELECT value_json FROM workflow_v2_facts
		WHERE run_id=? AND fact_key='review.feedback'`, run.ID).Scan(&feedbackJSON); err != nil {
		t.Fatal(err)
	}
	if receivedAfterEdit != feedbackAfterWebhook || reconciledAfterEdit != 1 || tasksAfterEdit != convergedTasks ||
		!strings.Contains(feedbackJSON, "Edited feedback converges") {
		t.Fatalf("edited feedback received/reconciled/tasks/fact = %d/%d/%d/%s",
			receivedAfterEdit, reconciledAfterEdit, tasksAfterEdit, feedbackJSON)
	}
	var revisedEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=?`, run.ID).Scan(&revisedEvents); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileWorkflowV2GitHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	var replayEvents, replayTasks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=?`, run.ID).Scan(&replayEvents); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_agent_tasks WHERE run_id=?`, run.ID).Scan(&replayTasks); err != nil {
		t.Fatal(err)
	}
	if replayEvents != revisedEvents || replayTasks != convergedTasks {
		t.Fatalf("reconciliation replay changed events/tasks from %d/%d to %d/%d",
			revisedEvents, convergedTasks, replayEvents, replayTasks)
	}
}

func TestWorkflowV2ReconciledMergeWinsOverSimultaneousHeadChange(t *testing.T) {
	target := workflowv2.DeliveryTarget{HeadSHA: "head-1", State: "open"}
	verified := typesv2.VerifiedPullRequest{HeadSHA: "head-2", State: "merged"}
	if got := workflowV2ReconciledPREventKind(target, verified); got != "pull_request.merged" {
		t.Fatalf("event kind = %q, want pull_request.merged", got)
	}
}
