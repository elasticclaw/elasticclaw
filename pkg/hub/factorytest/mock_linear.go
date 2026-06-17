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
	mu              sync.Mutex
	IssueStates     map[string]string // issueID → state ID (from mutations)
	IssueStateNames map[string]string // identifier → state name
	IssueAssignees  map[string]string // identifier → assignee ID
	IssueComments   map[string][]string
	GraphQLCalls    []string
	// PollingIssues holds issue data returned by the issues(filter:) polling query.
	// Key is identifier (e.g. "ELA-123"). Call SetIssueStateName to mutate state.
	PollingIssues map[string]map[string]interface{}
}

func NewMockLinear(t *testing.T) *MockLinear {
	t.Helper()
	m := &MockLinear{
		IssueStates:     make(map[string]string),
		IssueStateNames: make(map[string]string),
		IssueAssignees:  make(map[string]string),
		IssueComments:   make(map[string][]string),
		PollingIssues:   make(map[string]map[string]interface{}),
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
		// commentCreate mutation
		if strings.Contains(bodyStr, "commentCreate") {
			var req struct {
				Variables map[string]interface{} `json:"variables"`
			}
			_ = json.Unmarshal(body, &req)
			comment, _ := req.Variables["body"].(string)
			issueID, _ := req.Variables["issueId"].(string)
			m.mu.Lock()
			identifier := m.issueIdentifierLocked(issueID)
			if identifier == "" {
				identifier = issueID
			}
			m.IssueComments[identifier] = append(m.IssueComments[identifier], comment)
			m.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"commentCreate": map[string]interface{}{
						"success": true,
						"comment": map[string]interface{}{"id": "comment-1"},
					},
				},
			})
			return
		}
		// issueUpdate mutation (moveIssue)
		if strings.Contains(bodyStr, "issueUpdate") {
			var req struct {
				Variables map[string]interface{} `json:"variables"`
			}
			json.Unmarshal(body, &req)
			if id, ok := req.Variables["id"].(string); ok {
				m.mu.Lock()
				identifier := m.issueIdentifierLocked(id)
				if identifier == "" {
					identifier = id
				}
				m.mu.Unlock()
				if stateID, ok := req.Variables["stateId"].(string); ok {
					m.mu.Lock()
					m.IssueStates[id] = stateID
					m.IssueStateNames[identifier] = linearStateNameForID(stateID)
					if issue, ok := m.PollingIssues[identifier]; ok {
						issue["state"] = map[string]interface{}{"name": linearStateNameForID(stateID)}
					}
					m.mu.Unlock()
				}
				if assigneeID, ok := req.Variables["assigneeId"].(string); ok {
					m.mu.Lock()
					m.IssueAssignees[identifier] = assigneeID
					m.mu.Unlock()
				}
				if input, ok := req.Variables["input"].(map[string]interface{}); ok {
					if stateID, ok := input["stateId"].(string); ok {
						m.mu.Lock()
						m.IssueStates[id] = stateID
						m.IssueStateNames[identifier] = linearStateNameForID(stateID)
						if issue, ok := m.PollingIssues[identifier]; ok {
							issue["state"] = map[string]interface{}{"name": linearStateNameForID(stateID)}
						}
						m.mu.Unlock()
					}
					if assigneeID, ok := input["assigneeId"].(string); ok {
						m.mu.Lock()
						m.IssueAssignees[identifier] = assigneeID
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
							{"id": "agent-error-id", "name": "Agent Error", "type": "canceled"},
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
			var req struct {
				Variables map[string]interface{} `json:"variables"`
			}
			_ = json.Unmarshal(body, &req)
			id, _ := req.Variables["id"].(string)
			m.mu.Lock()
			issue := cloneMap(m.issueByIDLocked(id))
			m.mu.Unlock()
			if issue == nil {
				issue = map[string]interface{}{
					"id":          "issue-uuid-123",
					"identifier":  "ELA-123",
					"title":       "Add hello world to README",
					"description": "Please add a 'Hello World' section to the README.md file.",
					"url":         "https://linear.app/test/issue/ELA-123",
					"state":       map[string]interface{}{"name": "In Progress", "id": "in-progress-id"},
					"team": map[string]interface{}{
						"name": "Engineering",
						"key":  "ELA",
						"states": map[string]interface{}{
							"nodes": []map[string]interface{}{
								{"id": "done-state-id", "name": "Done", "type": "completed"},
								{"id": "in-progress-id", "name": "In Progress", "type": "started"},
								{"id": "agent-error-id", "name": "Agent Error", "type": "canceled"},
							},
						},
					},
				}
			}
			ensureMockLinearIssueStates(issue)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"issue": issue,
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
	if in == nil {
		return nil
	}
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

func (m *MockLinear) issueByIDLocked(id string) map[string]interface{} {
	if issue, ok := m.PollingIssues[id]; ok {
		return issue
	}
	for _, issue := range m.PollingIssues {
		if issueID, _ := issue["id"].(string); issueID == id {
			return issue
		}
	}
	return nil
}

func (m *MockLinear) issueIdentifierLocked(id string) string {
	if _, ok := m.PollingIssues[id]; ok {
		return id
	}
	for identifier, issue := range m.PollingIssues {
		if issueID, _ := issue["id"].(string); issueID == id {
			return identifier
		}
	}
	return ""
}

func ensureMockLinearIssueStates(issue map[string]interface{}) {
	team, _ := issue["team"].(map[string]interface{})
	if team == nil {
		team = map[string]interface{}{"name": "Engineering", "key": "ELA"}
		issue["team"] = team
	}
	if _, ok := team["states"]; ok {
		return
	}
	team["states"] = map[string]interface{}{
		"nodes": []map[string]interface{}{
			{"id": "done-state-id", "name": "Done", "type": "completed"},
			{"id": "in-progress-id", "name": "In Progress", "type": "started"},
			{"id": "agent-error-id", "name": "Agent Error", "type": "canceled"},
		},
	}
}

// SetIssueStateName updates the state name of a polling issue.
func (m *MockLinear) SetIssueStateName(identifier, stateName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if issue, ok := m.PollingIssues[identifier]; ok {
		issue["state"] = map[string]interface{}{"name": stateName}
		issue["updatedAt"] = "2026-05-10T00:01:00Z"
	}
	m.IssueStateNames[identifier] = stateName
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

func (m *MockLinear) Comments(issueID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.IssueComments[issueID]...)
}

func (m *MockLinear) IssueStateName(issueID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.IssueStateNames[issueID]; state != "" {
		return state
	}
	if issue, ok := m.PollingIssues[issueID]; ok {
		if state, ok := issue["state"].(map[string]interface{}); ok {
			if name, ok := state["name"].(string); ok {
				return name
			}
		}
	}
	return ""
}

func (m *MockLinear) IssueAssigneeID(issueID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.IssueAssignees[issueID]
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

func linearStateNameForID(id string) string {
	switch id {
	case "done-state-id":
		return "Done"
	case "in-progress-id":
		return "In Progress"
	case "agent-error-id":
		return "Agent Error"
	default:
		return id
	}
}
