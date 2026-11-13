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
