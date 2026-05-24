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
	IssueStates  map[string]string // issueID → state ID (from mutations)
	GraphQLCalls []string
	// PollingIssues holds issue data returned by the issues(filter:) polling query.
	// Key is identifier (e.g. "ELA-123"). Call SetIssueStateName to mutate state.
	PollingIssues map[string]map[string]interface{}
}

func NewMockLinear(t *testing.T) *MockLinear {
	t.Helper()
	m := &MockLinear{
		IssueStates:   make(map[string]string),
		PollingIssues: make(map[string]map[string]interface{}),
	}
	// Default polling issue
	m.PollingIssues["ELA-123"] = map[string]interface{}{
		"id":          "issue-uuid-123",
		"identifier":  "ELA-123",
		"title":       "Add hello world to README",
		"description": "Please add a 'Hello World' section to the README.md file.",
		"url":         "https://linear.app/test/issue/ELA-123",
		"updatedAt":   "2026-05-10T00:00:00Z",
		"state":       map[string]interface{}{"name": "Backlog"},
		"team":        map[string]interface{}{"name": "Engineering", "key": "ELA"},
		"labels":      map[string]interface{}{"nodes": []map[string]interface{}{}},
		"assignee":    nil,
	}
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
		// Polling query: issues(filter: { updatedAt: { gt: ... } })
		if strings.Contains(bodyStr, `issues(filter:`) {
			m.mu.Lock()
			var nodes []map[string]interface{}
			for _, issue := range m.PollingIssues {
				nodes = append(nodes, cloneMap(issue))
			}
			m.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"issues": map[string]interface{}{
						"nodes": nodes,
					},
				},
			})
			return
		}
		// issue(id:) query — returns a single issue directly
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

func cloneMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		switch typed := v.(type) {
		case map[string]interface{}:
			out[k] = cloneMap(typed)
		case []interface{}:
			out[k] = append([]interface{}(nil), typed...)
		default:
			out[k] = v
		}
	}
	return out
}

// SetIssueStateName updates the state name of a polling issue.
func (m *MockLinear) SetIssueStateName(identifier, stateName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if issue, ok := m.PollingIssues[identifier]; ok {
		issue["state"] = map[string]interface{}{"name": stateName}
		issue["updatedAt"] = "2026-05-10T00:01:00Z"
	}
}

// SawPollCall returns true if the mock has received an issues(filter:) GraphQL query
// (the polling endpoint) since the last reset.
func (m *MockLinear) SawPollCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.GraphQLCalls {
		if strings.Contains(c, `issues(filter:`) {
			return true
		}
	}
	return false
}

// ResetCalls clears the GraphQL call log.
func (m *MockLinear) ResetCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.GraphQLCalls = nil
}

// SawAPICall returns true if the mock has recorded at least one HTTP request.
// Used as a smoke check that the test server's integration logic actually
// hit the mock (not just the webhook endpoint on the test server itself).
func (m *MockLinear) SawAPICall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.GraphQLCalls) > 0
}

// BuildWebhookPayload returns a JSON webhook payload and the HMAC-SHA256
// signature for the given issue state transition.
func (m *MockLinear) BuildWebhookPayload(issueID, prevStatus, newStatus string) ([]byte, string) {
	payload := map[string]interface{}{
		"type":   "Issue",
		"action": "update",
		"data": map[string]interface{}{
			"id":          "issue-uuid-123",
			"identifier":  issueID,
			"title":       "Add hello world to README",
			"description": "Add a Hello World section to README.md",
			"url":         "https://linear.app/test/issue/" + issueID,
			"team": map[string]interface{}{
				"key":  "ELA",
				"name": "Engineering",
			},
			"state": map[string]interface{}{
				"name": newStatus,
			},
		},
		"updatedFrom": map[string]interface{}{
			"state": map[string]interface{}{
				"name": prevStatus,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b, ""
}
