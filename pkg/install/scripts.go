// Package install contains pure functions for generating the shell scripts
// and config files used by the elasticclaw install command.
// All functions are side-effect free and easily testable.
package install

import "fmt"

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

// ScriptInstallBinary returns the shell script to download and install the hub binary.
func ScriptInstallBinary(version string, useSudo bool) string {
	return ScriptInstallBinaryFromURL(HubBinaryURL(version), useSudo)
}

// ScriptInstallBinaryFromURL returns the shell script to download and install a hub binary.
func ScriptInstallBinaryFromURL(url string, useSudo bool) string {
	sudo := sudoPrefix(useSudo)
	return fmt.Sprintf(`set -ex
curl -fsSL %q -o /tmp/elasticclaw-bin
chmod +x /tmp/elasticclaw-bin
%s mv /tmp/elasticclaw-bin /usr/local/bin/elasticclaw`, url, sudo)
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
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sh_c 'gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg'
    curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sh_c 'tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null'
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
      x86_64) CADDY_ARCH=amd64 ;;
      aarch64|arm64) CADDY_ARCH=arm64 ;;
      *) echo "unsupported architecture for automatic Caddy install: $ARCH" >&2; exit 1 ;;
    esac

    CADDY_VERSION=$(curl -fsSL https://api.github.com/repos/caddyserver/caddy/releases/latest | awk -F '"' '/"tag_name":/ {print $4; exit}')
    if [ -z "$CADDY_VERSION" ]; then
      echo "failed to determine latest Caddy version" >&2
      exit 1
    fi

    curl -fsSL 'https://github.com/caddyserver/caddy/releases/download/'"$CADDY_VERSION"'/caddy_'"${CADDY_VERSION#v}"'_linux_'"$CADDY_ARCH"'.tar.gz' -o /tmp/caddy.tar.gz
    tar -xzf /tmp/caddy.tar.gz -C /tmp caddy
    sh_c 'install -m 0755 /tmp/caddy /usr/local/bin/caddy'
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
)`, sudo)
}

// ScriptWriteCaddyfile returns the shell script to write the Caddyfile and reload Caddy.
func ScriptWriteCaddyfile(domain string, useSudo bool) string {
	sudo := sudoPrefix(useSudo)
	return fmt.Sprintf(`cat > /tmp/Caddyfile << 'CADDYEOF'
%sCADDYEOF
%s mv /tmp/Caddyfile /etc/caddy/Caddyfile
%s systemctl reload caddy || %s systemctl restart caddy`, Caddyfile(domain), sudo, sudo, sudo)
}
