package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

type Datadog struct {
	Client readhttp.Client
	env    string
}

func NewDatadog(cfg config.Datadog, apiKey, appKey string, client *http.Client) *Datadog {
	return &Datadog{Client: readhttp.Client{HTTP: client, BaseURL: "https://api." + cfg.Site, Headers: http.Header{"Dd-Api-Key": []string{apiKey}, "Dd-Application-Key": []string{appKey}}, Secrets: []string{apiKey, appKey}}, env: cfg.Env}
}
func (d *Datadog) Tools() []tools.Tool {
	window := func() map[string]any {
		return map[string]any{"from": stringArg("Window start in RFC3339; maximum window is 7 days."), "to": stringArg("Window end in RFC3339.")}
	}
	metrics := window()
	metrics["aggregation"] = map[string]any{"type": "string", "enum": []string{"avg", "sum", "min", "max"}, "description": "Metric aggregation."}
	metrics["metric"] = stringArg("Datadog metric name.")
	metrics["tags"] = map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Additional exact tag filters; cannot change the configured environment."}
	metrics["groupBy"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional tag names to group by."}
	logs := window()
	logs["query"] = stringArg("Optional log search within the configured environment; do not include an env filter.")
	logs["limit"] = intArg("Maximum log rows, capped at 500; default 100.")
	return []tools.Tool{
		tools.ReadTool{Name: tools.Define("datadog_metrics", "Read metric series restricted to the configured environment. Build queries from structured fields.", metrics, "from", "to", "aggregation", "metric"), Source: "Datadog", Execute: d.Metrics},
		tools.ReadTool{Name: tools.Define("datadog_logs", "Search log rows restricted to the configured environment; maximum 7 days and 500 rows.", logs, "from", "to"), Source: "Datadog", Execute: d.Logs},
	}
}

var metricName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]{0,199}$`)
var tagName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.\/-]{0,99}$`)
var tagValue = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\/-]{0,199}$`)
var envFilter = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])env\s*:`)

func window(from, to string) (time.Time, time.Time, error) {
	start, e1 := time.Parse(time.RFC3339, from)
	end, e2 := time.Parse(time.RFC3339, to)
	if e1 != nil || e2 != nil || !end.After(start) || end.Sub(start) > 7*24*time.Hour {
		return time.Time{}, time.Time{}, fmt.Errorf("Use an RFC3339 time window of at most 7 days.")
	}
	return start, end, nil
}
func (d *Datadog) Metrics(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		From        string            `json:"from"`
		To          string            `json:"to"`
		Aggregation string            `json:"aggregation"`
		Metric      string            `json:"metric"`
		Tags        map[string]string `json:"tags"`
		GroupBy     []string          `json:"groupBy"`
	}
	if err := tools.DecodeArgs(raw, &args); err != nil {
		return tools.Result{}, err
	}
	start, end, err := window(args.From, args.To)
	if err != nil {
		return tools.Result{}, err
	}
	if !tagValue.MatchString(d.env) || !metricName.MatchString(args.Metric) || len(args.Tags) > 20 || len(args.GroupBy) > 10 {
		return tools.Result{}, fmt.Errorf("Invalid metric or tag filter.")
	}
	switch args.Aggregation {
	case "avg", "sum", "min", "max":
	default:
		return tools.Result{}, fmt.Errorf("Invalid metric aggregation.")
	}
	tags := []string{"env:" + d.env}
	for key, value := range args.Tags {
		if !tagName.MatchString(key) || !tagValue.MatchString(value) || (strings.EqualFold(key, "env") && (key != "env" || value != d.env)) {
			return tools.Result{}, fmt.Errorf("Invalid tag filter; the configured environment cannot be changed.")
		}
		if key != "env" {
			tags = append(tags, key+":"+value)
		}
	}
	sort.Strings(tags)
	query := args.Aggregation + ":" + args.Metric + "{" + strings.Join(tags, ",") + "}"
	for _, group := range args.GroupBy {
		if !tagName.MatchString(group) {
			return tools.Result{}, fmt.Errorf("Invalid group-by tag.")
		}
	}
	if len(args.GroupBy) > 0 {
		query += " by {" + strings.Join(args.GroupBy, ",") + "}"
	}
	params := url.Values{"from": []string{fmt.Sprint(start.Unix())}, "to": []string{fmt.Sprint(end.Unix())}, "query": []string{query}}
	var response struct {
		Series []json.RawMessage `json:"series"`
		Status string            `json:"status"`
		Error  string            `json:"error,omitempty"`
	}
	if err := d.Client.Do(ctx, http.MethodGet, "/api/v1/query?"+params.Encode(), nil, &response); err != nil {
		return tools.Result{}, err
	}
	if response.Error != "" || response.Status == "error" {
		return tools.Result{}, fmt.Errorf("Datadog rejected the metric query.")
	}
	return result("Datadog", response.Series, len(response.Series))
}

// The user query must stay inside the mandatory environment conjunction. Balanced
// parentheses and quotes, with no escapes, prevent breaking out of its wrapper.
func validLogQuery(query string) bool {
	if len(query) > 4000 || envFilter.MatchString(query) || strings.ContainsAny(query, "\\\x00\r\n") {
		return false
	}
	depth := 0
	quoted := false
	for _, c := range query {
		if c == '"' {
			quoted = !quoted
			continue
		}
		if quoted {
			continue
		}
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return !quoted && depth == 0
}
func (d *Datadog) Logs(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var args struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := tools.DecodeArgs(raw, &args); err != nil {
		return tools.Result{}, err
	}
	if _, _, err := window(args.From, args.To); err != nil {
		return tools.Result{}, err
	}
	if !tagValue.MatchString(d.env) || !validLogQuery(args.Query) {
		return tools.Result{}, fmt.Errorf("Invalid log query; the configured environment cannot be changed.")
	}
	limit, err := rowLimit(args.Limit)
	if err != nil {
		return tools.Result{}, err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		query = "*"
	}
	body := map[string]any{"filter": map[string]any{"from": args.From, "to": args.To, "query": "env:" + d.env + " (" + query + ")"}, "page": map[string]any{"limit": limit}, "sort": "timestamp"}
	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := d.Client.Do(ctx, http.MethodPost, "/api/v2/logs/events/search", body, &response); err != nil {
		return tools.Result{}, err
	}
	if len(response.Data) > limit {
		response.Data = response.Data[:limit]
	}
	return result("Datadog", response.Data, len(response.Data))
}
func (d *Datadog) Health(ctx context.Context) (string, error) {
	if !tagValue.MatchString(d.env) {
		return "", fmt.Errorf("Invalid configured Datadog environment.")
	}
	var response struct {
		Valid bool `json:"valid"`
	}
	if err := d.Client.Do(ctx, http.MethodGet, "/api/v1/validate", nil, &response); err != nil {
		return "", err
	}
	if !response.Valid {
		return "", fmt.Errorf("The API key was rejected.")
	}
	// v2 validate checks the application key as well as the API key.
	var application struct {
		Status string `json:"status"`
	}
	if err := d.Client.Do(ctx, http.MethodGet, "/api/v2/validate_keys", nil, &application); err != nil {
		return "", err
	}
	if application.Status != "ok" {
		return "", fmt.Errorf("The application key could not be validated.")
	}
	return "Metrics and logs · env " + d.env, nil
}
