package cmd

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install ElasticClaw on a remote server",
	Long: `Install and configure ElasticClaw on a remote VPS.

Installs the hub binary, web UI (Docker), Caddy reverse proxy with TLS,
and systemd service — fully configured and ready to use.

  elasticclaw install \
    --host user@my-server.com \
    --domain hub.mycompany.com

Prerequisites:
  - SSH access to the server (key-based auth)
  - Docker installed on the server
  - DNS A record for --domain pointing to the server IP`,
	RunE: runInstall,
}

var (
	installHost    string
	installDomain  string
	installSSHKey  string
	installVersion string
	installToken   string
	installUIToken string
)

func init() {
	rootCmd.AddCommand(installCmd)
	installCmd.Flags().StringVar(&installHost, "host", "", "SSH host (e.g. root@1.2.3.4 or user@myserver.com)")
	installCmd.Flags().StringVar(&installDomain, "domain", "", "Domain name that resolves to the server (e.g. hub.mycompany.com)")
	installCmd.Flags().StringVar(&installSSHKey, "ssh-key", "", "Path to SSH private key (default: SSH agent or ~/.ssh/id_rsa)")
	installCmd.Flags().StringVar(&installVersion, "version", "", "Hub version to install (default: latest release)")
	installCmd.Flags().StringVar(&installToken, "token", "", "Hub user token (default: randomly generated)")
	installCmd.Flags().StringVar(&installUIToken, "ui-token", "", "Web UI login password (default: randomly generated)")
	installCmd.MarkFlagRequired("host")
	installCmd.MarkFlagRequired("domain")
}

func runInstall(cmd *cobra.Command, args []string) error {
	// ── Resolve version ───────────────────────────────────────────────────────
	version := installVersion
	if version == "" {
		fmt.Print("Fetching latest release... ")
		var err error
		version, err = latestGitHubRelease("elasticclaw", "elasticclaw")
		if err != nil {
			return fmt.Errorf("could not determine latest version: %w\nUse --version to specify explicitly", err)
		}
		fmt.Printf("%s\n", version)
	}

	// ── Generate tokens if not provided ──────────────────────────────────────
	token := installToken
	if token == "" {
		token = randomHex32()
	}
	uiToken := installUIToken
	if uiToken == "" {
		uiToken = randomHex32()
	}
	clawToken := randomHex32()

	// ── Preflight: DNS check ──────────────────────────────────────────────────
	fmt.Printf("Checking DNS for %s... ", installDomain)
	addrs, err := net.LookupHost(installDomain)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("DNS lookup failed for %s — make sure an A record points to your server", installDomain)
	}
	fmt.Printf("OK (%s)\n", addrs[0])

	// ── Parse SSH host ────────────────────────────────────────────────────────
	sshUser, sshHost, err := parseSSHHost(installHost)
	if err != nil {
		return err
	}

	// ── Connect SSH ───────────────────────────────────────────────────────────
	fmt.Printf("Connecting to %s@%s... ", sshUser, sshHost)
	client, err := dialSSH(sshUser, sshHost, installSSHKey)
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()
	fmt.Println("OK")

	// ── Preflight: Docker ─────────────────────────────────────────────────────
	fmt.Print("Checking Docker... ")
	if out, err := sshRun(client, "docker --version 2>&1"); err != nil {
		return fmt.Errorf("Docker not found on server: %s\nInstall Docker first: https://docs.docker.com/engine/install/", out)
	} else {
		fmt.Printf("OK (%s)\n", strings.TrimSpace(strings.Split(out, "\n")[0]))
	}

	// ── Install hub binary ────────────────────────────────────────────────────
	fmt.Printf("Installing elasticclaw %s... ", version)
	hubURL := fmt.Sprintf(
		"https://github.com/elasticclaw/elasticclaw/releases/download/%s/elasticclaw-linux-amd64",
		version,
	)
	script := fmt.Sprintf(`set -e
curl -fsSL %q -o /tmp/elasticclaw-bin
chmod +x /tmp/elasticclaw-bin
mv /tmp/elasticclaw-bin /usr/local/bin/elasticclaw
elasticclaw --version`, hubURL)
	if out, err := sshRun(client, script); err != nil {
		return fmt.Errorf("hub install failed: %s", out)
	}
	fmt.Println("OK")

	// ── Write hub config ──────────────────────────────────────────────────────
	fmt.Print("Writing hub config... ")
	hubConfig := fmt.Sprintf(`url: https://%s
public_url: https://%s
token: %s
claw_token: %s
ui_token: %s
address: :8080
`, installDomain, installDomain, token, clawToken, uiToken)
	configScript := fmt.Sprintf(`mkdir -p /root/.elasticclaw
cat > /root/.elasticclaw/hub.yaml << 'HUBEOF'
%sHUBEOF`, hubConfig)
	if _, err := sshRun(client, configScript); err != nil {
		return fmt.Errorf("config write failed")
	}
	fmt.Println("OK")

	// ── Install systemd service for hub ───────────────────────────────────────
	fmt.Print("Installing hub systemd service... ")
	unitFile := `[Unit]
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
	serviceScript := fmt.Sprintf(`cat > /etc/systemd/system/elasticclaw.service << 'SVCEOF'
