package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/elasticclaw/elasticclaw/pkg/hub/workflowv2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	typesv2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestClawPRRegistrationValidation(t *testing.T) {
	const url = "https://github.com/owner/repo/pull/17"
	for _, tc := range []struct {
		name, token, auth, claw, url, source, status, repos, body string
		code                                                      int
	}{
		{name: "missing auth", code: 401},
		{name: "wrong token", token: "wrong", code: 401},
		{name: "hub token rejected", auth: "Bearer hub-token", code: 401},
		{name: "unknown claw", token: "claw-token", claw: "unknown", code: 404},
		{name: "malformed JSON", token: "claw-token", body: "{", code: 400},
		{name: "trailing JSON", token: "claw-token", body: `{"url":"` + url + `","source":"gh_create"} {}`, code: 400},
		{name: "oversized body", token: "claw-token", body: strings.Repeat(" ", 4097) + `{}`, code: 400},
		{name: "URL suffix", token: "claw-token", url: url + "/files", code: 400},
		{name: "URL prefix", token: "claw-token", url: "see " + url, code: 400},
		{name: "wrong host", token: "claw-token", url: "https://example.com/owner/repo/pull/1", code: 400},
		{name: "zero number", token: "claw-token", url: "https://github.com/owner/repo/pull/0", code: 400},
		{name: "overflow number", token: "claw-token", url: "https://github.com/owner/repo/pull/999999999999999999999", code: 400},
		{name: "untrusted source", token: "claw-token", source: "done", code: 400},
		{name: "forbidden repo", token: "claw-token", url: "https://github.com/other/repo/pull/1", code: 403},
		{name: "empty allowlist", token: "claw-token", repos: "[]", code: 403},
		{name: "invalid allowlist", token: "claw-token", repos: "invalid", code: 403},
		{name: "query auth", token: "claw-token", code: 200},
		{name: "bearer auth", auth: "Bearer claw-token", code: 200},
		{name: "query takes precedence", token: "wrong", auth: "Bearer claw-token", code: 401},
		{name: "glob", token: "claw-token", repos: `[{"repo":"owner/*"}]`, code: 200},
		{name: "legacy keys", token: "claw-token", repos: `[{"Repo":"owner/repo","Permissions":"write"}]`, code: 200},
		{name: "deleted no-op", token: "claw-token", status: "deleted", code: 200},
		{name: "error no-op", token: "claw-token", status: "error", code: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
			s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "hub-token", ClawToken: "claw-token"}, "", "", "")
			status := tc.status
			if status == "" {
				status = "running"
			}
			repos := tc.repos
			if repos == "" {
				repos = `[{"repo":"owner/repo","permissions":"write"}]`
			}
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,github_repos,created_at) VALUES('test-claw','test-tenant-id','test','',?,?,datetime('now'))`, status, repos); err != nil {
				t.Fatal(err)
			}
			claw := tc.claw
			if claw == "" {
				claw = "test-claw"
			}
			prURL := tc.url
			if prURL == "" {
				prURL = url
			}
			source := tc.source
			if source == "" {
				source = "gh_create"
			}
			body := tc.body
			if body == "" {
				data, _ := json.Marshal(map[string]string{"url": prURL, "source": source})
				body = string(data)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/claws/"+claw+"/prs?claw_token="+tc.token, strings.NewReader(body))
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.code == 200 && tc.status == "" {
				want = 1
			}
			if count != want {
				t.Fatalf("PR count %d, want %d", count, want)
			}
			if tc.code == 200 && tc.status != "" && !strings.Contains(rec.Body.String(), `"registered":false`) {
				t.Fatalf("terminal response: %s", rec.Body.String())
			}
		})
	}
}

func TestClawPRRegistrationAssociatesWithoutCompletion(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "new"
		if upgrade {
			name = "upgrade"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
			const clawID = "claw-pr-registration"
			const url = "https://github.com/owner/repo/pull/17"
			s, db := newTaskRunAnalyticsTestServer(t, clawID)
			s.hubCfg.ClawToken = "claw-token"
			if _, err := db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"}]' WHERE id=?`, clawID); err != nil {
				t.Fatal(err)
			}
			if upgrade {
				if _, err := s.storePRMention(clawID, "owner/repo", 17, url, true, "mention"); err != nil {
					t.Fatal(err)
				}
			}
			runID, _ := startTaskRunForTest(t, s, clawID, "pr-registration")
			for i := 0; i < 2; i++ {
				req := httptest.NewRequest(http.MethodPost, "/api/claws/"+clawID+"/prs", strings.NewReader(`{"url":"`+url+`","source":"gh_create"}`))
				req.Header.Set("Authorization", "Bearer claw-token")
				rec := httptest.NewRecorder()
				s.mux.ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
				}
				var result struct {
					Registered bool `json:"registered"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.Registered != (i == 0 && !upgrade) {
					t.Fatalf("unexpected inserted flag: %s", rec.Body.String())
				}
			}
			var count, mentionOnly int
			var source, status string
			if err := db.QueryRow(`SELECT COUNT(*),mention_only,source FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count, &mentionOnly, &source); err != nil {
				t.Fatal(err)
			}
			if count != 1 || mentionOnly != 0 || source != "gh_create" {
				t.Fatalf("row: %d %d %q", count, mentionOnly, source)
			}
			assertTaskRunPR(t, db, runID, "owner/repo", 17, taskRunPRStateOpen, false)
			if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != "running" {
				t.Fatalf("claw status changed: %s", status)
			}
			var completed int
			if err := db.QueryRow(`SELECT COUNT(*) FROM task_run_events WHERE run_id=? AND event_type=?`, runID, taskRunEventTaskCompleted).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if completed != 0 {
				t.Fatalf("registration completed task run")
			}
			// The existing dashboard route must retain hub-token authentication.
			req := httptest.NewRequest(http.MethodGet, "/api/claws/"+clawID+"/prs", nil)
			req.Header.Set("Authorization", "Bearer test-token")
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("GET PRs: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestClawPRRegistrationV2RegisterOnly(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	for _, mode := range []string{"durable", "connection", "control"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			const clawID = "claw-pr-v2"
			const url = "https://github.com/owner/repo/pull/1"
			s, db := newTaskRunAnalyticsTestServer(t, clawID)
			s.hubCfg.ClawToken = "claw-token"
			if _, err := db.Exec(`UPDATE claws SET github_repos='[{"repo":"owner/repo"}]' WHERE id=?`, clawID); err != nil {
				t.Fatal(err)
			}
			taskRunID, _ := startTaskRunForTest(t, s, clawID, "pr-v2-registration")
			if mode == "connection" {
				s.claws[clawID] = &clawConn{id: clawID, tenantID: "test-tenant-id", workflowV2Controlled: true}
			}
			store := workflowv2.NewStore(db)
			run, err := store.CreateRun(ctx, workflowv2.CreateRunRequest{
				ID: "pr-v2", TenantID: "test-tenant-id", InitialClawID: clawID,
				WorkspaceYAML: []byte("schema_version: 2\nname: test\nrepositories:\n  repo:\n    provider: github\n    repository: owner/repo\n"),
				WorkflowYAML:  []byte(workflowV2APIWorkflow),
			})
			if err != nil {
				t.Fatal(err)
			}
			oldClient := defaultGitHubClient
			defaultGitHubClient = newGitHubClient()
			t.Cleanup(func() { defaultGitHubClient = oldClient })
			defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/repos/owner/repo/pulls/1" {
					t.Fatalf("unexpected GitHub request: %s", r.URL)
				}
				if _, ok := r.Context().Deadline(); !ok && mode != "control" {
					t.Error("verification has no deadline")
				}
				return githubAppTokenResponse(http.StatusOK, `{"html_url":"`+url+`","state":"open","head":{"sha":"head-v2","ref":"feature"},"base":{"ref":"main"}}`), nil
			})}
			for i := 0; i < 2; i++ {
				if mode == "control" {
					receipt, err := s.applyWorkflowV2ClawControl(ctx, store, typesv2.ControlEnvelope{
						ProtocolVersion: typesv2.ControlProtocolVersion, MessageID: "claim-v2", Kind: typesv2.MessagePullRequestClaimed,
						RunID: run.ID, AttemptID: run.CurrentAttemptID, ExpectedStateVersion: &run.StateVersion,
						Payload: []byte(`{"pull_requests":[{"url":"` + url + `"}]}`),
					})
					want := typesv2.DispositionAccepted
					if i == 1 {
						want = typesv2.DispositionDuplicate
					}
					if err != nil || receipt.Disposition != want {
						t.Fatalf("claim: %#v, %v", receipt, err)
					}
					continue
				}
				req := httptest.NewRequest(http.MethodPost, "/api/claws/"+clawID+"/prs?claw_token=claw-token", strings.NewReader(`{"url":"`+url+`","source":"gh_create"}`))
				rec := httptest.NewRecorder()
				s.mux.ServeHTTP(rec, req)
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), fmt.Sprintf(`"registered":%t`, i == 0)) {
					t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
				}
			}
			var count, registered int
			if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("V2 registered a legacy PR")
			}
			if err := db.QueryRow(`SELECT COUNT(*),registered_only FROM workflow_v2_delivery_prs WHERE run_id=?`, run.ID).Scan(&count, &registered); err != nil {
				t.Fatal(err)
			}
			if count != 1 || registered != 1 {
				t.Fatalf("PR count=%d registered=%d", count, registered)
			}
			assertTaskRunPR(t, db, taskRunID, "owner/repo", 1, taskRunPRStateOpen, false)
			updated, err := store.GetRun(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.StateVersion != run.StateVersion || updated.State != run.State {
				t.Fatalf("registration changed workflow: %#v", updated)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM workflow_v2_events WHERE kind LIKE 'delivery.%'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("registration emitted delivery events")
			}
		})
	}
}

