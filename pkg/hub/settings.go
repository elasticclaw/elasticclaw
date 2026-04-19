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
	LLMKeys   map[string]string      `json:"llmKeys"`   // provider → masked key ("sk-ant-...***")
	Providers map[string]ProviderView `json:"providers"` // provider name → config (tokens masked)
	GitHub    []GitHubAppView        `json:"github"`
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
	AppID     int64  `json:"appId"`
	URL       string `json:"url,omitempty"`
	KeySet    bool   `json:"keySet"`
}

// SettingsPatch is the request body for PATCH /api/settings.
// Only non-nil fields are updated.
type SettingsPatch struct {
	LLMKeys   map[string]string         `json:"llmKeys,omitempty"`
	Providers map[string]ProviderPatch  `json:"providers,omitempty"`
	GitHub    []GitHubAppPatch          `json:"github,omitempty"`
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
	hasProvider := false
	for _, p := range s.hubCfg.Providers {
		if p.Token != "" || p.APIKey != "" {
			hasProvider = true
			break
		}
	}
	hasLLMKey := len(s.hubCfg.LLMKeys) > 0
	hasGitHub := len(s.hubCfg.GitHubApps) > 0

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
		LLMKeys:   make(map[string]string),
		Providers: make(map[string]ProviderView),
		GitHub:    []GitHubAppView{},
	}

	// Mask LLM keys — show prefix only
	for provider, key := range s.hubCfg.LLMKeys {
		if len(key) > 8 {
			view.LLMKeys[provider] = key[:8] + "***"
		} else if key != "" {
			view.LLMKeys[provider] = "***"
		}
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

	// GitHub Apps
	for _, app := range s.hubCfg.GitHubApps {
		view.GitHub = append(view.GitHub, GitHubAppView{
			AppID:  app.AppID,
			URL:    app.URL,
			KeySet: app.PrivateKeyPEM != "",
		})
	}

	jsonOK(w, view)
}

func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request) {
	var patch SettingsPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	// Update in-memory config
	// LLM keys
	if patch.LLMKeys != nil {
		if s.hubCfg.LLMKeys == nil {
			s.hubCfg.LLMKeys = make(map[string]string)
		}
		for k, v := range patch.LLMKeys {
			if v == "" {
				delete(s.hubCfg.LLMKeys, k)
			} else {
				s.hubCfg.LLMKeys[k] = v
			}
		}
	}

	// Providers
	if patch.Providers != nil {
		if s.hubCfg.Providers == nil {
			s.hubCfg.Providers = make(map[string]types.ProviderConfig)
		}
		for name, pp := range patch.Providers {
			existing := s.hubCfg.Providers[name]
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
			s.hubCfg.Providers[name] = existing
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
		s.hubCfg.GitHubApps = apps
	}

	// Save to disk
	if err := config.SaveHubConfig(s.hubCfg); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]bool{"ok": true})
}
