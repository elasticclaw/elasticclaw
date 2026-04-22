package factorytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type PRState struct {
	State  string
	Merged bool
}

type MockGitHub struct {
	*httptest.Server
	mu    sync.Mutex
	PRs   map[string]PRState // key: "owner/repo#number"
	Calls []string
}

func NewMockGitHub(t *testing.T) *MockGitHub {
	t.Helper()
	m := &MockGitHub{PRs: make(map[string]PRState)}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.Calls = append(m.Calls, r.Method+" "+r.URL.Path)
		m.mu.Unlock()
		// GET /repos/:owner/:repo/pulls/:number
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
		if len(parts) == 4 && parts[2] == "pulls" && r.Method == http.MethodGet {
			key := fmt.Sprintf("%s/%s#%s", parts[0], parts[1], parts[3])
			m.mu.Lock()
			state, ok := m.PRs[key]
			m.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"state":    state.State,
				"merged":   state.Merged,
				"number":   parts[3],
				"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%s", parts[0], parts[1], parts[3]),
			})
			return
		}
		// GET /repos/:owner/:repo/issues/:number/comments
		if len(parts) == 4 && parts[2] == "issues" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		// POST /repos/:owner/:repo/statuses/:sha
		if len(parts) == 4 && parts[2] == "statuses" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	})
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Server.Close)
	return m
}

func (m *MockGitHub) SetPR(owner, repo string, number int, state PRState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PRs[fmt.Sprintf("%s/%s#%d", owner, repo, number)] = state
}