func TestClawPRRegistrationV2RequiresActiveBinding(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	const clawID = "claw-no-v2-attempt"
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,github_repos,created_at) VALUES(?,'test-tenant-id','test','','running','[{"repo":"owner/repo"}]',datetime('now'))`, clawID); err != nil {
		t.Fatal(err)
	}
	s.claws[clawID] = &clawConn{id: clawID, tenantID: "test-tenant-id", workflowV2Controlled: true}
	req := httptest.NewRequest(http.MethodPost, "/api/claws/"+clawID+"/prs?claw_token=claw-token", strings.NewReader(`{"url":"https://github.com/owner/repo/pull/1","source":"gh_create"}`))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("missing binding: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPRSourcesAndUpgrades(t *testing.T) {
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s, db := newTaskRunAnalyticsTestServer(t, "claw-pr-sources")
	const clawID = "claw-pr-sources"
	const url = "https://github.com/owner/repo/pull/1"
	s.scanMessageForPRs(clawID, url, true, "mention")
	assertSource := func(url, source string) {
		t.Helper()
		var got string
		if err := db.QueryRow(`SELECT source FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, url).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != source {
			t.Fatalf("source %q, want %q", got, source)
		}
	}
	assertSource(url, "mention")
	s.scanMessageForPRs(clawID, url, false, "gate")
	assertSource(url, "gate")
	s.scanMessageForPRs(clawID, url, true, "mention")
	assertSource(url, "gate")
	if failed := s.registerDonePRURLs(clawID, []string{url, "https://github.com/owner/repo/pull/2"}); failed != "" {
		t.Fatal(failed)
	}
	assertSource(url, "gate")
	assertSource("https://github.com/owner/repo/pull/2", "done")
	const upgradeURL = "https://github.com/owner/repo/pull/3"
	s.scanMessageForPRs(clawID, upgradeURL, true, "mention")
	if failed := s.insertClawPRsAtomic(clawID, []prMentionCandidate{{repo: "owner/repo", number: 3, url: upgradeURL}}, false, "done"); failed != "" {
		t.Fatal(failed)
	}
	assertSource(upgradeURL, "done")
	if _, err := s.storePRMention(clawID, "owner/repo", 4, "https://github.com/owner/repo/pull/4", false, "trigger"); err != nil {
		t.Fatal(err)
	}
	assertSource("https://github.com/owner/repo/pull/4", "trigger")
}

func TestMigrateClawPRSource(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE claw_prs(id TEXT PRIMARY KEY,claw_id TEXT NOT NULL,repo TEXT NOT NULL,pr_number INTEGER NOT NULL,pr_url TEXT NOT NULL,last_ci_sha TEXT NOT NULL DEFAULT '',last_comment_id INTEGER NOT NULL DEFAULT 0,created_at DATETIME NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES('old','claw','owner/repo',1,'url',datetime('now'))`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
		var source string
		if err := db.QueryRow(`SELECT source FROM claw_prs WHERE id='old'`).Scan(&source); err != nil {
			t.Fatal(err)
		}
		if source != "" {
			t.Fatalf("legacy source %q", source)
		}
	}
}
