package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/notify"
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
	AuthProfile  string `json:"authProfile,omitempty"`
}

type ModelAuthProfileView struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	Mode          string `json:"mode"`
	Authenticated bool   `json:"authenticated"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
}

// MCPView is the redacted view of an MCP server config for the settings page.
type MCPView struct {
	Name    string            `json:"name"`
	Source  string            `json:"source"`
	Package string            `json:"package,omitempty"`
	Image   string            `json:"image,omitempty"`
	URL     string            `json:"url,omitempty"`
	Enabled bool              `json:"enabled"`
	Config  map[string]string `json:"config,omitempty"`
	Secrets []string          `json:"secrets,omitempty"` // env var names only, not values
	Command []string          `json:"command,omitempty"`
}

// ConcurrencyGroupView is the JSON-safe view of a concurrency group.
type ConcurrencyGroupView struct {
	Name  string `json:"name"`
	Limit int    `json:"limit"`
}

// SettingsView is the redacted view of hub config for the settings page.
// Secrets are masked — never returned in full.
type SettingsView struct {
	DefaultOpenClawImage string                      `json:"defaultOpenClawImage"`
	LLMKeys              []LLMKeyView                `json:"llmKeys"`
	ModelOptions         map[string][]LLMModelOption `json:"modelOptions,omitempty"`
	ModelAuthProfiles    []ModelAuthProfileView      `json:"modelAuthProfiles,omitempty"`
	Providers            map[string]ProviderView     `json:"providers"`
	GitHub               []GitHubAppView             `json:"github"`
	SSHPublicKeys        []string                    `json:"sshPublicKeys"`
	Integrations         *IntegrationsView           `json:"integrations"`
	Factories            []FactoryView               `json:"factories"`
	Secrets              []string                    `json:"secrets"`
	MCPServers           []MCPView                   `json:"mcpServers,omitempty"`
	Auth                 *AuthView                   `json:"auth,omitempty"`
	Notifications        *NotificationsView          `json:"notifications"`
	LifecycleEventTypes  []string                    `json:"lifecycleEventTypes"`
	// ConcurrencyGroups limits simultaneously running claws per group. 0 = unlimited.
	ConcurrencyGroups []ConcurrencyGroupView `json:"concurrencyGroups"`
	// MaxConcurrentClaws limits simultaneously running claws. 0 = unlimited.
	// DEPRECATED: Use ConcurrencyGroups instead.
	MaxConcurrentClaws int `json:"maxConcurrentClaws"`
}

// NotificationsView is the settings-safe view of outbound notifications.
// It intentionally exposes secret references, never secret values.
type NotificationsView struct {
	Notifiers map[string]NotifierView     `json:"notifiers"`
	Lifecycle *LifecycleNotificationsView `json:"lifecycle,omitempty"`
}

type NotifierView struct {
	Type            string `json:"type"`
	Channel         string `json:"channel,omitempty"`
	TokenSecret     string `json:"token_secret,omitempty"`
	APIBase         string `json:"api_base,omitempty"`
	MinSendInterval string `json:"min_send_interval,omitempty"`
}

// LifecycleNotificationsView deliberately uses the SAME field names the PATCH
// body is decoded under (types.LifecycleNotificationsConfig): a client that
// GETs this view, edits it and PATCHes it back must not silently drop the two
// durations. They were emitted as poll_interval/idle_after, which
// encoding/json discards as unknown keys on the way back in — resetting a
// deliberately raised idle_after to the 5m default and persisting the loss.
type LifecycleNotificationsView struct {
	Enabled      bool                         `json:"enabled"`
	Via          string                       `json:"via,omitempty"`
	Routes       []types.LifecycleRoute       `json:"routes"`
	PollInterval string                       `json:"pollInterval,omitempty"`
	IdleAfter    string                       `json:"idleAfter,omitempty"`
	Events       *types.LifecycleEventToggles `json:"events,omitempty"`
}

type AuthView struct {
	GitHubOAuth         *GitHubOAuthView `json:"githubOAuth,omitempty"`
	Access              *AccessView      `json:"access,omitempty"`
	DisablePasswordAuth bool             `json:"disablePasswordAuth"`
}

type GitHubOAuthView struct {
	ClientID        string   `json:"clientId"`
	ClientSecretSet bool     `json:"clientSecretSet"`
	AllowedUsers    []string `json:"allowedUsers"`
	AllowedOrgs     []string `json:"allowedOrgs"`
	AllowedTeams    []string `json:"allowedTeams"`
}

type AccessView struct {
	Admins               []string `json:"admins"`
	ViewRequiresTags     []string `json:"viewRequiresTags"`
	InteractRequiresTags []string `json:"interactRequiresTags"`
}

type IntegrationsView struct {
	Linear       []LinearIntegrationView       `json:"linear"`
	Shortcut     []ShortcutIntegrationView     `json:"shortcut"`
	GitHubIssues []GitHubIssuesIntegrationView `json:"githubIssues"`
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

type GitHubIssuesIntegrationView struct {
	Workspace        string `json:"workspace"`
	TokenSet         bool   `json:"tokenSet"`
	WebhookSecretSet bool   `json:"webhookSecretSet"`
}

// isFactoryEnabled returns true if the factory is enabled (nil = enabled by default).
func isFactoryEnabled(f *types.FactoryConfig) bool {
	return f.Enabled == nil || *f.Enabled
}

type FactoryView struct {
	Name                string                 `json:"name"`
	Integration         string                 `json:"integration"`
	Workspace           string                 `json:"workspace"`
	Team                string                 `json:"team"`
	TriggerStatus       string                 `json:"triggerStatus"`
	DoneStatus          string                 `json:"doneStatus"`
	TerminateOnLeave    bool                   `json:"terminateOnLeave"`
	Template            string                 `json:"template"`
	NamePattern         string                 `json:"namePattern"`
	WebhookSecretSet    bool                   `json:"webhookSecretSet"`
	WebhookSecretRef    string                 `json:"webhookSecretRef,omitempty"`
	PipelineYAML        string                 `json:"pipelineYAML,omitempty"`
	Tags                []string               `json:"tags"`
	Color               string                 `json:"color"`
	Labels              []string               `json:"labels,omitempty"`
	ExcludeLabels       []string               `json:"exclude_labels,omitempty"`
	AssignedTo          string                 `json:"assigned_to,omitempty"`
	Enabled             bool                   `json:"enabled"`
	ConcurrencyGroup    string                 `json:"concurrencyGroup,omitempty"`
	EnableManualTrigger bool                   `json:"enable_manual_trigger,omitempty"`
	SecretRefs          map[string]string      `json:"secret_refs,omitempty"`
	ExternalTrigger     *types.ExternalTrigger `json:"externalTrigger,omitempty"`
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
	// exe.dev
	SSHKeySet     bool   `json:"sshKeySet,omitempty"`
	SSHPublicKey  string `json:"sshPublicKey,omitempty"`
	DefaultCPU    int    `json:"defaultCpu,omitempty"`
	DefaultMemory string `json:"defaultMemory,omitempty"`
	DefaultDisk   string `json:"defaultDisk,omitempty"`
	_sshKeyPath   string `json:"-"` // internal: path for file I/O outside lock
	// Docker
	Image   string `json:"image,omitempty"`
	Network string `json:"network,omitempty"`
	// AWS Lambda MicroVMs
	AWSRegion                  string   `json:"awsRegion,omitempty"`
	AWSProfile                 string   `json:"awsProfile,omitempty"`
	ImageIdentifier            string   `json:"imageIdentifier,omitempty"`
	ImageVersion               string   `json:"imageVersion,omitempty"`
	ExecutionRoleARN           string   `json:"executionRoleArn,omitempty"`
	IngressNetworkConnectors   []string `json:"ingressNetworkConnectors,omitempty"`
	EgressNetworkConnectors    []string `json:"egressNetworkConnectors,omitempty"`
	IdleMaxDurationSeconds     int      `json:"idleMaxDurationSeconds,omitempty"`
	SuspendedDurationSeconds   int      `json:"suspendedDurationSeconds,omitempty"`
	AutoResume                 *bool    `json:"autoResume,omitempty"`
	MaximumDurationSeconds     int      `json:"maximumDurationSeconds,omitempty"`
	MicroVMBridgePort          int      `json:"bridgePort,omitempty"`
	AuthTokenExpirationMinutes int      `json:"authTokenExpirationMinutes,omitempty"`
}

// GitHubAppPermission is a single permission check result for a GitHub App.
type GitHubAppPermission struct {
	Name    string `json:"name"`
	Granted string `json:"granted"` // "read", "write", or ""
	Needed  string `json:"needed"`  // "read" or "write"
	OK      bool   `json:"ok"`
}

type GitHubAppView struct {
	AppID          int64                 `json:"appId"`
	URL            string                `json:"url,omitempty"`
	KeySet         bool                  `json:"keySet"`
	Permissions    []GitHubAppPermission `json:"permissions,omitempty"`
	PermCheckOK    *bool                 `json:"permCheckOk,omitempty"` // nil = not checked yet
	PermCheckError string                `json:"permCheckError,omitempty"`
}

// SettingsPatch is the request body for PATCH /api/settings.
// Only non-nil fields are updated.
// LLMKeyPatch adds/updates a named LLM key. Set APIKey to "" to remove.
type LLMKeyPatch struct {
	Name         string  `json:"name"`
	NewName      string  `json:"newName,omitempty"` // for rename: lookup by Name, set to NewName
	Provider     string  `json:"provider,omitempty"`
	APIKey       string  `json:"apiKey,omitempty"`
	Default      *bool   `json:"default,omitempty"`
	Delete       bool    `json:"delete,omitempty"`
	DefaultModel *string `json:"defaultModel,omitempty"`
	AuthProfile  *string `json:"authProfile,omitempty"`
}

type ModelAuthProfilePatch struct {
	Name     string `json:"name"`
	Provider string `json:"provider,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Delete   bool   `json:"delete,omitempty"`
}

