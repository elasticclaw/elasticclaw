// Package tracker provides read-only issue tracking. Creation belongs to the
// separate user-initiated action registry introduced with ticket handoffs.
package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

type Tracker interface {
	Search(context.Context, json.RawMessage) (tools.Result, error)
	Get(context.Context, json.RawMessage) (tools.Result, error)
}
type Linear struct {
	Client readhttp.Client
	team   string
}

func NewLinear(team, token string, client *http.Client) *Linear {
	return &Linear{Client: readhttp.Client{HTTP: client, BaseURL: "https://api.linear.app", Headers: http.Header{"Authorization": []string{token}}, Secrets: []string{token}}, team: team}
}
func (l *Linear) Tools() []tools.Tool {
	return []tools.Tool{
		tools.ReadTool{Name: tools.Define("linear_search", "Search existing Linear issues to find context and avoid duplicates.", map[string]any{"query": map[string]any{"type": "string", "description": "Text to search in issues."}, "team": map[string]any{"type": "string", "description": "Optional team name, key, or UUID; defaults to the configured team."}, "limit": map[string]any{"type": "integer", "description": "Maximum issues, 1 to 100; default 25."}}, "query"), Source: "Linear", Execute: l.Search},
		tools.ReadTool{Name: tools.Define("linear_get", "Read one Linear issue, including state, assignee, labels and a bounded description.", map[string]any{"id": map[string]any{"type": "string", "description": "Issue identifier, for example ABC-123."}}, "id"), Source: "Linear", Execute: l.Get},
	}
}

const issueFields = `identifier title state { name } assignee { name } labels(first: 50) { nodes { name } } description url`
const searchQuery = `query SearchIssues($query: String!, $limit: Int!, $filter: IssueFilter!) { searchIssues(term: $query, first: $limit, filter: $filter) { nodes { ` + issueFields + ` } } }`
const getQuery = `query GetIssue($id: String!) { issue(id: $id) { ` + issueFields + ` } }`
const healthQuery = `query ChatTeam($filter: TeamFilter!) { teams(first: 1, filter: $filter) { nodes { id name } } }`

type issue struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	State      struct {
		Name string `json:"name"`
	} `json:"state"`
	Assignee *struct {
		Name string `json:"name"`
	} `json:"assignee"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,49}-[1-9][0-9]{0,12}$`)

func teamFilter(team string) map[string]any {
	if uuid.MatchString(team) {
		return map[string]any{"id": map[string]any{"eq": team}}
	}
	return map[string]any{"or": []any{map[string]any{"name": map[string]any{"eq": team}}, map[string]any{"key": map[string]any{"eq": team}}}}
}
func (l *Linear) request(ctx context.Context, query string, variables any, out any) error {
	var response struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := l.Client.Do(ctx, http.MethodPost, "/graphql", map[string]any{"query": query, "variables": variables}, &response); err != nil {
		return err
	}
	if len(response.Errors) > 0 {
		return fmt.Errorf("Linear rejected the read query.")
	}
	if len(response.Data) == 0 || string(response.Data) == "null" || json.Unmarshal(response.Data, out) != nil {
		return fmt.Errorf("Invalid Linear response.")
	}
	return nil
}
func issueResult(issues []issue) (tools.Result, error) {
	for i := range issues {
		if len(issues[i].Description) > 8000 {
			end := 8000
			for !utf8.RuneStart(issues[i].Description[end]) {
				end--
			}
			issues[i].Description = issues[i].Description[:end] + "\n[truncated]"
		}
	}
	data, err := json.Marshal(issues)
	if err != nil {
		return tools.Result{}, fmt.Errorf("Invalid Linear response.")
	}
	return tools.Result{Summary: fmt.Sprintf("Read %d issues from Linear", len(issues)), Data: string(data), RowCount: len(issues)}, nil
}
func (l *Linear) Search(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Query string `json:"query"`
		Team  string `json:"team"`
		Limit int    `json:"limit"`
	}
	if err := tools.DecodeArgs(raw, &args); err != nil {
		return tools.Result{}, err
	}
	if strings.TrimSpace(args.Query) == "" || len(args.Query) > 500 || len(args.Team) > 200 || args.Limit < 0 || args.Limit > 100 {
		return tools.Result{}, tools.ArgError("Invalid issue search or limit.")
	}
	if args.Limit == 0 {
		args.Limit = 25
	}
	if args.Team == "" {
		args.Team = l.team
	}
	if strings.TrimSpace(args.Team) == "" {
		return tools.Result{}, tools.ArgError("A team is required for issue search.")
	}
	var response struct {
		SearchIssues struct {
			Nodes []issue `json:"nodes"`
		} `json:"searchIssues"`
	}
	if err := l.request(ctx, searchQuery, map[string]any{"query": args.Query, "limit": args.Limit, "filter": map[string]any{"team": teamFilter(args.Team)}}, &response); err != nil {
		return tools.Result{}, err
	}
	if len(response.SearchIssues.Nodes) > args.Limit {
		response.SearchIssues.Nodes = response.SearchIssues.Nodes[:args.Limit]
	}
	return issueResult(response.SearchIssues.Nodes)
}
func (l *Linear) Get(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := tools.DecodeArgs(raw, &args); err != nil {
		return tools.Result{}, err
	}
	if !identifier.MatchString(args.ID) {
		return tools.Result{}, tools.ArgError("Use an issue identifier such as ABC-123.")
	}
	var response struct {
		Issue *issue `json:"issue"`
	}
	if err := l.request(ctx, getQuery, map[string]any{"id": args.ID}, &response); err != nil {
		return tools.Result{}, err
	}
	if response.Issue == nil {
		return tools.Result{}, fmt.Errorf("Issue not found.")
	}
	return issueResult([]issue{*response.Issue})
}
func (l *Linear) Health(ctx context.Context) (string, error) {
	var response struct {
		Teams struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := l.request(ctx, healthQuery, map[string]any{"filter": teamFilter(l.team)}, &response); err != nil {
		return "", err
	}
	if len(response.Teams.Nodes) == 0 {
		return "", fmt.Errorf("The configured Linear team was not found.")
	}
	return "Team " + strings.Join(strings.Fields(response.Teams.Nodes[0].Name), " ") + " · reads issues to avoid duplicates", nil
}
