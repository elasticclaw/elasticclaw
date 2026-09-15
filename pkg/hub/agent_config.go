package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

func agentCredentialAvailable(cfg *types.HubConfig, key *types.LLMKeyConfig) bool {
	if key == nil {
		return false
	}
	if key.APIKey != "" {
		return true
	}
	if key.AuthProfile != "" {
		for _, profile := range cfg.ModelAuthProfiles {
			if profile != nil && profile.Name == key.AuthProfile && profile.Provider == key.Provider && strings.TrimSpace(profile.AuthState) != "" {
				return true
			}
		}
		return false
	}
	return llmKeyHasRequiredAPIKey(key)
}

// compatibleFallbackModel keeps a hub-level fallback model only when the
// credential resolveAgentConfig will select can serve it; otherwise it returns
// "" so that credential's own default applies instead of a provider mismatch.
func compatibleFallbackModel(cfg *types.HubConfig, keyName, model string) string {
	if cfg == nil || !strings.Contains(model, "/") {
		return model
	}
	key := resolveActiveKey(cfg.LLMKeys, keyName)
	if key == nil || modelMatchesProvider(key.Provider, model) {
		return model
	}
	return ""
}

// resolveAgentConfig pins model and credential names before provisioning.
func resolveAgentConfig(cfg *types.HubConfig, requested types.AgentConfig) (types.AgentConfig, error) {
	if cfg == nil {
		cfg = &types.HubConfig{}
	}
	resolve := func(name, model string) (string, string, *types.LLMKeyConfig, error) {
		var key *types.LLMKeyConfig
		if name != "" {
			for _, candidate := range cfg.LLMKeys {
				if candidate != nil && candidate.Name == name {
					key = candidate
					break
				}
			}
			if key == nil {
				return "", "", nil, fmt.Errorf("unknown LLM credential %q", name)
			}
			if !agentCredentialAvailable(cfg, key) {
				return "", "", nil, fmt.Errorf("LLM credential %q is unavailable", name)
			}
		} else {
			key = resolveActiveKey(cfg.LLMKeys, "")
		}
		if key != nil {
			if !agentCredentialAvailable(cfg, key) {
				return "", "", nil, fmt.Errorf("LLM credential %q is unavailable", key.Name)
			}
			name = key.Name
			if model == "" {
				model = resolveDefaultModelForKey(cfg, key)
			}
			if model == "" {
				return "", "", nil, fmt.Errorf("no default model configured for credential %q; set default_model on the credential or the hub", name)
			}
			if strings.Contains(model, "/") && !modelMatchesProvider(key.Provider, model) {
				return "", "", nil, fmt.Errorf("model %q does not match credential provider %q", model, key.Provider)
			}
			model = normalizeModelForProvider(key.Provider, model)
		} else {
			return "", "", nil, fmt.Errorf("no available LLM credential configured")
		}
		return name, model, key, nil
	}
	name, model, mainKey, err := resolve(requested.LLMKey, requested.DefaultModel)
	if err != nil {
		return types.AgentConfig{}, err
	}
	result := types.AgentConfig{DefaultModel: model, LLMKey: name}
	if requested.Subagents == nil {
		return result, nil
	}
	child := *requested.Subagents
	if child.MaxConcurrent < 0 || child.MaxConcurrent > 32 {
		return result, fmt.Errorf("subagent max_concurrent must be between 1 and 32, or omitted")
	}
	if child.LLMKey == "" {
		child.LLMKey = name
	}
	if child.Model == "" && child.LLMKey == name {
		child.Model = model
	}
	childName, childModel, mainChildKey, err := resolve(child.LLMKey, child.Model)
	child.LLMKey, child.Model = childName, childModel
	if err != nil {
		return result, fmt.Errorf("subagents: %w", err)
	}
	if mainKey != nil && mainChildKey != nil {
		if mainKey.Name != mainChildKey.Name && mainKey.EnvVarName() == mainChildKey.EnvVarName() {
			return result, fmt.Errorf("principal and subagents cannot use different credentials sharing %s", mainKey.EnvVarName())
		}
		if (mainKey.Provider == "codex" || mainChildKey.Provider == "codex") && (model != child.Model || mainKey.Name != mainChildKey.Name) {
			return result, fmt.Errorf("mixed agent models with the Codex runtime are not supported")
		}
	}
	result.Subagents = &child
	return result, nil
}

