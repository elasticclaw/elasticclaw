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

// LLMKeyView is the masked view of a named LLM key.
type LLMKeyView struct {
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	KeySet       bool   `json:"keySet"`
	Default      bool   `json:"default"`
	DefaultModel string `json:"defaultModel,omitempty"`
}

// SettingsView is the redacted view of hub config for the settings page.
// Secrets are masked — never returned in full.
type SettingsView struct {
	LLMKeys       []LLMKeyView            `json:"llmKeys"`
	Providers     map[string]ProviderView `json:"providers"`
	GitHub        []GitHubAppView         `json:"github"`
	SSHPublicKeys []string                `json:"sshPublicKeys"`
	Integrations  *IntegrationsView       `json:"integrations"`
	Factories     []FactoryView           `json:"factories"`
	Secrets       []string                `json:"secrets"`
}

type IntegrationsView struct {
	Linear   []LinearIntegrationView   `json:"linear"`
	Shortcut []ShortcutIntegrationView `json:"shortcut"`
}

type ShortcutIntegrationView struct {
	Workspace string `json:"workspace"`
	TokenSet  bool   `json:"tokenSet"`
}

type LinearIntegrationView struct {
	Workspace        string `json:"workspace"`
	TokenSet         bool   `json:"tokenSet"`
	WebhookSecretSet bool   `json:"webhookSecretSet"`
}

// isFactoryEnabled returns true if the factory is enabled (nil = enabled by default).
func isFactoryEnabled(f *types.FactoryConfig) bool {
	return f.Enabled == nil || *f.Enabled
}

