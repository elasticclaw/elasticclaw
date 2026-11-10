package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
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
		name, login, userType, branch, headRepo, state, url string
		fail, noApp, legacy, paginate, reconcile            bool
		want                                                int
	}{
		{name: "own bot", want: 1},
		{name: "GitHub canonical case", headRepo: "Owner/Repo", url: "https://github.com/Owner/Repo/pull/1", reconcile: true, want: 1},
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
				if !assertAdoptionLookupRequest(t, r) {
					return githubAppTokenResponse(404, `{}`), nil
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
				prURL := tc.url
				if prURL == "" {
					prURL = "https://github.com/owner/repo/pull/1"
				}
				body, _ := json.Marshal([]any{map[string]any{"number": 1, "html_url": prURL, "state": state, "user": map[string]string{"login": login, "type": userType}, "head": map[string]any{"ref": branch, "sha": "head", "repo": map[string]string{"full_name": headRepo}}, "base": map[string]string{"ref": "main"}}})
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
			if tc.reconcile {
				assertAdoptedPRReconciles(t, s, store, run.ID, tc.url)
			}
		})
	}
}

func TestWorkflowV2CronAdoptionFinishingRunSkipsProvisioning(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}}}, "", "", "")
	branchSweepTestToken(s)
	oldClient := defaultGitHubClient
	defaultGitHubClient = newGitHubClient()
	t.Cleanup(func() { defaultGitHubClient = oldClient })
	defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := json.Marshal([]any{map[string]any{"number": 1, "html_url": "https://github.com/owner/repo/pull/1", "state": "open",
			"user": map[string]string{"login": "factory-app[bot]", "type": "Bot"},
			"head": map[string]any{"ref": "deps/ts-minor-1", "sha": "head", "repo": map[string]string{"full_name": "owner/repo"}},
			"base": map[string]string{"ref": "main"}}})
		return githubAppTokenResponse(200, string(body)), nil
	})}
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "adoption", Files: map[string]string{
		"elasticclaw-config.yaml": adoptionWorkspace + "execution:\n  provider: noop\n",
	}}, []*types.WorkflowConfig{{Name: "adoption", RawConfig: strings.Replace(adoptionWorkflow, `  reviewing:
    phase: review`, `  reviewing:
    phase: done
    terminal: true`, 1) + "trigger:\n  cron:\n    schedule: \"0 9 * * *\"\n"}})
	s.cronSchedulerV2 = newCronSchedulerV2(s)

	if _, err := s.cronSchedulerV2.manualTrigger("adoption", "adoption", "test-tenant-id"); err != nil {
		t.Fatal(err)
	}
	var runStatus, clawID string
	if err := db.QueryRow(`SELECT r.status,c.claw_id FROM workflow_v2_cron_runs c JOIN workflow_v2_runs r ON r.id=c.v2_run_id`).
		Scan(&runStatus, &clawID); err != nil {
		t.Fatal(err)
	}
	if runStatus != string(workflowv2.RunCompleted) {
		t.Fatalf("run=%s", runStatus)
	}
	// Cron history and the overlap slot are finished before the trigger
	// returns, so the next fire is not skipped while the parent finishes.
	var cronStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_cron_runs`).Scan(&cronStatus); err != nil || cronStatus == "running" {
		t.Fatalf("cron=%s err=%v", cronStatus, err)
	}
	s.cronSchedulerV2.runningMu.Lock()
	running := s.cronSchedulerV2.running["adoption/adoption"]
	s.cronSchedulerV2.runningMu.Unlock()
	if running != 0 {
		t.Fatalf("cron slot still held: %d", running)
	}
	// The parent claw is finished in the background, never provisioned (noop
	// would connect it).
	deadline := time.Now().Add(5 * time.Second)
	for {
		var clawStatus, providerID string
		if err := db.QueryRow(`SELECT status,provider_id FROM claws WHERE id=?`, clawID).Scan(&clawStatus, &providerID); err != nil {
			t.Fatal(err)
		}
		if providerID != "" {
			t.Fatalf("claw %s was provisioned: provider=%s", clawStatus, providerID)
		}
		if clawStatus == "deleted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent claw not finished: %s", clawStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWorkflowV2ActivationAdoptionKeepsContextSuspension(t *testing.T) {
	ctx := context.Background()
	s, db := newTaskRunAnalyticsTestServer(t, "adoption-claw")
	parent, _ := startTaskRunForTest(t, s, "adoption-claw", "adoption")
	branchSweepTestToken(s)
	oldClient := defaultGitHubClient
	defaultGitHubClient = newGitHubClient()
	t.Cleanup(func() { defaultGitHubClient = oldClient })
	calls := 0
	defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := json.Marshal([]any{map[string]any{"number": 1, "html_url": "https://github.com/owner/repo/pull/1", "state": "open",
			"user": map[string]string{"login": "factory-app[bot]", "type": "Bot"},
			"head": map[string]any{"ref": "deps/ts-minor-1", "sha": "head", "repo": map[string]string{"full_name": "owner/repo"}},
			"base": map[string]string{"ref": "main"}}})
		return githubAppTokenResponse(200, string(body)), nil
	})}
	workspace := adoptionWorkspace + `knowledge:
  sources:
    principles:
      type: workspace_files
      scope: organization
      required: true
      paths: [MISSING.md]
