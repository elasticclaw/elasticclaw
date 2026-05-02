// linear.go — minimal Linear GraphQL CLI embedded in claw-bridge
//
// Reads LINEAR_API_KEY from env. Provides a small set of commands the claw
// can invoke via exec (the bridge binary is already present in the sandbox).
//
// Usage: claw-bridge linear <command> [args...]
//
// Commands:
//   issue get <identifier>           — fetch issue details (e.g. CAN-61)
//   issue update <identifier> --state=<name> --comment=<text>
//   issue search <query>             — search issues
//   teams                            — list teams
//
// This is intentionally minimal. The hub already injects full issue context
// into CONTEXT.md; these commands let the claw update status, add comments,
// or search for related issues without needing a separate Go binary.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var linearAPIURL = "https://api.linear.app/graphql"

func runLinearCLI(args []string) int {
	apiKey := os.Getenv("LINEAR_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "LINEAR_API_KEY not set")
		return 1
	}

	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: claw-bridge linear <command> [args...]")
		fmt.Fprintln(os.Stderr, "commands: issue get, issue update, issue search, teams")
		return 1
	}

	switch args[0] {
	case "issue":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: claw-bridge linear issue <get|update|search> ...")
			return 1
		}
		switch args[1] {
		case "get":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: claw-bridge linear issue get <identifier>")
				return 1
			}
			return linearIssueGet(apiKey, args[2])
		case "update":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: claw-bridge linear issue update <identifier> [--state=<name>] [--comment=<text>]")
				return 1
			}
			return linearIssueUpdate(apiKey, args[2], args[3:])
		case "search":
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: claw-bridge linear issue search <query>")
				return 1
			}
			// Join all remaining args so multi-word queries work without quoting.
			queryStr := strings.Join(args[2:], " ")
			return linearIssueSearch(apiKey, queryStr)
		default:
			fmt.Fprintf(os.Stderr, "unknown issue subcommand: %s\n", args[1])
			return 1
		}
	case "teams":
		return linearTeams(apiKey)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		return 1
	}
}

// linearClient is a shared HTTP client with a reasonable timeout.
var linearClient = &http.Client{Timeout: 15 * time.Second}

// linearQuery performs a GraphQL query/mutation against Linear's API.
func linearQuery(apiKey, query string, variables map[string]interface{}) (map[string]interface{}, error) {
	body := map[string]interface{}{"query": query}
	if variables != nil {
		body["variables"] = variables
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", linearAPIURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := linearClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w (body: %s)", err, string(data))
	}

	if errs, ok := result["errors"].([]interface{}); ok && len(errs) > 0 {
		return nil, fmt.Errorf("graphql error: %v", errs[0])
	}

	return result, nil
}

// linearIssueGet fetches an issue by identifier (e.g. "CAN-61").
func linearIssueGet(apiKey, identifier string) int {
	query := `
		query($id: String!) {
			issue(id: $id) {
				id
				identifier
				title
				description
				state { name }
				priority
				url
				team { name key }
				assignee { name }
				createdAt
				updatedAt
			}
		}
	`
	result, err := linearQuery(apiKey, query, map[string]interface{}{"id": identifier})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	data := dig(result, "data", "issue")
	if data == nil {
		fmt.Fprintf(os.Stderr, "issue %s not found\n", identifier)
		return 1
	}

	b, _ := json.MarshalIndent(data, "", "  ")
	fmt.Println(string(b))
	return 0
}

