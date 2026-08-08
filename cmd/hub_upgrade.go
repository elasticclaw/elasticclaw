package cmd

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/spf13/cobra"
)

var hubUpgradeServer string
var hubUpgradeSSHKey string
var hubUpgradeVersion string
var hubUpgradeTrustNewHostKey bool

var hubUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade the hub binary on a remote server to the latest release on the current track",
	Long: `Upgrade the elasticclaw hub on a remote server via SSH.

By default the hub is upgraded to the latest release on the same track as
the client binary — stable clients get the latest stable, prerelease clients
(e.g. beta, rc) get the latest on their prerelease track. Cross-track jumps
are prevented.

Use --version to override and install a specific release.

Examples:
  elasticclaw hub upgrade --server ssh://root@elasticclaw.example.com
  elasticclaw hub upgrade --server ssh://root@elasticclaw.example.com --version 2026.5.11.2
`,
	RunE: runHubUpgrade,
}

func init() {
	hubCmd.AddCommand(hubUpgradeCmd)
	hubUpgradeCmd.Flags().StringVar(&hubUpgradeServer, "server", "", "SSH target, e.g. ssh://root@host (required)")
	hubUpgradeCmd.Flags().StringVar(&hubUpgradeSSHKey, "ssh-key", "", "SSH private key path (optional; defaults to profile ssh_key when available)")
	hubUpgradeCmd.Flags().StringVar(&hubUpgradeVersion, "version", "", "Override the target version (default: client version)")
	hubUpgradeCmd.Flags().BoolVar(&hubUpgradeTrustNewHostKey, "trust-new-host-key", false, "Trust and persist an unknown SSH host key on first connection; prints the fingerprint after adding it")
}

func runHubUpgrade(cmd *cobra.Command, args []string) error {
	if hubUpgradeServer == "" || hubUpgradeSSHKey == "" {
		// Try to infer from --profile (or active profile)
		inferredServer, inferredKey := inferSSHFromProfile(profile)
		if hubUpgradeServer == "" {
			hubUpgradeServer = inferredServer
		}
		if hubUpgradeSSHKey == "" {
			hubUpgradeSSHKey = inferredKey
		}
	}
	if hubUpgradeServer == "" {
		return fmt.Errorf("--server required, e.g. --server ssh://root@elasticclaw.example.com")
	}

	user, host, err := parseSSHHost(hubUpgradeServer)
	if err != nil {
		return err
	}

	fmt.Printf("Connecting to %s@%s...\n", user, host)

	client, err := dialSSH(user, host, sshDialOptions{
		KeyPath:         hubUpgradeSSHKey,
		TrustNewHostKey: hubUpgradeTrustNewHostKey,
	})
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()

	useSudo, err := detectRemotePrivilegeMode(client)
	if err != nil {
		return err
	}

	// Check current version on server. Prefer PATH then the install default;
	// sudo when needed (matches the post-upgrade version probe).
	fmt.Println("Checking remote version...")
	remoteVersionCmd := `elasticclaw version 2>/dev/null || /usr/local/bin/elasticclaw version 2>/dev/null || true`
	if useSudo {
		remoteVersionCmd = `sudo elasticclaw version 2>/dev/null || sudo /usr/local/bin/elasticclaw version 2>/dev/null || elasticclaw version 2>/dev/null || /usr/local/bin/elasticclaw version 2>/dev/null || true`
	}
	remoteVerOut, err := sshRunClient(client, remoteVersionCmd)
	if err != nil {
		return fmt.Errorf("failed to check remote version: %w", err)
	}
	remoteVer := parseVersionFromOutput(strings.TrimSpace(remoteVerOut))

	// Determine target version: --version flag overrides track-based lookup.
	targetVer := hubUpgradeVersion
	if targetVer == "" {
		if Version == "dev" {
			return fmt.Errorf("cannot upgrade hub from a dev build — use --version or download a release from https://github.com/elasticclaw/elasticclaw/releases")
		}
		// Find the latest release on the same track as the client.
		var err error
		targetVer, err = latestReleaseOnTrack("elasticclaw", "elasticclaw", Version)
		if err != nil {
			return fmt.Errorf("no releases found on track %s: %w", extractTrack(Version), err)
		}
	} else {
		// Explicit --version: verify the release exists.
		if err := findGitHubRelease("elasticclaw", "elasticclaw", targetVer); err != nil {
			return fmt.Errorf("no matching release for version %s: %w", targetVer, err)
		}
	}

	fmt.Printf("Remote: %s  →  Target: %s\n", remoteVer, targetVer)

	// Compare normalized tags so remote "0.1.0" matches GitHub target "v0.1.0"
	// (and CalVer with/without accidental v). Download still uses targetVer as
	// the exact release tag on GitHub.
	if sameReleaseVersion(remoteVer, targetVer) {
		fmt.Println("Already up to date.")
		return nil
	}

	// Build download URL for linux/amd64 (server is always linux)
	downloadURL := fmt.Sprintf(
		"https://github.com/elasticclaw/elasticclaw/releases/download/%s/elasticclaw-linux-amd64",
		targetVer,
	)

	moveCmd := "mv /tmp/elasticclaw-new \"$SELF\""
	versionCmd := "elasticclaw version 2>/dev/null"
	restartCheckCmd := "systemctl is-active --quiet elasticclaw 2>/dev/null"
	restartCmd := "systemctl restart elasticclaw"
	if useSudo {
		moveCmd = "sudo mv /tmp/elasticclaw-new \"$SELF\""
		versionCmd = "sudo elasticclaw version 2>/dev/null"
		restartCheckCmd = "sudo systemctl is-active --quiet elasticclaw 2>/dev/null"
		restartCmd = "sudo systemctl restart elasticclaw"
	}

	script := fmt.Sprintf(`set -euo pipefail
echo "Downloading %s..."
curl -fsSL %q -o /tmp/elasticclaw-new
chmod +x /tmp/elasticclaw-new
SELF=$(which elasticclaw || echo /usr/local/bin/elasticclaw)
%s
echo "Upgraded to $(%s)"
if %s; then
  echo "Restarting hub service..."
  %s
  echo "Hub service restarted."
fi
`, targetVer, downloadURL, moveCmd, versionCmd, restartCheckCmd, restartCmd)

	fmt.Printf("Upgrading remote hub to %s...\n", targetVer)
	out, err := sshRunClient(client, script)
	if out != "" {
		fmt.Print(out)
	}
	if err != nil {
		return fmt.Errorf("upgrade failed: %w", err)
	}

	fmt.Printf("✓ Hub upgraded to %s\n", targetVer)
	return nil
}

