package harness

// SandboxSteps are the harness-specific shell commands for a hub-driven bootstrap (Daytona).
type SandboxSteps interface {
	InstallCommand(homeDir, version string) string
	InstallStatusCommand(homeDir, version string) string
	UninstallCommand() string
	VerifyVersionCommand(version string) string
	InstallModelPluginCommand(homeDir, provider, version string) string
	OnboardCommand(llmKeyEnv, onboardFlags string) string
	ConfigExistsCommand(homeDir string) string
	StagePluginDepsCommand() string
	StartGatewayCommand(port int) string
}
