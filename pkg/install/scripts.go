// Package install contains pure functions for generating the shell scripts
// and config files used by the elasticclaw install command.
// All functions are side-effect free and easily testable.
package install

import "fmt"

// Params holds all inputs needed to generate install scripts.
type Params struct {
	Domain       string
	Version      string
	Token        string
	ClawToken    string
	UIToken      string
	AnthropicKey string // optional; adds llm_keys to hub.yaml if set
}

// HubBinaryURL returns the GitHub releases download URL for the hub binary.
func HubBinaryURL(version string) string {
	return fmt.Sprintf(
		"https://github.com/elasticclaw/elasticclaw/releases/download/%s/elasticclaw-linux-amd64",
		version,
	)
}

// WebImageTag returns the Docker image tag for the web UI.
func WebImageTag(version string) string {
	return fmt.Sprintf("marc/elasticclaw-web:%s", version)
}

// HubConfig returns the hub.yaml config file content.
func HubConfig(p Params) string {
	llmKeys := ""
	if p.AnthropicKey != "" {
		llmKeys = fmt.Sprintf("\nllm_keys:\n  anthropic: %s", p.AnthropicKey)
	}
	return fmt.Sprintf(`url: https://%s
public_url: https://%s
token: %s
claw_token: %s
ui_token: %s
address: :8080%s
`, p.Domain, p.Domain, p.Token, p.ClawToken, p.UIToken, llmKeys)
}

// SystemdUnit returns the systemd unit file for the hub service.
func SystemdUnit() string {
	return `[Unit]
Description=ElasticClaw Hub
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/elasticclaw hub
Restart=always
RestartSec=5
WorkingDirectory=/root

[Install]
WantedBy=multi-user.target
`
}

// Caddyfile returns the Caddyfile config for the given domain.
func Caddyfile(domain string) string {
	return fmt.Sprintf(`%s {
	handle /api/login {
		reverse_proxy localhost:8080
	}
	handle /api/auth/* {
		reverse_proxy localhost:3000
	}
	handle /api/hub-config {
		reverse_proxy localhost:3000
	}
	handle /api/claws* {
		reverse_proxy localhost:8080
	}
	handle /api/messages* {
		reverse_proxy localhost:8080
	}
	handle /api/ws* {
		reverse_proxy localhost:8080
	}
	handle /api/terminal* {
		reverse_proxy localhost:8080
	}
	handle /api/github* {
		reverse_proxy localhost:8080
	}
	handle /api/debug* {
		reverse_proxy localhost:8080
	}
	handle /claw/* {
		reverse_proxy localhost:8080
	}
	handle {
		reverse_proxy localhost:3000
	}
}
`, domain)
}

// ScriptInstallBinary returns the shell script to download and install the hub binary.
func ScriptInstallBinary(version string) string {
	url := HubBinaryURL(version)
	return fmt.Sprintf(`set -e
curl -fsSL %q -o /tmp/elasticclaw-bin
chmod +x /tmp/elasticclaw-bin
mv /tmp/elasticclaw-bin /usr/local/bin/elasticclaw
elasticclaw --version`, url)
}

// ScriptWriteConfig returns the shell script to write the hub config.
func ScriptWriteConfig(p Params) string {
	return fmt.Sprintf(`mkdir -p /root/.elasticclaw
cat > /root/.elasticclaw/hub.yaml << 'HUBEOF'
%sHUBEOF`, HubConfig(p))
}

// ScriptInstallSystemd returns the shell script to install and start the systemd service.
func ScriptInstallSystemd() string {
	return fmt.Sprintf(`cat > /etc/systemd/system/elasticclaw.service << 'SVCEOF'
%sSVCEOF
systemctl daemon-reload
systemctl enable elasticclaw
systemctl restart elasticclaw`, SystemdUnit())
}

// ScriptInstallCaddy returns the shell script to install Caddy if not present.
func ScriptInstallCaddy() string {
	return `which caddy >/dev/null 2>&1 || (
  apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl 2>/dev/null
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
  apt-get install -y caddy
)`
}

// ScriptWriteCaddyfile returns the shell script to write the Caddyfile and reload Caddy.
func ScriptWriteCaddyfile(domain string) string {
	return fmt.Sprintf(`cat > /etc/caddy/Caddyfile << 'CADDYEOF'
%sCADDYEOF
systemctl reload caddy || systemctl restart caddy`, Caddyfile(domain))
}

// ScriptRunWebUI returns the shell script to pull and start the web UI Docker container.
func ScriptRunWebUI(p Params) string {
	image := WebImageTag(p.Version)
	return fmt.Sprintf(`docker stop elasticclaw-web 2>/dev/null || true
docker rm elasticclaw-web 2>/dev/null || true
docker pull %s
docker run -d \
  --name elasticclaw-web \
  --restart=always \
  --network=host \
  -e ELASTICCLAW_HUB_URL=http://localhost:8080 \
  -e ELASTICCLAW_HUB_TOKEN=%s \
  -e ELASTICCLAW_UI_TOKEN=%s \
  %s`, image, p.Token, p.UIToken, image)
}
