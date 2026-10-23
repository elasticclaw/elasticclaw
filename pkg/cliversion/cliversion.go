package cliversion

import (
	"os"
	"strings"
)

const (
	OpenClawVersion      = "2026.9.4"
	OpenClawImageVersion = "2026.9.4"
	OpenClawImage        = "ghcr.io/openclaw/openclaw:" + OpenClawImageVersion
	CodexPluginVersion   = "2026.9.4"
	CodexCLIVersion      = "0.153.4"
	GrokCLIVersion       = "0.2.103"
)

// NodeCompatibleExpr is a JS boolean over major, minor and patch for the Node
// range claws accept: ">=24.22.1 <25 || >=26.12.1". OpenClaw 2026.9.x's own
// engines clause starts at 24.16.0 / 26.1.0, but releases below 24.22.1 and
// 26.12.1 bundle a Brotli decoder vulnerable to CVE-2026-111497, reachable from
// any HTTP response an agent decompresses.
const NodeCompatibleExpr = "(major === 24 && (minor > 22 || (minor === 22 && patch >= 1))) || major > 26 || (major === 26 && (minor > 12 || (minor === 12 && patch >= 1)))"

// NodeCompatibleCheck is a shell command that exits 0 when the node on PATH
// satisfies NodeCompatibleExpr.
const NodeCompatibleCheck = `node -e 'const [major, minor, patch] = process.versions.node.split(".").map(Number); process.exit(` + NodeCompatibleExpr + ` ? 0 : 1)'`

func FromEnv(envName, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
		return v
	}
	return fallback
}
