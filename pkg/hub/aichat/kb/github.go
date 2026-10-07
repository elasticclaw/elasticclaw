// Package kb reads a workspace knowledge base without write capabilities.
package kb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	gh "github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/github"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

type KB interface {
	List(context.Context, string) ([]string, error)
	Read(context.Context, string) (string, error)
	Search(context.Context, string) ([]Match, error)
}
type Match struct {
	Path    string `json:"path"`
	Heading string `json:"heading"`
	Line    int    `json:"line"`
}
type GitHub struct {
	config config.KnowledgeBase
	client gh.Client
}

func New(cfg config.KnowledgeBase, api string, token func(string) string, client *http.Client) *GitHub {
	return &GitHub{config: cfg, client: gh.Client{BaseURL: api, HTTP: client, Token: token}}
}
func (g *GitHub) files(ctx context.Context) (string, []gh.Entry, error) {
	if !gh.ValidRepo(g.config.Repo) {
		return "", nil, errors.New("Invalid knowledge base repository.")
	}
	sha, err := g.client.Commit(ctx, g.config.Repo, g.config.Branch)
	if err != nil {
		return "", nil, err
	}
	tree, err := g.client.Tree(ctx, g.config.Repo, sha)
	if err != nil {
		return "", nil, err
	}
	files := []gh.Entry{}
	for _, e := range tree {
		if e.Type == "blob" && e.Mode != "120000" && markdown(e.Path) {
			files = append(files, e)
		}
	}
	return sha, files, nil
}
func markdown(file string) bool { return strings.HasSuffix(strings.ToLower(file), ".md") }
func (g *GitHub) List(ctx context.Context, dir string) ([]string, error) {
	if !gh.ValidPath(dir, true) {
		return nil, tools.ArgError("Invalid relative directory path.")
	}
	_, files, err := g.files(ctx)
	if err != nil {
		return nil, err
	}
	paths := []string{}
	for _, file := range files {
		if dir == "" || strings.HasPrefix(file.Path, dir+"/") {
			paths = append(paths, g.client.Redact(g.config.Repo, file.Path))
		}
	}
	return paths, nil
}
func (g *GitHub) Read(ctx context.Context, file string) (string, error) {
	if !gh.ValidRepo(g.config.Repo) || !gh.ValidPath(file, false) || !markdown(file) {
		return "", tools.ArgError("Expected a relative Markdown file path.")
	}
	sha, err := g.client.Commit(ctx, g.config.Repo, g.config.Branch)
	if err != nil {
		return "", err
	}
	return g.client.Read(ctx, g.config.Repo, file, sha)
}

const indexWorkers = 8
const maxIndexedFiles = 250
const maxHeadings = 5000
const maxIndexBytes = 8 * 1024 * 1024
const cacheTTL = 5 * time.Minute

type indexEntry struct {
	created  time.Time
	headings []Match
	next     int
	bytes    int
	complete bool
}

var indexes = struct {
	sync.Mutex
	entries map[string]indexEntry
}{entries: map[string]indexEntry{}}

func (g *GitHub) Search(ctx context.Context, query string) ([]Match, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 256 {
		return nil, tools.ArgError("Search query must contain 1 to 256 characters.")
	}
	sha, files, err := g.files(ctx)
	if err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s\x00%s\x00%s", g.client.BaseURL, g.config.Repo, sha)
	indexes.Lock()
	cached, ok := indexes.entries[key]
	indexes.Unlock()
	if !ok || time.Since(cached.created) >= cacheTTL {
		cached = indexEntry{created: time.Now()}
	}
	if !cached.complete {
		selected := make([]gh.Entry, 0, maxIndexedFiles)
		for _, file := range files {
			if file.Size <= gh.MaxFileBytes {
				selected = append(selected, file)
				if len(selected) == maxIndexedFiles {
					break
				}
			}
		}
		// Work on a private slice so concurrent searches cannot mutate cached data.
		cached.headings = append([]Match(nil), cached.headings...)
		for cached.next < len(selected) && len(cached.headings) < maxHeadings && cached.bytes < maxIndexBytes {
			batch := selected[cached.next:min(cached.next+indexWorkers, len(selected))]
			contents := make([]string, len(batch))
			errs := make([]error, len(batch))
			var wg sync.WaitGroup
			for i, file := range batch {
				wg.Add(1)
				go func(i int, file gh.Entry) {
					defer wg.Done()
					contents[i], errs[i] = g.client.Read(ctx, g.config.Repo, file.Path, sha)
				}(i, file)
			}
			wg.Wait()
			for i, file := range batch {
				if errs[i] != nil {
					// Keep completed files so the next request can resume at this commit.
					storeIndex(key, cached)
					return nil, errs[i]
				}
				if cached.bytes+len(contents[i]) > maxIndexBytes {
					cached.complete = true
					break
				}
				cached.bytes += len(contents[i])
				cached.next++
				cached.headings = append(cached.headings, parseHeadings(file.Path, contents[i])...)
				if len(cached.headings) >= maxHeadings {
					cached.headings = cached.headings[:maxHeadings]
					break
				}
			}
			if cached.complete {
				break
			}
		}
		cached.complete = true
		storeIndex(key, cached)
	}
	matches := []Match{}
	needle := strings.ToLower(query)
	for _, heading := range cached.headings {
		heading.Path = g.client.Redact(g.config.Repo, heading.Path)
		heading.Heading = g.client.Redact(g.config.Repo, heading.Heading)
		if strings.Contains(strings.ToLower(heading.Heading), needle) {
			matches = append(matches, heading)
			if len(matches) >= 100 {
				break
			}
		}
	}
	return matches, nil
}
func storeIndex(key string, entry indexEntry) {
	indexes.Lock()
	defer indexes.Unlock()
	if previous, ok := indexes.entries[key]; ok && time.Since(previous.created) < cacheTTL && (previous.complete || previous.next > entry.next) {
		return
	}
	if _, ok := indexes.entries[key]; !ok && len(indexes.entries) >= 32 {
		oldestKey := ""
		var oldest time.Time
		for k, v := range indexes.entries {
			if oldestKey == "" || v.created.Before(oldest) {
				oldestKey, oldest = k, v.created
			}
		}
		delete(indexes.entries, oldestKey)
	}
	indexes.entries[key] = entry
}

