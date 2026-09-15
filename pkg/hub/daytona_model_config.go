package hub

// daytonaOpenClawConfigPatch exports model data literally before running the
// provider patch. Go string quoting is not shell quoting: Bash still evaluates
// command substitutions inside double-quoted strings.
func daytonaOpenClawConfigPatch(model, gatewayPassword, keyEnv, providerConfig string) string {
	return "export HOME=/home/daytona; export OPENCLAW_DEFAULT_MODEL=" + shellQuote(model) +
		"; export ELASTICCLAW_GATEWAY_PASSWORD=" + shellQuote(gatewayPassword) + "; " + keyEnv + providerConfig
}
