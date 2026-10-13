package hub

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

func TestStageBranchCheckScriptPushesOnlyTicketBranches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	unpushed, unpushedOrigin := newBranchCheckRepo(t, home, "unpushed")
	runGit(t, unpushed, "checkout", "-q", "-b", "NEXT-1-work")
	runGit(t, unpushed, "commit", "-q", "--allow-empty", "-m", "work")

	onDefault, onDefaultOrigin := newBranchCheckRepo(t, home, "ondefault")
	runGit(t, onDefault, "commit", "-q", "--allow-empty", "-m", "oops")

	dirty, _ := newBranchCheckRepo(t, home, "dirty")
	if err := os.WriteFile(filepath.Join(dirty, "x.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	clean, _ := newBranchCheckRepo(t, home, "clean")
	runGit(t, clean, "checkout", "-q", "-b", "NEXT-1-done")
	runGit(t, clean, "commit", "-q", "--allow-empty", "-m", "done")
	runGit(t, clean, "push", "-q", "-u", "origin", "NEXT-1-done")

	result, err := executeLocalPipelineRun(context.Background(), `cd "$HOME/.openclaw/workspace" && `+stageBranchCheckScript)
	if err != nil {
		t.Fatalf("script: %v\nstderr: %s", err, result.Stderr)
	}
	pushed, problems := parseStageBranchCheck(result.Stdout)
	if want := []string{"unpushed: NEXT-1-work"}; !reflect.DeepEqual(pushed, want) {
		t.Fatalf("pushed=%q, want %q\nstdout:\n%s", pushed, want, result.Stdout)
	}
	if want := []string{"dirty: uncommitted changes", "ondefault: commits on the default branch develop"}; !reflect.DeepEqual(problems, want) {
		t.Fatalf("problems=%q, want %q\nstdout:\n%s", problems, want, result.Stdout)
	}
	if got, want := runGit(t, unpushedOrigin, "rev-parse", "NEXT-1-work"), runGit(t, unpushed, "rev-parse", "HEAD"); got != want {
		t.Fatalf("origin NEXT-1-work=%s, want %s", got, want)
	}
	if got, want := runGit(t, onDefaultOrigin, "rev-list", "--count", "develop"), "1"; got != want {
		t.Fatalf("default branch was pushed: origin develop has %s commits, want %s", got, want)
	}
}

func TestStageBranchCheckScriptNeverContainsDotDot(t *testing.T) {
	if strings.Contains(stageBranchCheckScript, "..") {
		t.Fatal("hub-run commands must not contain \"..\"")
	}
}
