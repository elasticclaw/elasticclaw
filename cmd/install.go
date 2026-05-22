package cmd

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/install"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
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
	installServer     string
	installDomain     string
	installSSHKey     string
	installVersion    string
	installToken      string
	installUIPassword string
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
		Domain:     installDomain,
		Version:    version,
		Token:      token,
		ClawToken:  clawToken,
		UIPassword: uiToken,
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

	useSudo, err := detectRemotePrivilegeMode(client)
	if err != nil {
		return err
	}

	steps := []struct {
		name   string
		script string
	}{
		{"Installing hub binary", install.ScriptInstallBinary(version, useSudo)},
		{"Writing hub config", install.ScriptWriteConfig(params, useSudo)},
		{"Installing systemd service", install.ScriptInstallSystemd(useSudo)},
	}
	skipCaddy, _ := cmd.Flags().GetBool("skip-caddy")
	if !skipCaddy {
		steps = append(steps,
			struct{ name, script string }{"Installing Caddy", install.ScriptInstallCaddy(useSudo)},
			struct{ name, script string }{"Configuring Caddy", install.ScriptWriteCaddyfile(installDomain, useSudo)},
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
	fmt.Println()
	fmt.Printf("  Login: elasticclaw login --hub https://%s --token %s\n", installDomain, token)
	fmt.Printf("  Web UI: https://%s\n", installDomain)
	if !skipCaddy {
		fmt.Println()
		fmt.Println("  ⏳ Caddy is obtaining a TLS certificate from Let's Encrypt.")
		fmt.Println("     This may take a minute. If the site is unreachable, wait 60s and try again.")
	}
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

// findGitHubRelease checks whether a specific release tag exists on GitHub.
func findGitHubRelease(owner, repo, tag string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, tag)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("release %s not found", tag)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API error: HTTP %d", resp.StatusCode)
	}
	return nil
}

// extractTrack returns the non-incrementing prefix of a CalVer/SemVer tag.
//
//	"2026.05.11-beta.2" → "2026.05.11-beta"
//	"2026.05.11.1"      → "2026.05.11"
//	"2026.05.11"        → "2026.05.11"
func extractTrack(version string) string {
	if idx := strings.LastIndex(version, "-"); idx != -1 {
		suffix := version[idx+1:]
		if dotIdx := strings.LastIndex(suffix, "."); dotIdx != -1 {
			if _, err := strconv.Atoi(suffix[dotIdx+1:]); err == nil {
				return version[:idx+1+dotIdx]
			}
		}
		return version
	}
	parts := strings.Split(version, ".")
	if len(parts) >= 4 {
		return strings.Join(parts[:3], ".")
	}
	return version
}

// releaseMatchesTrack reports whether a release tag belongs to the same
// upgrade track as the given track prefix.
func releaseMatchesTrack(tag, track string) bool {
	if strings.Contains(track, "-") {
		return tag == track || strings.HasPrefix(tag, track+".")
	}
	if tag == track {
		return true
	}
	if !strings.HasPrefix(tag, track+".") {
		return false
	}
	suffix := tag[len(track)+1:]
	// Stable track: suffix must be numeric only (no prerelease indicators)
	if strings.Contains(suffix, "-") {
		return false
	}
	_, err := strconv.Atoi(suffix)
	return err == nil
}

// trackSuffix returns the numeric suffix after the track prefix, or 0 if none.
func trackSuffix(tag, track string) int {
	if tag == track {
		return 0
	}
	if !strings.HasPrefix(tag, track) {
		return 0
	}
	rest := tag[len(track):]
	if strings.HasPrefix(rest, ".") {
		rest = rest[1:]
	}
	n, _ := strconv.Atoi(rest)
	return n
}

