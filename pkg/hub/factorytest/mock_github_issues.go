package factorytest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// IssueState holds the mutable state of a GitHub issue for testing.
type IssueState struct {
	Title     string
	Body      string
	State     string
	Labels    []string
	Assignee  string
	UpdatedAt string // RFC3339
}

type IssueCommentState struct {
	ID        int64
	Body      string
	User      string
	CreatedAt string
	HTMLURL   string
}

// MockGitHubIssues is a REST-style mock for the GitHub Issues API and webhook
// delivery.
type MockGitHubIssues struct {
	*httptest.Server
	mu            sync.Mutex
	Issues        map[string]IssueState // key: "owner/repo#number"
	IssueEvents   map[string][]map[string]interface{}
	IssueComments map[string][]IssueCommentState
	Calls         []string
	AuthHeaders   []string
	WebhookSecret string
}

func NewMockGitHubIssues(t *testing.T) *MockGitHubIssues {
	t.Helper()
	m := &MockGitHubIssues{
		Issues:        make(map[string]IssueState),
		IssueEvents:   make(map[string][]map[string]interface{}),
		IssueComments: make(map[string][]IssueCommentState),
	}
	mux := http.NewServeMux()

	// /repos/:owner/:repo/issues endpoints.
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/")
		parts := strings.Split(path, "/")
		if len(parts) < 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		owner, repo := parts[0], parts[1]
		rest := strings.Join(parts[2:], "/")

		m.mu.Lock()
		m.Calls = append(m.Calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		m.AuthHeaders = append(m.AuthHeaders, r.Header.Get("Authorization"))
		m.mu.Unlock()

		// issues endpoint
		if strings.HasPrefix(rest, "issues") {
			parts2 := strings.Split(rest, "/")
			if r.Method == http.MethodPost && len(parts2) == 3 && parts2[0] == "issues" && parts2[2] == "comments" {
				var num int
				fmt.Sscanf(parts2[1], "%d", &num)
				var req struct {
					Body string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				key := fmt.Sprintf("%s/%s#%d", owner, repo, num)
				m.mu.Lock()
				id := int64(len(m.IssueComments[key]) + 1)
				m.IssueComments[key] = append(m.IssueComments[key], IssueCommentState{
					ID:        id,
					Body:      req.Body,
					User:      "elasticclaw-bot",
					CreatedAt: time.Now().UTC().Format(time.RFC3339),
				})
				m.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]interface{}{"id": id, "body": req.Body})
				return
			}
			if r.Method == http.MethodPost && len(parts2) == 3 && parts2[0] == "issues" && parts2[2] == "labels" {
				var num int
				fmt.Sscanf(parts2[1], "%d", &num)
				var req struct {
					Labels []string `json:"labels"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				key := fmt.Sprintf("%s/%s#%d", owner, repo, num)
				m.mu.Lock()
				issue := m.Issues[key]
				for _, label := range req.Labels {
					if !stringSliceContains(issue.Labels, label) {
						issue.Labels = append(issue.Labels, label)
					}
				}
				m.Issues[key] = issue
				m.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(labelsToNameMaps(req.Labels))
				return
			}
			if r.Method == http.MethodPatch && len(parts2) == 2 && parts2[0] == "issues" {
				var num int
				fmt.Sscanf(parts2[1], "%d", &num)
				var req struct {
					Assignees []string `json:"assignees"`
					State     string   `json:"state"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				key := fmt.Sprintf("%s/%s#%d", owner, repo, num)
				m.mu.Lock()
				issue := m.Issues[key]
				if len(req.Assignees) > 0 {
					issue.Assignee = req.Assignees[0]
				}
				if req.State != "" {
					issue.State = req.State
				}
				m.Issues[key] = issue
				m.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(issueToMap(num, issue, owner, repo))
				return
			}
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if len(parts2) == 3 && parts2[0] == "issues" && parts2[2] == "events" {
				var num int
				fmt.Sscanf(parts2[1], "%d", &num)
				key := fmt.Sprintf("%s/%s#%d", owner, repo, num)
				m.mu.Lock()
				events := append([]map[string]interface{}(nil), m.IssueEvents[key]...)
				m.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(events)
				return
			}
			if len(parts2) == 3 && parts2[0] == "issues" && parts2[2] == "comments" {
				var num int
				fmt.Sscanf(parts2[1], "%d", &num)
				key := fmt.Sprintf("%s/%s#%d", owner, repo, num)
				m.mu.Lock()
				comments := append([]IssueCommentState(nil), m.IssueComments[key]...)
				m.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(issueCommentsToMaps(comments, owner, repo, num))
				return
			}
			// Single issue fetch: /repos/{owner}/{repo}/issues/{number}
			if len(parts2) == 2 && parts2[0] == "issues" {
				var num int
				fmt.Sscanf(parts2[1], "%d", &num)
				key := fmt.Sprintf("%s/%s#%d", owner, repo, num)
				m.mu.Lock()
				issue, ok := m.Issues[key]
				m.mu.Unlock()
				if !ok {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(issueToMap(num, issue, owner, repo))
				return
			}
			// List issues: /repos/{owner}/{repo}/issues?since=...
			since := r.URL.Query().Get("since")
			var results []map[string]interface{}
			m.mu.Lock()
			for key, issue := range m.Issues {
				if !strings.HasPrefix(key, owner+"/"+repo+"#") {
					continue
				}
				numStr := strings.TrimPrefix(key, owner+"/"+repo+"#")
				var num int
				fmt.Sscanf(numStr, "%d", &num)
				if since != "" && issue.UpdatedAt < since {
					continue
				}
				results = append(results, issueToMap(num, issue, owner, repo))
			}
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(results)
			return
		}

		http.NotFound(w, r)
	})

	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Server.Close)
	return m
}

