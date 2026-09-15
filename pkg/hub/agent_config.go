package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"gopkg.in/yaml.v3"
)

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
			if !llmKeyHasRequiredAPIKey(key) {
				return "", "", nil, fmt.Errorf("LLM credential %q is unavailable", name)
			}
		} else {
			key = resolveActiveKey(cfg.LLMKeys, "")
		}
		if key != nil {
			name = key.Name
			if model == "" {
				model = resolveDefaultModelForKey(cfg, key)
			}
			if strings.Contains(model, "/") && !modelMatchesProvider(key.Provider, model) {
				return "", "", nil, fmt.Errorf("model %q does not match credential provider %q", model, key.Provider)
			}
			model = normalizeModelForProvider(key.Provider, model)
		} else if model == "" {
			model = cfg.DefaultModel
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
		if child.Model == "" {
			child.Model = model
		}
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
				options = append(options, credential{key.Name, key.Provider, resolveDefaultModelForKey(s.hubCfg, key), llmKeyHasRequiredAPIKey(key)})
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
		var node yaml.Node
		if err := node.Encode(value); err != nil {
			return err
		}
		found := false
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == key {
				root.Content[i+1] = &node
				found = true
				break
			}
		}
		if !found {
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
