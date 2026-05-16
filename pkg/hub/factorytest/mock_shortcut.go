package factorytest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type StoryState struct {
	Name            string
	WorkflowStateID int64
	Description     string
	Labels          []string
	OwnerIDs        []string
	UpdatedAt       string
}

type MockShortcut struct {
	*httptest.Server
	mu            sync.Mutex
	Stories       map[int64]StoryState // key: story ID
	Workflows     []map[string]interface{}
	Calls         []string
	WebhookSecret string
}

func NewMockShortcut(t *testing.T) *MockShortcut {
	t.Helper()
	m := &MockShortcut{
		Stories:   make(map[int64]StoryState),
		Workflows: defaultShortcutWorkflows(),
	}
	mux := http.NewServeMux()

	// /api/v3/stories/:id — GET (fetch) or PUT (update state)
	mux.HandleFunc("/api/v3/stories/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v3/stories/")
		path = strings.TrimPrefix(path, "sc-")
		id, err := strconv.ParseInt(path, 10, 64)
		if err != nil {
			http.Error(w, "invalid story id", http.StatusBadRequest)
			return
		}

		m.mu.Lock()
		m.Calls = append(m.Calls, r.Method+" "+r.URL.Path)

		switch r.Method {
		case http.MethodGet:
			story, ok := m.Stories[id]
			m.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":                id,
				"name":              story.Name,
				"description":       story.Description,
				"app_url":           fmt.Sprintf("https://app.shortcut.com/test/story/%d", id),
				"updated_at":        story.UpdatedAt,
				"workflow_state_id": story.WorkflowStateID,
				"labels":            labelsToMaps(story.Labels),
				"owner_ids":         story.OwnerIDs,
			})

		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			var update map[string]interface{}
			json.Unmarshal(body, &update)
			story, ok := m.Stories[id]
			if ok {
				if wfID, ok := update["workflow_state_id"].(float64); ok {
					story.WorkflowStateID = int64(wfID)
					story.Name = m.stateNameForID(story.WorkflowStateID)
					story.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				}
				m.Stories[id] = story
			}
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": id})

		default:
			m.mu.Unlock()
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// POST /api/v3/stories/search
	mux.HandleFunc("/api/v3/stories/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)

		m.mu.Lock()
		m.Calls = append(m.Calls, r.Method+" "+r.URL.Path+" "+string(body))

		// Parse updated_at_start from body to filter
		var reqBody map[string]interface{}
		json.Unmarshal(body, &reqBody)
		since, _ := reqBody["updated_at_start"].(string)

		var results []map[string]interface{}
		for id, story := range m.Stories {
			// Respect updated_at_start filter: if since is set, only include stories
			// whose updated_at is >= since. Stories store updated_at as a string.
			storyUpdatedAt := story.UpdatedAt
			if storyUpdatedAt == "" {
				storyUpdatedAt = time.Now().UTC().Format(time.RFC3339)
			}
			if since != "" && storyUpdatedAt < since {
				continue
			}
			results = append(results, map[string]interface{}{
				"id":                id,
				"name":              story.Name,
				"description":       story.Description,
				"app_url":           fmt.Sprintf("https://app.shortcut.com/test/story/%d", id),
				"updated_at":        storyUpdatedAt,
				"workflow_state_id": story.WorkflowStateID,
				"labels":            labelsToMaps(story.Labels),
				"owner_ids":         story.OwnerIDs,
			})
		}
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// Shortcut returns raw array, not {data: [...]} wrapper
		json.NewEncoder(w).Encode(results)
	})

	// GET /api/v3/workflows
	mux.HandleFunc("/api/v3/workflows", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		m.Calls = append(m.Calls, r.Method+" "+r.URL.Path)
		workflows := m.Workflows
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(workflows)
	})

	// GET /api/v3/members/:id
	mux.HandleFunc("/api/v3/members/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		m.mu.Lock()
		m.Calls = append(m.Calls, r.Method+" "+r.URL.Path)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":           "member-123",
			"name":         "Test User",
			"mention_name": "testuser",
		})
	})

	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Server.Close)
	return m
}

