package hub

import (
	"encoding/json"
	"net/http"
	"strings"
)

type workspaceSecretUpsertRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (s *Server) handleWorkspaceSecretsCRUD(w http.ResponseWriter, r *http.Request) {
	workspace := strings.TrimSpace(r.PathValue("workspace"))
	if workspace == "" {
		http.Error(w, "workspace required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		names, err := workspaceSecretNames(workspace)
		if err != nil {
			http.Error(w, "list secrets: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string][]string{"secrets": names})
	case http.MethodPut, http.MethodPost:
		var req workspaceSecretUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if err := saveWorkspaceSecret(workspace, req.Name, req.Value); err != nil {
			http.Error(w, "save secret: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"upserted": req.Name})
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if err := deleteWorkspaceSecret(workspace, name); err != nil {
			http.Error(w, "delete secret: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"deleted": name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type workspaceGitHubAppUpsertRequest struct {
	Name          string `json:"name"`
	AppID         int64  `json:"appId"`
	URL           string `json:"url,omitempty"`
	Installation  string `json:"installation,omitempty"`
	PrivateKeyPEM string `json:"privateKeyPem,omitempty"`
}

func (s *Server) handleWorkspaceGitHubAppsCRUD(w http.ResponseWriter, r *http.Request) {
	workspace := strings.TrimSpace(r.PathValue("workspace"))
	if workspace == "" {
		http.Error(w, "workspace required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		apps, err := workspaceGitHubAppViews(workspace)
		if err != nil {
			http.Error(w, "list github apps: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string][]workspaceGitHubAppView{"githubApps": apps})
	case http.MethodPut, http.MethodPost:
		var req workspaceGitHubAppUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if req.AppID == 0 {
			http.Error(w, "appId required", http.StatusBadRequest)
			return
		}
		app := workspaceGitHubApp{
			AppID:        req.AppID,
			URL:          req.URL,
			Installation: req.Installation,
		}
		if req.PrivateKeyPEM != "" {
			app.PrivateKeyPEM = req.PrivateKeyPEM
		} else if existing, err := loadWorkspaceGitHubApps(workspace); err == nil {
			if existingApp, ok := existing[req.Name]; ok {
				app.PrivateKeyPEM = existingApp.PrivateKeyPEM
			}
		}
		if err := saveWorkspaceGitHubApp(workspace, req.Name, app); err != nil {
			http.Error(w, "save github app: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"upserted": req.Name})
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		if err := deleteWorkspaceGitHubApp(workspace, name); err != nil {
			http.Error(w, "delete github app: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"deleted": name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
