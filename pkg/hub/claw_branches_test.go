package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func branchSweepTestToken(s *Server) {
	s.hubCfg.GitHubApps = []*types.GitHubAppConfig{{AppID: 1, PrivateKeyPEM: "cached-test-key", URL: "https://github.com/apps/factory-app"}}
	s.ghTokenCache = map[string]cachedGitHubToken{
		"owner/repo:read": {token: "repo-token", expiresAt: time.Now().Add(time.Hour)},
		"":                {token: "repo-token", expiresAt: time.Now().Add(time.Hour)},
	}
}

func TestClawBranchesEndpoint(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	for _, tc := range []struct {
		name, auth, token, claw, status, body string
		code                                  int
	}{
		{name: "missing auth", code: 401}, {name: "wrong auth", auth: "Bearer wrong", code: 401},
		{name: "query precedence", token: "wrong", auth: "Bearer claw-token", code: 401},
		{name: "unknown claw", token: "claw-token", claw: "unknown", code: 404},
		{name: "invalid JSON", token: "claw-token", body: "{", code: 400},
		{name: "trailing JSON", token: "claw-token", body: `{"branches":[]} {}`, code: 400},
		{name: "invalid SHA", token: "claw-token", body: `{"branches":[{"repo":"owner/repo","branch":"feature","head_sha":"nope"}]}`, code: 400},
		{name: "invalid repo", token: "claw-token", body: `{"branches":[{"repo":"owner/repo/extra","branch":"feature","head_sha":"` + strings.Repeat("a", 40) + `"}]}`, code: 400},
		{name: "unconfigured repo skipped", token: "claw-token", body: `{"branches":[{"repo":"other/repo","branch":"feature","head_sha":"` + strings.Repeat("a", 40) + `"}]}`, code: 200},
		{name: "query auth", token: "claw-token", body: `{"branches":[]}`, code: 200},
		{name: "bearer auth", auth: "Bearer claw-token", body: `{"branches":[]}`, code: 200},
		{name: "deleted", token: "claw-token", status: "deleted", code: 200},
		{name: "error", token: "claw-token", status: "error", code: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := newTaskRunAnalyticsTestServer(t, "branch-claw")
			s.hubCfg.ClawToken = "claw-token"
			status := tc.status
			if status == "" {
				status = "running"
			}
			if _, err := db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"}]',status=? WHERE id='branch-claw'`, status); err != nil {
				t.Fatal(err)
			}
			claw := tc.claw
			if claw == "" {
				claw = "branch-claw"
			}
			body := tc.body
			if body == "" {
				body = `{"branches":[{"repo":"owner/repo","branch":"feature","head_sha":"` + strings.Repeat("a", 40) + `"}]}`
			}
			req := httptest.NewRequest(http.MethodPost, "/api/claws/"+claw+"/branches?claw_token="+tc.token, strings.NewReader(body))
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM claw_branches`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("unexpected persisted branches: %d", count)
			}
		})
	}
}

func TestStoreClawBranchesChanges(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := newTaskRunAnalyticsTestServer(t, "branch-claw")
	branch := clawBranch{Repo: "owner/repo", Branch: "feature", HeadSHA: strings.Repeat("a", 40)}
	for i, want := range []int{1, 0, 1, 0} {
		if i == 2 {
			branch.HeadSHA = strings.Repeat("b", 40)
		}
		if i == 3 {
			if _, err := db.Exec(`UPDATE claw_branches SET pr_url='https://github.com/owner/repo/pull/1'`); err != nil {
				t.Fatal(err)
			}
			branch.HeadSHA = strings.Repeat("c", 40)
		}
		pending, err := s.storeClawBranches("branch-claw", []clawBranch{branch})
		if err != nil || len(pending) != want {
			t.Fatalf("pass %d: pending=%v err=%v", i, pending, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_branches`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
}

func TestClawBranchSweepFiltering(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	for _, tc := range []struct {
		name, author, login, headRepo, headRef, headSHA, state, otherStatus, otherKind string
		noApp, v2, want                                                                bool
	}{
		{name: "own app bot", author: "Bot", headRepo: "owner/repo", want: true},
		// Without the App's identity any Bot would qualify: fail closed.
		{name: "no app identity", author: "Bot", headRepo: "owner/repo", noApp: true},
		{name: "other app bot", author: "Bot", login: "dependabot[bot]", headRepo: "owner/repo"},
		{name: "human", author: "User", login: "someone", headRepo: "owner/repo"},
		{name: "fork", author: "Bot", headRepo: "other/repo"},
		{name: "wrong branch", author: "Bot", headRepo: "owner/repo", headRef: "unrelated"},
		{name: "closed", author: "Bot", headRepo: "owner/repo", state: "closed"},
		{name: "active owner", author: "Bot", headRepo: "owner/repo", otherStatus: "running", otherKind: "delivered"},
		{name: "offline owner", author: "Bot", headRepo: "owner/repo", otherStatus: "offline", otherKind: "delivered"},
		{name: "terminal owner", author: "Bot", headRepo: "owner/repo", otherStatus: "deleted", otherKind: "delivered", want: true},
		{name: "error owner", author: "Bot", headRepo: "owner/repo", otherStatus: "error", otherKind: "registered", want: true},
		{name: "mention by other", author: "Bot", headRepo: "owner/repo", otherStatus: "running", otherKind: "mention", want: true},
		{name: "registered by other", author: "Bot", headRepo: "owner/repo", otherStatus: "running", otherKind: "registered"},
		{name: "registered by offline", author: "Bot", headRepo: "owner/repo", otherStatus: "offline", otherKind: "registered"},
		{name: "V2 registration by other", author: "Bot", headRepo: "owner/repo", otherStatus: "running", otherKind: "v2_registered"},
		{name: "V2 registration by offline", author: "Bot", headRepo: "owner/repo", otherStatus: "offline", otherKind: "v2_registered"},
		{name: "V2 delivery by other", author: "Bot", headRepo: "owner/repo", otherStatus: "running", otherKind: "v2_delivered"},
		{name: "V2 delivery by offline", author: "Bot", headRepo: "owner/repo", otherStatus: "offline", otherKind: "v2_delivered"},
		{name: "V2 register only", author: "Bot", headRepo: "owner/repo", v2: true, want: true},
		// A reused branch name: another sandbox pushed the PR head, not this one.
		{name: "head pushed elsewhere", author: "Bot", headRepo: "owner/repo", headSHA: strings.Repeat("b", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const clawID = "branch-claw"
			const prURL = "https://github.com/owner/repo/pull/1"
			s, db := newTaskRunAnalyticsTestServer(t, clawID)
			if _, err := db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"}]' WHERE id=?`, clawID); err != nil {
				t.Fatal(err)
			}
			runID, _ := startTaskRunForTest(t, s, clawID, "sweep")
			var v2run workflowv2.Run
			if tc.v2 {
				var err error
				v2run, err = workflowv2.NewStore(db).CreateRun(context.Background(), workflowv2.CreateRunRequest{
					ID: "branch-v2", TenantID: "test-tenant-id", InitialClawID: clawID,
					WorkspaceYAML: []byte("schema_version: 2\nname: test\nrepositories:\n  repo:\n    provider: github\n    repository: owner/repo\n"),
					WorkflowYAML:  []byte(workflowV2APIWorkflow),
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			branchSweepTestToken(s)
			if tc.noApp {
				s.hubCfg.GitHubApps[0].URL = ""
			}
			oldClient := defaultGitHubClient
			defaultGitHubClient = newGitHubClient()
			t.Cleanup(func() { defaultGitHubClient = oldClient })
			calls := 0
			setup := true // other-claw ownership fixtures fetch PR state too
			defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if setup && r.URL.Path != "/repos/owner/repo/pulls/1" {
					return githubAppTokenResponse(200, `[]`), nil
				}
				if !setup {
					calls++
					if r.Header.Get("Authorization") != "Bearer repo-token" {
						t.Error("missing repo token")
					}
				}
				// Watermarks keep a swept PR's existing history from replaying.
				switch r.URL.Path {
				case "/repos/owner/repo/pulls/1":
					return githubAppTokenResponse(200, `{"html_url":"`+prURL+`","state":"open","head":{"sha":"head","ref":"feature/x"},"base":{"ref":"main"}}`), nil
				case "/repos/owner/repo/issues/1/comments":
					return githubAppTokenResponse(200, `[{"id":5,"created_at":"2026-01-01T00:00:00Z"}]`), nil
				case "/repos/owner/repo/pulls/1/reviews":
					return githubAppTokenResponse(200, `[{"id":9}]`), nil
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Error("missing deadline")
				}
				if r.URL.Path != "/repos/owner/repo/pulls" || r.URL.Query().Get("head") != "owner:feature/x" || r.URL.Query().Get("state") != "open" {
					t.Fatalf("unexpected request %s", r.URL)
				}
				headRef, login, state := tc.headRef, tc.login, tc.state
				if headRef == "" {
					headRef = "feature/x"
				}
				if login == "" {
					login = "factory-app[bot]"
				}
				if state == "" {
					state = "open"
				}
				headSHA := tc.headSHA
				if headSHA == "" {
					headSHA = strings.Repeat("a", 40)
				}
				return githubAppTokenResponse(200, fmt.Sprintf(`[{"number":1,"html_url":%q,"state":%q,"user":{"login":%q,"type":%q},"head":{"ref":%q,"sha":%q,"repo":{"full_name":%q}}}]`, prURL, state, login, tc.author, headRef, headSHA, tc.headRepo)), nil
			})}
			if tc.otherStatus != "" {
				if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,created_at) VALUES('other-claw','test-tenant-id','other','',?,?)`, tc.otherStatus, now()); err != nil {
					t.Fatal(err)
				}
				var err error
				switch tc.otherKind {
				case "delivered":
					_, err = s.storePRMention("other-claw", "owner/repo", 1, prURL, false, "done")
				case "mention":
					_, err = s.storePRMention("other-claw", "owner/repo", 1, prURL, true, "mention")
				case "registered":
					_, err = s.storePRRegistration("other-claw", "owner/repo", 1, prURL, "gh_create")
				case "v2_registered", "v2_delivered":
					store := workflowv2.NewStore(db)
					other, createErr := store.CreateRun(context.Background(), workflowv2.CreateRunRequest{
						ID: "other-v2", TenantID: "test-tenant-id", InitialClawID: "other-claw",
						WorkspaceYAML: []byte("schema_version: 2\nname: test\nrepositories:\n  repo:\n    provider: github\n    repository: owner/repo\n"),
						WorkflowYAML:  []byte(workflowV2APIWorkflow),
					})
					if createErr != nil {
						t.Fatal(createErr)
					}
					claim := typesv2.PullRequestClaim{URL: prURL}
					if tc.otherKind == "v2_registered" {
						_, _, err = store.RegisterPullRequest(context.Background(), other.ID, other.CurrentAttemptID, claim, s.workflowV2PullRequestVerifier())
					} else {
						_, err = store.SubmitDelivery(context.Background(), other.ID, other.CurrentAttemptID,
							typesv2.DeliveryManifest{PullRequests: []typesv2.PullRequestClaim{claim}}, s.workflowV2PullRequestVerifier())
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			setup = false
			branch := clawBranch{Repo: "owner/repo", Branch: "feature/x", HeadSHA: strings.Repeat("a", 40)}
			pending, err := s.storeClawBranches(clawID, []clawBranch{branch})
			if err != nil {
				t.Fatal(err)
			}
			s.sweepClawBranches(clawID, pending)
			var registeredURL string
			var checked bool
			if err := db.QueryRow(`SELECT pr_url,checked_at IS NOT NULL FROM claw_branches`).Scan(&registeredURL, &checked); err != nil {
				t.Fatal(err)
			}
			if (registeredURL == prURL) != tc.want || checked == tc.noApp {
				t.Fatalf("URL=%q checked=%t", registeredURL, checked)
			}
			if tc.noApp && calls != 0 {
				t.Fatalf("listed PRs without an App identity: %d calls", calls)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_prs WHERE run_id=?`, runID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != boolInt(tc.want) {
				t.Fatalf("task run PR count=%d", count)
			}
			if tc.want && !tc.v2 {
				var source string
				var lastComment, lastReview int64
				if err := db.QueryRow(`SELECT source,last_comment_id,last_review_id FROM claw_prs WHERE claw_id=?`, clawID).Scan(&source, &lastComment, &lastReview); err != nil {
					t.Fatal(err)
				}
				if source != "branch_sweep" || lastComment != 5 || lastReview != 9 {
					t.Fatalf("source=%q watermarks=%d/%d", source, lastComment, lastReview)
				}
			}
			if tc.v2 {
				if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_delivery_prs WHERE run_id=? AND registered_only=1`, v2run.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("V2 registration=%d, %v", count, err)
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE kind LIKE 'delivery.%'`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("delivery events=%d, %v", count, err)
				}
				loaded, err := workflowv2.NewStore(db).GetRun(context.Background(), v2run.ID)
				if err != nil || loaded.StateVersion != v2run.StateVersion {
					t.Fatalf("V2 advanced: %#v, %v", loaded, err)
				}
			}
			var status string
			if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil || status != "running" {
				t.Fatalf("claw advanced: %q, %v", status, err)
			}
			if tc.want {
				previousCalls := calls
				s.sweepClawBranches(clawID, []clawBranch{branch})
				if calls != previousCalls {
					t.Fatal("registered branch queried again")
				}
			}
		})
	}
}

