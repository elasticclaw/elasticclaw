// Package install generates install scripts and verifies downloaded assets.
package install

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/cliversion"
)

// Params holds all inputs needed to generate install scripts.
type Params struct {
	Domain     string
	Version    string
	Token      string
	ClawToken  string
	UIPassword string
}

// HubBinaryURL returns the GitHub releases download URL for the hub binary.
func HubBinaryURL(version string) string {
	return fmt.Sprintf(
		"https://github.com/elasticclaw/elasticclaw/releases/download/%s/elasticclaw-linux-amd64",
		version,
	)
}

// HubConfig returns the hub.yaml config file content.
func HubConfig(p Params) string {
	return fmt.Sprintf(`url: https://%s
public_url: https://%s
token: %s
claw_token: %s
ui_password: %s
address: :8080
`, p.Domain, p.Domain, p.Token, p.ClawToken, p.UIPassword)
}

// SystemdUnit returns the systemd unit file for the hub service.
func SystemdUnit() string {
	return `[Unit]
Description=ElasticClaw Hub
After=network.target

[Service]
Type=simple
Environment=HOME=/root
ExecStart=/usr/local/bin/elasticclaw hub
Restart=always
RestartSec=5
WorkingDirectory=/root

[Install]
WantedBy=multi-user.target
`
}

// Caddyfile returns the Caddyfile config for the given domain.
// The hub now serves both the web UI and API on a single port (8080),
// so Caddy simply reverse proxies everything to it.
func Caddyfile(domain string) string {
	return fmt.Sprintf(`%s {
	reverse_proxy localhost:8080
}
`, domain)
}

func sudoPrefix(useSudo bool) string {
	if useSudo {
		return "sudo "
	}
	return ""
}

// ScriptInstallBinary returns the shell script to download and install the
// release hub binary. digest is its SHA256, already verified by the caller
// (see FetchReleaseChecksums), so the target host needs no signature tooling.
func ScriptInstallBinary(version, digest string, useSudo bool) string {
	return ScriptInstallBinaryFromURL(HubBinaryURL(version), digest, useSudo)
}

// ScriptInstallBinaryFromURL returns the shell script to download a hub binary
// and install it only if it matches digest.
func ScriptInstallBinaryFromURL(url, digest string, useSudo bool) string {
	return fmt.Sprintf(`set -ex
download_dir=$(mktemp -d)
trap 'rm -rf "$download_dir"' EXIT
curl -fsSL %s -o "$download_dir/elasticclaw-bin"
digest=%s
if ! printf '%%s\n' "$digest" | grep -Eq '^[0-9a-fA-F]{64}$' || [ "${#digest}" -ne 64 ]; then
  echo "Missing or invalid checksum for hub binary" >&2
  exit 1
fi
printf '%%s  %%s\n' "$digest" "$download_dir/elasticclaw-bin" > "$download_dir/verify.txt"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum -c "$download_dir/verify.txt"
else
  shasum -a 256 -c "$download_dir/verify.txt"
fi
chmod +x "$download_dir/elasticclaw-bin"
%smv "$download_dir/elasticclaw-bin" /usr/local/bin/elasticclaw`,
		shellQuote(url), shellQuote(digest), sudoPrefix(useSudo))
}

// ScriptWriteConfig returns the shell script to write the hub config.
func ScriptWriteConfig(p Params, useSudo bool) string {
	configDir := "$HOME/.elasticclaw"
	sudo := sudoPrefix(useSudo)
	if useSudo {
		configDir = "/root/.elasticclaw"
	}
	return fmt.Sprintf(`umask 077
cat > /tmp/hub.yaml << 'HUBEOF'
%sHUBEOF
%s mkdir -p %q
%s mv /tmp/hub.yaml %q/hub.yaml`, HubConfig(p), sudo, configDir, sudo, configDir)
}

// ScriptInstallSystemd returns the shell script to install and start the systemd service.
func ScriptInstallSystemd(useSudo bool) string {
	sudo := sudoPrefix(useSudo)
	return fmt.Sprintf(`cat > /tmp/elasticclaw.service << 'SVCEOF'
%sSVCEOF
%s mv /tmp/elasticclaw.service /etc/systemd/system/elasticclaw.service
%s systemctl daemon-reload
%s systemctl enable elasticclaw
%s systemctl restart elasticclaw`, SystemdUnit(), sudo, sudo, sudo, sudo)
}

const (
	CaddyVersion     = "2.11.9"
	CaddyAMD64SHA512 = "0e85544340290a16ce4bfb76fe7d65e9de1bf3a6e67f5ccfff30b30d27ea4c73e078077b7bf3e58ae60b6b12e44327705287059c775bca5782e8ee85ea7edd75"
	CaddyARM64SHA512 = "9e5e76d7888542924de2c25353ae87c5f77e2a22c5b1198633cae13688922282e05383d142599205c88d8d80eee5e967d291214d7abce4835329a191b20dd6e0"
)

