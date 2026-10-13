package hub

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newBranchCheckRepo clones a fresh bare origin into $HOME/.openclaw/workspace/<name>.
func newBranchCheckRepo(t *testing.T, home, name string) (repo, origin string) {
	t.Helper()
	origin = filepath.Join(home, "remotes", name+".git")
	seed := filepath.Join(home, "seed-"+name)
	runGit(t, home, "init", "-q", "--bare", "-b", "develop", origin)
	runGit(t, home, "init", "-q", "-b", "develop", seed)
	runGit(t, seed, "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, seed, "push", "-q", origin, "develop")
	repo = filepath.Join(home, ".openclaw", "workspace", name)
	runGit(t, home, "clone", "-q", origin, repo)
	return repo, origin
}

func commitOnNewBranch(t *testing.T, repo, branch string) {
	t.Helper()
	runGit(t, repo, "checkout", "-q", "-b", branch)
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", branch)
}

func TestStageBranchCheckScriptPushesOnlyTicketBranches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	unpushed, unpushedOrigin := newBranchCheckRepo(t, home, "unpushed")
	commitOnNewBranch(t, unpushed, "NEXT-1-work")

	// The ticket branch is pushed even when the agent switched away from it.
	offCheckout, offCheckoutOrigin := newBranchCheckRepo(t, home, "offcheckout")
	commitOnNewBranch(t, offCheckout, "NEXT-2-work")
	runGit(t, offCheckout, "checkout", "-q", "develop")

	onDefault, onDefaultOrigin := newBranchCheckRepo(t, home, "ondefault")
	runGit(t, onDefault, "commit", "-q", "--allow-empty", "-m", "oops")

	// main, master and release/* are never pushed, nor branches with no work.
	stray, strayOrigin := newBranchCheckRepo(t, home, "stray")
	commitOnNewBranch(t, stray, "main")
	commitOnNewBranch(t, stray, "release/1.0")
	runGit(t, stray, "checkout", "-q", "develop")
	runGit(t, stray, "branch", "NEXT-3-empty")

	dirty, _ := newBranchCheckRepo(t, home, "dirty")
	if err := os.WriteFile(filepath.Join(dirty, "x.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dirty, "add", "x.txt")

	// Untracked files alone are not reported.
	untracked, _ := newBranchCheckRepo(t, home, "untracked")
	if err := os.WriteFile(filepath.Join(untracked, "scratch.log"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	detached, _ := newBranchCheckRepo(t, home, "detached")
	runGit(t, detached, "checkout", "-q", "--detach")

	badRemote, _ := newBranchCheckRepo(t, home, "badremote")
	commitOnNewBranch(t, badRemote, "NEXT-4-work")
	runGit(t, badRemote, "remote", "set-url", "origin", filepath.Join(home, "missing.git"))

	clean, _ := newBranchCheckRepo(t, home, "clean")
	commitOnNewBranch(t, clean, "NEXT-1-done")
	runGit(t, clean, "push", "-q", "-u", "origin", "NEXT-1-done")

	result, err := executeLocalPipelineRun(context.Background(), `cd "$HOME/.openclaw/workspace" && `+stageBranchCheckScript)
	if err != nil {
		t.Fatalf("script: %v\nstderr: %s", err, result.Stderr)
	}
	pushed, problems := parseStageBranchCheck(result.Stdout)
	if want := []string{"offcheckout: NEXT-2-work", "unpushed: NEXT-1-work"}; !reflect.DeepEqual(pushed, want) {
		t.Fatalf("pushed=%q, want %q\nstdout:\n%s", pushed, want, result.Stdout)
	}
	wantProblems := []string{
		"badremote: push of NEXT-4-work failed",
		"detached: detached HEAD",
		"dirty: uncommitted changes",
		"ondefault: commits on the default branch develop",
	}
	if !reflect.DeepEqual(problems, wantProblems) {
		t.Fatalf("problems=%q, want %q\nstdout:\n%s", problems, wantProblems, result.Stdout)
	}
	if got, want := runGit(t, unpushedOrigin, "rev-parse", "NEXT-1-work"), runGit(t, unpushed, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin NEXT-1-work=%s, want %s", got, want)
	}
	if got, want := runGit(t, offCheckoutOrigin, "rev-parse", "NEXT-2-work"), runGit(t, offCheckout, "rev-parse", "NEXT-2-work"); got != want {
		t.Fatalf("origin NEXT-2-work=%s, want %s", got, want)
	}
	if got, want := runGit(t, onDefaultOrigin, "rev-list", "--count", "develop"), "1"; got != want {
		t.Fatalf("default branch was pushed: origin develop has %s commits, want %s", got, want)
	}
	if got := runGit(t, strayOrigin, "for-each-ref", "--format=%(refname:short)", "refs/heads"); got != "develop" {
		t.Fatalf("origin of stray has branches %q, want only develop", got)
	}
}

func TestRunOnEnterReportsBranchCheckThatCouldNotRun(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		ClawToken: "test-claw-token",
		Providers: map[string]types.ProviderConfig{"failing": {Type: "failing"}},
	}, "", "", "")

	const clawID = "claw-branch-check-fails"
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, provider, provider_id, github_repos, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "test-claw", "elasticclaw", "connected", "failing", "failing-id",
		`[{"repo":"acme/widgets","permissions":"write"}]`,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}

	countNotes := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=? AND content LIKE '[hub] Branch check before this stage: the hub could not finish checking your branches%'`, clawID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := s.runOnEnter(clawID, pipeline.Stage{ID: "start", Entry: true}, pipelineContext{}); err != nil {
		t.Fatal(err)
	}
	if n := countNotes(); n != 0 {
		t.Fatalf("entry stage ran the branch check: %d notes", n)
	}

	// Without an inject the note goes to the dashboard.
	if _, err := s.runOnEnter(clawID, pipeline.Stage{ID: "review"}, pipelineContext{}); err != nil {
		t.Fatal(err)
	}
	if n := countNotes(); n != 1 {
		t.Fatalf("got %d branch check notes, want 1", n)
	}
}

func TestStageBranchCheckScriptNeverContainsDotDot(t *testing.T) {
	if strings.Contains(stageBranchCheckScript, "..") {
		t.Fatal("hub-run commands must not contain \"..\"")
	}
}