func TestClawBranchSweepRetriesUnchangedHead(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	for _, initialStatus := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(initialStatus), func(t *testing.T) {
			const clawID = "branch-retry-claw"
			s, db := newTaskRunAnalyticsTestServer(t, clawID)
			if _, err := db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"}]' WHERE id=?`, clawID); err != nil {
				t.Fatal(err)
			}
			branchSweepTestToken(s)
			oldClient := defaultGitHubClient
			defaultGitHubClient = newGitHubClient()
			t.Cleanup(func() { defaultGitHubClient = oldClient })
			calls := 0
			defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/repos/owner/repo/pulls" {
					return githubAppTokenResponse(200, `[]`), nil // registration watermarks
				}
				calls++
				if calls == 1 {
					return githubAppTokenResponse(initialStatus, `[]`), nil
				}
				return githubAppTokenResponse(200, `[{"number":1,"html_url":"https://github.com/owner/repo/pull/1","state":"open","user":{"login":"factory-app[bot]","type":"Bot"},"head":{"ref":"feature","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":{"full_name":"owner/repo"}}}]`), nil
			})}
			branch := clawBranch{Repo: "owner/repo", Branch: "feature", HeadSHA: strings.Repeat("a", 40)}
			pending, err := s.storeClawBranches(clawID, []clawBranch{branch})
			if err != nil {
				t.Fatal(err)
			}
			s.sweepClawBranches(clawID, pending)
			s.retryClawBranches()
			if calls != 1 {
				t.Fatal("retry was not throttled")
			}
			if _, err := db.Exec(`UPDATE claw_branches SET checked_at=?`, now().Add(-6*time.Minute)); err != nil {
				t.Fatal(err)
			}
			s.retryClawBranches()
			if calls != 2 {
				t.Fatalf("retry calls=%d", calls)
			}
			var prURL string
			if err := db.QueryRow(`SELECT pr_url FROM claw_branches`).Scan(&prURL); err != nil || prURL == "" {
				t.Fatalf("retry did not register PR: %q, %v", prURL, err)
			}
			s.retryClawBranches()
			if calls != 2 {
				t.Fatal("registered branch retried")
			}
		})
	}
}

