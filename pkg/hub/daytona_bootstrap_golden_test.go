package hub

import "testing"

func TestDaytonaHarnessBootstrapGolden(t *testing.T) {
	t.Setenv("ELASTICCLAW_CODEX_PLUGIN_VERSION", "2026.7.1-2")
	cases := []struct {
		name    string
		command string
	}{
		{"uninstall", daytonaUninstallOpenClawCommand()},
		{"install", daytonaStartOpenClawInstallCommand("2026.7.1-2")},
		{"install_quoted_version", daytonaStartOpenClawInstallCommand("2026.7.1-'test'")},
		{"install_status", daytonaOpenClawInstallStatusCommand("2026.7.1-2")},
		{"install_status_quoted_version", daytonaOpenClawInstallStatusCommand("2026.7.1-'test'")},
		{"verify_version", daytonaVerifyOpenClawCommand("2026.7.1-2")},
		{"model_plugin_codex", daytonaInstallModelPluginCommand("codex")},
		{"model_plugin_openai", daytonaInstallModelPluginCommand("openai")},
		{"model_plugin_empty", daytonaInstallModelPluginCommand("")},
		{"onboard", daytonaOnboardCommand("export ANTHROPIC_API_KEY='test-key'; ", "--auth-choice anthropic-api-key --anthropic-api-key \"$ANTHROPIC_API_KEY\"")},
		{"onboard_empty", daytonaOnboardCommand("", "")},
		{"config_exists", daytonaOpenClawConfigExistsCommand()},
		{"stage_plugin_deps", daytonaStageOpenClawPluginDepsCommand()},
		{"start_gateway", daytonaStartOpenClawGatewayCommand()},
		{"start_gateway_key_env", withDaytonaLLMKeyEnv("export OPENAI_API_KEY='test-key'; ", daytonaStartOpenClawGatewayCommand())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertHarnessBootstrapGolden(t, "daytona_"+tc.name, tc.command)
		})
	}

	t.Run("model_plugin_quoted_version", func(t *testing.T) {
		t.Setenv("ELASTICCLAW_CODEX_PLUGIN_VERSION", "2026.7.1-'test'")
		assertHarnessBootstrapGolden(t, "daytona_model_plugin_quoted_version", daytonaInstallModelPluginCommand("codex"))
	})
}
