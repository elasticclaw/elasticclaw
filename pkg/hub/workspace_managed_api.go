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
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		names, err := workspaceSecretNames(workspace)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "list secrets: "+err.Error())
			return
		}
		jsonOK(w, map[string][]string{"secrets": names})
	case http.MethodPut, http.MethodPost:
		var req workspaceSecretUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name required")
			return
		}
		if err := saveWorkspaceSecret(workspace, req.Name, req.Value); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "save secret: "+err.Error())
			return
		}
		jsonOK(w, map[string]string{"upserted": req.Name})
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name required")
			return
		}
		if err := deleteWorkspaceSecret(workspace, name); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "delete secret: "+err.Error())
			return
		}
		jsonOK(w, map[string]string{"deleted": name})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
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
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		apps, err := workspaceGitHubAppViews(workspace)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "list github apps: "+err.Error())
			return
		}
		enrichWorkspaceGitHubAppInstallations(r, apps, workspace)
		jsonOK(w, map[string][]workspaceGitHubAppView{"githubApps": apps})
	case http.MethodPut, http.MethodPost:
		var req workspaceGitHubAppUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name required")
			return
		}
		if req.AppID == 0 {
			writeErr(w, http.StatusBadRequest, "bad_request", "appId required")
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
			writeErr(w, http.StatusInternalServerError, "internal", "save github app: "+err.Error())
			return
		}
		jsonOK(w, map[string]string{"upserted": req.Name})
	case http.MethodDelete:
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "name required")
			return
		}
		if err := deleteWorkspaceGitHubApp(workspace, name); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "delete github app: "+err.Error())
			return
		}
		jsonOK(w, map[string]string{"deleted": name})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
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
	BaseURL           string `json:"baseUrl,omitempty"`
	Username          string `json:"username,omitempty"`
	Token             string `json:"token,omitempty"`
	WebhookSecret     string `json:"webhookSecret,omitempty"`
}

func (s *Server) handleWorkspaceIssueTrackersCRUD(w http.ResponseWriter, r *http.Request) {
	workspaceName := strings.TrimSpace(r.PathValue("workspace"))
	if workspaceName == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "workspace required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		trackers, err := workspaceIssueTrackerViews(workspaceName)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "list issue trackers: "+err.Error())
			return
		}
		jsonOK(w, map[string][]workspaceIssueTrackerView{"issueTrackers": trackers})
	case http.MethodPut, http.MethodPost:
		var req workspaceIssueTrackerUpsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
			return
		}
		req.Type = strings.TrimSpace(req.Type)
		req.Workspace = strings.TrimSpace(req.Workspace)
		req.BaseURL = strings.TrimSpace(req.BaseURL)
		req.Username = strings.TrimSpace(req.Username)
		if req.Workspace == "" {
			req.Workspace = workspaceName
		}
		if req.Type == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "type required")
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
				if req.BaseURL == "" {
					req.BaseURL = existing.BaseURL
				}
				if req.Username == "" {
					req.Username = existing.Username
				}
			}
		}
		if req.Token == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "token required")
			return
		}
		if req.Type == "jira" && req.BaseURL == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "baseUrl required")
			return
		}
		if req.OriginalWorkspace != "" && !strings.EqualFold(req.OriginalWorkspace, req.Workspace) {
			if err := deleteWorkspaceIssueTracker(workspaceName, req.Type, req.OriginalWorkspace); err != nil {
				writeErr(w, http.StatusInternalServerError, "internal", "rename issue tracker: "+err.Error())
				return
			}
		}
		if err := saveWorkspaceIssueTracker(workspaceName, req.Type, req.Workspace, workspaceIssueTracker{
			BaseURL:       req.BaseURL,
			Username:      req.Username,
			Token:         req.Token,
			WebhookSecret: req.WebhookSecret,
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "save issue tracker: "+err.Error())
			return
		}
		jsonOK(w, map[string]string{"upserted": req.Workspace})
	case http.MethodDelete:
		trackerType := strings.TrimSpace(r.URL.Query().Get("type"))
		name := strings.TrimSpace(r.URL.Query().Get("workspace"))
		if trackerType == "" || name == "" {
			writeErr(w, http.StatusBadRequest, "bad_request", "type and workspace required")
			return
		}
		if err := deleteWorkspaceIssueTracker(workspaceName, trackerType, name); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal", "delete issue tracker: "+err.Error())
			return
		}
		jsonOK(w, map[string]string{"deleted": name})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}