func TestClawBranchesEndpointQueuesRegistration(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	const clawID = "branch-endpoint-claw"
	s, db := newTaskRunAnalyticsTestServer(t, clawID)
	s.hubCfg.ClawToken = "claw-token"
	if _, err := db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"}]' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	branchSweepTestToken(s)
	oldClient := defaultGitHubClient
	defaultGitHubClient = newGitHubClient()
	t.Cleanup(func() { defaultGitHubClient = oldClient })
	defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return githubAppTokenResponse(200, `[{"number":1,"html_url":"https://github.com/owner/repo/pull/1","state":"open","user":{"login":"factory-app[bot]","type":"Bot"},"head":{"ref":"feature","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":{"full_name":"owner/repo"}}}]`), nil
	})}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/claws/"+clawID+"/branches?claw_token=claw-token", strings.NewReader(`{"branches":[{"repo":"owner/repo","branch":"feature","head_sha":"`+strings.Repeat("a", 40)+`"},{"repo":"other/lib","branch":"feature","head_sha":"`+strings.Repeat("b", 40)+`"}]}`))
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), fmt.Sprintf(`"queued":%d`, 1-i)) {
			t.Fatalf("report %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var prURL string
		if err := db.QueryRow(`SELECT pr_url FROM claw_branches WHERE claw_id=?`, clawID).Scan(&prURL); err != nil {
			t.Fatal(err)
		}
		if prURL != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("async branch registration did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var source string
	if err := db.QueryRow(`SELECT source FROM claw_prs WHERE claw_id=?`, clawID).Scan(&source); err != nil || source != "branch_sweep" {
		t.Fatalf("source=%q, %v", source, err)
	}
	var stored int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_branches WHERE claw_id=?`, clawID).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("unconfigured repo branch was stored: rows=%d, %v", stored, err)
	}
}
