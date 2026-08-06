package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newDoneTestServer creates a minimal Server with an in-memory DB and an
// optional GitHub API base URL override (for mocking PR validation).
func newDoneTestServer(t *testing.T, githubBase string) *Server {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	s := &Server{
		db:            db,
		hubCfg:        &types.HubConfig{},
		claws:         map[string]*clawConn{},
		users:         map[string]*userConn{},
		githubBaseURL: githubBase,
	}
	return s
}

// mockGitHubPRServer spins up a test HTTP server that responds to GitHub PR
// API calls. prStates maps "owner/repo/pulls/N" → state string ("open",
// "closed", etc.). Missing entries return 404.
func mockGitHubPRServer(t *testing.T, prStates map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// strip leading "/repos/" → "owner/repo/pulls/N"
		const prefix = "/repos/"
		if len(r.URL.Path) <= len(prefix) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		key := r.URL.Path[len(prefix):]
		state, ok := prStates[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"state": state, "number": 1})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- extractDonePRURLs ---

func TestExtractDonePRURLs(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantURLs []string
	}{
		{
			name:     "single PR URL",
			message:  "[DONE] https://github.com/org/repo/pull/123",
			wantURLs: []string{"https://github.com/org/repo/pull/123"},
		},
		{
			name:    "multiple PR URLs same line",
			message: "[DONE] https://github.com/org/repo/pull/1 https://github.com/org/other/pull/2",
			wantURLs: []string{
				"https://github.com/org/repo/pull/1",
				"https://github.com/org/other/pull/2",
			},
		},
		{
			name:     "bare DONE no URL",
			message:  "[DONE]",
			wantURLs: nil,
		},
		{
			name:     "DONE with surrounding text on same line",
			message:  "All done! [DONE] https://github.com/org/repo/pull/42",
			wantURLs: []string{"https://github.com/org/repo/pull/42"},
		},
		{
			name:     "DONE on its own line, URL on next line",
			message:  "[DONE]\nhttps://github.com/org/repo/pull/99",
			wantURLs: []string{"https://github.com/org/repo/pull/99"},
		},
		{
			name:    "DONE then multiple PR URLs on following lines",
			message: "[DONE]\nhttps://github.com/org/repo/pull/1\nhttps://github.com/org/other/pull/2",
			wantURLs: []string{
				"https://github.com/org/repo/pull/1",
				"https://github.com/org/other/pull/2",
			},
		},
		{
			name:     "no DONE token at all",
			message:  "I finished the work https://github.com/org/repo/pull/7",
			wantURLs: nil,
		},
		{
			name:     "DONE buried in multiline message",
			message:  "Here is a summary.\n\n[DONE] https://github.com/org/repo/pull/55\n\nThanks!",
			wantURLs: []string{"https://github.com/org/repo/pull/55"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractDonePRURLs(tt.message)
			if len(got) != len(tt.wantURLs) {
				t.Fatalf("got %v, want %v", got, tt.wantURLs)
			}
			for i := range got {
				if got[i] != tt.wantURLs[i] {
					t.Errorf("url[%d]: got %q, want %q", i, got[i], tt.wantURLs[i])
				}
			}
		})
	}
}

// --- validateDonePRs ---

func TestValidateDonePRs_NoURLs(t *testing.T) {
	srv := mockGitHubPRServer(t, nil)
	s := newDoneTestServer(t, srv.URL)
	rejected, reason := s.validateDonePRs("claw1", nil, "tok")
	if !rejected {
		t.Fatal("expected rejection when no URLs provided")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

func TestValidateDonePRs_SingleOpen(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{
		"org/repo/pulls/42": "open",
	})
	s := newDoneTestServer(t, srv.URL)
	urls := []string{"https://github.com/org/repo/pull/42"}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if rejected {
		t.Fatalf("expected acceptance, got rejection: %s", reason)
	}
}

func TestValidateDonePRs_MultipleAllOpen(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{
		"org/repo/pulls/1":  "open",
		"org/other/pulls/2": "open",
	})
	s := newDoneTestServer(t, srv.URL)
	urls := []string{
		"https://github.com/org/repo/pull/1",
		"https://github.com/org/other/pull/2",
	}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if rejected {
		t.Fatalf("expected acceptance, got rejection: %s", reason)
	}
}

