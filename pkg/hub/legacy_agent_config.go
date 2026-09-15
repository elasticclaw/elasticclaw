package hub

import (
	"encoding/json"
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// resolveTemplateAgentSnapshot preserves legacy defaults when no subagent
// configuration is declared, and pins configured child models before insertion.
func (s *Server) resolveTemplateAgentSnapshot(tmpl *types.TemplateConfig, model, key string) (string, string, string, error) {
	if tmpl == nil || tmpl.Subagents == nil {
		return model, key, "null", nil
	}
	s.mu.RLock()
	if tmpl.DefaultModel == "" {
		// Factory creators pass the named credential's default when the
		// template names llm_key and the hub default otherwise; drop the
		// latter when the selected credential's provider cannot serve it.
		model = compatibleFallbackModel(s.hubCfg, key, model)
	}
	resolved, err := resolveAgentConfig(s.hubCfg, types.AgentConfig{DefaultModel: model, LLMKey: key, Subagents: tmpl.Subagents})
	s.mu.RUnlock()
	if err != nil {
		return "", "", "", fmt.Errorf("resolve template agents: %w", err)
	}
	data, err := json.Marshal(resolved.Subagents)
	return resolved.DefaultModel, resolved.LLMKey, string(data), err
}
