package hub

import (
	"github.com/elasticclaw/elasticclaw/pkg/harness"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Keep existing builder tests exercising the registered adapter.
func buildOpenClawProviderConfig(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	return openClawBootstrap().Config(harness.BootstrapInput{LLMKeys: keys, SelectedKey: selectedKeyName}).ProviderConfig
}

func buildOpenClawAPIKeyAuthSyncShell(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	return openClawBootstrap().Config(harness.BootstrapInput{LLMKeys: keys, SelectedKey: selectedKeyName}).APIKeyAuthSync
}

func buildOpenClawOAuthAuthSyncShell(keys []*types.LLMKeyConfig, selectedKeyName string) string {
	return openClawBootstrap().Config(harness.BootstrapInput{LLMKeys: keys, SelectedKey: selectedKeyName}).OAuthAuthSync
}

func buildOnboardFlags(keys []*types.LLMKeyConfig, selectedKeyName, defaultModel string) string {
	return openClawBootstrap().Config(harness.BootstrapInput{LLMKeys: keys, SelectedKey: selectedKeyName, DefaultModel: defaultModel}).OnboardFlags
}
