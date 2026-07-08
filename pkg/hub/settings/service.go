// Package settings implements the hub's configuration surface: the
// /api/settings endpoints (redacted views and patches of hub.yaml), the AI
// config assistant (/api/ai-config*), CLI model-auth device logins
// (/api/model-auth*), and the /api/doctor diagnostics report.
//
// The package was extracted mechanically from pkg/hub (settings.go,
// ai_config.go, model_auth.go and doctor.go) as part of the phase-2 hub
// reorganization; behavior is unchanged. It shares the hub's config mutex and
// model-auth job state through injected hooks so it does not import pkg/hub
// (which would create an import cycle).
package settings

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// GitHubPermissionsChecker is the slice of the hub's GitHubTokenProvider that
// the settings page needs to test a GitHub App's permissions.
type GitHubPermissionsChecker interface {
	CheckAppPermissions(ctx context.Context) (map[string]string, error)
}

// Deps carries the hub-owned state and helpers the settings service needs.
// Everything is injected so the package does not depend on pkg/hub.
type Deps struct {
	// Mu is the hub's config mutex. It must be the same mutex the hub uses
	// to guard its live *types.HubConfig so config reads/writes keep the
	// exact same synchronization as before the extraction.
	Mu *sync.RWMutex
	// DB is the hub database (used by doctor's legacy template lookup).
	DB *sql.DB
	// HubCfg reads the hub's live config; SetHubCfg replaces it. Both are
	// called with Mu held, mirroring the pre-extraction field access.
	HubCfg    func() *types.HubConfig
	SetHubCfg func(*types.HubConfig)

	// Logf / LogfCtx are the printf-style slog bridges injected by the hub
	// so log lines keep the same format and component attribution.
	Logf    func(format string, args ...any)
	LogfCtx func(ctx context.Context, format string, args ...any)

	// PromotePendingClaws is invoked after the concurrency limit changes.
	PromotePendingClaws func()
	// FireworksModelOptions lists the Fireworks model options for the
	// settings page (hub-owned cache).
	FireworksModelOptions func(ctx context.Context, apiKey string) []LLMModelOption
	// NewGitHubPermissionsChecker wraps the hub's NewGitHubTokenProvider.
	NewGitHubPermissionsChecker func(app *types.GitHubAppConfig) (GitHubPermissionsChecker, error)
	// GenerateSSHKey wraps the hub's GenerateExedevKey.
	GenerateSSHKey func(dir string) (pubKey string, privPath string, err error)

	// External storage helpers (factories and templates live next to hub.yaml).
	HubConfigDir          func() string
	TemplatesDir          func() string
	LoadExternalFactories func() ([]*types.FactoryConfig, error)
	LoadExternalFactory   func(name string) (*types.FactoryConfig, error)
	SaveExternalFactory   func(f *types.FactoryConfig) error
	DeleteExternalFactory func(name string) error
	ListExternalTemplates func() ([]string, error)
	LoadExternalTemplate  func(name string) (map[string]string, error)

	// ResolveDefaultModelForKey mirrors the hub's runtime model resolution
	// (used by doctor to avoid false positives).
	ResolveDefaultModelForKey func(cfg *types.HubConfig, key *types.LLMKeyConfig) string

	// ModelAuthJobsMu guards the model-auth login job map; ModelAuthJobs
	// returns the (lazily initialized) map and must be called with
	// ModelAuthJobsMu held. The state stays on the hub's Server so
	// hand-built test servers keep working.
	ModelAuthJobsMu *sync.Mutex
	ModelAuthJobs   func() map[string]*ModelAuthLoginJob
}

// Service owns the settings, AI config, model-auth, and doctor handlers. It
// is cheap to construct; all mutable state lives on the hub side behind the
// injected hooks.
type Service struct {
	mu                          *sync.RWMutex
	db                          *sql.DB
	hubCfg                      func() *types.HubConfig
	setHubCfg                   func(*types.HubConfig)
	logf                        func(format string, args ...any)
	logfCtx                     func(ctx context.Context, format string, args ...any)
	promotePendingClaws         func()
	fireworksModelOptions       func(ctx context.Context, apiKey string) []LLMModelOption
	newGitHubPermissionsChecker func(app *types.GitHubAppConfig) (GitHubPermissionsChecker, error)
	generateSSHKey              func(dir string) (pubKey string, privPath string, err error)
	hubConfigDir                func() string
	templatesDir                func() string
	loadExternalFactories       func() ([]*types.FactoryConfig, error)
	loadExternalFactory         func(name string) (*types.FactoryConfig, error)
	saveExternalFactory         func(f *types.FactoryConfig) error
	deleteExternalFactory       func(name string) error
	listExternalTemplates       func() ([]string, error)
	loadExternalTemplate        func(name string) (map[string]string, error)
	resolveDefaultModelForKey   func(cfg *types.HubConfig, key *types.LLMKeyConfig) string
	modelAuthJobsMu             *sync.Mutex
	modelAuthJobs               func() map[string]*ModelAuthLoginJob
}