// latestReleaseOnTrack queries GitHub for the most recent release whose tag
// belongs to the same track as currentVersion. It paginates through releases
// until it finds at least one match or exhausts all pages (max 10 pages = 1000
// releases) to avoid the case where >100 releases on other tracks push the
// target track off the first page.
func latestReleaseOnTrack(owner, repo, currentVersion string) (string, error) {
	track := extractTrack(currentVersion)
	const maxPages = 10
	var best string
	bestSuffix := -1

	for page := 1; page <= maxPages; page++ {
		url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases?per_page=100&page=%d", owner, repo, page)
		resp, err := http.Get(url)
		if err != nil {
			return "", err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("GitHub API error: HTTP %d", resp.StatusCode)
		}

		var releases []struct {
			TagName string `json:"tag_name"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
			resp.Body.Close()
			return "", err
		}
		resp.Body.Close()

		if len(releases) == 0 {
			break // no more pages
		}

		for _, r := range releases {
			if !releaseMatchesTrack(r.TagName, track) {
				continue
			}
			suf := trackSuffix(r.TagName, track)
			if suf > bestSuffix {
				bestSuffix = suf
				best = r.TagName
			}
		}

		// Continue scanning all pages up to maxPages.
		// GitHub returns releases newest-first by publication time, but
		// bestSuffix is a version-number comparison — the two orderings
		// can diverge (e.g. beta.5 published before beta.3). We stop
		// only when a page is empty.
	}

	if best == "" {
		return "", fmt.Errorf("no releases found on track %s", track)
	}
	return best, nil
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
			if keyPath != "" && os.IsNotExist(err) {
				return nil, fmt.Errorf("ssh key file not found: %s", p)
			}
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

	hostKeyCallback, err := knownHostsCallback()
	if err != nil {
		return nil, err
	}

	cfg := &gossh.ClientConfig{
		User:            user,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         15 * time.Second,
	}
	return gossh.Dial("tcp", addr, cfg)
}

func knownHostsCallback() (gossh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not locate home directory for SSH known_hosts: %w", err)
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	callback, err := knownhosts.New(path)
	if err != nil {
		if os.IsNotExist(err) {
			return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
				return trustUnknownHostKey(path, hostname, key)
			}, nil
		}
		return nil, fmt.Errorf("could not load SSH known_hosts from %s: %w", path, err)
	}
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		if err := callback(hostname, remote, key); err != nil {
			var keyErr *knownhosts.KeyError
			if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
				return trustUnknownHostKey(path, hostname, key)
			}
			return fmt.Errorf("SSH host key verification failed for %s: %w; add or update the trusted host key with: %s", hostname, err, sshKeyscanHint(hostname, path))
		}
		return nil
	}, nil
}

func trustUnknownHostKey(knownHostsPath, hostname string, key gossh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(knownHostsPath), 0700); err != nil {
		return fmt.Errorf("could not create SSH known_hosts directory: %w", err)
	}
	file, err := os.OpenFile(knownHostsPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("could not open SSH known_hosts at %s: %w", knownHostsPath, err)
	}
	defer file.Close()

	if _, err := file.WriteString(knownhosts.Line([]string{hostname}, key) + "\n"); err != nil {
		return fmt.Errorf("could not add SSH host key for %s to %s: %w", hostname, knownHostsPath, err)
	}
	fmt.Fprintf(os.Stderr, "Trusted new SSH host key for %s (%s); added to %s\n", hostname, key.Type(), knownHostsPath)
	return nil
}

func sshKeyscanHint(hostname, knownHostsPath string) string {
	host := hostname
	port := ""
	if splitHost, splitPort, err := net.SplitHostPort(hostname); err == nil {
		host = splitHost
		port = splitPort
	}
	if port != "" && port != "22" {
		return fmt.Sprintf("ssh-keyscan -p %s -H %s >> %s", port, host, knownHostsPath)
	}
	return fmt.Sprintf("ssh-keyscan -H %s >> %s", host, knownHostsPath)
}

func detectRemotePrivilegeMode(client *gossh.Client) (bool, error) {
	out, err := sshRunClient(client, "sh -c 'printf __EC_UID__ ; id -u ; printf __EC_DONE__'")
	if err != nil {
		return false, fmt.Errorf("failed to detect remote uid: %w", err)
	}

	start := strings.Index(out, "__EC_UID__")
	end := strings.Index(out, "__EC_DONE__")
	if start == -1 || end == -1 || end <= start+len("__EC_UID__") {
		return false, fmt.Errorf("failed to parse remote uid probe output")
	}

	uid := strings.TrimSpace(out[start+len("__EC_UID__") : end])
	if uid == "0" {
		return false, nil
	}

	if _, err := sshRunClient(client, "sudo -n true"); err != nil {
		return false, fmt.Errorf("remote user is not root and passwordless sudo is unavailable")
	}

	return true, nil
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