func (m *MockGitHubIssues) SetIssue(repo string, number int, state IssueState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state.UpdatedAt == "" {
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	m.Issues[fmt.Sprintf("%s#%d", repo, number)] = state
}

func (m *MockGitHubIssues) SetIssueEvents(repo string, number int, events []map[string]interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.IssueEvents[fmt.Sprintf("%s#%d", repo, number)] = append([]map[string]interface{}(nil), events...)
}

func (m *MockGitHubIssues) SetIssueComments(repo string, number int, comments []IssueCommentState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// repo must be in "owner/repo" format to match handler lookup keys.
	m.IssueComments[fmt.Sprintf("%s#%d", repo, number)] = append([]IssueCommentState(nil), comments...)
}

func (m *MockGitHubIssues) SetIssueState(repo string, number int, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%s#%d", repo, number)
	if issue, ok := m.Issues[key]; ok {
		issue.State = state
		issue.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		m.Issues[key] = issue
	}
}

// SawPollCall returns true if the mock has received an issues list GET call
// (the polling endpoint) since the last reset.
func (m *MockGitHubIssues) SawPollCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.Calls {
		// A list call looks like "GET /repos/owner/repo/issues?since=...".
		// A single-issue fetch looks like "GET /repos/owner/repo/issues/42".
		// We only want to match list calls: path ends with /issues (possibly
		// followed by ?query).  Match "GET .../issues?" or "GET .../issues "
		// (the latter shouldn't happen, but be defensive).
		if strings.Contains(c, "/issues?") || strings.HasSuffix(c, "/issues") {
			return true
		}
	}
	return false
}

// ResetCalls clears the call log.
func (m *MockGitHubIssues) ResetCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = nil
}

// SawAPICall returns true if the mock has recorded at least one HTTP request.
// Used as a smoke check that the test server's integration logic actually
// hit the mock (not just the webhook endpoint on the test server itself).
func (m *MockGitHubIssues) SawAPICall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls) > 0
}

func (m *MockGitHubIssues) Issue(repo string, number int) IssueState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Issues[fmt.Sprintf("%s#%d", repo, number)]
}

