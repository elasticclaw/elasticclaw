package hub

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
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

func TestClawPRRegistrationV2NotImplemented(t *testing.T) {
	for _, connected := range []bool{false, true} {
		name := "durable"
		if connected {
			name = "connection"
		}
		t.Run(name, func(t *testing.T) {
			s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
			const clawID = "claw-pr-v2"
			if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,github_repos,created_at) VALUES(?,'test-tenant-id','test','','running','[{"repo":"owner/repo"}]',datetime('now'))`, clawID); err != nil {
				t.Fatal(err)
			}
			if connected {
				s.claws[clawID] = &clawConn{id: clawID, tenantID: "test-tenant-id", workflowV2Controlled: true}
			} else {
				insertWorkflowV2Run(t, db, "pr-v2", "attempt-pr-v2", time.Time{}, now())
				if _, err := db.Exec(`UPDATE workflow_v2_runs SET tenant_id='test-tenant-id' WHERE id='pr-v2'`); err != nil {
					t.Fatal(err)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/api/claws/"+clawID+"/prs?claw_token=claw-token", strings.NewReader(`{"url":"https://github.com/owner/repo/pull/1","source":"gh_create"}`))
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != 501 {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("V2 registered a legacy PR")
			}
		})
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
