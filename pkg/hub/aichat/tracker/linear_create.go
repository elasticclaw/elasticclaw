package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
)

const createTeamQuery = `query TicketTeam($filter: TeamFilter!) { teams(first: 2, filter: $filter) { nodes { id name } } }`
const createStatesQuery = `query TicketStates($filter: WorkflowStateFilter!, $after: String) { workflowStates(first: 100, filter: $filter, after: $after) { nodes { id name } pageInfo { hasNextPage endCursor } } }`
const createLabelsQuery = `query TicketLabels($filter: IssueLabelFilter!, $after: String) { issueLabels(first: 100, filter: $filter, after: $after) { nodes { id name } pageInfo { hasNextPage endCursor } } }`
const createIssueMutation = `mutation CreateChatTicket($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { identifier url title state { name } } } }`

type namedID struct{ ID, Name string }
type namedPage struct {
	Nodes    []namedID
	PageInfo struct {
		HasNextPage bool
		EndCursor   string
	}
}

func (l *Linear) createError(message string) error {
	return &CreateError{Message: readhttp.Redact(message, l.Client.Secrets)}
}
func (l *Linear) permissionError(team string) error {
	return l.createError(fmt.Sprintf("The workspace token cannot create issues in team %s.", team))
}

func (l *Linear) createRequest(ctx context.Context, query string, variables any, out any, team string, mutation bool) error {
	var response struct {
		Data   json.RawMessage
		Errors []struct {
			Message    string
			Extensions struct {
				Code string
				Type string
			}
		}
	}
	err := l.Client.Do(ctx, http.MethodPost, "/graphql", map[string]any{"query": query, "variables": variables}, &response)
	if err != nil {
		var status *readhttp.StatusError
		if errors.As(err, &status) {
			if status.Code == 401 || status.Code == 403 {
				return l.permissionError(team)
			}
			if mutation && status.Code >= 500 {
				return l.createError(LinearUnconfirmed)
			}
			return l.createError(LinearRejected)
		}
		if mutation {
			return l.createError(LinearUnconfirmed)
		}
		return l.createError(LinearRejected)
	}
	if len(response.Errors) > 0 {
		for _, e := range response.Errors {
			category := strings.ToLower(e.Extensions.Code + " " + e.Extensions.Type + " " + e.Message)
			for _, word := range []string{"permission", "forbidden", "unauthorized", "authentication", "access denied", "not authorized"} {
				if strings.Contains(category, word) {
					return l.permissionError(team)
				}
			}
		}
		return l.createError(LinearRejected)
	}
	if len(response.Data) == 0 || string(response.Data) == "null" || json.Unmarshal(response.Data, out) != nil {
		if mutation {
			return l.createError(LinearUnconfirmed)
		}
		return l.createError(LinearRejected)
	}
	return nil
}

func (l *Linear) resolveTeam(ctx context.Context, team string) (namedID, error) {
	var result struct{ Teams namedPage }
	if err := l.createRequest(ctx, createTeamQuery, map[string]any{"filter": teamFilter(team)}, &result, team, false); err != nil {
		return namedID{}, err
	}
	if len(result.Teams.Nodes) != 1 || result.Teams.Nodes[0].ID == "" {
		return namedID{}, l.createError("The configured Linear team was not found or is ambiguous.")
	}
	return result.Teams.Nodes[0], nil
}

func (l *Linear) Target(ctx context.Context) (Target, error) {
	team, err := l.resolveTeam(ctx, l.team)
	return LinearTarget(team.Name, "", nil), err
}

func (l *Linear) names(ctx context.Context, query string, filter any, team string) ([]namedID, error) {
	var names []namedID
	var after any
	for page := 0; page < 100; page++ {
		var result struct{ WorkflowStates, IssueLabels namedPage }
		if err := l.createRequest(ctx, query, map[string]any{"filter": filter, "after": after}, &result, team, false); err != nil {
			return nil, err
		}
		items := result.IssueLabels
		if query == createStatesQuery {
			items = result.WorkflowStates
		}
		names = append(names, items.Nodes...)
		if !items.PageInfo.HasNextPage {
			return names, nil
		}
		if items.PageInfo.EndCursor == "" || items.PageInfo.EndCursor == after {
			break
		}
		after = items.PageInfo.EndCursor
	}
	return nil, l.createError(LinearRejected)
}

func (l *Linear) Create(ctx context.Context, draft Draft) (Issue, error) {
	if strings.TrimSpace(draft.Title) == "" || utf8.RuneCountInString(draft.Title) > 70 || strings.TrimSpace(draft.Body) == "" {
		return Issue{}, l.createError(LinearRejected)
	}
	if err := ctx.Err(); err != nil {
		return Issue{}, err
	}
	teamName := draft.Team
	if teamName == "" {
		teamName = l.team
	}
	team, err := l.resolveTeam(ctx, teamName)
	if err != nil {
		return Issue{}, err
	}
	input := map[string]any{"teamId": team.ID, "title": readhttp.Redact(draft.Title, l.Client.Secrets), "description": readhttp.Redact(draft.Body, l.Client.Secrets)}
	if draft.State != "" {
		states, err := l.names(ctx, createStatesQuery, map[string]any{"team": map[string]any{"id": map[string]any{"eq": team.ID}}}, team.Name)
		if err != nil {
			return Issue{}, err
		}
		for _, state := range states {
			if strings.EqualFold(state.Name, draft.State) {
				input["stateId"] = state.ID
				break
			}
		}
		if input["stateId"] == nil {
			return Issue{}, l.createError(fmt.Sprintf("State %q does not exist in Linear team %s.", draft.State, team.Name))
		}
	}
	labels := []string{}
	if len(draft.Labels) > 0 {
		filter := map[string]any{"or": []any{map[string]any{"team": map[string]any{"id": map[string]any{"eq": team.ID}}}, map[string]any{"team": map[string]any{"null": true}}}}
		available, err := l.names(ctx, createLabelsQuery, filter, team.Name)
		if err != nil {
			return Issue{}, err
		}
		ids := []string{}
		seen := map[string]bool{}
		for _, name := range draft.Labels {
			found := false
			for _, label := range available {
				if strings.EqualFold(label.Name, name) {
					if !seen[label.ID] {
						ids = append(ids, label.ID)
						labels = append(labels, label.Name)
						seen[label.ID] = true
					}
					found = true
					break
				}
			}
			if !found {
				return Issue{}, l.createError(fmt.Sprintf("Label %q does not exist in Linear team %s.", name, team.Name))
			}
		}
		input["labelIds"] = ids
	}
	if err := ctx.Err(); err != nil {
		return Issue{}, err
	}
	if draft.BeforeSend != nil {
		if err := draft.BeforeSend(); err != nil {
			return Issue{}, err
		}
	}
	var result struct {
		IssueCreate struct {
			Success bool
			Issue   *issue
		}
	}
	if err := l.createRequest(ctx, createIssueMutation, map[string]any{"input": input}, &result, team.Name, true); err != nil {
		return Issue{}, err
	}
	if !result.IssueCreate.Success {
		return Issue{}, l.createError(LinearRejected)
	}
	v := result.IssueCreate.Issue
	if v == nil || v.Identifier == "" || v.URL == "" {
		return Issue{}, l.createError(LinearUnconfirmed)
	}
	return Issue{Identifier: v.Identifier, URL: v.URL, Title: v.Title, State: v.State.Name, Team: team.Name, Labels: labels}, nil
}
