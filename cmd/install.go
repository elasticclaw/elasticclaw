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

	"github.com/elasticclaw/elasticclaw/pkg/install"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install ElasticClaw on a remote server",
	Long: `Install and configure ElasticClaw on a remote VPS.

Installs the hub binary (with embedded web UI), Caddy reverse proxy with TLS,
and systemd service — fully configured and ready to use.

  elasticclaw install \
    --server ssh://root@my-server.com \
    --domain hub.mycompany.com

Prerequisites:
  - SSH access to the server (key-based auth)
  - DNS A record for --domain pointing to the server IP`,
	RunE: runInstall,
}

var (
	installServer   string
	installDomain  string
	installSSHKey  string
	installVersion string
	installToken        string
	installUIPassword   string
)

func init() {
	rootCmd.AddCommand(installCmd)
	installCmd.Flags().StringVar(&installServer, "server", "", "SSH URI of the server (e.g. ssh://root@1.2.3.4 or ssh://root@1.2.3.4:22)")
	installCmd.Flags().StringVar(&installDomain, "domain", "", "Domain name that resolves to the server (e.g. hub.mycompany.com)")
	installCmd.Flags().StringVar(&installSSHKey, "ssh-key", "", "Path to SSH private key (default: SSH agent or ~/.ssh/id_rsa)")
	installCmd.Flags().StringVar(&installVersion, "version", "", "Hub version to install (default: latest release)")
	installCmd.Flags().StringVar(&installToken, "token", "", "Hub user token (default: randomly generated)")
	installCmd.Flags().StringVar(&installUIPassword, "ui-password", "", "Web UI login password (used as ui_password in hub.yaml) (default: randomly generated)")
	installCmd.Flags().Bool("skip-caddy", false, "Skip Caddy installation and TLS (useful when domain/DNS not ready)")
	installCmd.MarkFlagRequired("server")
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

	// ── Generate tokens ───────────────────────────────────────────────────────
	token := installToken
	if token == "" {
		token = randomHex32()
	}
	uiToken := installUIPassword
	if uiToken == "" {
		uiToken = randomHex32()
	}
	clawToken := randomHex32()

	params := install.Params{
		Domain:       installDomain,
		Version:      version,
		Token:        token,
		ClawToken:    clawToken,
		UIPassword:   uiToken,
	}

	// ── Preflight: DNS check (skipped with --skip-caddy) ─────────────────────
	skipCaddyPreflight, _ := cmd.Flags().GetBool("skip-caddy")
	if !skipCaddyPreflight {
		fmt.Printf("Checking DNS for %s... ", installDomain)
		addrs, err := net.LookupHost(installDomain)
		if err != nil || len(addrs) == 0 {
			return fmt.Errorf("DNS lookup failed for %s — make sure an A record points to your server", installDomain)
		}
		fmt.Printf("OK (%s)\n", addrs[0])
	}

	// ── Connect SSH ───────────────────────────────────────────────────────────
	sshUser, sshHost, err := parseSSHHost(installServer)
	if err != nil {
		return err
	}
	fmt.Printf("Connecting to %s@%s... ", sshUser, sshHost)
	client, err := dialSSH(sshUser, sshHost, installSSHKey)
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()
	fmt.Println("OK")

	steps := []struct {
		name   string
		script string
	}{
		{"Installing hub binary", install.ScriptInstallBinary(version)},
		{"Writing hub config", install.ScriptWriteConfig(params)},
		{"Installing systemd service", install.ScriptInstallSystemd()},
	}
	skipCaddy, _ := cmd.Flags().GetBool("skip-caddy")
	if !skipCaddy {
		steps = append(steps,
			struct{ name, script string }{"Installing Caddy", install.ScriptInstallCaddy()},
			struct{ name, script string }{"Configuring Caddy", install.ScriptWriteCaddyfile(installDomain)},
		)
	}

	for _, step := range steps {
		fmt.Printf("%s... ", step.name)
		if out, err := sshRunClient(client, step.script); err != nil {
			return fmt.Errorf("%s failed: %v\n%s", step.name, err, out)
		}
		fmt.Println("OK")
	}

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
	// Strip ssh:// prefix
	host = strings.TrimPrefix(host, "ssh://")
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

	// Try SSH agent first — call Signers() eagerly so failures are visible
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentClient := agent.NewClient(conn)
			if signers, err := agentClient.Signers(); err == nil && len(signers) > 0 {
				authMethods = append(authMethods, gossh.PublicKeys(signers...))
			}
		}
	}

	// Load all available keys (explicit path or all standard ~/.ssh/ keys)
	keyPaths := []string{}
	if keyPath != "" {
		keyPaths = []string{keyPath}
	} else {
		home, _ := os.UserHomeDir()
		for _, k := range []string{"id_ed25519", "id_rsa", "id_ecdsa", "id_ecdsa_sk", "id_ed25519_sk"} {
			keyPaths = append(keyPaths, home+"/.ssh/"+k)
		}
	}
	var signers []gossh.Signer
	for _, p := range keyPaths {
		key, err := os.ReadFile(p)
		if err != nil {
			continue // key doesn't exist, skip
		}
		signer, err := gossh.ParsePrivateKey(key)
		if err != nil {
			continue // passphrase-protected or unreadable, skip (agent handles it)
		}
		signers = append(signers, signer)
	}
	if len(signers) > 0 {
		authMethods = append(authMethods, gossh.PublicKeys(signers...))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no SSH auth methods available — ensure SSH agent is running (eval $(ssh-agent)) or use --ssh-key with an unencrypted key")
	}

	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	return gossh.Dial("tcp", addr, cfg)
}

func sshRunClient(client *gossh.Client, script string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput("bash -c " + shellescape(script))
	return string(out), err
}

func randomHex32() string {
	b := make([]byte, 16)
	io.ReadFull(rand.Reader, b)
	return fmt.Sprintf("%x", b)
}

func shellescape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