func (m *MockShortcut) SetStory(id int64, state StoryState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Stories[id] = state
}

func (m *MockShortcut) SetStoryState(id int64, workflowStateID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if story, ok := m.Stories[id]; ok {
		story.WorkflowStateID = workflowStateID
		story.Name = m.stateNameForID(workflowStateID)
		story.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		m.Stories[id] = story
	}
}

func (m *MockShortcut) stateNameForID(id int64) string {
	for _, wf := range m.Workflows {
		var stateList []map[string]interface{}
		switch v := wf["states"].(type) {
		case []map[string]interface{}:
			stateList = v
		case []interface{}:
			for _, st := range v {
				if s, ok := st.(map[string]interface{}); ok {
					stateList = append(stateList, s)
				}
			}
		}
		for _, state := range stateList {
			var sid int64
			switch v := state["id"].(type) {
			case float64:
				sid = int64(v)
			case int:
				sid = int64(v)
			case int64:
				sid = v
			}
			if sid == id {
				name, _ := state["name"].(string)
				return name
			}
		}
	}
	return ""
}

// SawPollCall returns true if the mock has received a stories/search call
// (the polling endpoint) since the last reset.
func (m *MockShortcut) SawPollCall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.Calls {
		if strings.Contains(c, "stories/search") {
			return true
		}
	}
	return false
}

// SawAPICall returns true if the mock has recorded at least one HTTP request.
// Used as a smoke check that the test server's integration logic actually
// hit the mock (not just the webhook endpoint on the test server itself).
func (m *MockShortcut) SawAPICall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Calls) > 0
}

// StateIDForName looks up a workflow state ID by name from the mock workflows.
func (m *MockShortcut) StateIDForName(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, wf := range m.Workflows {
		var stateList []map[string]interface{}
		switch v := wf["states"].(type) {
		case []map[string]interface{}:
			stateList = v
		case []interface{}:
			for _, st := range v {
				if s, ok := st.(map[string]interface{}); ok {
					stateList = append(stateList, s)
				}
			}
		default:
			// unknown states type — ignore
		}
		for _, state := range stateList {
			stateName, _ := state["name"].(string)
			if strings.EqualFold(stateName, name) {
				var id int64
				switch v := state["id"].(type) {
				case float64:
					id = int64(v)
				case int:
					id = int64(v)
				case int64:
					id = v
				}
				return id
			}
		}
	}
	return 0
}

func (m *MockShortcut) BuildWebhookPayload(storyID int64, prevStateID, newStateID int64, secret string) ([]byte, string) {
	payload := map[string]interface{}{
		"id":         fmt.Sprintf("webhook-%d", storyID),
		"changed_at": "2026-05-10T00:01:00Z",
		"actions": []map[string]interface{}{
			{
				"id":          storyID,
				"entity_type": "story",
				"action":      "update",
				"name":        "Test Story",
				"app_url":     fmt.Sprintf("https://app.shortcut.com/test/story/%d", storyID),
				"description": "Test description",
				"changes": map[string]interface{}{
					"workflow_state_id": map[string]interface{}{
						"new": newStateID,
						"old": prevStateID,
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	// Sign with HMAC-SHA256 if secret provided
	var sig string
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		sig = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return body, sig
}

func defaultShortcutWorkflows() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"id":   1001,
			"name": "Engineering",
			"states": []map[string]interface{}{
				{"id": 5001, "name": "Backlog", "type": "unstarted"},
				{"id": 5002, "name": "In Progress", "type": "started"},
				{"id": 5003, "name": "Done", "type": "done"},
			},
		},
	}
}

func labelsToMaps(labels []string) []map[string]interface{} {
	var result []map[string]interface{}
	for _, l := range labels {
		result = append(result, map[string]interface{}{"name": l})
	}
	return result
}