// ScriptInstallCaddy returns the shell script to install Caddy if not present.
func ScriptInstallCaddy(useSudo bool) string {
	sudo := sudoPrefix(useSudo)
	return fmt.Sprintf(`which caddy >/dev/null 2>&1 || (
  set -eu

  SUDO=%q
  sh_c() {
    if [ -n "$SUDO" ]; then
      sudo sh -c "$1"
    else
      sh -c "$1"
    fi
  }

  OS_ID=""
  OS_LIKE=""
  if [ -f /etc/os-release ]; then
    OS_ID=$(awk -F= '/^ID=/{gsub(/"/, "", $2); print $2}' /etc/os-release)
    OS_LIKE=$(awk -F= '/^ID_LIKE=/{gsub(/"/, "", $2); print $2}' /etc/os-release)
  fi

  if command -v apt-get >/dev/null 2>&1; then
    sh_c 'install -d /usr/share/keyrings /etc/apt/sources.list.d'
    sh_c 'apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl gpg 2>/dev/null || true'
    %s
    (
      CADDY_LIST=$(mktemp)
      trap 'rm -f "$CADDY_LIST"' EXIT
      curl -fsSL --proto '=https' 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' -o "$CADDY_LIST"
      sh_c 'tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null' < "$CADDY_LIST"
    )
    sh_c 'apt-get update -qq'
    sh_c 'apt-get install -y caddy'
  elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
    PKG_MGR=dnf
    if ! command -v dnf >/dev/null 2>&1; then
      PKG_MGR=yum
    fi

    sh_c "$PKG_MGR install -y curl tar"

    ARCH=$(uname -m)
    case "$ARCH" in
      x86_64) CADDY_ARCH=amd64; CADDY_SHA512=%s ;;
      aarch64|arm64) CADDY_ARCH=arm64; CADDY_SHA512=%s ;;
      *) echo "unsupported architecture for automatic Caddy install: $ARCH" >&2; exit 1 ;;
    esac

    CADDY_VERSION=%s
    CADDY_TMP=$(mktemp -d)
    trap 'rm -rf "$CADDY_TMP"' EXIT
    curl -fsSL --proto '=https' "https://github.com/caddyserver/caddy/releases/download/v$CADDY_VERSION/caddy_${CADDY_VERSION}_linux_$CADDY_ARCH.tar.gz" -o "$CADDY_TMP/caddy.tar.gz"
    printf '%%s  %%s\n' "$CADDY_SHA512" "$CADDY_TMP/caddy.tar.gz" | sha512sum -c -
    tar -xzf "$CADDY_TMP/caddy.tar.gz" -C "$CADDY_TMP" caddy
    %sinstall -m 0755 "$CADDY_TMP/caddy" /usr/local/bin/caddy
    sh_c 'install -d /etc/caddy'

    cat > /tmp/caddy.service <<'CADDYSVC'
[Unit]
Description=Caddy
Documentation=https://caddyserver.com/docs/
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/caddy run --environ --config /etc/caddy/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config /etc/caddy/Caddyfile
TimeoutStopSec=5s
LimitNOFILE=1048576
LimitNPROC=512
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
CADDYSVC

    sh_c 'mv /tmp/caddy.service /etc/systemd/system/caddy.service'
    sh_c 'systemctl daemon-reload'
    sh_c 'systemctl enable caddy'
  else
    echo "unsupported Linux distribution for automatic Caddy install (ID=${OS_ID:-unknown}, ID_LIKE=${OS_LIKE:-unknown})" >&2
    exit 1
  fi
)`, sudo, cliversion.VerifiedAPTKeyScript("https://dl.cloudsmith.io/public/caddy/stable/gpg.key", "/usr/share/keyrings/caddy-stable-archive-keyring.gpg", cliversion.CaddyKeyFingerprint, sudo), CaddyAMD64SHA512, CaddyARM64SHA512, CaddyVersion, sudo)
}

// ScriptWriteCaddyfile returns the shell script to write the Caddyfile and reload Caddy.
func ScriptWriteCaddyfile(domain string, useSudo bool) string {
	sudo := sudoPrefix(useSudo)
	return fmt.Sprintf(`cat > /tmp/Caddyfile << 'CADDYEOF'
%sCADDYEOF
%s mv /tmp/Caddyfile /etc/caddy/Caddyfile
%s systemctl reload caddy || %s systemctl restart caddy`, Caddyfile(domain), sudo, sudo, sudo)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
