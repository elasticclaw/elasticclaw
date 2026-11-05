package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpClawInput struct {
	ClawID string `json:"claw_id" jsonschema:"Agent ID"`
}
type mcpListClawsInput struct {
	Status string `json:"status,omitempty" jsonschema:"Filter by displayed agent status"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum agents returned; default 50"`
}
type mcpTimelineInput struct {
	mcpClawInput
	Before string `json:"before,omitempty" jsonschema:"Exclusive timestamp cursor"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Page size; default 50 maximum 100"`
}
type mcpLogFilters struct {
	From     string `json:"from,omitempty" jsonschema:"Exclusive lower timestamp bound (RFC3339Nano)"`
	To       string `json:"to,omitempty" jsonschema:"Exclusive upper timestamp bound (RFC3339Nano)"`
	Before   string `json:"before,omitempty" jsonschema:"Exclusive timestamp pagination cursor"`
	BeforeID string `json:"before_id,omitempty" jsonschema:"Message ID paired with before for stable pagination"`
	Limit    int    `json:"limit,omitempty" jsonschema:"Page size; default 200 maximum 500"`
	Order    string `json:"order,omitempty" jsonschema:"Sort order: asc (default) or desc"`
}
type mcpActivityInput struct {
	mcpClawInput
	mcpLogFilters
}
type mcpDiagnosticsInput struct {
	mcpClawInput
	MaxBytes int `json:"max_bytes,omitempty" jsonschema:"Total log tail bytes; default 65536 maximum 262144"`
}
type mcpWorkspaceInput struct {
	Workspace string `json:"workspace" jsonschema:"Workspace name"`
}
type mcpWorkflowInput struct {
	mcpWorkspaceInput
	Workflow string `json:"workflow" jsonschema:"Workflow name"`
}
type mcpWorkflowRunsInput struct {
	mcpWorkflowInput
	Status string `json:"status,omitempty" jsonschema:"Run status filter"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum runs returned; default 50"`
}
type mcpRunInput struct {
	RunID string `json:"run_id" jsonschema:"Workflow run ID"`
}
type mcpRunLogsInput struct {
	mcpRunInput
	mcpLogFilters
}
type mcpAttemptLogsInput struct {
	mcpRunLogsInput
	AttemptID string `json:"attempt_id" jsonschema:"Workflow attempt ID"`
}
type mcpAnalyticsFilters struct {
	From             string `json:"from,omitempty" jsonschema:"Start date as RFC3339 or Unix milliseconds"`
	To               string `json:"to,omitempty" jsonschema:"End date as RFC3339 or Unix milliseconds"`
	Status           string `json:"status,omitempty" jsonschema:"Comma-separated run statuses"`
	Owner            string `json:"owner,omitempty" jsonschema:"Comma-separated owner IDs or display names"`
	OwnerType        string `json:"owner_type,omitempty" jsonschema:"Comma-separated owner types"`
	Workspace        string `json:"workspace,omitempty" jsonschema:"Comma-separated workspace names"`
	Workflow         string `json:"workflow,omitempty" jsonschema:"Comma-separated workflow names"`
	Factory          string `json:"factory,omitempty" jsonschema:"Comma-separated factory names"`
	Integration      string `json:"integration,omitempty" jsonschema:"Comma-separated integrations"`
	Repo             string `json:"repo,omitempty" jsonschema:"Comma-separated repositories"`
	Model            string `json:"model,omitempty" jsonschema:"Comma-separated model names"`
	WarningType      string `json:"warning_type,omitempty" jsonschema:"Comma-separated warning types"`
	FailureType      string `json:"failure_type,omitempty" jsonschema:"Comma-separated failure types"`
	RequiresPR       *bool  `json:"requires_pr,omitempty" jsonschema:"Filter runs requiring a pull request"`
	AnalyticsEnabled *bool  `json:"analytics_enabled,omitempty" jsonschema:"Filter by analytics enabled flag"`
	HumanTouched     *bool  `json:"human_touched,omitempty" jsonschema:"Filter by human intervention"`
	MergedPRs        *bool  `json:"merged_prs,omitempty" jsonschema:"Filter by merged pull requests"`
}
type mcpAnalyticsGeneralInput struct {
	mcpAnalyticsFilters
	TZ string `json:"tz,omitempty" jsonschema:"IANA time zone for business hours"`
}
type mcpAnalyticsRunsInput struct {
	mcpAnalyticsGeneralInput
	Limit  int    `json:"limit,omitempty" jsonschema:"Page size; default 50 maximum 200"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque pagination cursor from previous response"`
}
type mcpAnalyticsRunInput struct {
	mcpRunInput
	TZ string `json:"tz,omitempty" jsonschema:"IANA time zone for business hours"`
}
type mcpAnalyticsTicketsInput struct {
	mcpAnalyticsFilters
	Key    string `json:"key,omitempty" jsonschema:"Ticket key filter"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque pagination cursor from previous response"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Ticket page size"`
}
type mcpAnalyticsCostsInput struct {
	mcpAnalyticsFilters
	Days    int    `json:"days,omitempty" jsonschema:"Daily series window; default 30 range 1 to 366"`
	Scope   string `json:"scope,omitempty" jsonschema:"Set to ledger for ledger scope; omit for default"`
	GroupBy string `json:"groupBy,omitempty" jsonschema:"Set to model for model cost series"`
}
type mcpAnalyticsDriversInput struct {
	mcpAnalyticsFilters
	GroupBy string `json:"groupBy,omitempty" jsonschema:"Cost grouping: factory (default) or workflow"`
}
type mcpSendInput struct {
	mcpClawInput
	Content string `json:"content" jsonschema:"Message to send to the agent"`
}
type mcpTriggerInput struct {
	mcpWorkflowInput
	Input map[string]any `json:"input,omitempty" jsonschema:"Complete trigger request body passed unchanged; for example an object containing inputs"`
}
type mcpUpdateInput struct {
	mcpClawInput
	Name  *string   `json:"name,omitempty" jsonschema:"New agent name"`
	Tags  *[]string `json:"tags,omitempty" jsonschema:"Replacement tags; empty array clears tags"`
	Color *string   `json:"color,omitempty" jsonschema:"New agent color"`
}