func TestValidateDonePRs_OneClosed(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{
		"org/repo/pulls/1": "open",
		"org/repo/pulls/2": "closed",
	})
	s := newDoneTestServer(t, srv.URL)
	urls := []string{
		"https://github.com/org/repo/pull/1",
		"https://github.com/org/repo/pull/2",
	}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if !rejected {
		t.Fatal("expected rejection when one PR is closed")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

func TestValidateDonePRs_NotFound(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{}) // nothing registered → 404
	s := newDoneTestServer(t, srv.URL)
	urls := []string{"https://github.com/org/repo/pull/99"}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if !rejected {
		t.Fatal("expected rejection when PR returns 404")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

func TestValidateDonePRs_MergedIsNotOpen(t *testing.T) {
	srv := mockGitHubPRServer(t, map[string]string{
		"org/repo/pulls/5": "merged",
	})
	s := newDoneTestServer(t, srv.URL)
	urls := []string{"https://github.com/org/repo/pull/5"}
	rejected, reason := s.validateDonePRs("claw1", urls, "tok")
	if !rejected {
		t.Fatal("expected rejection when PR state is 'merged' not 'open'")
	}
	if reason == "" {
		t.Fatal("expected non-empty reason")
	}
}

// --- handleClawDoneSignal gate blocking ---

func TestHandleClawDoneSignal_BlockedByFailedRequiredGate(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-done-gate-block"
	// Insert a claw with a linear issue (tenant "test-tenant-id" is already created by NewTestServerWithConfig)
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, linear_issue_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "ENG-123",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Insert a failed required gate
	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		clawID, "android_validation", "android_validation", "fail", "status", "failed", 1)
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}

	// Call handleClawDoneSignal — should be blocked
	s.handleClawDoneSignal(clawID, "[DONE] https://github.com/org/repo/pull/42")

	// Verify no PR was stored
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count claw_prs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 PRs stored, got %d", count)
	}

	// Verify a message was injected to the claw (injectUserMessage uses role='user')
	var msgCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user'`, clawID).Scan(&msgCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if msgCount != 1 {
		t.Fatalf("expected 1 user message, got %d", msgCount)
	}
}

func TestHandleClawDoneSignal_PipelineDoneTriggerDoesNotRequirePRURL(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	s.hubCfg.Factories = []*types.FactoryConfig{
		{
			Name:     "faster_apps",
			Template: "elasticclaw",
			PipelineYAML: `
stages:
  - id: working
    label: Working
    entry: true
  - id: android_validation
    label: Android Validation
    triggers:
      - message_contains: "[DONE]"
    on_enter:
      inject: "Android validation started"
`,
		},
	}

	const clawID = "claw-done-pipeline"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, linear_issue_id, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "NEXT-257", "elasticclaw", "connected", `["factory:faster_apps"]`, "NEXT-257", "working",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	s.handleClawDoneSignal(clawID, "[DONE]")

	var stage string
	if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage); err != nil {
		t.Fatalf("select pipeline_stage: %v", err)
	}
	if stage != "android_validation" {
		t.Fatalf("pipeline_stage = %q, want android_validation", stage)
	}

	var noPRWarnings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND role='user' AND content LIKE '%no PR URLs%'`, clawID).Scan(&noPRWarnings); err != nil {
		t.Fatalf("count no-pr warnings: %v", err)
	}
	if noPRWarnings != 0 {
		t.Fatalf("expected no PR URL warning to be suppressed, got %d", noPRWarnings)
	}

	var injected string
	if err := db.QueryRow(`SELECT content FROM messages WHERE claw_id=? ORDER BY created_at DESC LIMIT 1`, clawID).Scan(&injected); err != nil {
		t.Fatalf("select injected message: %v", err)
	}
	if !strings.Contains(injected, "Android validation started") {
		t.Fatalf("injected message = %q, want validation inject", injected)
	}
}

func TestHandleClawDoneSignal_PipelineDoneStillRegistersPRURLs(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	s.hubCfg.Factories = []*types.FactoryConfig{
		{
			Name:     "github_todo",
			Template: "elasticclaw",
			PipelineYAML: `