%sSVCEOF
systemctl daemon-reload
systemctl enable elasticclaw
systemctl restart elasticclaw`, unitFile)
	if _, err := sshRun(client, serviceScript); err != nil {
		return fmt.Errorf("systemd service install failed")
	}
	fmt.Println("OK")

	// ── Pull and start web UI Docker container ────────────────────────────────
	fmt.Printf("Starting web UI (marc/elasticclaw-web:%s)... ", version)
	webScript := fmt.Sprintf(`docker stop elasticclaw-web 2>/dev/null || true
docker rm elasticclaw-web 2>/dev/null || true
docker pull marc/elasticclaw-web:%s
docker run -d \
  --name elasticclaw-web \
  --restart=always \
  --network=host \
  -e ELASTICCLAW_HUB_URL=http://localhost:8080 \
  -e ELASTICCLAW_HUB_TOKEN=%s \
  -e ELASTICCLAW_UI_TOKEN=%s \
  marc/elasticclaw-web:%s`, version, token, uiToken, version)
	if _, err := sshRun(client, webScript); err != nil {
		return fmt.Errorf("web UI start failed")
	}
	fmt.Println("OK")

	// ── Install Caddy ─────────────────────────────────────────────────────────
	fmt.Print("Installing Caddy... ")
	caddyInstall := `which caddy >/dev/null 2>&1 || (
  apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl 2>/dev/null
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
  apt-get update -qq
  apt-get install -y caddy
)`
	if _, err := sshRun(client, caddyInstall); err != nil {
		return fmt.Errorf("Caddy install failed")
	}
	fmt.Println("OK")

	// ── Write Caddyfile ───────────────────────────────────────────────────────
	fmt.Print("Configuring Caddy... ")
	caddyfile := fmt.Sprintf(`%s {
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
`, installDomain)
	caddyScript := fmt.Sprintf(`cat > /etc/caddy/Caddyfile << 'CADDYEOF'
%sCAddyEOF
systemctl reload caddy || systemctl restart caddy`, caddyfile)
	if _, err := sshRun(client, caddyScript); err != nil {
		return fmt.Errorf("Caddy config failed")
	}
	fmt.Println("OK")

	// ── Done ──────────────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Printf("✓ ElasticClaw installed at https://%s\n", installDomain)
	fmt.Println()
	fmt.Printf("  Hub token:    %s\n", token)
	fmt.Printf("  UI password:  %s\n", uiToken)
	fmt.Printf("  Claw token:   %s  (used internally by claws)\n", clawToken)
	fmt.Println()
	fmt.Printf("  Login: elasticclaw login --hub https://%s --token %s\n", installDomain, token)
	fmt.Printf("  Web UI: https://%s\n", installDomain)
	fmt.Println()
	fmt.Println("  Save these credentials — they won't be shown again.")

	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func latestGitHubRelease(owner, repo string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("no releases found")
	}
	return rel.TagName, nil
}

func parseSSHHost(host string) (user, addr string, err error) {
	user = "root"
	addr = host
	if strings.Contains(host, "@") {
		parts := strings.SplitN(host, "@", 2)
		user, addr = parts[0], parts[1]
	}
	if !strings.Contains(addr, ":") {
		addr = addr + ":22"
	}
	return user, addr, nil
}

func dialSSH(user, addr, keyPath string) (*gossh.Client, error) {
	var authMethods []gossh.AuthMethod

	// Try SSH agent first
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			authMethods = append(authMethods, gossh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	// Try key file
	if keyPath == "" {
		home, _ := os.UserHomeDir()
		for _, k := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
			p := home + "/.ssh/" + k
			if _, err := os.Stat(p); err == nil {
				keyPath = p
				break
			}
		}
	}
	if keyPath != "" {
		if key, err := os.ReadFile(keyPath); err == nil {
			if signer, err := gossh.ParsePrivateKey(key); err == nil {
				authMethods = append(authMethods, gossh.PublicKeys(signer))
			}
		}
	}

	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	return gossh.Dial("tcp", addr, cfg)
}

func sshRun(client *gossh.Client, script string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(script)
	return string(out), err
}

func randomHex32() string {
	b := make([]byte, 16)
	io.ReadFull(rand.Reader, b)
	return fmt.Sprintf("%x", b)
}