type FactoryView struct {
	Name             string   `json:"name"`
	Integration      string   `json:"integration"`
	Workspace        string   `json:"workspace"`
	Team             string   `json:"team"`
	TriggerStatus    string   `json:"triggerStatus"`
	DoneStatus       string   `json:"doneStatus"`
	TerminateOnLeave bool     `json:"terminateOnLeave"`
	Template         string   `json:"template"`
	NamePattern      string   `json:"namePattern"`
	WebhookSecretSet bool     `json:"webhookSecretSet"`
	WebhookSecretRef string   `json:"webhookSecretRef,omitempty"`
	PipelineYAML     string   `json:"pipelineYAML,omitempty"`
	Tags             []string `json:"tags"`
	Color            string   `json:"color"`
	Labels           []string `json:"labels,omitempty"`
	AssignedTo       string   `json:"assigned_to,omitempty"`
	Enabled          bool     `json:"enabled"`
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
// LLMKeyPatch adds/updates a named LLM key. Set APIKey to "" to remove.
type LLMKeyPatch struct {
	Name         string  `json:"name"`
	Provider     string  `json:"provider,omitempty"`
	APIKey       string  `json:"apiKey,omitempty"`
	Default      *bool   `json:"default,omitempty"`
	Delete       bool    `json:"delete,omitempty"`
	DefaultModel *string `json:"defaultModel,omitempty"`
}

type SettingsPatch struct {
	LLMKeys       []LLMKeyPatch            `json:"llmKeys,omitempty"`
	Providers     map[string]ProviderPatch `json:"providers,omitempty"`
	GitHub        []GitHubAppPatch         `json:"github,omitempty"`
	UIPassword    string                   `json:"uiPassword,omitempty"`
	SSHPublicKeys *[]string                `json:"sshPublicKeys,omitempty"`
	Integrations  *IntegrationsPatch       `json:"integrations,omitempty"`
	Factories     []FactoryPatch           `json:"factories,omitempty"`
}

type IntegrationsPatch struct {
	Linear   []LinearIntegrationPatch   `json:"linear,omitempty"`
	Shortcut []ShortcutIntegrationPatch `json:"shortcut,omitempty"`
}

type ShortcutIntegrationPatch struct {
	Workspace         string `json:"workspace"`
	OriginalWorkspace string `json:"originalWorkspace,omitempty"`
	Token             string `json:"token,omitempty"`
	Delete            bool   `json:"delete,omitempty"`
}

type LinearIntegrationPatch struct {
	Workspace         string `json:"workspace"`
	OriginalWorkspace string `json:"originalWorkspace,omitempty"`
	Token             string `json:"token,omitempty"`
	WebhookSecret     string `json:"webhookSecret,omitempty"`
}

type FactoryPatch struct {
	Name             string   `json:"name"`
	OriginalName     string   `json:"originalName,omitempty"`
	Integration      string   `json:"integration"`
	Workspace        string   `json:"workspace"`
	Team             string   `json:"team,omitempty"`
	TriggerStatus    string   `json:"triggerStatus"`
	DoneStatus       string   `json:"doneStatus,omitempty"`
	TerminateOnLeave bool     `json:"terminateOnLeave"`
	Template         string   `json:"template"`
	NamePattern      string   `json:"namePattern,omitempty"`
	WebhookSecret    string   `json:"webhookSecret,omitempty"`
	WebhookSecretRef string   `json:"webhookSecretRef,omitempty"`
	PipelineYAML     string   `json:"pipelineYAML,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Color            string   `json:"color,omitempty"`
	Labels           []string `json:"labels,omitempty"`
	AssignedTo       string   `json:"assigned_to,omitempty"`
	Enabled          *bool    `json:"enabled,omitempty"`
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
		Providers: make(map[string]ProviderView),
		GitHub:    []GitHubAppView{},
	}

	s.mu.RLock()
	// LLM keys — mask actual key values
	view.LLMKeys = []LLMKeyView{}
	for _, k := range s.hubCfg.LLMKeys {
		view.LLMKeys = append(view.LLMKeys, LLMKeyView{
			Name:         k.Name,
			Provider:     k.Provider,
			KeySet:       k.APIKey != "",
			Default:      k.Default,
			DefaultModel: k.DefaultModel,
		})
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
	view.Integrations = &IntegrationsView{Linear: []LinearIntegrationView{}, Shortcut: []ShortcutIntegrationView{}}
	if s.hubCfg.Integrations != nil {
		for _, sc := range s.hubCfg.Integrations.Shortcut {
			view.Integrations.Shortcut = append(view.Integrations.Shortcut, ShortcutIntegrationView{
				Workspace: sc.Workspace,
				TokenSet:  sc.Token != "",
			})
		}
		for _, li := range s.hubCfg.Integrations.Linear {
			view.Integrations.Linear = append(view.Integrations.Linear, LinearIntegrationView{
				Workspace:        li.Workspace,
				TokenSet:         li.Token != "",
				WebhookSecretSet: li.WebhookSecret != "",
			})
		}
	}

	// Secrets — names only, read from disk so manually-edited hub.yaml entries are visible
	view.Secrets = []string{}
	s.mu.RUnlock()
	if diskCfg, err := config.LoadHubConfig(); err == nil && diskCfg != nil {
		for k := range diskCfg.Secrets {
			view.Secrets = append(view.Secrets, k)
		}
	}
	s.mu.RLock()

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
			NamePattern:      f.NamePattern,
			WebhookSecretSet: f.WebhookSecret != "" || f.WebhookSecretRef != "",
			WebhookSecretRef: f.WebhookSecretRef,
			PipelineYAML:     f.PipelineYAML,
			Tags:             f.Tags,
			Color:            f.Color,
			Labels:           f.Labels,
			AssignedTo:       f.AssignedTo,
			Enabled:          isFactoryEnabled(f),
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

	// LLM keys — upsert/delete by name
	if len(patch.LLMKeys) > 0 {
		// Deep copy existing keys
		existing := make([]*types.LLMKeyConfig, len(updatedCfg.LLMKeys))
		for i, k := range updatedCfg.LLMKeys {
			copy := *k
			existing[i] = &copy
		}
		for _, kp := range patch.LLMKeys {
			if kp.Delete {
				// Remove by name
				filtered := existing[:0]
				for _, k := range existing {
					if k.Name != kp.Name {
						filtered = append(filtered, k)
					}
				}
				existing = filtered
				continue
			}
			// Find existing by name
			var found *types.LLMKeyConfig
			for _, k := range existing {
				if k.Name == kp.Name {
					found = k
					break
				}
			}
			if found == nil {
				found = &types.LLMKeyConfig{Name: kp.Name}
				existing = append(existing, found)
			}
			if kp.Provider != "" {
				found.Provider = kp.Provider
			}
			if kp.APIKey != "" {
				found.APIKey = kp.APIKey
			}
			if kp.Default != nil {
				// Clear other defaults
				if *kp.Default {
					for _, k := range existing {
						k.Default = false
					}
				}
				found.Default = *kp.Default
			}
			if kp.DefaultModel != nil {
				found.DefaultModel = *kp.DefaultModel
			}
		}
		updatedCfg.LLMKeys = existing
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
		// Deep copy IntegrationsConfig to avoid mutating live config
		var existingIntegrations *types.IntegrationsConfig
		if updatedCfg.Integrations != nil {
			existingIntegrations = updatedCfg.Integrations
		}
		updatedCfg.Integrations = &types.IntegrationsConfig{}

		var linears []*types.LinearIntegrationConfig
		for _, lp := range patch.Integrations.Linear {
			li := &types.LinearIntegrationConfig{Workspace: lp.Workspace}
			// Find existing to preserve secrets not being updated
			// Use originalWorkspace if provided (for renames), otherwise use workspace
			matchWorkspace := lp.Workspace
			if lp.OriginalWorkspace != "" {
				matchWorkspace = lp.OriginalWorkspace
			}
			if existingIntegrations != nil {
				for _, existing := range existingIntegrations.Linear {
					if existing.Workspace == matchWorkspace {
						li.Token = existing.Token
						li.WebhookSecret = existing.WebhookSecret
						break
					}
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

		// Preserve existing Shortcut integrations not in patch
		if existingIntegrations != nil {
			updatedCfg.Integrations.Shortcut = existingIntegrations.Shortcut
		}
	}

	if patch.Integrations != nil && patch.Integrations.Shortcut != nil {
		// Deep copy IntegrationsConfig to avoid mutating live config
		var existingIntegrations *types.IntegrationsConfig
		if updatedCfg.Integrations != nil {
			existingIntegrations = updatedCfg.Integrations
		}
		if updatedCfg.Integrations == nil {
			updatedCfg.Integrations = &types.IntegrationsConfig{}
		} else {
			updatedCfg.Integrations = &types.IntegrationsConfig{
				Linear:   existingIntegrations.Linear,
				Shortcut: existingIntegrations.Shortcut,
			}
		}
		existing := updatedCfg.Integrations.Shortcut
		var shortcuts []*types.ShortcutIntegrationConfig
		for _, sp := range patch.Integrations.Shortcut {
			if sp.Delete {
				continue
			}
			sc := &types.ShortcutIntegrationConfig{Workspace: sp.Workspace}
			matchWorkspace := sp.Workspace
			if sp.OriginalWorkspace != "" {
				matchWorkspace = sp.OriginalWorkspace
			}
			for _, ex := range existing {
				if ex.Workspace == matchWorkspace {
					sc.Token = ex.Token
					break
				}
			}
			if sp.Token != "" {
				sc.Token = sp.Token
			}
			shortcuts = append(shortcuts, sc)
		}
		updatedCfg.Integrations.Shortcut = shortcuts
	}

	// Factories (full replace)
	if patch.Factories != nil {
		var factories []*types.FactoryConfig
		for _, fp := range patch.Factories {
			// Preserve existing webhook secret if not being replaced
			webhookSecret := fp.WebhookSecret
			if webhookSecret == "" {
				// Find existing factory by name and keep its secret
				// Use originalName if provided (for renames), otherwise use name
				matchName := fp.Name
				if fp.OriginalName != "" {
					matchName = fp.OriginalName
				}
				for _, existing := range s.hubCfg.Factories {
					if existing.Name == matchName {
						webhookSecret = existing.WebhookSecret
						break
					}
				}
			}
			factories = append(factories, &types.FactoryConfig{
				Name: fp.Name, Integration: fp.Integration,
				Workspace: fp.Workspace, Team: fp.Team,
				TriggerStatus: fp.TriggerStatus, DoneStatus: fp.DoneStatus,
				TerminateOnLeave: fp.TerminateOnLeave, Template: fp.Template,
				NamePattern: fp.NamePattern, WebhookSecret: webhookSecret,
				WebhookSecretRef: fp.WebhookSecretRef, PipelineYAML: fp.PipelineYAML,
				Tags: fp.Tags, Color: fp.Color, Labels: fp.Labels,
				AssignedTo: fp.AssignedTo, Enabled: fp.Enabled,
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
