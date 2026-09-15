package hub

import (
	"encoding/json"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestTemplateAgentSnapshotPinsSeparateProvider(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{LLMKeys: types.LLMKeysList{
		{Name: "primary", Provider: "anthropic", APIKey: "fixture", Default: true, DefaultModel: "main"},
		{Name: "workers", Provider: "openai", APIKey: "fixture", DefaultModel: "worker"},
	}}}
	tmpl := &types.TemplateConfig{Subagents: &types.SubagentConfig{LLMKey: "workers", MaxConcurrent: 3}}
	model, key, raw, err := s.resolveTemplateAgentSnapshot(tmpl, "anthropic/main", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if model != "anthropic/main" || key != "primary" {
		t.Fatalf("principal = %s/%s", key, model)
	}
	tmpl.Subagents.LLMKey = "changed"
	var child types.SubagentConfig
	if err := json.Unmarshal([]byte(raw), &child); err != nil {
		t.Fatal(err)
	}
	if child.Model != "openai/worker" || child.LLMKey != "workers" || child.MaxConcurrent != 3 {
		t.Fatalf("snapshot changed or unresolved: %+v", child)
	}
}

func TestTemplateAgentSnapshotPreservesUnconfiguredLegacy(t *testing.T) {
	s := &Server{}
	for _, tmpl := range []*types.TemplateConfig{nil, {}} {
		model, key, raw, err := s.resolveTemplateAgentSnapshot(tmpl, "legacy/model", "legacy")
		if err != nil || model != "legacy/model" || key != "legacy" || raw != "null" {
			t.Fatalf("legacy snapshot = %q, %q, %q, %v", model, key, raw, err)
		}
	}
}

func TestTemplateAgentSnapshotDropsIncompatibleHubModel(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{DefaultModel: "openai/hub-model", LLMKeys: types.LLMKeysList{
		{Name: "primary", Provider: "anthropic", APIKey: "fixture", Default: true, DefaultModel: "main"},
	}}}
	tmpl := &types.TemplateConfig{Subagents: &types.SubagentConfig{}}
	model, key, _, err := s.resolveTemplateAgentSnapshot(tmpl, s.hubCfg.DefaultModel, "")
	if err != nil {
		t.Fatal(err)
	}
	if model != "anthropic/main" || key != "primary" {
		t.Fatalf("principal = %s/%s, want credential default", key, model)
	}
}

func TestTemplateAgentSnapshotRejectsUnavailableChild(t *testing.T) {
	s := &Server{hubCfg: &types.HubConfig{LLMKeys: types.LLMKeysList{
		{Name: "primary", Provider: "anthropic", APIKey: "fixture"},
	}}}
	_, _, _, err := s.resolveTemplateAgentSnapshot(&types.TemplateConfig{Subagents: &types.SubagentConfig{LLMKey: "missing"}}, "anthropic/main", "primary")
	if err == nil {
		t.Fatal("expected unavailable child credential error")
	}
}