// ConcurrencyGroupPatch is a patch for a concurrency group.
type ConcurrencyGroupPatch struct {
	Name   string `json:"name"`
	Limit  int    `json:"limit"`
	Delete bool   `json:"delete,omitempty"`
}

type SettingsPatch struct {
	LLMKeys           []LLMKeyPatch            `json:"llmKeys,omitempty"`
	ModelAuthProfiles []ModelAuthProfilePatch  `json:"modelAuthProfiles,omitempty"`
	Providers         map[string]ProviderPatch `json:"providers,omitempty"`
	GitHub            []GitHubAppPatch         `json:"github,omitempty"`
	UIPassword        string                   `json:"uiPassword,omitempty"`
	SSHPublicKeys     *[]string                `json:"sshPublicKeys,omitempty"`
	Integrations      *IntegrationsPatch       `json:"integrations,omitempty"`
	Factories         []FactoryPatch           `json:"factories,omitempty"`
	MCPServers        []MCPPatch               `json:"mcpServers,omitempty"`
	Auth              *AuthPatch               `json:"auth,omitempty"`
	// ConcurrencyGroups replaces the full list of concurrency groups.
	ConcurrencyGroups *[]ConcurrencyGroupPatch `json:"concurrencyGroups,omitempty"`
	// MaxConcurrentClaws limits simultaneously running claws. 0 or omitted = unlimited.
	// DEPRECATED: Use ConcurrencyGroups instead.
	MaxConcurrentClaws *int `json:"maxConcurrentClaws,omitempty"`
	// Notifications replaces the complete notifications configuration.
	Notifications *types.NotificationsConfig `json:"notifications,omitempty"`
}

// MCPPatch is a request to add/update an MCP server config.
type MCPPatch struct {
	Name    string            `json:"name"`
	Source  string            `json:"source"`
	Package string            `json:"package,omitempty"`
	Image   string            `json:"image,omitempty"`
	URL     string            `json:"url,omitempty"`
	Enabled *bool             `json:"enabled,omitempty"`
	Config  map[string]string `json:"config,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"` // envVar -> secretRefName
	Command []string          `json:"command,omitempty"`
	Delete  bool              `json:"delete,omitempty"`
}

type AuthPatch struct {
	GitHubOAuth         *GitHubOAuthPatch `json:"githubOAuth,omitempty"`
	Access              *AccessPatch      `json:"access,omitempty"`
	RemoveGitHubOAuth   bool              `json:"removeGithubOAuth,omitempty"`
	DisablePasswordAuth *bool             `json:"disablePasswordAuth,omitempty"`
}

type GitHubOAuthPatch struct {
	ClientID     string   `json:"clientId,omitempty"`
	ClientSecret string   `json:"clientSecret,omitempty"`
	AllowedUsers []string `json:"allowedUsers,omitempty"`
	AllowedOrgs  []string `json:"allowedOrgs,omitempty"`
	AllowedTeams []string `json:"allowedTeams,omitempty"`
}

type AccessPatch struct {
	Admins               []string `json:"admins"`
	ViewRequiresTags     []string `json:"viewRequiresTags"`
	InteractRequiresTags []string `json:"interactRequiresTags"`
}