func parseHeadings(file, content string) []Match {
	matches := []Match{}
	fenced := false
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		n := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
		if n > 0 && n <= 6 && len(trimmed) > n && trimmed[n] == ' ' {
			title := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(trimmed[n:]), "#"))
			if title != "" {
				matches = append(matches, Match{file, title, i + 1})
			}
			continue
		}
		if i > 0 && len(trimmed) >= 3 && (strings.Trim(trimmed, "=") == "" || strings.Trim(trimmed, "-") == "") && strings.TrimSpace(lines[i-1]) != "" {
			matches = append(matches, Match{file, strings.TrimSpace(lines[i-1]), i})
		}
	}
	return matches
}
func (g *GitHub) Health(ctx context.Context) (string, error) {
	if _, err := g.Read(ctx, g.config.Entry); err != nil {
		return "", err
	}
	return g.client.Redact(g.config.Repo, g.config.Repo+" · "+g.config.Entry), nil
}
func (g *GitHub) Tools() []tools.Tool {
	return []tools.Tool{
		tools.ReadTool{Name: tools.Define("kb_list", "List Markdown paths in the knowledge base, optionally under a relative directory.", map[string]any{"dir": map[string]any{"type": "string", "description": "Optional relative directory; no traversal."}}), Source: "Knowledge base", Execute: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			var a struct {
				Dir string `json:"dir"`
			}
			if err := tools.DecodeArgs(raw, &a); err != nil {
				return tools.Result{}, err
			}
			paths, err := g.List(ctx, a.Dir)
			if err != nil {
				return tools.Result{}, err
			}
			data, _ := json.Marshal(paths)
			return tools.Result{Summary: fmt.Sprintf("Listed %d knowledge base pages", len(paths)), Data: string(data), RowCount: len(paths)}, nil
		}},
		tools.ReadTool{Name: tools.Define("kb_read", "Read one Markdown file from the knowledge base.", map[string]any{"path": map[string]any{"type": "string", "description": "Relative Markdown file path; no traversal."}}, "path"), Source: "Knowledge base", Execute: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := tools.DecodeArgs(raw, &a); err != nil {
				return tools.Result{}, err
			}
			data, err := g.Read(ctx, a.Path)
			if err != nil {
				return tools.Result{}, err
			}
			return tools.Result{Summary: g.client.Redact(g.config.Repo, "Read "+g.config.Repo+"/"+a.Path), Data: data, RowCount: 1}, nil
		}},
		tools.ReadTool{Name: tools.Define("kb_search", "Search a cached Markdown heading index (up to 250 files, 128 KB per file and 5,000 headings; refreshed every five minutes).", map[string]any{"query": map[string]any{"type": "string", "description": "Text to match in Markdown headings."}}, "query"), Source: "Knowledge base", Execute: func(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := tools.DecodeArgs(raw, &a); err != nil {
				return tools.Result{}, err
			}
			matches, err := g.Search(ctx, a.Query)
			if err != nil {
				return tools.Result{}, err
			}
			data, _ := json.Marshal(matches)
			return tools.Result{Summary: fmt.Sprintf("Found %d knowledge base headings", len(matches)), Data: string(data), RowCount: len(matches)}, nil
		}},
	}
}