// linearIssueUpdate updates an issue state and/or adds a comment.
func linearIssueUpdate(apiKey, identifier string, flags []string) int {
	var stateName, comment string
	for _, f := range flags {
		if strings.HasPrefix(f, "--state=") {
			stateName = strings.TrimPrefix(f, "--state=")
		} else if strings.HasPrefix(f, "--comment=") {
			comment = strings.TrimPrefix(f, "--comment=")
		}
	}

	if stateName == "" && comment == "" {
		fmt.Fprintln(os.Stderr, "nothing to do: specify --state=<name> and/or --comment=<text>")
		return 1
	}

	// Resolve issue ID and team ID from identifier
	query := `query($id: String!) {
		issue(id: $id) {
			id
			team { id name }
		}
	}`
	result, err := linearQuery(apiKey, query, map[string]interface{}{"id": identifier})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error resolving issue: %v\n", err)
		return 1
	}

	data := dig(result, "data", "issue")
	if data == nil {
		fmt.Fprintf(os.Stderr, "issue %s not found\n", identifier)
		return 1
	}
	issue0, _ := data.(map[string]interface{})
	issueID := issue0["id"]
	id, _ := issueID.(string)
	if id == "" {
		fmt.Fprintf(os.Stderr, "issue %s has no id\n", identifier)
		return 1
	}

	// Extract team ID for state scoping
	var teamID string
	if teamData, ok := issue0["team"].(map[string]interface{}); ok {
		teamID, _ = teamData["id"].(string)
	}

	// Update state if requested
	if stateName != "" {
		// Find state ID by name, scoped to the issue's team
		var statesQuery string
		var statesVars map[string]interface{}
		if teamID != "" {
			statesQuery = `query($name: String!, $teamId: String!) {
				workflowStates(filter: { name: { eq: $name }, team: { id: { eq: $teamId } } }) {
					nodes { id name team { name } }
				}
			}`
			statesVars = map[string]interface{}{"name": stateName, "teamId": teamID}
		} else {
			statesQuery = `query($name: String!) {
				workflowStates(filter: { name: { eq: $name } }) {
					nodes { id name team { name } }
				}
			}`
			statesVars = map[string]interface{}{"name": stateName}
		}
		statesResult, err := linearQuery(apiKey, statesQuery, statesVars)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error finding state '%s': %v\n", stateName, err)
			return 1
		}
		states := dig(statesResult, "data", "workflowStates", "nodes")
		statesList, _ := states.([]interface{})
		if len(statesList) == 0 {
			if teamID != "" {
				fmt.Fprintf(os.Stderr, "state '%s' not found for this issue's team\n", stateName)
			} else {
				fmt.Fprintf(os.Stderr, "state '%s' not found\n", stateName)
			}
			return 1
		}
		state0, _ := statesList[0].(map[string]interface{})
		stateID := state0["id"]
		sid, _ := stateID.(string)

		mut := `mutation($id: String!, $stateId: String!) {
			issueUpdate(id: $id, input: { stateId: $stateId }) {
				success
				issue { id identifier state { name } }
			}
		}`
		updateResult, err := linearQuery(apiKey, mut, map[string]interface{}{"id": id, "stateId": sid})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error updating state: %v\n", err)
			return 1
		}
		if payload, _ := dig(updateResult, "data", "issueUpdate").(map[string]interface{}); payload != nil {
			if ok, _ := payload["success"].(bool); !ok {
				fmt.Fprintf(os.Stderr, "issueUpdate returned success=false for %s\n", identifier)
				return 1
			}
		}
		fmt.Printf("Updated %s state → %s\n", identifier, stateName)
	}

	// Add comment if requested
	if comment != "" {
		mut := `mutation($issueId: String!, $body: String!) {
			commentCreate(input: { issueId: $issueId, body: $body }) {
				success
				comment { id body }
			}
		}`
		commentResult, err := linearQuery(apiKey, mut, map[string]interface{}{"issueId": id, "body": comment})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error adding comment: %v\n", err)
			return 1
		}
		if payload, _ := dig(commentResult, "data", "commentCreate").(map[string]interface{}); payload != nil {
			if ok, _ := payload["success"].(bool); !ok {
				fmt.Fprintf(os.Stderr, "commentCreate returned success=false for %s\n", identifier)
				return 1
			}
		}
		fmt.Printf("Added comment to %s\n", identifier)
	}

	return 0
}

// linearIssueSearch searches for issues by query string.
func linearIssueSearch(apiKey, queryStr string) int {
	query := `
		query($query: String!) {
			issueSearch(query: $query) {
				nodes {
					id
					identifier
					title
					state { name }
					team { name key }
					assignee { name }
					url
				}
			}
		}
	`
	result, err := linearQuery(apiKey, query, map[string]interface{}{"query": queryStr})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	data := dig(result, "data", "issueSearch", "nodes")
	issues, ok := data.([]interface{})
	if !ok {
		fmt.Fprintln(os.Stderr, "no results")
		return 0
	}

	for _, i := range issues {
		issue, _ := i.(map[string]interface{})
		id, _ := issue["identifier"].(string)
		title, _ := issue["title"].(string)
		state := ""
		if s, ok := issue["state"].(map[string]interface{}); ok {
			state, _ = s["name"].(string)
		}
		fmt.Printf("%s [%s] %s\n", id, state, title)
	}
	return 0
}

// linearTeams lists all teams.
func linearTeams(apiKey string) int {
	query := `
		query {
			teams {
				nodes {
					id
					name
					key
				}
			}
		}
	`
	result, err := linearQuery(apiKey, query, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	data := dig(result, "data", "teams", "nodes")
	teams, ok := data.([]interface{})
	if !ok {
		fmt.Fprintln(os.Stderr, "no teams")
		return 0
	}

	for _, t := range teams {
		team, _ := t.(map[string]interface{})
		key, _ := team["key"].(string)
		name, _ := team["name"].(string)
		fmt.Printf("%s — %s\n", key, name)
	}
	return 0
}

// dig walks a nested map[string]interface{} path. Returns nil if any step fails.
func dig(m map[string]interface{}, keys ...string) interface{} {
	var v interface{} = m
	for _, k := range keys {
		next, ok := v.(map[string]interface{})
		if !ok {
			return nil
		}
		v, ok = next[k]
		if !ok {
			return nil
		}
	}
	return v
}