type IntegrationsPatch struct {
	Linear       []LinearIntegrationPatch       `json:"linear,omitempty"`
	Shortcut     []ShortcutIntegrationPatch     `json:"shortcut,omitempty"`
	GitHubIssues []GitHubIssuesIntegrationPatch `json:"githubIssues,omitempty"`
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

type GitHubIssuesIntegrationPatch struct {
	Workspace         string `json:"workspace"`
	OriginalWorkspace string `json:"originalWorkspace,omitempty"`
	Token             string `json:"token,omitempty"`
	WebhookSecret     string `json:"webhookSecret,omitempty"`
	Delete            bool   `json:"delete,omitempty"`
}

type FactoryPatch struct {
	Name                string                 `json:"name"`
	OriginalName        string                 `json:"originalName,omitempty"`
	Integration         string                 `json:"integration"`
	Workspace           string                 `json:"workspace"`
	Team                string                 `json:"team,omitempty"`
	TriggerStatus       string                 `json:"triggerStatus"`
	DoneStatus          string                 `json:"doneStatus,omitempty"`
	TerminateOnLeave    bool                   `json:"terminateOnLeave"`
	Template            string                 `json:"template"`
	NamePattern         string                 `json:"namePattern,omitempty"`
	WebhookSecret       string                 `json:"webhookSecret,omitempty"`
	WebhookSecretRef    string                 `json:"webhookSecretRef,omitempty"`
	PipelineYAML        string                 `json:"pipelineYAML,omitempty"`
	Tags                []string               `json:"tags,omitempty"`
	Color               string                 `json:"color,omitempty"`
	Labels              []string               `json:"labels,omitempty"`
	ExcludeLabels       []string               `json:"exclude_labels,omitempty"`
	AssignedTo          string                 `json:"assigned_to,omitempty"`
	Enabled             *bool                  `json:"enabled,omitempty"`
	ConcurrencyGroup    string                 `json:"concurrencyGroup,omitempty"`
	EnableManualTrigger bool                   `json:"enable_manual_trigger,omitempty"`
	SecretRefs          map[string]string      `json:"secret_refs,omitempty"`
	Inputs              []types.FactoryInput   `json:"inputs,omitempty"`
	Repos               []string               `json:"repos,omitempty"`
	Trigger             *types.GitHubTrigger   `json:"trigger,omitempty"`
	ExternalTrigger     *types.ExternalTrigger `json:"externalTrigger,omitempty"`
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
	// exe.dev
	DefaultCPU    int    `json:"defaultCpu,omitempty"`
	DefaultMemory string `json:"defaultMemory,omitempty"`
	DefaultDisk   string `json:"defaultDisk,omitempty"`
	// Docker
	Image   string `json:"image,omitempty"`
	Network string `json:"network,omitempty"`
	// AWS Lambda MicroVMs
	AWSRegion                  string   `json:"awsRegion,omitempty"`
	AWSProfile                 string   `json:"awsProfile,omitempty"`
	ImageIdentifier            string   `json:"imageIdentifier,omitempty"`
	ImageVersion               string   `json:"imageVersion,omitempty"`
	ExecutionRoleARN           string   `json:"executionRoleArn,omitempty"`
	IngressNetworkConnectors   []string `json:"ingressNetworkConnectors,omitempty"`
	EgressNetworkConnectors    []string `json:"egressNetworkConnectors,omitempty"`
	IdleMaxDurationSeconds     int      `json:"idleMaxDurationSeconds,omitempty"`
	SuspendedDurationSeconds   int      `json:"suspendedDurationSeconds,omitempty"`
	AutoResume                 *bool    `json:"autoResume,omitempty"`
	MaximumDurationSeconds     int      `json:"maximumDurationSeconds,omitempty"`
	MicroVMBridgePort          int      `json:"bridgePort,omitempty"`
	AuthTokenExpirationMinutes int      `json:"authTokenExpirationMinutes,omitempty"`
	// Delete removes this provider when true.
	Delete bool `json:"delete,omitempty"`
}

type GitHubAppPatch struct {
	AppID         int64  `json:"appId"`
	URL           string `json:"url,omitempty"`
	PrivateKeyPEM string `json:"privateKeyPem,omitempty"` // full PEM, stored on server only
}

func (s *Server) handleSettingsStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	hasProvider := hasConfiguredProvider(s.hubCfg.Providers)
	hasLLMKey := len(s.hubCfg.LLMKeys) > 0
	hasGitHub := len(s.hubCfg.GitHubApps) > 0
	s.mu.RUnlock()

	jsonOK(w, SettingsStatus{
		HasProvider: hasProvider,
		HasLLMKey:   hasLLMKey,
		HasGitHub:   hasGitHub,
	})
}