stages:
  - id: working
    entry: true
  - id: pr_opened
    triggers:
      - message_contains: "[DONE]"
    on_enter:
      inject: "verify prs"
  - id: merged
    triggers:
      - pr_merged: {}
    terminal: true
`,
		},
	}

	const clawID = "claw-done-pipeline-prs"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, github_issue_id, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "issue-1", "elasticclaw", "connected", `["factory:github_todo"]`, "org/repo/1", "working",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Pipeline owns [DONE] — previously this path returned before registering PRs.
	s.handleClawDoneSignal(clawID, "[DONE]\nhttps://github.com/org/repo/pull/42\nhttps://github.com/org/other/pull/7")

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count claw_prs: %v", err)
	}
	if count != 2 {
		t.Fatalf("claw_prs count = %d, want 2 (pipeline [DONE] must still arm PR monitoring)", count)
	}

	var stage string
	if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage); err != nil {
		t.Fatalf("select pipeline_stage: %v", err)
	}
	if stage != "pr_opened" {
		t.Fatalf("pipeline_stage = %q, want pr_opened", stage)
	}
}

func TestHandleClawDoneSignal_PipelineDoneBlockedByFailedRequiredGate(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	s.hubCfg.Factories = []*types.FactoryConfig{
		{
			Name:     "gated_todo",
			Template: "elasticclaw",
			PipelineYAML: `
stages:
  - id: working
    entry: true
  - id: pr_opened
    triggers:
      - message_contains: "[DONE]"
    on_enter:
      inject: "should not reach"
`,
		},
	}

	const clawID = "claw-done-pipeline-gate"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, github_issue_id, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "issue-2", "elasticclaw", "connected", `["factory:gated_todo"]`, "org/repo/2", "working",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		clawID, "pr_opened", "pr_links", "fail", "status", "failed", 1)
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}

	s.handleClawDoneSignal(clawID, "[DONE] https://github.com/org/repo/pull/99")

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count claw_prs: %v", err)
	}
	if count != 0 {
		t.Fatalf("claw_prs count = %d, want 0 when required gate failed", count)
	}
	var stage string
	if err := db.QueryRow(`SELECT pipeline_stage FROM claws WHERE id=?`, clawID).Scan(&stage); err != nil {
		t.Fatalf("select pipeline_stage: %v", err)
	}
	if stage != "working" {
		t.Fatalf("pipeline_stage = %q, want working (blocked)", stage)
	}
}

func TestHandleClawDoneSignal_BlockedByErrorRequiredGate(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-done-gate-error"
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, linear_issue_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "ENG-125",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Insert an error verdict required gate — should also block [DONE]
	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		clawID, "android_validation", "android_validation", "error", "", "", 1)
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}

	s.handleClawDoneSignal(clawID, "[DONE] https://github.com/org/repo/pull/42")

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count claw_prs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 PRs stored for error verdict, got %d", count)
	}
}

func TestHandleClawDoneSignal_AllowedWhenNoFailedRequiredGate(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")

	const clawID = "claw-done-gate-allow"
	// Insert a claw with a linear issue (tenant "test-tenant-id" is already created by NewTestServerWithConfig)
	_, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, linear_issue_id, created_at) VALUES(?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "base", "connected", "ENG-124",
	)
	if err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	// Insert a passed required gate
	_, err = db.Exec(`
		INSERT INTO pipeline_gate_results(claw_id, stage_id, output_name, verdict, matched_path, matched_value, required, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		clawID, "android_validation", "android_validation", "pass", "status", "passed", 1)
	if err != nil {
		t.Fatalf("insert gate result: %v", err)
	}

	// Call handleClawDoneSignal — should proceed (no GH App, so no PR validation)
	s.handleClawDoneSignal(clawID, "[DONE]")

	// Verify no PR was stored (no GH App configured, but no gate blocking either)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatalf("count claw_prs: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 PRs stored (no GH App), got %d", count)
	}
}

