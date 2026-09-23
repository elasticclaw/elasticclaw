package hub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// agentBootstrapPlan is shared by every infrastructure bootstrap path.
// Credential selection is resolved before scripts are generated so the selected
// worker key cannot be shadowed by a provider's unrelated default credential.
type agentBootstrapPlan struct {
	LLMKeyEnv      string
	ModelAuthEnv   string
	APIKeyAuthSync string
	OAuthAuthSync  string
	ProviderConfig string
}

func buildAgentBootstrapPlan(cfg *types.HubConfig, mainKey, mainModel string, sub *types.SubagentConfig) (agentBootstrapPlan, error) {
	plan := agentBootstrapPlan{
		LLMKeyEnv:      buildLLMKeyEnv(cfg.LLMKeys, mainKey),
		ModelAuthEnv:   buildModelAuthEnv(cfg, mainKey),
		APIKeyAuthSync: buildOpenClawAPIKeyAuthSyncShell(cfg.LLMKeys, mainKey),
		OAuthAuthSync:  buildOpenClawOAuthAuthSyncShell(cfg.LLMKeys, mainKey),
		ProviderConfig: buildOpenClawProviderConfig(cfg.LLMKeys, mainKey),
	}
	if sub == nil {
		return plan, nil
	}
	resolved, err := resolveAgentConfig(cfg, types.AgentConfig{DefaultModel: mainModel, LLMKey: mainKey, Subagents: sub})
	if err != nil {
		return agentBootstrapPlan{}, err
	}
	sub = resolved.Subagents
	selected := []*types.LLMKeyConfig{}
	seen := map[string]bool{}
	for _, name := range []string{resolved.LLMKey, sub.LLMKey} {
		key := resolveActiveKey(cfg.LLMKeys, name)
		if key != nil && !seen[key.Name] {
			selected = append(selected, key)
			seen[key.Name] = true
		}
	}
	// Keep unrelated providers available while prioritizing both selected credentials.
	plan.LLMKeyEnv = buildLLMKeyEnv(cfg.LLMKeys, resolved.LLMKey, sub.LLMKey)
	plan.APIKeyAuthSync = ""
	plan.OAuthAuthSync = ""
	files := map[string]string{}
	authProvider := ""
	for _, key := range selected {
		if script := buildOpenClawAPIKeyAuthSyncShell(selected, key.Name); script != "" {
			plan.APIKeyAuthSync += script + "\n"
		}
		if script := buildOpenClawOAuthAuthSyncShell(selected, key.Name); script != "" {
			plan.OAuthAuthSync += script + "\n"
		}
		if key.AuthProfile == "" || key.APIKey != "" {
			continue
		}
		foundProfile := false
		for _, profile := range cfg.ModelAuthProfiles {
			if profile == nil || profile.Name != key.AuthProfile || profile.Provider != key.Provider || profile.AuthState == "" {
				continue
			}
			foundProfile = true
			raw, err := base64.StdEncoding.DecodeString(profile.AuthState)
			if err != nil {
				return agentBootstrapPlan{}, fmt.Errorf("decode model auth profile %q: invalid state", profile.Name)
			}
			var bundle struct {
				Files map[string]string `json:"files"`
			}
			if json.Unmarshal(raw, &bundle) != nil {
				return agentBootstrapPlan{}, fmt.Errorf("decode model auth profile %q: invalid bundle", profile.Name)
			}
			for path, content := range bundle.Files {
				if previous, exists := files[path]; exists && previous != content {
					return agentBootstrapPlan{}, fmt.Errorf("model auth profiles contain conflicting file %q; use independent provider profiles", path)
				}
				files[path] = content
			}
			if authProvider == "" || profile.Provider == "grok" {
				authProvider = profile.Provider
			}
		}
		if !foundProfile {
			return agentBootstrapPlan{}, fmt.Errorf("model auth profile %q is unavailable", key.AuthProfile)
		}
	}
	plan.ModelAuthEnv = ""
	if len(files) > 0 {
		raw, _ := json.Marshal(map[string]any{"files": files})
		plan.ModelAuthEnv = fmt.Sprintf("export ELASTICCLAW_MODEL_AUTH_PROVIDER=%q\nexport ELASTICCLAW_MODEL_AUTH_STATE=%q\n", authProvider, base64.StdEncoding.EncodeToString(raw))
	}
	plan.ProviderConfig = buildOpenClawProviderConfig(cfg.LLMKeys, resolved.LLMKey, sub)
	return plan, nil
}

func (s *Server) clawAgentBootstrapPlan(clawID string, cfg *types.HubConfig, mainKey, mainModel string) (agentBootstrapPlan, error) {
	sub, err := s.loadClawSubagentConfig(clawID)
	if err != nil {
		return agentBootstrapPlan{}, fmt.Errorf("load subagent configuration: %w", err)
	}
	s.mu.RLock()
	plan, err := buildAgentBootstrapPlan(cfg, strings.TrimSpace(mainKey), mainModel, sub)
	s.mu.RUnlock()
	if err != nil {
		return agentBootstrapPlan{}, fmt.Errorf("resolve agent bootstrap: %w", err)
	}
	return plan, nil
}

// resolveDaytonaBootstrapModel preserves pinned models, while retaining the
// legacy Daytona correction for stored models belonging to another provider.
// New claws store provider-prefixed models; an unprefixed value comes from a
// legacy claw, whose restarts always used the credential's default model.
func resolveDaytonaBootstrapModel(cfg *types.HubConfig, key *types.LLMKeyConfig, stored string) (model string, legacyMismatch bool) {
	if stored == "" || !strings.Contains(stored, "/") {
		return resolveDefaultModelForKey(cfg, key), false
	}
	if key == nil {
		return stored, false
	}
	if !modelMatchesProvider(key.Provider, stored) {
		return resolveDefaultModelForKey(cfg, key), true
	}
	return stored, false
}