// Only explicitly selected fields become query parameters; path and body
// arguments are never accidentally forwarded as filters.
func mcpQuery(input any, names ...string) url.Values {
	data, _ := json.Marshal(input)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	query := url.Values{}
	for _, name := range names {
		raw, ok := fields[name]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil {
			query.Set(name, text)
		} else {
			query.Set(name, string(raw))
		}
	}
	return query
}

func mcpAddTool[In any](server *mcp.Server, name, description string, call func(context.Context, In) *mcp.CallToolResult) {
	interact := name == "send_message" || name == "trigger_workflow" || name == "update_claw" || name == "kill_claw"
	destructive := name == "kill_claw"
	mcp.AddTool(server, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{
		ReadOnlyHint: !interact, IdempotentHint: name == "update_claw", DestructiveHint: &destructive,
	}}, func(ctx context.Context, _ *mcp.CallToolRequest, input In) (*mcp.CallToolResult, any, error) {
		return call(ctx, input), nil, nil
	})
}

// REST prefix handlers split URL.Path themselves. Reject separators rather
// than allowing an ID to select a different subresource through those handlers.
func mcpPath(parts ...string) string {
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\%\x00\r\n") {
			return ""
		}
	}
	escaped := make([]string, len(parts))
	for i, part := range parts {
		escaped[i] = url.PathEscape(part)
	}
	return "/" + strings.Join(escaped, "/")
}

