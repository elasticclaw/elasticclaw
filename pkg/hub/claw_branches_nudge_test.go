package hub

import (
	"net/http"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

const sweptPRPipeline = `stages:
  - id: open_pr
    label: Open PR
    entry: true
  - id: pr_opened
    label: PR Opened
    triggers:
      - message_contains: "[DONE]"
  - id: merged
    triggers:
      - pr_merged: {}
    terminal: true
`

const sweptPRURL = "https://github.com/owner/repo/pull/1"

// sweptPRServer is a claw in sweptPRPipeline whose branch carries an open PR
// opened by the hub's App.
func sweptPRServer(t *testing.T, clawID, stage string) *Server {
	t.Helper()
	t.Setenv("ELASTICCLAW_HUB_CONFIG", t.TempDir()+"/hub.yaml")
	s := newNudgeTestServer(t, clawID, []*types.FactoryConfig{{Name: "swept", PipelineYAML: sweptPRPipeline}})
	if _, err := s.db.Exec(`UPDATE claws SET pipeline_stage=?, github_repos='[{"repo":"owner/repo"}]' WHERE id=?`, stage, clawID); err != nil {
		t.Fatal(err)
	}
	if got := s.getPipelineStage(clawID); got != stage {
		t.Fatalf("pipeline stage = %q, want %q", got, stage)
	}
	if _, ok := s.findPipelineContextForClaw(clawID); !ok {
		t.Fatal("claw has no pipeline context")
	}
	branchSweepTestToken(s)
	oldClient := defaultGitHubClient
	defaultGitHubClient = newGitHubClient()
	t.Cleanup(func() { defaultGitHubClient = oldClient })
	defaultGitHubClient.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/repos/owner/repo/pulls" {
			return githubAppTokenResponse(200, `[]`), nil // registration watermarks
		}
		return githubAppTokenResponse(200, `[{"number":1,"html_url":"`+sweptPRURL+`","state":"open","user":{"login":"factory-app[bot]","type":"Bot"},"head":{"ref":"feature","sha":"`+strings.Repeat("a", 40)+`","repo":{"full_name":"owner/repo"}}}]`), nil
	})}
	return s
}

func sweepTestBranch(t *testing.T, s *Server, clawID string) {
	t.Helper()
	branch := clawBranch{Repo: "owner/repo", Branch: "feature", HeadSHA: strings.Repeat("a", 40)}
	if _, err := s.storeClawBranches(clawID, []clawBranch{branch}); err != nil {
		t.Fatal(err)
	}
	if err := s.sweepClawBranch(clawID, branch); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := s.db.QueryRow(`SELECT source FROM claw_prs WHERE claw_id=? AND pr_url=?`, clawID, sweptPRURL).Scan(&source); err != nil || source != "branch_sweep" {
		t.Fatalf("swept PR not registered: source=%q, %v", source, err)
	}
}

