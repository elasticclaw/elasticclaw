package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type workspaceBranch struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
}

// One worker coalesces turn completions; git and HTTP never delay a reply.
type branchReporter struct {
	wake                       chan struct{}
	workspace, endpoint, token string
	client                     *http.Client
	lastReport                 []byte
}

func newBranchReporter(ctx context.Context, hubURL, clawID, token string) *branchReporter {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[branches] home directory: %v", err)
		return nil
	}
	hubURL = strings.Replace(strings.Replace(hubURL, "wss://", "https://", 1), "ws://", "http://", 1)
	r := &branchReporter{
		wake: make(chan struct{}, 1), workspace: filepath.Join(home, ".openclaw", "workspace"),
		endpoint: strings.TrimRight(hubURL, "/") + "/api/claws/" + url.PathEscape(clawID) + "/branches",
		token:    token, client: &http.Client{Timeout: 10 * time.Second},
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
				if err := r.report(ctx); err != nil {
					log.Printf("[branches] report failed: %v", err)
				}
			}
		}
	}()
	return r
}

func (r *branchReporter) trigger() {
	if r != nil {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
}

func (r *branchReporter) report(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	branches, err := collectWorkspaceBranches(ctx, r.workspace)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		Branches []workspaceBranch `json:"branches"`
	}{branches})
	if err != nil || bytes.Equal(body, r.lastReport) {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned %d", resp.StatusCode)
	}
	r.lastReport = body
	return nil
}

func branchGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func collectWorkspaceBranches(ctx context.Context, workspace string) ([]workspaceBranch, error) {
	branches := make(map[string]workspaceBranch)
	var visit func(string, int) error
	visit = func(dir string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			found, err := collectRepoBranches(ctx, dir)
			if err != nil {
				log.Printf("[branches] skipping repository %s: %v", dir, err)
			}
			for _, branch := range found {
				branches[branch.Repo+"\x00"+branch.Branch] = branch
			}
			if len(branches) > 256 {
				return fmt.Errorf("workspace branch limit exceeded")
			}
		}
		if depth == 2 {
			return nil
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			log.Printf("[branches] skipping directory %s: %v", dir, err)
			return nil
		}
		for _, entry := range entries {
			if entry.IsDir() && entry.Name() != ".git" {
				if err := visit(filepath.Join(dir, entry.Name()), depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(workspace, 0); err != nil {
		return nil, err
	}
	result := make([]workspaceBranch, 0, len(branches))
	for _, branch := range branches {
		result = append(result, branch)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Repo != result[j].Repo {
			return result[i].Repo < result[j].Repo
		}
		return result[i].Branch < result[j].Branch
	})
	return result, nil
}

func collectRepoBranches(ctx context.Context, dir string) ([]workspaceBranch, error) {
	remoteNames, err := branchGit(ctx, dir, "remote")
	if err != nil {
		return nil, err
	}
	// Remote-tracking refs, not upstreams: a plain "git push origin x" (no -u)
	// records the push on refs/remotes/origin/x without setting an upstream.
	out, err := branchGit(ctx, dir, "for-each-ref", "--count=257", "--format=%(refname)", "refs/remotes/")
	if err != nil {
		return nil, err
	}
	refs := strings.Split(out, "\n")
	if len(refs) > 256 {
		return nil, fmt.Errorf("repository branch limit exceeded")
	}
	var branches []workspaceBranch
	for _, name := range strings.Fields(remoteNames) {
		prefix := "refs/remotes/" + name + "/"
		var repo, defaultRef string
		for _, ref := range refs {
			branch, ok := strings.CutPrefix(ref, prefix)
			if !ok || branch == "HEAD" || branch == "main" || branch == "master" {
				continue
			}
			if repo == "" {
				remoteURL, err := branchGit(ctx, dir, "remote", "get-url", "--", name)
				if err != nil {
					return nil, err
				}
				if repo = githubRemoteRepo(remoteURL); repo == "" {
					break
				}
				// Missing remote HEAD is normal for freshly added remotes.
				defaultRef, _ = branchGit(ctx, dir, "symbolic-ref", prefix+"HEAD")
			}
			if ref == defaultRef {
				continue
			}
			// A tracking ref alone does not prove ownership: gh pr checkout and
			// fetches create them too. Report only branches this sandbox pushed,
			// with the pushed SHA, which is what a PR of this claw points at.
			pushed := lastPushedSHA(ctx, dir, ref)
			if pushed == "" {
				continue
			}
			branches = append(branches, workspaceBranch{Repo: repo, Branch: branch, HeadSHA: pushed})
		}
	}
	return branches, ctx.Err()
}

// lastPushedSHA returns the commit of the newest push git recorded for the
// remote-tracking ref, or "". Fetches and clones log other reasons, so only a
// local push leaves this entry.
func lastPushedSHA(ctx context.Context, dir, trackingRef string) string {
	out, err := branchGit(ctx, dir, "reflog", "show", "--format=%H%x00%gs", trackingRef, "--")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if sha, subject, ok := strings.Cut(line, "\x00"); ok && subject == "update by push" {
			return sha
		}
	}
	return ""
}

func githubRemoteRepo(remote string) string {
	if strings.HasPrefix(remote, "git@github.com:") {
		remote = "ssh://git@github.com/" + strings.TrimPrefix(remote, "git@github.com:")
	}
	u, err := url.Parse(remote)
	if err != nil || !strings.EqualFold(u.Hostname(), "github.com") || (u.Scheme != "https" && u.Scheme != "ssh" && u.Scheme != "git" && u.Scheme != "http") {
		return ""
	}
	repo := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return repo
}