`
	store := workflowv2.NewStore(db)
	run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{ID: "adoption-run", TenantID: "test-tenant-id", InitialClawID: "adoption-claw", TaskRunID: parent, WorkspaceYAML: []byte(workspace), WorkflowYAML: []byte(adoptionWorkflow), ActivationPending: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AssembleOrganizationContext(ctx, run.ID, workspaceFileKnowledgeResolver(nil)); err != nil {
		t.Fatal(err)
	}
	if err := s.completeWorkflowV2Activation(ctx, store, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != workflowv2.RunSuspended || got.State != "working" || !strings.Contains(got.WaitingReason, "required knowledge") {
		t.Fatalf("run=%+v", got)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE run_id=? AND kind LIKE 'delivery.%'`, run.ID).Scan(&events); err != nil || events != 0 || calls != 0 {
		t.Fatalf("delivery events=%d calls=%d err=%v", events, calls, err)
	}
}

// assertAdoptedPRReconciles proves an adopted PR keeps reconciling when
// GitHub's canonical owner/repo case differs from the workspace declaration.
func assertAdoptedPRReconciles(t *testing.T, s *Server, store *workflowv2.Store, runID, prURL string) {
	t.Helper()
	defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/repos/owner/repo/pulls/1" {
			return githubAppTokenResponse(404, `{}`), nil
		}
		body, _ := json.Marshal(map[string]any{"id": 1, "number": 1, "html_url": prURL, "state": "closed", "merged": true,
			"head": map[string]any{"ref": "deps/ts-minor-1", "sha": "head"}, "base": map[string]string{"ref": "main"}})
		return githubAppTokenResponse(200, string(body)), nil
	})}
	if err := s.reconcileWorkflowV2GitHub(context.Background()); err != nil {
		t.Fatal(err)
	}
	inspection, err := store.InspectRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if facts := inspection.Facts["delivery"].(map[string]interface{}); facts["merged"] != float64(1) {
		t.Fatalf("adopted PR did not reconcile: facts=%v", facts)
	}
}

// assertAdoptionLookupRequest checks that the adoption lookup lists open PRs
// on the configured repository with the repo token and a bounded context.
func assertAdoptionLookupRequest(t *testing.T, r *http.Request) bool {
	t.Helper()
	if r.URL.Path != "/repos/owner/repo/pulls" || r.URL.Query().Get("state") != "open" || r.Header.Get("Authorization") != "Bearer repo-token" {
		t.Errorf("unexpected adoption lookup request=%s", r.URL)
		return false
	}
	if _, ok := r.Context().Deadline(); !ok {
		t.Error("adoption lookup request has no deadline")
		return false
	}
	return true
}

func TestWorkflowV2ManualAdoptionFinishingRunReportsTerminalStatus(t *testing.T) {
	for _, tc := range []struct{ terminalState, want string }{
		{terminalState: "reviewing", want: string(workflowv2.RunCompleted)},
		{terminalState: "cancelled", want: string(workflowv2.RunCancelled)},
	} {
		t.Run(tc.terminalState, func(t *testing.T) {
			testWorkflowV2ManualAdoptionFinishingRun(t, tc.terminalState, tc.want)
		})
	}
}

func testWorkflowV2ManualAdoptionFinishingRun(t *testing.T, terminalState, want string) {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	t.Setenv("ELASTICCLAW_NOOP_PROVIDER", "1")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token", ClawToken: "claw-token",
		Providers: map[string]types.ProviderConfig{"noop": {Type: "noop"}}}, "", "", "")
	branchSweepTestToken(s)
	oldClient := defaultGitHubClient
	defaultGitHubClient = newGitHubClient()
	t.Cleanup(func() { defaultGitHubClient = oldClient })
	calls := 0
	defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !assertAdoptionLookupRequest(t, r) {
			return githubAppTokenResponse(404, `{}`), nil
		}
		body, _ := json.Marshal([]any{map[string]any{"number": 1, "html_url": "https://github.com/owner/repo/pull/1", "state": "open",
			"user": map[string]string{"login": "factory-app[bot]", "type": "Bot"},
			"head": map[string]any{"ref": "deps/ts-minor-1", "sha": "head", "repo": map[string]string{"full_name": "owner/repo"}},
			"base": map[string]string{"ref": "main"}}})
		return githubAppTokenResponse(200, string(body)), nil
	})}
	SaveWorkspaceForTest(t, &types.WorkspaceConfig{Name: "adoption", Files: map[string]string{
		"elasticclaw-config.yaml": adoptionWorkspace + "execution:\n  provider: noop\n",
	}}, []*types.WorkflowConfig{{Name: "adoption", RawConfig: strings.NewReplacer(`  reviewing:
    phase: review`, "  "+terminalState+`:
    phase: done
    terminal: true`, "to: reviewing", "to: "+terminalState).Replace(adoptionWorkflow) + "manual_trigger: true\n"}})

	req := httptest.NewRequest(http.MethodPost, "/api/workspaces/adoption/workflows/adoption/trigger", strings.NewReader(`{"inputs":{}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var created map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM workflow_v2_runs WHERE id=?`, created["run_id"]).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if calls == 0 || created["status"] != want || runStatus != want {
		t.Fatalf("calls=%d response=%v run=%s", calls, created, runStatus)
	}
	// The parent claw is finished in the background.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var clawStatus string
		if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, created["claw_id"]).Scan(&clawStatus); err != nil {
			t.Fatal(err)
		}
		if clawStatus == "deleted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent claw not finished: %s", clawStatus)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
