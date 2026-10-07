// Package repos exposes read-only tools scoped to configured repositories.
package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	gh "github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/github"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

type GitHub struct {
	repositories []string
	client       gh.Client
}

func New(repositories []string, api string, token func(string) string, client *http.Client) *GitHub {
	return &GitHub{repositories: append([]string(nil), repositories...), client: gh.Client{BaseURL: api, HTTP: client, Token: token}}
}

type reference struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	PR   *int   `json:"pr"`
}

func (g *GitHub) allowed(repo string) bool {
	if !gh.ValidRepo(repo) {
		return false
	}
	for _, configured := range g.repositories {
		if configured == repo {
			return true
		}
	}
	return false
}
func (g *GitHub) resolve(ctx context.Context, a reference) (string, error) {
	if !g.allowed(a.Repo) {
		return "", errors.New("Repository is not configured for this workspace.")
	}
	if (a.PR != nil && (*a.PR <= 0 || *a.PR > 100000000)) || (a.Ref != "" && a.PR != nil) || !gh.ValidRef(a.Ref) {
		return "", errors.New("Specify either a valid branch reference or a positive pull request number.")
	}
	if a.PR != nil {
		return g.client.PullHead(ctx, a.Repo, *a.PR)
	}
	return g.client.Commit(ctx, a.Repo, a.Ref)
}
func (g *GitHub) Health(ctx context.Context) (string, error) {
	for _, repo := range g.repositories {
		if !g.allowed(repo) {
			return "", errors.New("Invalid repository configuration.")
		}
		if _, err := g.client.DefaultBranch(ctx, repo); err != nil {
			return "", err
		}
	}
	detail := strings.Join(g.repositories, ", ") + " · default branch"
	for _, repo := range g.repositories {
		detail = g.client.Redact(repo, detail)
	}
	return detail, nil
}
func refProperties() map[string]any {
	return map[string]any{
		"repo": map[string]any{"type": "string", "description": "One exact owner/repo from the workspace configuration."},
		"ref":  map[string]any{"type": "string", "description": "Optional branch name; defaults to the repository's default branch. Mutually exclusive with pr."},
		"pr":   map[string]any{"type": "integer", "minimum": 1, "description": "Optional pull request number; reads its head commit. Mutually exclusive with ref."},
	}
}
func (g *GitHub) Tools() []tools.Tool {
	treeProps := refProperties()
	treeProps["dir"] = map[string]any{"type": "string", "description": "Optional relative directory to list."}
	readProps := refProperties()
	readProps["path"] = map[string]any{"type": "string", "description": "Relative text file path; no traversal."}
	return []tools.Tool{
		tools.ReadTool{Name: tools.Define("repo_tree", "List repository paths at the default branch, a named branch, or a pull request head (maximum 5,000 tree entries).", treeProps, "repo"), Source: "Repositories", Execute: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			var a struct {
				reference
				Dir string `json:"dir"`
			}
			if err := tools.DecodeArgs(raw, &a); err != nil {
				return tools.Result{}, err
			}
			if !gh.ValidPath(a.Dir, true) {
				return tools.Result{}, errors.New("Invalid relative directory path.")
			}
			sha, err := g.resolve(ctx, a.reference)
			if err != nil {
				return tools.Result{}, err
			}
			tree, err := g.client.Tree(ctx, a.Repo, sha)
			if err != nil {
				return tools.Result{}, err
			}
			paths := []string{}
			for _, entry := range tree {
				if a.Dir == "" || strings.HasPrefix(entry.Path, a.Dir+"/") {
					paths = append(paths, g.client.Redact(a.Repo, entry.Path))
				}
			}
			data, _ := json.Marshal(paths)
			return tools.Result{Summary: fmt.Sprintf("Listed %d repository paths", len(paths)), Data: string(data), RowCount: len(paths)}, nil
		}},
		tools.ReadTool{Name: tools.Define("repo_read", "Read a text file at the default branch, a named branch, or a pull request head (maximum 128 KB).", readProps, "repo", "path"), Source: "Repositories", Execute: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			var a struct {
				reference
				Path string `json:"path"`
			}
			if err := tools.DecodeArgs(raw, &a); err != nil {
				return tools.Result{}, err
			}
			if !gh.ValidPath(a.Path, false) {
				return tools.Result{}, errors.New("Invalid relative file path.")
			}
			sha, err := g.resolve(ctx, a.reference)
			if err != nil {
				return tools.Result{}, err
			}
			data, err := g.client.Read(ctx, a.Repo, a.Path, sha)
			if err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Summary: g.client.Redact(a.Repo, "Read "+a.Repo+"/"+a.Path), Data: data, RowCount: 1}, nil
		}},
		tools.ReadTool{Name: tools.Define("repo_search", "Search code in one configured repository. GitHub code search covers the default branch only; branch and pull request references are not supported.", map[string]any{"repo": refProperties()["repo"], "query": map[string]any{"type": "string", "description": "Search text without repository/organization qualifiers or Boolean operators."}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum matches; defaults to 30."}}, "repo", "query"), Source: "Repositories", Execute: g.search},
	}
}
func (g *GitHub) search(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var a struct {
		Repo  string `json:"repo"`
		Query string `json:"query"`
		Limit *int   `json:"limit"`
	}
	if err := tools.DecodeArgs(raw, &a); err != nil {
		return tools.Result{}, err
	}
	if !g.allowed(a.Repo) {
		return tools.Result{}, errors.New("Repository is not configured for this workspace.")
	}
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" || len(a.Query) > 256 || strings.ContainsAny(a.Query, ":\x00\r\n()\"") {
		return tools.Result{}, errors.New("Use plain search text without qualifiers or Boolean operators.")
	}
	for _, word := range strings.Fields(strings.ToUpper(a.Query)) {
		if word == "OR" || word == "NOT" || word == "AND" {
			return tools.Result{}, errors.New("Use plain search text without Boolean operators.")
		}
	}
	limit := 30
	if a.Limit != nil {
		limit = *a.Limit
	}
	if limit < 1 || limit > 100 {
		return tools.Result{}, errors.New("Search limit must be between 1 and 100.")
	}
	var response struct {
		Items []struct {
			Path       string `json:"path"`
			URL        string `json:"html_url"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			TextMatches []struct {
				Fragment string `json:"fragment"`
			} `json:"text_matches"`
		} `json:"items"`
	}
	endpoint := "/search/code?q=" + url.QueryEscape("repo:"+a.Repo+" "+a.Query) + fmt.Sprintf("&per_page=%d", limit)
	if err := g.client.Get(ctx, a.Repo, endpoint, &response); err != nil {
		return tools.Result{}, err
	}
	type match struct {
		Path      string   `json:"path"`
		URL       string   `json:"url"`
		Fragments []string `json:"fragments,omitempty"`
	}
	matches := []match{}
	for _, item := range response.Items {
		if !strings.EqualFold(item.Repository.FullName, a.Repo) || !gh.ValidPath(item.Path, false) {
			continue
		}
		entry := match{Path: g.client.Redact(a.Repo, item.Path), URL: g.client.Redact(a.Repo, item.URL)}
		for _, text := range item.TextMatches {
			entry.Fragments = append(entry.Fragments, g.client.Redact(a.Repo, text.Fragment))
		}
		matches = append(matches, entry)
		if len(matches) >= limit {
			break
		}
	}
	data, _ := json.Marshal(matches)
	return tools.Result{Summary: fmt.Sprintf("Found %d repository code matches", len(matches)), Data: string(data), RowCount: len(matches)}, nil
}
