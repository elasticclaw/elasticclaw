package hub

// This file is the compatibility bridge left behind by the extraction of the
// settings surface (settings, ai_config, model_auth, doctor) into
// pkg/hub/settings (phase-2 hub reorganization). It keeps the existing call
// sites in this package (including tests) unchanged while the pkg/hub split
// proceeds one subpackage at a time: the aliases below simply forward to the
// settings package. New code should use pkg/hub/settings directly; callers
// drop these aliases as they migrate to their own subpackages.

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/settings"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// settingsSvc returns the settings service bound to this server's state. The
// service itself is stateless; all mutable state (config, model-auth jobs)
// stays on the Server behind the injected hooks, so building it per call is
// cheap and keeps hand-built test servers (&Server{...}) working without
// extra wiring.
func (s *Server) settingsSvc() *settings.Service {
	return settings.New(settings.Deps{
		CfgMu:                 &s.cfgMu,
		DB:                    s.db,
		HubCfg:                func() *types.HubConfig { return s.hubCfg },
		SetHubCfg:             func(cfg *types.HubConfig) { s.hubCfg = cfg },
		Logf:                  logf,
		LogfCtx:               logfCtx,
		PromotePendingClaws:   s.promotePendingClaws,
		FireworksModelOptions: s.fireworksModelOptions,
		NewGitHubPermissionsChecker: func(app *types.GitHubAppConfig) (settings.GitHubPermissionsChecker, error) {
			provider, err := NewGitHubTokenProvider(app)
			if err != nil {
				return nil, err
			}
			return provider, nil
		},
		GenerateSSHKey:            GenerateExedevKey,
		HubConfigDir:              hubConfigDir,
		TemplatesDir:              templatesDir,
		LoadExternalFactories:     loadExternalFactories,
		LoadExternalFactory:       loadExternalFactory,
		SaveExternalFactory:       saveExternalFactory,
		DeleteExternalFactory:     deleteExternalFactory,
		ListExternalTemplates:     listExternalTemplates,
		LoadExternalTemplate:      loadExternalTemplate,
		ResolveDefaultModelForKey: resolveDefaultModelForKey,
		ModelAuthJobsMu:           &s.modelAuthJobsMu,
		ModelAuthJobs: func() map[string]*settings.ModelAuthLoginJob {
			if s.modelAuthJobs == nil {
				s.modelAuthJobs = map[string]*modelAuthLoginJob{}
			}
			return s.modelAuthJobs
		},
	})
}

// Types moved to pkg/hub/settings.
type (
	SettingsStatus = settings.SettingsStatus
	SettingsView   = settings.SettingsView
	LLMKeyView     = settings.LLMKeyView
	MCPView        = settings.MCPView
	LLMModelOption = settings.LLMModelOption

	aiChatMessage            = settings.AIChatMessage
	modelAuthLoginJob        = settings.ModelAuthLoginJob
	oauthTokenResponse       = settings.OAuthTokenResponse
	openAICompatibleProvider = settings.OpenAICompatibleProvider
)

// Server methods moved to settings.Service (HTTP handlers).

func (s *Server) handleSettingsStatus(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleSettingsStatus(w, r)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleSettings(w, r)
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().GetSettings(w, r)
}

func (s *Server) handleGitHubAppTest(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleGitHubAppTest(w, r)
}

func (s *Server) handleAIConfig(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleAIConfig(w, r)
}

func (s *Server) handleAIConfigApply(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleAIConfigApply(w, r)
}

func (s *Server) handleAIConfigRevert(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleAIConfigRevert(w, r)
}

func (s *Server) handleAIConfigBackup(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleAIConfigBackup(w, r)
}

func (s *Server) handleAIConfigCurrentConfig(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleAIConfigCurrentConfig(w, r)
}

func (s *Server) handleAIConfigStream(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleAIConfigStream(w, r)
}

func (s *Server) handleModelAuthLogin(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleModelAuthLogin(w, r)
}