func hasConfiguredProvider(providers map[string]types.ProviderConfig) bool {
	for name, p := range providers {
		providerType := p.Type
		if providerType == "" {
			providerType = name
		}
		switch providerType {
		case "daytona":
			if p.APIKey != "" {
				return true
			}
		case "replicated":
			if p.Token != "" {
				return true
			}
		case "exedev", "docker":
			return true
		case "lambda-microvms":
			if p.ImageIdentifier != "" {
				return true
			}
		default:
			if p.Token != "" || p.APIKey != "" || p.AccessToken != "" {
				return true
			}
		}
	}
	return false
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
		DefaultOpenClawImage: cliversion.OpenClawImage,
		Providers:            make(map[string]ProviderView),
		GitHub:               []GitHubAppView{},
	}

	s.mu.RLock()
	var fireworksAPIKey string
	// LLM keys — mask actual key values
	view.LLMKeys = []LLMKeyView{}
	for _, k := range s.hubCfg.LLMKeys {
		if k.Provider == "fireworks" && k.APIKey != "" && fireworksAPIKey == "" {
			fireworksAPIKey = k.APIKey
		}
		view.LLMKeys = append(view.LLMKeys, LLMKeyView{
			Name:         k.Name,
			Provider:     k.Provider,
			KeySet:       llmKeyHasRequiredAPIKey(k),
			Default:      k.Default,
			DefaultModel: k.DefaultModel,
			AuthProfile:  k.AuthProfile,
		})
	}
	view.ModelAuthProfiles = []ModelAuthProfileView{}
	view.LifecycleEventTypes = append([]string(nil), types.LifecycleEventTypes...)
	view.Notifications = buildNotificationsView(s.hubCfg.Notifications)
	for _, profile := range s.hubCfg.ModelAuthProfiles {
		if profile == nil {
			continue
		}
		view.ModelAuthProfiles = append(view.ModelAuthProfiles, ModelAuthProfileView{
			Name:          profile.Name,
			Provider:      profile.Provider,
			Mode:          profile.Mode,
			Authenticated: profile.AuthState != "",
			UpdatedAt:     profile.UpdatedAt,
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
		case "exedev":
			pv.SSHKeySet = p.SSHKeyPath != ""
			pv.DefaultCPU = p.DefaultCPU
			pv.DefaultMemory = p.DefaultMemory
			pv.DefaultDisk = p.DefaultDisk
			// Record SSHKeyPath for file I/O outside the lock
			if p.SSHKeyPath != "" {
				pv._sshKeyPath = p.SSHKeyPath
			}
		case "docker":
			pv.Image = p.Image
			pv.Network = p.Network
		case "lambda-microvms":
			pv.AWSRegion = p.AWSRegion
			pv.AWSProfile = p.AWSProfile
			pv.ImageIdentifier = p.ImageIdentifier
			pv.ImageVersion = p.ImageVersion
			pv.ExecutionRoleARN = p.ExecutionRoleARN
			pv.IngressNetworkConnectors = append([]string(nil), p.IngressNetworkConnectors...)
			pv.EgressNetworkConnectors = append([]string(nil), p.EgressNetworkConnectors...)
			pv.IdleMaxDurationSeconds = p.IdleMaxDurationSeconds
			pv.SuspendedDurationSeconds = p.SuspendedDurationSeconds
			pv.AutoResume = p.AutoResume
			pv.MaximumDurationSeconds = p.MaximumDurationSeconds
			pv.MicroVMBridgePort = p.MicroVMBridgePort
			pv.AuthTokenExpirationMinutes = p.AuthTokenExpirationMinutes
		}
		view.Providers[name] = pv
	}

	// SSH public keys
	view.SSHPublicKeys = s.hubCfg.SSHPublicKeys
	if view.SSHPublicKeys == nil {
		view.SSHPublicKeys = []string{}
	}

	// GitHub Apps — copy configs under lock, check permissions after releasing lock
	ghAppCfgs := make([]*types.GitHubAppConfig, len(s.hubCfg.GitHubApps))
	for i, app := range s.hubCfg.GitHubApps {
		copy := *app
		ghAppCfgs[i] = &copy
	}

	// Integrations
	view.Integrations = &IntegrationsView{Linear: []LinearIntegrationView{}, Shortcut: []ShortcutIntegrationView{}, GitHubIssues: []GitHubIssuesIntegrationView{}}
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
		for _, gi := range s.hubCfg.Integrations.GitHubIssues {
			view.Integrations.GitHubIssues = append(view.Integrations.GitHubIssues, GitHubIssuesIntegrationView{
				Workspace:        gi.Workspace,
				TokenSet:         gi.Token != "",
				WebhookSecretSet: gi.WebhookSecret != "",
			})
		}
	}

	// Concurrency limit
	view.MaxConcurrentClaws = s.hubCfg.MaxConcurrentClaws

	// Concurrency groups — ensure "global" always exists
	view.ConcurrencyGroups = []ConcurrencyGroupView{}
	hasGlobal := false
	for _, g := range s.hubCfg.ConcurrencyGroups {
		view.ConcurrencyGroups = append(view.ConcurrencyGroups, ConcurrencyGroupView{
			Name:  g.Name,
			Limit: g.Limit,
		})
		if g.Name == "global" {
			hasGlobal = true
		}
	}
	if !hasGlobal {
		view.ConcurrencyGroups = append(view.ConcurrencyGroups, ConcurrencyGroupView{
			Name:  "global",
			Limit: s.hubCfg.MaxConcurrentClaws,
		})
	}

	// Factories are retired; keep an empty slice for API compatibility.
	view.Factories = []FactoryView{}
	// Auth config — copy under lock before RUnlock
	var authView *AuthView
	if s.hubCfg.Auth != nil {
		authView = &AuthView{
			DisablePasswordAuth: s.hubCfg.Auth.DisablePasswordAuth,
		}
		if s.hubCfg.Auth.GitHubOAuth != nil {
			gh := s.hubCfg.Auth.GitHubOAuth
			authView.GitHubOAuth = &GitHubOAuthView{
				ClientID:        gh.ClientID,
				ClientSecretSet: gh.ClientSecret != "",
				AllowedUsers:    append([]string(nil), gh.AllowedUsers...),
				AllowedOrgs:     append([]string(nil), gh.AllowedOrgs...),
				AllowedTeams:    append([]string(nil), gh.AllowedTeams...),
			}
		}
		if s.hubCfg.Auth.Access != nil {
			acc := s.hubCfg.Auth.Access
			authView.Access = &AccessView{
				Admins:               append([]string(nil), acc.Admins...),
				ViewRequiresTags:     append([]string(nil), acc.ViewRequiresTags...),
				InteractRequiresTags: append([]string(nil), acc.InteractRequiresTags...),
			}
		}
	}

	s.mu.RUnlock()

	view.Auth = authView
	view.ModelOptions = map[string][]LLMModelOption{
		"fireworks": s.fireworksModelOptions(r.Context(), fireworksAPIKey),
	}

	// Read SSH public keys outside the lock (file I/O)
	for name, pv := range view.Providers {
		if pv._sshKeyPath != "" {
			if pubBytes, err := os.ReadFile(pv._sshKeyPath + ".pub"); err == nil {
				pv.SSHPublicKey = string(pubBytes)
				view.Providers[name] = pv
			}
		}
	}

	// Check GitHub App permissions outside the lock (network call)
	for _, app := range ghAppCfgs {
		view.GitHub = append(view.GitHub, s.buildGitHubAppView(r.Context(), app))
	}

	// Secrets — names only, read from disk so manually-edited hub.yaml entries are visible
	view.Secrets = []string{}
	if diskCfg, err := config.LoadHubConfig(); err == nil && diskCfg != nil {
		for k := range diskCfg.Secrets {
			view.Secrets = append(view.Secrets, k)
		}
	}

	// MCP Servers — redacted view, read from disk
	view.MCPServers = []MCPView{}
	if diskCfg, err := config.LoadHubConfig(); err == nil && diskCfg != nil {
		for _, mcp := range diskCfg.MCPServers {
			if mcp == nil {
				continue
			}
			mcpView := MCPView{
				Name:    mcp.Name,
				Source:  string(mcp.Source),
				Package: mcp.Package,
				Image:   mcp.Image,
				URL:     mcp.URL,
				Enabled: mcp.Enabled,
				Config:  mcp.Config,
				Command: mcp.Command,
			}
			if mcp.Secrets != nil {
				for envVar := range mcp.Secrets {
					mcpView.Secrets = append(mcpView.Secrets, envVar)
				}
			}
			view.MCPServers = append(view.MCPServers, mcpView)
		}
	}

	jsonOK(w, view)
}

func buildNotificationsView(cfg *types.NotificationsConfig) *NotificationsView {
	if cfg == nil {
		return &NotificationsView{Notifiers: map[string]NotifierView{}}
	}
	view := &NotificationsView{Notifiers: make(map[string]NotifierView, len(cfg.Notifiers))}
	for name, notifier := range cfg.Notifiers {
		view.Notifiers[name] = NotifierView{
			Type:            notifier.Type,
			Channel:         notifierSettingString(notifier, "channel"),
			TokenSecret:     notifierSettingString(notifier, "token_secret"),
			APIBase:         notifierSettingString(notifier, "api_base"),
			MinSendInterval: notifierSettingString(notifier, "min_send_interval"),
		}
	}
	if lc := cfg.Lifecycle; lc != nil {
		lifecycle := &LifecycleNotificationsView{
			Enabled:      lc.IsEnabled(),
			Via:          lc.Via,
			Routes:       make([]types.LifecycleRoute, len(lc.Routes)),
			PollInterval: lc.PollInterval,
			IdleAfter:    lc.IdleAfter,
		}
		for i, route := range lc.Routes {
			lifecycle.Routes[i] = types.LifecycleRoute{Via: route.Via, Events: append([]string(nil), route.Events...)}
		}
		if lc.Events != nil {
			events := *lc.Events
			lifecycle.Events = &events
		}
		view.Lifecycle = lifecycle
	}
	return view
}