func (s *Server) loadClawSubagentConfig(clawID string) (*types.SubagentConfig, error) {
	var raw string
	if err := s.db.QueryRow(`SELECT COALESCE(subagents_config,'null') FROM claws WHERE id=?`, clawID).Scan(&raw); err != nil {
		return nil, err
	}
	var config *types.SubagentConfig
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return nil, fmt.Errorf("decode claw agent snapshot: %w", err)
	}
	return config, nil
}

func (s *Server) handleAgentOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	type credential struct {
		Name         string `json:"name"`
		Provider     string `json:"provider"`
		DefaultModel string `json:"default_model"`
		Available    bool   `json:"available"`
	}
	options := []credential{}
	s.mu.RLock()
	defaultModel := ""
	if s.hubCfg != nil {
		defaultModel = s.hubCfg.DefaultModel
		for _, key := range s.hubCfg.LLMKeys {
			if key != nil {
				options = append(options, credential{key.Name, key.Provider, resolveDefaultModelForKey(s.hubCfg, key), agentCredentialAvailable(s.hubCfg, key)})
			}
		}
	}
	s.mu.RUnlock()
	jsonOK(w, map[string]interface{}{"credentials": options, "default_model": defaultModel})
}

func workflowAgentConfig(workflow *types.WorkflowConfig) types.AgentConfig {
	return types.AgentConfig{DefaultModel: workflow.DefaultModel, LLMKey: workflow.LLMKey, Subagents: workflow.Subagents}
}
func applyWorkflowAgents(workflow *types.WorkflowConfig, agents types.AgentConfig) {
	workflow.DefaultModel = agents.DefaultModel
	workflow.LLMKey = agents.LLMKey
	workflow.Subagents = agents.Subagents
}
func mergeAgentConfig(base, override types.AgentConfig) types.AgentConfig {
	if override.LLMKey != "" {
		base.LLMKey = override.LLMKey
		if override.DefaultModel == "" {
			base.DefaultModel = ""
		}
	}
	if override.DefaultModel != "" {
		base.DefaultModel = override.DefaultModel
	}
	if override.Subagents != nil {
		base.Subagents = override.Subagents
	}
	return base
}

// patchWorkflowYAML preserves unrelated fields, including v2 states and transitions.
func patchWorkflowYAML(workflow *types.WorkflowConfig, fields map[string]interface{}) error {
	var document yaml.Node
	raw := workflow.RawConfig
	if strings.TrimSpace(raw) == "" {
		data, err := yaml.Marshal(workflow)
		if err != nil {
			return err
		}
		raw = string(data)
	}
	if err := yaml.Unmarshal([]byte(raw), &document); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("workflow must be a YAML mapping")
	}
	root := document.Content[0]
	for key, value := range fields {
		index := -1
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				index = i
				break
			}
		}
		// Empty values drop the key: older hubs reject unknown v2 keys, so an
		// authored `llm_key: ""` or `subagents: null` must not survive a rollback.
		if emptyPatchValue(value) {
			if index >= 0 {
				root.Content = append(root.Content[:index], root.Content[index+2:]...)
			}
			continue
		}
		var node yaml.Node
		if err := node.Encode(value); err != nil {
			return err
		}
		if index >= 0 {
			root.Content[index+1] = &node
		} else {
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &node)
		}
	}
	data, err := yaml.Marshal(&document)
	if err != nil {
		return err
	}
	workflow.RawConfig = string(data)
	return nil
}

// emptyPatchValue reports values patchWorkflowYAML removes instead of encoding:
// empty strings and nil pointers. Booleans such as enabled=false are kept.
func emptyPatchValue(value interface{}) bool {
	if value == nil {
		return true
	}
	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.String:
		return v.Len() == 0
	case reflect.Ptr, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// effectiveWorkflowAgents applies workspace defaults before workflow overrides.
func effectiveWorkflowAgents(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig) types.AgentConfig {
	var base types.AgentConfig
	if workspace != nil && !isWorkflowV2(workflow) {
		_ = yaml.Unmarshal([]byte(workspace.Files["elasticclaw-config.yaml"]), &base)
	}
	return mergeAgentConfig(base, workflowAgentConfig(workflow))
}

// validateWorkflowAgents resolves authored agent settings the way claw creation
// will. Workflows without authored settings keep the legacy path, which runs on
// hubs without llm_keys, so they are not validated here.
func (s *Server) validateWorkflowAgents(workspace *types.WorkspaceConfig, workflow *types.WorkflowConfig) error {
	agents := effectiveWorkflowAgents(workspace, workflow)
	if workflow.DefaultModel == "" && workflow.LLMKey == "" && agents.Subagents == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := resolveAgentConfig(s.hubCfg, agents)
	return err
}