func (s *Server) handleModelAuthLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleModelAuthLoginStatus(w, r)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	s.settingsSvc().HandleDoctor(w, r)
}

// Server methods moved to settings.Service (non-handler, still used by tests
// in this package).

func (s *Server) checkLLMKeys(cfg *types.HubConfig) []settings.DoctorCheck {
	return s.settingsSvc().CheckLLMKeys(cfg)
}

func (s *Server) checkDefaultModel(cfg *types.HubConfig) []settings.DoctorCheck {
	return s.settingsSvc().CheckDefaultModel(cfg)
}

func (s *Server) captureModelAuthOutput(job *modelAuthLoginJob, r io.Reader, done chan<- struct{}) {
	s.settingsSvc().CaptureModelAuthOutput(job, r, done)
}

func (s *Server) appendModelAuthOutput(job *modelAuthLoginJob, raw string) {
	s.settingsSvc().AppendModelAuthOutput(job, raw)
}

// Package-level helpers moved to pkg/hub/settings.

func resolveActiveKey(keys []*types.LLMKeyConfig, selectedKeyName string) *types.LLMKeyConfig {
	return settings.ResolveActiveKey(keys, selectedKeyName)
}

func llmKeyHasRequiredAPIKey(key *types.LLMKeyConfig) bool {
	return settings.LLMKeyHasRequiredAPIKey(key)
}

func stripProviderPrefix(model string) string {
	return settings.StripProviderPrefix(model)
}

func openAICompatibleConfig(provider string) openAICompatibleProvider {
	return settings.OpenAICompatibleConfig(provider)
}

func sanitizeHubConfig(cfg *types.HubConfig) (string, error) {
	return settings.SanitizeHubConfig(cfg)
}

func sanitizeAIChatHistory(history []aiChatMessage) []aiChatMessage {
	return settings.SanitizeAIChatHistory(history)
}

func selectAIConfigProvider(llmKeys types.LLMKeysList, defaultModel string) (*settings.AIConfigProviderChoice, error) {
	return settings.SelectAIConfigProvider(llmKeys, defaultModel)
}

func streamLLMWithSystemPrompt(ctx context.Context, systemPrompt string, msgs []aiChatMessage, llmKeys types.LLMKeysList, defaultModel string, onToken func(string)) error {
	return settings.StreamLLMWithSystemPrompt(ctx, systemPrompt, msgs, llmKeys, defaultModel, onToken)
}

func substitutePlaceholders(yamlStr string, secrets map[string]string) (string, error) {
	return settings.SubstitutePlaceholders(yamlStr, secrets)
}

func restoreMaskedSecretsFromDisk(yamlStr string, diskCfg *types.HubConfig) (string, error) {
	return settings.RestoreMaskedSecretsFromDisk(yamlStr, diskCfg)
}

func validateHubConfig(cfg *types.HubConfig) error {
	return settings.ValidateHubConfig(cfg)
}

func checkMaskedValues(cfg *types.HubConfig) error {
	return settings.CheckMaskedValues(cfg)
}

func buildModelAuthEnv(cfg *types.HubConfig, selectedKeyName string) string {
	return settings.BuildModelAuthEnv(cfg, selectedKeyName)
}

func buildModelAuthRestoreShell(modelAuthEnv string) string {
	return settings.BuildModelAuthRestoreShell(modelAuthEnv)
}

func writeCodexAuthFiles(root string, token oauthTokenResponse) error {
	return settings.WriteCodexAuthFiles(root, token)
}

func writeGrokAuthFilesFromUserInfo(root string, token oauthTokenResponse, userInfo map[string]any, now time.Time) error {
	return settings.WriteGrokAuthFilesFromUserInfo(root, token, userInfo, now)
}

// Constants moved to pkg/hub/settings (still referenced by tests here).
const (
	grokAuthIssuer   = settings.GrokAuthIssuer
	grokAuthClientID = settings.GrokAuthClientID
)
