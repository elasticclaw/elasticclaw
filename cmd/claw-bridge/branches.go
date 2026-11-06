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
	out, err := branchGit(ctx, dir, "for-each-ref", "--count=257", "--format=%(refname:short)%00%(upstream:remotename)%00%(upstream:remoteref)%00%(objectname)%00%(upstream)", "refs/heads/")
	if err != nil {
		return nil, err
	}
	type remoteInfo struct{ repo, defaultRef string }
	remotes := make(map[string]remoteInfo)
	var branches []workspaceBranch
	lines := strings.Split(out, "\n")
	if len(lines) > 256 {
		return nil, fmt.Errorf("repository branch limit exceeded")
	}
	for _, line := range lines {
		fields := strings.Split(line, "\x00")
		if len(fields) != 5 || fields[1] == "" || fields[1] == "." || fields[0] == "main" || fields[0] == "master" {
			continue
		}
		remote, ok := remotes[fields[1]]
		if !ok {
			remoteURL, err := branchGit(ctx, dir, "remote", "get-url", "--", fields[1])
			if err != nil {
				return nil, err
			}
			remote.repo = githubRemoteRepo(remoteURL)
			if remote.repo != "" {
				// Missing remote HEAD is normal for freshly added remotes.
				remote.defaultRef, _ = branchGit(ctx, dir, "symbolic-ref", "refs/remotes/"+fields[1]+"/HEAD")
			}
			remotes[fields[1]] = remote
		}
		branch := strings.TrimPrefix(fields[2], "refs/heads/")
		if remote.repo == "" || !strings.HasPrefix(fields[2], "refs/heads/") || branch == "main" || branch == "master" || fields[4] == remote.defaultRef {
			continue
		}
		// An upstream alone does not prove ownership: gh pr checkout tracks other
		// authors' branches too. Report only branches this sandbox pushed.
		if !pushedFromHere(ctx, dir, fields[4]) {
			continue
		}
		// GitHub knows the upstream branch name, which can differ from the local name.
		branches = append(branches, workspaceBranch{Repo: remote.repo, Branch: branch, HeadSHA: fields[3]})
	}
	return branches, ctx.Err()
}

// pushedFromHere reports whether git recorded a push to the remote-tracking ref.
// Fetches and clones log other reasons, so only a local push leaves this entry.
func pushedFromHere(ctx context.Context, dir, trackingRef string) bool {
	out, err := branchGit(ctx, dir, "reflog", "show", "--format=%gs", trackingRef, "--")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "update by push" {
			return true
		}
	}
	return false
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
