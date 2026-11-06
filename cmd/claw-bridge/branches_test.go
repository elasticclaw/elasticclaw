package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func branchTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func branchTestRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	branchTestGit(t, dir, "init", "-b", "main")
	branchTestGit(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "Initial")
	sha := branchTestGit(t, dir, "rev-parse", "HEAD")
	branchTestGit(t, dir, "remote", "add", "origin", "git@github.com:owner/repo.git")
	// Pushes go to a local bare repository while the fetch URL stays on GitHub.
	bare := filepath.Join(t.TempDir(), "remote.git")
	branchTestGit(t, dir, "init", "--bare", bare)
	branchTestGit(t, dir, "config", "url."+bare+".pushInsteadOf", "git@github.com:owner/repo.git")
	for _, branch := range []string{"main", "master", "trunk", "feature", "checked-out", "unpublished", "pushed-without-upstream", "pushed-renamed"} {
		if branch != "main" {
			branchTestGit(t, dir, "branch", branch)
		}
		switch branch {
		case "unpublished":
			continue
		case "pushed-without-upstream":
			// A plain "git push origin x" records the push but sets no upstream.
			branchTestGit(t, dir, "push", "-q", "origin", branch)
			continue
		case "pushed-renamed":
			// Pushed under another remote name, again with no upstream.
			branchTestGit(t, dir, "push", "-q", "origin", branch+":refs/heads/remote-renamed")
			continue
		case "checked-out":
			// Fetched like gh pr checkout does: tracked, but never pushed from here.
			branchTestGit(t, dir, "update-ref", "-m", "fetch: storing head", "refs/remotes/origin/"+branch, sha)
		default:
			branchTestGit(t, dir, "push", "-q", "origin", branch)
		}
		branchTestGit(t, dir, "branch", "--set-upstream-to=origin/"+branch, branch)
	}
	branchTestGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	return sha
}

func TestCollectWorkspaceBranches(t *testing.T) {
	workspace := t.TempDir()
	sha := branchTestRepo(t, filepath.Join(workspace, "group", "repo"))
	branchTestRepo(t, filepath.Join(workspace, "too", "deep", "repo"))
	other := filepath.Join(workspace, "other")
	branchTestRepo(t, other)
	branchTestGit(t, other, "remote", "set-url", "origin", "https://gitlab.com/owner/repo.git")
	for i := 0; i < 300; i++ {
		if err := os.MkdirAll(filepath.Join(workspace, "noise", fmt.Sprint(i)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(workspace, "broken"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "broken", ".git"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := collectWorkspaceBranches(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != (workspaceBranch{Repo: "owner/repo", Branch: "feature", HeadSHA: sha}) ||
		got[1] != (workspaceBranch{Repo: "owner/repo", Branch: "pushed-without-upstream", HeadSHA: sha}) ||
		got[2] != (workspaceBranch{Repo: "owner/repo", Branch: "remote-renamed", HeadSHA: sha}) {
		t.Fatalf("branches: %#v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collectWorkspaceBranches(ctx, workspace); err == nil {
		t.Fatal("canceled scan succeeded")
	}
}

func TestGitHubRemoteRepo(t *testing.T) {
	for remote, want := range map[string]string{
		"git@github.com:owner/repo.git": "owner/repo", "https://github.com/owner/repo": "owner/repo",
		"ssh://git@github.com/owner/repo.git": "owner/repo", "https://github.com.evil/owner/repo": "",
		"https://gitlab.com/owner/repo": "", "/local/repo": "", "https://github.com/owner/repo/extra": "",
	} {
		if got := githubRemoteRepo(remote); got != want {
			t.Errorf("%s = %q, want %q", remote, got, want)
		}
	}
}

type branchReportTransport func(*http.Request) (*http.Response, error)

func (f branchReportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBranchReporterChangesAndRetries(t *testing.T) {
	workspace := t.TempDir()
	repo := filepath.Join(workspace, "repo")
	branchTestRepo(t, repo)
	calls := 0
	status := http.StatusInternalServerError
	r := &branchReporter{workspace: workspace, endpoint: "https://hub/api/claws/test/branches", token: "secret"}
	r.client = &http.Client{Transport: branchReportTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		if _, ok := req.Context().Deadline(); !ok {
			t.Error("missing timeout")
		}
		var report struct {
			Branches []workspaceBranch `json:"branches"`
		}
		if err := json.NewDecoder(req.Body).Decode(&report); err != nil || len(report.Branches) != 3 {
			t.Errorf("report: %#v, %v", report, err)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	if err := r.report(context.Background()); err == nil {
		t.Fatal("failed report accepted")
	}
	status = http.StatusOK
	for i := 0; i < 2; i++ {
		if err := r.report(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("unchanged report resent: %d calls", calls)
	}
	branchTestGit(t, repo, "checkout", "feature")
	branchTestGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "Changed")
	// The report carries the last pushed SHA, so an unpushed commit changes nothing.
	if err := r.report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("unpushed commit resent the report: %d calls", calls)
	}
	branchTestGit(t, repo, "push", "-q", "origin", "feature")
	if err := r.report(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("changed report not sent: %d calls", calls)
	}
}

// A full clone mirrors every branch under refs/remotes; only the branch this
// sandbox pushed may be reported, however many others were fetched.
func TestCollectRepoBranchesIgnoresFetchedRemoteRefs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	sha := branchTestRepo(t, dir)
	var refs strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&refs, "update refs/remotes/origin/fetched-%d %s\n", i, sha)
	}
	cmd := exec.Command("git", "-C", dir, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(refs.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v: %s", err, out)
	}
	got, err := collectRepoBranches(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Branch != "feature" || got[1].Branch != "pushed-without-upstream" || got[2].Branch != "remote-renamed" {
		t.Fatalf("branches: %#v", got)
	}
}

func TestSplitTrackingRefRejectsAmbiguousRemotes(t *testing.T) {
	remotes := []string{"origin", "origin/fork", "upstream"}
	for ref, want := range map[string][2]string{
		"refs/remotes/upstream/topic":    {"upstream", "topic"},
		"refs/remotes/origin/feature/x":  {"origin", "feature/x"},
		"refs/remotes/origin/fork/topic": {"", ""},
		"refs/remotes/other/topic":       {"", ""},
		"":                               {"", ""},
	} {
		if remote, branch := splitTrackingRef(ref, remotes); remote != want[0] || branch != want[1] {
			t.Errorf("%q: got (%q, %q), want %v", ref, remote, branch, want)
		}
	}
}
