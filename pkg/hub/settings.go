package hub

import (
	"encoding/json"
	"net/http"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// SettingsStatus is returned by GET /api/settings/status.
// Used by the web UI to decide whether to redirect to settings on login.
type SettingsStatus struct {
	HasProvider bool `json:"hasProvider"`
	HasLLMKey   bool `json:"hasLLMKey"`
	HasGitHub   bool `json:"hasGitHub"`
}

// SettingsView is the redacted view of hub config for the settings page.
// Secrets are masked — never returned in full.
type SettingsView struct {
	LLMKeys       map[string]bool         `json:"llmKeys"`
	Providers     map[string]ProviderView `json:"providers"`
	GitHub        []GitHubAppView         `json:"github"`
	SSHPublicKeys []string                `json:"sshPublicKeys"`
	Integrations  *IntegrationsView       `json:"integrations"`
	Factories     []FactoryView           `json:"factories"`
}

type IntegrationsView struct {
	Linear []LinearIntegrationView `json:"linear"`
}

type LinearIntegrationView struct {
	Workspace        string `json:"workspace"`
	TokenSet         bool   `json:"tokenSet"`
	WebhookSecretSet bool   `json:"webhookSecretSet"`
}

type FactoryView struct {
	Name             string `json:"name"`
	Integration      string `json:"integration"`
	Workspace        string `json:"workspace"`
	Team             string `json:"team"`
	TriggerStatus    string `json:"triggerStatus"`
	DoneStatus       string `json:"doneStatus"`
	TerminateOnLeave bool   `json:"terminateOnLeave"`
	Template         string `json:"template"`
}

type ProviderView struct {
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	// Daytona
	APIURL          string `json:"apiUrl,omitempty"`
	APIKeySet       bool   `json:"apiKeySet,omitempty"`
	DefaultSnapshot string `json:"defaultSnapshot,omitempty"`
	// Replicated
	TokenSet            bool   `json:"tokenSet,omitempty"`
	DefaultTTL          string `json:"defaultTtl,omitempty"`
	DefaultInstanceType string `json:"defaultInstanceType,omitempty"`
}

type GitHubAppView struct {
	AppID  int64  `json:"appId"`
	URL    string `json:"url,omitempty"`
	KeySet bool   `json:"keySet"`
}

// SettingsPatch is the request body for PATCH /api/settings.
// Only non-nil fields are updated.
type SettingsPatch struct {
	LLMKeys       map[string]string        `json:"llmKeys,omitempty"`
	Providers     map[string]ProviderPatch `json:"providers,omitempty"`
	GitHub        []GitHubAppPatch         `json:"github,omitempty"`
	UIPassword    string                   `json:"uiPassword,omitempty"`
	SSHPublicKeys *[]string                `json:"sshPublicKeys,omitempty"`
	Integrations  *IntegrationsPatch       `json:"integrations,omitempty"`
	Factories     []FactoryPatch           `json:"factories,omitempty"`
}

type IntegrationsPatch struct {
	Linear []LinearIntegrationPatch `json:"linear,omitempty"`
}

type LinearIntegrationPatch struct {
	Workspace     string `json:"workspace"`
	Token         string `json:"token,omitempty"`
	WebhookSecret string `json:"webhookSecret,omitempty"`
}

type FactoryPatch struct {
	Name             string `json:"name"`
	Integration      string `json:"integration"`
	Workspace        string `json:"workspace"`
	Team             string `json:"team,omitempty"`
	TriggerStatus    string `json:"triggerStatus"`
	DoneStatus       string `json:"doneStatus,omitempty"`
	TerminateOnLeave bool   `json:"terminateOnLeave"`
	Template         string `json:"template"`
}

type ProviderPatch struct {
	// Daytona
	APIURL          string `json:"apiUrl,omitempty"`
	APIKey          string `json:"apiKey,omitempty"`
	DefaultSnapshot string `json:"defaultSnapshot,omitempty"`
	// Replicated
	Token               string `json:"token,omitempty"`
	DefaultTTL          string `json:"defaultTtl,omitempty"`
	DefaultInstanceType string `json:"defaultInstanceType,omitempty"`
}

type GitHubAppPatch struct {
	AppID         int64  `json:"appId"`
	URL           string `json:"url,omitempty"`
	PrivateKeyPEM string `json:"privateKeyPem,omitempty"` // full PEM, stored on server only
}

func (s *Server) handleSettingsStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	hasProvider := false
	for _, p := range s.hubCfg.Providers {
		if p.Token != "" || p.APIKey != "" || p.AccessToken != "" || p.Enabled {
			hasProvider = true
			break
		}
	}
	hasLLMKey := len(s.hubCfg.LLMKeys) > 0
	hasGitHub := len(s.hubCfg.GitHubApps) > 0
	s.mu.RUnlock()

	jsonOK(w, SettingsStatus{
		HasProvider: hasProvider,
		HasLLMKey:   hasLLMKey,
		HasGitHub:   hasGitHub,
	})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getSettings(w, r)
	case http.MethodPatch:
		s.patchSettings(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	view := SettingsView{
		LLMKeys:   make(map[string]bool),
		Providers: make(map[string]ProviderView),
		GitHub:    []GitHubAppView{},
	}

	s.mu.RLock()
	// LLM keys — boolean flag only
	for provider, key := range s.hubCfg.LLMKeys {
		view.LLMKeys[provider] = key != ""
	}

	// Providers
	for name, p := range s.hubCfg.Providers {
		pv := ProviderView{Type: name, Enabled: true}
		switch name {
		case "daytona":
			pv.APIURL = p.APIURL
			pv.APIKeySet = p.APIKey != ""
			pv.DefaultSnapshot = p.DefaultSnapshot
		case "replicated":
			pv.TokenSet = p.Token != ""
			pv.DefaultTTL = p.DefaultTTL
			pv.DefaultInstanceType = p.DefaultInstanceType
		}
		view.Providers[name] = pv
	}

	// SSH public keys
	view.SSHPublicKeys = s.hubCfg.SSHPublicKeys
	if view.SSHPublicKeys == nil {
		view.SSHPublicKeys = []string{}
	}

	// GitHub Apps
	for _, app := range s.hubCfg.GitHubApps {
		view.GitHub = append(view.GitHub, GitHubAppView{
			AppID:  app.AppID,
			URL:    app.URL,
			KeySet: app.PrivateKeyPEM != "",
		})
	}

	// Integrations
	view.Integrations = &IntegrationsView{Linear: []LinearIntegrationView{}}
	if s.hubCfg.Integrations != nil {
		for _, li := range s.hubCfg.Integrations.Linear {
			view.Integrations.Linear = append(view.Integrations.Linear, LinearIntegrationView{
				Workspace:        li.Workspace,
				TokenSet:         li.Token != "",
				WebhookSecretSet: li.WebhookSecret != "",
			})
		}
	}

	// Factories
	view.Factories = []FactoryView{}
	for _, f := range s.hubCfg.Factories {
		view.Factories = append(view.Factories, FactoryView{
			Name:             f.Name,
			Integration:      f.Integration,
			Workspace:        f.Workspace,
			Team:             f.Team,
			TriggerStatus:    f.TriggerStatus,
			DoneStatus:       f.DoneStatus,
			TerminateOnLeave: f.TerminateOnLeave,
			Template:         f.Template,
		})
	}
	s.mu.RUnlock()

	jsonOK(w, view)
}

func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request) {
	var patch SettingsPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Shallow copy of config struct; maps and slices are deep-copied only when modified below
	updatedCfg := *s.hubCfg

	// LLM keys
	if patch.LLMKeys != nil {
		if updatedCfg.LLMKeys == nil {
			updatedCfg.LLMKeys = make(map[string]string)
		} else {
			// Deep copy the map
			newKeys := make(map[string]string, len(updatedCfg.LLMKeys))
			for k, v := range updatedCfg.LLMKeys {
				newKeys[k] = v
			}
			updatedCfg.LLMKeys = newKeys
		}
		for k, v := range patch.LLMKeys {
			if v == "" {
				delete(updatedCfg.LLMKeys, k)
			} else {
				updatedCfg.LLMKeys[k] = v
			}
		}
	}

	// Providers
	if patch.Providers != nil {
		if updatedCfg.Providers == nil {
			updatedCfg.Providers = make(map[string]types.ProviderConfig)
		} else {
			// Deep copy the map
			newProviders := make(map[string]types.ProviderConfig, len(updatedCfg.Providers))
			for k, v := range updatedCfg.Providers {
				newProviders[k] = v
			}
			updatedCfg.Providers = newProviders
		}
		for name, pp := range patch.Providers {
			existing := updatedCfg.Providers[name]
			switch name {
			case "daytona":
				if pp.APIURL != "" {
					existing.APIURL = pp.APIURL
				}
				if pp.APIKey != "" {
					existing.APIKey = pp.APIKey
				}
				if pp.DefaultSnapshot != "" {
					existing.DefaultSnapshot = pp.DefaultSnapshot
				}
			case "replicated":
				if pp.Token != "" {
					existing.Token = pp.Token
				}
				if pp.DefaultTTL != "" {
					existing.DefaultTTL = pp.DefaultTTL
				}
				if pp.DefaultInstanceType != "" {
					existing.DefaultInstanceType = pp.DefaultInstanceType
				}
			}
			updatedCfg.Providers[name] = existing
		}
	}

	// GitHub Apps
	if patch.GitHub != nil {
		var apps []*types.GitHubAppConfig
		for _, ga := range patch.GitHub {
			app := &types.GitHubAppConfig{
				AppID: ga.AppID,
				URL:   ga.URL,
			}
			if ga.PrivateKeyPEM != "" {
				app.PrivateKeyPEM = ga.PrivateKeyPEM
			} else {
				// Preserve existing key if not provided
				for _, existing := range s.hubCfg.GitHubApps {
					if existing.AppID == ga.AppID {
						app.PrivateKeyPEM = existing.PrivateKeyPEM
						break
					}
				}
			}
			apps = append(apps, app)
		}
		updatedCfg.GitHubApps = apps
	}

	// UI password
	if patch.UIPassword != "" {
		updatedCfg.UIPassword = patch.UIPassword
	}

	// SSH public keys
	if patch.SSHPublicKeys != nil {
		updatedCfg.SSHPublicKeys = *patch.SSHPublicKeys
	}

	// Integrations
	if patch.Integrations != nil && patch.Integrations.Linear != nil {
		if updatedCfg.Integrations == nil {
			updatedCfg.Integrations = &types.IntegrationsConfig{}
		}
		var linears []*types.LinearIntegrationConfig
		for _, lp := range patch.Integrations.Linear {
			li := &types.LinearIntegrationConfig{Workspace: lp.Workspace}
			// Find existing to preserve secrets not being updated
			for _, existing := range updatedCfg.Integrations.Linear {
				if existing.Workspace == lp.Workspace {
					li.Token = existing.Token
					li.WebhookSecret = existing.WebhookSecret
					break
				}
			}
			if lp.Token != "" {
				li.Token = lp.Token
			}
			if lp.WebhookSecret != "" {
				li.WebhookSecret = lp.WebhookSecret
			}
			linears = append(linears, li)
		}
		updatedCfg.Integrations.Linear = linears
	}

	// Factories (full replace)
	if patch.Factories != nil {
		var factories []*types.FactoryConfig
		for _, fp := range patch.Factories {
			factories = append(factories, &types.FactoryConfig{
				Name: fp.Name, Integration: fp.Integration,
				Workspace: fp.Workspace, Team: fp.Team,
				TriggerStatus: fp.TriggerStatus, DoneStatus: fp.DoneStatus,
				TerminateOnLeave: fp.TerminateOnLeave, Template: fp.Template,
			})
		}
		updatedCfg.Factories = factories
	}

	// Save to disk before applying to in-memory config
	if err := config.SaveHubConfig(&updatedCfg); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Only update in-memory config after successful disk write
	s.hubCfg = &updatedCfg

	jsonOK(w, map[string]bool{"ok": true})
}
