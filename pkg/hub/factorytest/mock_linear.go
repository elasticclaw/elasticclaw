package factorytest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type MockLinear struct {
	*httptest.Server
	mu           sync.Mutex
	IssueStates  map[string]string // issueID → state name
	GraphQLCalls []string
}

func NewMockLinear(t *testing.T) *MockLinear {
	t.Helper()
	m := &MockLinear{IssueStates: make(map[string]string)}
	mux := http.NewServeMux()
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)
		m.mu.Lock()
		m.GraphQLCalls = append(m.GraphQLCalls, bodyStr)
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// issueUpdate mutation (moveIssue)
		if strings.Contains(bodyStr, "issueUpdate") {
			// Extract issueId and stateId from variables - best effort
			var req struct {
				Variables map[string]interface{} `json:"variables"`
			}
			json.Unmarshal(body, &req)
			if id, ok := req.Variables["id"].(string); ok {
				if input, ok := req.Variables["input"].(map[string]interface{}); ok {
					if stateID, ok := input["stateId"].(string); ok {
						m.mu.Lock()
						m.IssueStates[id] = stateID
						m.mu.Unlock()
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"issueUpdate": map[string]interface{}{"success": true},
				},
			})
			return
		}
		// workflowStates query
		if strings.Contains(bodyStr, "workflowStates") {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"workflowStates": map[string]interface{}{
						"nodes": []map[string]interface{}{
							{"id": "done-state-id", "name": "Done", "type": "completed"},
							{"id": "in-progress-id", "name": "In Progress", "type": "started"},
						},
					},
				},
			})
			return
		}
		// issue query
		if strings.Contains(bodyStr, "issue(") || strings.Contains(bodyStr, `"issue"`) {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"issue": map[string]interface{}{
						"id":          "issue-uuid-123",
						"identifier":  "ELA-123",
						"title":       "Add hello world to README",
						"description": "Please add a 'Hello World' section to the README.md file.",
						"url":         "https://linear.app/test/issue/ELA-123",
						"state":       map[string]interface{}{"name": "In Progress", "id": "in-progress-id"},
						"team":        map[string]interface{}{"name": "Engineering", "key": "ELA"},
					},
				},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{}})
	})
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Server.Close)
	return m
}
