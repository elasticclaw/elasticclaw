package cliversion

import _ "embed"

//go:embed openclaw/package.json
var OpenClawPackageJSON []byte

//go:embed openclaw/package-lock.json
var OpenClawPackageLock []byte

//go:embed openclaw-codex-plugin/package.json
var CodexPluginPackageJSON []byte

//go:embed openclaw-codex-plugin/package-lock.json
var CodexPluginPackageLock []byte

const (
	OrasVersion          = "1.2.2"
	OrasLinuxAMD64SHA256 = "bff970346470e5ef888e9f2c0bf7f8ee47283f5a45207d6e7a037da1fb0eae0d"
	OpenClawVersion      = "2026.9.4"
	// OpenClawImageVersion is only the docker provider's runtime base: the
	// bridge installs OpenClawVersion over it whenever the two differ. 2026.10.6
	// is the first image whose Node (24.22.1) satisfies NodeCompatibleRange.
	OpenClawImageVersion = "2026.10.6"
	OpenClawImageDigest  = "sha256:7dbff0fe5f5359b1badefe90a23efa9f203cd0d3c36bee095f4896c7a1d3864b"
	OpenClawImage        = "ghcr.io/openclaw/openclaw:" + OpenClawImageVersion + "@" + OpenClawImageDigest
	CodexPluginVersion   = "2026.9.4"
	CodexCLIVersion      = "0.153.4"
	GrokCLIVersion       = "0.2.103"
)

// NodeCompatibleRange is the Node range claws accept, in npm engines syntax.
const NodeCompatibleRange = ">=24.22.1 <25 || >=26.12.1"

// NodeCompatibleExpr is a JS boolean over major, minor and patch for
// NodeCompatibleRange. OpenClaw 2026.9.x's own
// engines clause starts at 24.16.0 / 26.1.0, but releases below 24.22.1 and
// 26.12.1 bundle a Brotli decoder vulnerable to CVE-2026-111497, reachable from
// any HTTP response an agent decompresses.
const NodeCompatibleExpr = "(major === 24 && (minor > 22 || (minor === 22 && patch >= 1))) || major > 26 || (major === 26 && (minor > 12 || (minor === 12 && patch >= 1)))"

// NodeCompatibleCheck is a shell command that exits 0 when the node on PATH
// satisfies NodeCompatibleExpr.
const NodeCompatibleCheck = `node -e 'const [major, minor, patch] = process.versions.node.split(".").map(Number); process.exit(` + NodeCompatibleExpr + ` ? 0 : 1)'`