func TestHandleClawDoneSignal_IssueLessWorkflowCompletesWithoutPR(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	s.cronScheduler = newCronScheduler(s)
	const clawID = "claw-issue-less-done"
	_, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "workflow claw", "base", "connected")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO workflow_runs(id, tenant_id, workflow_name, workspace_name, trigger_type, status, claw_id, run_context, started_at, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'),datetime('now'))`, "run-issue-less-done", "test-tenant-id", "nightly", "engineering", "cron", "running", clawID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	s.handleClawDoneSignal(clawID, "[DONE]")
	var clawStatus, runStatus string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&clawStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM workflow_runs WHERE id=?`, "run-issue-less-done").Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if clawStatus != "deleted" || runStatus != "completed" {
		t.Fatalf("statuses = claw %q, run %q; want deleted, completed", clawStatus, runStatus)
	}
}

func TestHandleClawDoneSignal_IssueLessWorkflowWatchesPR(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-issue-less-pr"
	_, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "workflow claw", "base", "connected")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO workflow_runs(id, tenant_id, workflow_name, workspace_name, trigger_type, status, claw_id, run_context, started_at, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'),datetime('now'))`, "run-issue-less-pr", "test-tenant-id", "nightly", "engineering", "cron", "running", clawID, "{}")
	if err != nil {
		t.Fatal(err)
	}
	s.handleClawDoneSignal(clawID, "[DONE] https://github.com/org/repo/pull/42")
	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "idle" {
		t.Fatalf("status = %q, want idle", status)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id=?`, clawID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("claw_prs count = %d, want 1", count)
	}
}

func TestHandleClawDoneSignal_IssueLessInteractiveClawIsNoOp(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{Token: "test-token"}, "", "", "")
	const clawID = "claw-issue-less-interactive"
	_, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "interactive claw", "base", "connected")
	if err != nil {
		t.Fatal(err)
	}

	s.handleClawDoneSignal(clawID, "[DONE]")

	var status string
	if err := db.QueryRow(`SELECT status FROM claws WHERE id=?`, clawID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "connected" {
		t.Fatalf("status = %q, want connected", status)
	}
}

func TestHandleClawDoneSignal_DoesNotMoveTrackerWhenTerminalTxFails(t *testing.T) {
	requests := make(chan struct{}, 1)
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- struct{}{}:
		default:
		}
		_, _ = w.Write([]byte(`{"data":{"issue":{"id":"issue-1"}}}`))
	}))
	t.Cleanup(tracker.Close)

	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:        "test-token",
		Integrations: &types.IntegrationsConfig{Linear: []*types.LinearIntegrationConfig{{Workspace: "workspace", Token: "token"}}},
		Factories:    []*types.FactoryConfig{{Name: "factory", Integration: "linear", Workspace: "workspace", FinishedStatus: "Done"}},
	}, "", tracker.URL, "")
	const clawID = "claw-done-closed-db"
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,template,status,linear_issue_id,tags,created_at) VALUES(?,?,?,?,?,?,?,datetime('now'))`, clawID, "test-tenant-id", "test-claw", "base", "connected", "ENG-1", `["factory:factory"]`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_terminal_tx BEFORE UPDATE OF status ON claws WHEN NEW.status='idle' BEGIN SELECT RAISE(ABORT, 'terminal transaction failed'); END`); err != nil {
		t.Fatalf("create terminal transaction failure trigger: %v", err)
	}

	s.handleClawDoneSignal(clawID, "[DONE] https://github.com/org/repo/pull/42")

	select {
	case <-requests:
		t.Fatal("tracker was called although terminal DB transaction failed")
	default:
	}
}