func notifierSettingString(notifier types.NotifierConfig, key string) string {
	value, _ := notifier.Settings[key].(string)
	return value
}

// mergeNotifierSettings folds a patched notifier's settings over the ones
// already in the config. GET /api/settings projects only a handful of settings
// keys (see buildNotificationsView) and the settings screen rebuilds the whole
// notifications block from that projection, so replacing the block outright
// would silently drop every other key under notifications.notifiers.<name> the
// first time an operator touches the screen.
func mergeNotifierSettings(current, patch *types.NotificationsConfig) {
	if current == nil || patch == nil {
		return
	}
	for name, patched := range patch.Notifiers {
		existing, ok := current.Notifiers[name]
		if !ok || len(existing.Settings) == 0 {
			continue
		}
		merged := make(map[string]any, len(existing.Settings)+len(patched.Settings))
		for key, value := range existing.Settings {
			merged[key] = value
		}
		for key, value := range patched.Settings {
			merged[key] = value
		}
		patched.Settings = merged
		patch.Notifiers[name] = patched
	}
}

// validateSettingsNotifications checks the patched notifications block. The
// provider-level "can this be built" check runs only on notifiers the patch
// actually adds or changes: load-time validation (types.ValidateNotificationsConfig)
// accepts a notifier this build refuses to construct, so a hub.yaml written by
// hand — or by an older build — can hold one and run fine, logging "notifier
// unavailable" per tick. The settings screen submits the whole notifier map on
// every save, so failing the patch on such an entry would 400 every save from
// the screen, including saves that touch an entirely different channel, with
// no way to repair the offender from the UI. Whatever the operator does touch
// is still checked, so this handler never persists a NEW broken notifier.
func validateSettingsNotifications(current, cfg *types.NotificationsConfig) error {
	if err := types.ValidateNotificationsConfig(cfg); err != nil {
		return err
	}
	for name, notifier := range cfg.Notifiers {
		// Checked before the unchanged short-circuit and against the stored
		// value key by key, so editing a notifier that already carries an
		// api_base stays possible while introducing or changing one does not.
		if err := validateNotifierAPIBase(current, name, notifier); err != nil {
			return err
		}
		if notifierUnchanged(current, name, notifier) {
			continue
		}
		if !notify.Supported(notifier.Type) {
			return fmt.Errorf("notifications.notifiers.%s: unsupported notifier type %q", name, notifier.Type)
		}
		// The type's own required fields, checked here rather than at the first
		// send: a notifier that cannot be built delivers nothing and says so
		// only in the hub log, so a PATCH that persists one silently mutes
		// every route pointing at it.
		if err := notify.ValidateConfig(notifier.Type, notifier.Settings); err != nil {
			return fmt.Errorf("notifications.notifiers.%s: %w", name, err)
		}
	}
	return nil
}

