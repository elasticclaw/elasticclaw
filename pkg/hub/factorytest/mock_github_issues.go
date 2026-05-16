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

// MockGitHubIssues is a REST-style mock for the GitHub Issues API and webhook
// delivery. It currently handles:
//   - GET /repos/{owner}/{repo}/issues?since=...&state=all&sort=updated&direction=desc
//   - GET /repos/{owner}/{repo}/issues/{number}
//
// POST endpoints for comments/labels are not yet implemented (needed for
// move_issue parity tests in a future phase).
type MockGitHubIssues struct {
	*httptest.Server
	mu      sync.Mutex
	Issues  map[string]IssueState // key: "owner/repo#number"
	Calls   []string
	WebhookSecret string
}

func NewMockGitHubIssues(t *testing.T) *MockGitHubIssues {
	t.Helper()
	m := &MockGitHubIssues{
		Issues: make(map[string]IssueState),
	}
	mux := http.NewServeMux()

	// GET /repos/:owner/:repo/issues — polling endpoint
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
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
		m.mu.Unlock()

		// issues endpoint
		if strings.HasPrefix(rest, "issues") {
			// Single issue fetch: /repos/{owner}/{repo}/issues/{number}
			parts2 := strings.Split(rest, "/")
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
			"id":         number,
			"number":     number,
			"title":      issue.Title,
			"body":       issue.Body,
			"html_url":   fmt.Sprintf("https://github.com/%s/issues/%d", repo, number),
			"state":      newState,
			"state_reason": "",
			"labels":     labelsToNameMaps(issue.Labels),
			"assignee":   nil,
			"user":       map[string]interface{}{"login": "testuser", "type": "User"},
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

func labelsToNameMaps(labels []string) []map[string]interface{} {
	var result []map[string]interface{}
	for _, l := range labels {
		result = append(result, map[string]interface{}{"name": l})
	}
	return result
}
