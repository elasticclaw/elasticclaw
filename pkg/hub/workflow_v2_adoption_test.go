package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
)

const adoptionWorkspace = `
schema_version: 2
name: adoption
repositories:
  repo:
    provider: github
    repository: owner/repo
`

const adoptionWorkflow = `
schema_version: 2
name: adoption
enabled: true
initial_state: working
states:
  working:
    phase: build
  reviewing:
    phase: review
transitions:
  adopted:
    from: working
    on: delivery.adopted
    to: reviewing
delivery:
  pull_requests:
    branches:
      - repository: repo
        pattern: deps/ts-minor-*
`

func TestWorkflowV2ActivationAdoption(t *testing.T) {
	for _, tc := range []struct {
		name, login, userType, branch, headRepo, state string
		fail, noApp, legacy, paginate                  bool
		want                                           int
	}{
		{name: "own bot", want: 1},
		{name: "pagination", paginate: true, want: 1},
		{name: "human", login: "contributor", userType: "User"},
		{name: "other bot", login: "dependabot[bot]"},
		{name: "wrong branch", branch: "deps/ts-major-1"},
		{name: "own bot on fork", headRepo: "someone/repo"},
		{name: "closed", state: "closed"},
		{name: "GitHub unavailable", fail: true},
		{name: "no App identity", noApp: true},
		{name: "legacy ownership", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, db := newTaskRunAnalyticsTestServer(t, "adoption-claw")
			parent, _ := startTaskRunForTest(t, s, "adoption-claw", "adoption")
			branchSweepTestToken(s)
			if tc.noApp {
				s.hubCfg.GitHubApps[0].URL = ""
			}
			oldClient := defaultGitHubClient
			defaultGitHubClient = newGitHubClient()
			t.Cleanup(func() { defaultGitHubClient = oldClient })
			calls := 0
			defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.fail {
					return nil, fmt.Errorf("GitHub unavailable")
				}
				if r.URL.Path != "/repos/owner/repo/pulls" || r.URL.Query().Get("state") != "open" || r.Header.Get("Authorization") != "Bearer repo-token" {
					t.Fatalf("request=%s", r.URL)
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("missing deadline")
				}
				if tc.paginate && r.URL.Query().Get("page") == "" {
					response := githubAppTokenResponse(200, `[]`)
					response.Header.Set("Link", `<https://api.github.com/repos/owner/repo/pulls?state=open&per_page=100&page=2>; rel="next"`)
					return response, nil
				}
				login, userType, branch, headRepo, state := tc.login, tc.userType, tc.branch, tc.headRepo, tc.state
				if login == "" {
					login = "factory-app[bot]"
				}
				if userType == "" {
					userType = "Bot"
				}
				if branch == "" {
					branch = "deps/ts-minor-1"
				}
				if headRepo == "" {
					headRepo = "owner/repo"
				}
				if state == "" {
					state = "open"
				}
				body, _ := json.Marshal([]any{map[string]any{"number": 1, "html_url": "https://github.com/owner/repo/pull/1", "state": state, "user": map[string]string{"login": login, "type": userType}, "head": map[string]any{"ref": branch, "sha": "head", "repo": map[string]string{"full_name": headRepo}}, "base": map[string]string{"ref": "main"}}})
				return githubAppTokenResponse(200, string(body)), nil
			})}
			if tc.legacy {
				if _, err := db.Exec(`INSERT INTO claw_prs(claw_id,repo,pr_number,pr_url,source,mention_only,created_at) VALUES('adoption-claw','owner/repo',1,'https://github.com/owner/repo/pull/1','gh_create',1,datetime('now'))`); err != nil {
					t.Fatal(err)
				}
			}
			store := workflowv2.NewStore(db)
			run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "adoption-run", TenantID: "test-tenant-id", InitialClawID: "adoption-claw", TaskRunID: parent, WorkspaceYAML: []byte(adoptionWorkspace), WorkflowYAML: []byte(adoptionWorkflow), ActivationPending: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AssembleOrganizationContext(ctx, run.ID, workspaceFileKnowledgeResolver(nil)); err != nil {
				t.Fatal(err)
			}
			if err := s.completeWorkflowV2Activation(ctx, store, run.ID); err != nil {
				t.Fatal(err)
			}
			inspection, err := store.InspectRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if inspection.Run.Status != workflowv2.RunActive || inspection.Delivery.ActivePullRequests != tc.want {
				t.Fatalf("run=%+v delivery=%+v", inspection.Run, inspection.Delivery)
			}
			facts := inspection.Facts["delivery"].(map[string]interface{})
			if facts["adopted"] != (tc.want > 0) || facts["adopted_count"] != float64(tc.want) {
				t.Fatalf("facts=%v", facts)
			}
			if tc.want > 0 && inspection.Run.State != "reviewing" {
				t.Fatalf("state=%s", inspection.Run.State)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_prs WHERE run_id=?`, parent).Scan(&count); err != nil || count != tc.want {
				t.Fatalf("mirrored=%d err=%v", count, err)
			}
			if tc.fail || tc.noApp {
				var payload string
				if err := db.QueryRow(`SELECT payload_json FROM workflow_v2_events WHERE run_id=? AND kind='delivery.adoption_checked'`, run.ID).Scan(&payload); err != nil || !strings.Contains(payload, "adoption_error") {
					t.Fatalf("payload=%s err=%v", payload, err)
				}
			}
			if tc.paginate && calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