// New creates a Service. Logf and LogfCtx may be nil; safe defaults are
// installed so hand-built test servers keep working.
func New(deps Deps) *Service {
	if deps.Logf == nil {
		deps.Logf = func(format string, args ...any) {
			slog.Default().Info(fmt.Sprintf(format, args...))
		}
	}
	if deps.LogfCtx == nil {
		logf := deps.Logf
		deps.LogfCtx = func(_ context.Context, format string, args ...any) {
			logf(format, args...)
		}
	}
	return &Service{
		mu:                          deps.Mu,
		db:                          deps.DB,
		hubCfg:                      deps.HubCfg,
		setHubCfg:                   deps.SetHubCfg,
		logf:                        deps.Logf,
		logfCtx:                     deps.LogfCtx,
		promotePendingClaws:         deps.PromotePendingClaws,
		fireworksModelOptions:       deps.FireworksModelOptions,
		newGitHubPermissionsChecker: deps.NewGitHubPermissionsChecker,
		generateSSHKey:              deps.GenerateSSHKey,
		hubConfigDir:                deps.HubConfigDir,
		templatesDir:                deps.TemplatesDir,
		loadExternalFactories:       deps.LoadExternalFactories,
		loadExternalFactory:         deps.LoadExternalFactory,
		saveExternalFactory:         deps.SaveExternalFactory,
		deleteExternalFactory:       deps.DeleteExternalFactory,
		listExternalTemplates:       deps.ListExternalTemplates,
		loadExternalTemplate:        deps.LoadExternalTemplate,
		resolveDefaultModelForKey:   deps.ResolveDefaultModelForKey,
		modelAuthJobsMu:             deps.ModelAuthJobsMu,
		modelAuthJobs:               deps.ModelAuthJobs,
	}
}

// LLMModelOption is a selectable model in the settings UI. Moved here from
// the hub's fireworks_models.go because SettingsView embeds it.
type LLMModelOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DefaultFireworksModel is the fallback Fireworks model. The hub's
// fireworks_models.go aliases this constant so the two sides cannot drift.
const DefaultFireworksModel = "fireworks/accounts/fireworks/models/kimi-k2p7"

// StripProviderPrefix strips the "provider/" prefix from a model identifier.
// Moved here from the hub's failure_summary.go together with the
// OpenAI-compatible provider table; the hub re-exports them via aliases.
func StripProviderPrefix(model string) string {
	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return model
}

// OpenAICompatibleProvider identifies an OpenAI-compatible Chat Completions
// endpoint. Moved here from the hub's failure_summary.go because the AI
// config assistant uses it; the hub aliases it back.
type OpenAICompatibleProvider struct {
	Name    string
	BaseURL string
}

// OpenAICompatibleConfig returns the endpoint config for a provider name.
func OpenAICompatibleConfig(provider string) OpenAICompatibleProvider {
	switch provider {
	case "fireworks":
		return OpenAICompatibleProvider{Name: "Fireworks", BaseURL: "https://api.fireworks.ai/inference/v1"}
	case "groq":
		return OpenAICompatibleProvider{Name: "Groq", BaseURL: "https://api.groq.com/openai/v1"}
	case "grok":
		return OpenAICompatibleProvider{Name: "Grok", BaseURL: "https://api.x.ai/v1"}
	case "deepseek":
		return OpenAICompatibleProvider{Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1"}
	case "ollama":
		baseURL := os.Getenv("OLLAMA_BASE_URL")
		if baseURL == "" {
			baseURL = "http://ollama:11434"
		}
		return OpenAICompatibleProvider{Name: "Ollama", BaseURL: strings.TrimRight(baseURL, "/") + "/v1"}
	default:
		return OpenAICompatibleProvider{Name: "OpenAI", BaseURL: "https://api.openai.com/v1"}
	}
}

// ResolveActiveKey selects the active key by selected name, then default,
// then first. Moved here from the hub's bootstrap.go because
// BuildModelAuthEnv needs it; the hub delegates to it via the bridge.
func ResolveActiveKey(keys []*types.LLMKeyConfig, selectedKeyName string) *types.LLMKeyConfig {
	for _, k := range keys {
		if k.Name == selectedKeyName && LLMKeyHasRequiredAPIKey(k) {
			return k
		}
	}
	for _, k := range keys {
		if k.Default && LLMKeyHasRequiredAPIKey(k) {
			return k
		}
	}
	if len(keys) > 0 {
		for _, k := range keys {
			if LLMKeyHasRequiredAPIKey(k) {
				return k
			}
		}
	}
	return nil
}