func (m *MockGitHubIssues) PostedComments(repo string, number int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	states := m.IssueComments[fmt.Sprintf("%s#%d", repo, number)]
	out := make([]string, 0, len(states))
	for _, state := range states {
		out = append(out, state.Body)
	}
	return out
}

func (m *MockGitHubIssues) AuthHeaderCount(header string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, got := range m.AuthHeaders {
		if got == header {
			count++
		}
	}
	return count
}

// BuildWebhookPayload returns a JSON webhook payload and the X-Hub-Signature-256
// for the given issue state transition.
//
// The action field is computed from the prev/new state pair. This mock
// intentionally only covers state transitions (opened/closed/reopened/edited);
// labeled/assigned/commented events are not yet needed by the parity matrix
// and would require additional payload fields.
func (m *MockGitHubIssues) BuildWebhookPayload(repo string, number int, prevState, newState string) ([]byte, string) {
	m.mu.Lock()
	issue, ok := m.Issues[fmt.Sprintf("%s#%d", repo, number)]
	m.mu.Unlock()
	if !ok {
		issue = IssueState{Title: "Test Issue", Body: "Test body", State: newState}
	}

	action := "edited"
	switch {
	case prevState == "" && newState == "open":
		action = "opened"
	case prevState == "open" && newState == "closed":
		action = "closed"
	case prevState == "closed" && newState == "open":
		action = "reopened"
	}

	payload := map[string]interface{}{
		"action": action,
		"issue": map[string]interface{}{
			"id":           number,
			"number":       number,
			"title":        issue.Title,
			"body":         issue.Body,
			"html_url":     fmt.Sprintf("https://github.com/%s/issues/%d", repo, number),
			"state":        newState,
			"state_reason": "",
			"labels":       labelsToNameMaps(issue.Labels),
			"assignee":     nil,
			"user":         map[string]interface{}{"login": "testuser", "type": "User"},
		},
		"repository": map[string]interface{}{
			"full_name": repo,
		},
		"sender": map[string]interface{}{
			"login": "testuser",
			"type":  "User",
		},
	}
	b, _ := json.Marshal(payload)

	var sig string
	if m.WebhookSecret != "" {
		mac := hmac.New(sha256.New, []byte(m.WebhookSecret))
		mac.Write(b)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return b, sig
}

func issueToMap(number int, issue IssueState, owner, repo string) map[string]interface{} {
	labels := make([]map[string]interface{}, len(issue.Labels))
	for i, l := range issue.Labels {
		labels[i] = map[string]interface{}{"name": l}
	}
	assignee := interface{}(nil)
	if issue.Assignee != "" {
		assignee = map[string]interface{}{"login": issue.Assignee}
	}
	return map[string]interface{}{
		"number":     number,
		"title":      issue.Title,
		"body":       issue.Body,
		"html_url":   fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, number),
		"state":      issue.State,
		"updated_at": issue.UpdatedAt,
		"labels":     labels,
		"assignee":   assignee,
		"user":       map[string]interface{}{"login": "testuser"},
	}
}

func issueCommentsToMaps(comments []IssueCommentState, owner, repo string, number int) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(comments))
	for idx, comment := range comments {
		id := comment.ID
		if id == 0 {
			id = int64(idx + 1)
		}
		user := comment.User
		if user == "" {
			user = "testuser"
		}
		createdAt := comment.CreatedAt
		if createdAt == "" {
			createdAt = time.Now().UTC().Format(time.RFC3339)
		}
		htmlURL := comment.HTMLURL
		if htmlURL == "" {
			htmlURL = fmt.Sprintf("https://github.com/%s/%s/issues/%d#issuecomment-%d", owner, repo, number, id)
		}
		out = append(out, map[string]interface{}{
			"id":         id,
			"body":       comment.Body,
			"html_url":   htmlURL,
			"created_at": createdAt,
			"user":       map[string]interface{}{"login": user, "type": "User"},
		})
	}
	return out
}

func labelsToNameMaps(labels []string) []map[string]interface{} {
	var result []map[string]interface{}
	for _, l := range labels {
		result = append(result, map[string]interface{}{"name": l})
	}
	return result
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
