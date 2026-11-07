package hub

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

type workflowV2A3Transport func(*http.Request) (*http.Response, error)

func (f workflowV2A3Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newWorkflowV2A3Server(t *testing.T, response func(*http.Request) string) *Server {
	t.Helper()
	oldTransport, oldClient := http.DefaultTransport, defaultGitHubClient
	http.DefaultTransport = githubAppTokenTransport{base: workflowV2A3Transport(func(r *http.Request) (*http.Response, error) {
		return githubAppTokenResponse(http.StatusOK, response(r)), nil
	})}
	defaultGitHubClient = newGitHubClient()
	t.Cleanup(func() { http.DefaultTransport, defaultGitHubClient = oldTransport, oldClient })
	cfg := &types.HubConfig{
		GitHubApps: []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: testGitHubAppPEM(t)}},
		Factories: []*types.FactoryConfig{{Name: "token-scope", Integration: "github", Template: "elasticclaw",
			Provider: "noop", Repos: []string{"org/api"}, Trigger: &types.GitHubTrigger{On: "pull_request"}}},
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}},
	}
	s, _ := NewTestServerWithConfig(t, cfg, "https://github-a3.test", "", "")
	return s
}

func createWorkflowV2A3Delivery(t *testing.T, s *Server, id, workspace string) workflowv2.Run {
	t.Helper()
	store := workflowv2.NewStore(s.db)
	if _, err := s.db.Exec(`INSERT INTO claws(id,tenant_id,name,template,provider,status,created_at)
		VALUES(?,'test-tenant-id',?,'github-v2','noop','connected',?)`, id, id, now()); err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
		ID: id, TenantID: "test-tenant-id", InitialClawID: id,
		WorkspaceYAML: []byte(workspace), WorkflowYAML: []byte(workflowV2GitHubWorkflowYAML),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.SubmitDelivery(context.Background(), run.ID, run.CurrentAttemptID, typesv2.DeliveryManifest{
		PullRequests: []typesv2.PullRequestClaim{{URL: "https://github.com/org/api/pull/10"}},
	}, workflowv2.PullRequestVerifierFunc(func(context.Context, workflowv2.Run, *typesv2.Workspace, typesv2.PullRequestClaim) (typesv2.VerifiedPullRequest, error) {
		observed := time.Now().UTC()
		return typesv2.VerifiedPullRequest{URL: "https://github.com/org/api/pull/10", RepositoryName: "api", Repository: "org/api", Number: 10,
			SourceBranch: "feature", BaseBranch: "main", HeadSHA: "head-1", State: "open", VerifiedAt: observed,
			Provenance: typesv2.EvidenceProvenance{Producer: string(workflowv2.ProducerSourceControl), Connection: "github", ObservedAt: observed}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestWorkflowV2A3CheckNameAndCommitStatusEvidence(t *testing.T) {
	for _, appSlug := range []string{"", "depot"} {
		t.Run("app="+appSlug, func(t *testing.T) {
			statusState := "success"
			actionsCalls, statusCalls := 0, 0
			s := newWorkflowV2A3Server(t, func(r *http.Request) string {
				switch {
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					unit := ""
					if appSlug != "" {
						unit = `,{"id":3,"name":"unit","status":"completed","conclusion":"success","app":{"slug":"depot"}},{"id":4,"name":"unit","status":"completed","conclusion":"failure","app":{"slug":"other"}}`
					}
					return `{"check_runs":[{"id":1,"name":"lint","status":"completed","conclusion":"success","app":{"slug":"depot"}},
						{"id":2,"name":"unrelated","status":"completed","conclusion":"failure","app":{"slug":"other"}}` + unit + `]}`
				case strings.HasSuffix(r.URL.Path, "/status"):
					statusCalls++
					return fmt.Sprintf(`{"statuses":[{"id":10,"context":"unit","state":%q,"updated_at":"2026-11-06T12:00:00Z"}]}`, statusState)
				case strings.Contains(r.URL.Path, "/actions/runs"):
					actionsCalls++
					return `{"workflow_runs":[]}`
				default:
					t.Fatalf("unexpected API path %s", r.URL.Path)
					return ""
				}
			})
			fields := "check_name: '[lu]*'"
			if appSlug != "" {
				fields += "\n      app_slug: " + appSlug
			}
			run := createWorkflowV2A3Delivery(t, s, "check-names", strings.Replace(workflowV2GitHubWorkspaceYAML, "workflow: ci.yml", fields, 1))
			store := workflowv2.NewStore(s.db)
			payload := githubCheckPayload{Action: "completed"}
			payload.Repository.FullName, payload.CheckRun.HeadSHA = "org/api", "head-1"
			s.processWorkflowV2GitHubCheckEvent("check_run", payload)
			policy, err := store.EvaluateDeliveryPolicy(context.Background(), run.ID)
			if err != nil || !policy.CISatisfied {
				t.Fatalf("policy=%#v, err=%v", policy, err)
			}
			if actionsCalls != 0 {
				t.Fatalf("check-name pipeline fetched Actions runs %d times", actionsCalls)
			}
			if appSlug != "" && statusCalls != 0 {
				t.Fatal("app-scoped pipeline fetched unattributable statuses")
			}
			if appSlug == "" {
				statusState = "failure"
				s.processWorkflowV2GitHubCheckEvent("check_run", payload)
				policy, err = store.EvaluateDeliveryPolicy(context.Background(), run.ID)
				if err != nil || policy.CIStatus != "unsatisfied" {
					t.Fatalf("failed status policy=%#v, err=%v", policy, err)
				}
			}
		})
	}
}

func TestWorkflowV2A3MergeabilityWebhookAndReconciler(t *testing.T) {
	state, mergeable, base := "dirty", "false", "base-1"
	gets := 0
	s := newWorkflowV2A3Server(t, func(r *http.Request) string {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/10"):
			gets++
			return fmt.Sprintf(`{"html_url":"https://github.com/org/api/pull/10","state":"open","mergeable":%s,"mergeable_state":%q,
				"head":{"sha":"head-1","ref":"feature"},"base":{"sha":%q,"ref":"main"}}`, mergeable, state, base)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			return `{"check_runs":[]}`
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			return `{"workflow_runs":[]}`
		case strings.HasSuffix(r.URL.Path, "/reviews"), strings.HasSuffix(r.URL.Path, "/comments"):
			return `[]`
		default:
			t.Fatalf("unexpected API path %s", r.URL.Path)
			return ""
		}
	})
	run := createWorkflowV2A3Delivery(t, s, "merge-a", workflowV2GitHubWorkspaceYAML)
	createWorkflowV2A3Delivery(t, s, "merge-b", workflowV2GitHubWorkspaceYAML)
	if _, err := s.db.Exec(`UPDATE workflow_v2_runs SET status='suspended',waiting_reason='manual' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		state, mergeable, base   string
		conflicts, behind, clean int
	}{
		{"dirty", "null", "base-1", 0, 0, 0},
		{"dirty", "false", "base-1", 1, 0, 0},
		{"dirty", "false", "base-1", 1, 0, 0},
		{"behind", "true", "base-2", 1, 1, 0},
		{"clean", "true", "base-2", 1, 1, 1},
	} {
		state, mergeable, base = step.state, step.mergeable, step.base
		before := gets
		if err := s.reconcileWorkflowV2GitHub(context.Background()); err != nil {
			t.Fatal(err)
		}
		if gets-before != 1 {
			t.Fatalf("PR GETs per cycle = %d, want 1 for shared PR", gets-before)
		}
		var payload githubPRPayload
		payload.Action, payload.Number, payload.Repository.FullName = "edited", 10, "org/api"
		s.processWorkflowV2GitHubPREvent(payload)
		for kind, want := range map[string]int{"conflicted": step.conflicts, "behind": step.behind, "mergeable": step.clean} {
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND kind=?`, run.ID, "pull_request."+kind).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != want {
				t.Fatalf("%s: %d events, want %d", kind, count, want)
			}
		}
	}
}