func sweptPRNudges(t *testing.T, s *Server, clawID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT content FROM messages WHERE claw_id=? AND role='hub' AND content LIKE '%has NOT received your%' ORDER BY rowid`, clawID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatal(err)
		}
		out = append(out, content)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBranchSweepNudgesStageAwaitingDone(t *testing.T) {
	const clawID = "swept-pr-claw"
	s := sweptPRServer(t, clawID, "open_pr")
	// The incident: transport errors paused the claw although the agent ran.
	if _, err := s.db.Exec(`UPDATE claws SET no_progress_paused=1, bridge_error_pause_kind='transport' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}

	sweepTestBranch(t, s, clawID)
	nudges := sweptPRNudges(t, s, clawID)
	if len(nudges) != 1 {
		t.Fatalf("nudges = %q, want one", nudges)
	}
	for _, want := range []string{sweptPRURL, "[DONE]", "Do not open another PR", "even if you believe you already sent it"} {
		if !strings.Contains(nudges[0], want) {
			t.Fatalf("nudge %q does not contain %q", nudges[0], want)
		}
	}
	var paused int
	var kind string
	if err := s.db.QueryRow(`SELECT no_progress_paused, bridge_error_pause_kind FROM claws WHERE id=?`, clawID).Scan(&paused, &kind); err != nil || paused != 0 || kind != "" {
		t.Fatalf("bridge-error pause not lifted: paused=%d kind=%q err=%v", paused, kind, err)
	}

	// A later sweep of the same PR, or a repeated nudge, adds nothing.
	if _, err := s.db.Exec(`UPDATE claw_branches SET pr_url='', checked_at=NULL WHERE claw_id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	sweepTestBranch(t, s, clawID)
	s.nudgeSweptPRStageSignal(clawID, sweptPRURL)
	if got := sweptPRNudges(t, s, clawID); len(got) != 1 {
		t.Fatalf("nudges after re-sweep = %d, want 1", len(got))
	}
}

func TestBranchSweepDoesNotNudgeAfterDone(t *testing.T) {
	const clawID = "swept-pr-done-claw"
	s := sweptPRServer(t, clawID, "pr_opened")
	if s.pipelineAwaitsDoneSignal(clawID) {
		t.Fatal("stage reached by [DONE] reported as awaiting it")
	}
	sweepTestBranch(t, s, clawID)
	if got := sweptPRNudges(t, s, clawID); len(got) != 0 {
		t.Fatalf("nudged after [DONE]: %q", got)
	}
}

func TestBranchSweepDoesNotNudgeTerminalStage(t *testing.T) {
	const clawID = "swept-pr-terminal-claw"
	s := sweptPRServer(t, clawID, "merged")
	sweepTestBranch(t, s, clawID)
	if got := sweptPRNudges(t, s, clawID); len(got) != 0 {
		t.Fatalf("nudged in a terminal stage: %q", got)
	}
}

func sweptBranchPRURL(t *testing.T, s *Server, clawID string) string {
	t.Helper()
	var prURL string
	if err := s.db.QueryRow(`SELECT pr_url FROM claw_branches WHERE claw_id=?`, clawID).Scan(&prURL); err != nil {
		t.Fatal(err)
	}
	return prURL
}

func resweepTestBranch(t *testing.T, s *Server, clawID string) {
	t.Helper()
	if err := s.sweepClawBranch(clawID, clawBranch{Repo: "owner/repo", Branch: "feature", HeadSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
}

// A running turn may still end with [DONE]; a nudge queued behind it would
// reach the next stage, so the sweep defers until the claw is idle.
func TestBranchSweepDefersNudgeWhileTurnRuns(t *testing.T) {
	const clawID = "swept-pr-busy-claw"
	s := sweptPRServer(t, clawID, "open_pr")
	s.mu.Lock()
	s.claws[clawID] = &clawConn{id: clawID, tenantID: "test-tenant-id", awaitingResponse: true}
	s.mu.Unlock()

	sweepTestBranch(t, s, clawID)
	if got := sweptPRNudges(t, s, clawID); len(got) != 0 {
		t.Fatalf("nudged mid-turn: %q", got)
	}
	if got := sweptBranchPRURL(t, s, clawID); got != "" {
		t.Fatalf("deferred branch settled with pr_url %q", got)
	}

	// The turn ends without the signal: the next sweep nudges.
	s.mu.Lock()
	delete(s.claws, clawID)
	s.mu.Unlock()
	resweepTestBranch(t, s, clawID)
	if got := sweptPRNudges(t, s, clawID); len(got) != 1 {
		t.Fatalf("nudges after the turn = %q, want one", got)
	}
	if got := sweptBranchPRURL(t, s, clawID); got != sweptPRURL {
		t.Fatalf("branch pr_url = %q, want %q", got, sweptPRURL)
	}
}

func TestBranchSweepSkipsNudgeAfterTurnDeliversPR(t *testing.T) {
	const clawID = "swept-pr-busy-done-claw"
	s := sweptPRServer(t, clawID, "open_pr")
	s.mu.Lock()
	s.claws[clawID] = &clawConn{id: clawID, tenantID: "test-tenant-id", awaitingResponse: true}
	s.mu.Unlock()
	sweepTestBranch(t, s, clawID)

	// The turn ends with [DONE] + URL before the async stage transition.
	if errURL := s.registerDonePRURLs(clawID, []string{sweptPRURL}); errURL != "" {
		t.Fatalf("register [DONE] PR %s", errURL)
	}
	s.mu.Lock()
	delete(s.claws, clawID)
	s.mu.Unlock()
	if !s.pipelineAwaitsDoneSignal(clawID) {
		t.Fatal("setup: stage no longer awaits [DONE]")
	}
	resweepTestBranch(t, s, clawID)
	if got := sweptPRNudges(t, s, clawID); len(got) != 0 {
		t.Fatalf("nudged for a PR [DONE] delivered: %q", got)
	}
	if got := sweptBranchPRURL(t, s, clawID); got != sweptPRURL {
		t.Fatalf("branch pr_url = %q, want %q", got, sweptPRURL)
	}
}

func TestDropSweptPRNudgesRemovesQueuedNudge(t *testing.T) {
	const clawID = "swept-pr-drop-claw"
	s := sweptPRServer(t, clawID, "open_pr")
	sweepTestBranch(t, s, clawID)
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE claw_id=? AND delivered_at IS NULL AND content LIKE '%has NOT received your%'`, clawID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("setup: pending nudges = %d, %v", pending, err)
	}
	s.injectMessage(clawID, "[hub] unrelated", "hub")

	s.dropSweptPRNudges(clawID)
	if got := sweptPRNudges(t, s, clawID); len(got) != 0 {
		t.Fatalf("queued nudge survived an accepted [DONE]: %q", got)
	}
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM messages WHERE claw_id=? AND content='[hub] unrelated'`, clawID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("unrelated message dropped: %d, %v", pending, err)
	}
}

func TestBranchSweepDoesNotNudgeWorkflowV2Claw(t *testing.T) {
	const clawID = "swept-pr-v2-claw"
	s := sweptPRServer(t, clawID, "open_pr")
	// Register as a legacy claw mid-turn (no nudge yet); V2 then takes over.
	s.mu.Lock()
	s.claws[clawID] = &clawConn{id: clawID, tenantID: "test-tenant-id", awaitingResponse: true}
	s.mu.Unlock()
	sweepTestBranch(t, s, clawID)
	s.mu.Lock()
	s.claws[clawID] = &clawConn{id: clawID, tenantID: "test-tenant-id", workflowV2Controlled: true}
	s.mu.Unlock()
	if !s.nudgeSweptPRStageSignal(clawID, sweptPRURL) {
		t.Fatal("idle V2 claw deferred the nudge")
	}
	if got := sweptPRNudges(t, s, clawID); len(got) != 0 {
		t.Fatalf("nudged a workflow V2 claw: %q", got)
	}
}

func TestBranchSweepDoesNotNudgeWithoutPipeline(t *testing.T) {
	const clawID = "swept-pr-no-pipeline-claw"
	s := sweptPRServer(t, clawID, "open_pr")
	if _, err := s.db.Exec(`UPDATE claws SET tags='[]' WHERE id=?`, clawID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.findPipelineContextForClaw(clawID); ok {
		t.Fatal("setup: claw still has a pipeline context")
	}
	sweepTestBranch(t, s, clawID)
	if got := sweptPRNudges(t, s, clawID); len(got) != 0 {
		t.Fatalf("nudged a claw without a pipeline: %q", got)
	}
}
