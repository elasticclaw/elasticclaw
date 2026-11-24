package hub

import (
	"log"
	"sort"
	"strings"
)

// sandboxManagedEnvKeys lists values derived from dedicated hub parameters.
// Custom env must not override or duplicate them.
var sandboxManagedEnvKeys = map[string]bool{
	"ELASTICCLAW_HUB_URL":            true,
	"ELASTICCLAW_CLAW_ID":            true,
	"ELASTICCLAW_CLAW_TOKEN":         true,
	"ELASTICCLAW_MODEL_AUTH_TOKEN":   true,
	"ELASTICCLAW_CLAW_NAME":          true,
	"ELASTICCLAW_TEMPLATE":           true,
	"ELASTICCLAW_GITHUB_REPOS":       true,
	"ELASTICCLAW_BOOTSTRAP":          true,
	"ELASTICCLAW_WAIT_FOR_WORKSPACE": true,
	"ELASTICCLAW_GATEWAY_PASSWORD":   true,
	"OPENCLAW_GATEWAY_PASSWORD":      true,
	"OPENCLAW_DEFAULT_MODEL":         true,
	"ELASTICCLAW_LLM_PROVIDER":       true,
	"ELASTICCLAW_NIX":                true,
	"ELASTICCLAW_DOCKER":             true,
	"ELASTICCLAW_PROVIDER_CONFIG":    true,
	"ELASTICCLAW_API_KEY_AUTH_SYNC":  true,
	"ELASTICCLAW_OAUTH_AUTH_SYNC":    true,
	"ELASTICCLAW_ONBOARD_FLAGS":      true,
}

// isReservedSandboxEnvKey protects hub-managed values and installation inputs.
// Apply it only to user-supplied layers, before merging hub-managed values.
func isReservedSandboxEnvKey(name string) bool {
	if sandboxManagedEnvKeys[name] {
		return true
	}
	if strings.HasPrefix(strings.ToUpper(name), "NPM_CONFIG_") ||
		strings.HasPrefix(name, "YARN_") || strings.HasPrefix(name, "PNPM_") || strings.HasPrefix(name, "COREPACK_") ||
		(strings.HasPrefix(name, "ELASTICCLAW_") && strings.HasSuffix(name, "_VERSION")) {
		return true
	}
	switch name {
	case "NODE_OPTIONS", "NODE_PATH", "PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "NIX_CONFIG", "NIX_PATH",
		// Sourced by every non-interactive bootstrap shell.
		"BASH_ENV", "ENV",
		// Package indexes for uvx MCP launches.
		"PIP_INDEX_URL", "PIP_EXTRA_INDEX_URL", "UV_INDEX_URL", "UV_EXTRA_INDEX_URL", "UV_DEFAULT_INDEX", "UV_INDEX",
		// TLS trust used by installers. Proxies stay allowed: without a
		// custom CA they cannot alter verified HTTPS downloads.
		"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE":
		return true
	}
	return false
}

// sandboxEnvFilter tracks rejected names across all user layers for one claw.
type sandboxEnvFilter map[string]bool

func (f sandboxEnvFilter) allow(name string) bool {
	if !isReservedSandboxEnvKey(name) {
		return true
	}
	f[name] = true
	return false
}

func (f sandboxEnvFilter) log(clawID string) {
	if len(f) == 0 {
		return
	}
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	sort.Strings(names)
	log.Printf("[sandbox] claw %s: dropped reserved user env keys: %q", clawID, names)
}
