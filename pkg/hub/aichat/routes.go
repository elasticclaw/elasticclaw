package aichat

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
)

// Deps is the hub boundary. Connectors and runner dependencies are added when
// they are needed; this package never imports its parent hub package.
type Deps struct {
	WebAuth     func(http.HandlerFunc) http.HandlerFunc
	WithFeature func(string, http.HandlerFunc) http.HandlerFunc
	CallerLogin func(*http.Request) string
	Workspaces  func() ([]string, error)
	ManagedDir  func(string) string
}

// OwnerLogin normalizes authenticated identities; password sessions share admin.
func OwnerLogin(login string) string {
	if login = strings.ToLower(strings.TrimSpace(login)); login != "" {
		return login
	}
	return "admin"
}

// Routes gates the entire namespace, including endpoints added in later PRs.
func Routes(deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ai-chat/sources", func(w http.ResponseWriter, r *http.Request) { sources(w, r, deps) })
	return deps.WebAuth(deps.WithFeature("ai-chat", mux.ServeHTTP))
}

type SourcesResponse struct {
	Configured bool            `json:"configured"`
	Workspaces []string        `json:"workspaces"`
	Workspace  string          `json:"workspace"`
	Sources    []config.Source `json:"sources"`
}

func sources(w http.ResponseWriter, r *http.Request, deps Deps) {
	names, err := deps.Workspaces()
	if err != nil {
		http.Error(w, "unable to list workspaces", http.StatusInternalServerError)
		return
	}
	sort.Strings(names)
	response := SourcesResponse{Workspaces: []string{}, Workspace: r.URL.Query().Get("workspace"), Sources: []config.Source{}}
	known := false
	for _, name := range names {
		if name == response.Workspace {
			known = true
		}
		info, err := os.Stat(filepath.Join(deps.ManagedDir(name), "ai_chat.yaml"))
		if err == nil && info.Mode().IsRegular() {
			response.Workspaces = append(response.Workspaces, name)
		}
	}
	if response.Workspace == "" && len(response.Workspaces) > 0 {
		response.Workspace = response.Workspaces[0]
		known = true
	}
	// Only names returned by the workspace registry may reach the path resolver.
	if known {
		if cfg, err := config.Load(deps.ManagedDir(response.Workspace)); err == nil {
			response.Configured = true
			response.Sources = cfg.Sources
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
