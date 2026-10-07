package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

type PostHog struct {
	Client  readhttp.Client
	project string
}

func NewPostHog(cfg config.PostHog, key string, client *http.Client) *PostHog {
	return &PostHog{Client: readhttp.Client{HTTP: client, BaseURL: strings.TrimRight(cfg.Host, "/"), Headers: http.Header{"Authorization": []string{"Bearer " + key}}, Secrets: []string{key}}, project: cfg.ProjectID}
}
func (p *PostHog) path(suffix string) string {
	return "/api/projects/" + url.PathEscape(p.project) + "/" + suffix
}
func (p *PostHog) Tools() []tools.Tool {
	return []tools.Tool{
		tools.ReadTool{Name: tools.Define("posthog_query", "Read rows with one SELECT or WITH HogQL query; at most 500 rows are returned.", map[string]any{"query": stringArg("A read-only SELECT or WITH HogQL query."), "limit": intArg("Maximum returned rows, capped at 500; default 100.")}, "query"), Source: "PostHog", Execute: p.Query},
		tools.ReadTool{Name: tools.Define("posthog_insights", "List saved insights, search by name, or read one insight and its cached result.", map[string]any{"search": stringArg("Optional insight name search."), "id": stringArg("Optional insight numeric ID or short ID; cannot combine with search."), "limit": intArg("Maximum returned insights, capped at 500; default 100.")}), Source: "PostHog", Execute: p.Insights},
	}
}
func (p *PostHog) Query(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := tools.DecodeArgs(raw, &args); err != nil {
		return tools.Result{}, err
	}
	if !readQuery(args.Query) {
		return tools.Result{}, fmt.Errorf("Only one read-only SELECT or WITH query is allowed.")
	}
	limit, err := rowLimit(args.Limit)
	if err != nil {
		return tools.Result{}, err
	}
	var response struct {
		Columns []any             `json:"columns"`
		Results []json.RawMessage `json:"results"`
		Error   json.RawMessage   `json:"error,omitempty"`
	}
	err = p.Client.Do(ctx, http.MethodPost, p.path("query/"), map[string]any{"query": map[string]any{"kind": "HogQLQuery", "query": args.Query}}, &response)
	if err != nil {
		return tools.Result{}, err
	}
	if len(response.Error) > 0 && string(response.Error) != "null" {
		return tools.Result{}, fmt.Errorf("PostHog rejected the read query.")
	}
	if len(response.Results) > limit {
		response.Results = response.Results[:limit]
	}
	return result("PostHog", response, len(response.Results))
}

var insightID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

func (p *PostHog) Insights(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		Search string `json:"search"`
		ID     string `json:"id"`
		Limit  int    `json:"limit"`
	}
	if err := tools.DecodeArgs(raw, &args); err != nil {
		return tools.Result{}, err
	}
	if len(args.Search) > 500 || (args.ID != "" && (!insightID.MatchString(args.ID) || args.Search != "")) {
		return tools.Result{}, fmt.Errorf("Invalid insight ID or search.")
	}
	limit, err := rowLimit(args.Limit)
	if err != nil {
		return tools.Result{}, err
	}
	if args.ID != "" {
		var response map[string]any
		if err := p.Client.Do(ctx, http.MethodGet, p.path("insights/"+url.PathEscape(args.ID)+"/"), nil, &response); err != nil {
			return tools.Result{}, err
		}
		return result("PostHog", response, 1)
	}
	query := url.Values{"limit": []string{fmt.Sprint(limit)}, "saved": []string{"true"}}
	if args.Search != "" {
		query.Set("search", args.Search)
	}
	var response struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := p.Client.Do(ctx, http.MethodGet, p.path("insights/?")+query.Encode(), nil, &response); err != nil {
		return tools.Result{}, err
	}
	if len(response.Results) > limit {
		response.Results = response.Results[:limit]
	}
	return result("PostHog", response.Results, len(response.Results))
}
func (p *PostHog) Health(ctx context.Context) (string, []string, error) {
	var project struct {
		Name string `json:"name"`
	}
	if err := p.Client.Do(ctx, http.MethodGet, p.path(""), nil, &project); err != nil {
		return "", nil, err
	}
	var definitions struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	}
	// Event metadata enriches the prompt but a missing metadata permission must not
	// disable otherwise usable project queries.
	_ = p.Client.Do(ctx, http.MethodGet, p.path("event_definitions/?limit=50"), nil, &definitions)
	events := make([]string, 0, 50)
	for _, event := range definitions.Results {
		if len(events) == 50 {
			break
		}
		if len(event.Name) <= 200 {
			events = append(events, event.Name)
		}
	}
	name := project.Name
	if name == "" {
		name = p.project
	}
	name = strings.Join(strings.Fields(name), " ")
	return "Project " + name + " · events and saved insights", events, nil
}
