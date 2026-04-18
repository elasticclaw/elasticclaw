package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockHub is a minimal HTTP server that stands in for a real ElasticClaw hub
// during container integration tests. It handles the endpoints the bootstrap
// script and claw-bridge call.
type mockHub struct {
	server *httptest.Server
}

func newMockHub(t *testing.T) *mockHub {
	t.Helper()
	mux := http.NewServeMux()

	// claw-bridge registers here on connect
	mux.HandleFunc("/ws/claw", func(w http.ResponseWriter, r *http.Request) {
		// Accept WebSocket upgrade — for unit tests just return 200
		w.WriteHeader(http.StatusSwitchingProtocols)
	})

	// Credential helper calls this to get a GitHub token
	mux.HandleFunc("/api/github/token/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"token": "test-github-token"})
	})

	// Health / status
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// Claw list (bridge may call this)
	mux.HandleFunc("/api/claws", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{})
	})

	// Catch-all: log and return 200 so the script doesn't fail
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("[mockHub] %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return &mockHub{server: s}
}

func (m *mockHub) URL() string {
	return m.server.URL
}

func (m *mockHub) Close() {
	m.server.Close()
}