// validateNotifierAPIBase refuses an api_base the patch introduces or changes
// unless it addresses Slack over https. api_base decides where the notifier's
// bot token is sent: a patch that keeps token_secret and repoints api_base at
// an attacker-controlled host turns the next test send into token
// exfiltration (and every send into an SSRF probe from the hub). The stored
// value is exempt — an operator who wrote an internal Slack proxy into
// hub.yaml keeps it, and the settings screen, which round-trips api_base
// without rendering it, keeps saving — because hub.yaml is the operator's own
// file while this handler is remote input. Repointing a live notifier is
// therefore a hub.yaml edit, not an API call.
func validateNotifierAPIBase(current *types.NotificationsConfig, name string, patched types.NotifierConfig) error {
	apiBase := strings.TrimSpace(notifierSettingString(patched, "api_base"))
	if apiBase == "" {
		return nil
	}
	if current != nil {
		if existing, ok := current.Notifiers[name]; ok && strings.TrimSpace(notifierSettingString(existing, "api_base")) == apiBase {
			return nil
		}
	}
	u, err := url.Parse(apiBase)
	if err != nil {
		return fmt.Errorf("notifications.notifiers.%s: api_base %q is not a valid URL", name, apiBase)
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || (host != "slack.com" && !strings.HasSuffix(host, ".slack.com")) {
		return fmt.Errorf("notifications.notifiers.%s: api_base %q must be an https URL on slack.com — the notifier's bot token is sent to this host, so it cannot be repointed through the settings API", name, apiBase)
	}
	return nil
}

// dropRejectedLifecycleDurations clears a lifecycle poll_interval or
// idle_after that the patch re-sends unchanged from the stored config and that
// validation would reject anyway. Both are validated on every patch, floors
// included and regardless of `enabled`, while the settings screen rebuilds the
// whole notifications block from a view that renders neither field: a hub.yaml
// holding "idle_after: 30s" would otherwise 400 every save made from that
// screen — the master switch, a category toggle, adding a channel — on a value
// the screen can neither show nor clear, leaving a hand edit of hub.yaml as the
// only way out. The offender is dropped rather than preserved because it is
// already inert: lifecycleNotifierTick refuses to run while the config fails
// validation, so keeping it would trade an unusable screen for a healthy-
// looking screen that still delivers nothing. A value the patch itself
// introduces or edits is left alone and still fails the save.
func dropRejectedLifecycleDurations(current, patch *types.NotificationsConfig) {
	if patch == nil || patch.Lifecycle == nil || current == nil || current.Lifecycle == nil {
		return
	}
	stored, lc := current.Lifecycle, patch.Lifecycle
	if lc.PollInterval == stored.PollInterval && lifecycleDurationRejected(&types.LifecycleNotificationsConfig{PollInterval: lc.PollInterval}) {
		lc.PollInterval = ""
	}
	if lc.IdleAfter == stored.IdleAfter && lifecycleDurationRejected(&types.LifecycleNotificationsConfig{IdleAfter: lc.IdleAfter}) {
		lc.IdleAfter = ""
	}
}

// lifecycleDurationRejected asks the real validator whether a lone duration
// passes, so the floors live in exactly one place. The probe is disabled
// explicitly: both durations are validated above the `if !lc.IsEnabled()`
// short-circuit, while an ENABLED probe carries neither `via` nor routes and
// would be rejected for that instead — reporting every value, valid ones
// included, as rejected and silently erasing the stored durations on every save.
func lifecycleDurationRejected(probe *types.LifecycleNotificationsConfig) bool {
	probe.Enabled = new(bool)
	return types.ValidateNotificationsConfig(&types.NotificationsConfig{Lifecycle: probe}) != nil
}

// notifierUnchanged reports whether the patched notifier is byte-identical to
// the one already stored under that name — the patch is re-sending it, not
// editing it. Compared after mergeNotifierSettings, so a patch that only omits
// keys the settings view does not project still counts as unchanged.
func notifierUnchanged(current *types.NotificationsConfig, name string, patched types.NotifierConfig) bool {
	if current == nil {
		return false
	}
	existing, ok := current.Notifiers[name]
	if !ok {
		return false
	}
	return existing.Type == patched.Type && reflect.DeepEqual(existing.Settings, patched.Settings)
}

// buildGitHubAppView builds a GitHubAppView for the settings page, including
// a live permission check if the private key is set.
func (s *Server) buildGitHubAppView(ctx context.Context, app *types.GitHubAppConfig) GitHubAppView {
	view := GitHubAppView{
		AppID:  app.AppID,
		URL:    app.URL,
		KeySet: app.PrivateKeyPEM != "",
	}
	if !view.KeySet {
		view.PermCheckError = "Private key is required to test permissions"
		return view
	}

	provider, err := NewGitHubTokenProvider(app)
	if err != nil {
		view.PermCheckError = fmt.Sprintf("invalid key: %v", err)
		return view
	}

	perms, err := provider.CheckAppPermissions(ctx)
	if err != nil {
		view.PermCheckError = err.Error()
		return view
	}

	// Permissions ElasticClaw needs from the GitHub App
	required := []struct {
		name   string
		needed string
	}{
		{"contents", "write"},
		{"pull_requests", "write"},
		{"metadata", "read"},
		{"checks", "read"},
		{"statuses", "read"},
	}

	allOK := true
	for _, req := range required {
		granted := perms[req.name]
		ok := granted == req.needed || (req.needed == "read" && (granted == "read" || granted == "write"))
		if !ok {
			allOK = false
		}
		view.Permissions = append(view.Permissions, GitHubAppPermission{
			Name:    req.name,
			Granted: granted,
			Needed:  req.needed,
			OK:      ok,
		})
	}
	view.PermCheckOK = &allOK
	return view
}

// handleGitHubAppTest tests a GitHub App's permissions without saving it.
func (s *Server) handleGitHubAppTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AppID         int64  `json:"appId"`
		URL           string `json:"url,omitempty"`
		PrivateKeyPEM string `json:"privateKeyPem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.AppID == 0 || req.PrivateKeyPEM == "" {
		http.Error(w, "appId and privateKeyPem required", http.StatusBadRequest)
		return
	}

	app := &types.GitHubAppConfig{
		AppID:         req.AppID,
		URL:           req.URL,
		PrivateKeyPEM: req.PrivateKeyPEM,
	}
	view := s.buildGitHubAppView(r.Context(), app)
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

	if patch.Notifications != nil {
		// A settings patch is the migration point from legacy lifecycle.via
		// to routes: a supplied route set always wins and is written alone.
		// Gated on a NON-EMPTY route set, never on a non-nil slice:
		// LifecycleNotificationsView always emits `routes` (as `[]` for a
		// via-only config), so a client that GETs the view and PATCHes it back
		// verbatim — the round trip the view's doc comment invites — would
		// otherwise decode to an empty-but-present slice and silently destroy
		// the only channel binding the hub has.
		if patch.Notifications.Lifecycle != nil && len(patch.Notifications.Lifecycle.Routes) > 0 {
			patch.Notifications.Lifecycle.Via = ""
		}
		mergeNotifierSettings(s.hubCfg.Notifications, patch.Notifications)
		dropRejectedLifecycleDurations(s.hubCfg.Notifications, patch.Notifications)
		if err := validateSettingsNotifications(s.hubCfg.Notifications, patch.Notifications); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// s.hubCfg.Factories is read directly: resolveFactories takes the lock
		// this handler already holds.
		if err := validateNotifierRemovals(s.hubCfg.Notifications, patch.Notifications, s.hubCfg.Factories); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		updatedCfg.Notifications = patch.Notifications
	}

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
			// Find existing by name (or newName if renaming)
			lookupName := kp.Name
			var found *types.LLMKeyConfig
			for _, k := range existing {
				if k.Name == lookupName {
					found = k
					break
				}
			}
			if found == nil {
				found = &types.LLMKeyConfig{Name: kp.Name}
				existing = append(existing, found)
			}
			if kp.NewName != "" && kp.NewName != found.Name {
				found.Name = kp.NewName
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
			if kp.AuthProfile != nil {
				found.AuthProfile = *kp.AuthProfile
			}
		}
		updatedCfg.LLMKeys = existing
	}

	if len(patch.ModelAuthProfiles) > 0 {
		existing := make([]*types.ModelAuthProfileConfig, len(updatedCfg.ModelAuthProfiles))
		for i, p := range updatedCfg.ModelAuthProfiles {
			if p == nil {
				continue
			}
			copy := *p
			existing[i] = &copy
		}
		for _, pp := range patch.ModelAuthProfiles {
			if pp.Name == "" {
				continue
			}
			if pp.Delete {
				filtered := existing[:0]
				for _, p := range existing {
					if p == nil || p.Name != pp.Name {
						filtered = append(filtered, p)
					}
				}
				existing = filtered
				continue
			}
			var found *types.ModelAuthProfileConfig
			for _, p := range existing {
				if p != nil && p.Name == pp.Name {
					found = p
					break
				}
			}
			if found == nil {
				found = &types.ModelAuthProfileConfig{Name: pp.Name}
				existing = append(existing, found)
			}
			if pp.Provider != "" {
				found.Provider = pp.Provider
			}
			if pp.Mode != "" {
				found.Mode = pp.Mode
			}
		}
		updatedCfg.ModelAuthProfiles = existing
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
			if pp.Delete {
				delete(updatedCfg.Providers, name)
				continue
			}
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
			case "exedev":
				// Auto-generate SSH key if not already present
				if existing.SSHKeyPath == "" {
					_, privPath, err := GenerateExedevKey(filepath.Join(hubConfigDir(), "keys"))
					if err != nil {
						log.Printf("failed to generate exedev key: %v", err)
					} else {
						existing.SSHKeyPath = privPath
					}
				}
				if pp.DefaultCPU > 0 {
					existing.DefaultCPU = pp.DefaultCPU
				}
				if pp.DefaultMemory != "" {
					existing.DefaultMemory = pp.DefaultMemory
				}
				if pp.DefaultDisk != "" {
					existing.DefaultDisk = pp.DefaultDisk
				}
			case "docker":
				if pp.Image != "" {
					existing.Image = pp.Image
				}
				if pp.Network != "" {
					existing.Network = pp.Network
				}
			case "lambda-microvms":
				if pp.AWSRegion != "" {
					existing.AWSRegion = pp.AWSRegion
				}
				if pp.AWSProfile != "" {
					existing.AWSProfile = pp.AWSProfile
				}
				if pp.ImageIdentifier != "" {
					existing.ImageIdentifier = pp.ImageIdentifier
				}
				if pp.ImageVersion != "" {
					existing.ImageVersion = pp.ImageVersion
				}
				if pp.ExecutionRoleARN != "" {
					existing.ExecutionRoleARN = pp.ExecutionRoleARN
				}
				if pp.IngressNetworkConnectors != nil {
					existing.IngressNetworkConnectors = append([]string(nil), pp.IngressNetworkConnectors...)
				}
				if pp.EgressNetworkConnectors != nil {
					existing.EgressNetworkConnectors = append([]string(nil), pp.EgressNetworkConnectors...)
				}
				if pp.IdleMaxDurationSeconds > 0 {
					existing.IdleMaxDurationSeconds = pp.IdleMaxDurationSeconds
				}
				if pp.SuspendedDurationSeconds > 0 {
					existing.SuspendedDurationSeconds = pp.SuspendedDurationSeconds
				}
				if pp.AutoResume != nil {
					existing.AutoResume = pp.AutoResume
				}
				if pp.MaximumDurationSeconds > 0 {
					existing.MaximumDurationSeconds = pp.MaximumDurationSeconds
				}
				if pp.MicroVMBridgePort > 0 {
					existing.MicroVMBridgePort = pp.MicroVMBridgePort
				}
				if pp.AuthTokenExpirationMinutes > 0 {
					existing.AuthTokenExpirationMinutes = pp.AuthTokenExpirationMinutes
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

		// Preserve existing Shortcut and GitHub Issues integrations not in patch
		if existingIntegrations != nil {
			updatedCfg.Integrations.Shortcut = existingIntegrations.Shortcut
			updatedCfg.Integrations.GitHubIssues = existingIntegrations.GitHubIssues
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
				Linear:       existingIntegrations.Linear,
				Shortcut:     existingIntegrations.Shortcut,
				GitHubIssues: existingIntegrations.GitHubIssues,
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

	if patch.Integrations != nil && patch.Integrations.GitHubIssues != nil {
		var existingIntegrations *types.IntegrationsConfig
		if updatedCfg.Integrations != nil {
			existingIntegrations = updatedCfg.Integrations
		}
		if updatedCfg.Integrations == nil {
			updatedCfg.Integrations = &types.IntegrationsConfig{}
		} else {
			updatedCfg.Integrations = &types.IntegrationsConfig{
				Linear:       existingIntegrations.Linear,
				Shortcut:     existingIntegrations.Shortcut,
				GitHubIssues: existingIntegrations.GitHubIssues,
			}
		}
		existing := updatedCfg.Integrations.GitHubIssues
		var githubIssues []*types.GitHubIssuesIntegrationConfig
		for _, gp := range patch.Integrations.GitHubIssues {
			if gp.Delete {
				continue
			}
			gi := &types.GitHubIssuesIntegrationConfig{Workspace: gp.Workspace}
			matchWorkspace := gp.Workspace
			if gp.OriginalWorkspace != "" {
				matchWorkspace = gp.OriginalWorkspace
			}
			for _, ex := range existing {
				if ex.Workspace == matchWorkspace {
					gi.Token = ex.Token
					gi.WebhookSecret = ex.WebhookSecret
					break
				}
			}
			if gp.Token != "" {
				gi.Token = gp.Token
			}
			if gp.WebhookSecret != "" {
				gi.WebhookSecret = gp.WebhookSecret
			}
			githubIssues = append(githubIssues, gi)
		}
		updatedCfg.Integrations.GitHubIssues = githubIssues
	}

	// MaxConcurrentClaws
	if patch.MaxConcurrentClaws != nil {
		updatedCfg.MaxConcurrentClaws = *patch.MaxConcurrentClaws
	}

	// ConcurrencyGroups (full replace)
	if patch.ConcurrencyGroups != nil {
		var groups []*types.ConcurrencyGroup
		for _, gp := range *patch.ConcurrencyGroups {
			if gp.Delete {
				continue
			}
			groups = append(groups, &types.ConcurrencyGroup{
				Name:  gp.Name,
				Limit: gp.Limit,
			})
		}
		updatedCfg.ConcurrencyGroups = groups
	}

	// Factories are retired. Settings must not write factories/ or hubCfg.Factories.
	// Non-empty patches are rejected so the UI cannot reintroduce dual-fire automations.
	if patch.Factories != nil && len(patch.Factories) > 0 {
		http.Error(w, errFactoriesRetired.Error(), http.StatusGone)
		return
	}
	if patch.Factories != nil {
		updatedCfg.Factories = nil
	}

	// Auth config
	if patch.Auth != nil {
		if updatedCfg.Auth != nil {
			authCopy := *updatedCfg.Auth
			if authCopy.GitHubOAuth != nil {
				githubOAuthCopy := *authCopy.GitHubOAuth
				githubOAuthCopy.AllowedUsers = append([]string(nil), authCopy.GitHubOAuth.AllowedUsers...)
				githubOAuthCopy.AllowedOrgs = append([]string(nil), authCopy.GitHubOAuth.AllowedOrgs...)
				githubOAuthCopy.AllowedTeams = append([]string(nil), authCopy.GitHubOAuth.AllowedTeams...)
				authCopy.GitHubOAuth = &githubOAuthCopy
			}
			if authCopy.Access != nil {
				accessCopy := *authCopy.Access
				accessCopy.Admins = append([]string(nil), authCopy.Access.Admins...)
				accessCopy.ViewRequiresTags = append([]string(nil), authCopy.Access.ViewRequiresTags...)
				accessCopy.InteractRequiresTags = append([]string(nil), authCopy.Access.InteractRequiresTags...)
				authCopy.Access = &accessCopy
			}
			updatedCfg.Auth = &authCopy
		}
		if patch.Auth.RemoveGitHubOAuth {
			if updatedCfg.Auth != nil {
				updatedCfg.Auth.GitHubOAuth = nil
				updatedCfg.Auth.DisablePasswordAuth = false
				if updatedCfg.Auth.Access == nil {
					updatedCfg.Auth = nil
				}
			}
		} else if patch.Auth.GitHubOAuth != nil {
			if updatedCfg.Auth == nil {
				updatedCfg.Auth = &types.AuthConfig{}
			}
			if updatedCfg.Auth.GitHubOAuth == nil {
				updatedCfg.Auth.GitHubOAuth = &types.GitHubOAuthConfig{}
			}
			gh := patch.Auth.GitHubOAuth
			if gh.ClientID != "" {
				updatedCfg.Auth.GitHubOAuth.ClientID = gh.ClientID
			}
			if gh.ClientSecret != "" {
				updatedCfg.Auth.GitHubOAuth.ClientSecret = gh.ClientSecret
			}
			if gh.AllowedUsers != nil {
				updatedCfg.Auth.GitHubOAuth.AllowedUsers = gh.AllowedUsers
			}
			if gh.AllowedOrgs != nil {
				updatedCfg.Auth.GitHubOAuth.AllowedOrgs = gh.AllowedOrgs
			}
			if gh.AllowedTeams != nil {
				updatedCfg.Auth.GitHubOAuth.AllowedTeams = gh.AllowedTeams
			}
		}
		if patch.Auth.DisablePasswordAuth != nil {
			if updatedCfg.Auth == nil {
				updatedCfg.Auth = &types.AuthConfig{}
			}
			updatedCfg.Auth.DisablePasswordAuth = *patch.Auth.DisablePasswordAuth
		}
		if patch.Auth.Access != nil {
			if updatedCfg.Auth == nil {
				updatedCfg.Auth = &types.AuthConfig{}
			}
			// Use patch values if provided; otherwise preserve existing
			var admins []string
			if patch.Auth.Access.Admins != nil {
				admins = patch.Auth.Access.Admins
			} else if updatedCfg.Auth.Access != nil {
				admins = append([]string(nil), updatedCfg.Auth.Access.Admins...)
			}
			var viewRequiresTags []string
			if patch.Auth.Access.ViewRequiresTags != nil {
				viewRequiresTags = patch.Auth.Access.ViewRequiresTags
			} else if updatedCfg.Auth.Access != nil {
				viewRequiresTags = append([]string(nil), updatedCfg.Auth.Access.ViewRequiresTags...)
			}
			var interactRequiresTags []string
			if patch.Auth.Access.InteractRequiresTags != nil {
				interactRequiresTags = patch.Auth.Access.InteractRequiresTags
			} else if updatedCfg.Auth.Access != nil {
				interactRequiresTags = append([]string(nil), updatedCfg.Auth.Access.InteractRequiresTags...)
			}
			updatedCfg.Auth.Access = &types.AccessConfig{
				Admins:               admins,
				ViewRequiresTags:     viewRequiresTags,
				InteractRequiresTags: interactRequiresTags,
			}
			if len(updatedCfg.Auth.Access.Admins) == 0 && len(updatedCfg.Auth.Access.ViewRequiresTags) == 0 && len(updatedCfg.Auth.Access.InteractRequiresTags) == 0 {
				updatedCfg.Auth.Access = nil
				if updatedCfg.Auth.GitHubOAuth == nil {
					updatedCfg.Auth = nil
				}
			}
		}
	}

	// MCP Servers — merge patch entries into existing list
	if patch.MCPServers != nil {
		// Build a map of patch entries by name for quick lookup
		patchByName := make(map[string]MCPPatch, len(patch.MCPServers))
		for _, mp := range patch.MCPServers {
			patchByName[mp.Name] = mp
		}

		var mcps []*types.MCPServerHubConfig
		// Keep existing entries not mentioned in patch; update or delete those that are
		for _, existing := range s.hubCfg.MCPServers {
			mp, inPatch := patchByName[existing.Name]
			if !inPatch {
				// Not in patch — preserve unchanged
				mcps = append(mcps, existing)
				continue
			}
			if mp.Delete {
				// Delete — drop this entry
				continue
			}
			// Update existing entry with patched fields
			updated := &types.MCPServerHubConfig{
				Name:    existing.Name,
				Source:  existing.Source,
				Package: existing.Package,
				Image:   existing.Image,
				URL:     existing.URL,
				Enabled: existing.Enabled,
				Config:  existing.Config,
				Secrets: existing.Secrets,
				Command: existing.Command,
			}
			if mp.Source != "" {
				updated.Source = types.MCPSource(mp.Source)
			}
			if mp.Package != "" {
				updated.Package = mp.Package
			}
			if mp.Image != "" {
				updated.Image = mp.Image
			}
			if mp.URL != "" {
				updated.URL = mp.URL
			}
			if mp.Enabled != nil {
				updated.Enabled = *mp.Enabled
			}
			if mp.Config != nil {
				updated.Config = mp.Config
			}
			if mp.Command != nil {
				updated.Command = mp.Command
			}
			if mp.Secrets != nil {
				updated.Secrets = mp.Secrets
			}
			mcps = append(mcps, updated)
		}
		// Add new entries (names not found in existing)
		for _, mp := range patch.MCPServers {
			if mp.Delete {
				continue
			}
			// Check if this name already exists (already handled above)
			found := false
			for _, existing := range s.hubCfg.MCPServers {
				if existing.Name == mp.Name {
					found = true
					break
				}
			}
			if found {
				continue
			}
			// Validate new entry
			if mp.Source == "" {
				http.Error(w, "source required for mcp "+mp.Name, http.StatusBadRequest)
				return
			}
			switch types.MCPSource(mp.Source) {
			case types.MCPSourceNpx, types.MCPSourceUvx, types.MCPSourceSmithery:
				if mp.Package == "" {
					http.Error(w, "package required for mcp "+mp.Name+" source "+mp.Source, http.StatusBadRequest)
					return
				}
			case types.MCPSourceDocker:
				if mp.Image == "" {
					http.Error(w, "image required for mcp "+mp.Name+" source docker", http.StatusBadRequest)
					return
				}
			case types.MCPSourceSSE:
				if mp.URL == "" {
					http.Error(w, "url required for mcp "+mp.Name+" source sse", http.StatusBadRequest)
					return
				}
			default:
				http.Error(w, "invalid mcp source for "+mp.Name+": must be npx, uvx, smithery, docker, or sse", http.StatusBadRequest)
				return
			}
			mcp := &types.MCPServerHubConfig{
				Name:    mp.Name,
				Source:  types.MCPSource(mp.Source),
				Package: mp.Package,
				Image:   mp.Image,
				URL:     mp.URL,
				Config:  mp.Config,
				Command: mp.Command,
			}
			if mp.Enabled != nil {
				mcp.Enabled = *mp.Enabled
			} else {
				mcp.Enabled = true
			}
			if mp.Secrets != nil {
				mcp.Secrets = mp.Secrets
			}
			mcps = append(mcps, mcp)
		}
		updatedCfg.MCPServers = mcps
	}

	// Save to disk before applying to in-memory config
	if err := config.SaveHubConfig(&updatedCfg); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Only update in-memory config after successful disk write
	s.hubCfg = &updatedCfg

	// If the concurrency limit was raised or removed, try to promote pending claws.
	// This must run AFTER s.hubCfg is updated so promotePendingClaws reads the new limit.
	if patch.MaxConcurrentClaws != nil {
		go s.promotePendingClaws()
	}

	jsonOK(w, map[string]bool{"ok": true})
}
