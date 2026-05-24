package hub

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
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
		enrichWorkspaceGitHubAppInstallations(r, apps, workspace)
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

func enrichWorkspaceGitHubAppInstallations(r *http.Request, views []workspaceGitHubAppView, workspace string) {
	apps, err := loadWorkspaceGitHubApps(workspace)
	if err != nil {
		return
	}
	for i := range views {
		app, ok := apps[views[i].Name]
		if !ok || app.PrivateKeyPEM == "" || app.AppID == 0 {
			continue
		}
		provider, err := NewGitHubTokenProvider(&types.GitHubAppConfig{
			AppID:         app.AppID,
			URL:           app.URL,
			PrivateKeyPEM: app.PrivateKeyPEM,
		})
		if err != nil {
			continue
		}
		installations, err := provider.ListInstallations(r.Context())
		if err != nil {
			continue
		}
		owners := make([]string, 0, len(installations))
		seen := map[string]bool{}
		for _, installation := range installations {
			owner := strings.TrimSpace(installation.Account.Login)
			if owner == "" || seen[strings.ToLower(owner)] {
				continue
			}
			seen[strings.ToLower(owner)] = true
			owners = append(owners, owner)
		}
		sort.Strings(owners)
		views[i].Installations = owners
	}
}

type workspaceIssueTrackerUpsertRequest struct {
	Type              string `json:"type"`
	Workspace         string `json:"workspace"`
	OriginalWorkspace string `json:"originalWorkspace,omitempty"`
	Token             string `json:"token,omitempty"`
	WebhookSecret     string `json:"webhookSecret,omitempty"`
}

func (s *Server) handleWorkspaceIssueTrackersCRUD(w http.ResponseWriter, r *http.Request) {
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	if workspaceName == "" {
		http.Error(w, "workspace required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		trackers, err := workspaceIssueTrackerViews(workspaceName)
		if err != nil {
			http.Error(w, "list issue trackers: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string][]workspaceIssueTrackerView{"issueTrackers": trackers})
	case http.MethodPut, http.MethodPost:
		var req workspaceIssueTrackerUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.Type = strings.TrimSpace(req.Type)
		req.Workspace = strings.TrimSpace(req.Workspace)
		if req.Workspace == "" {
			req.Workspace = workspaceName
		}
		if req.Type == "" {
			http.Error(w, "type required", http.StatusBadRequest)
			return
		}
		if req.Token == "" {
			existingName := req.OriginalWorkspace
			if existingName == "" {
				existingName = req.Workspace
			}
			if existing, ok := findWorkspaceIssueTracker(workspaceName, req.Type, existingName); ok {
				req.Token = existing.Token
				if req.WebhookSecret == "" {
					req.WebhookSecret = existing.WebhookSecret
				}
			}
		}
		if req.Token == "" {
			http.Error(w, "token required", http.StatusBadRequest)
			return
		}
		if req.OriginalWorkspace != "" && !strings.EqualFold(req.OriginalWorkspace, req.Workspace) {
			if err := deleteWorkspaceIssueTracker(workspaceName, req.Type, req.OriginalWorkspace); err != nil {
				http.Error(w, "rename issue tracker: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if err := saveWorkspaceIssueTracker(workspaceName, req.Type, req.Workspace, workspaceIssueTracker{
			Token:         req.Token,
			WebhookSecret: req.WebhookSecret,
		}); err != nil {
			http.Error(w, "save issue tracker: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"upserted": req.Workspace})
	case http.MethodDelete:
		trackerType := strings.TrimSpace(r.URL.Query().Get("type"))
		name := strings.TrimSpace(r.URL.Query().Get("workspace"))
		if trackerType == "" || name == "" {
			http.Error(w, "type and workspace required", http.StatusBadRequest)
			return
		}
		if err := deleteWorkspaceIssueTracker(workspaceName, trackerType, name); err != nil {
			http.Error(w, "delete issue tracker: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"deleted": name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