func (s *Server) registerFactoryMCPTools(server *mcp.Server) {
	get := func(ctx context.Context, handler http.HandlerFunc, path string, values map[string]string, query url.Values) *mcp.CallToolResult {
		if path == "" {
			return mcpText("Invalid path identifier", true)
		}
		return s.mcpCallHandler(ctx, handler, http.MethodGet, path, values, query, nil)
	}
	logFields := []string{"from", "to", "before", "before_id", "limit", "order"}
	analyticsFields := []string{"from", "to", "status", "owner", "owner_type", "workspace", "workflow", "factory", "integration", "repo", "model", "warning_type", "failure_type", "requires_pr", "analytics_enabled", "human_touched", "merged_prs"}
	analyticsQuery := func(input any, extra ...string) url.Values {
		return mcpQuery(input, append(append([]string{}, analyticsFields...), extra...)...)
	}
	clawValues := func(id string) map[string]string { return map[string]string{"id": id} }
	runValues := func(id string) map[string]string { return map[string]string{"runId": id} }
	workflowValues := func(input mcpWorkflowInput) map[string]string {
		return map[string]string{"workspace": input.Workspace, "workflow": input.Workflow}
	}

	mcpAddTool(server, "list_claws", "List visible agents, optionally filtered by status.", func(ctx context.Context, in mcpListClawsInput) *mcp.CallToolResult {
		handler := func(w http.ResponseWriter, r *http.Request) {
			raw := httptest.NewRecorder()
			s.handleClaws(raw, r)
			if raw.Code < 200 || raw.Code >= 300 {
				w.WriteHeader(raw.Code)
				_, _ = w.Write(raw.Body.Bytes())
				return
			}
			var claws []json.RawMessage
			if err := json.Unmarshal(raw.Body.Bytes(), &claws); err != nil {
				jsonError(w, 500, "invalid agent response")
				return
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 50
			}
			filtered := []json.RawMessage{}
			for _, claw := range claws {
				var item struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(claw, &item)
				if in.Status != "" && item.Status != in.Status {
					continue
				}
				filtered = append(filtered, claw)
				if len(filtered) >= limit {
					break
				}
			}
			jsonOK(w, filtered)
		}
		return get(ctx, handler, "/api/claws", nil, nil)
	})
	mcpAddTool(server, "get_claw", "Get a visible agent.", func(ctx context.Context, in mcpClawInput) *mcp.CallToolResult {
		return get(ctx, s.handleClawDetail, mcpPath("api", "claws", in.ClawID), clawValues(in.ClawID), nil)
	})
	mcpAddTool(server, "get_claw_timeline", "Read an agent's conversation timeline.", func(ctx context.Context, in mcpTimelineInput) *mcp.CallToolResult {
		return get(ctx, s.handleMessages, mcpPath("api", "messages", in.ClawID, "timeline"), nil, mcpQuery(in, "before", "limit"))
	})
	mcpAddTool(server, "get_claw_activity", "Read agent activity logs.", func(ctx context.Context, in mcpActivityInput) *mcp.CallToolResult {
		return get(ctx, s.handleMessages, mcpPath("api", "messages", in.ClawID, "activity"), nil, mcpQuery(in, logFields...))
	})
	mcpAddTool(server, "get_claw_prs", "List an agent's pull requests.", func(ctx context.Context, in mcpClawInput) *mcp.CallToolResult {
		return get(ctx, s.handleClawSubresource, mcpPath("api", "claws", in.ClawID, "prs"), nil, nil)
	})
	mcpAddTool(server, "get_gateway_diagnostics", "Read captured gateway and bridge log tails, newest captures first, with a total byte budget.", func(ctx context.Context, in mcpDiagnosticsInput) *mcp.CallToolResult {
		return get(ctx, s.handleMCPGatewayDiagnostics, "/diagnostics", clawValues(in.ClawID), url.Values{"limit": {fmt.Sprint(in.MaxBytes)}})
	})
	mcpAddTool(server, "list_workspaces", "List workspaces.", func(ctx context.Context, _ struct{}) *mcp.CallToolResult {
		return get(ctx, s.handleWorkspacesCRUD, "/api/workspaces", nil, nil)
	})
	mcpAddTool(server, "list_workflows", "List workflows in a workspace.", func(ctx context.Context, in mcpWorkspaceInput) *mcp.CallToolResult {
		return get(ctx, s.handleWorkspaceWorkflowsList, mcpPath("api", "workspaces", in.Workspace, "workflows"), map[string]string{"name": in.Workspace}, nil)
	})
	mcpAddTool(server, "list_workflow_runs", "List workflow runs.", func(ctx context.Context, in mcpWorkflowRunsInput) *mcp.CallToolResult {
		return get(ctx, s.handleWorkflowV2Runs, mcpPath("api", "v2", "workspaces", in.Workspace, "workflows", in.Workflow, "runs"), workflowValues(in.mcpWorkflowInput), mcpQuery(in, "status", "limit"))
	})
	mcpAddTool(server, "get_workflow_run", "Get a workflow run.", func(ctx context.Context, in mcpRunInput) *mcp.CallToolResult {
		return get(ctx, s.handleWorkflowV2Run, mcpPath("api", "v2", "workflow-runs", in.RunID), runValues(in.RunID), nil)
	})
	mcpAddTool(server, "get_workflow_run_logs", "Read workflow run activity logs.", func(ctx context.Context, in mcpRunLogsInput) *mcp.CallToolResult {
		return get(ctx, s.handleWorkflowV2RunLogs, mcpPath("api", "v2", "workflow-runs", in.RunID, "logs"), runValues(in.RunID), mcpQuery(in, logFields...))
	})
	mcpAddTool(server, "list_workflow_run_attempts", "List workflow run attempts.", func(ctx context.Context, in mcpRunInput) *mcp.CallToolResult {
		return get(ctx, s.handleWorkflowV2RunAttempts, mcpPath("api", "v2", "workflow-runs", in.RunID, "attempts"), runValues(in.RunID), nil)
	})
	mcpAddTool(server, "get_workflow_attempt_logs", "Read workflow attempt activity logs.", func(ctx context.Context, in mcpAttemptLogsInput) *mcp.CallToolResult {
		return get(ctx, s.handleWorkflowV2AttemptLogs, mcpPath("api", "v2", "workflow-runs", in.RunID, "attempts", in.AttemptID, "logs"), map[string]string{"runId": in.RunID, "attemptId": in.AttemptID}, mcpQuery(in, logFields...))
	})
	mcpAddTool(server, "analytics_summary", "Summarize workflow analytics.", func(ctx context.Context, in mcpAnalyticsFilters) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsSummary, "/api/analytics/summary", nil, analyticsQuery(in))
	})
	mcpAddTool(server, "analytics_general_stats", "Read general workflow statistics.", func(ctx context.Context, in mcpAnalyticsGeneralInput) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsGeneralStats, "/api/analytics/general-stats", nil, analyticsQuery(in, "tz"))
	})
	mcpAddTool(server, "analytics_effectiveness", "Read workflow effectiveness metrics.", func(ctx context.Context, in mcpAnalyticsFilters) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsEffectiveness, "/api/analytics/effectiveness", nil, analyticsQuery(in))
	})
	mcpAddTool(server, "analytics_runs", "List analytics runs.", func(ctx context.Context, in mcpAnalyticsRunsInput) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsRuns, "/api/analytics/runs", nil, analyticsQuery(in, "limit", "cursor", "tz"))
	})
	mcpAddTool(server, "analytics_run", "Read analytics for one run.", func(ctx context.Context, in mcpAnalyticsRunInput) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsRuns, mcpPath("api", "analytics", "runs", in.RunID), nil, mcpQuery(in, "tz"))
	})
	mcpAddTool(server, "analytics_tickets", "List ticket analytics.", func(ctx context.Context, in mcpAnalyticsTicketsInput) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsTickets, "/api/analytics/tickets", nil, analyticsQuery(in, "key", "cursor", "limit"))
	})
	mcpAddTool(server, "analytics_costs", "Read workflow cost analytics.", func(ctx context.Context, in mcpAnalyticsCostsInput) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsCosts, "/api/analytics/costs", nil, analyticsQuery(in, "days", "scope", "groupBy"))
	})
	mcpAddTool(server, "analytics_cost_drivers", "Read workflow cost drivers.", func(ctx context.Context, in mcpAnalyticsDriversInput) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsCostDrivers, "/api/analytics/cost-drivers", nil, analyticsQuery(in, "groupBy"))
	})
	mcpAddTool(server, "analytics_filter_options", "List available analytics filters.", func(ctx context.Context, _ struct{}) *mcp.CallToolResult {
		return get(ctx, s.handleTaskRunAnalyticsFilterOptions, "/api/analytics/filter-options", nil, nil)
	})

	mcpAddTool(server, "send_message", "Send a message to an agent; offline messages are queued.", func(ctx context.Context, in mcpSendInput) *mcp.CallToolResult {
		path := mcpPath("api", "messages", in.ClawID)
		if path == "" {
			return mcpText("Invalid claw_id", true)
		}
		return s.mcpCallHandler(ctx, s.handleMessages, http.MethodPost, path, nil, nil, map[string]string{"content": in.Content})
	})
	mcpAddTool(server, "trigger_workflow", "Trigger a workflow with an optional request body.", func(ctx context.Context, in mcpTriggerInput) *mcp.CallToolResult {
		path := mcpPath("api", "workspaces", in.Workspace, "workflows", in.Workflow, "trigger")
		if path == "" {
			return mcpText("Invalid workspace or workflow", true)
		}
		if in.Input == nil {
			in.Input = map[string]any{}
		}
		return s.mcpCallHandler(ctx, s.handleWorkspaceWorkflowTrigger, http.MethodPost, path, workflowValues(in.mcpWorkflowInput), nil, in.Input)
	})
	mcpAddTool(server, "update_claw", "Update an agent's name, tags, or color.", func(ctx context.Context, in mcpUpdateInput) *mcp.CallToolResult {
		path := mcpPath("api", "claws", in.ClawID)
		if path == "" {
			return mcpText("Invalid claw_id", true)
		}
		body := map[string]any{}
		if in.Name != nil {
			body["name"] = *in.Name
		}
		if in.Tags != nil {
			body["tags"] = *in.Tags
		}
		if in.Color != nil {
			body["color"] = *in.Color
		}
		return s.mcpCallHandler(ctx, s.handleClawDetail, http.MethodPatch, path, clawValues(in.ClawID), nil, body)
	})
	mcpAddTool(server, "kill_claw", "Irreversibly terminates the agent and its sandbox.", func(ctx context.Context, in mcpClawInput) *mcp.CallToolResult {
		path := mcpPath("api", "claws", in.ClawID)
		if path == "" {
			return mcpText("Invalid claw_id", true)
		}
		return s.mcpCallHandler(ctx, s.handleClawDetail, http.MethodDelete, path, clawValues(in.ClawID), nil, nil)
	})
}