// inferSSHFromProfile tries to guess the SSH target and key from the given profile's hub config.
// Pass "" to use the active profile.
func inferSSHFromProfile(profileName string) (string, string) {
	hubProfile, _, err := config.ResolveHub(profileName)
	if err != nil || hubProfile == nil {
		return "", ""
	}
	// Use explicit ssh_uri if set (e.g. ssh://marc@canio-factory)
	if hubProfile.SSHURI != "" {
		if strings.HasPrefix(hubProfile.SSHURI, "ssh://") {
			return hubProfile.SSHURI, hubProfile.SSHKey
		}
		// bare hostname — wrap as ssh://root@<host>
		return fmt.Sprintf("ssh://root@%s", hubProfile.SSHURI), hubProfile.SSHKey
	}
	if hubProfile.URL == "" {
		return "", hubProfile.SSHKey
	}
	// Derive from URL
	host := hubProfile.URL
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.SplitN(host, "/", 2)[0]
	host = strings.SplitN(host, ":", 2)[0]
	if host == "" || host == "localhost" || host == "127.0.0.1" {
		return "", hubProfile.SSHKey
	}
	return fmt.Sprintf("ssh://root@%s", host), hubProfile.SSHKey
}

// normalizeReleaseVersion strips a single leading "v" so GitHub tags like
// v0.1.0 and binary output like "0.1.0" compare equal.
func normalizeReleaseVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "unknown" {
		return v
	}
	return strings.TrimPrefix(v, "v")
}

// sameReleaseVersion reports whether two version strings refer to the same
// release tag, ignoring an optional leading "v".
func sameReleaseVersion(a, b string) bool {
	na, nb := normalizeReleaseVersion(a), normalizeReleaseVersion(b)
	if na == "" || nb == "" || na == "unknown" || nb == "unknown" {
		return false
	}
	return na == nb
}

// parseVersionFromOutput extracts the version tag from elasticclaw version output.
// Accepts CalVer with or without a leading "v", e.g.:
//
//	elasticclaw 2026.8.8-beta.2 (commit: abc, built: ...)
//	elasticclaw v0.1.0 (commit: abc, built: ...)
//
// The returned value is normalized (no leading "v") for display; comparisons
// with GitHub tags should use sameReleaseVersion so "v0.1.0" still matches.
func parseVersionFromOutput(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Using config file:") {
			continue
		}
		// Plain: "elasticclaw <version> ..."
		if strings.HasPrefix(line, "elasticclaw ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return normalizeReleaseVersion(fields[1])
			}
		}
		// JSON: {"version":"...","commit":"...","buildDate":"..."}
		if strings.HasPrefix(line, "{") && strings.Contains(line, `"version"`) {
			const key = `"version":"`
			if i := strings.Index(line, key); i >= 0 {
				rest := line[i+len(key):]
				if j := strings.IndexByte(rest, '"'); j > 0 {
					return normalizeReleaseVersion(rest[:j])
				}
			}
		}
	}
	return "unknown"
}
